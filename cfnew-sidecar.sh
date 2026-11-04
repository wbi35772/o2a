#!/bin/sh
# Turn cfnew/Clash subscriptions into a local SOCKS pool for the free tier.
#
# The container has no global IPv6, so the WARP /64 pool is the wrong exit
# mechanism. This sidecar fetches the Clash view of every configured
# subscription, gives each selected node its own loopback SOCKS listener inside
# one mihomo process, writes the listener URLs to a proxyfile, and points the
# app at it with WARP disabled (while a WARP pool exists the free tier ignores
# proxies).
#
# Sources: CF_SUB_URL (may hold several URLs separated by whitespace) plus one
# URL per line in $state_dir/cfnew-sub.txt. It runs before the app starts; any
# failure leaves the existing config alone.
set -eu

state_dir=${STATE_DIR:-/var/lib/opencode2api}
config_path=${CONFIG_PATH:-$state_dir/config.json}
work_dir=$state_dir/cfnew

node_count=${CF_NODE_COUNT:-600}
port_base=${CF_PORT_BASE:-21000}

mkdir -p "$work_dir"

log() { printf 'cfnew: %s\n' "$*" >&2; }

# 1. Collect subscription URLs, newest file entries last, and fetch each Clash
#    view. One broken subscription must not stop the others.
urls=""
if [ -n "${CF_SUB_URL:-}" ]; then
    urls=$CF_SUB_URL
fi
if [ -s "$state_dir/cfnew-sub.txt" ]; then
    urls="$urls
$(tr -d '\r' < "$state_dir/cfnew-sub.txt")"
fi

files=""
index=0
fetch_one() {
    url=$1
    index=$((index + 1))
    yaml=$work_dir/sub-$index.yaml
    case "$url" in
        *\?*) separator='&' ;;
        *)    separator='?' ;;
    esac
    if ! wget -q -T 25 -U 'clash.meta' -O "$yaml" "${url}${separator}target=clash"; then
        log "fetch failed: $url"
        return 0
    fi
    if ! yq -e '.proxies | length > 0' "$yaml" >/dev/null 2>&1; then
        log "no proxies in: $url"
        return 0
    fi
    if ! yq -o=json '.' "$yaml" > "$work_dir/sub-$index.json"; then
        log "cannot parse: $url"
        return 0
    fi
    files="$files $work_dir/sub-$index.json"
}

old_ifs=$IFS
IFS='
'
for url in $urls; do
    IFS=$old_ifs
    url=$(printf '%s' "$url" | tr -d ' \t')
    [ -z "$url" ] && continue
    fetch_one "$url"
    IFS='
'
done
IFS=$old_ifs

if [ -z "$files" ]; then
    log 'no usable subscription; keeping the current configuration'
    exit 0
fi

# 2. Merge the subscriptions. Every node gets a source-and-index prefix so
#    equal node names from different subscriptions cannot collide.
# shellcheck disable=SC2086
jq -s '[range(0; length) as $i
        | .[$i].proxies | to_entries[]
        | .value.name = ("s" + ($i | tostring) + "-" + (.key | tostring) + "|" + .value.name)
        | .value]' \
    $files > "$work_dir/merged.json"

# 3. Build the mihomo config. Nodes are sampled evenly so the selection spans
#    every subscription instead of only its first region.
jq --argjson want "$node_count" --argjson base "$port_base" '
  . as $all
  | ($all | length) as $total
  | (if $total <= $want then [range(0; $total)]
     else [range(0; $want) | (. * $total / $want | floor)] end) as $idx
  | {
      mode: "direct",
      "log-level": "warning",
      ipv6: false,
      rules: ["MATCH,DIRECT"],
      proxies: [$idx[] as $i | $all[$i]],
      listeners: [
        $idx | to_entries[]
        | {
            name: ("o2a-" + (.key | tostring)),
            type: "socks",
            listen: "127.0.0.1",
            port: ($base + .key),
            udp: false,
            proxy: $all[.value].name
          }
      ]
    }
' "$work_dir/merged.json" > "$work_dir/mihomo.json"

yq -p=json -o=yaml '.' "$work_dir/mihomo.json" > "$work_dir/mihomo.yaml"
jq -r '.listeners[] | "socks5://127.0.0.1:\(.port)"' "$work_dir/mihomo.json" > "$work_dir/proxies.txt"
jq -r '.listeners | length' "$work_dir/mihomo.json" > "$work_dir/count"

count=$(cat "$work_dir/count")
total=$(jq -r 'length' "$work_dir/merged.json")
log "selected $count of $total nodes from $(printf '%s' "$files" | wc -w | tr -d ' ') subscriptions"
if [ "$count" -eq 0 ]; then
    log 'no nodes selected; keeping the current configuration'
    exit 0
fi

# 4. Start mihomo. A stale instance from an earlier boot is replaced.
if [ -f "$work_dir/mihomo.pid" ]; then
    old_pid=$(cat "$work_dir/mihomo.pid")
    kill "$old_pid" 2>/dev/null || true
    tries=0
    while kill -0 "$old_pid" 2>/dev/null && [ "$tries" -lt 5 ]; do
        tries=$((tries + 1))
        sleep 1
    done
    kill -9 "$old_pid" 2>/dev/null || true
fi
/usr/local/bin/mihomo -d "$work_dir" -f "$work_dir/mihomo.yaml" > "$work_dir/mihomo.log" 2>&1 &
mihomo_pid=$!
printf '%s\n' "$mihomo_pid" > "$work_dir/mihomo.pid"

# 5. Wait until the last listener is accepting connections. mihomo binds them
#    in order, so the last one proves the whole range is up. /proc/net/tcp
#    lists TCP sockets; state 0A is LISTEN.
wait_listener() {
    hex=$(printf '%04X' "$1")
    awk -v port="$hex" 'index($2, ":" port) && $4 == "0A" { found = 1 } END { exit !found }' /proc/net/tcp 2>/dev/null
}
last_port=$((port_base + count - 1))
ready=0
i=0
while [ "$i" -lt 30 ]; do
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

# 6. Point the app at the pool. Keep one untouched copy of the WARP-era config
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
