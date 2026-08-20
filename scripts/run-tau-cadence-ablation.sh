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
  local chunk_seconds="$1"
  local chunk_duration="$2"
  local expected_ms="$3"
  "${repository_root}/scripts/local-cascade.sh" stop
  OPENREALTIME_FAST_PROVIDER=vllm \
  OPENREALTIME_FAST_MODEL=qwen-fast \
  OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-0.6B \
  OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.14 \
  OPENREALTIME_ASR_CHUNK_SECONDS="${chunk_seconds}" \
  OPENREALTIME_ASR_PROVIDER_CHUNK="${chunk_duration}" \
  OPENREALTIME_ASR_PROVIDER_MAX_CHUNK=0s \
  OPENREALTIME_SLOW_EFFORT=high \
  OPENREALTIME_PREPARATION_POLICY=continuous \
  OPENREALTIME_SLOW_CONTEXT_POLICY=canonical \
    "${repository_root}/scripts/local-cascade.sh" start
  local health
  health="$("${repository_root}/scripts/fetch-json-endpoint.sh" \
    http://127.0.0.1:8765/healthz "gateway /healthz")"
  if ! jq -e \
    --argjson expected_ms "${expected_ms}" \
    '.asr.model == "Qwen/Qwen3-ASR-0.6B" and
     .asr.provider_chunk_ms == $expected_ms and .slow_context == "canonical" and
     .fast.provider == "vllm" and .fast.model == "qwen-fast" and
     .fast.effort == "minimal" and .fast.tool_authority == "propose" and
     .slow.provider == "google" and .slow.model == "gemini-3.5-flash" and
     .slow.effort == "high" and .slow.tool_authority == "execute"' \
    <<<"${health}" >/dev/null; then
    echo "gateway does not report the preregistered ${expected_ms} ms profile" >&2
    return 1
  fi
}

restore_baseline() {
  if [[ "${restored}" == false ]]; then
    start_profile 0.2 200ms 200
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

# The order is frozen in cadence-ablation-v1.json and alternates around 200 ms
# so monotone machine drift is not confounded with a monotone cadence sweep.
for condition in \
  '100:0.1:100ms:100' \
  '400:0.4:400ms:400' \
  '050:0.05:50ms:50' \
  '800:0.8:800ms:800'; do
  IFS=: read -r tag chunk_seconds chunk_duration expected_ms <<<"${condition}"
  matrix="${repository_root}/benchmarks/tau-voice/matrix-cadence${tag}-v1.json"
  start_profile "${chunk_seconds}" "${chunk_duration}" "${expected_ms}"
  TAU_VOICE_MATRIX="${matrix}" \
    "${repository_root}/scripts/run-tau-voice-matrix.sh"
  "${repository_root}/scripts/report-tau-voice-matrix.sh" "${matrix}"
  "${repository_root}/scripts/archive-tau-voice-artifacts.sh" "${matrix}"
done

restore_baseline
trap - EXIT
echo "tau cadence ablation complete"
