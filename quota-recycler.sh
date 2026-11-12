#!/bin/bash
# quota-recycler.sh - watch gateway 429s, quarantine the burned /64, and make
# the owning relay slot re-register a fresh WARP identity.
set -o pipefail

SHARED=/data/o2a/deploy/data/shared
BURNED="$SHARED/burned-prefix64.tsv"
QUEUE="$SHARED/relay-reroll.queue"
LOG=/data/o2a/quota-recycler.log
RELAY_API=http://127.0.0.1:11081
POOL_API=http://127.0.0.1:1081
TTL=64800

RELAY_TOKEN=$(sed -n 's/^RELAY_API_TOKEN=//p' /data/o2a/deploy/.env | head -1)
POOL_TOKEN=$(sed -n 's/^POOL_API_TOKEN=//p' /data/o2a/deploy/.env | head -1)

mkdir -p "$SHARED"
touch "$BURNED" "$QUEUE"
chmod 666 "$BURNED" "$QUEUE" 2>/dev/null || true

log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" >>"$LOG"; tail -n 2000 "$LOG" >"$LOG.tmp" 2>/dev/null && mv -f "$LOG.tmp" "$LOG"; }

prune() {
  local now tmp
  now=$(date +%s); tmp="$BURNED.tmp.$$"
  awk -F '\t' -v now="$now" -v ttl="$TTL" 'NF>=2 && $2 ~ /^[0-9]+$/ && now-$2 < ttl {print $1 "\t" $2}' "$BURNED" >"$tmp" 2>/dev/null && mv -f "$tmp" "$BURNED"
  rm -f "$tmp" 2>/dev/null || true
}

lookup_relay() {
  local port=$1 js
  js=$(curl -sS -m 5 -H "Authorization: Bearer $RELAY_TOKEN" "$RELAY_API/api/v1/status" 2>/dev/null) || return 1
  printf '%s' "$js" | jq -r --argjson p "$port" '.nodes[] | select(.port==$p) | [.index,.prefix64,.user] | @tsv' 2>/dev/null
}

lookup_pool() {
  local port=$1 js
  js=$(curl -sS -m 5 -H "Authorization: Bearer $POOL_TOKEN" "$POOL_API/api/v1/status" 2>/dev/null) || return 1
  printf '%s' "$js" | jq -r --argjson p "$port" '.nodes[] | select(.port==$p) | [.index,.prefix64,.user] | @tsv' 2>/dev/null
}

handle() {
  local line=$1 port prefix idx user now
  port=$(printf '%s' "$line" | jq -r '.proxy // empty' 2>/dev/null | sed -n 's/.*@127\.0\.0\.1:\([0-9][0-9]*\).*/\1/p')
  [ -n "$port" ] || return 0
  (( port >= 1578 && port <= 1633 )) || return 0

  local row
  if (( port >= 1594 )); then
    row=$(lookup_relay "$port") || return 0
  else
    row=$(lookup_pool "$port") || return 0
  fi
  [ -n "$row" ] || { log "port $port not found in status"; return 0; }
  IFS=$'\t' read -r idx prefix user <<<"$row"
  [ -n "$prefix" ] && [ "$prefix" != "-" ] || return 0

  now=$(date +%s)
  if grep -q "^$prefix"$'\t' "$BURNED" 2>/dev/null; then
    log "port $port prefix $prefix already burned"
    return 0
  fi
  printf '%s\t%s\n' "$prefix" "$now" >>"$BURNED"
  log "burned $prefix on port $port ($([ "$port" -ge 1594 ] && echo relay || echo warp))"
  prune

  if (( port >= 1594 )); then
    printf '%s\t%s\t%s\n' "$idx" "$prefix" "$now" >>"$QUEUE"
    log "enqueued relay reroll n$idx"
  fi

  sleep 20
  docker exec o2a-gateway /usr/local/bin/pool-sync.sh >>"$LOG" 2>&1 && log "pool-sync triggered"
}

replay_warp() {
  local line port row prefix now n=0
  while IFS= read -r line; do
    port=$(printf '%s' "$line" | jq -r '.proxy // empty' 2>/dev/null | sed -n 's/.*@127\.0\.0\.1:\([0-9][0-9]*\).*/\1/p')
    [ -n "$port" ] || continue
    (( port >= 1578 && port <= 1593 )) || continue
    row=$(lookup_pool "$port") || continue
    IFS=$'\t' read -r _ prefix _ <<<"$row"
    [ -n "$prefix" ] && [ "$prefix" != "-" ] || continue
    grep -q "^$prefix"$'\t' "$BURNED" 2>/dev/null && continue
    now=$(date +%s); printf '%s\t%s\n' "$prefix" "$now" >>"$BURNED"; n=$((n+1))
    log "replay burned $prefix on port $port"
  done < <(docker logs --since 24h o2a-gateway 2>&1 | grep 'free_usage_limit_reached')
  prune
  log "replay done: $n new prefixes"
  docker exec o2a-gateway /usr/local/bin/pool-sync.sh >>"$LOG" 2>&1 && log "pool-sync after replay"
}

arg1="$1"
case "$arg1" in
  --replay) replay_warp; exit 0 ;;
  --handle-line) handle "$2"; exit $? ;;
esac

log "quota recycler started"
while :; do
  docker logs --follow --since 1s o2a-gateway 2>&1 | while IFS= read -r line; do
    case "$line" in
      *free_usage_limit_reached*) handle "$line" ;;
    esac
  done
  log "gateway log stream ended; reconnecting"
  sleep 3
done
