#!/usr/bin/env bash
# Pinned PersonaPlex service. Weights/source must already be installed.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
PLAN="$ROOT/.runtime/duplex-plan"
PORT="${PERSONAPLEX_PORT:-9146}"
PIDFILE="$PLAN/pids/personaplex.pid"
LOG="$PLAN/logs/personaplex.log"
SOURCE="$PLAN/src/personaplex"
REV=3428dfd95309a7f3c84fd93259ded0f810d1ff91
SNAPSHOT="${HF_HUB_CACHE:-$HOME/.cache/huggingface/hub}/models--nvidia--personaplex-7b-v1/snapshots/fdaf4090a61cb315c138a1faee287ffd6c716309"
PYTHON="$PLAN/venvs/kyutai/bin/python"
owned() {
  [[ -f "$PIDFILE" ]] || return 1
  local pid
  pid="$(cat "$PIDFILE")"
  [[ "$pid" =~ ^[0-9]+$ && -r /proc/$pid/cmdline ]] || return 1
  tr '\0' ' ' < "/proc/$pid/cmdline" | grep -Fq 'sidecars/personaplex_sidecar.py'
}
case "${1:-status}" in
 start)
  owned && { echo "already running $(cat "$PIDFILE")"; exit 0; }
  [[ "$(git -C "$SOURCE" rev-parse HEAD)" == "$REV" ]] || { echo 'unexpected source revision' >&2; exit 1; }
  [[ -z "$(git -C "$SOURCE" status --porcelain)" ]] || { echo 'modified PersonaPlex source' >&2; exit 1; }
  [[ -f "$SNAPSHOT/model.safetensors" && -f "$PLAN/data/personaplex/voices/NATF2.pt" ]] || { echo 'missing local model or voice prompt' >&2; exit 1; }
  "$PYTHON" - "$PORT" <<'PY'
import socket,sys
with socket.socket() as s: s.bind(('127.0.0.1',int(sys.argv[1])))
PY
  mkdir -p "$PLAN/pids" "$PLAN/logs" "$PLAN/results/native"
  cd "$ROOT"
  # No queue while owning the lock; fail if another large model is active.
  env -u HF_TOKEN HF_HUB_OFFLINE=1 PYTHONPATH="$SOURCE/moshi" setsid nohup \
    flock -n "$PLAN/gpu/large.lock" "$PYTHON" sidecars/personaplex_sidecar.py \
    --snapshot "$SNAPSHOT" --voice "$PLAN/data/personaplex/voices/NATF2.pt" \
    --listen "tcp:127.0.0.1:$PORT" --stats-file "$PLAN/results/native/personaplex-stats.jsonl" \
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
