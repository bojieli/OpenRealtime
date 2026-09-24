#!/bin/bash
# Queued native DuplexCascade run from the capability worktree. Waits for the
# shared large-model lease, then for GPU/RAM headroom and a host load below 40
# (native timing is latency-sensitive). Admission failures are retained.
set -u
cd "$(dirname "$0")/../.."
PAIR=${1:?prepared pair.json}; OUT=${2:?output dir}; FAIL=${3:?admission failure dir}
PY=.runtime/duplex-plan/venvs/ellsa/bin/python
fail() { mkdir -p "$FAIL"; printf '{"status":"failed-before-model-load","stage":"%s","reason":"%s","trials_started":false,"capability_score":null,"observed_utc":"%s"}\n' "$1" "$2" "$(date -u +%FT%TZ)" > "$FAIL/failure.json"; exit 1; }
exec 9>.runtime/duplex-plan/gpu/large.lock
flock -w 43200 9 || fail exclusive-gpu-lock-admission "flock timeout after 43200 s"
for i in $(seq 1 240); do
  mem=$(awk '/MemAvailable/{print int($2/1048576)}' /proc/meminfo)
  gpu=$(nvidia-smi --query-gpu=memory.free --format=csv,noheader,nounits | head -1)
  load=$(awk '{print int($1)}' /proc/loadavg)
  [ "$mem" -ge 24 ] && [ "$gpu" -ge 20000 ] && [ "$load" -lt 40 ] && break
  [ "$i" = 240 ] && fail resource-headroom "MemAvailable ${mem} GiB, GPU free ${gpu} MiB, load ${load} after 2 h"
  sleep 30
done
curl -fsS -m 5 http://127.0.0.1:9125/health >/dev/null || fail tts-health "Kyutai :9125 not healthy"
exec "$PY" tools/interactionstudy/native_live.py \
  --source .runtime/duplex-plan/src/DuplexCascade \
  --snapshot /home/ubuntu/.cache/huggingface/hub/models--sbintuitions--DuplexCascade/snapshots/31c038ece2f006a28722dd60d1df3868fbb2cc42 \
  --base /home/ubuntu/.cache/huggingface/hub/models--Qwen--Qwen2-7B-Instruct/snapshots/f2826a00ceef68f0f2b946d945ecc0477ce4450c \
  --prepared-pair "$PAIR" --words-per-tick 2 --out "$OUT"
