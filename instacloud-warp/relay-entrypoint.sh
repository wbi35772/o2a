#!/usr/bin/env bash
# warp-relay: N sing-box sessions, each one WireGuard-over-trojan/WS/TLS
# endpoint behind a local SOCKS port, fanned out by socksmux.
#
# Live relay parameters are read from $STATE_DIR/relay/settings.env:
#   RELAY_HOST=relay.example.com
#   RELAY_PASSWORD=...
#   RELAY_PORT=443            (optional)
#   RELAY_SNI=relay.example.com (optional, defaults to RELAY_HOST)
#   RELAY_WS_PATH=/pgtrojan   (optional)
# The file is re-read every 5s; a change tears down and rebuilds sessions.
# The SOCKS password is generated once and kept in relay/secrets.env (0600).

set -Eeuo pipefail
umask 027

STATE_DIR="${STATE_DIR:-/data}"
RELAY_DIR="$STATE_DIR/relay"
SESSION_ROOT="$RELAY_DIR/instances"
SETTINGS_FILE="$RELAY_DIR/settings.env"
SECRETS_FILE="$RELAY_DIR/secrets.env"
NODES_FILE="${RELAY_NODES_FILE:-$RELAY_DIR/nodes.tsv}"
STATUS_FILE="$STATE_DIR/status.tsv"

MUX_PORT="${MUX_PORT:-1081}"
MUX_LISTEN="${MUX_LISTEN:-0.0.0.0}"
RELAY_COUNT_ENV="${RELAY_COUNT:-}"
RELAY_COUNT="${RELAY_COUNT_ENV:-}"
SING_PORT_BASE="${SING_PORT_BASE:-12101}"
SOCKS_USER="${SOCKS_USER:-warpuser}"
RELAY_PORT="${RELAY_PORT:-443}"
RELAY_WS_PATH="${RELAY_WS_PATH:-/pgtrojan}"
RELAY_START_STAGGER="${RELAY_START_STAGGER:-2.5}"
RELAY_PROBE_URL="${RELAY_PROBE_URL:-https://[2606:4700:4700::1111]/cdn-cgi/trace}"
RELAY_PROBE_TIMEOUT="${RELAY_PROBE_TIMEOUT:-8}"
HEALTH_INTERVAL="${HEALTH_INTERVAL:-300}"
DEDUP_RETRIES="${DEDUP_RETRIES:-4}"
REGISTER_RETRIES="${REGISTER_RETRIES:-3}"
WGCF_KEEPALIVE="${WGCF_KEEPALIVE:-25}"
RELAY_PREFIX_ALLOW="${RELAY_PREFIX_ALLOW:-2a09:}"
DENY_48_PREFIXES="${DENY_48_PREFIXES:-2a09:bac5:55 2a09:bac1:65 2a09:bac5:c82 2a09:bac1:27b 2a09:bac1:1e}"
WG_ENDPOINT_HOST="${WG_ENDPOINT_HOST:-162.159.192.1}"
WG_ENDPOINT_PORT="${WG_ENDPOINT_PORT:-2408}"
RELAY_PRIVATE_HOST="${RELAY_PRIVATE_HOST:-warp-relay.insta.internal}"
RELAY_PUBLIC_HOST="${RELAY_PUBLIC_HOST:-}"

RELAY_API_TOKEN="${RELAY_API_TOKEN:-}"
RELAY_HOST_ENV="${RELAY_HOST:-}"
RELAY_PASSWORD_ENV="${RELAY_PASSWORD:-}"
RELAY_PORT_ENV="${RELAY_PORT}"
RELAY_SNI_ENV="${RELAY_SNI:-}"
RELAY_WS_PATH_ENV="${RELAY_WS_PATH}"
RELAY_HOST="${RELAY_HOST_ENV}"
RELAY_PASSWORD="${RELAY_PASSWORD_ENV}"
RELAY_SNI="${RELAY_SNI_ENV}"
RELAY_NODE_NAME="${RELAY_NODE_NAME:-}"

# Subscription auto-sync: when RELAY_SUB_URL is set, the relay periodically
# fetches the Clash subscription and rewrites relay/nodes.tsv. A change in the
# file is picked up by settings_sig() and rebuilds the sessions.
RELAY_SUB_URL_ENV="${RELAY_SUB_URL:-}"
RELAY_SUB_URL="$RELAY_SUB_URL_ENV"
RELAY_SYNC_INTERVAL="${RELAY_SYNC_INTERVAL:-1800}"

# Multi-node mode: nodes.tsv is one tab-separated row per subscription node:
#   label<TAB>host<TAB>port<TAB>sni<TAB>ws_path<TAB>password
# Sessions are assigned round-robin. With no file the single RELAY_* settings
# above are used for every session.
declare -a NODE_LABEL=() NODE_HOSTS=() NODE_PORTS=() NODE_SNIS=() NODE_WSPATHS=() NODE_PASSWORDS=()
NODE_COUNT=0

