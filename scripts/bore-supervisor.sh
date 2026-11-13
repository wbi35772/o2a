#!/usr/bin/env bash
# bore-supervisor.sh - keep a local TCP port range exposed through a public bore relay.
#
# Use this on nodes behind NAT or on PaaS/CI hosts with no inbound public ports:
# the producer keeps listening on 127.0.0.1:PORT_START..PORT_END, and this
# supervisor keeps one `bore local` tunnel per port alive. The gateway receives
# the relay endpoints (BORE_SERVER:BORE_PORT_START..) instead of the node IP.
#
# Remote mapping is linear:
#   remote = BORE_PORT_START + (local - PORT_START)
#
# The script only manages ports in PORT_START..PORT_END, stores one pidfile per
# port, verifies the pid still belongs to the expected `bore local <port>`
# command before trusting it, and restarts only that port. SIGTERM/SIGINT stops
# the tunnels it owns. Do not run two supervisors for the same port range.
#
# Environment:
#   LOCAL_HOST         local address bore connects to        (default 127.0.0.1)
#   PORT_START         first local port                      (default 1081)
#   PORT_END           last local port                       (default 1096)
#   BORE_PORT_START    remote port for PORT_START            (default 40001)
#   BORE_SERVER        bore relay host                       (default bore.pub)
#   BORE_BIN           bore binary path                      (default /tmp/bore)
#   BORE_VERSION       release used for auto-download        (default 0.6.0)
#   BORE_DOWNLOAD_URL  explicit tarball URL (overrides arch) (default empty)
#   CHECK_INTERVAL     seconds between health sweeps         (default 30)
#   LOG_DIR            pid/log directory                     (default /tmp)
#   DRY_RUN            1 = print commands, do not start      (default 0)
#
# Examples:
#   bash scripts/bore-supervisor.sh
#   PORT_START=2081 PORT_END=2096 BORE_PORT_START=41001 bash scripts/bore-supervisor.sh
#   DRY_RUN=1 bash scripts/bore-supervisor.sh
set -u

usage() {
  cat <<'EOF'
bore-supervisor.sh - keep a local TCP port range exposed through bore.

Environment:
  LOCAL_HOST         local address bore connects to        (default 127.0.0.1)
  PORT_START         first local port                      (default 1081)
  PORT_END           last local port                       (default 1096)
  BORE_PORT_START    remote port for PORT_START            (default 40001)
  BORE_SERVER        bore relay host                       (default bore.pub)
  BORE_BIN           bore binary path                      (default /tmp/bore)
  BORE_VERSION       release used for auto-download        (default 0.6.0)
  BORE_DOWNLOAD_URL  explicit tarball URL (overrides arch) (default empty)
  CHECK_INTERVAL     seconds between health sweeps         (default 30)
  LOG_DIR            pid/log directory                     (default /tmp)
  DRY_RUN            1 = print commands, do not start      (default 0)

Remote mapping:
  remote = BORE_PORT_START + (local - PORT_START)
EOF
}

case "${1:-}" in
  -h|--help) usage; exit 0 ;;
esac

LOCAL_HOST="${LOCAL_HOST:-127.0.0.1}"
PORT_START="${PORT_START:-1081}"
PORT_END="${PORT_END:-1096}"
BORE_PORT_START="${BORE_PORT_START:-40001}"
BORE_SERVER="${BORE_SERVER:-bore.pub}"
BORE_BIN="${BORE_BIN:-/tmp/bore}"
BORE_VERSION="${BORE_VERSION:-0.6.0}"
BORE_DOWNLOAD_URL="${BORE_DOWNLOAD_URL:-}"
CHECK_INTERVAL="${CHECK_INTERVAL:-30}"
LOG_DIR="${LOG_DIR:-/tmp}"
DRY_RUN="${DRY_RUN:-0}"

is_uint() {
  case "$1" in
    ''|*[!0-9]*) return 1 ;;
    *) return 0 ;;
  esac
}

die() {
  echo "bore-supervisor: $*" >&2
  exit 1
}

is_uint "$PORT_START" || die "PORT_START must be a positive integer"
is_uint "$PORT_END" || die "PORT_END must be a positive integer"
is_uint "$BORE_PORT_START" || die "BORE_PORT_START must be a positive integer"
is_uint "$CHECK_INTERVAL" || die "CHECK_INTERVAL must be a positive integer"
[ "$PORT_END" -ge "$PORT_START" ] || die "PORT_END must be >= PORT_START"
[ "$PORT_START" -ge 1 ] && [ "$PORT_END" -le 65535 ] || die "local ports must be in 1..65535"
REMOTE_END=$((BORE_PORT_START + PORT_END - PORT_START))
[ "$BORE_PORT_START" -ge 1 ] && [ "$REMOTE_END" -le 65535 ] || die "remote ports must be in 1..65535"

