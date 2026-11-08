#!/bin/sh
# pool-sync.sh - keep the main container's static SOCKS list in sync with the
# pool management API. Every integration parameter comes from the environment
# (or from the optional env file), nothing is baked in.
#
# Modes:
#   pool-sync.sh                 one fetch; reload only when the list changed
#   pool-sync.sh --fetch-only    one fetch; never reload
#   pool-sync.sh --loop [SEC]    fetch every SEC seconds (default 60)
#
# Environment:
#   POOL_API_URL        base URL of the pool control plane, e.g.
#                       http://POOL_PRIVATE_HOST:1081
#   POOL_API_TOKEN      bearer token for /api/v1/*
#   POOL_ENV_FILE       optional env file sourced before the above
#                       (default: $STATE_DIR/upgrade/pool.env)
#   POOL_SYNC_SCOPE     private | public (default: private)
#   EGRESS_FILE         list written for the gateway
#                       (default: $STATE_DIR/egress-ext.txt)
#   ADMIN_API           local gateway base URL (default: http://127.0.0.1:8081)
#   ADMIN_USER          WebUI user for the reload call (default: admin)
#   ADMIN_PASS          WebUI password; when empty, ADMIN_PASS_FILE is read
#   ADMIN_PASS_FILE     default: $STATE_DIR/upgrade/admin.pass
#   POOL_SYNC_RELOAD    1 = reload after a changed list, 0 = fetch only
#   POOL_SYNC_INTERVAL  default loop interval in seconds (default: 60)
#   RELOAD_TIMEOUT      curl timeout for /api/config/reload (default: 900)
#   RELOAD_RETRIES      login+reload attempts (default: 3)
#   RELOAD_RETRY_DELAY  seconds between attempts (default: 15)
set -u

BASE="${STATE_DIR:-/var/lib/opencode2api}"
ENVF="${POOL_ENV_FILE:-$BASE/upgrade/pool.env}"
EGRESS="${EGRESS_FILE:-$BASE/egress-ext.txt}"
LOG="${POOL_SYNC_LOG:-$BASE/upgrade/pool-sync.log}"
LOCK="${POOL_SYNC_LOCK:-/tmp/pool-sync.lock}"
ADMIN_API="${ADMIN_API:-http://127.0.0.1:8081}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASS="${ADMIN_PASS:-}"
ADMIN_PASS_FILE="${ADMIN_PASS_FILE:-$BASE/upgrade/admin.pass}"
POOL_SYNC_SCOPE="${POOL_SYNC_SCOPE:-private}"
POOL_SYNC_RELOAD="${POOL_SYNC_RELOAD:-1}"
POOL_SYNC_INTERVAL="${POOL_SYNC_INTERVAL:-60}"
POOL_HTTP_TIMEOUT="${POOL_HTTP_TIMEOUT:-10}"
RELOAD_TIMEOUT="${RELOAD_TIMEOUT:-900}"
RELOAD_RETRIES="${RELOAD_RETRIES:-3}"
RELOAD_RETRY_DELAY="${RELOAD_RETRY_DELAY:-15}"

MODE=once
LOOP_INTERVAL="$POOL_SYNC_INTERVAL"
case "${1:-}" in
  --fetch-only) MODE=fetch ;;
  --loop)
    MODE=loop
    [ -n "${2:-}" ] && LOOP_INTERVAL="$2"
    ;;
  --once | "") ;;
  *)
    printf '%s\n' "usage: $0 [--once|--fetch-only|--loop [SEC]]" >&2
    exit 2
    ;;
esac

mkdir -p "$(dirname "$LOG")" 2>/dev/null || true
log() { echo "$(date -u +%FT%TZ) $*" >>"$LOG" 2>/dev/null || true; }

[ -f "$ENVF" ] && . "$ENVF"

