#!/usr/bin/env bash
# Audio Flamingo 3 bounded audio observer (plan cell A0): POST /v1/observe on :9160.
#
#   code     tools/duplexmodels/audio_observer.py (transformers AudioFlamingo3ForConditionalGeneration)
#   weights  nvidia/audio-flamingo-3-hf (transformers conversion of nvidia/audio-flamingo-3;
#            NVIDIA OneWay Non-Commercial license - research use only)
#   stack    torch 2.9.1+cu128, transformers 5.17.0, venv .runtime/duplex-plan/venvs/perception
#   memory   bf16 ~16-17 GB resident -> needs the large-model lease (flock on
#            .runtime/duplex-plan/gpu/large.lock, held exactly while the service runs).
#            AUDIO_OBSERVER_QUANTIZE=nf4 loads 4-bit weights; it runs without the lease
#            only when its measured peak is under 8 GB (see results/perception).
#
# The observer is asynchronous and off the 500 ms micro-turn critical path: each
# answer carries available_at_ms / expires_at_ms and the consumer discards stale ones.
#
#   deploy/duplex/services/audio-observer.sh start     # waits for the lease, then for /health
#   deploy/duplex/services/audio-observer.sh stop
#   deploy/duplex/services/audio-observer.sh status
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
PORT=${AUDIO_OBSERVER_PORT:-9160}
VENV=${AUDIO_OBSERVER_VENV:-$PLAN/venvs/perception}
QUANTIZE=${AUDIO_OBSERVER_QUANTIZE:-none}
PIDFILE=$PLAN/pids/audio-observer.pid
LOG=$PLAN/logs/audio-observer.log
LEASE_WAIT=${LEASE_WAIT:-14400}

running() { [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }
ready() { curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; }

case ${1:-status} in
  start)
    if running; then echo "already running (pid $(cat "$PIDFILE"))"; exit 0; fi
    mkdir -p "$PLAN/pids" "$PLAN/logs"
    cd "$ROOT"
    rm -f "$PIDFILE"
    CMD=("$VENV/bin/python" tools/duplexmodels/audio_observer.py serve --port "$PORT" --quantize "$QUANTIZE")
    if [[ $QUANTIZE == none ]]; then
      # The recorded PID is flock's; the service is its child and holds the lease.
      PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1 setsid nohup \
        bash -c 'echo $$ > "$0"; exec flock -w "$1" "$2" "${@:3}"' \
        "$PIDFILE" "$LEASE_WAIT" "$PLAN/gpu/large.lock" "${CMD[@]}" \
        > "$LOG" 2>&1 < /dev/null &
    else
      PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1 setsid nohup \
        bash -c 'echo $$ > "$0"; exec "${@:1}"' "$PIDFILE" "${CMD[@]}" \
        > "$LOG" 2>&1 < /dev/null &
    fi
    for _ in $(seq 1 50); do [[ -s $PIDFILE ]] && break; sleep 0.1; done
    for _ in $(seq 1 $((LEASE_WAIT / 2 + 300))); do
      if ready; then echo "ready on http://127.0.0.1:$PORT (pid $(cat "$PIDFILE"))"; exit 0; fi
      running || { echo "exited; see $LOG"; exit 1; }
      sleep 2
    done
    echo "not ready; see $LOG"; exit 1 ;;
  stop)
    if running; then
      pid=$(cat "$PIDFILE")
      pkill -TERM -P "$pid" 2>/dev/null || true
      kill "$pid" 2>/dev/null || true
      echo stopped
    else echo "not running"; fi
    rm -f "$PIDFILE" ;;
  status)
    if running && ready; then echo "serving on http://127.0.0.1:$PORT (pid $(cat "$PIDFILE"))"; curl -s "http://127.0.0.1:$PORT/health"; echo
    elif running; then echo "starting or waiting for the lease (pid $(cat "$PIDFILE"))"
    else echo "not running"; exit 1; fi ;;
  *) echo "usage: $0 start|stop|status"; exit 2 ;;
esac