mkdir -p "$LOG_DIR" || die "cannot create LOG_DIR=$LOG_DIR"

default_download_url() {
  arch="$(uname -m 2>/dev/null || echo unknown)"
  case "$arch" in
    x86_64|amd64) target="x86_64-unknown-linux-musl" ;;
    aarch64|arm64) target="aarch64-unknown-linux-musl" ;;
    *) die "unsupported arch '$arch'; set BORE_DOWNLOAD_URL" ;;
  esac
  echo "https://github.com/ekzhang/bore/releases/download/v${BORE_VERSION}/bore-v${BORE_VERSION}-${target}.tar.gz"
}

ensure_bore() {
  if [ -x "$BORE_BIN" ]; then
    return 0
  fi
  if [ "$DRY_RUN" = "1" ]; then
    echo "would install bore $BORE_VERSION to $BORE_BIN"
    return 0
  fi
  url="${BORE_DOWNLOAD_URL:-$(default_download_url)}"
  tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/bore.XXXXXX")" || die "mktemp failed"
  echo "bore-supervisor: downloading $url"
  if ! curl -fsSL "$url" -o "$tmpdir/bore.tar.gz"; then
    rm -rf "$tmpdir"
    die "download failed: $url"
  fi
  if ! tar -xzf "$tmpdir/bore.tar.gz" -C "$tmpdir"; then
    rm -rf "$tmpdir"
    die "extract failed: $url"
  fi
  if [ ! -f "$tmpdir/bore" ]; then
    rm -rf "$tmpdir"
    die "archive does not contain a 'bore' binary"
  fi
  cp "$tmpdir/bore" "$BORE_BIN" && chmod 0755 "$BORE_BIN" || {
    rm -rf "$tmpdir"
    die "cannot install $BORE_BIN"
  }
  rm -rf "$tmpdir"
}

pidfile_for() { echo "$LOG_DIR/bore-$1.pid"; }
logfile_for() { echo "$LOG_DIR/bore-$1.log"; }

is_ours() {
  port="$1"
  pidfile="$(pidfile_for "$port")"
  [ -f "$pidfile" ] || return 1
  pid="$(cat "$pidfile" 2>/dev/null || true)"
  is_uint "$pid" || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  if [ -r "/proc/$pid/cmdline" ]; then
    cmdline="$(tr '\0' ' ' < "/proc/$pid/cmdline" 2>/dev/null || true)"
    case "$cmdline" in
      *"bore local $port "*) return 0 ;;
      *) return 1 ;;
    esac
  fi
  return 0
}

start_one() {
  port="$1"
  remote="$2"
  if is_ours "$port"; then
    return 0
  fi
  cmd="$BORE_BIN local $port --to $BORE_SERVER --port $remote --local-host $LOCAL_HOST"
  if [ "$DRY_RUN" = "1" ]; then
    echo "$cmd"
    return 0
  fi
  echo "$(date -u +%FT%TZ) start local=$port remote=$BORE_SERVER:$remote" >> "$LOG_DIR/bore-supervisor.log"
  nohup "$BORE_BIN" local "$port" --to "$BORE_SERVER" --port "$remote" --local-host "$LOCAL_HOST" \
    >> "$(logfile_for "$port")" 2>&1 &
  echo $! > "$(pidfile_for "$port")"
}

stop_owned() {
  port="$1"
  pidfile="$(pidfile_for "$port")"
  [ -f "$pidfile" ] || return 0
  if is_ours "$port"; then
    pid="$(cat "$pidfile" 2>/dev/null || true)"
    kill "$pid" 2>/dev/null || true
    i=0
    while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 3 ]; do
      sleep 1
      i=$((i + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
      kill -9 "$pid" 2>/dev/null || true
    fi
  fi
  rm -f "$pidfile"
}

cleanup() {
  status=$?
  trap - TERM INT EXIT
  port="$PORT_START"
  while [ "$port" -le "$PORT_END" ]; do
    stop_owned "$port"
    port=$((port + 1))
  done
  exit "$status"
}
trap cleanup TERM INT EXIT

ensure_bore

echo "bore-supervisor: local $LOCAL_HOST:$PORT_START-$PORT_END -> $BORE_SERVER:$BORE_PORT_START-$REMOTE_END"
echo "bore-supervisor: check every ${CHECK_INTERVAL}s; logs in $LOG_DIR/bore-<port>.log"

while :; do
  port="$PORT_START"
  while [ "$port" -le "$PORT_END" ]; do
    remote=$((BORE_PORT_START + port - PORT_START))
    start_one "$port" "$remote"
    port=$((port + 1))
  done
  if [ "$DRY_RUN" = "1" ]; then
    exit 0
  fi
  sleep "$CHECK_INTERVAL"
done
