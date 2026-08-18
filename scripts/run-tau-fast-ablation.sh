#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
baseline_data="${repository_root}/.runtime/tau2-bench/data/simulations"
candidate_matrix="${repository_root}/benchmarks/tau-voice/matrix-fast-gemini-v1.json"
restored=false

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done

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
      echo "paired Qwen-fast baseline ${cell}/${domain} is incomplete: ${actual}/${expected}" >&2
      exit 1
    fi
  done
done

restore_local_fast() {
  if [[ "${restored}" == false ]]; then
    "${repository_root}/scripts/local-cascade.sh" stop
    OPENREALTIME_FAST_PROVIDER=vllm \
      "${repository_root}/scripts/local-cascade.sh" start
    restored=true
  fi
}
cleanup() {
  restore_local_fast || true
}
trap cleanup EXIT

"${repository_root}/scripts/local-cascade.sh" stop
OPENREALTIME_FAST_PROVIDER=gemini \
OPENREALTIME_FAST_MODEL=gemini-3.5-flash \
  "${repository_root}/scripts/local-cascade.sh" start

TAU_VOICE_MATRIX="${candidate_matrix}" \
  "${repository_root}/scripts/run-tau-voice-matrix.sh"

restore_local_fast
trap - EXIT
