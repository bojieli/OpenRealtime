#!/usr/bin/env bash
# Interaction predictors (plan stage P6, cells I0/I1): tools/duplexmodels/turn_server.py
#
#   core    :9130  Smart Turn v3.2 (ONNX, GPU fp32 + CPU int8), LiveKit turn-detector
#                  v0.4.1-intl (ONNX int8, CPU), stereo VAP (CPC+GPT), DualTurn
#                  (Mimi + Qwen2.5-0.5B). venv .runtime/duplex-plan/venvs/turn
#                  (torch 2.9.1+cu128, transformers 4.57, onnxruntime-gpu 1.23.2 = CUDA 12).
#                  ~3.5 GB GPU -> small-model rule (load only with >= 10 GB free).
#   x2turn  :9131  X2-Turn-4B-0812 (Voxtral Mini 4B Realtime + turn head), bf16 ~10 GB ->
#                  large-model lease. venv .runtime/duplex-plan/venvs/x2turn (transformers 5.17).
#   soulx   :9132  SoulX-Duplug-0.6B (+ GLM-4-Voice tokenizer, SenseVoiceSmall ASR).
#                  venv .runtime/duplex-plan/venvs/soulx (upstream pins: transformers 4.52.1,
#                  funasr 1.2.6, WeTextProcessing 1.0.3; torch 2.9.1+cu128 for Blackwell).
#
# Every route is observation-only evidence; see the module docstring for the wire format.
#
#   deploy/duplex/services/turn.sh start [core|x2turn|soulx]
#   deploy/duplex/services/turn.sh stop [core|x2turn|soulx]
#   deploy/duplex/services/turn.sh status [core|x2turn|soulx]
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
ACTION=${1:-status}
WHICH=${2:-core}
LEASE_WAIT=${LEASE_WAIT:-14400}

case $WHICH in
  core)   PORT=${TURN_PORT:-9130};   VENV=$PLAN/venvs/turn;   MODELS=${TURN_MODELS:-smart-turn,livekit,vap,dualturn}; LEASE=0 ;;
  x2turn) PORT=${X2TURN_PORT:-9131}; VENV=$PLAN/venvs/x2turn; MODELS=x2turn; LEASE=1 ;;
  soulx)  PORT=${SOULX_PORT:-9132};  VENV=$PLAN/venvs/soulx;  MODELS=soulx;  LEASE=0 ;;
  *) echo "unknown service $WHICH (core|x2turn|soulx)"; exit 2 ;;
esac
NAME=turn-$WHICH
PIDFILE=$PLAN/pids/$NAME.pid
LOG=$PLAN/logs/$NAME.log

running() { [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }
ready() { curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; }

case $ACTION in
  start)
    if running; then echo "$NAME already running (pid $(cat "$PIDFILE"))"; exit 0; fi
    mkdir -p "$PLAN/pids" "$PLAN/logs"
    cd "$ROOT"
    rm -f "$PIDFILE"
    CMD=("$VENV/bin/python" tools/duplexmodels/turn_server.py --port "$PORT" --models "$MODELS")
    if [[ $LEASE == 1 ]]; then
      # The recorded PID is flock's; the server is its child and holds the lease while it runs.
      PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1 setsid nohup \
        bash -c 'echo $$ > "$0"; exec flock -w "$1" "$2" "${@:3}"' \
        "$PIDFILE" "$LEASE_WAIT" "$PLAN/gpu/large.lock" "${CMD[@]}" \
        > "$LOG" 2>&1 < /dev/null &
    else
      PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1 MODELSCOPE_OFFLINE=1 setsid nohup \
        bash -c 'echo $$ > "$0"; exec "${@:1}"' "$PIDFILE" "${CMD[@]}" \
        > "$LOG" 2>&1 < /dev/null &
    fi
    for _ in $(seq 1 50); do [[ -s $PIDFILE ]] && break; sleep 0.1; done
    for _ in $(seq 1 $((LEASE_WAIT / 2 + 600))); do
      if ready; then echo "$NAME ready on http://127.0.0.1:$PORT (pid $(cat "$PIDFILE"))"; exit 0; fi
      running || { echo "$NAME exited; see $LOG"; tail -5 "$LOG"; exit 1; }
      sleep 2
    done
    echo "$NAME not ready; see $LOG"; exit 1 ;;
  stop)
    if running; then
      # setsid made the recorded PID a process-group leader: signal the group so a
      # server started under flock goes down with it (and releases the lease).
      kill -- -"$(cat "$PIDFILE")" 2>/dev/null || kill "$(cat "$PIDFILE")"
      for _ in $(seq 1 30); do running || break; sleep 1; done
      running && kill -9 -- -"$(cat "$PIDFILE")"
      echo "$NAME stopped"
    else
      echo "$NAME not running"
    fi
    rm -f "$PIDFILE" ;;
  status)
    if running; then
      echo "$NAME running (pid $(cat "$PIDFILE"))"
      curl -sf "http://127.0.0.1:$PORT/health" | head -c 2000; echo
    else
      echo "$NAME not running"
    fi ;;
  *) echo "usage: $0 start|stop|status [core|x2turn|soulx]"; exit 2 ;;
esac
