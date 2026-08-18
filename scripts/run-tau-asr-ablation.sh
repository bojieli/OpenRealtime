#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
baseline_data="${repository_root}/.runtime/tau2-bench/data/simulations"
baseline_matrix="${repository_root}/benchmarks/tau-voice/matrix-v1.json"
candidate_matrix="${repository_root}/benchmarks/tau-voice/matrix-asr17-v1.json"
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
  --cell i1-qg-control \
  --cell i1-qg-regular

for cell in i1-qg-control i1-qg-regular; do
  for domain_tasks in airline:50 retail:114 telecom:114; do
    domain="${domain_tasks%%:*}"
    expected="${domain_tasks##*:}"
    simulation_dir="${baseline_data}/openrealtime-tau-voice-matrix-v1-${cell}-${domain}-seed300/simulations"
    actual=0
    if [[ -d "${simulation_dir}" ]]; then
      actual="$(find "${simulation_dir}" -maxdepth 1 -type f -name '*.json' | wc -l)"
    fi
    if [[ "${actual}" != "${expected}" ]]; then
      echo "paired baseline ${cell}/${domain} is incomplete: ${actual}/${expected}" >&2
      exit 1
    fi
  done
done

restore_baseline_asr() {
  if [[ "${restored}" == false ]]; then
    "${repository_root}/scripts/local-cascade.sh" stop || true
    OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-0.6B \
    OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.14 \
    OPENREALTIME_ASR_CHUNK_SECONDS=0.2 \
    OPENREALTIME_ASR_PROVIDER_CHUNK=200ms \
      "${repository_root}/scripts/local-cascade.sh" start || true
    restored=true
  fi
}
trap restore_baseline_asr EXIT

"${repository_root}/scripts/local-cascade.sh" stop
OPENREALTIME_ASR_MODEL=Qwen/Qwen3-ASR-1.7B \
OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION=0.18 \
OPENREALTIME_ASR_CHUNK_SECONDS=0.2 \
OPENREALTIME_ASR_PROVIDER_CHUNK=200ms \
  "${repository_root}/scripts/local-cascade.sh" start

if ! tr '\0' ' ' <"/proc/$(<"${repository_root}/.runtime/local-cascade/pids/asr.pid")/cmdline" | grep -F 'Qwen/Qwen3-ASR-1.7B' >/dev/null; then
  echo "ASR process does not identify the preregistered 1.7B candidate" >&2
  exit 1
fi
gateway_health="$(curl --fail --silent --show-error http://127.0.0.1:8765/healthz)"
if ! jq -e '.asr.model == "Qwen/Qwen3-ASR-1.7B" and .asr.provider_chunk_ms == 200' \
  <<<"${gateway_health}" >/dev/null; then
  echo "gateway does not report the preregistered 1.7B ASR profile" >&2
  exit 1
fi

TAU_VOICE_MATRIX="${candidate_matrix}" \
  "${repository_root}/scripts/run-tau-voice-matrix.sh"

"${repository_root}/scripts/report-tau-voice-matrix.sh" \
  "${candidate_matrix}" "" \
  --validate-only \
  --cell i1-qg-asr17-control \
  --cell i1-qg-asr17-regular
