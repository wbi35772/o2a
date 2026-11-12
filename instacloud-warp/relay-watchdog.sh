#!/bin/bash
# relay-watchdog.sh - keep the relay supervisor alive inside the warp-pool
# container and keep its published status file fresh.
#
# The relay is optional infrastructure: the pool control plane keeps working
# without it, so this script only restarts a missing supervisor and refreshes
# the relay status table; it never blocks the pool entrypoint.
#
# Environment (all optional):
#   RELAY_STATE_DIR      relay state root (default /data/relay-root; the pool
#                        STATE_DIR=/data must not leak into the relay)
#   RELAY_MUX_PORT       relay mux/API port (default 11081; pool MUX_PORT=1081
#                        must not leak into the relay)
#   RELAY_COUNT          relay sessions (default 10)
#   SING_PORT_BASE       first local session port (default 1594)
#   RELAY_PROBE_URL      probe URL used for /64 discovery
#   WG_ENDPOINT_HOST     WARP endpoint (default 162.159.192.1)
#   DEDUP_RETRIES        startup dedup attempts (default 4)
#   RELAY_HEALTH_INTERVAL supervisor health pass (default 86400; keep sessions
#                        stable, the watchdog refreshes the mux status instead)
#   RELAY_REFRESH_INTERVAL status refresh period (default 300)
#   RELAY_LENIENT_HEALTH 1 = do not rotate a duplicate /64 aggressively
#   API_ALLOW_ANON       1 = allow the local mux status without a token
set -u

STATE_DIR="${RELAY_STATE_DIR:-/data/relay-root}"
MUX_PORT="${RELAY_MUX_PORT:-11081}"
RELAY_COUNT="${RELAY_COUNT:-}"
SING_PORT_BASE="${SING_PORT_BASE:-1594}"
RELAY_PROBE_URL="${RELAY_PROBE_URL:-https://[2606:4700:4700::1111]/cdn-cgi/trace}"
WG_ENDPOINT_HOST="${WG_ENDPOINT_HOST:-162.159.192.1}"
DEDUP_RETRIES="${DEDUP_RETRIES:-4}"
HEALTH_INTERVAL="${RELAY_HEALTH_INTERVAL:-86400}"
REFRESH_INTERVAL="${RELAY_REFRESH_INTERVAL:-300}"
RECOVER_PER_ROUND="${RELAY_RECOVER_PER_ROUND:-1}"
RELAY_LENIENT_HEALTH="${RELAY_LENIENT_HEALTH:-1}"
API_ALLOW_ANON="${API_ALLOW_ANON:-1}"

LOG="$STATE_DIR/watchdog.log"
if ! ( : >>"$LOG" ) 2>/dev/null; then
  LOG=/tmp/relay-watchdog.log
  echo "warn: $STATE_DIR/watchdog.log not writable, logging to $LOG" >&2
fi
RELAY_LOG="$STATE_DIR/relay.log"
if ! ( : >>"$RELAY_LOG" ) 2>/dev/null; then
  RELAY_LOG=/tmp/relay.log
  echo "warn: $STATE_DIR/relay.log not writable, relaying to $RELAY_LOG" >&2
fi
ENTRYPOINT=/usr/local/bin/relay-entrypoint.sh