export API_ALLOW_ANON="${API_ALLOW_ANON:-1}"
export RELAY_LENIENT_HEALTH="${RELAY_LENIENT_HEALTH:-1}"

log() { printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >&2; }
die() { log "FATAL: $*"; exit 1; }

declare -A STATUS=()
MUX_PID=""

# ---- layout ----------------------------------------------------------------

sess_dir() { printf '%s/n%02d' "$SESSION_ROOT" "$1"; }
sing_port() { printf '%d' $((SING_PORT_BASE + $1 - 1)); }

# ---- settings / secrets ----------------------------------------------------

apply_kv() {
  case "$1" in
    RELAY_HOST) RELAY_HOST=$2 ;;
    RELAY_PASSWORD) RELAY_PASSWORD=$2 ;;
    RELAY_PORT) RELAY_PORT=$2 ;;
    RELAY_SNI) RELAY_SNI=$2 ;;
    RELAY_WS_PATH) RELAY_WS_PATH=$2 ;;
    RELAY_API_TOKEN) RELAY_API_TOKEN=$2 ;;
    RELAY_COUNT) RELAY_COUNT=$2 ;;
    RELAY_SUB_NAME) RELAY_SUB_NAME=$2 ;;
    RELAY_NODE_NAME) RELAY_NODE_NAME=$2 ;;
    RELAY_SUB_URL) RELAY_SUB_URL=$2 ;;
    RELAY_SYNC_INTERVAL) RELAY_SYNC_INTERVAL=$2 ;;
  esac
}

