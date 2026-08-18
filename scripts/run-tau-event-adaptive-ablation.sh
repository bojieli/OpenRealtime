#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
baseline_matrix="${repository_root}/benchmarks/tau-voice/matrix-v1.json"
revision_matrix="${repository_root}/benchmarks/tau-voice/matrix-revision-event-v1.json"
adaptive_matrix="${repository_root}/benchmarks/tau-voice/matrix-adaptive-v1.json"
restored=false

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done

start_profile() {
  local server_chunk_seconds="$1"
  local minimum_chunk="$2"
  local maximum_chunk="$3"
  local expected_minimum_ms="$4"
  local expected_maximum_ms="$5"
  local expected_strategy="$6"

  "${repository_root}/scripts/local-cascade.sh" stop
  OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-0.6B \
  OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.14 \
  OPENREALTIME_ASR_CHUNK_SECONDS="${server_chunk_seconds}" \
  OPENREALTIME_ASR_PROVIDER_CHUNK="${minimum_chunk}" \
  OPENREALTIME_ASR_PROVIDER_MAX_CHUNK="${maximum_chunk}" \
  OPENREALTIME_SLOW_EFFORT=high \
  OPENREALTIME_SLOW_CONTEXT_POLICY=canonical \
    "${repository_root}/scripts/local-cascade.sh" start

  local asr_pid
  asr_pid="$(<"${repository_root}/.runtime/local-cascade/pids/asr.pid")"
  if ! tr '\0' ' ' <"/proc/${asr_pid}/cmdline" | \
    grep -F -- "--chunk-size-sec ${server_chunk_seconds}" >/dev/null; then
    echo "ASR server does not report the preregistered ${server_chunk_seconds} second chunk" >&2
    return 1
  fi

  local health
  health="$(curl --fail --silent --show-error http://127.0.0.1:8765/healthz)"
  if ! jq -e \
    --argjson minimum_ms "${expected_minimum_ms}" \
    --argjson maximum_ms "${expected_maximum_ms}" \
    --arg strategy "${expected_strategy}" \
    '.asr.model == "Qwen/Qwen3-ASR-0.6B" and
     .asr.provider_chunk_ms == $minimum_ms and
     .asr.provider_max_chunk_ms == $maximum_ms and
     .asr.strategy == $strategy and
     .slow_context == "canonical" and
     .fast.provider == "vllm" and .fast.model == "qwen-fast" and
     .fast.effort == "minimal" and .fast.tool_authority == "propose" and
     .slow.provider == "google" and .slow.model == "gemini-3.5-flash" and
     .slow.effort == "high" and .slow.tool_authority == "execute"' \
    <<<"${health}" >/dev/null; then
    echo "gateway does not report the preregistered ${expected_strategy} ASR profile" >&2
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
    OPENREALTIME_SLOW_CONTEXT_POLICY=canonical \
      "${repository_root}/scripts/local-cascade.sh" start || true
  fi
}
trap restore_baseline EXIT

"${repository_root}/scripts/report-tau-voice-matrix.sh" \
  "${baseline_matrix}" "" \
  --validate-only \
  --cell i1-qg-control \
  --cell i1-qg-regular

# Both conditions receive 50 ms input opportunities. The control preserves
# fixed 200 ms provider advances; the candidate changes only the typed
# revision-driven threshold policy.
start_profile 0.2 200ms 0s 200 200 fixed
TAU_VOICE_MATRIX="${revision_matrix}" \
  "${repository_root}/scripts/run-tau-voice-matrix.sh"
"${repository_root}/scripts/report-tau-voice-matrix.sh" "${revision_matrix}"

start_profile 0.1 100ms 400ms 100 400 revision-adaptive
TAU_VOICE_MATRIX="${adaptive_matrix}" \
  "${repository_root}/scripts/run-tau-voice-matrix.sh"
"${repository_root}/scripts/report-tau-voice-matrix.sh" "${adaptive_matrix}"

restore_baseline
trap - EXIT
echo "tau event-adaptive ablation complete"