# Reuse probe/status_set/flush_status from the relay entrypoint. Sourcing only
# defines functions and defaults; main() is not invoked by source. The watchdog
# log() must be defined after the source, otherwise the relay log() shadows it
# and every watchdog line lands in relay.log instead of watchdog.log.
# Resolve RELAY_COUNT from relay/settings.env and relay/nodes.tsv once here so
# the first supervisor start already uses the configured session count.
# shellcheck source=/dev/null
. "$ENTRYPOINT"
load_secrets
reload_settings
if ! [[ "$RELAY_COUNT" =~ ^[0-9]+$ ]] || ((10#$RELAY_COUNT < 1)); then
  RELAY_COUNT=10
fi
RELAY_COUNT=$((10#$RELAY_COUNT))

log() { printf '[%s] %s\n' "$(date '+%F %T')" "$*" >>"$LOG"; }

refresh_status() {
  local i port dir res prefix ip loc pid ok=0 failed=()
  load_secrets
  reload_settings
  validate_env
  for ((i = 1; i <= RELAY_COUNT; i++)); do
    port=$((SING_PORT_BASE + i - 1))
    dir="$STATE_DIR/relay/instances/$(printf 'n%02d' "$i")"
    res=$(probe "$port" 2>/dev/null || true)
    pid=$(cat "$dir/sing-box.pid" 2>/dev/null || printf '0')
    if [[ -n "$res" ]]; then
      IFS=$'\t' read -r prefix ip loc <<<"$res"
      status_set "$port" "$pid" "$(session_endpoint "$i")" "$prefix" "$loc" "ok"
      ok=$((ok + 1))
    else
      status_set "$port" "$pid" "$(session_endpoint "$i")" "" "" "probe-failed"
      failed+=("$i")
    fi
  done
  flush_status
  log "status refreshed: ok=$ok/$RELAY_COUNT failed=${#failed[@]}"

  # Recover at most RECOVER_PER_ROUND failed sessions per pass, so a flapping
  # subscription node does not trigger a re-register storm across all tunnels.
  local n=0 idx
  for idx in "${failed[@]}"; do
    ((n >= RECOVER_PER_ROUND)) && break
    port=$((SING_PORT_BASE + idx - 1))
    dir="$STATE_DIR/relay/instances/$(printf 'n%02d' "$idx")"
    res=""
    log "recover n$(printf '%02d' "$idx") port=$port"
    if recover_session "$idx" >>"$RELAY_LOG" 2>&1; then
      res=$(probe "$port" 2>/dev/null || true)
      pid=$(cat "$dir/sing-box.pid" 2>/dev/null || printf '0')
      if [[ -n "$res" ]]; then
        IFS=$'\t' read -r prefix ip loc <<<"$res"
        status_set "$port" "$pid" "$(session_endpoint "$idx")" "$prefix" "$loc" "ok"
        log "recovered n$(printf '%02d' "$idx") prefix=$prefix"
      else
        status_set "$port" "$pid" "$(session_endpoint "$idx")" "" "" "probe-failed"
        log "recover n$(printf '%02d' "$idx") still failing"
      fi
    else
      log "recover n$(printf '%02d' "$idx") failed (register/up error)"
    fi
    if [[ -z "$res" ]]; then
      log "rebind n$(printf '%02d' "$idx") port=$port after failed recovery"
      if rebind_session "$idx" >>"$RELAY_LOG" 2>&1; then
        res=$(probe "$port" 2>/dev/null || true)
        pid=$(cat "$dir/sing-box.pid" 2>/dev/null || printf '0')
        if [[ -n "$res" ]]; then
          IFS=$'	' read -r prefix ip loc <<<"$res"
          status_set "$port" "$pid" "$(session_endpoint "$idx")" "$prefix" "$loc" "ok"
          log "rebound n$(printf '%02d' "$idx") prefix=$prefix"
        else
          status_set "$port" "$pid" "$(session_endpoint "$idx")" "" "" "probe-failed"
          log "rebind n$(printf '%02d' "$idx") still failing"
        fi
      fi
    fi
    n=$((n + 1))
  done
  if ((n > 0)); then
    flush_status
    log "after recovery: $(awk -F'\t' '$6=="ok"{c++} END{print c+0}' "$STATUS_FILE")/${RELAY_COUNT} ok"
  fi
}

mkdir -p "$STATE_DIR"

# Single-instance lock: a second watchdog exits immediately instead of probing
# and rewriting status.tsv concurrently with the first one.
exec 9>"$STATE_DIR/watchdog.lock" || { echo "cannot open $STATE_DIR/watchdog.lock" >&2; exit 1; }
# The relay supervisor may inherit fd 9; a pgrep guard is used instead.
if pgrep -f 'relay-watchdog\.sh' | grep -qv "^$$$"; then
  log "another relay watchdog is running; exiting"
  exit 0
fi

# First refresh shortly after start, then every REFRESH_INTERVAL seconds.
LAST_REFRESH=$(($(date +%s) - REFRESH_INTERVAL + 120))
START_TS=$(date +%s)

while :; do
  if ! pgrep -f 'relay-entrypoint\.sh' >/dev/null 2>&1; then
    log "relay supervisor missing; starting"
    cd "$STATE_DIR" || exit 1
    setsid env STATE_DIR="$STATE_DIR" MUX_PORT="$MUX_PORT" \
      SING_PORT_BASE="$SING_PORT_BASE" RELAY_PROBE_URL="$RELAY_PROBE_URL" \
      WG_ENDPOINT_HOST="$WG_ENDPOINT_HOST" DEDUP_RETRIES="$DEDUP_RETRIES" \
      HEALTH_INTERVAL="$HEALTH_INTERVAL" RELAY_LENIENT_HEALTH="$RELAY_LENIENT_HEALTH" \
      API_ALLOW_ANON="$API_ALLOW_ANON" \
      "$ENTRYPOINT" >>"$RELAY_LOG" 2>&1 9>&- &
    log "started pid $!"
    LAST_REFRESH=$(($(date +%s) - REFRESH_INTERVAL + 120))
  else
    now=$(date +%s)
    if (( now - START_TS < 240 )); then
      sleep 60 9>&-
      continue
    fi
    if ((now - LAST_REFRESH >= REFRESH_INTERVAL)); then
      refresh_status || log "status refresh failed"
      LAST_REFRESH=$now
    fi
  fi
  sleep 60 9>&-
done
