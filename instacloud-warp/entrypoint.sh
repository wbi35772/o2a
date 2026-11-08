#!/usr/bin/env bash
# warp-pool-producer (Session B)
# Chained WARP registration -> pure-IPv6 wireproxy SOCKS5 endpoints.

set -Eeuo pipefail
umask 022

STATE_DIR="${STATE_DIR:-/data}"
START_PORT="${START_PORT:-1081}"
INSTANCE_COUNT="${INSTANCE_COUNT:-${POOL_SIZE:-16}}"
POOL_SIZE="${POOL_SIZE:-$INSTANCE_COUNT}"
SOCKS_USER="${SOCKS_USER:?SOCKS_USER must be set}"
SOCKS_PASS="${SOCKS_PASS:?SOCKS_PASS must be set}"
REROLL_INTERVAL="${REROLL_INTERVAL:-900}"
REROLL_BATCH="${REROLL_BATCH:-4}"
DEDUP_RETRIES="${DEDUP_RETRIES:-4}"
REGISTER_RETRIES="${REGISTER_RETRIES:-3}"
PROBE_URL="${PROBE_URL:-https://opencode.ai/cdn-cgi/trace}"
PROBE_TIMEOUT="${PROBE_TIMEOUT:-6}"
HEALTH_INTERVAL="${HEALTH_INTERVAL:-300}"
ENDPOINTS_FILE="${ENDPOINTS_FILE:-/usr/local/share/warp-pool/endpoints.txt}"
EXTERNAL_HOST="${EXTERNAL_HOST:-}"
KEEPALIVE="${WGCF_KEEPALIVE:-25}"
MUX_PORT="${MUX_PORT:-}"
MUX_LISTEN="${MUX_LISTEN:-0.0.0.0}"
WIREPROXY_BIND_HOST="${WIREPROXY_BIND_HOST:-0.0.0.0}"
DENY_48_PREFIXES="${DENY_48_PREFIXES:-2a09:bac5:55 2a09:bac1:65 2a09:bac5:c82 2a09:bac1:27b 2a09:bac1:1e}"
API_TOKEN="${API_TOKEN:-${POOL_API_TOKEN:-}}"
POOL_ID="${POOL_ID:-$(hostname 2>/dev/null || printf 'warp-pool')}"
POOL_PUBLIC_HOST="${POOL_PUBLIC_HOST:-}"
POOL_PRIVATE_HOST="${POOL_PRIVATE_HOST:-}"
MIN_HEALTHY="${MIN_HEALTHY:-14}"
CONTROL_DIR="$STATE_DIR/control"
QUEUE_DIR="$CONTROL_DIR/queue"
DISABLED_FILE="$CONTROL_DIR/disabled"
CONFIG_FILE="$CONTROL_DIR/config.json"
SCALE_FILE="$CONTROL_DIR/scale"

log() { printf '[%s] %s\n' "$(date '+%Y-%m-%d %H:%M:%S')" "$*" >&2; }
die() { log "FATAL: $*"; exit 1; }

declare -a ENDPOINTS=()
declare -A EP_USED=()
declare -A STATUS=()
declare -a WIREPROXY_PIDS=()
LAST_WORKING_PORT=""
PUBLIC_HOST=""
MUX_PID=""
FORCE_REROLL=0

inst_name() { printf 'n%02d' "$1"; }
inst_dir() { printf '%s/instances/%s' "$STATE_DIR" "$(inst_name "$1")"; }
port_for() { printf '%d' $((START_PORT + $1 - 1)); }

LAST_ROTATION=""
ROTATION_CHANGED=0

load_runtime_config() {
  [[ -f "$CONFIG_FILE" ]] || return 0
  local v
  v=$(jq -r '.rotation_interval_seconds // empty' "$CONFIG_FILE" 2>/dev/null || true)
  [[ "$v" =~ ^[0-9]+$ ]] && REROLL_INTERVAL="$v"
  v=$(jq -r '.reroll_batch // empty' "$CONFIG_FILE" 2>/dev/null || true)
  [[ "$v" =~ ^[0-9]+$ ]] && REROLL_BATCH="$v"
  v=$(jq -r '.health_interval_seconds // empty' "$CONFIG_FILE" 2>/dev/null || true)
  [[ "$v" =~ ^[0-9]+$ ]] && HEALTH_INTERVAL="$v"
  v=$(jq -r '.min_healthy // empty' "$CONFIG_FILE" 2>/dev/null || true)
  [[ "$v" =~ ^[0-9]+$ ]] && MIN_HEALTHY="$v"
  if [[ "$REROLL_INTERVAL" != "$LAST_ROTATION" ]]; then
    LAST_ROTATION="$REROLL_INTERVAL"
    ROTATION_CHANGED=1
  fi
}

