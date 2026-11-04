#!/bin/sh
# The platform runs the service start command directly, so the image entrypoint
# never sees it. This wrapper prepares the proxy fleet, then becomes the app.
# proxyctl owns mihomo and rewrites config.json in place; a failure leaves the
# previous configuration (and WARP, if enabled) untouched.
set -u

state_dir=${STATE_DIR:-/var/lib/opencode2api}

if [ -n "${CF_SUB_URL:-}" ] \
    || [ -s "$state_dir/cfnew-sub.txt" ] \
    || [ -s "$state_dir/proxycenter/providers.txt" ] \
    || [ -s "$state_dir/proxycenter/merged.json" ]; then
    /usr/local/bin/proxyctl start \
        || printf '%s\n' 'proxyctl failed; continuing with the existing configuration' >&2
fi

exec /usr/local/bin/opencode2api-app "$@"
