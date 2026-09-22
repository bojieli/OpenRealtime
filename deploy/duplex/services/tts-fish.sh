#!/usr/bin/env bash
# Duplex-plan P3/C4 local TTS service: tools/duplexmodels/tts_fish.py on :9124.
# GPU: LARGE (~20 GB; takes the large-model lease). Serves POST /v1/audio/speech and WS /v1/tts/stream.
#
#   deploy/duplex/services/tts-fish.sh [start|stop|status] [service args...]
#
# Environment: build it first with deploy/duplex/services/tts-setup.sh.
set -euo pipefail
ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
PORT=${PORT:-9124}
NAME=tts-fish
LOG=$PLAN/logs/$NAME.log
PIDFILE=$PLAN/pids/$NAME.pid
action=${1:-start}
[ $# -gt 0 ] && shift
case "$action" in
  start)
    export PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1
    cd "$ROOT"
    # bash writes its own PID (the session leader, whatever setsid does about
    # forking) and then execs the service, so stop can signal the group.
    setsid nohup bash -c 'echo $$ > "$0"; exec "$@"' "$PIDFILE" flock -w 14400 "$PLAN/gpu/large.lock" "$ROOT/.runtime/fish-env/bin/python" tools/duplexmodels/tts_fish.py --port "$PORT" "$@" > "$LOG" 2>&1 < /dev/null &
    sleep 1
    echo "$NAME pid $(cat "$PIDFILE") port $PORT log $LOG (ready when the log says 'ready on :$PORT')"
    ;;
  stop)
    # The launcher is a session leader; stop its whole process group.
    [ -f "$PIDFILE" ] && kill -- -"$(cat "$PIDFILE")" 2>/dev/null && echo "stopped $NAME" || echo "$NAME not running"
    rm -f "$PIDFILE"
    ;;
  status)
    curl -sf "http://127.0.0.1:$PORT/health" || { echo "$NAME not answering on :$PORT"; exit 1; }
    echo
    ;;
  *) echo "usage: $0 start|stop|status" >&2; exit 2 ;;
esac
