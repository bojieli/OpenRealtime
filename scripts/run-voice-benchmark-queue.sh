#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
control_pid_file="${repository_root}/.runtime/benchmark-runs/tau-voice/openrealtime-tau-voice-matrix-v1/control.pid"
queue_root="${repository_root}/.runtime/benchmark-runs/voice-benchmark-queue-v1"

: "${OPENAI_API_KEY:?OPENAI_API_KEY must be set for the tau user simulator}"
: "${GEMINI_API_KEY:?GEMINI_API_KEY must be set for slow reasoning}"
: "${OPENREALTIME_GATEWAY_TOKEN:?OPENREALTIME_GATEWAY_TOKEN must be set}"
mkdir -p "${queue_root}"

if [[ -f "${control_pid_file}" ]]; then
  control_pid="$(<"${control_pid_file}")"
  if [[ "${control_pid}" =~ ^[1-9][0-9]*$ ]] && kill -0 "${control_pid}" 2>/dev/null; then
    echo "waiting for tau control process ${control_pid}"
    tail --pid="${control_pid}" -f /dev/null
  fi
fi

tau_data="${repository_root}/.runtime/tau2-bench/data/simulations"
for domain_tasks in airline:50 retail:114 telecom:114; do
  domain="${domain_tasks%%:*}"
  expected="${domain_tasks##*:}"
  simulation_dir="${tau_data}/openrealtime-tau-voice-matrix-v1-i1-qg-control-${domain}-seed300/simulations"
  actual=0
  if [[ -d "${simulation_dir}" ]]; then
    actual="$(find "${simulation_dir}" -maxdepth 1 -type f -name '*.json' | wc -l)"
  fi
  if [[ "${actual}" != "${expected}" ]]; then
    echo "tau control is incomplete for ${domain}: ${actual}/${expected}; queue stopped" >&2
    exit 1
  fi
done

echo "starting complete tau regular cell"
"${repository_root}/scripts/run-tau-voice-matrix.sh" i1-qg-regular \
  2>&1 | tee "${queue_root}/tau-regular.log"

echo "starting complete Full-Duplex-Bench v1.5 overlap population"
OPENREALTIME_API_KEY="${OPENREALTIME_GATEWAY_TOKEN}" \
  "${repository_root}/scripts/run-fdb15-openrealtime.sh" \
  2>&1 | tee "${queue_root}/fdb15-openrealtime.log"

echo "starting complete Full-Duplex-Bench v3 tool-use population"
OPENREALTIME_API_KEY="${OPENREALTIME_GATEWAY_TOKEN}" \
  FDBV3_USE_LLM_JUDGE="${FDBV3_USE_LLM_JUDGE:-true}" \
  "${repository_root}/scripts/run-fdb-v3-openrealtime.sh" \
  2>&1 | tee "${queue_root}/fdb-v3-openrealtime.log"

echo "voice benchmark queue complete"
