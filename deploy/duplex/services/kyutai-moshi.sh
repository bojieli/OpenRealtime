#!/usr/bin/env bash
# Moshi (kyutai/moshiko-pytorch-bf16) native full-duplex model as a persistent
# sidecar (protocol v1 over TCP) for `openrealtime serve -binding duplex`.
#
#   moshi      0.2.13, torch 2.9.1+cu128 (venv ~/moshi-venv, or KYUTAI_VENV)
#   weights    kyutai/moshiko-pytorch-bf16 @ 2bfc9ae6e89079a5cc7ed2a68436010d91a3d289
#   languages  English only
#   memory     ~17.5 GB peak on one GPU -> needs the large-model lease.
#   speed      ~30 ms per 80 ms frame (encode + LM step + decode) on the RTX PRO 6000
#
# The sidecar loads the weights once, warms up CUDA graphs, and serves one
# session at a time (batch size 1) on tcp:127.0.0.1:$MOSHI_PORT. It runs under
# the large-model lease (flock on .runtime/duplex-plan/gpu/large.lock) for
# exactly as long as it runs. Per-session frame-loop evidence (p50/p95 frame
# time, starved/dropped frames, turns) is appended to $STATS as JSON lines.
#
#   deploy/duplex/services/kyutai-moshi.sh start     # waits for the lease, then for readiness
#   deploy/duplex/services/kyutai-moshi.sh stop
#   deploy/duplex/services/kyutai-moshi.sh status
#
# Runtime side (shared model, no per-session load):
#   openrealtime serve -binding duplex -sidecar-address tcp:127.0.0.1:9140 \
#     -slow-provider openai-compatible -slow-url http://127.0.0.1:9100/v1 -slow-model qwen3-8b
# or one model load per session (~25 s before each session is ready):
#   openrealtime serve -binding duplex \
#     -sidecar "$HOME/moshi-venv/bin/python sidecars/moshi_sidecar.py" ...
#
# What Moshi exposes: its text stream is its own speech (text_delta/text_done),
# never a user transcript; it takes no instructions and no injected text, so
# the engine's background reasoner is never engaged in this preset.
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
PORT=${MOSHI_PORT:-9140}
PYTHON=${MOSHI_PYTHON:-${KYUTAI_VENV:-$HOME/moshi-venv}/bin/python}
PIDFILE=$PLAN/pids/kyutai-moshi.pid
LOG=$PLAN/logs/kyutai-moshi.log
STATS=${MOSHI_SIDECAR_STATS:-$PLAN/results/native/moshi-frame-stats.jsonl}
LEASE_WAIT=${LEASE_WAIT:-14400}

running() { [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }
# Readiness is the sidecar's own log line: a probe connection would open a session.
ready() { grep -q "moshi sidecar listening on tcp:127.0.0.1:$PORT" "$LOG" 2>/dev/null; }

case ${1:-status} in
  start)
    if running; then echo "already running (pid $(cat "$PIDFILE"))"; exit 0; fi
    mkdir -p "$PLAN/pids" "$PLAN/logs" "$(dirname "$STATS")"
    cd "$ROOT"
    rm -f "$PIDFILE"
    # The recorded PID is flock's; the sidecar is its child and holds the
    # lease exactly while it runs.
    PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1 setsid nohup \
      bash -c 'echo $$ > "$0"; exec flock -w "$1" "$2" "${@:3}"' \
      "$PIDFILE" "$LEASE_WAIT" "$PLAN/gpu/large.lock" \
      "$PYTHON" sidecars/moshi_sidecar.py --listen "tcp:127.0.0.1:$PORT" --stats-file "$STATS" \
      > "$LOG" 2>&1 < /dev/null &
    for _ in $(seq 1 50); do [[ -s $PIDFILE ]] && break; sleep 0.1; done
    for _ in $(seq 1 $((LEASE_WAIT / 2 + 150))); do
      if ready; then echo "ready on tcp:127.0.0.1:$PORT (pid $(cat "$PIDFILE"))"; exit 0; fi
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
    if running && ready; then echo "serving on tcp:127.0.0.1:$PORT (pid $(cat "$PIDFILE"))"
    elif running; then echo "starting or waiting for the lease (pid $(cat "$PIDFILE"))"
    else echo "not running"; exit 1; fi ;;
  *) echo "usage: $0 start|stop|status"; exit 2 ;;
esac
