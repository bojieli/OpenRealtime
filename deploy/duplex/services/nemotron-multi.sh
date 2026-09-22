#!/usr/bin/env bash
# NVIDIA Nemotron 3.5 ASR Streaming 0.6B (multilingual, language-ID prompt;
# cache-aware FastConformer-RNNT)
# behind the duplex-plan streaming recogniser contract (start/chunk/finish +
# stable_text, stability "decoder-append-only") on :9111.
#
#   code       tools/duplexmodels/asr_nemotron.py (incremental conformer_stream_step,
#              carried encoder caches + RNNT hypotheses per session)
#   stack      NeMo 3.1.0 (.runtime/duplex-plan/src/Speech @ 5dbdde6), torch 2.9.1+cu128,
#              venv .runtime/duplex-plan/venvs/nemo (setup: .runtime/duplex-plan/setup-nemo.sh)
#   weights    nvidia/nemotron-3.5-asr-streaming-0.6b @ ea30d66debe3740a08b573244286791d423d6b3e
#   latency    NEMOTRON_MULTI_ATT=56,3 -> 320 ms chunks (240 ms look-ahead, checkpoint default);
#              56,6 -> 560 ms; 56,13 -> 1120 ms; 56,0 -> 80 ms (trained look-aheads of this checkpoint).
#   language   NEMOTRON_MULTI_LANG=auto (language tag detected per utterance and returned as
#              "language"), or a fixed locale prompt such as en-US or zh-CN.
#
# ~3 GB of GPU memory: a small model, no large-model lease.
#
#   deploy/duplex/services/nemotron-multi.sh start    # background, PID in .runtime/duplex-plan/pids/nemotron-multi.pid
#   deploy/duplex/services/nemotron-multi.sh stop
#   deploy/duplex/services/nemotron-multi.sh status
#
# Runtime side:
#   openrealtime serve ... -asr-provider streaming-asr -asr-url http://127.0.0.1:9111 \
#     -asr-model nvidia/nemotron-3.5-asr-streaming-0.6b
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
NAME=nemotron-multi
PORT=${NEMOTRON_MULTI_PORT:-9111}
ATT=${NEMOTRON_MULTI_ATT:-56,3}
LANG_PROMPT=${NEMOTRON_MULTI_LANG:-auto}
MODEL=${NEMOTRON_MULTI_MODEL:-nvidia/nemotron-3.5-asr-streaming-0.6b}
VENV=${NEMO_VENV:-$PLAN/venvs/nemo}
EXTRA=${NEMOTRON_MULTI_ARGS:---cuda-graphs}
PIDFILE=$PLAN/pids/$NAME.pid
LOG=$PLAN/logs/$NAME.log

running() { [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }

case ${1:-status} in
  start)
    if running; then echo "already running (pid $(cat "$PIDFILE"))"; exit 0; fi
    mkdir -p "$PLAN/pids" "$PLAN/logs"
    cd "$ROOT"
    # shellcheck disable=SC2086
    PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1 setsid nohup \
      "$VENV/bin/python" tools/duplexmodels/asr_nemotron.py --model "$MODEL" \
        --att-context-size "$ATT" --language "$LANG_PROMPT" --port "$PORT" $EXTRA \
      > "$LOG" 2>&1 < /dev/null &
    echo $! > "$PIDFILE"
    for _ in $(seq 1 150); do
      # Ready only when the answering server is this process (not one still shutting down).
      if curl -sf "http://127.0.0.1:$PORT/health" | grep -q "\"pid\":$(cat "$PIDFILE")[,}]"; then
        echo "ready on :$PORT (pid $(cat "$PIDFILE"))"; exit 0
      fi
      running || { echo "exited; see $LOG"; exit 1; }
      sleep 2
    done
    echo "not ready after 300 s; see $LOG"; exit 1 ;;
  stop)
    if running; then
      pid=$(cat "$PIDFILE"); kill "$pid"
      for _ in $(seq 1 60); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
      echo stopped
    else echo "not running"; fi
    rm -f "$PIDFILE" ;;
  status)
    if running; then curl -s "http://127.0.0.1:$PORT/health"; echo; else echo "not running"; exit 1; fi ;;
  *) echo "usage: $0 start|stop|status"; exit 2 ;;
esac
