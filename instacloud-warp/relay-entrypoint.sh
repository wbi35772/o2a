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
# Parallel job width for bring-up, probe and recovery passes. Registration and
# probes are network bound; running RELAY_START_JOBS at a time keeps boot and
# recovery O(N/JOBS) instead of O(N). Raise it with the session count.
RELAY_START_JOBS="${RELAY_START_JOBS:-16}"
[[ "$RELAY_START_JOBS" =~ ^[0-9]+$ ]] || RELAY_START_JOBS=16
(( RELAY_START_JOBS >= 1 )) || RELAY_START_JOBS=16

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
RELAY_PLAIN_WS="${RELAY_PLAIN_WS:-0}"
RELAY_NODE_HOST_OVERRIDE="${RELAY_NODE_HOST_OVERRIDE:-}"
RELAY_NODE_PORT_OVERRIDE="${RELAY_NODE_PORT_OVERRIDE:-}"
RELAY_WG_ENDPOINTS="${RELAY_WG_ENDPOINTS:-162.159.192.1:2408 162.159.192.2:2408 162.159.193.1:2408 188.114.96.1:2408 188.114.97.1:2408 188.114.98.1:2408 188.114.99.1:2408}"
BURNED_FILE="${BURNED_FILE:-$STATE_DIR/burned-prefix64.tsv}"
REROLL_QUEUE="${REROLL_QUEUE:-$RELAY_DIR/reroll.queue}"
SESSION_MAP_FILE="${SESSION_MAP_FILE:-$RELAY_DIR/session-nodes.tsv}"
NODE_COOLING_SECONDS="${NODE_COOLING_SECONDS:-1800}"
RELAY_MAX_PER_NODE="${RELAY_MAX_PER_NODE:-8}"
RELAY_ACCT_REREG_AFTER="${RELAY_ACCT_REREG_AFTER:-2}"
RELAY_ACCT_REREG_COOLDOWN="${RELAY_ACCT_REREG_COOLDOWN:-900}"
REGISTER_TIMEOUT="${REGISTER_TIMEOUT:-20}"
GENERATE_TIMEOUT="${GENERATE_TIMEOUT:-15}"
RELAY_BACKOFF_BASE="${RELAY_BACKOFF_BASE:-120}"
RELAY_BACKOFF_MAX="${RELAY_BACKOFF_MAX:-1800}"
NODE_BAD_STREAK="${NODE_BAD_STREAK:-3}"
NODE_QUARANTINE_SECONDS="${NODE_QUARANTINE_SECONDS:-900}"
NODE_EVENTS_FILE="$STATE_DIR/node-events.log"
REGISTER_COOLDOWN="${REGISTER_COOLDOWN:-180}"
RELAY_REGISTER_JOBS="${RELAY_REGISTER_JOBS:-3}"
REGISTER_COOLDOWN_FILE="$STATE_DIR/register-cooldown"

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

