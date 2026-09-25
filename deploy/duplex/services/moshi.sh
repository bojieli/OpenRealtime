#!/usr/bin/env bash
# Pinned Moshi service. Weights/source must already be installed.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
PLAN="$ROOT/.runtime/duplex-plan"
PORT="${MOSHI_PORT:-9148}"
PIDFILE="$PLAN/pids/moshi.pid"
LOG="$PLAN/logs/moshi.log"
REV=2bfc9ae6e89079a5cc7ed2a68436010d91a3d289
SNAPSHOT="${HF_HUB_CACHE:-$HOME/.cache/huggingface/hub}/models--kyutai--moshiko-pytorch-bf16/snapshots/$REV"
PYTHON="$PLAN/venvs/kyutai/bin/python"
owned() {
  [[ -f "$PIDFILE" ]] || return 1
  local pid
  pid="$(cat "$PIDFILE")"
  [[ "$pid" =~ ^[0-9]+$ && -r /proc/$pid/cmdline ]] || return 1
  tr '\0' ' ' < "/proc/$pid/cmdline" | grep -Fq 'sidecars/moshi_sidecar.py'
}
case "${1:-status}" in
 start)
  owned && { echo "already running $(cat "$PIDFILE")"; exit 0; }
  for asset in model.safetensors tokenizer-e351c8d8-checkpoint125.safetensors tokenizer_spm_32k_3.model; do
    [[ -f "$SNAPSHOT/$asset" ]] || { echo "missing local Moshi asset: $asset" >&2; exit 1; }
  done
  "$PYTHON" - "$PORT" <<'PY'
import socket,sys,subprocess
free=int(subprocess.check_output(["nvidia-smi","--query-gpu=memory.free","--format=csv,noheader,nounits"],text=True).splitlines()[0])
if free < 22000: raise SystemExit(f"Moshi requires 22000 MiB free; available {free}")
with socket.socket() as s: s.bind(('127.0.0.1',int(sys.argv[1])))
PY
  mkdir -p "$PLAN/pids" "$PLAN/logs" "$PLAN/results/native"
  cd "$ROOT"
  # Wait up to 60 s for the lease (a previous model may still be exiting),
  # then fail rather than queue behind another active large model.
  env -u HF_TOKEN HF_HUB_OFFLINE=1 setsid nohup \
    flock -w 60 "$PLAN/gpu/large.lock" "$PYTHON" sidecars/moshi_sidecar.py \
    --repository kyutai/moshiko-pytorch-bf16 --revision "$REV" \
    --listen "tcp:127.0.0.1:$PORT" --stats-file "$PLAN/results/native/moshi-stats.jsonl" \
    > "$LOG" 2>&1 < /dev/null &
  pid=$!
  echo "$pid" > "$PIDFILE"
  for ((i=0;i<240;i++)); do
    kill -0 "$pid" 2>/dev/null || { echo "exited; see $LOG (empty: large-model lease still held after 60 s)" >&2; exit 1; }
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
