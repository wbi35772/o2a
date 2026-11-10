#!/bin/bash
# relay-watchdog.sh - keep the relay supervisor alive inside the warp-pool
# container. The relay is optional infrastructure: the pool control plane keeps
# working without it, so this script only restarts a missing supervisor and
# never blocks the pool entrypoint.
#
# Environment (all optional):
#   STATE_DIR            relay state root (default /data/relay-root)
#   MUX_PORT             relay mux/API port (default 11081)
#   RELAY_COUNT          relay sessions (default 10)
#   SING_PORT_BASE       first local session port (default 1594)
#   RELAY_PROBE_URL      probe URL used for /64 discovery
#   WG_ENDPOINT_HOST     WARP endpoint (default 162.159.192.1)
#   DEDUP_RETRIES        startup dedup attempts (default 4)
#   HEALTH_INTERVAL      supervisor health pass (default 86400; keep sessions
#                        stable, the mux reports the live status instead)
#   RELAY_LENIENT_HEALTH 1 = do not rotate a duplicate /64 aggressively
#   API_ALLOW_ANON       1 = allow the local mux status without a token
set -u

STATE_DIR="${STATE_DIR:-/data/relay-root}"
MUX_PORT="${MUX_PORT:-11081}"
RELAY_COUNT="${RELAY_COUNT:-10}"
SING_PORT_BASE="${SING_PORT_BASE:-1594}"
RELAY_PROBE_URL="${RELAY_PROBE_URL:-https://[2606:4700:4700::1111]/cdn-cgi/trace}"
WG_ENDPOINT_HOST="${WG_ENDPOINT_HOST:-162.159.192.1}"
DEDUP_RETRIES="${DEDUP_RETRIES:-4}"
HEALTH_INTERVAL="${HEALTH_INTERVAL:-86400}"
RELAY_LENIENT_HEALTH="${RELAY_LENIENT_HEALTH:-1}"
API_ALLOW_ANON="${API_ALLOW_ANON:-1}"

LOG="$STATE_DIR/watchdog.log"
ENTRYPOINT=/usr/local/bin/relay-entrypoint.sh

log() { printf '[%s] %s\n' "$(date '+%F %T')" "$*" >>"$LOG"; }

mkdir -p "$STATE_DIR"

while :; do
  if ! pgrep -f 'relay-entrypoint\.sh' >/dev/null 2>&1; then
    log "relay supervisor missing; starting"
    cd "$STATE_DIR" || exit 1
    setsid env STATE_DIR="$STATE_DIR" MUX_PORT="$MUX_PORT" RELAY_COUNT="$RELAY_COUNT" \
      SING_PORT_BASE="$SING_PORT_BASE" RELAY_PROBE_URL="$RELAY_PROBE_URL" \
      WG_ENDPOINT_HOST="$WG_ENDPOINT_HOST" DEDUP_RETRIES="$DEDUP_RETRIES" \
      HEALTH_INTERVAL="$HEALTH_INTERVAL" RELAY_LENIENT_HEALTH="$RELAY_LENIENT_HEALTH" \
      API_ALLOW_ANON="$API_ALLOW_ANON" \
      "$ENTRYPOINT" >>"$STATE_DIR/relay.log" 2>&1 &
    log "started pid $!"
  fi
  sleep 60
done
