#!/bin/sh
# pool-sync.sh - fused egress sync (v3, probe-fresh)
# Probe every slot through its mux user to learn the current /64, then publish
# one direct SOCKS endpoint per unique prefix. Relay first, local WARP second.
set +u

BASE=/var/lib/opencode2api
EGRESS=$BASE/egress-ext.txt
LOG=$BASE/upgrade/pool-sync.log
ADMIN_API=http://127.0.0.1:8081
ADMIN_USER=admin
ADMIN_PASS_FILE=$BASE/upgrade/admin.pass
SOCKS_USER=warpuser
PROBE_URL=$PROBE_URL
[ -n "$PROBE_URL" ] || PROBE_URL='https://[2606:4700:4700::1111]/cdn-cgi/trace'
PROBE_TIMEOUT=$PROBE_TIMEOUT
[ -n "$PROBE_TIMEOUT" ] || PROBE_TIMEOUT=10
PROBE_JOBS=$PROBE_JOBS
[ -n "$PROBE_JOBS" ] || PROBE_JOBS=8
RELAY_START=$RELAY_PORT_START
[ -n "$RELAY_START" ] || RELAY_START=1594
RELAY_COUNT=$RELAY_PORT_COUNT
[ -n "$RELAY_COUNT" ] || RELAY_COUNT=24
WARP_START=$WARP_PORT_START
[ -n "$WARP_START" ] || WARP_START=1578
WARP_COUNT=$WARP_PORT_COUNT
[ -n "$WARP_COUNT" ] || WARP_COUNT=16
BURNED_FILE=$POOL_BURNED_FILE
[ -n "$BURNED_FILE" ] || BURNED_FILE=/shared/burned-prefix64.tsv
BURNED_TTL=$BURNED_TTL
[ -n "$BURNED_TTL" ] || BURNED_TTL=64800
RELOAD_TIMEOUT=$RELOAD_TIMEOUT
[ -n "$RELOAD_TIMEOUT" ] || RELOAD_TIMEOUT=900
RELOAD_RETRIES=$RELOAD_RETRIES
[ -n "$RELOAD_RETRIES" ] || RELOAD_RETRIES=3
RELOAD_RETRY_DELAY=$RELOAD_RETRY_DELAY
[ -n "$RELOAD_RETRY_DELAY" ] || RELOAD_RETRY_DELAY=15
INTERVAL=$POOL_SYNC_INTERVAL
[ -n "$INTERVAL" ] || INTERVAL=60

MODE=once
case "$1" in
  --fetch-only) MODE=fetch ;;
  --loop) MODE=loop; [ -n "$2" ] && INTERVAL=$2 ;;
esac

mkdir -p "$(dirname "$LOG")" 2>/dev/null || true
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" >>"$LOG" 2>/dev/null || true; }

burned_clean() {
  [ -s "$BURNED_FILE" ] || return 0
  now=$(date +%s)
  tmp="$BURNED_FILE.tmp.$$"
  awk -F '\t' -v now="$now" -v ttl="$BURNED_TTL" 'NF>=1 && $1!="" {ts=($2 ~ /^[0-9]+$/)?$2:0; if(ts==0 || now-ts<ttl) print $1 "\t" ts}' "$BURNED_FILE" >"$tmp" 2>/dev/null && mv -f "$tmp" "$BURNED_FILE"
  rm -f "$tmp" 2>/dev/null || true
}

probe_one() {
  port=$1; mux=$2; user=$3; out=$4
  res=$(curl -sS -m "$PROBE_TIMEOUT" --socks5-hostname "$user:$SOCKS_PASS@127.0.0.1:$mux" "$PROBE_URL" 2>/dev/null) || return 1
  ip=$(printf '%s\n' "$res" | sed -n 's/^ip=//p' | head -1)
  warp=$(printf '%s\n' "$res" | sed -n 's/^warp=//p' | head -1)
  loc=$(printf '%s\n' "$res" | sed -n 's/^loc=//p' | head -1)
  [ "$warp" = "on" ] || return 1
  case "$ip" in
    *:*) ;;
    *) return 1 ;;
  esac
  prefix=$(printf '%s' "$ip" | cut -d: -f1-4)
  printf '%s\t%s\t%s\t%s\n' "$prefix" "$port" "$loc" "$mux" >"$out"
}

