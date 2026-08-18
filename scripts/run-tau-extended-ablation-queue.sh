#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
report_pid_file="${repository_root}/.runtime/benchmark-runs/tau-report-queue-v1/queue.pid"
report_log="${repository_root}/.runtime/benchmark-runs/tau-report-queue-v1/queue.log"
queue_root="${repository_root}/.runtime/benchmark-runs/tau-extended-ablation-queue-v1"

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done
mkdir -p "${queue_root}"
if [[ ! -f "${report_pid_file}" ]]; then
  echo "tau reporting queue PID file is missing" >&2
  exit 1
fi
report_pid="$(<"${report_pid_file}")"
if [[ ! "${report_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "tau reporting queue PID is invalid" >&2
  exit 1
fi
if kill -0 "${report_pid}" 2>/dev/null; then
  echo "waiting for tau reporting queue ${report_pid}"
  tail --pid="${report_pid}" -f /dev/null
fi
if ! grep -Fx 'tau reporting queue complete' "${report_log}" >/dev/null; then
  echo "tau reporting queue did not complete; extended ablations stopped" >&2
  exit 1
fi

echo "starting complete fixed-cadence tau-Voice ablation"
"${repository_root}/scripts/with-study-runtime.sh" \
  "${repository_root}/scripts/run-tau-cadence-ablation.sh" \
  2>&1 | tee "${queue_root}/cadence.log"

echo "starting complete slow-effort tau-Voice ablation"
"${repository_root}/scripts/with-study-runtime.sh" \
  "${repository_root}/scripts/run-tau-slow-effort-ablation.sh" \
  2>&1 | tee "${queue_root}/slow-effort.log"

echo "tau extended ablation queue complete"