load_scale() {
  [[ -f "$SCALE_FILE" ]] || return 0
  local v
  v=$(tr -dc '0-9' <"$SCALE_FILE" 2>/dev/null || true)
  if [[ -n "$v" ]] && ((10#$v >= 1 && 10#$v <= 1024)); then
    INSTANCE_COUNT=$((10#$v))
    POOL_SIZE="$INSTANCE_COUNT"
    log "scale: persisted instance count ${INSTANCE_COUNT}"
  fi
}

# scale_pool changes the live node count, then restarts the mux so its fan-out
# list matches. Accounts and profiles on disk are kept, so scaling back up
# reuses the existing WARP registrations.
scale_pool() {
  local target=$1 old=$INSTANCE_COUNT i port dir ep_count
  [[ "$target" =~ ^[0-9]+$ ]] || { log "scale: invalid count '$target'"; return 1; }
  target=$((10#$target))
  ((target >= 1 && target <= 1024)) || { log "scale: count must be 1-1024"; return 1; }
  ep_count=$(grep -cEv '^[[:space:]]*(#|$)' "$ENDPOINTS_FILE" 2>/dev/null || printf '0')
  ((target <= ep_count)) || { log "scale: target $target exceeds endpoint pool ($ep_count)"; return 1; }
  if [[ -n "$MUX_PORT" ]]; then
    local last=$((START_PORT + target - 1))
    if ((MUX_PORT >= START_PORT && MUX_PORT <= last)); then
      log "scale: MUX_PORT $MUX_PORT conflicts with node ports $START_PORT-$last"
      return 1
    fi
  fi
  if ((target == old)); then
    log "scale: already at $target instances"
    return 0
  fi
  mkdir -p "$CONTROL_DIR"
  printf '%s\n' "$target" >"$SCALE_FILE"
  log "scale: $old -> $target instances"
  if ((target > old)); then
    for ((i = old + 1; i <= target; i++)); do
      bring_up "$i" || log "WARN scale: $(inst_name "$i") failed to come up"
    done
  else
    for ((i = target + 1; i <= old; i++)); do
      port=$(port_for "$i")
      dir=$(inst_dir "$i")
      stop_wireproxy "$dir"
      unset "STATUS[$port]" "EP_USED[$port]"
      log "scale: node $(inst_name "$i") stopped (port $port)"
    done
    if [[ -n "$LAST_WORKING_PORT" ]] && ((LAST_WORKING_PORT > START_PORT + target - 1)); then
      LAST_WORKING_PORT=""
    fi
  fi
  INSTANCE_COUNT=$target
  POOL_SIZE=$target
  stop_mux
  start_mux
  dedup_pass
  write_egress
  flush_status
  log "scale: done, ${INSTANCE_COUNT} instances on ports ${START_PORT}-$((START_PORT + INSTANCE_COUNT - 1))"
}

user_index() {
  local u=$1 n
  [[ "$u" == "$SOCKS_USER"* ]] || return 1
  n=${u#"$SOCKS_USER"}
  [[ "$n" =~ ^[0-9]+$ ]] || return 1
  n=$((10#$n))
  ((n >= 1 && n <= INSTANCE_COUNT)) || return 1
  printf '%d' "$n"
}

is_disabled() {
  [[ -f "$DISABLED_FILE" ]] || return 1
  grep -qxF -- "$1" "$DISABLED_FILE"
}

set_disabled() {
  local u=$1 mode=$2 tmp
  mkdir -p "$CONTROL_DIR"
  touch "$DISABLED_FILE"
  if [[ "$mode" == "on" ]]; then
    grep -qxF -- "$u" "$DISABLED_FILE" || printf '%s\n' "$u" >>"$DISABLED_FILE"
    return 0
  fi
  tmp="$DISABLED_FILE.tmp"
  grep -vxF -- "$u" "$DISABLED_FILE" >"$tmp" || true
  mv "$tmp" "$DISABLED_FILE"
}

stop_node() {
  local i=$1 dir port
  dir=$(inst_dir "$i")
  port=$(port_for "$i")
  stop_wireproxy "$dir"
  status_set "$port" 0 "${EP_USED[$port]:-}" "" "" "disabled"
  flush_status
  log "node $(inst_name "$i") disabled (port $port stopped)"
}

start_node() {
  local i=$1 user dir port
  dir=$(inst_dir "$i")
  port=$(port_for "$i")
  user="${SOCKS_USER}$(printf '%02d' "$i")"
  set_disabled "$user" off
  if bring_up "$i"; then
    log "node $(inst_name "$i") enabled"
  else
    log "WARN node $(inst_name "$i") enable failed"
  fi
  flush_status
}

process_queue() {
  local f cmd action user idx
  [[ -d "$QUEUE_DIR" ]] || return 0
  while IFS= read -r f; do
    [[ -f "$f" ]] || continue
    cmd=$(head -n1 "$f" 2>/dev/null || true)
    rm -f "$f"
    action=${cmd%% *}
    user=${cmd#* }
    case "$action" in
      reroll-all)
        log "control: reroll-all requested"
        FORCE_REROLL=1
        ;;
      reload-config)
        log "control: reload-config requested"
        load_runtime_config
        ;;
      rotate)
        if idx=$(user_index "$user"); then
          is_disabled "$user" && { log "control: rotate $user ignored (disabled)"; continue; }
          log "control: rotate $user"
          reroll_instance "$idx" || log "control: rotate $user failed"
          write_egress
        fi
        ;;
      enable)
        if idx=$(user_index "$user"); then
          log "control: enable $user"
          start_node "$idx"
          write_egress
        fi
        ;;
      disable)
        if idx=$(user_index "$user"); then
          log "control: disable $user"
          set_disabled "$user" on
          stop_node "$idx"
          write_egress
        fi
        ;;
      scale)
        log "control: scale to $user"
        scale_pool "$user" || log "control: scale to $user failed"
        ;;
      *)
        [[ -n "$cmd" ]] && log "control: unknown command '$cmd'"
        ;;
    esac
  done < <(find "$QUEUE_DIR" -maxdepth 1 -type f -name '*.cmd' 2>/dev/null | sort)
}

status_set() {
  local port=$1; shift
  local -a f=("$@")
  local out="" i v
  for ((i = 0; i < 5; i++)); do
    v="${f[$i]:-}"
    [[ -n "$v" ]] || v="-"
    out+="${out:+$'\t'}$v"
  done
  STATUS["$port"]="$out"$'\t'"$(date +%s)"
}

flush_status() {
  local tmp="$STATE_DIR/status.tsv.tmp" port
  : >"$tmp"
  if ((${#STATUS[@]})); then
    while IFS= read -r port; do
      printf '%s\t%s\n' "$port" "${STATUS[$port]}" >>"$tmp"
    done < <(printf '%s\n' "${!STATUS[@]}" | sort -n)
  fi
  mv "$tmp" "$STATE_DIR/status.tsv"
}

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

pick_endpoint() {
  local exclude="${1:-}" ep tries=0 used=0 p
  local n=${#ENDPOINTS[@]}
  ((n > 0)) || die "endpoint pool is empty ($ENDPOINTS_FILE)"
  while ((tries < 200)); do
    ep="${ENDPOINTS[RANDOM % n]}"
    tries=$((tries + 1))
    [[ -n "$exclude" && "$ep" == "$exclude" ]] && continue
    used=0
    for p in "${!EP_USED[@]}"; do
      if [[ "${EP_USED[$p]}" == "$ep" ]]; then
        used=1
        break
      fi
    done
    if ((used == 0)); then
      printf '%s' "$ep"
      return 0
    fi
  done
  while :; do
    ep="${ENDPOINTS[RANDOM % n]}"
    [[ "$ep" == "$exclude" ]] || {
      printf '%s' "$ep"
      return 0
    }
  done
}

render_conf() {
  local dir=$1 port=$2 endpoint=$3
  local src="$dir/wgcf-profile.conf" tmp="$dir/wireproxy.conf.tmp"
  [[ -s "$src" ]] || return 1

  # B2: strip IPv4 address / resolver / default route, keep the tunnel IPv6-only.
  sed -e 's/172\.16\.0\.2\/32, *//g' \
    -e 's/, *172\.16\.0\.2\/32//g' \
    -e 's/1\.1\.1\.1, *1\.0\.0\.1, *//g' \
    -e 's/1\.0\.0\.1, *1\.1\.1\.1, *//g' \
    -e 's/0\.0\.0\.0\/0, *//g' \
    -e 's/, *0\.0\.0\.0\/0//g' \
    -e "s#engage\.cloudflareclient\.com:2408#${endpoint}#g" \
    "$src" >"$tmp"

  # Keep only IPv6 entries on Address and DNS, whatever order wgcf emitted.
  awk '
    /^[[:space:]]*Address[[:space:]]*=/ {
      line=$0
      sub(/^[[:space:]]*Address[[:space:]]*=[[:space:]]*/, "", line)
      n=split(line, parts, ",")
      out=""
      for (i=1; i<=n; i++) {
        gsub(/^[ \t]+|[ \t]+$/, "", parts[i])
        if (parts[i] ~ /:/) out=(out == "" ? parts[i] : out ", " parts[i])
      }
      if (out != "") print "Address = " out
      next
    }
    /^[[:space:]]*DNS[[:space:]]*=/ {
      line=$0
      sub(/^[[:space:]]*DNS[[:space:]]*=[[:space:]]*/, "", line)
      n=split(line, parts, ",")
      out=""
      for (i=1; i<=n; i++) {
        gsub(/^[ \t]+|[ \t]+$/, "", parts[i])
        if (parts[i] ~ /:/) out=(out == "" ? parts[i] : out ", " parts[i])
      }
      if (out != "") print "DNS = " out
      next
    }
    { print }
  ' "$tmp" >"$tmp.2"

  sed -i 's#^AllowedIPs[[:space:]]*=.*#AllowedIPs = ::/0#' "$tmp.2"
  sed -i "s#^Endpoint[[:space:]]*=.*#Endpoint = ${endpoint}#" "$tmp.2"

  if ! grep -Eq '^DNS[[:space:]]*=.*:' "$tmp.2"; then
    awk 'BEGIN { done=0 }
      /^\[Interface\]/ {
        print
        if (!done) { print "DNS = 2606:4700:4700::1111, 2606:4700:4700::1001"; done=1 }
        next
      }
      { print }
    ' "$tmp.2" >"$tmp.3"
    mv "$tmp.3" "$tmp.2"
  fi

  if ! grep -Eq '^Address[[:space:]]*=.*:' "$tmp.2"; then
    rm -f "$tmp" "$tmp.2" "$tmp.3"
    return 1
  fi

  cat >>"$tmp.2" <<EOF

[Socks5]
BindAddress = ${WIREPROXY_BIND_HOST}:${port}
Username = ${SOCKS_USER}
Password = ${SOCKS_PASS}
EOF

  mv "$tmp.2" "$dir/wireproxy.conf"
  rm -f "$tmp" "$tmp.3"
  return 0
}

stop_wireproxy() {
  local dir=$1 pidfile="$1/wireproxy.pid" pid
  [[ -f "$pidfile" ]] || return 0
  pid=$(cat "$pidfile" 2>/dev/null || true)
  if [[ -n "$pid" ]] && kill -0 "$pid" 2>/dev/null; then
    kill "$pid" 2>/dev/null || true
    for _ in {1..20}; do
      kill -0 "$pid" 2>/dev/null || break
      sleep 0.1
    done
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$pidfile"
}

start_wireproxy() {
  local dir=$1 port=$2 logf="$1/wireproxy.log" pidfile="$1/wireproxy.pid"
  stop_wireproxy "$dir"
  (
    cd "$dir" || exit 1
    exec wireproxy -c wireproxy.conf -s
  ) >>"$logf" 2>&1 &
  local pid=$!
  printf '%s\n' "$pid" >"$pidfile"
  WIREPROXY_PIDS+=("$pid")
  log "started wireproxy $(basename "$dir") pid=$pid port=$port"
}

wait_port() {
  local port=$1 timeout=${2:-12} i
  for ((i = 0; i < timeout * 10; i++)); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${port}") 2>/dev/null; then
      exec 3>&- 3<&-
      return 0
    fi
    sleep 0.1
  done
  return 1
}

probe() {
  local port=$1 body ip loc prefix
  body=$(curl -sS --max-time "$PROBE_TIMEOUT" \
    -x "socks5h://${SOCKS_USER}:${SOCKS_PASS}@127.0.0.1:${port}" \
    "$PROBE_URL" 2>/dev/null) || return 1
  ip=$(printf '%s\n' "$body" | awk -F= '$1=="ip" { print $2; exit }')
  loc=$(printf '%s\n' "$body" | awk -F= '$1=="loc" { print $2; exit }')
  [[ -n "$ip" ]] || return 1
  prefix=$(printf '%s' "$ip" | awk -F: 'NF>=4 { print $1":"$2":"$3":"$4 }')
  [[ -n "$prefix" ]] || return 1
  printf '%s\t%s\t%s\n' "$prefix" "$ip" "$loc"
}

valid_prefix() {
  local prefix=$1 p48 d
  [[ -n "$prefix" ]] || return 1
  [[ "${prefix,,}" == 2a09:* ]] || return 1
  p48=$(printf '%s' "$prefix" | cut -d: -f1-3)
  for d in $DENY_48_PREFIXES; do
    [[ "$p48" == "$d"* ]] && return 2
  done
  return 0
}

register_instance() {
  local dir=$1 chain_port=${2:-} attempt=0
  local -a proxy_env=()
  while ((attempt < REGISTER_RETRIES)); do
    attempt=$((attempt + 1))
    rm -f "$dir/wgcf-account.toml"
    proxy_env=()
    if [[ -n "$chain_port" ]]; then
      proxy_env=("HTTPS_PROXY=socks5h://${SOCKS_USER}:${SOCKS_PASS}@127.0.0.1:${chain_port}")
      log "register $(basename "$dir") attempt ${attempt}/${REGISTER_RETRIES} via port ${chain_port}"
    else
      log "register $(basename "$dir") attempt ${attempt}/${REGISTER_RETRIES} direct"
    fi
    if (cd "$dir" && env "${proxy_env[@]}" wgcf register --accept-tos \
      --config wgcf-account.toml >>"$dir/register.log" 2>&1); then
      if account_valid "$dir"; then
        log "registered $(basename "$dir")"
        return 0
      fi
      log "register $(basename "$dir") reported success but the account file is invalid"
    fi
    rm -f "$dir/wgcf-account.toml"
    ((attempt < REGISTER_RETRIES)) || break
    if [[ -n "$chain_port" && $attempt -eq 1 ]]; then
      log "waiting 15s before retrying through port $chain_port"
      sleep 15
    elif [[ -n "$chain_port" ]]; then
      log "chain registration failed twice; falling back to direct"
      chain_port=""
      sleep 15
    else
      sleep 15
    fi
  done
  return 1
}

generate_profile() {
  local dir=$1 chain_port=${2:-} attempt
  profile_valid "$dir" && return 0
  local -a proxy_env=()
  for attempt in 1 2 3; do
    proxy_env=()
    if [[ -n "$chain_port" && $attempt -lt 3 ]]; then
      proxy_env=("HTTPS_PROXY=socks5h://${SOCKS_USER}:${SOCKS_PASS}@127.0.0.1:${chain_port}")
    fi
    if (cd "$dir" && env "${proxy_env[@]}" wgcf generate \
      --config wgcf-account.toml --profile wgcf-profile.conf --keepalive "$KEEPALIVE" \
      >>"$dir/generate.log" 2>&1); then
      profile_valid "$dir" && return 0
    fi
    log "generate profile failed for $(basename "$dir") (attempt $attempt)"
    sleep 2
  done
  return 1
}

bring_up() {
  local i=$1 dir port ep res prefix ip loc pid
  dir=$(inst_dir "$i")
  port=$(port_for "$i")
  mkdir -p "$dir"
  if ! account_valid "$dir"; then
    if ! register_instance "$dir" "$LAST_WORKING_PORT"; then
      status_set "$port" 0 "" "" "" "register-failed"
      return 1
    fi
  fi
  if ! generate_profile "$dir" "$LAST_WORKING_PORT"; then
    status_set "$port" 0 "" "" "" "generate-failed"
    return 1
  fi
  ep=$(pick_endpoint "${EP_USED[$port]:-}")
  EP_USED["$port"]="$ep"
  if ! render_conf "$dir" "$port" "$ep"; then
    log "render failed for $(basename "$dir")"
    status_set "$port" 0 "$ep" "" "" "render-failed"
    return 1
  fi
  if ! wireproxy -n -c "$dir/wireproxy.conf" >/dev/null 2>"$dir/configtest.log"; then
    log "config test failed for $(basename "$dir")"
    sed 's/^/  /' "$dir/configtest.log" >&2 || true
    status_set "$port" 0 "$ep" "" "" "config-failed"
    return 1
  fi
  start_wireproxy "$dir" "$port"
  pid=$(cat "$dir/wireproxy.pid" 2>/dev/null || printf '0')
  if ! wait_port "$port" 12; then
    log "port $port did not start listening"
    status_set "$port" "$pid" "$ep" "" "" "listen-failed"
    return 1
  fi
  if res=$(probe "$port"); then
    IFS=$'\t' read -r prefix ip loc <<<"$res" || true
    if valid_prefix "$prefix"; then
      status_set "$port" "$pid" "$ep" "$prefix" "$loc" "ok"
      LAST_WORKING_PORT="$port"
      log "up $(basename "$dir") port=$port endpoint=$ep prefix=$prefix loc=${loc:-?}"
      return 0
    fi
    status_set "$port" "$pid" "$ep" "$prefix" "$loc" "bad-prefix"
    log "up $(basename "$dir") port=$port endpoint=$ep prefix=$prefix (invalid, dedup will reroll)"
    return 0
  fi
  status_set "$port" "$pid" "$ep" "" "" "probe-failed"
  log "probe failed for $(basename "$dir") port=$port"
  return 1
}

reroll_instance() {
  local i=$1 dir port old_ep new_ep
  dir=$(inst_dir "$i")
  port=$(port_for "$i")
  old_ep="${EP_USED[$port]:-}"
  new_ep=$(pick_endpoint "$old_ep")
  EP_USED["$port"]="$new_ep"
  log "reroll $(basename "$dir") port=$port endpoint=${old_ep:-none} -> $new_ep"
  render_conf "$dir" "$port" "$new_ep" || return 1
  start_wireproxy "$dir" "$port"
  wait_port "$port" 12 || {
    log "port $port failed to listen after reroll"
    return 1
  }
  return 0
}

recover_account() {
  local i=$1 dir port
  dir=$(inst_dir "$i")
  port=$(port_for "$i")
  log "recover $(basename "$dir"): endpoint rerolls exhausted, re-registering a fresh account"
  stop_wireproxy "$dir"
  rm -f "$dir/wgcf-account.toml" "$dir/wgcf-profile.conf"
  if bring_up "$i"; then
    log "recovered $(basename "$dir") port=$port prefix=$(printf '%s' "${STATUS[$port]:-}" | cut -f3)"
    return 0
  fi
  log "recover failed for $(basename "$dir")"
  return 1
}

dedup_pass() {
  local -A seen=()
  local i port dir res prefix ip loc pid tries ok=0
  for ((i = 1; i <= INSTANCE_COUNT; i++)); do
    port=$(port_for "$i")
    dir=$(inst_dir "$i")
    is_disabled "${SOCKS_USER}$(printf '%02d' "$i")" && continue
    [[ -f "$dir/wireproxy.conf" ]] || continue
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
      pid=$(cat "$dir/wireproxy.pid" 2>/dev/null || printf '0')
      if [[ -n "$prefix" ]] && valid_prefix "$prefix" && [[ -z "${seen[$prefix]:-}" ]]; then
        seen["$prefix"]="$port"
        ok=$((ok + 1))
        status_set "$port" "$pid" "${EP_USED[$port]:-}" "$prefix" "$loc" "ok"
        break
      fi
      if ((tries >= DEDUP_RETRIES)); then
        log "WARN $(basename "$dir") port=$port prefix='${prefix:-none}' still invalid or duplicate after ${DEDUP_RETRIES} retries"
        if ! valid_prefix "$prefix"; then
          if recover_account "$i"; then
            prefix=$(printf '%s' "${STATUS[$port]:-}" | cut -f3)
            loc=$(printf '%s' "${STATUS[$port]:-}" | cut -f4)
            if [[ -n "$prefix" && "$prefix" != "-" ]] && valid_prefix "$prefix" && [[ -z "${seen[$prefix]:-}" ]]; then
              seen["$prefix"]="$port"
              ok=$((ok + 1))
              break
            fi
          fi
        fi
        status_set "$port" "$pid" "${EP_USED[$port]:-}" "$prefix" "$loc" "bad"
        break
      fi
      tries=$((tries + 1))
      log "dedup reroll $(basename "$dir") port=$port prefix='${prefix:-none}' try=$tries"
      reroll_instance "$i" || true
    done
  done
  flush_status
  log "Active Unique /64 Pool Size: ${ok} / ${INSTANCE_COUNT}"
}

deny_summary() {
  local port prefix p48 bad=0
  for port in $(printf '%s\n' "${!STATUS[@]}" | sort -n); do
    IFS=$'\t' read -r _pid _ep prefix _loc _state _ts <<<"${STATUS[$port]}" || true
    [[ -n "$prefix" && "$prefix" != "-" ]] || continue
    p48=$(printf '%s' "$prefix" | cut -d: -f1-3)
    if ! valid_prefix "$prefix"; then
      bad=$((bad + 1))
      log "DENY/INVALID port=$port prefix=$prefix family=$p48"
    fi
  done
  log "deny-family overlap / invalid prefixes: ${bad}"
}

detect_host() {
  if [[ -n "$EXTERNAL_HOST" ]]; then
    printf '%s' "$EXTERNAL_HOST"
    return 0
  fi
  local h url
  for url in https://api.ipify.org https://ipv4.icanhazip.com https://ifconfig.me/ip; do
    h=$(curl -4 -sS --max-time 8 "$url" 2>/dev/null | tr -d '[:space:]' || true)
    if [[ "$h" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
      printf '%s' "$h"
      return 0
    fi
  done
  h=$(hostname -i 2>/dev/null | awk '{ print $1 }')
  [[ -n "$h" ]] || die "cannot determine the public host; set EXTERNAL_HOST"
  printf '%s' "$h"
}

write_egress() {
  local i port state out="$STATE_DIR/egress-ext.txt"
  [[ -n "$PUBLIC_HOST" ]] || PUBLIC_HOST="${POOL_PUBLIC_HOST:-$(detect_host)}"
  : >"$out"
  for ((i = 1; i <= INSTANCE_COUNT; i++)); do
    port=$(port_for "$i")
    is_disabled "${SOCKS_USER}$(printf '%02d' "$i")" && continue
    state=$(printf '%s' "${STATUS[$port]:-}" | cut -f5)
    [[ "$state" == "ok" ]] || continue
    if [[ -n "$MUX_PORT" ]]; then
      printf 'socks5://%s%02d:%s@%s:%s\n' "$SOCKS_USER" "$i" "$SOCKS_PASS" "$PUBLIC_HOST" "$MUX_PORT" >>"$out"
    else
      printf 'socks5://%s:%s@%s:%s\n' "$SOCKS_USER" "$SOCKS_PASS" "$PUBLIC_HOST" "$port" >>"$out"
    fi
  done
  if [[ ! -s "$out" ]]; then
    log "WARN no healthy SOCKS5 endpoint yet; $out is empty"
  fi
  chmod 0644 "$out" 2>/dev/null || true
  log "wrote $out ($(wc -l <"$out") lines, host=$PUBLIC_HOST)"
}

print_egress_block() {
  printf '\n'
  printf '================= egress-ext.txt (copy to session A) =================\n'
  cat "$STATE_DIR/egress-ext.txt"
  printf '======================================================================\n'
  printf 'node public host: %s\n' "$PUBLIC_HOST"
  printf 'socks auth: %s / %s\n' "$SOCKS_USER" "$SOCKS_PASS"
  printf '\n'
}

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
  [[ -n "$MUX_PORT" ]] || return 0
  local last_port=$((START_PORT + INSTANCE_COUNT - 1))
  if ((MUX_PORT >= START_PORT && MUX_PORT <= last_port)); then
    log "WARN MUX_PORT ${MUX_PORT} is inside the wireproxy range ${START_PORT}-${last_port}; mux disabled until START_PORT moves past it"
    return 0
  fi
  log "starting socks mux on ${MUX_LISTEN}:${MUX_PORT} -> 127.0.0.1:${START_PORT}-${last_port}"
  /usr/local/bin/socksmux \
    -listen "${MUX_LISTEN}:${MUX_PORT}" \
    -backend-host 127.0.0.1 \
    -base-port "$START_PORT" \
    -count "$INSTANCE_COUNT" \
    -upstream-user "$SOCKS_USER" \
    -upstream-pass "$SOCKS_PASS" \
    -client-pass "$SOCKS_PASS" \
    -api-token "$API_TOKEN" \
    -state-dir "$STATE_DIR" \
    -public-host "${POOL_PUBLIC_HOST:-$PUBLIC_HOST}" \
    -private-host "$POOL_PRIVATE_HOST" \
    -pool-id "$POOL_ID" \
    -rotation-interval "$REROLL_INTERVAL" \
    -reroll-batch "$REROLL_BATCH" \
    -health-interval "$HEALTH_INTERVAL" \
    -min-healthy "$MIN_HEALTHY" \
    -mux-port "$MUX_PORT" >>"$STATE_DIR/socksmux.log" 2>&1 &
  MUX_PID=$!
  sleep 0.5
  if ! kill -0 "$MUX_PID" 2>/dev/null; then
    log "WARN socks mux failed to start; see $STATE_DIR/socksmux.log"
    MUX_PID=""
  fi
}

health_pass() {
  local i port dir res prefix ip loc pid state changed=0
  local -A hseen=()
  local tmpd="$STATE_DIR/.probe.$$"
  local -a probe_pids=()
  mkdir -p "$tmpd"
  for ((i = 1; i <= INSTANCE_COUNT; i++)); do
    port=$(port_for "$i")
    if is_disabled "${SOCKS_USER}$(printf '%02d' "$i")"; then
      continue
    fi
    (probe "$port" >"$tmpd/$port" 2>/dev/null || true) &
    probe_pids+=("$!")
  done
  for pid in "${probe_pids[@]}"; do
    wait "$pid" || true
  done
  for ((i = 1; i <= INSTANCE_COUNT; i++)); do
    port=$(port_for "$i")
    dir=$(inst_dir "$i")
    if is_disabled "${SOCKS_USER}$(printf '%02d' "$i")"; then
      status_set "$port" 0 "${EP_USED[$port]:-}" "" "" "disabled"
      continue
    fi
    res=$(cat "$tmpd/$port" 2>/dev/null || true)
    prefix=""
    ip=""
    loc=""
    if [[ -n "$res" ]]; then
      IFS=$'\t' read -r prefix ip loc <<<"$res" || true
    fi
    pid=$(cat "$dir/wireproxy.pid" 2>/dev/null || printf '0')
    if [[ -n "$prefix" ]] && valid_prefix "$prefix" && [[ -z "${hseen[$prefix]:-}" ]]; then
      hseen["$prefix"]="$port"
      status_set "$port" "$pid" "${EP_USED[$port]:-}" "$prefix" "$loc" "ok"
      continue
    fi
    if [[ -n "$prefix" ]] && valid_prefix "$prefix"; then
      log "health: port $port duplicate /64 $prefix (already on ${hseen[$prefix]}); rerolling"
    else
      log "health: port $port probe failed (prefix='${prefix:-none}'); rerolling"
    fi
    if ! reroll_instance "$i"; then
      log "health: reroll failed for $(inst_name "$i"); attempting full bring-up"
      bring_up "$i" || log "health: bring-up failed for $(inst_name "$i")"
    fi
    changed=1
    res=""
    res=$(probe "$port" 2>/dev/null || true)
    prefix=""
    ip=""
    loc=""
    if [[ -n "$res" ]]; then
      IFS=$'\t' read -r prefix ip loc <<<"$res" || true
    fi
    state="ok"
    if [[ -z "$prefix" ]] || ! valid_prefix "$prefix" || [[ -n "${hseen[$prefix]:-}" ]]; then
      state="bad"
    else
      hseen["$prefix"]="$port"
    fi
    status_set "$port" "$pid" "${EP_USED[$port]:-}" "$prefix" "$loc" "$state"
  done
  rm -rf "$tmpd"
  flush_status
  if ((changed)); then
    dedup_pass
    write_egress
  fi
}

reroll_all_batched() {
  local i batch_start=1 end
  log "scheduled reroll: all ${INSTANCE_COUNT} endpoints (batch=${REROLL_BATCH})"
  while ((batch_start <= INSTANCE_COUNT)); do
    end=$((batch_start + REROLL_BATCH - 1))
    if ((end > INSTANCE_COUNT)); then
      end=$INSTANCE_COUNT
    fi
    for ((i = batch_start; i <= end; i++)); do
      is_disabled "${SOCKS_USER}$(printf '%02d' "$i")" && continue
      reroll_instance "$i" || log "reroll failed for $(inst_name "$i")"
    done
    batch_start=$((end + 1))
    if ((batch_start <= INSTANCE_COUNT)); then
      sleep 5
    fi
  done
}

cleanup() {
  local pid f
  if [[ -n "$MUX_PID" ]]; then
    log "stopping socks mux"
    kill "$MUX_PID" 2>/dev/null || true
  fi
  log "stopping wireproxy instances"
  if ((${#WIREPROXY_PIDS[@]})); then
    for pid in "${WIREPROXY_PIDS[@]}"; do
      [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
    done
  fi
  for f in "$STATE_DIR"/instances/*/wireproxy.pid; do
    [[ -f "$f" ]] || continue
    pid=$(cat "$f" 2>/dev/null || true)
    [[ -n "$pid" ]] && kill "$pid" 2>/dev/null || true
  done
  sleep 0.2
}

main() {
  mkdir -p "$STATE_DIR/instances" "$QUEUE_DIR"
  load_runtime_config
  load_scale
  [[ -r "$ENDPOINTS_FILE" ]] || die "endpoint pool file not found: $ENDPOINTS_FILE"
  mapfile -t ENDPOINTS < <(grep -Ev '^[[:space:]]*(#|$)' "$ENDPOINTS_FILE")
  ((${#ENDPOINTS[@]} >= INSTANCE_COUNT)) ||
    die "need at least ${INSTANCE_COUNT} endpoints, found ${#ENDPOINTS[@]} in $ENDPOINTS_FILE"

  PUBLIC_HOST="${POOL_PUBLIC_HOST:-$(detect_host)}"
  local last_port=$((START_PORT + INSTANCE_COUNT - 1))
  log "warp-pool-producer starting: ${INSTANCE_COUNT} instances, ports ${START_PORT}-${last_port}, host=$PUBLIC_HOST"
  log "reroll=${REROLL_INTERVAL}s batch=${REROLL_BATCH} probe=${PROBE_URL}"
  start_mux

  local i boot_start boot_elapsed next_reroll now sleep_for remaining
  boot_start=$(date +%s)
  for ((i = 1; i <= INSTANCE_COUNT; i++)); do
    bring_up "$i" || log "WARN instance $(inst_name "$i") failed to come up"
  done
  boot_elapsed=$(($(date +%s) - boot_start))
  log "boot finished in ${boot_elapsed}s; running first dedup pass"

  dedup_pass
  deny_summary
  write_egress
  print_egress_block

  next_reroll=$(($(date +%s) + REROLL_INTERVAL))
  local next_health
  next_health=$(($(date +%s) + HEALTH_INTERVAL))
  while :; do
    process_queue
    load_runtime_config
    if ((ROTATION_CHANGED)); then
      ROTATION_CHANGED=0
      next_reroll=$(($(date +%s) + REROLL_INTERVAL))
      log "control: rotation interval now ${REROLL_INTERVAL}s"
    fi
    now=$(date +%s)
    if ((FORCE_REROLL)); then
      FORCE_REROLL=0
      reroll_all_batched
      dedup_pass
      deny_summary
      write_egress
      print_egress_block
      next_reroll=$(($(date +%s) + REROLL_INTERVAL))
      next_health=$(($(date +%s) + HEALTH_INTERVAL))
      continue
    fi
    if ((now >= next_health)); then
      health_pass
      next_health=$(($(date +%s) + HEALTH_INTERVAL))
      continue
    fi
    if ((now >= next_reroll)); then
      reroll_all_batched
      dedup_pass
      deny_summary
      write_egress
      print_egress_block
      next_reroll=$(($(date +%s) + REROLL_INTERVAL))
      next_health=$(($(date +%s) + HEALTH_INTERVAL))
      continue
    fi
    sleep_for=$((next_health - now))
    if ((next_reroll - now < sleep_for)); then
      sleep_for=$((next_reroll - now))
    fi
    if ((sleep_for > 5)); then
      sleep_for=5
    fi
    if ((sleep_for < 1)); then
      sleep_for=1
    fi
    sleep "$sleep_for"
  done
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  trap 'cleanup; exit 0' INT TERM
  trap cleanup EXIT
  main "$@"
fi
