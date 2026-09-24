#!/bin/bash
# Queued P3 memory probe: waits for the shared large-model lease, then for GPU
# memory headroom and a host load below 40, and records admission failures.
set -u
cd "$(dirname "$0")/../../.."
OUT=${1:?output dir}; FAIL=${2:?admission failure dir}; NEED_MIB=${3:-32000}
PY=.runtime/duplex-plan/venvs/lychee/bin/python
SNAP=/home/ubuntu/.cache/huggingface/hub/models--Qwen--Qwen3-8B/snapshots/b968826d9c46dd6066d109eabc6255188de91218
fail() { mkdir -p "$FAIL"; printf '{"status":"failed-before-model-load","stage":"%s","reason":"%s","observed_utc":"%s"}\n' "$1" "$2" "$(date -u +%FT%TZ)" > "$FAIL/failure.json"; exit 1; }
exec 9>.runtime/duplex-plan/gpu/large.lock
flock -w 86400 9 || fail exclusive-gpu-lock-admission "flock timeout after 86400 s"
for i in $(seq 1 1440); do
  gpu=$(nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits | head -1)
  load=$(awk '{print int($1)}' /proc/loadavg)
  [ "$gpu" -ge "$NEED_MIB" ] && [ "$load" -lt 40 ] && break
  [ "$i" = 1440 ] && fail resource-headroom "GPU free ${gpu} MiB (need ${NEED_MIB}), load ${load} after 12 h"
  sleep 30
done
exec "$PY" tools/interactionstudy/train/memory_probe.py --snapshot "$SNAP" --out "$OUT" --seq-len 4096 --steps 8