collect() {
  start=$1; count=$2; mux=$3; tag=$4
  i=0; n=$start; end=$((start+count))
  while [ "$n" -lt "$end" ]; do
    idx=$((n-start+1))
    user=$(printf 'warpuser%02d' "$idx")
    probe_one "$n" "$mux" "$user" "$TMP/$tag.$n" &
    i=$((i+1))
    [ $((i % PROBE_JOBS)) -eq 0 ] && wait
    n=$((n+1))
  done
  wait
}

fetch_once() {
  burned_clean
  TMP=$(mktemp -d /tmp/poolsync.XXXXXX) || return 1
  collect "$RELAY_START" "$RELAY_COUNT" 11081 r
  collect "$WARP_START" "$WARP_COUNT" 1081 w
  tmp="$EGRESS.tmp.$$"
  cat "$TMP"/r.* "$TMP"/w.* 2>/dev/null | awk -F '\t' -v burned="$BURNED_FILE" -v u="$SOCKS_USER" -v p="$SOCKS_PASS" '
    BEGIN { while ((getline line < burned) > 0) { split(line,b,"\t"); if (b[1]!="") bs[b[1]]=1 } close(burned) }
    ($1!="" && $2!="" && !bs[$1] && !seen[$1]++) { printf "socks5://%s:%s@127.0.0.1:%s#%s\n", u, p, $2, $1 }' >"$tmp"
  n=$(wc -l <"$tmp" | tr -d ' ')
  rm -rf "$TMP"
  case "$n" in ''|0) log "probe list empty; keeping current"; rm -f "$tmp"; return 1 ;; esac
  if cmp -s "$tmp" "$EGRESS"; then rm -f "$tmp"; return 0; fi
  cp "$tmp" "$EGRESS" && chmod 644 "$EGRESS"
  rm -f "$tmp"
  log "egress updated: $n endpoints"
  return 2
}

reload_gateway() {
  [ -n "$ADMIN_PASS" ] || { [ -f "$ADMIN_PASS_FILE" ] && ADMIN_PASS=$(cat "$ADMIN_PASS_FILE"); }
  if [ -z "$ADMIN_PASS" ]; then log "no admin password; reload skipped"; return 0; fi
  command -v jq >/dev/null 2>&1 || { log "jq missing; reload skipped"; return 0; }
  LOCK=/tmp/pool-sync.lock
  mkdir "$LOCK" 2>/dev/null || { log "reload busy"; return 0; }
  ck=$(mktemp /tmp/ck.XXXXXX); body=$(mktemp /tmp/rl.XXXXXX)
  i=0
  while [ "$i" -lt "$RELOAD_RETRIES" ]; do
    json=$(jq -n --arg u "$ADMIN_USER" --arg p "$ADMIN_PASS" '{username:$u,password:$p}')
    csrf=$(curl -sS -m 15 -c "$ck" -X POST "$ADMIN_API/api/auth/login" -H 'Content-Type: application/json' -d "$json" | jq -r '.csrf_token // empty')
    if [ -n "$csrf" ]; then
      rc=$(curl -sS -m "$RELOAD_TIMEOUT" -b "$ck" -H "X-CSRF-Token: $csrf" -o "$body" -w '%{http_code}' -X POST "$ADMIN_API/api/config/reload" -H 'Content-Type: application/json' -d '{}')
      log "reload attempt $i -> $rc $(head -c 160 "$body" 2>/dev/null)"
      [ "$rc" = "200" ] && break
    else
      log "login failed; retry $i"
    fi
    i=$((i+1)); sleep "$RELOAD_RETRY_DELAY"
  done
  rm -f "$ck" "$body"; rmdir "$LOCK" 2>/dev/null
}

run_once() {
  fetch_once; rc=$?
  case "$MODE" in fetch) return "$rc" ;; esac
  [ "$rc" = "2" ] && reload_gateway
  return "$rc"
}

if [ "$MODE" = "loop" ]; then
  log "probe sync loop: relay=$RELAY_START+$RELAY_COUNT warp=$WARP_START+$WARP_COUNT interval=$INTERVAL"
  while :; do run_once; sleep "$INTERVAL"; done
fi
run_once
