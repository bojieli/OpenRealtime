#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
baseline_matrix="${repository_root}/benchmarks/tau-voice/matrix-canonical-v1.json"
restored=false

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done

start_profile() {
  local policy="$1"
  "${repository_root}/scripts/local-cascade.sh" stop
  OPENREALTIME_FAST_PROVIDER=vllm \
  OPENREALTIME_FAST_MODEL=qwen-fast \
  OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-0.6B \
  OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.14 \
  OPENREALTIME_ASR_CHUNK_SECONDS=0.2 \
  OPENREALTIME_ASR_PROVIDER_CHUNK=200ms \
  OPENREALTIME_ASR_PROVIDER_MAX_CHUNK=0s \
  OPENREALTIME_SLOW_EFFORT=high \
  OPENREALTIME_PREPARATION_POLICY=continuous \
  OPENREALTIME_SLOW_CONTEXT_POLICY="${policy}" \
    "${repository_root}/scripts/local-cascade.sh" start
  local health
  health="$(curl --fail --silent --show-error http://127.0.0.1:8765/healthz)"
  if ! jq -e \
    --arg policy "${policy}" \
    '.asr.model == "Qwen/Qwen3-ASR-0.6B" and
     .asr.provider_chunk_ms == 200 and .slow_context == $policy and
     .fast.provider == "vllm" and .fast.model == "qwen-fast" and
     .fast.effort == "minimal" and .fast.tool_authority == "propose" and
     .slow.provider == "google" and .slow.model == "gemini-3.5-flash" and
     .slow.effort == "high" and .slow.tool_authority == "execute"' \
    <<<"${health}" >/dev/null; then
    echo "gateway does not report the preregistered ${policy} context profile" >&2
    return 1
  fi
}

restore_baseline() {
  if [[ "${restored}" == false ]]; then
    start_profile canonical
    restored=true
  fi
}
cleanup() {
  restore_baseline || true
}
trap cleanup EXIT

"${repository_root}/scripts/report-tau-voice-matrix.sh" \
  "${baseline_matrix}" "" \
  --validate-only \
  --cell i1-qg-canonical-control \
  --cell i1-qg-canonical-regular

for policy in content-only independent; do
  matrix="${repository_root}/benchmarks/tau-voice/matrix-${policy}-v1.json"
  start_profile "${policy}"
  TAU_VOICE_MATRIX="${matrix}" \
    "${repository_root}/scripts/run-tau-voice-matrix.sh"
  "${repository_root}/scripts/report-tau-voice-matrix.sh" "${matrix}"
  "${repository_root}/scripts/archive-tau-voice-artifacts.sh" "${matrix}"
done

restore_baseline
trap - EXIT
echo "tau context-projection ablation complete"
