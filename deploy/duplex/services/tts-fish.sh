#!/usr/bin/env bash
# Duplex-plan P3/C4 local TTS service: tools/duplexmodels/tts_fish.py on :9124.
# GPU: LARGE (~20 GB; takes the large-model lease). Serves POST /v1/audio/speech and WS /v1/tts/stream.
#
#   deploy/duplex/services/tts-fish.sh [start|stop|status] [service args...]
#
# Environment: build it first with deploy/duplex/services/tts-setup.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
PLAN=$ROOT/.runtime/duplex-plan
PORT=${PORT:-9124}
NAME=tts-fish
LOG=$PLAN/logs/$NAME.log
PIDFILE=$PLAN/pids/$NAME.pid
owned() {
    [[ -f "$PIDFILE" ]] || return 1
    local pid
    pid=$(cat "$PIDFILE")
    [[ "$pid" =~ ^[0-9]+$ && -r /proc/$pid/cmdline ]] || return 1
    tr '\0' ' ' < "/proc/$pid/cmdline" | grep -Fq 'tools/duplexmodels/tts_fish.py'
}
action=${1:-start}
[ $# -gt 0 ] && shift
case "$action" in
  start)
    owned && { echo "$NAME already running (pid $(cat "$PIDFILE"))"; exit 0; }
    mkdir -p "$PLAN/pids" "$PLAN/logs" "$PLAN/gpu"
    "$ROOT/.runtime/fish-env/bin/python" - "$PORT" <<'PYTHON'
import socket, subprocess, sys
with socket.socket() as sock:
    sock.bind(('127.0.0.1', int(sys.argv[1])))
free = int(subprocess.check_output(['nvidia-smi', '--query-gpu=memory.free',
                                  '--format=csv,noheader,nounits'], text=True).splitlines()[0])
if free < 22000:
    raise SystemExit(f'Fish requires 22000 MiB free before loading; available {free}')
PYTHON
    export PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1
    cd "$ROOT"
    # Fail immediately if the lease is busy; never wait for capacity while
    # holding it. Stop targets only the verified service process group.
    setsid nohup flock -n "$PLAN/gpu/large.lock" "$ROOT/.runtime/fish-env/bin/python" \
      tools/duplexmodels/tts_fish.py --port "$PORT" "$@" > "$LOG" 2>&1 < /dev/null &
    pid=$!
    echo "$pid" > "$PIDFILE"
    for ((i=0;i<300;i++)); do
      kill -0 "$pid" 2>/dev/null || { echo "exited; see $LOG" >&2; exit 1; }
      if curl -sf "http://127.0.0.1:$PORT/health" >/dev/null; then
        echo "ready on :$PORT (pid $pid)"; exit 0
      fi
      sleep 1
    done
    kill -TERM -- "-$pid" 2>/dev/null || true
    echo "readiness timed out; see $LOG" >&2; exit 1
    ;;
  stop)
    if owned; then
      pid=$(cat "$PIDFILE")
      kill -TERM -- "-$pid"
      for ((i=0;i<20;i++)); do
        kill -0 "$pid" 2>/dev/null || break
        sleep .5
      done
      if owned; then kill -KILL -- "-$pid" 2>/dev/null || true; fi
      echo "stopped $NAME"
    else
      echo "$NAME not running"
    fi
    rm -f "$PIDFILE"
    ;;
  status)
    curl -sf "http://127.0.0.1:$PORT/health" || { echo "$NAME not answering on :$PORT"; exit 1; }
    echo
    ;;
  *) echo "usage: $0 start|stop|status" >&2; exit 2 ;;
esac
