#!/usr/bin/env bash
# Pinned DuplexCascade service. Weights/source must already be installed.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
PLAN="$ROOT/.runtime/duplex-plan"
PORT="${DUPLEXCASCADE_PORT:-9147}"
PIDFILE="$PLAN/pids/duplexcascade.pid"
LOG="$PLAN/logs/duplexcascade.log"
SOURCE="$PLAN/src/DuplexCascade"
REV=42893024ca90c8de8ac3ed624467ebc123512ff8
SNAPSHOT="${HF_HUB_CACHE:-$HOME/.cache/huggingface/hub}/models--sbintuitions--DuplexCascade/snapshots/31c038ece2f006a28722dd60d1df3868fbb2cc42"
PYTHON="$PLAN/venvs/ellsa/bin/python"
BASE="${HF_HUB_CACHE:-$HOME/.cache/huggingface/hub}/models--Qwen--Qwen2-7B-Instruct/snapshots/f2826a00ceef68f0f2b946d945ecc0477ce4450c"
owned() {
  [[ -f "$PIDFILE" ]] || return 1
  local pid
  pid="$(cat "$PIDFILE")"
  [[ "$pid" =~ ^[0-9]+$ && -r /proc/$pid/cmdline ]] || return 1
  tr '\0' ' ' < "/proc/$pid/cmdline" | grep -Fq 'sidecars/duplexcascade_sidecar.py'
}
case "${1:-status}" in
 start)
  owned && { echo "already running $(cat "$PIDFILE")"; exit 0; }
  [[ "$(git -C "$SOURCE" rev-parse HEAD)" == "$REV" ]] || { echo 'unexpected source revision' >&2; exit 1; }
  [[ -z "$(git -C "$SOURCE" status --porcelain -- . ":(exclude)__pycache__")" ]] || { echo 'modified DuplexCascade source' >&2; exit 1; }
  [[ -f "$SNAPSHOT/model_state.safetensors" && -f "$BASE/config.json" ]] || { echo 'missing local checkpoint or base model' >&2; exit 1; }
  "$PYTHON" - "$PORT" <<'PY'
import socket,sys
with socket.socket() as s: s.bind(('127.0.0.1',int(sys.argv[1])))
PY
  mkdir -p "$PLAN/pids" "$PLAN/logs" "$PLAN/results/native"
  cd "$ROOT"
  # No queue while owning the lock; fail if another large model is active.
  env -u HF_TOKEN HF_HUB_OFFLINE=1 OMP_NUM_THREADS=4 setsid nohup \
    flock -n "$PLAN/gpu/large.lock" "$PYTHON" sidecars/duplexcascade_sidecar.py \
    --source "$SOURCE" --snapshot "$SNAPSHOT" --base "$BASE" \
    --listen "tcp:127.0.0.1:$PORT" \
    > "$LOG" 2>&1 < /dev/null &
  pid=$!
  echo "$pid" > "$PIDFILE"
  for ((i=0;i<180;i++)); do
    kill -0 "$pid" 2>/dev/null || { echo "exited; see $LOG" >&2; exit 1; }
    if grep -Fq "sidecar listening on tcp:127.0.0.1:$PORT" "$LOG"; then
      echo "ready on tcp:127.0.0.1:$PORT (pid $pid)"; exit 0
    fi
    sleep 1
  done
  kill -TERM -- "-$pid" 2>/dev/null || true
  echo "readiness timed out; see $LOG" >&2; exit 1 ;;
 stop)
  if owned; then kill -TERM -- "-$(cat "$PIDFILE")"; fi
  rm -f "$PIDFILE" ;;
 status)
  if owned; then echo "running $(cat "$PIDFILE")"; else echo 'not running'; exit 1; fi ;;
 *) echo "usage: $0 start|stop|status" >&2; exit 2 ;;
esac
