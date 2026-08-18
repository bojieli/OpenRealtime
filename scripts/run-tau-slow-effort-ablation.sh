#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
baseline_matrix="${repository_root}/benchmarks/tau-voice/matrix-v1.json"
candidate_matrix="${repository_root}/benchmarks/tau-voice/matrix-slow-medium-v1.json"
restored=false

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done

start_profile() {
  local effort="$1"
  "${repository_root}/scripts/local-cascade.sh" stop
  OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-0.6B \
  OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.14 \
  OPENREALTIME_ASR_CHUNK_SECONDS=0.2 \
  OPENREALTIME_ASR_PROVIDER_CHUNK=200ms \
  OPENREALTIME_SLOW_EFFORT="${effort}" \
    "${repository_root}/scripts/local-cascade.sh" start
  local health
  health="$(curl --fail --silent --show-error http://127.0.0.1:8765/healthz)"
  if ! jq -e \
    --arg effort "${effort}" \
    '.asr.model == "Qwen/Qwen3-ASR-0.6B" and
     .asr.provider_chunk_ms == 200 and
     .fast.provider == "vllm" and .fast.model == "qwen-fast" and
     .fast.effort == "minimal" and .fast.tool_authority == "propose" and
     .slow.provider == "google" and .slow.model == "gemini-3.5-flash" and
     .slow.effort == $effort and .slow.tool_authority == "execute"' \
    <<<"${health}" >/dev/null; then
    echo "gateway does not report the preregistered slow-${effort} profile" >&2
    return 1
  fi
}

restore_baseline() {
  if [[ "${restored}" == false ]]; then
    restored=true
    "${repository_root}/scripts/local-cascade.sh" stop || true
    OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-0.6B \
    OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.14 \
    OPENREALTIME_ASR_CHUNK_SECONDS=0.2 \
    OPENREALTIME_ASR_PROVIDER_CHUNK=200ms \
    OPENREALTIME_SLOW_EFFORT=high \
      "${repository_root}/scripts/local-cascade.sh" start || true
  fi
}
trap restore_baseline EXIT

"${repository_root}/scripts/report-tau-voice-matrix.sh" \
  "${baseline_matrix}" "" \
  --validate-only \
  --cell i1-qg-control \
  --cell i1-qg-regular

start_profile medium
TAU_VOICE_MATRIX="${candidate_matrix}" \
  "${repository_root}/scripts/run-tau-voice-matrix.sh"
"${repository_root}/scripts/report-tau-voice-matrix.sh" "${candidate_matrix}"

restore_baseline
trap - EXIT
echo "tau slow-effort ablation complete"
