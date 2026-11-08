#!/usr/bin/env bash
set -euo pipefail

STATE_DIR="${STATE_DIR:-/data}"
STATUS_FILE="$STATE_DIR/status.tsv"
MIN_HEALTHY="${MIN_HEALTHY:-14}"
SOCKS_USER="${SOCKS_USER:?SOCKS_USER must be set}"
SOCKS_PASS="${SOCKS_PASS:?SOCKS_PASS must be set}"
PROBE_URL="${PROBE_URL:-https://opencode.ai/cdn-cgi/trace}"

[[ -s "$STATUS_FILE" ]] || exit 1

now=$(date +%s)
ok=$(awk -F'\t' -v now="$now" 'NF>=7 && $6=="ok" && (now-$7)<900 { count++ } END { print count+0 }' "$STATUS_FILE")
[[ "$ok" -ge "$MIN_HEALTHY" ]] || exit 1

port=$(awk -F'\t' '$6=="ok" { print $1; exit }' "$STATUS_FILE")
[[ -n "$port" ]] || exit 1

curl -sS --max-time 8 \
  -x "socks5h://${SOCKS_USER}:${SOCKS_PASS}@127.0.0.1:${port}" \
  "$PROBE_URL" | grep -q '^ip=2a09:'
