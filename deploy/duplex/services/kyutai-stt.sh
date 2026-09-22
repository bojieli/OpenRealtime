#!/usr/bin/env bash
# Kyutai delayed-streams STT (kyutai/stt-1b-en_fr) behind the duplex-plan
# streaming recogniser contract (start/chunk/finish + stable_text) on :9112.
#
#   moshi      0.2.13, torch 2.9.1+cu128, venv .runtime/duplex-plan/venvs/kyutai
#   weights    kyutai/stt-1b-en_fr @ 1c34c6b4f7e9299bb61985f145052ff131005dde
#   languages  English, French (no Mandarin)
#
# ~3 GB of GPU memory: a small model, no large-model lease.
#
#   deploy/duplex/services/kyutai-stt.sh start    # background, PID in .runtime/duplex-plan/pids/kyutai-stt.pid
#   deploy/duplex/services/kyutai-stt.sh stop
#   deploy/duplex/services/kyutai-stt.sh status
#
# Runtime side:
#   openrealtime serve ... -asr-provider streaming-asr -asr-url http://127.0.0.1:9112 -asr-model kyutai/stt-1b-en_fr
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
PORT=${KYUTAI_STT_PORT:-9112}
VENV=${KYUTAI_VENV:-$PLAN/venvs/kyutai}
SLOTS=${KYUTAI_STT_SLOTS:-8}
PIDFILE=$PLAN/pids/kyutai-stt.pid
LOG=$PLAN/logs/kyutai-stt.log

running() { [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }

case ${1:-status} in
  start)
    if running; then echo "already running (pid $(cat "$PIDFILE"))"; exit 0; fi
    mkdir -p "$PLAN/pids" "$PLAN/logs"
    cd "$ROOT"
    PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True setsid nohup \
      "$VENV/bin/python" tools/duplexmodels/asr_kyutai.py --port "$PORT" --slots "$SLOTS" \
      > "$LOG" 2>&1 < /dev/null &
    echo $! > "$PIDFILE"
    for _ in $(seq 1 120); do
      if curl -sf "http://127.0.0.1:$PORT/health" > /dev/null; then echo "ready on :$PORT (pid $(cat "$PIDFILE"))"; exit 0; fi
      running || { echo "exited; see $LOG"; exit 1; }
      sleep 2
    done
    echo "not ready after 240 s; see $LOG"; exit 1 ;;
  stop)
    if running; then
      pid=$(cat "$PIDFILE"); kill "$pid"
      # uvicorn's graceful shutdown waits for open streams; do not wait forever.
      for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
      kill -0 "$pid" 2>/dev/null && kill -9 "$pid"
      echo stopped
    else echo "not running"; fi
    rm -f "$PIDFILE" ;;
  status)
    if running; then curl -s "http://127.0.0.1:$PORT/health"; echo; else echo "not running"; exit 1; fi ;;
  *) echo "usage: $0 start|stop|status"; exit 2 ;;
esac
