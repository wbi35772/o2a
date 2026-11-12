#!/bin/bash
# matrix-sync.sh - merge live matrix subscription nodes into relay-nodes.tsv
set -uo pipefail
SUB_URL="http://172.17.0.2:8090/sub/583936ebb3a88b24c2e4ba73430ac477"
NODES=/data/o2a/deploy/data/relay-nodes.tsv
LOG=/data/o2a/matrix-sync.log
TMP=$(mktemp /tmp/matrix-sub.XXXXXX) || exit 1
trap 'rm -f "$TMP"' EXIT
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" >>"$LOG"; tail -n 1000 "$LOG" >"$LOG.tmp" 2>/dev/null && mv -f "$LOG.tmp" "$LOG"; }

curl -fsS -m 15 "$SUB_URL" -o "$TMP" || { log "sub fetch failed"; exit 1; }
python3 - "$TMP" "$NODES" <<'PYEOF'
import base64, sys, urllib.parse, os
raw = open(sys.argv[1], 'rb').read().strip()
try: txt = base64.b64decode(raw).decode()
except Exception: txt = raw.decode()
rows = []
for line in txt.splitlines():
    if not line.startswith('trojan://'): continue
    u = urllib.parse.urlparse(line.strip())
    q = urllib.parse.parse_qs(u.query)
    label = urllib.parse.unquote(u.fragment) or 'node'
    host = u.hostname or ''
    port = u.port or 443
    sni = (q.get('sni') or [host])[0]
    path = (q.get('path') or ['/pgtrojan'])[0]
    pw = urllib.parse.unquote(u.username or '')
    if not host or not pw: continue
    rows.append((label.replace('\t',' '), host, str(port), sni, path, pw))
if not rows:
    print('no nodes'); sys.exit(1)
existing = []
path = sys.argv[2]
if os.path.exists(path):
    for line in open(path):
        parts = line.rstrip('\n').split('\t')
        if len(parts) >= 6:
            existing.append(parts[:6])
known = {r[0] for r in existing}
merged = existing + [r for r in rows if r[0] not in known]
with open(path, 'w') as f:
    for r in merged: f.write('\t'.join(r) + '\n')
print('sub=%d existing=%d merged=%d' % (len(rows), len(existing), len(merged)))
PYEOF
rc=$?
if [ $rc -ne 0 ]; then log "parse failed rc=$rc"; exit $rc; fi
log "merged: $(wc -l < "$NODES") rows"
