#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
continuous_matrix="${repository_root}/benchmarks/tau-voice/matrix-continuous-preparation-v1.json"
endpoint_matrix="${repository_root}/benchmarks/tau-voice/matrix-endpoint-preparation-v1.json"
restored=false

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done

start_profile() {
  local preparation_policy="$1"
  "${repository_root}/scripts/local-cascade.sh" stop
  OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-0.6B \
  OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.14 \
  OPENREALTIME_ASR_CHUNK_SECONDS=0.2 \
  OPENREALTIME_ASR_PROVIDER_CHUNK=200ms \
  OPENREALTIME_ASR_PROVIDER_MAX_CHUNK=0s \
  OPENREALTIME_SLOW_EFFORT=high \
  OPENREALTIME_PREPARATION_POLICY="${preparation_policy}" \
  OPENREALTIME_SLOW_CONTEXT_POLICY=canonical \
    "${repository_root}/scripts/local-cascade.sh" start

  local health
  health="$(curl --fail --silent --show-error http://127.0.0.1:8765/healthz)"
  if ! jq -e \
    --arg preparation_policy "${preparation_policy}" \
    '.asr.model == "Qwen/Qwen3-ASR-0.6B" and
     .asr.provider_chunk_ms == 200 and .asr.provider_max_chunk_ms == 200 and
     .asr.strategy == "fixed" and
     .preparation_policy == $preparation_policy and .slow_context == "canonical" and
     .fast.provider == "vllm" and .fast.model == "qwen-fast" and
     .fast.effort == "minimal" and .fast.tool_authority == "propose" and
     .slow.provider == "google" and .slow.model == "gemini-3.5-flash" and
     .slow.effort == "high" and .slow.tool_authority == "execute" and
     .speech.name == "openai-speech-streaming/fishaudio/s2-pro" and
     .speech.version == "openai-audio-speech-1"' \
    <<<"${health}" >/dev/null; then
    echo "gateway does not report the preregistered ${preparation_policy} preparation profile" >&2
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
    OPENREALTIME_ASR_PROVIDER_MAX_CHUNK=0s \
    OPENREALTIME_SLOW_EFFORT=high \
    OPENREALTIME_PREPARATION_POLICY=continuous \
    OPENREALTIME_SLOW_CONTEXT_POLICY=canonical \
      "${repository_root}/scripts/local-cascade.sh" start || true
  fi
}
trap restore_baseline EXIT

start_profile continuous
TAU_VOICE_MATRIX="${continuous_matrix}" \
  "${repository_root}/scripts/run-tau-voice-matrix.sh"
"${repository_root}/scripts/report-tau-voice-matrix.sh" "${continuous_matrix}"

start_profile endpoint-only
TAU_VOICE_MATRIX="${endpoint_matrix}" \
  "${repository_root}/scripts/run-tau-voice-matrix.sh"
"${repository_root}/scripts/report-tau-voice-matrix.sh" "${endpoint_matrix}"
python3 "${repository_root}/scripts/report-tau-endpoint-preparation.py" \
  --repository-root "${repository_root}" \
  --manifest "${repository_root}/benchmarks/tau-voice/endpoint-preparation-ablation-v1.json"
"${repository_root}/scripts/archive-tau-voice-artifacts.sh" "${continuous_matrix}"
"${repository_root}/scripts/archive-tau-voice-artifacts.sh" "${endpoint_matrix}"

restore_baseline
trap - EXIT
echo "tau endpoint-preparation ablation complete"
