#!/usr/bin/env bash
# Kyutai delayed-streams TTS (kyutai/tts-1.6b-en_fr) behind the duplex-plan
# synthesis contract on :9125: OpenAI /v1/audio/speech (complete text, PCM
# stream) and the openrealtime-incremental-speech/1 WebSocket /v1/tts/stream
# (words appended into the running generation; incremental_text=true).
#
#   moshi      0.2.13, torch 2.9.1+cu128, venv .runtime/duplex-plan/venvs/kyutai
#   weights    kyutai/tts-1.6b-en_fr @ f65439609986c392cb12df63938abcc550c3fb15
#   voices     kyutai/tts-voices (default expresso/ex03-ex01_happy_001_channel1_334s.wav)
#   languages  English, French
#
# ~5 GB of GPU memory: a small model, no large-model lease.
#
#   deploy/duplex/services/kyutai-tts.sh start    # background, PID in .runtime/duplex-plan/pids/kyutai-tts.pid
#   deploy/duplex/services/kyutai-tts.sh stop
#   deploy/duplex/services/kyutai-tts.sh status
#
# Runtime side (complete-text route):
#   openrealtime serve ... -tts-provider openai-compatible -tts-url http://127.0.0.1:9125/v1/audio/speech \
#     -tts-model kyutai/tts-1.6b-en_fr -tts-voice default
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
PORT=${KYUTAI_TTS_PORT:-9125}
VENV=${KYUTAI_VENV:-$PLAN/venvs/kyutai}
ROWS=${KYUTAI_TTS_ROWS:-4}
PIDFILE=$PLAN/pids/kyutai-tts.pid
LOG=$PLAN/logs/kyutai-tts.log

running() { [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }

case ${1:-status} in
  start)
    if running; then echo "already running (pid $(cat "$PIDFILE"))"; exit 0; fi
    mkdir -p "$PLAN/pids" "$PLAN/logs"
    cd "$ROOT"
    PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True setsid nohup \
      "$VENV/bin/python" tools/duplexmodels/tts_kyutai.py --port "$PORT" --rows "$ROWS" \
      > "$LOG" 2>&1 < /dev/null &
    echo $! > "$PIDFILE"
    for _ in $(seq 1 150); do
      if curl -sf "http://127.0.0.1:$PORT/health" > /dev/null; then echo "ready on :$PORT (pid $(cat "$PIDFILE"))"; exit 0; fi
      running || { echo "exited; see $LOG"; exit 1; }
      sleep 2
    done
    echo "not ready after 300 s; see $LOG"; exit 1 ;;
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
