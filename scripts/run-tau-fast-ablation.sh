#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
baseline_data="${repository_root}/.runtime/tau2-bench/data/simulations"
baseline_matrix="${repository_root}/benchmarks/tau-voice/matrix-canonical-v1.json"
candidate_matrix="${repository_root}/benchmarks/tau-voice/matrix-fast-gemini-v1.json"
restored=false

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done

"${repository_root}/scripts/report-tau-voice-matrix.sh" \
  "${baseline_matrix}" "" \
  --validate-only \
  --cell i1-qg-canonical-control \
  --cell i1-qg-canonical-regular

for cell in i1-qg-canonical-control i1-qg-canonical-regular; do
  for domain_tasks in airline:50 retail:114 telecom:114; do
    domain="${domain_tasks%%:*}"
    expected="${domain_tasks##*:}"
    simulation_dir="${baseline_data}/openrealtime-tau-voice-canonical-v1-${cell}-${domain}-seed300/simulations"
    actual=0
    if [[ -d "${simulation_dir}" ]]; then
      actual="$(find "${simulation_dir}" -maxdepth 1 -type f -name '*.json' | wc -l)"
    fi
    if [[ "${actual}" != "${expected}" ]]; then
      echo "paired Qwen-fast baseline ${cell}/${domain} is incomplete: ${actual}/${expected}" >&2
      exit 1
    fi
  done
done

start_fast_profile() {
  local provider="$1"
  local model="$2"
  "${repository_root}/scripts/local-cascade.sh" stop
  OPENREALTIME_FAST_PROVIDER="${provider}" \
  OPENREALTIME_FAST_MODEL="${model}" \
  OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-0.6B \
  OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.14 \
  OPENREALTIME_ASR_CHUNK_SECONDS=0.2 \
  OPENREALTIME_ASR_PROVIDER_CHUNK=200ms \
  OPENREALTIME_ASR_PROVIDER_MAX_CHUNK=0s \
  OPENREALTIME_SLOW_EFFORT=high \
  OPENREALTIME_PREPARATION_POLICY=continuous \
  OPENREALTIME_SLOW_CONTEXT_POLICY=canonical \
    "${repository_root}/scripts/local-cascade.sh" start

  local health
  health="$("${repository_root}/scripts/fetch-json-endpoint.sh" \
    http://127.0.0.1:8765/healthz "gateway /healthz")"
  if ! jq -e \
    --arg provider "${provider}" \
    --arg model "${model}" '
    .asr.model == "Qwen/Qwen3-ASR-0.6B" and
    .asr.provider_chunk_ms == 200 and .asr.provider_max_chunk_ms == 200 and
    .asr.strategy == "fixed" and .preparation_policy == "continuous" and
    .slow_context == "canonical" and
    .fast.provider == $provider and .fast.model == $model and
    .fast.effort == "minimal" and .fast.tool_authority == "propose" and
    .slow.provider == "google" and .slow.model == "gemini-3.5-flash" and
    .slow.effort == "high" and .slow.tool_authority == "execute"
  ' <<<"${health}" >/dev/null; then
    echo "gateway does not report the preregistered ${provider}/${model} fast profile" >&2
    return 1
  fi
}

restore_local_fast() {
  if [[ "${restored}" == false ]]; then
    start_fast_profile vllm qwen-fast
    restored=true
  fi
}
cleanup() {
  restore_local_fast || true
}
trap cleanup EXIT

start_fast_profile gemini gemini-3.5-flash

TAU_VOICE_MATRIX="${candidate_matrix}" \
  "${repository_root}/scripts/run-tau-voice-matrix.sh"

"${repository_root}/scripts/report-tau-voice-matrix.sh" \
  "${candidate_matrix}" "" \
  --validate-only \
  --cell i1-gg-control \
  --cell i1-gg-regular
"${repository_root}/scripts/archive-tau-voice-artifacts.sh" "${candidate_matrix}"

restore_local_fast
trap - EXIT
