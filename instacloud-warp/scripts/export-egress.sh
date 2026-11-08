#!/usr/bin/env bash
# Export the SOCKS5 list consumed by Session A and optionally verify every port.
set -Eeuo pipefail

HOST="${EXTERNAL_HOST:-}"
SOCKS_USER="${SOCKS_USER:-warpuser}"
SOCKS_PASS="${SOCKS_PASS:-}"
START_PORT="${START_PORT:-1081}"
COUNT="${INSTANCE_COUNT:-${POOL_SIZE:-16}}"
STATE_DIR="${STATE_DIR:-/data}"
VERIFY=0
MIN_UNIQUE=16
OUT=""

usage() {
  cat <<'EOF'
Usage: export-egress.sh --host HOST [options]

  --host HOST        Public IPv4 of the WARP node (or set EXTERNAL_HOST)
  --user USER        SOCKS5 username (default: warpuser)
  --pass PASS        SOCKS5 password (required; or set SOCKS_PASS)
  --start-port PORT  First SOCKS5 port (default: 1081)
  --count N          Number of ports (default: 16)
  --out FILE         Output path (default: ./egress-ext.txt)
  --verify           Probe each port and require unique IPv6 /64 prefixes
  --min-unique N     Required unique prefixes with --verify (default: 16)
  -h, --help         Show this help
EOF
}

while (($#)); do
  case "$1" in
    --host) HOST=$2; shift 2 ;;
    --user) SOCKS_USER=$2; shift 2 ;;
    --pass) SOCKS_PASS=$2; shift 2 ;;
    --start-port) START_PORT=$2; shift 2 ;;
    --count) COUNT=$2; shift 2 ;;
    --out) OUT=$2; shift 2 ;;
    --verify) VERIFY=1; shift ;;
    --min-unique) MIN_UNIQUE=$2; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *)
      echo "unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ -n "$HOST" ]] || {
  echo "error: --host or EXTERNAL_HOST is required" >&2
  exit 2
}
[[ -n "$SOCKS_PASS" ]] || {
  echo "error: --pass or SOCKS_PASS is required" >&2
  exit 2
}
[[ "$COUNT" =~ ^[0-9]+$ ]] && ((COUNT > 0)) || {
  echo "error: --count must be a positive integer" >&2
  exit 2
}
[[ "$START_PORT" =~ ^[0-9]+$ ]] && ((START_PORT > 0)) || {
  echo "error: --start-port must be a positive integer" >&2
  exit 2
}
[[ -n "$OUT" ]] || OUT="$PWD/egress-ext.txt"

denied_family() {
  local family=$1 d
  for d in 2a09:bac5:55 2a09:bac1:65 2a09:bac5:c82 2a09:bac1:27b 2a09:bac1:1e; do
    [[ "$family" == "$d"* ]] && return 0
  done
  return 1
}

lines=()
for ((i = 0; i < COUNT; i++)); do
  port=$((START_PORT + i))
  lines+=("socks5://${SOCKS_USER}:${SOCKS_PASS}@${HOST}:${port}")
done

if ((VERIFY)); then
  declare -A seen=()
  unique=0
  failures=0
  for ((i = 0; i < COUNT; i++)); do
    port=$((START_PORT + i))
    body=$(curl -sS --max-time 8 \
      -x "socks5h://${SOCKS_USER}:${SOCKS_PASS}@127.0.0.1:${port}" \
      "https://opencode.ai/cdn-cgi/trace" 2>/dev/null) || {
      echo "port $port: unreachable" >&2
      failures=$((failures + 1))
      continue
    }
    ip=$(awk -F= '$1=="ip" { print $2; exit }' <<<"$body")
    loc=$(awk -F= '$1=="loc" { print $2; exit }' <<<"$body")
    prefix=$(awk -F: 'NF>=4 { print $1":"$2":"$3":"$4 }' <<<"$ip")
    if [[ -z "$prefix" ]]; then
      echo "port $port: no IPv6 egress (ip=${ip:-none})" >&2
      failures=$((failures + 1))
      continue
    fi
    if [[ "$prefix" != 2a09:* ]]; then
      echo "port $port: unexpected prefix $prefix" >&2
      failures=$((failures + 1))
      continue
    fi
    family=$(cut -d: -f1-3 <<<"$prefix")
    if denied_family "$family"; then
      echo "port $port: DENY family $family" >&2
      failures=$((failures + 1))
      continue
    fi
    if [[ -n "${seen[$prefix]:-}" ]]; then
      echo "port $port: duplicate $prefix (already on ${seen[$prefix]})" >&2
      failures=$((failures + 1))
      continue
    fi
    seen["$prefix"]="$port"
    unique=$((unique + 1))
    printf 'port %s: %s loc=%s ip=%s\n' "$port" "$prefix" "${loc:-?}" "$ip" >&2
  done
  echo "verify: ${unique}/${COUNT} unique IPv6 /64 prefixes, ${failures} failure(s)" >&2
  if ((unique < MIN_UNIQUE)); then
    echo "FAIL: need at least ${MIN_UNIQUE} unique prefixes" >&2
    exit 1
  fi
fi

umask 022
printf '%s\n' "${lines[@]}" >"$OUT"

# Contract v1.1: the gateway skips limitedUntil cooldown for any node tagged
# "#pool", so the exported list must stay pure "socks5://user:pass@ip:port".
if grep -q '#' "$OUT"; then
  echo "FAIL: $OUT contains '#' tags (would disable the 15-minute limitedUntil cooldown)" >&2
  rm -f "$OUT"
  exit 1
fi
actual=$(grep -c '^socks5://' "$OUT")
if ((actual != COUNT)); then
  echo "FAIL: $OUT has ${actual} valid lines, expected ${COUNT}" >&2
  exit 1
fi

echo "wrote $OUT ($(wc -l <"$OUT") lines)" >&2
cat "$OUT"
