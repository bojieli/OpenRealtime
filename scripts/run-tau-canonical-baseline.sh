#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
matrix="${repository_root}/benchmarks/tau-voice/matrix-canonical-v1.json"

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN OPENREALTIME_GATEWAY_BINARY OPENREALTIME_GATEWAY_SHA256; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done

"${repository_root}/scripts/local-cascade.sh" stop
OPENREALTIME_FAST_PROVIDER=vllm \
OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-0.6B \
OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.14 \
OPENREALTIME_ASR_CHUNK_SECONDS=0.2 \
OPENREALTIME_ASR_PROVIDER_CHUNK=200ms \
OPENREALTIME_ASR_PROVIDER_MAX_CHUNK=0s \
OPENREALTIME_SLOW_EFFORT=high \
OPENREALTIME_PREPARATION_POLICY=continuous \
OPENREALTIME_SLOW_CONTEXT_POLICY=canonical \
  "${repository_root}/scripts/local-cascade.sh" start

TAU_VOICE_MATRIX="${matrix}" \
  "${repository_root}/scripts/run-tau-voice-matrix.sh"
"${repository_root}/scripts/report-tau-voice-matrix.sh" "${matrix}" "" --validate-only
"${repository_root}/scripts/archive-tau-voice-artifacts.sh" "${matrix}"