fetch_once() {
  if [ -z "${POOL_API_URL:-}" ]; then
    log "POOL_API_URL not set; sync skipped"
    return 0
  fi

  tmp="$EGRESS.tmp.$$"
  code=$(curl -sS -m "$POOL_HTTP_TIMEOUT" \
    -H "Authorization: Bearer ${POOL_API_TOKEN:-}" \
    -o "$tmp" -w '%{http_code}' \
    "$POOL_API_URL/api/v1/endpoints?scope=${POOL_SYNC_SCOPE}" 2>/dev/null || echo 000)
  if [ "$code" != "200" ] || [ ! -s "$tmp" ]; then
    size=$(wc -c <"$tmp" 2>/dev/null || echo 0)
    log "fetch failed http=$code size=$size; keeping current list"
    rm -f "$tmp"
    return 1
  fi

  n=$(grep -c '^socks5://' "$tmp" 2>/dev/null || true)
  if [ "${n:-0}" -eq 0 ]; then
    log "fetch returned no socks5:// lines; keeping current list"
    rm -f "$tmp"
    return 1
  fi

  if cmp -s "$tmp" "$EGRESS"; then
    rm -f "$tmp"
    return 0
  fi

  cp "$tmp" "$EGRESS" && chmod 644 "$EGRESS"
  rm -f "$tmp"
  log "egress list updated: $n endpoints"
  return 2
}

reload_gateway() {
  [ "$POOL_SYNC_RELOAD" = "1" ] || return 0
  if [ -z "$ADMIN_PASS" ]; then
    [ -f "$ADMIN_PASS_FILE" ] || {
      log "no admin password (ADMIN_PASS/ADMIN_PASS_FILE); reload skipped"
      return 0
    }
    ADMIN_PASS=$(cat "$ADMIN_PASS_FILE")
  fi
  command -v jq >/dev/null 2>&1 || {
    log "jq not found; reload skipped"
    return 0
  }

  if ! mkdir "$LOCK" 2>/dev/null; then
    log "reload already in progress; list saved for next reload"
    return 0
  fi
  trap 'rmdir "$LOCK" 2>/dev/null' EXIT INT TERM

  ck=$(mktemp /tmp/pool-ck.XXXXXX)
  body=$(mktemp /tmp/pool-reload.XXXXXX)
  i=0
  while [ "$i" -lt "$RELOAD_RETRIES" ]; do
    json=$(jq -n --arg u "$ADMIN_USER" --arg p "$ADMIN_PASS" '{username:$u,password:$p}')
    csrf=$(curl -sS -m 15 -c "$ck" -X POST "$ADMIN_API/api/auth/login" \
      -H 'Content-Type: application/json' -d "$json" | jq -r '.csrf_token // empty')
    if [ -z "$csrf" ]; then
      log "login failed; retry $i"
      i=$((i + 1))
      sleep "$RELOAD_RETRY_DELAY"
      continue
    fi
    rc=$(curl -sS -m "$RELOAD_TIMEOUT" -b "$ck" -H "X-CSRF-Token: $csrf" \
      -o "$body" -w '%{http_code}' \
      -X POST "$ADMIN_API/api/config/reload" \
      -H 'Content-Type: application/json' -d '{}')
    log "reload attempt $i -> $rc $(head -c 160 "$body" 2>/dev/null)"
    [ "$rc" = "200" ] && break
    i=$((i + 1))
    sleep "$RELOAD_RETRY_DELAY"
  done
  rm -f "$ck" "$body"
  rmdir "$LOCK" 2>/dev/null
  trap - EXIT INT TERM
}

run_once() {
  fetch_once
  rc=$?
  case "$MODE" in
    fetch) return 0 ;;
    *) [ "$rc" = "2" ] && reload_gateway ;;
  esac
  return 0
}


if [ "$MODE" = "loop" ]; then
  log "pool sync loop started: interval=${LOOP_INTERVAL}s scope=${POOL_SYNC_SCOPE} url=${POOL_API_URL:-unset}"
  while :; do
    run_once
    sleep "$LOOP_INTERVAL"
  done
fi

run_once