load_settings() {
  [[ -f "$SETTINGS_FILE" ]] || return 0
  local line k v
  while IFS= read -r line || [[ -n "$line" ]]; do
    line=${line%$'\r'}
    [[ "$line" =~ ^[[:space:]]*(#|$) ]] && continue
    [[ "$line" =~ ^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*=(.*)$ ]] || continue
    k=${BASH_REMATCH[1]}
    v=${BASH_REMATCH[2]}
    v="${v#"${v%%[![:space:]]*}"}"
    v="${v%"${v##*[![:space:]]}"}"
    if [[ "$v" == \"*\" || "$v" == \'*\' ]]; then v=${v:1:${#v}-2}; fi
    apply_kv "$k" "$v"
  done <"$SETTINGS_FILE"
}

reload_settings() {
  RELAY_HOST="$RELAY_HOST_ENV"
  RELAY_PASSWORD="$RELAY_PASSWORD_ENV"
  RELAY_PORT="$RELAY_PORT_ENV"
  RELAY_SNI="$RELAY_SNI_ENV"
  RELAY_WS_PATH="$RELAY_WS_PATH_ENV"
  RELAY_COUNT="$RELAY_COUNT_ENV"
  RELAY_SUB_URL="$RELAY_SUB_URL_ENV"
  load_settings
  load_nodes
  if [[ -z "$RELAY_COUNT" ]]; then
    if ((NODE_COUNT > 0)); then
      RELAY_COUNT=$NODE_COUNT
      if ((RELAY_COUNT > 32)); then RELAY_COUNT=32; fi
    else
      RELAY_COUNT=10
    fi
  fi
  [[ "$RELAY_COUNT" =~ ^[0-9]+$ ]] || RELAY_COUNT=10
  [[ "$RELAY_SYNC_INTERVAL" =~ ^[0-9]+$ ]] || RELAY_SYNC_INTERVAL=1800
  if ((10#$RELAY_SYNC_INTERVAL < 60)); then RELAY_SYNC_INTERVAL=60; fi
}

# nodes.tsv is authoritative when present: each row is one subscription node.
load_nodes() {
  NODE_LABEL=()
  NODE_HOSTS=()
  NODE_PORTS=()
  NODE_SNIS=()
  NODE_WSPATHS=()
  NODE_PASSWORDS=()
  NODE_COUNT=0
  [[ -f "$NODES_FILE" ]] || return 0
  local line label host port sni ws pass
  while IFS=$'\t' read -r label host port sni ws pass || [[ -n "$label" ]]; do
    label=${label%$'\r'}
    [[ -z "$label" || "$label" == \#* ]] && continue
    [[ -n "$host" && -n "$pass" ]] || continue
    NODE_LABEL+=("$label")
    NODE_HOSTS+=("$host")
    NODE_PORTS+=("${port:-443}")
    NODE_SNIS+=("${sni:-$host}")
    NODE_WSPATHS+=("${ws:-/pgtrojan}")
    NODE_PASSWORDS+=("$pass")
    NODE_COUNT=$((NODE_COUNT + 1))
  done <"$NODES_FILE"
}

# Fetch the Clash subscription and rewrite nodes.tsv. The file is only
# replaced when the parsed node list actually changed, so an unchanged
# subscription does not trigger a session rebuild through settings_sig().
sync_nodes() {
  [[ -n "$RELAY_SUB_URL" ]] || return 0
  local raw="$RELAY_DIR/subscription.yaml.tmp" tmp="$NODES_FILE.tmp.$$" n=0
  if ! curl -fsS --max-time 30 "$RELAY_SUB_URL" -o "$raw" 2>/dev/null; then
    rm -f "$raw"
    log "subscription sync failed (keeping current node list)"
    return 1
  fi
  awk '
    function clean(s) { gsub(/\r/,"",s); gsub(/^[ \t]+|[ \t]+$/,"",s); gsub(/^"|"$/,"",s); return s }
    function flush() {
      if (name != "" && host != "" && pass != "" && typ == "trojan")
        printf "%s\t%s\t%s\t%s\t%s\t%s\n", name, host, (port==""?"443":port), (sni==""?host:sni), (path==""?"/pgtrojan":path), pass
      name=host=port=sni=path=pass=typ=""
    }
    /^[ \t]*- name:[ \t]*/   { flush(); v=$0; sub(/^[ \t]*- name:[ \t]*/,"",v); name=clean(v); next }
    /^[ \t]*type:[ \t]*/     { v=$0; sub(/^[ \t]*type:[ \t]*/,"",v); typ=clean(v); next }
    /^[ \t]*server:[ \t]*/   { v=$0; sub(/^[ \t]*server:[ \t]*/,"",v); host=clean(v); next }
    /^[ \t]*port:[ \t]*/     { v=$0; sub(/^[ \t]*port:[ \t]*/,"",v); port=clean(v); next }
    /^[ \t]*password:[ \t]*/ { v=$0; sub(/^[ \t]*password:[ \t]*/,"",v); pass=clean(v); next }
    /^[ \t]*sni:[ \t]*/      { v=$0; sub(/^[ \t]*sni:[ \t]*/,"",v); sni=clean(v); next }
    /^[ \t]*path:[ \t]*/     { v=$0; sub(/^[ \t]*path:[ \t]*/,"",v); path=clean(v); next }
    END { flush() }
  ' "$raw" >"$tmp" 2>/dev/null || { rm -f "$raw" "$tmp"; log "subscription parse failed"; return 1; }
  rm -f "$raw"
  [[ -s "$tmp" ]] && n=$(wc -l <"$tmp")
  if ((n < 1)); then
    rm -f "$tmp"
    log "subscription returned no trojan nodes (keeping current node list)"
    return 1
  fi
  if [[ -f "$NODES_FILE" ]] && cmp -s "$tmp" "$NODES_FILE"; then
    rm -f "$tmp"
    return 0
  fi
  chmod 600 "$tmp" 2>/dev/null || true
  mv -f "$tmp" "$NODES_FILE"
  log "subscription synced: $n nodes"
  return 0
}

settings_sig() {
  local sig="none"
  if [[ -f "$SETTINGS_FILE" ]]; then
    sig=$(printf '%s-%s' "$(stat -c %Y "$SETTINGS_FILE" 2>/dev/null || printf '0')" \
      "$(cksum <"$SETTINGS_FILE" 2>/dev/null | awk '{print $1"-"$2}')")
  fi
  if [[ -f "$NODES_FILE" ]]; then
    sig+='-n'
    sig+=$(printf '%s-%s' "$(stat -c %Y "$NODES_FILE" 2>/dev/null || printf '0')" \
      "$(cksum <"$NODES_FILE" 2>/dev/null | awk '{print $1"-"$2}')")
  fi
  printf '%s' "$sig"
}

load_secrets() {
  mkdir -p "$RELAY_DIR"
  chmod 700 "$RELAY_DIR" 2>/dev/null || true
  if [[ -f "$SECRETS_FILE" ]]; then
    # shellcheck disable=SC1090
    source "$SECRETS_FILE"
  fi
  if [[ -z "${SOCKS_PASS:-}" ]]; then
    SOCKS_PASS=$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')
  fi
  if [[ ! -f "$SECRETS_FILE" ]]; then
    printf 'SOCKS_PASS=%s\n' "$SOCKS_PASS" >"$SECRETS_FILE"
    chmod 600 "$SECRETS_FILE" 2>/dev/null || true
  fi
}

relay_enabled() {
  ((NODE_COUNT > 0)) && return 0
  [[ -n "$RELAY_HOST" && -n "$RELAY_PASSWORD" ]] && return 0
  return 1
}

relay_endpoint() { printf '%s:%s' "$RELAY_HOST" "$RELAY_PORT"; }

# Sessions are split into contiguous per-node blocks, so the first
# ceil(RELAY_COUNT/NODE_COUNT) sessions use node 1, the next block node 2,
# and so on. Reordering nodes.tsv therefore moves whole blocks, never a
# single live session in the middle of a line.
session_node_index() {
  local i=$1 per
  if ((NODE_COUNT > 0)); then
    per=$(( (RELAY_COUNT + NODE_COUNT - 1) / NODE_COUNT ))
    ((per > 0)) || per=1
    printf '%d' $(( (i - 1) / per + 1 ))
  else
    printf '1'
  fi
}

# node_field <session> <label|host|port|sni|ws|pass>
node_field() {
  local i=$1 f=$2 idx
  if ((NODE_COUNT > 0)); then
    idx=$(session_node_index "$i")
    idx=$((idx - 1))
    case "$f" in
      label) printf '%s' "${NODE_LABEL[$idx]}" ;;
      host) printf '%s' "${NODE_HOSTS[$idx]}" ;;
      port) printf '%s' "${NODE_PORTS[$idx]}" ;;
      sni) printf '%s' "${NODE_SNIS[$idx]}" ;;
      ws) printf '%s' "${NODE_WSPATHS[$idx]}" ;;
      pass) printf '%s' "${NODE_PASSWORDS[$idx]}" ;;
    esac
  else
    case "$f" in
      label) printf '%s' "${RELAY_NODE_NAME:-$RELAY_HOST}" ;;
      host) printf '%s' "$RELAY_HOST" ;;
      port) printf '%s' "$RELAY_PORT" ;;
      sni) printf '%s' "${RELAY_SNI:-$RELAY_HOST}" ;;
      ws) printf '%s' "$RELAY_WS_PATH" ;;
      pass) printf '%s' "$RELAY_PASSWORD" ;;
    esac
  fi
}

