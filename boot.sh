#!/bin/sh
# boot.sh - main-container entrypoint for InstaCloud-style deployments where
# the platform rebuilds the root disk but keeps the data volume.
#
# It keeps the gateway available while WARP registers (fail-closed) and keeps
# the external static pool in sync with the pool control plane. Every path and
# integration parameter is environment driven.
#
# Environment:
#   STATE_DIR            default /var/lib/opencode2api
#   CONFIG_PATH          runtime config written at boot
#                        (default: $STATE_DIR/config-boot.json)
#   CONFIG_SOURCE        source config (default: $STATE_DIR/config.json)
#   O2A_BIN              gateway binary
#                        (default: $STATE_DIR/upgrade/opencode2api-app.new,
#                         fallback /usr/local/bin/opencode2api-app)
#   O2A_WEB_SAME_PORT    1 = pass -web-same-port (default: 1)
#   O2A_LISTEN           optional -listen value
#   O2A_WEB_LISTEN       optional -web-listen value
#   WARP_ENABLE_ON_BOOT  1 = start with WARP off then hot-reload it (default: 1)
#   WARP_RELOAD_DELAY    seconds before the background WARP reload (default: 15)
#   ADMIN_API            local gateway base URL (default: http://127.0.0.1:8081)
#   ADMIN_USER           default admin
#   ADMIN_PASS / ADMIN_PASS_FILE
#   POOL_SYNC_ENABLE     1 = run pool sync (default: 1)
#   POOL_SYNC_BIN        default: /usr/local/bin/pool-sync.sh if present,
#                        otherwise $STATE_DIR/upgrade/pool-sync.sh
#   POOL_SYNC_INTERVAL   default 60
#   POOL_ENV_FILE / POOL_API_URL / POOL_API_TOKEN / EGRESS_FILE ...
#                        passed through to pool-sync.sh
set -u

BASE="${STATE_DIR:-/var/lib/opencode2api}"
CFG="${CONFIG_PATH:-$BASE/config-boot.json}"
SRC="${CONFIG_SOURCE:-$BASE/config.json}"
[ "$CFG" = "$SRC" ] && CFG="$BASE/config-boot.json"
BIN="${O2A_BIN:-$BASE/upgrade/opencode2api-app.new}"
BOOTSTATE="$CFG.warp-state.json"
STATE="$SRC.warp-state.json"
ADMIN_API="${ADMIN_API:-http://127.0.0.1:8081}"
ADMIN_USER="${ADMIN_USER:-admin}"
ADMIN_PASS="${ADMIN_PASS:-}"
ADMIN_PASS_FILE="${ADMIN_PASS_FILE:-$BASE/upgrade/admin.pass}"
WARP_ENABLE_ON_BOOT="${WARP_ENABLE_ON_BOOT:-1}"
WARP_RELOAD_DELAY="${WARP_RELOAD_DELAY:-15}"
POOL_SYNC_ENABLE="${POOL_SYNC_ENABLE:-1}"
POOL_SYNC_INTERVAL="${POOL_SYNC_INTERVAL:-60}"
LOG="${BOOT_LOG:-$BASE/upgrade/boot.log}"

log() { echo "$(date -u +%FT%TZ) $*" >>"$LOG" 2>/dev/null || true; }
mkdir -p "$(dirname "$LOG")" 2>/dev/null || true

if [ ! -x "$BIN" ]; then
  if [ -x /usr/local/bin/opencode2api-app ]; then
    BIN=/usr/local/bin/opencode2api-app
  else
    log "FATAL gateway binary not found (O2A_BIN=$BIN)"
    exit 1
  fi
fi
[ -f "$SRC" ] || { log "FATAL config source not found: $SRC"; exit 1; }
command -v jq >/dev/null 2>&1 || { log "FATAL jq not found"; exit 1; }

if [ -z "${POOL_SYNC_BIN:-}" ]; then
  if [ -x /usr/local/bin/pool-sync.sh ]; then
    POOL_SYNC_BIN=/usr/local/bin/pool-sync.sh
  else
    POOL_SYNC_BIN="$BASE/upgrade/pool-sync.sh"
  fi
fi

if [ "$POOL_SYNC_ENABLE" = "1" ] && [ -x "$POOL_SYNC_BIN" ]; then
  "$POOL_SYNC_BIN" --fetch-only >>"$LOG" 2>&1 || log "initial pool sync failed; using existing list"
fi

jq '.warp.enabled=false' "$SRC" >"$CFG.tmp" && chmod 644 "$CFG.tmp" && mv "$CFG.tmp" "$CFG" \
  || { log "FATAL cannot write $CFG"; exit 1; }

if [ "$WARP_ENABLE_ON_BOOT" = "1" ]; then
  (
    sleep "$WARP_RELOAD_DELAY"
    i=0
    while [ "$i" -lt 24 ]; do
      curl -sS -m 5 "$ADMIN_API/healthz" >/dev/null 2>&1 && break
      i=$((i + 1))
      sleep 5
    done
    pass="$ADMIN_PASS"
    if [ -z "$pass" ]; then
      [ -f "$ADMIN_PASS_FILE" ] || { log "no admin password; WARP stays disabled"; exit 0; }
      pass=$(cat "$ADMIN_PASS_FILE")
    fi
    if [ ! -f "$BOOTSTATE" ] && [ -f "$STATE" ]; then
      cp "$STATE" "$BOOTSTATE" && chmod 600 "$BOOTSTATE"
    fi
    jq '.warp.enabled=true' "$CFG" >"$CFG.tmp" && chmod 644 "$CFG.tmp" && mv "$CFG.tmp" "$CFG"
    n=0
    while [ "$n" -lt 3 ]; do
      json=$(jq -n --arg u "$ADMIN_USER" --arg p "$pass" '{username:$u,password:$p}')
      csrf=$(curl -sS -m 15 -c /tmp/boot-ck.txt -X POST "$ADMIN_API/api/auth/login" \
        -H 'Content-Type: application/json' -d "$json" | jq -r '.csrf_token // empty')
      if [ -z "$csrf" ]; then
        log "login failed; retry $n"
        n=$((n + 1))
        sleep 30
        continue
      fi
      rc=$(curl -sS -m 900 -b /tmp/boot-ck.txt -H "X-CSRF-Token: $csrf" \
        -o /tmp/boot-reload.json -w '%{http_code}' \
        -X POST "$ADMIN_API/api/config/reload" \
        -H 'Content-Type: application/json' -d '{}')
      log "reload attempt $n -> $rc $(head -c 200 /tmp/boot-reload.json 2>/dev/null)"
      [ "$rc" = "200" ] && break
      n=$((n + 1))
      sleep 30
    done
  ) >>"$LOG" 2>&1 &
fi

if [ "$POOL_SYNC_ENABLE" = "1" ] && [ -x "$POOL_SYNC_BIN" ]; then
  (
    sleep 90
    exec "$POOL_SYNC_BIN" --loop "$POOL_SYNC_INTERVAL"
  ) >>"$LOG" 2>&1 &
fi

set -- "$BIN" -config "$CFG"
[ "${O2A_WEB_SAME_PORT:-1}" = "1" ] && set -- "$@" -web-same-port
[ -n "${O2A_LISTEN:-}" ] && set -- "$@" -listen "$O2A_LISTEN"
[ -n "${O2A_WEB_LISTEN:-}" ] && set -- "$@" -web-listen "$O2A_WEB_LISTEN"
exec "$@"
