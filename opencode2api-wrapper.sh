#!/bin/sh
# The platform runs the service start command directly, so the image entrypoint
# never sees it. This wrapper prepares the proxy fleet, then becomes the app.
# proxyctl owns mihomo and rewrites config.json in place; a failure leaves the
# previous configuration (and WARP, if enabled) untouched.
set -u

state_dir=${STATE_DIR:-/var/lib/opencode2api}

# Keep the external static pool in sync when a pool control plane is configured.
# pool-sync.sh sources POOL_ENV_FILE itself, so either form works.
if [ "${POOL_SYNC_ENABLE:-1}" = "1" ] \
    && { [ -n "${POOL_API_URL:-}" ] || [ -s "${POOL_ENV_FILE:-$state_dir/upgrade/pool.env}" ]; } \
    && [ -x /usr/local/bin/pool-sync.sh ]; then
    mkdir -p "$state_dir/upgrade" 2>/dev/null || true
    /usr/local/bin/pool-sync.sh --loop "${POOL_SYNC_INTERVAL:-60}" \
        >>"${POOL_SYNC_LOG:-$state_dir/upgrade/pool-sync.log}" 2>&1 &
fi

if [ -n "${CF_SUB_URL:-}" ] \
    || [ -s "$state_dir/cfnew-sub.txt" ] \
    || [ -s "$state_dir/proxycenter/providers.txt" ] \
    || [ -s "$state_dir/proxycenter/merged.json" ]; then
    /usr/local/bin/proxyctl start \
        || printf '%s\n' 'proxyctl failed; continuing with the existing configuration' >&2
fi

exec /usr/local/bin/opencode2api-app "$@"
