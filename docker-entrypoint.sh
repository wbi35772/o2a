#!/bin/sh
set -eu

state_dir=${STATE_DIR:-/var/lib/opencode2api}
config_path=${CONFIG_PATH:-$state_dir/config.json}
config_seed_path=${CONFIG_SEED_PATH:-}
# Empty means "defer to config.json". Only an explicitly provided value becomes
# a command-line override, so the file stays authoritative as documented.
listen_address=${LISTEN_ADDRESS:-}
webui_listen_address=${WEBUI_LISTEN_ADDRESS:-}

if [ "$(id -u)" = "0" ]; then
    mkdir -p "$state_dir"
    chown opencode2api:opencode2api "$state_dir"
    chmod 700 "$state_dir"
    exec su-exec opencode2api:opencode2api "$0" "$@"
fi

if [ "$#" -gt 0 ]; then
    exec "$@"
fi

# InstaCloud-style deployments keep an operator-managed boot.sh on the data
# volume. Delegate to it so a repo-built image and a volume-managed image take
# the same startup path (WARP staged off, pool sync, then the gateway).
boot_sh=${O2A_BOOT_SH:-$state_dir/upgrade/boot.sh}
if [ "${O2A_USE_BOOT_SH:-1}" = "1" ] \
    && [ -x "$boot_sh" ] \
    && [ -z "${O2A_BOOT_SH_ACTIVE:-}" ]; then
    export O2A_BOOT_SH_ACTIVE=1
    # boot.sh writes its own CONFIG_PATH (a derived runtime config); the image
    # default would otherwise make source and runtime config the same file.
    unset CONFIG_PATH CONFIG_SEED_PATH
    exec "$boot_sh"
fi

# The example config binds the API to loopback, which is unreachable from
# outside a container through published ports. Rewrite only that exact default
# while seeding; any other address is left exactly as the operator wrote it.
seed_container_listen() {
    if grep -q '"listen"[[:space:]]*:[[:space:]]*"127.0.0.1:8080"' "$1"; then
        sed -i 's|"listen"[[:space:]]*:[[:space:]]*"127.0.0.1:8080"|"listen": "0.0.0.0:8080"|' "$1"
        printf '%s\n' "Seeded $1 with listen 0.0.0.0:8080 so it is reachable through published ports."
    fi
}

if [ ! -f "$config_path" ]; then
    mkdir -p "$(dirname "$config_path")"
    if [ -n "$config_seed_path" ] && [ -f "$config_seed_path" ]; then
        cp "$config_seed_path" "$config_path"
        seed_container_listen "$config_path"
        printf '%s\n' "Initialized $config_path from $config_seed_path."
    else
        cp /app/config.example.json "$config_path"
        seed_container_listen "$config_path"
        printf '%s\n' \
            "config.json not found; created $config_path. Set API keys or enable anonymous mode, and change the WebUI password before use."
    fi
fi

# The configuration carries upstream API keys and the WebUI password hash.
# A copied seed may be group/world-readable; never keep it that way.
chmod 600 "$config_path" 2>/dev/null || true

set -- /usr/local/bin/opencode2api -config "$config_path"
if [ -n "$listen_address" ]; then
    set -- "$@" -listen "$listen_address"
fi
if [ -n "$webui_listen_address" ]; then
    set -- "$@" -web-listen "$webui_listen_address"
fi
exec "$@"
