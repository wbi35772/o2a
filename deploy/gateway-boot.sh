#!/bin/sh
# gateway-boot.sh - compose entrypoint for the gateway container.
#
# Fresh volume: seed config.json from config.seed.json (admin password and
# server key come from the environment), create the egress file, then run.
# Existing volume: leave config.json and state alone; only refresh files the
# gateway expects to exist.
set -eu

BASE=${STATE_DIR:-/var/lib/opencode2api}
SEED=${CONFIG_SEED_PATH:-/run/o2a/config.seed.json}
CFG=${CONFIG_PATH:-$BASE/config.json}
EGRESS=${EGRESS_FILE:-$BASE/egress-ext.txt}
ADMIN_PASS_FILE=${ADMIN_PASS_FILE:-$BASE/upgrade/admin.pass}

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$BASE/upgrade" "$BASE/proxycenter"
    chown -R opencode2api:opencode2api "$BASE"
    chmod 700 "$BASE"
    exec su-exec opencode2api:opencode2api /bin/sh /run/o2a/gateway-boot.sh
fi

mkdir -p "$BASE/upgrade" "$BASE/proxycenter"
: >>"$EGRESS"
chmod 644 "$EGRESS" 2>/dev/null || true

if [ ! -f "$CFG" ]; then
    if [ ! -f "$SEED" ]; then
        echo "gateway-boot: seed config missing at $SEED" >&2
        exit 1
    fi
    if [ -n "${ADMIN_PASS:-}" ] && command -v jq >/dev/null 2>&1; then
        jq --arg p "$ADMIN_PASS" '.webui.password = $p' "$SEED" >"$CFG.tmp"
    else
        cp "$SEED" "$CFG.tmp"
    fi
    if [ -n "${SERVER_KEY:-}" ] && command -v jq >/dev/null 2>&1; then
        jq --arg k "$SERVER_KEY" '.server_keys = [$k]' "$CFG.tmp" >"$CFG.tmp2"
        mv "$CFG.tmp2" "$CFG.tmp"
    fi
    mv "$CFG.tmp" "$CFG"
    chmod 600 "$CFG"
    echo "gateway-boot: seeded $CFG"
fi

# pool-sync reloads through the WebUI API, so it needs the admin password.
if [ ! -s "$ADMIN_PASS_FILE" ] && [ -n "${ADMIN_PASS:-}" ]; then
    printf '%s\n' "$ADMIN_PASS" >"$ADMIN_PASS_FILE"
    chmod 600 "$ADMIN_PASS_FILE"
fi

exec /usr/local/bin/opencode2api -config "$CFG"
