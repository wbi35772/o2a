#!/bin/sh
# Turn a cfnew/Clash subscription into a local SOCKS pool for the free tier.
#
# The container has no global IPv6, so the WARP /64 pool is the wrong exit
# mechanism. This sidecar fetches the Clash view of the subscription, gives
# every selected node its own loopback SOCKS listener inside one mihomo
# process, writes the listener URLs to a proxyfile, and points the app at it
# with WARP disabled (while a WARP pool exists the free tier ignores proxies).
#
# It runs before the app starts. Any failure leaves the existing config alone.
set -eu

state_dir=${STATE_DIR:-/var/lib/opencode2api}
config_path=${CONFIG_PATH:-$state_dir/config.json}
work_dir=$state_dir/cfnew

sub_url=${CF_SUB_URL:-}
if [ -z "$sub_url" ] && [ -s "$state_dir/cfnew-sub.txt" ]; then
    sub_url=$(tr -d '\r\n' < "$state_dir/cfnew-sub.txt")
fi
if [ -z "$sub_url" ]; then
    exit 0
fi

node_count=${CF_NODE_COUNT:-200}
port_base=${CF_PORT_BASE:-21000}

mkdir -p "$work_dir"

log() { printf 'cfnew: %s\n' "$*" >&2; }

# 1. Fetch the Clash-format view of the subscription.
case "$sub_url" in
    *\?*) separator='&' ;;
    *)    separator='?' ;;
esac
if ! wget -q -T 20 -U 'clash.meta' -O "$work_dir/subscription.yaml" "${sub_url}${separator}target=clash"; then
    log 'subscription fetch failed; keeping the current configuration'
    exit 0
fi
if ! yq -e '.proxies | length > 0' "$work_dir/subscription.yaml" >/dev/null 2>&1; then
    log 'subscription has no proxies; keeping the current configuration'
    exit 0
fi

# 2. Build the mihomo config. Nodes are sampled evenly so the selection spans
#    the whole subscription instead of only its first region.
yq -o=json '.' "$work_dir/subscription.yaml" > "$work_dir/subscription.json"
jq --argjson want "$node_count" --argjson base "$port_base" '
  .proxies as $p
  | ($p | length) as $total
  | (if $total <= $want then [range(0; $total)]
     else [range(0; $want) | (. * $total / $want | floor)] end) as $idx
  | {
      mode: "direct",
      "log-level": "warning",
      ipv6: false,
      rules: ["MATCH,DIRECT"],
      proxies: [$idx[] as $i | $p[$i]],
      listeners: [
        $idx | to_entries[]
        | {
            name: ("o2a-" + (.key | tostring)),
            type: "socks",
            listen: "127.0.0.1",
            port: ($base + .key),
            udp: false,
            proxy: $p[.value].name
          }
      ]
    }
' "$work_dir/subscription.json" > "$work_dir/mihomo.json"

yq -p=json -o=yaml '.' "$work_dir/mihomo.json" > "$work_dir/mihomo.yaml"
jq -r '.listeners[] | "socks5://127.0.0.1:\(.port)"' "$work_dir/mihomo.json" > "$work_dir/proxies.txt"
jq -r '.listeners | length' "$work_dir/mihomo.json" > "$work_dir/count"

count=$(cat "$work_dir/count")
total=$(jq -r '.proxies | length' "$work_dir/subscription.json")
log "selected $count of $total nodes"
if [ "$count" -eq 0 ]; then
    log 'no nodes selected; keeping the current configuration'
    exit 0
fi

# 3. Start mihomo. A stale instance from an earlier boot is replaced.
if [ -f "$work_dir/mihomo.pid" ]; then
    kill "$(cat "$work_dir/mihomo.pid")" 2>/dev/null || true
fi
/usr/local/bin/mihomo -d "$work_dir" -f "$work_dir/mihomo.yaml" > "$work_dir/mihomo.log" 2>&1 &
mihomo_pid=$!
printf '%s\n' "$mihomo_pid" > "$work_dir/mihomo.pid"

# 4. Wait until the last listener is accepting connections. mihomo binds them
#    in order, so the last one proves the whole range is up. /proc/net/tcp
#    lists TCP sockets; state 0A is LISTEN.
wait_listener() {
    hex=$(printf '%04X' "$1")
    awk -v port="$hex" 'index($2, ":" port) && $4 == "0A" { found = 1 } END { exit !found }' /proc/net/tcp 2>/dev/null
}
last_port=$((port_base + count - 1))
ready=0
i=0
while [ "$i" -lt 20 ]; do
    if ! kill -0 "$mihomo_pid" 2>/dev/null; then
        log "mihomo exited during startup; see $work_dir/mihomo.log"
        break
    fi
    if wait_listener "$last_port"; then
        ready=1
        break
    fi
    i=$((i + 1))
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    log 'mihomo listeners did not come up; keeping the current configuration'
    exit 0
fi

# 5. Point the app at the pool. Keep one untouched copy of the WARP-era config
#    the first time this runs.
if [ -f "$config_path" ]; then
    if [ ! -f "$config_path.warp-backup" ]; then
        cp "$config_path" "$config_path.warp-backup"
    fi
    if jq --arg pf "$work_dir/proxies.txt" '
        .warp.enabled = false
        | .proxies = []
        | .proxyfile = $pf
    ' "$config_path" > "$config_path.cfnew.tmp" 2>/dev/null; then
        mv "$config_path.cfnew.tmp" "$config_path"
        log "wrote $config_path: warp disabled, proxyfile=$work_dir/proxies.txt"
    else
        rm -f "$config_path.cfnew.tmp"
        log "could not rewrite $config_path; keeping WARP"
    fi
else
    log "no config at $config_path yet"
fi