# Auto session count follows nodes.tsv. RELAY_MAX_AUTO is only a memory guard:
# every session is one sing-box process plus its own wgcf account.
RELAY_MAX_AUTO="${RELAY_MAX_AUTO:-100}"
[[ "$RELAY_MAX_AUTO" =~ ^[0-9]+$ ]] || RELAY_MAX_AUTO=100

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
declare -A SESSION_NODE=()
declare -A NODE_FAILS=()
declare -A NODE_COOL=()
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
      if ((RELAY_COUNT > RELAY_MAX_AUTO)); then RELAY_COUNT=$RELAY_MAX_AUTO; fi
    else
      RELAY_COUNT=10
    fi
  fi
  [[ "$RELAY_COUNT" =~ ^[0-9]+$ ]] || RELAY_COUNT=10
  [[ "$RELAY_SYNC_INTERVAL" =~ ^[0-9]+$ ]] || RELAY_SYNC_INTERVAL=1800
  if ((10#$RELAY_SYNC_INTERVAL < 60)); then RELAY_SYNC_INTERVAL=60; fi
  load_session_map
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
merge_extra_nodes() {
  local extra="${RELAY_EXTRA_NODES_FILE:-/data/relay-extra-nodes.tsv}"
  [[ -s "$extra" && -s "$NODES_FILE" ]] || return 0
  local tmp="$NODES_FILE.merge.$$"
  if awk -v nmax="${RELAY_NODE_MAX:-64}" '
    FNR==NR { if (NF>=6) { k=$6; if (!(k in seen)) { seen[k]=1; print; cnt++ } } next }
    NF>=6 { k=$6; if (!(k in seen) && cnt+0 < nmax+0) { seen[k]=1; print; cnt++ } }
  ' "$NODES_FILE" "$extra" >"$tmp" 2>/dev/null; then
    mv -f "$tmp" "$NODES_FILE"
  else
    rm -f "$tmp"
  fi
}

sync_nodes() {
  [[ -n "$RELAY_SUB_URL" ]] || { merge_extra_nodes; return 0; }
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
  local extra="${RELAY_EXTRA_NODES_FILE:-/data/relay-extra-nodes.tsv}"
  if [[ -s "$extra" ]]; then
    awk -v nmax="${RELAY_NODE_MAX:-64}" '
      FNR==NR { if (NF>=6) { k=$6; if (!(k in seen)) { seen[k]=1; print; cnt++ } } next }
      NF>=6 { k=$6; if (!(k in seen) && cnt+0 < nmax+0) { seen[k]=1; print; cnt++ } }
    ' "$tmp" "$extra" >"$tmp.merge" && mv -f "$tmp.merge" "$tmp"
  fi
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

# Session -> upstream node mapping is explicit and persistent so a failed
# Trojan node can be replaced for one session without renumbering the rest.
load_session_map() {
  SESSION_NODE=()
  [[ -f "$SESSION_MAP_FILE" ]] || return 0
  local s n
  while IFS=$'\t' read -r s n || [[ -n "$s" ]]; do
    [[ "$s" =~ ^[0-9]+$ && "$n" =~ ^[0-9]+$ ]] || continue
    SESSION_NODE[$((10#$s))]=$((10#$n))
  done <"$SESSION_MAP_FILE"
}

save_session_map() {
  local i tmp="$SESSION_MAP_FILE.tmp.$$.$RANDOM"
  : >"$tmp"
  for ((i = 1; i <= RELAY_COUNT; i++)); do
    printf '%d\t%d\n' "$i" "$(session_node_index "$i")" >>"$tmp"
  done
  chmod 600 "$tmp" 2>/dev/null || true
  mv -f "$tmp" "$SESSION_MAP_FILE"
}

session_node_index() {
  local i=$1 idx nf
  nf="$(sess_dir "$i")/node"
  if [[ -f "$nf" ]]; then
    idx=$(cat "$nf" 2>/dev/null)
    if [[ "$idx" =~ ^[0-9]+$ ]]; then printf '%d' "$((10#$idx))"; return 0; fi
  fi
  if ((NODE_COUNT > 0)); then
    idx=${SESSION_NODE[$i]:-}
    if [[ -z "$idx" ]] || ((idx < 1 || idx > NODE_COUNT)); then
      idx=$(( (i - 1) % NODE_COUNT + 1 ))
    fi
    printf '%d' "$idx"
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
      host) if [[ -n "$RELAY_NODE_HOST_OVERRIDE" ]]; then printf '%s' "$RELAY_NODE_HOST_OVERRIDE"; else printf '%s' "${NODE_HOSTS[$idx]}"; fi ;;
      port) if [[ -n "$RELAY_NODE_HOST_OVERRIDE" ]]; then printf '%s' "${RELAY_NODE_PORT_OVERRIDE:-8090}"; else printf '%s' "${NODE_PORTS[$idx]}"; fi ;;
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
  local entry="$out"$'\t'"$(date +%s)"$'\t'"${node:--}"
  STATUS["$port"]="$entry"
  mkdir -p "$STATE_DIR/status.d"
  printf '%s\t%s\n' "$port" "$entry" >"$STATE_DIR/status.d/$port"
}

flush_status() {
  local tmp="$STATE_DIR/status.tsv.tmp" f
  : >"$tmp"
  if [[ -d "$STATE_DIR/status.d" ]]; then
    for f in "$STATE_DIR"/status.d/*; do
      [[ -f "$f" ]] || continue
      cat "$f" >>"$tmp"
    done
    sort -t$'\t' -k1,1n "$tmp" -o "$tmp" 2>/dev/null || true
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

register_cooling_left() {
  local ts now
  [[ -s "$REGISTER_COOLDOWN_FILE" ]] || { printf '0'; return 0; }
  ts=$(cat "$REGISTER_COOLDOWN_FILE" 2>/dev/null)
  [[ "$ts" =~ ^[0-9]+$ ]] || ts=0
  now=$(date +%s)
  if (( ts > now )); then printf '%s' $((ts - now)); else printf '0'; fi
}

register_cooling() {
  local left
  left=$(register_cooling_left)
  (( left > 0 ))
}

register_cooldown_set() {
  printf '%s
' "$(( $(date +%s) + REGISTER_COOLDOWN ))" >"$REGISTER_COOLDOWN_FILE"
}

reg_sem_acquire() {
  local k n=0
  mkdir -p "$STATE_DIR/regsem"
  while (( n < 150 )); do
    find "$STATE_DIR/regsem" -maxdepth 1 -name 's*' -mmin +2 -exec rmdir {} + 2>/dev/null || true
    for ((k = 1; k <= RELAY_REGISTER_JOBS; k++)); do
      if mkdir "$STATE_DIR/regsem/s$k" 2>/dev/null; then printf '%s
' "$k"; return 0; fi
    done
    sleep 0.2
    n=$((n + 1))
  done
  return 1
}

reg_sem_release() { rmdir "$STATE_DIR/regsem/s$1" 2>/dev/null || true; }

# ---- account freeze + proxied registration --------------------------------
now_cs() {
  local e=${EPOCHREALTIME:-0} s f
  s=${e%%.*}
  f=${e#*.}00
  printf '%s\n' "$(( 10#$s * 100 + 10#${f:0:2} ))"
}

to_cs() {
  local v=$1 a f=000
  a=${v%%.*}
  [[ "$v" == *.* ]] && f=${v#*.}00
  printf '%s' "$(( 10#$a * 100 + 10#${f:0:2} ))"
}

pause_cs() {
  local n=$1
  (( n > 0 )) || return 0
  sleep "$(( n / 100 )).$(printf '%02d' $(( n % 100 )))"
}

register_proxy_list() {
  local p
  for p in ${REGISTER_SOCKS_PORTS:-}; do
    [[ "$p" =~ ^[0-9]+$ ]] || continue
    printf '%s\n' "$p"
  done
  awk -F'\t' '($6=="ok") && $1 ~ /^[0-9]+$/ {print $1}' "$STATUS_FILE" 2>/dev/null || true
}

register_via_proxy() {
  local dir=$1 port=$2 tmp rc=1
  tmp=$(mktemp -d "${TMPDIR:-/tmp}/wgcf-reg-XXXXXX") || return 1
  if (cd "$tmp" && env -u ALL_PROXY -u HTTP_PROXY \
      HTTPS_PROXY="socks5h://${REGISTER_SOCKS_USER:-$SOCKS_USER}:${REGISTER_SOCKS_PASS:-$SOCKS_PASS}@127.0.0.1:${port}" \
      timeout "$REGISTER_TIMEOUT" wgcf register --accept-tos --config wgcf-account.toml >register.log 2>&1) &&
     [[ -s "$tmp/wgcf-account.toml" ]] &&
     (cd "$tmp" && timeout "$GENERATE_TIMEOUT" wgcf generate --config wgcf-account.toml \
      --profile wgcf-profile.conf --keepalive "$WGCF_KEEPALIVE" >>register.log 2>&1) &&
     [[ -s "$tmp/wgcf-profile.conf" ]]; then
    mkdir -p "$dir"
    mv -f "$tmp/wgcf-account.toml" "$tmp/wgcf-profile.conf" "$dir/"
    rc=0
  fi
  rm -rf "$tmp"
  return $rc
}

register_session() {
  local i=$1 dir
  dir=$(sess_dir "$i")
  mkdir -p "$dir"
  chmod 700 "$dir" 2>/dev/null || true
  if account_valid "$dir" && profile_valid "$dir"; then
    return 0
  fi
  if register_cooling; then
    log "session $(printf 'n%02d' "$i") register cooling ${REGISTER_COOLDOWN}s; skip"
    return 1
  fi
  local -a ports=()
  mapfile -t ports < <(register_proxy_list)
  ((${#ports[@]})) || ports=("")
  local n=${#ports[@]} attempt idx port sem rc
  for attempt in 1 2 3 4 5 6; do
    idx=$(( (i + attempt - 2) % n ))
    port=${ports[$idx]}
    sem=$(reg_sem_acquire) || sem=0
    log "session $(printf 'n%02d' "$i") register attempt $attempt via socks ${port:-direct}"
    if [[ -n "$port" ]]; then
      register_via_proxy "$dir" "$port"
      rc=$?
    else
      (cd "$dir" && timeout "$REGISTER_TIMEOUT" wgcf register --accept-tos --config wgcf-account.toml >>register.log 2>&1)
      rc=$?
    fi
    (( sem )) && reg_sem_release "$sem"
    if (( rc == 0 )) && account_valid "$dir" && profile_valid "$dir"; then
      log "session $(printf 'n%02d' "$i") account frozen (exit socks ${port:-direct})"
      return 0
    fi
    sleep 1
  done
  register_cooldown_set
  log "session $(printf 'n%02d' "$i") register failed after retries; cooling ${REGISTER_COOLDOWN}s"
  return 1
}

generate_profile() {
  local i=$1 dir
  dir=$(sess_dir "$i")
  local attempt
  for attempt in 1 2 3; do
    if (cd "$dir" && timeout "$GENERATE_TIMEOUT" wgcf generate --config wgcf-account.toml \
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
  local priv peerpub addr6 peer_ep peer_port peerhost peerport sni port rhost_ip
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

  peerhost="$WG_ENDPOINT_HOST"
  peerport="$WG_ENDPOINT_PORT"
  if [[ -n "$RELAY_WG_ENDPOINTS" ]]; then
    local ep_i ep_n
    ep_i=$(cat "$dir/endpoint.index" 2>/dev/null || printf '%d' "$(( i - 1 ))" )
    [[ "$ep_i" =~ ^[0-9]+$ ]] || ep_i=$(( i - 1 ))
    ep_n=$(printf '%s\n' $RELAY_WG_ENDPOINTS | wc -l)
    (( ep_n > 0 )) || ep_n=1
    ep_i=$(( ep_i % ep_n + 1 ))
    peer_ep=$(printf '%s\n' $RELAY_WG_ENDPOINTS | sed -n "${ep_i}p")
    peerhost=${peer_ep%:*}
    peerport=${peer_ep##*:}
  fi
  [[ "$peerport" =~ ^[0-9]+$ ]] || peerport="$WG_ENDPOINT_PORT"
  peerport=$((10#$peerport))
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
    --arg peerhost "$peerhost" \
    --argjson peerport "$peerport" \
    --arg plain "$RELAY_PLAIN_WS" \
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
        (if $plain == "1" then
          {
            type: "trojan", tag: "relay", server: $rhost, server_port: $rport,
            password: $rpass,
            transport: {type: "ws", path: $ws, headers: {Host: $sni}}
          }
        else
          {
            type: "trojan", tag: "relay", server: $rhost, server_port: $rport,
            password: $rpass,
            tls: {enabled: true, server_name: $sni, utls: {enabled: true, fingerprint: "chrome"}},
            transport: {type: "ws", path: $ws, headers: {Host: $sni}}
          }
        end),
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
  timeout 10 sing-box check -c "$cfg" >/dev/null 2>"$dir/check.log" || return 1
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

start_gate_wait() {
  local node=$1 f="$STATE_DIR/startgate" nf gap ngap last cs need
  mkdir -p "$f"
  gap=$(to_cs "${RELAY_START_SPACING:-1.5}")
  ngap=$(to_cs "${RELAY_START_NODE_GAP:-2}")
  nf="$f/node-$node"
  last=$(cat "$nf" 2>/dev/null || printf '0')
  [[ "$last" =~ ^[0-9]+$ ]] || last=0
  cs=$(now_cs)
  need=$(( last + ngap - cs ))
  pause_cs "$need"
  printf '%s\n' "$(now_cs)" >"$nf"
  local lt=0
  while ! mkdir "$f/pace.mk" 2>/dev/null; do
    sleep 0.1
    lt=$((lt + 1))
    (( lt > 300 )) && break
  done
  last=$(cat "$f/pace.ts" 2>/dev/null || printf '0')
  [[ "$last" =~ ^[0-9]+$ ]] || last=0
  cs=$(now_cs)
  need=$(( last + gap - cs ))
  pause_cs "$need"
  printf '%s\n' "$(now_cs)" >"$f/pace.ts"
  rmdir "$f/pace.mk" 2>/dev/null || true
}

start_gate_acquire() {
  local k conc=${RELAY_START_CONCURRENCY:-4} waited=0
  mkdir -p "$STATE_DIR/startgate"
  while :; do
    for ((k = 1; k <= conc; k++)); do
      if mkdir "$STATE_DIR/startgate/t$k" 2>/dev/null; then
        printf '%s' "$k"
        return 0
      fi
    done
    sleep 0.2
    waited=$((waited + 1))
    if (( waited >= 80 )); then
      printf 'skip'
      return 0
    fi
  done
}

start_session() {
  local i=$1 dir=$2 port=$3 token node
  node=$(session_node_index "$i" 2>/dev/null || printf '0')
  start_gate_wait "$node"
  token=$(start_gate_acquire)
  stop_session "$i"
  (
    cd "$dir" || exit 1
    exec sing-box run -c sing-box.json
  ) >>"$dir/sing-box.log" 2>&1 &
  local pid=$!
  printf '%s\n' "$pid" >"$dir/sing-box.pid"
  if wait_port "$port" 15; then
    [[ "$token" == "skip" ]] || rmdir "$STATE_DIR/startgate/t$token" 2>/dev/null || true
    return 0
  fi
  log "session $(printf 'n%02d' "$i") port $port did not listen; log tail:"
  tail -n 5 "$dir/sing-box.log" >&2 || true
  kill "$pid" 2>/dev/null || true
  rm -f "$dir/sing-box.pid"
  [[ "$token" == "skip" ]] || rmdir "$STATE_DIR/startgate/t$token" 2>/dev/null || true
  return 1
}

# ---- burned-64 / reroll queue / node rebind --------------------------------

burned_active() {
  local prefix=$1 now ts p
  [[ -f "$BURNED_FILE" ]] || return 1
  now=$(date +%s)
  while IFS=$'\t' read -r p ts || [[ -n "$p" ]]; do
    [[ "$p" == "$prefix" ]] || continue
    [[ "$ts" =~ ^[0-9]+$ ]] || continue
    (( now - ts < 86400 )) && return 0
  done <"$BURNED_FILE"
  return 1
}

prefix_usable() {
  local prefix=$1
  valid_prefix "$prefix" || return 1
  burned_active "$prefix" && return 2
  return 0
}

# ---- failure backoff + node scoring ---------------------------------------

slot_backoff_file() {
  printf '%s/backoff/%s' "$STATE_DIR" "$(printf 'n%02d' "$1")"
}

slot_fails_get() {
  cat "$(slot_backoff_file "$1")" 2>/dev/null || true
}

slot_backoff_ok() {
  rm -f "$(slot_backoff_file "$1")" 2>/dev/null || true
}

slot_backoff_fail() {
  local i=$1 f rec fails next delay
  f=$(slot_backoff_file "$i")
  mkdir -p "$STATE_DIR/backoff"
  rec=$(cat "$f" 2>/dev/null || true)
  fails=${rec%%$'\t'*}
  [[ "$fails" =~ ^[0-9]+$ ]] || fails=0
  fails=$((fails + 1))
  if (( fails > 6 )); then delay=$RELAY_BACKOFF_MAX; else delay=$((RELAY_BACKOFF_BASE * (1 << (fails - 1)))); fi
  (( delay > RELAY_BACKOFF_MAX )) && delay=$RELAY_BACKOFF_MAX
  next=$(($(date +%s) + delay))
  printf '%s\t%s\n' "$fails" "$next" >"$f.tmp.$$" && mv -f "$f.tmp.$$" "$f"
  log "backoff n$(printf '%02d' "$i") fails=$fails retry in ${delay}s"
}

slot_in_backoff() {
  local rec next
  rec=$(slot_fails_get "$1")
  [[ -n "$rec" ]] || return 1
  next=${rec##*$'\t'}
  [[ "$next" =~ ^[0-9]+$ ]] || return 1
  (( next > $(date +%s) ))
}

node_event() {
  local node=$1 kind=$2
  [[ "$kind" == "ok" || "$kind" == "fail" ]] || return 0
  printf '%s\t%s\t%s\n' "$(date +%s)" "$node" "$kind" >>"$NODE_EVENTS_FILE"
}

node_score() {
  local node=$1
  [[ -s "$NODE_EVENTS_FILE" ]] || { printf '0\t0\t0\n'; return 0; }
  awk -F'\t' -v n="$node" '
    $2 == n {
      if ($3 == "ok") { ok++; streak = 0 } else { fail++; streak++; last_fail = $1 }
    }
    END { printf "%d\t%d\t%d\n", ok * 2 - fail, last_fail + 0, streak }
  ' "$NODE_EVENTS_FILE" 2>/dev/null || printf '0\t0\t0\n'
}

node_quarantined() {
  local node=$1 score last_fail streak now
  read -r score last_fail streak <<<"$(node_score "$node")"
  now=$(date +%s)
  (( streak >= NODE_BAD_STREAK && last_fail > 0 && now - last_fail < NODE_QUARANTINE_SECONDS ))
}

node_choose() {
  local cur=$1 now j new best=0 bestscore=-2147483648 score _x _y i used=" " cap="${RELAY_MAX_PER_NODE:-8}" n_i
  now=$(date +%s)
  declare -A nuse=()
  for ((i = 1; i <= RELAY_COUNT; i++)); do
    (( i == cur )) && continue
    n_i=$(session_node_index "$i")
    [[ "$n_i" =~ ^[0-9]+$ ]] || n_i=${SESSION_NODE[i]:-0}
    used+="$n_i: "
    (( n_i > 0 )) && nuse[$n_i]=$(( ${nuse[$n_i]:-0} + 1 ))
  done
  for ((j = 1; j <= NODE_COUNT; j++)); do
    new=$(( (cur - 1 + j) % NODE_COUNT + 1 ))
    (( new == cur )) && continue
    (( ${NODE_COOL[$new]:-0} > now )) && continue
    node_quarantined "$new" && continue
    [[ "$used" == *" $new: "* ]] && continue
    (( ${nuse[$new]:-0} >= cap )) && continue
    read -r score _x _y <<<"$(node_score "$new")"
    [[ "$score" =~ ^-?[0-9]+$ ]] || score=0
    if (( score > bestscore )); then bestscore=$score; best=$new; fi
  done
  if (( best == 0 )); then
    for ((j = 1; j <= NODE_COUNT; j++)); do
      new=$(( (cur - 1 + j) % NODE_COUNT + 1 ))
      (( new == cur )) && continue
      (( ${nuse[$new]:-0} >= cap )) && continue
      read -r score _x _y <<<"$(node_score "$new")"
      [[ "$score" =~ ^-?[0-9]+$ ]] || score=0
      if (( score > bestscore )); then bestscore=$score; best=$new; fi
    done
  fi
  if (( best == 0 )); then
    for ((j = 1; j <= NODE_COUNT; j++)); do
      new=$(( (cur - 1 + j) % NODE_COUNT + 1 ))
      (( new == cur )) && continue
      read -r score _x _y <<<"$(node_score "$new")"
      [[ "$score" =~ ^-?[0-9]+$ ]] || score=0
      if (( score > bestscore )); then bestscore=$score; best=$new; fi
    done
  fi
  (( best > 0 )) || best=$(( cur % NODE_COUNT + 1 ))
  printf '%s\n' "$best"
}

_node_move_lock() {
  local lk="$STATE_DIR/rebind.lock" k=0
  while ! mkdir "$lk" 2>/dev/null; do
    k=$((k + 1))
    if (( k > 50 )); then rmdir "$lk" 2>/dev/null; mkdir "$lk" 2>/dev/null; break; fi
    sleep 0.1
  done
}

_node_move_drop() { rmdir "$STATE_DIR/rebind.lock" 2>/dev/null || true; }

_node_spread_precool() {
  local j u c now
  now=$(date +%s)
  local -A use=()
  for ((j = 1; j <= RELAY_COUNT; j++)); do
    u=$(session_node_index "$j")
    (( u >= 1 && u <= NODE_COUNT )) || continue
    use[$u]=$(( ${use[$u]:-0} + 1 ))
  done
  for u in "${!use[@]}"; do
    c=$(printf '%s' "${use[$u]:-0}")
    [[ "$c" =~ ^[0-9]+$ ]] || continue
    (( c >= 2 )) || continue
    (( ${NODE_COOL[$u]:-0} > now + 2 )) && continue
    NODE_COOL[$u]=$(( now + 1 ))
  done
}

rebind_node_only() {
  local i=$1 cur new now
  ((NODE_COUNT > 1)) || return 1
  _node_move_lock
  cur=$(session_node_index "$i")
  now=$(date +%s)
  NODE_COOL[$cur]=$((now + NODE_COOLING_SECONDS))
  _node_spread_precool
  new=$(node_choose "$cur")
  (( new != cur )) || new=$(( cur % NODE_COUNT + 1 ))
  SESSION_NODE[$i]=$new
  mkdir -p "$(sess_dir "$i")"
  printf '%d\n' "$new" >"$(sess_dir "$i")/node"
  save_session_map
  _node_move_drop
  log "node move n$(printf '%02d' "$i"): node $cur -> $new (cooldown ${NODE_COOLING_SECONDS}s)"
}

rebind_session() {
  local i=$1 cur new now
  ((NODE_COUNT > 1)) || return 1
  _node_move_lock
  cur=$(session_node_index "$i")
  now=$(date +%s)
  NODE_COOL[$cur]=$((now + NODE_COOLING_SECONDS))
  _node_spread_precool
  new=$(node_choose "$cur")
  (( new != cur )) || new=$(( cur % NODE_COUNT + 1 ))
  SESSION_NODE[$i]=$new
  mkdir -p "$(sess_dir "$i")"
  printf '%d\n' "$new" >"$(sess_dir "$i")/node"
  save_session_map
  _node_move_drop
  log "rebind n$(printf '%02d' "$i"): node $cur -> $new (score-based, old node cooling $NODE_COOLING_SECONDS s)"
  render_config "$i" "$(sess_dir "$i")" && restart_session "$i"
  return 0
}

process_reroll_queue() {
  [[ -s "$REROLL_QUEUE" ]] || return 0
  local i p ts n=0
  while IFS=$'\t' read -r i p ts || [[ -n "$i" ]]; do
    [[ "$i" =~ ^[0-9]+$ ]] || continue
    i=$((10#$i))
    (( i >= 1 && i <= RELAY_COUNT )) || continue
    log "reroll request n$(printf '%02d' "$i") prefix=${p:-?}"
    recover_session "$i" || true
    n=$((n + 1))
  done <"$REROLL_QUEUE"
  : >"$REROLL_QUEUE"
  if ((n > 0)); then
    log "reroll queue processed: $n session(s); dedup pass follows"
    dedup_pass
  fi
}

# ---- probe / dedup ---------------------------------------------------------

probe() {
  local port=$1 body ip loc prefix
  body=$(curl -sS --max-time "$RELAY_PROBE_TIMEOUT" \
    -x "socks5h://${SOCKS_USER}:${SOCKS_PASS}@127.0.0.1:${port}" \
    "$RELAY_PROBE_URL" 2>/dev/null) || return 1
  ip=""
  loc=""
  warp=""
  if [[ "$body" == *"ip="* ]]; then
    ip=$(printf '%s\n' "$body" | awk -F= '$1=="ip" { print $2; exit }')
    loc=$(printf '%s\n' "$body" | awk -F= '$1=="loc" { print $2; exit }')
    warp=$(printf '%s\n' "$body" | awk -F= '$1=="warp" { print $2; exit }')
  else
    ip=$(printf '%s' "$body" | tr -d '[:space:]')
  fi
  [[ "$warp" == "on" ]] || return 1
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

badacct_file() { printf '%s/badacct/%s' "$STATE_DIR" "$(printf 'n%02d' "$1")"; }

badacct_bump() {
  local i=$1 f c
  f=$(badacct_file "$i")
  mkdir -p "$STATE_DIR/badacct" 2>/dev/null || true
  c=$(cat "$f" 2>/dev/null || printf '0')
  [[ "$c" =~ ^[0-9]+$ ]] || c=0
  printf '%d
' "$((c + 1))" >"$f"
}

badacct_reset() { rm -f "$(badacct_file "$1")"; }

relay_ok_ratio_healthy() {
  local f ok=0 n=0
  for f in "$STATE_DIR/status.d"/*; do
    [[ -f "$f" ]] || continue
    n=$((n + 1))
    if awk -F'\t' '$6 == "ok" { found = 1 } END { exit !found }' "$f" 2>/dev/null; then
      ok=$((ok + 1))
    fi
  done
  (( n > 0 && ok * 4 >= n * 3 ))
}

replace_account() {
  local i=$1 dir bak
  dir=$(sess_dir "$i")
  if ! mkdir "$dir/acct.lock" 2>/dev/null; then
    log "session $(printf 'n%02d' "$i") account replace busy; skip"
    return 1
  fi
  bak="$dir/acct-replaced.$(date +%s)"
  mkdir -p "$bak" || { rmdir "$dir/acct.lock" 2>/dev/null; return 1; }
  mv "$dir/wgcf-account.toml" "$bak/" 2>/dev/null || true
  mv "$dir/wgcf-profile.conf" "$bak/" 2>/dev/null || true
  if register_session "$i" && generate_profile "$i"; then
    rm -rf "$bak"
    badacct_reset "$i"
    rmdir "$dir/acct.lock" 2>/dev/null || true
    log "session $(printf 'n%02d' "$i") account replaced; old account discarded"
    return 0
  fi
  mv "$bak/wgcf-account.toml" "$dir/" 2>/dev/null || true
  mv "$bak/wgcf-profile.conf" "$dir/" 2>/dev/null || true
  rmdir "$bak" 2>/dev/null || true
  rmdir "$dir/acct.lock" 2>/dev/null || true
  log "session $(printf 'n%02d' "$i") account replace failed; old account restored"
  return 1
}

maybe_replace_account() {
  local i=$1 f c now last
  f=$(badacct_file "$i")
  c=$(cat "$f" 2>/dev/null || printf '0')
  [[ "$c" =~ ^[0-9]+$ ]] || c=0
  (( c >= RELAY_ACCT_REREG_AFTER )) || return 1
  now=$(date +%s)
  last=$(cat "$f.replaced" 2>/dev/null || printf '0')
  [[ "$last" =~ ^[0-9]+$ ]] || last=0
  (( now - last >= RELAY_ACCT_REREG_COOLDOWN )) || return 1
  relay_ok_ratio_healthy || { log "session $(printf 'n%02d' "$i") account replace skipped (mass failure in progress)"; return 1; }
  printf '%d
' "$now" >"$f.replaced"
  replace_account "$i"
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
      slot_backoff_fail "$i"
      return 1
    fi
  fi
  if ! profile_valid "$dir"; then
    if ! generate_profile "$i"; then
      status_set "$port" 0 "$(session_endpoint "$i")" "" "" "generate-failed"
      slot_backoff_fail "$i"
      return 1
    fi
  fi
  if ! render_config "$i" "$dir"; then
    log "session $(printf 'n%02d' "$i") config render/check failed"
    status_set "$port" 0 "$(session_endpoint "$i")" "" "" "config-failed"
    slot_backoff_fail "$i"
    return 1
  fi
  if ! start_session "$i" "$dir" "$port"; then
    status_set "$port" 0 "$(session_endpoint "$i")" "" "" "listen-failed"
    slot_backoff_fail "$i"
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
  if [[ -n "$prefix" ]] && prefix_usable "$prefix"; then
    status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "ok"
    slot_backoff_ok "$i"
    node_event "$(session_node_index "$i")" ok
    badacct_reset "$i"
    log "up $(printf 'n%02d' "$i") port=$port prefix=$prefix"
    return 0
  fi
  if [[ -z "$prefix" ]]; then
    badacct_bump "$i"
    slot_backoff_fail "$i"
    node_event "$(session_node_index "$i")" fail
  fi
  status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "bad-prefix"
  log "up $(printf 'n%02d' "$i") port=$port prefix='${prefix:-none}' (invalid or duplicate; dedup will reroll)"
  return 0
}

endpoint_rotate() {
  local i=$1 dir idx
  dir=$(sess_dir "$i")
  idx=$(cat "$dir/endpoint.index" 2>/dev/null || printf '0')
  [[ "$idx" =~ ^[0-9]+$ ]] || idx=0
  printf '%s\n' "$((idx + 1))" >"$dir/endpoint.index"
  render_config "$i" "$dir"
}

rotate_session() {
  local i=$1 port res
  port=$(sing_port "$i")
  log "rotate n$(printf '%02d' "$i"): endpoint cycle (account kept)"
  if endpoint_rotate "$i"; then
    if start_session "$i" "$(sess_dir "$i")" "$port"; then
      res=$(probe "$port" 2>/dev/null || true)
      if [[ -n "$res" ]]; then
        slot_backoff_ok "$i"
        node_event "$(session_node_index "$i")" ok
        return 0
      fi
    fi
  fi
  log "rotate n$(printf '%02d' "$i") failed; rebinding node"
  rebind_session "$i" || true
}

recover_session() {
  local i=$1 mode=${2:-soft} dir res
  dir=$(sess_dir "$i")
  if [[ "$mode" != "force" && -f "$dir/wgcf-profile.conf" ]]; then
    if restart_session "$i"; then
      res=$(probe "$(sing_port "$i")" 2>/dev/null || true)
      [[ -n "$res" ]] && return 0
    fi
    log "soft recover n$(printf 'n%02d' "$i") restart failed; account kept, rotation expected"
  fi
  stop_session "$i"
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
  local workdir="$STATE_DIR/dedup-work"
  local max_rounds="${DEDUP_RETRIES:-4}"
  [[ "$max_rounds" =~ ^[0-9]+$ ]] || max_rounds=4
  local round i port res prefix ip loc pid ok
  local -a idx=() reroll=()
  for ((i = 1; i <= RELAY_COUNT; i++)); do idx+=("$i"); done
  for ((round = 0; round <= max_rounds; round++)); do
    rm -rf "$workdir"
    mkdir -p "$workdir"
    WORKER_FN=_worker_probe
    _pool_run "$workdir" "${idx[@]}"
    local -A seen=()
    reroll=()
    ok=0
    for i in "${idx[@]}"; do
      [[ -f "$workdir/probe.$i" ]] || continue
      IFS=$'\t' read -r _ port res <"$workdir/probe.$i"
      prefix=""
      ip=""
      loc=""
      if [[ -n "$res" ]]; then
        IFS=$'\t' read -r prefix ip loc <<<"$res" || true
      fi
      pid=$(cat "$(sess_dir "$i")/sing-box.pid" 2>/dev/null || printf '0')
      if [[ -n "$prefix" ]] && prefix_usable "$prefix" && [[ -z "${seen[$prefix]:-}" ]]; then
        seen["$prefix"]="$port"
        ok=$((ok + 1))
        status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "ok"
        continue
      fi
      if [[ -z "$prefix" ]]; then
        status_set "$port" "$pid" "$(session_endpoint "$i")" "" "" "probe-failed"
        reroll+=("$i")
        continue
      fi
      status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "bad"
      reroll+=("$i")
    done
    flush_status
    log "dedup round $round: unique /64 ${ok}/${RELAY_COUNT}, reroll=${#reroll[@]}"
    (( ${#reroll[@]} == 0 )) && break
    WORKER_FN=_worker_reroll
    _pool_run "$workdir" "${reroll[@]}"
    _pool_collect "$workdir"
    flush_status
    if register_cooling; then
      log "dedup paused: register cooling $(register_cooling_left)s"
      break
    fi
  done
  (( ${#reroll[@]} == 0 )) || log "WARN dedup: ${#reroll[@]} session(s) still duplicate or down"
}


health_pass() {
  local workdir="$STATE_DIR/health-work"
  local i port res prefix ip loc pid state changed=0 ok=0
  local -a idx=() ops=()
  for ((i = 1; i <= RELAY_COUNT; i++)); do idx+=("$i"); done
  rm -rf "$workdir"
  mkdir -p "$workdir"
  WORKER_FN=_worker_probe
  _pool_run "$workdir" "${idx[@]}"
  local -A seen=()
  for i in "${idx[@]}"; do
    port=$(sing_port "$i")
    res=""
    [[ -f "$workdir/probe.$i" ]] && IFS=$'\t' read -r _ port res <"$workdir/probe.$i"
    prefix=""
    ip=""
    loc=""
    if [[ -n "$res" ]]; then IFS=$'\t' read -r prefix ip loc <<<"$res" || true; fi
    pid=$(cat "$(sess_dir "$i")/sing-box.pid" 2>/dev/null || printf '0')
    if [[ -n "$prefix" ]] && prefix_usable "$prefix" && [[ -z "${seen[$prefix]:-}" ]]; then
      seen["$prefix"]="$port"
      ok=$((ok + 1))
      state=$(printf '%s' "${STATUS[$port]:-}" | cut -f5)
      [[ "$state" == "ok" ]] || changed=1
      status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "ok"
      NODE_FAILS[$i]=0
      continue
    fi
    changed=1
    if [[ -z "$prefix" ]]; then
      NODE_FAILS[$i]=$(( ${NODE_FAILS[$i]:-0} + 1 ))
      if (( ${NODE_FAILS[$i]} >= 2 )); then
        NODE_FAILS[$i]=0
        ops+=("rebind:$i")
      else
        ops+=("restart:$i")
      fi
    elif burned_active "$prefix" || [[ -n "${seen[$prefix]:-}" ]]; then
      ops+=("recover:$i")
    elif ! valid_prefix "$prefix"; then
      ops+=("restart:$i")
    else
      ops+=("recover:$i")
    fi
    status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "bad"
  done
  if (( ${#ops[@]} )); then
    WORKER_FN=_worker_health_fix
    local -a specs=("${ops[@]}")
    _pool_run "$workdir" "${specs[@]}"
    local o
    for o in "${specs[@]}"; do
      i=${o#*:}
      i=$((10#$i))
      port=$(sing_port "$i")
      res=""
      [[ -f "$workdir/probe.$i" ]] && IFS=$'\t' read -r _ port res <"$workdir/probe.$i"
      prefix=""
      ip=""
      loc=""
      if [[ -n "$res" ]]; then IFS=$'\t' read -r prefix ip loc <<<"$res" || true; fi
      pid=$(cat "$(sess_dir "$i")/sing-box.pid" 2>/dev/null || printf '0')
      state="bad"
      if [[ -n "$prefix" ]] && prefix_usable "$prefix" && [[ -z "${seen[$prefix]:-}" ]]; then
        seen["$prefix"]="$port"
        state="ok"
      elif [[ -z "$prefix" ]]; then
        state="probe-failed"
      fi
      status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "$state"
    done
  fi
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


# ---- bounded parallel executor -----------------------------------------
# Session passes run one worker per slot, RELAY_START_JOBS at a time. Workers
# touch only per-session files; the parent merges their status lines so
# STATUS/status.tsv keeps a single writer.
_pool_run() {
  local workdir=$1
  shift
  local -a pids=()
  local idx
  for idx in "$@"; do
    ( "$WORKER_FN" "$idx" "$workdir" ) &
    pids+=("$!")
    while (( ${#pids[@]} >= RELAY_START_JOBS )); do
      wait "${pids[0]}" 2>/dev/null || true
      pids=("${pids[@]:1}")
    done
  done
  for idx in "${pids[@]}"; do
    wait "$idx" 2>/dev/null || true
  done
}

_worker_status_shadow() {
  local workdir=$1
  status_set() {
    local p=$1
    shift
    local -a f=("$@")
    local out="" n v node="${f[5]:-}"
    for ((n = 0; n < 5; n++)); do
      v="${f[$n]:-}"
      [[ -n "$v" ]] || v="-"
      out+="${out:+$'\t'}$v"
    done
    [[ -n "$node" ]] || node=$(node_name_for_port "$p")
    mkdir -p "$STATE_DIR/status.d"
    printf '%s\t%s\t%s\t%s\n' "$p" "$out" "$(date +%s)" "${node:--}" >"$STATE_DIR/status.d/$p"
  }
}

_pool_collect() {
  local workdir=$1 f line port
  for f in "$workdir"/status.*; do
    [[ -e "$f" ]] || continue
    line=$(cat "$f")
    port=${line%%$'\t'*}
    [[ "$port" =~ ^[0-9]+$ ]] && STATUS["$port"]="${line#*$'\t'}"
    rm -f "$f"
  done
}

_worker_bring_up() {
  local i=$1 workdir=$2
  _worker_status_shadow "$workdir"
  bring_up "$i" || log "WARN session $(printf 'n%02d' "$i") failed to come up"
}

_worker_probe() {
  local i=$1 workdir=$2 port res
  port=$(sing_port "$i")
  res=$(probe "$port" 2>/dev/null || true)
  printf '%s\t%s\t%s\n' "$i" "$port" "$res" >"$workdir/probe.$i"
}

_worker_reroll() {
  local i=$1 workdir=$2 fails
  _worker_status_shadow "$workdir"
  fails=$(slot_fails_get "$i" 2>/dev/null | cut -f1)
  [[ "$fails" =~ ^[0-9]+$ ]] || fails=0
  if (( fails >= 2 )) && (( NODE_COUNT > 1 )); then
    log "dedup reroll n$(printf '%02d' "$i") fails=$fails; rebind node (account kept)"
    rebind_session "$i" || true
    return 0
  fi
  log "dedup reroll n$(printf '%02d' "$i") fails=$fails; rotating endpoint (account kept)"
  rotate_session "$i" || true
}

_worker_health_fix() {
  local spec=$1 workdir=$2 i port res
  i=${spec#*:}
  i=$((10#$i))
  port=$(sing_port "$i")
  case "${spec%%:*}" in
    restart) restart_session "$i" || true ;;
    rebind) rebind_session "$i" || true ;;
    recover) rotate_session "$i" || true ;;
  esac
  res=$(probe "$port" 2>/dev/null || true)
  printf '%s\t%s\t%s\n' "$i" "$port" "$res" >"$workdir/probe.$i"
}


start_all_sessions() {
  local i workdir="$STATE_DIR/boot-work"
  local -a idx=()
  for ((i = 1; i <= RELAY_COUNT; i++)); do idx+=("$i"); done
  rm -rf "$STATE_DIR/backoff"
  rm -rf "$workdir"
  mkdir -p "$workdir"
  WORKER_FN=_worker_bring_up
  _pool_run "$workdir" "${idx[@]}"
  _pool_collect "$workdir"
  rm -rf "$workdir"
  flush_status
  log "boot: ${RELAY_COUNT} session(s) brought up"
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
  ((RELAY_COUNT >= 1 && RELAY_COUNT <= 4096)) || die "RELAY_COUNT must be 1-128"
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
  local _lf
  for _lf in relay.log watchdog.log; do
    if [[ -f "$STATE_DIR/$_lf" ]] && (( $(stat -c %s "$STATE_DIR/$_lf" 2>/dev/null || echo 0) > 8388608 )); then
      mv -f "$STATE_DIR/$_lf" "$STATE_DIR/$_lf.1" 2>/dev/null || true
      : >"$STATE_DIR/$_lf"
    fi
  done
  : >"$STATE_DIR/relay-mode"

  load_secrets
  merge_extra_nodes || true
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

  local next_health next_sync next_dedup now ns nodes_stamp cur_nodes
  nodes_stamp=$(stat -c %Y "$NODES_FILE" 2>/dev/null || printf 0)
  nodes_stamp+="-$(cksum <"$NODES_FILE" 2>/dev/null | awk '{print $1"-"$2}')"
  next_health=$(($(date +%s) + HEALTH_INTERVAL))
  next_sync=$(($(date +%s) + RELAY_SYNC_INTERVAL))
  next_dedup=$(($(date +%s) + ${DEDUP_INTERVAL:-900}))
  while :; do
    sleep 5
    now=$(date +%s)
    cur_nodes=$(stat -c %Y "$NODES_FILE" 2>/dev/null || printf 0)
    cur_nodes+="-$(cksum <"$NODES_FILE" 2>/dev/null | awk '{print $1"-"$2}')"
    if [[ "$cur_nodes" != "$nodes_stamp" ]]; then
      nodes_stamp=$cur_nodes
      load_nodes
      log "nodes changed: $NODE_COUNT nodes (soft reload; live sessions kept)"
    fi
    if [[ -s "$REROLL_QUEUE" ]]; then
      process_reroll_queue
      next_health=$(($(date +%s) + HEALTH_INTERVAL))
      continue
    fi
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
      if ((now >= next_dedup)); then
        dedup_pass
        next_dedup=$(($(date +%s) + ${DEDUP_INTERVAL:-900}))
      fi
    fi
  done
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  trap 'cleanup; exit 0' INT TERM
  trap cleanup EXIT
  main "$@"
fi
