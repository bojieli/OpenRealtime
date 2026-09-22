#!/usr/bin/env bash
# MiniCPM-o 4.5 official full-duplex mode as a shared sidecar (plan cell N1).
#
# The sidecar loads the model once through the official demo's own PyTorch code
# (OpenBMB/MiniCPM-o-Demo, "Audio Full-Duplex": prepare -> per-second
# prefill/generate/finalize) and serves OpenRealtime sidecar sessions over TCP,
# one at a time.
#
#   demo       OpenBMB/MiniCPM-o-Demo @ 47709a9210dfd71afa76c058e017fc8c4db5c8d2 (.runtime/duplex-plan/src/MiniCPM-o-Demo)
#   weights    openbmb/MiniCPM-o-4_5 @ 503e754207c94da6bb26850b4469f367c9ea3582
#   runtime    torch 2.8.0+cu128, transformers 4.51.0, minicpmo-utils 1.0.6 (venv .runtime/duplex-plan/venvs/minicpm-duplex,
#              built by .runtime/duplex-plan/setup-minicpm-duplex.sh from the demo's worker Dockerfile)
#
# It is a large model (~20 GB): it runs under the GPU large-model lease for as
# long as it lives.
#
#   deploy/duplex/services/minicpm-o-duplex.sh start|stop|status
#
# Engine side:
#   openrealtime serve -binding duplex -sidecar-address tcp:127.0.0.1:9145 ...
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
PORT=${MINICPM_DUPLEX_PORT:-9145}
PY=${MINICPM_DUPLEX_PYTHON:-$PLAN/venvs/minicpm-duplex/bin/python}
PIDFILE=$PLAN/pids/minicpm-o-duplex.pid
LOG=$PLAN/logs/minicpm-o-duplex.log
METRICS=${MINICPM_DUPLEX_METRICS:-$PLAN/results/native/minicpm-duplex-units.jsonl}

start() {
  if [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "minicpm-o-duplex already running (pid $(cat "$PIDFILE"))"; return 0
  fi
  mkdir -p "$PLAN/pids" "$PLAN/logs" "$(dirname "$METRICS")"
  cd "$ROOT"
  setsid nohup "$ROOT/deploy/duplex/services/minicpm-o-duplex.sh" _attempts "$@" > "$LOG" 2>&1 < /dev/null &
  echo $! > "$PIDFILE"
  echo "minicpm-o-duplex starting (pid $(cat "$PIDFILE"), log $LOG, tcp:127.0.0.1:$PORT)"
}

# GPU room is waited for *outside* the lease: holding the single large-model
# lease while waiting starves every other large model on this host.
_attempts() {
  local attempt=1 started elapsed status
  while (( attempt <= ${MINICPM_DUPLEX_ATTEMPTS:-4} )); do
    echo "=== attempt $attempt"
    _wait_for_memory || return 1
    started=$SECONDS
    # Capture failure explicitly: under set -e a bare flock failure exits
    # before the retry path, including a capacity race after lease acquisition.
    status=0
    flock -w 14400 "$PLAN/gpu/large.lock" "$ROOT/deploy/duplex/services/minicpm-o-duplex.sh" _run "$@" || status=$?
    elapsed=$((SECONDS - started))
    if (( elapsed > 120 )); then
      echo "=== sidecar exited after ${elapsed}s; not retrying"
      return "$status"
    fi
    echo "=== sidecar did not reach a serving state (${elapsed}s, exit $status); retrying"
    attempt=$((attempt + 1))
    sleep 10
  done
  echo "gave up" >&2
  return 1
}

_wait_for_memory() {
  local need=${MINICPM_DUPLEX_NEED_MIB:-24000} waited=0 used total
  while true; do
    read -r used total < <(nvidia-smi --query-gpu=memory.used,memory.total --format=csv,noheader,nounits | tr -d ',')
    if (( total - used >= need )); then break; fi
    if (( waited % 60 == 0 )); then echo "waiting (no lease held) for ${need} MiB free (now $((total - used)) MiB)"; fi
    if (( waited >= ${MINICPM_DUPLEX_MEMORY_WAIT_S:-7200} )); then echo "gave up waiting for GPU memory" >&2; return 1; fi
    sleep 5; waited=$((waited + 5))
  done
  echo "$((total - used)) MiB free; queueing for the large-model lease"
}

_run() {
  local need=${MINICPM_DUPLEX_NEED_MIB:-24000} used total
  read -r used total < <(nvidia-smi --query-gpu=memory.used,memory.total --format=csv,noheader,nounits | tr -d ',')
  if (( total - used < need )); then
    echo "lease acquired but only $((total - used)) MiB is free; releasing it rather than holding it idle"
    return 3
  fi
  echo "lease acquired with $((total - used)) MiB free"
  cd "$ROOT"
  export PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1
  exec "$PY" sidecars/minicpm_o_duplex_sidecar.py --listen "tcp:127.0.0.1:$PORT" \
      --metrics-log "$METRICS" "$@"
}

stop() {
  [[ -f $PIDFILE ]] || { echo "minicpm-o-duplex not running"; return 0; }
  local pid; pid=$(cat "$PIDFILE")
  kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 30); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill -KILL -- "-$pid" 2>/dev/null || true
  rm -f "$PIDFILE"
  echo "minicpm-o-duplex stopped"
}

status() {
  if [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "running pid $(cat "$PIDFILE")"
    grep -E "listening|loaded" "$LOG" | tail -2
  else
    echo "not running"
  fi
}

command=${1:-start}
shift || true
case "$command" in
  start) start "$@" ;;
  _attempts) _attempts "$@" ;;
  _run) _run "$@" ;;
  stop) stop ;;
  status) status ;;
  *) echo "usage: $0 start|stop|status [sidecar args]" >&2; exit 2 ;;
esac
