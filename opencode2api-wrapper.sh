#!/bin/sh
# The platform runs the service start command directly, so the image entrypoint
# never sees it. This wrapper keeps one path: prepare the cfnew exits, then
# become the app. The sidecar rewrites config.json in place; a failure leaves
# the previous configuration (and WARP, if enabled) untouched.
set -u

state_dir=${STATE_DIR:-/var/lib/opencode2api}

if [ -n "${CF_SUB_URL:-}" ] || [ -s "$state_dir/cfnew-sub.txt" ]; then
    /usr/local/bin/cfnew-sidecar \
        || printf '%s\n' 'cfnew sidecar failed; continuing with the existing configuration' >&2
fi

exec /usr/local/bin/opencode2api-app "$@"