session_endpoint() {
  local i=$1
  printf '%s:%s' "$(node_field "$i" host)" "$(node_field "$i" port)"
}

node_name_for_port() {
  local port=$1 i
  i=$((port - SING_PORT_BASE + 1))
  if ((i >= 1 && i <= RELAY_COUNT)); then
    node_field "$i" label
  else
    printf '%s' "-"
  fi
}

# ---- status ----------------------------------------------------------------

status_set() {
  local port=$1
  shift
  local -a f=("$@")
  local out="" i v node="${f[5]:-}"
  for ((i = 0; i < 5; i++)); do
    v="${f[$i]:-}"
    [[ -n "$v" ]] || v="-"
    out+="${out:+$'\t'}$v"
  done
  [[ -n "$node" ]] || node=$(node_name_for_port "$port")
  STATUS["$port"]="$out"$'\t'"$(date +%s)"$'\t'"${node:--}"
}

flush_status() {
  local tmp="$STATE_DIR/status.tsv.tmp" port
  : >"$tmp"
  if ((${#STATUS[@]})); then
    while IFS= read -r port; do
      printf '%s\t%s\n' "$port" "${STATUS[$port]}" >>"$tmp"
    done < <(printf '%s\n' "${!STATUS[@]}" | sort -n)
  fi
  mv "$tmp" "$STATUS_FILE"
}

# ---- wgcf ------------------------------------------------------------------

toml_val() {
  sed -nE "s/^$1[[:space:]]*=[[:space:]]*['\"]?([^'\"]+)['\"]?[[:space:]]*\$/\\1/p" "$2" | head -1
}

account_valid() {
  local f="$1/wgcf-account.toml" k
  [[ -s "$f" ]] || return 1
  for k in device_id access_token private_key; do
    [[ -n "$(toml_val "$k" "$f")" ]] || return 1
  done
}

profile_valid() {
  local f="$1/wgcf-profile.conf"
  [[ -s "$f" ]] || return 1
  grep -q '^\[Interface\]' "$f" &&
    grep -q '^\[Peer\]' "$f" &&
    grep -q '^PrivateKey[[:space:]]*=' "$f" &&
    grep -q '^PublicKey[[:space:]]*=' "$f" &&
    grep -q '^Endpoint[[:space:]]*=' "$f"
}

register_session() {
  local i=$1 dir
  dir=$(sess_dir "$i")
  mkdir -p "$dir"
  chmod 700 "$dir" 2>/dev/null || true
  local attempt=0
  while ((attempt < REGISTER_RETRIES)); do
    attempt=$((attempt + 1))
    rm -f "$dir/wgcf-account.toml" "$dir/wgcf-profile.conf"
    log "session $(printf 'n%02d' "$i") register attempt ${attempt}/${REGISTER_RETRIES}"
    if (cd "$dir" && wgcf register --accept-tos --config wgcf-account.toml >>register.log 2>&1) &&
      account_valid "$dir"; then
      return 0
    fi
    sleep 5
  done
  return 1
}

generate_profile() {
  local i=$1 dir
  dir=$(sess_dir "$i")
  local attempt
  for attempt in 1 2 3; do
    if (cd "$dir" && wgcf generate --config wgcf-account.toml \
      --profile wgcf-profile.conf --keepalive "$WGCF_KEEPALIVE" \
      >>generate.log 2>&1) && profile_valid "$dir"; then
      return 0
    fi
    log "session $(printf 'n%02d' "$i") profile generate failed (attempt $attempt)"
    sleep 2
  done
  return 1
}

# ---- sing-box --------------------------------------------------------------

render_config() {
  local i=$1 dir=$2
  local profile="$dir/wgcf-profile.conf" cfg="$dir/sing-box.json"
  local priv peerpub addr6 peer_ep peer_port sni port rhost_ip
  priv=$(toml_val PrivateKey "$profile")
  peerpub=$(toml_val PublicKey "$profile")
  addr6=$(awk '
    /^[[:space:]]*Address[[:space:]]*=/ {
      line=$0
      sub(/^[[:space:]]*Address[[:space:]]*=[[:space:]]*/, "", line)
      n=split(line, parts, ",")
      for (i=1; i<=n; i++) {
        gsub(/[ \t]/, "", parts[i])
        if (parts[i] ~ /:/) { print parts[i]; exit }
      }
    }' "$profile")
  [[ -n "$priv" && -n "$peerpub" && -n "$addr6" ]] || return 1
  [[ "$addr6" == */* ]] || addr6="${addr6}/128"

  peer_ep=$(toml_val Endpoint "$profile")
  peer_port=${peer_ep##*:}
  [[ "$peer_port" =~ ^[0-9]+$ ]] || peer_port="$WG_ENDPOINT_PORT"
  peer_port=$((10#$peer_port))
  local nhost nport nsni nws npass
  nhost=$(node_field "$i" host)
  nport=$(node_field "$i" port)
  nsni=$(node_field "$i" sni)
  nws=$(node_field "$i" ws)
  npass=$(node_field "$i" pass)
  sni="$nsni"
  rhost_ip=$(getent ahostsv4 "$nhost" 2>/dev/null | awk '$2=="STREAM"{print $1; exit}' || true)
  [[ -n "$rhost_ip" ]] || rhost_ip="$nhost"
  port=$(sing_port "$i")

  if ! jq -n \
    --argjson port "$port" \
    --arg user "$SOCKS_USER" \
    --arg pass "$SOCKS_PASS" \
    --arg rhost "$rhost_ip" \
    --argjson rport "$((10#$nport))" \
    --arg rpass "$npass" \
    --arg sni "$sni" \
    --arg ws "$nws" \
    --arg priv "$priv" \
    --arg addr6 "$addr6" \
    --arg peerpub "$peerpub" \
    --arg peerhost "$WG_ENDPOINT_HOST" \
    --argjson peerport "$peer_port" \
    '{
      log: {level: "error"},
      dns: {
        servers: [{tag: "cf-v6", address: "tcp://[2606:4700:4700::1111]:53", detour: "wg"}],
        strategy: "ipv6_only"
      },
      inbounds: [{
        type: "socks", tag: "in", listen: "127.0.0.1", listen_port: $port,
        users: [{username: $user, password: $pass}]
      }],
      outbounds: [
        {
          type: "trojan", tag: "relay", server: $rhost, server_port: $rport,
          password: $rpass,
          tls: {enabled: true, server_name: $sni, utls: {enabled: true, fingerprint: "chrome"}},
          transport: {type: "ws", path: $ws, headers: {Host: $sni}}
        },
        {type: "direct", tag: "direct"}
      ],
      endpoints: [{
        type: "wireguard", tag: "wg", system: false,
        address: [$addr6],
        private_key: $priv,
        peers: [{
          address: $peerhost, port: $peerport, public_key: $peerpub,
          persistent_keepalive_interval: 25,
          allowed_ips: ["::/0"]
        }],
        mtu: 1280,
        detour: "relay"
      }],
      route: {
        rules: [{inbound: ["in"], outbound: "wg"}],
        final: "direct"
      }
    }' >"$cfg.tmp"; then
    rm -f "$cfg.tmp"
    return 1
  fi
  mv "$cfg.tmp" "$cfg"
  chmod 600 "$cfg" 2>/dev/null || true
  sing-box check -c "$cfg" >/dev/null 2>"$dir/check.log" || return 1
  return 0
}

stop_session() {
  local i=$1 dir pid
  dir=$(sess_dir "$i")
  local pf="$dir/sing-box.pid"
  [[ -f "$pf" ]] || return 0
  pid=$(cat "$pf" 2>/dev/null || true)
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    local n
    for n in {1..20}; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$pf"
}

wait_port() {
  local port=$1 timeout=${2:-15} i
  for ((i = 0; i < timeout * 10; i++)); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
      exec 3>&- 3<&-
      return 0
    fi
    sleep 0.1
  done
  return 1
}

start_session() {
  local i=$1 dir=$2 port=$3
  stop_session "$i"
  (
    cd "$dir" || exit 1
    exec sing-box run -c sing-box.json
  ) >>"$dir/sing-box.log" 2>&1 &
  local pid=$!
  printf '%s\n' "$pid" >"$dir/sing-box.pid"
  if wait_port "$port" 15; then
    return 0
  fi
  log "session $(printf 'n%02d' "$i") port $port did not listen; log tail:"
  tail -n 5 "$dir/sing-box.log" >&2 || true
  kill "$pid" 2>/dev/null || true
  rm -f "$dir/sing-box.pid"
  return 1
}

# ---- probe / dedup ---------------------------------------------------------

probe() {
  local port=$1 body ip loc prefix
  body=$(curl -sS --max-time "$RELAY_PROBE_TIMEOUT" \
    -x "socks5h://${SOCKS_USER}:${SOCKS_PASS}@127.0.0.1:${port}" \
    "$RELAY_PROBE_URL" 2>/dev/null) || return 1
  ip=""
  loc=""
  if [[ "$body" == *"ip="* ]]; then
    ip=$(printf '%s\n' "$body" | awk -F= '$1=="ip" { print $2; exit }')
    loc=$(printf '%s\n' "$body" | awk -F= '$1=="loc" { print $2; exit }')
  else
    ip=$(printf '%s' "$body" | tr -d '[:space:]')
  fi
  [[ "$ip" == *:* ]] || return 1
  prefix=$(printf '%s' "$ip" | awk -F: 'NF>=4 { print $1":"$2":"$3":"$4 }')
  [[ -n "$prefix" ]] || return 1
  printf '%s\t%s\t%s\n' "$prefix" "$ip" "${loc:--}"
}

valid_prefix() {
  local prefix=$1 p48 d
  [[ -n "$prefix" ]] || return 1
  [[ "${prefix,,}" == "${RELAY_PREFIX_ALLOW,,}"* ]] || return 2
  p48=$(printf '%s' "$prefix" | cut -d: -f1-3)
  for d in $DENY_48_PREFIXES; do
    [[ "$p48" == "$d"* ]] && return 2
  done
  return 0
}

bring_up() {
  local i=$1 dir port res prefix ip loc pid
  dir=$(sess_dir "$i")
  port=$(sing_port "$i")
  mkdir -p "$dir"
  chmod 700 "$dir" 2>/dev/null || true

  if ! relay_enabled; then
    status_set "$port" 0 "" "" "" "unconfigured"
    return 1
  fi
  if ! account_valid "$dir"; then
    if ! register_session "$i"; then
      status_set "$port" 0 "$(session_endpoint "$i")" "" "" "register-failed"
      return 1
    fi
  fi
  if ! profile_valid "$dir"; then
    if ! generate_profile "$i"; then
      status_set "$port" 0 "$(session_endpoint "$i")" "" "" "generate-failed"
      return 1
    fi
  fi
  if ! render_config "$i" "$dir"; then
    log "session $(printf 'n%02d' "$i") config render/check failed"
    status_set "$port" 0 "$(session_endpoint "$i")" "" "" "config-failed"
    return 1
  fi
  if ! start_session "$i" "$dir" "$port"; then
    status_set "$port" 0 "$(session_endpoint "$i")" "" "" "listen-failed"
    return 1
  fi
  pid=$(cat "$dir/sing-box.pid" 2>/dev/null || printf '0')
  res=""
  res=$(probe "$port" 2>/dev/null || true)
  prefix=""
  ip=""
  loc=""
  if [[ -n "$res" ]]; then
    IFS=$'\t' read -r prefix ip loc <<<"$res" || true
  fi
  if [[ -n "$prefix" ]] && valid_prefix "$prefix"; then
    status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "ok"
    log "up $(printf 'n%02d' "$i") port=$port prefix=$prefix"
    return 0
  fi
  status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "bad-prefix"
  log "up $(printf 'n%02d' "$i") port=$port prefix='${prefix:-none}' (invalid or duplicate; dedup will reroll)"
  return 0
}

recover_session() {
  local i=$1 dir
  dir=$(sess_dir "$i")
  stop_session "$i"
  rm -f "$dir/wgcf-account.toml" "$dir/wgcf-profile.conf"
  bring_up "$i"
}

restart_session() {
  local i=$1 dir
  dir=$(sess_dir "$i")
  local port
  port=$(sing_port "$i")
  if [[ -f "$dir/sing-box.json" ]]; then
    start_session "$i" "$dir" "$port"
  else
    bring_up "$i"
  fi
}

dedup_pass() {
  local -A seen=()
  local i port dir res prefix ip loc pid tries ok=0
  for ((i = 1; i <= RELAY_COUNT; i++)); do
    port=$(sing_port "$i")
    dir=$(sess_dir "$i")
    tries=0
    while :; do
      res=""
      res=$(probe "$port" 2>/dev/null || true)
      prefix=""
      ip=""
      loc=""
      if [[ -n "$res" ]]; then
        IFS=$'\t' read -r prefix ip loc <<<"$res" || true
      fi
      pid=$(cat "$dir/sing-box.pid" 2>/dev/null || printf '0')
      if [[ -n "$prefix" ]] && valid_prefix "$prefix" && [[ -z "${seen[$prefix]:-}" ]]; then
        seen["$prefix"]="$port"
        ok=$((ok + 1))
        status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "ok"
        break
      fi
      if [[ -z "$prefix" ]]; then
        if ((tries == 0)); then
          tries=1
          log "dedup $(printf 'n%02d' "$i") port=$port probe failed; retrying once"
          sleep 2
          continue
        fi
        status_set "$port" "$pid" "$(session_endpoint "$i")" "" "" "probe-failed"
        break
      fi
      if ((tries >= DEDUP_RETRIES)); then
        log "WARN $(printf 'n%02d' "$i") port=$port prefix='${prefix:-none}' still invalid or duplicate after ${DEDUP_RETRIES} rerolls"
        status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "bad"
        break
      fi
      tries=$((tries + 1))
      log "dedup $(printf 'n%02d' "$i") port=$port prefix='${prefix:-none}' try=$tries"
      recover_session "$i" || true
    done
  done
  flush_status
  log "relay unique /64: ${ok}/${RELAY_COUNT}"
}

health_pass() {
  local i port dir res prefix ip loc pid state changed=0
  local -A seen=()
  for ((i = 1; i <= RELAY_COUNT; i++)); do
    port=$(sing_port "$i")
    dir=$(sess_dir "$i")
    res=""
    res=$(probe "$port" 2>/dev/null || true)
    prefix=""
    ip=""
    loc=""
    if [[ -n "$res" ]]; then
      IFS=$'\t' read -r prefix ip loc <<<"$res" || true
    fi
    pid=$(cat "$dir/sing-box.pid" 2>/dev/null || printf '0')
    if [[ -n "$prefix" ]] && valid_prefix "$prefix" && [[ -z "${seen[$prefix]:-}" ]]; then
      seen["$prefix"]="$port"
      state=$(printf '%s' "${STATUS[$port]:-}" | cut -f5)
      [[ "$state" == "ok" ]] || changed=1
      status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "ok"
      continue
    fi
    changed=1
    if [[ -z "$prefix" ]]; then
      log "health: port $port probe failed; restarting sing-box"
      restart_session "$i" || true
    elif valid_prefix "$prefix"; then
      log "health: port $port duplicate /64 $prefix; keeping session"
      status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "bad"
      continue
    else
      log "health: port $port bad prefix $prefix; restarting sing-box"
      restart_session "$i" || true
    fi
    res=""
    res=$(probe "$port" 2>/dev/null || true)
    prefix=""
    ip=""
    loc=""
    if [[ -n "$res" ]]; then
      IFS=$'\t' read -r prefix ip loc <<<"$res" || true
    fi
    pid=$(cat "$dir/sing-box.pid" 2>/dev/null || printf '0')
    state="bad"
    if [[ -n "$prefix" ]] && valid_prefix "$prefix" && [[ -z "${seen[$prefix]:-}" ]]; then
      seen["$prefix"]="$port"
      state="ok"
    elif [[ -z "$prefix" ]]; then
      state="probe-failed"
    fi
    status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "$state"
  done
  flush_status
  ((changed)) && log "health: relay pool changed"
}

# ---- mux -------------------------------------------------------------------

stop_mux() {
  [[ -n "$MUX_PID" ]] || return 0
  if kill -0 "$MUX_PID" 2>/dev/null; then
    kill "$MUX_PID" 2>/dev/null || true
    local n
    for n in {1..30}; do
      kill -0 "$MUX_PID" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$MUX_PID" 2>/dev/null || true
  fi
  MUX_PID=""
}

start_mux() {
  local last_port=$((SING_PORT_BASE + RELAY_COUNT - 1))
  if ((MUX_PORT >= SING_PORT_BASE && MUX_PORT <= last_port)); then
    die "MUX_PORT ${MUX_PORT} overlaps session range ${SING_PORT_BASE}-${last_port}"
  fi
  log "starting socks mux on ${MUX_LISTEN}:${MUX_PORT} -> 127.0.0.1:${SING_PORT_BASE}-${last_port} (count=${RELAY_COUNT})"
  API_ALLOW_ANON="$API_ALLOW_ANON" RELAY_LENIENT_HEALTH="$RELAY_LENIENT_HEALTH" \
    /usr/local/bin/socksmux \
    -listen "${MUX_LISTEN}:${MUX_PORT}" \
    -backend-host 127.0.0.1 \
    -base-port "$SING_PORT_BASE" \
    -count "$RELAY_COUNT" \
    -upstream-user "$SOCKS_USER" \
    -upstream-pass "$SOCKS_PASS" \
    -client-pass "$SOCKS_PASS" \
    -api-token "${RELAY_API_TOKEN:-}" \
    -state-dir "$STATE_DIR" \
    -public-host "$RELAY_PUBLIC_HOST" \
    -private-host "$RELAY_PRIVATE_HOST" \
    -pool-id warp-relay \
    -rotation-interval 86400 \
    -reroll-batch 1 \
    -health-interval "$HEALTH_INTERVAL" \
    -min-healthy 1 \
    -mux-port "$MUX_PORT" >>"$STATE_DIR/socksmux.log" 2>&1 &
  MUX_PID=$!
  sleep 0.5
  if ! kill -0 "$MUX_PID" 2>/dev/null; then
    log "WARN socks mux failed to start; see $STATE_DIR/socksmux.log"
    MUX_PID=""
  fi
}

# ---- main ------------------------------------------------------------------

cleanup() {
  trap - EXIT INT TERM
  stop_mux
  local i
  for ((i = 1; i <= RELAY_COUNT; i++)); do
    stop_session "$i"
  done
}

start_all_sessions() {
  local i
  for ((i = 1; i <= RELAY_COUNT; i++)); do
    bring_up "$i" || log "WARN session $(printf 'n%02d' "$i") failed to come up"
    if ((i < RELAY_COUNT)); then
      sleep "$RELAY_START_STAGGER"
    fi
  done
  dedup_pass
}

mark_all() {
  local state=$1 i ep=""
  for ((i = 1; i <= RELAY_COUNT; i++)); do
    ep=""
    relay_enabled && ep=$(session_endpoint "$i")
    status_set "$(sing_port "$i")" 0 "$ep" "" "" "$state"
  done
  flush_status
}

validate_env() {
  [[ "$RELAY_COUNT" =~ ^[0-9]+$ ]] || die "RELAY_COUNT must be numeric"
  RELAY_COUNT=$((10#$RELAY_COUNT))
  ((RELAY_COUNT >= 1 && RELAY_COUNT <= 128)) || die "RELAY_COUNT must be 1-128"
  [[ "$SING_PORT_BASE" =~ ^[0-9]+$ ]] || die "SING_PORT_BASE must be numeric"
  SING_PORT_BASE=$((10#$SING_PORT_BASE))
  ((SING_PORT_BASE + RELAY_COUNT - 1 <= 65535)) || die "session port range overflows"
  [[ "$MUX_PORT" =~ ^[0-9]+$ ]] || die "MUX_PORT must be numeric"
  MUX_PORT=$((10#$MUX_PORT))
  [[ "$RELAY_PORT" =~ ^[0-9]+$ ]] || die "RELAY_PORT must be numeric"
  RELAY_PORT=$((10#$RELAY_PORT))
  [[ "$RELAY_START_STAGGER" =~ ^[0-9]+(\.[0-9]+)?$ ]] || die "RELAY_START_STAGGER must be numeric"
  command -v sing-box >/dev/null || die "sing-box not found"
  command -v wgcf >/dev/null || die "wgcf not found"
}

main() {
  mkdir -p "$SESSION_ROOT"
  chmod 700 "$RELAY_DIR" 2>/dev/null || true
  : >"$STATE_DIR/relay-mode"

  load_secrets
  reload_settings
  if [[ -n "$RELAY_SUB_URL" ]]; then
    sync_nodes || true
    reload_settings
  fi
  validate_env
  local sig
  sig=$(settings_sig)

  mark_all "unconfigured"
  start_mux

  if relay_enabled; then
    log "relay configured: nodes=${NODE_COUNT} host=$(node_field 1 host) sessions=${RELAY_COUNT}"
    start_all_sessions
  else
    log "relay not configured (missing nodes.tsv or host/password in $SETTINGS_FILE); mux and API only"
  fi

  local next_health next_sync now ns
  next_health=$(($(date +%s) + HEALTH_INTERVAL))
  next_sync=$(($(date +%s) + RELAY_SYNC_INTERVAL))
  while :; do
    sleep 5
    now=$(date +%s)
    if [[ -n "$RELAY_SUB_URL" ]] && ((now >= next_sync)); then
      next_sync=$((now + RELAY_SYNC_INTERVAL))
      if sync_nodes; then
        reload_settings
      fi
    fi
    ns=$(settings_sig)
    if [[ "$ns" != "$sig" ]]; then
      sig=$ns
      log "relay settings/nodes changed; rebuilding sessions"
      local i old_count=$RELAY_COUNT
      for ((i = 1; i <= old_count; i++)); do stop_session "$i"; done
      reload_settings
      validate_env
      if ((RELAY_COUNT != old_count)); then
        stop_mux
        start_mux
      fi
      if relay_enabled; then
        log "relay reconfigured: nodes=${NODE_COUNT} sessions=${RELAY_COUNT}"
        start_all_sessions
      else
        mark_all "unconfigured"
        log "relay disabled (missing host/password); mux and API only"
      fi
      next_health=$(($(date +%s) + HEALTH_INTERVAL))
      continue
    fi
    if relay_enabled; then
      now=$(date +%s)
      if ((now >= next_health)); then
        health_pass
        next_health=$(($(date +%s) + HEALTH_INTERVAL))
      fi
    fi
  done
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  trap 'cleanup; exit 0' INT TERM
  trap cleanup EXIT
  main "$@"
fi
