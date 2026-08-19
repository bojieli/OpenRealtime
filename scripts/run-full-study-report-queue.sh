#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
endpoint_pid_file="${repository_root}/.runtime/benchmark-runs/tau-endpoint-preparation-queue-v1/queue.pid"
endpoint_log="${repository_root}/.runtime/benchmark-runs/tau-endpoint-preparation-queue-v1/queue.log"
queue_root="${repository_root}/.runtime/benchmark-runs/full-study-report-queue-v1"

mkdir -p "${queue_root}"
if [[ ! -f "${endpoint_pid_file}" ]]; then
  echo "tau endpoint-preparation queue PID file is missing" >&2
  exit 1
fi
endpoint_pid="$(<"${endpoint_pid_file}")"
if [[ ! "${endpoint_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "tau endpoint-preparation queue PID is invalid" >&2
  exit 1
fi
if kill -0 "${endpoint_pid}" 2>/dev/null; then
  echo "waiting for tau endpoint-preparation queue ${endpoint_pid}"
  tail --pid="${endpoint_pid}" -f /dev/null
fi
if ! grep -Fx 'tau endpoint-preparation queue complete' "${endpoint_log}" >/dev/null; then
  echo "tau endpoint-preparation queue did not complete; full-study report stopped" >&2
  exit 1
fi

python3 "${repository_root}/scripts/report-full-study.py" \
  --repository-root "${repository_root}" \
  --manifest "${repository_root}/benchmarks/full-study-v1.json" \
  --output "${repository_root}/.runtime/benchmark-runs/full-study-v1/report.json"
echo "full study report queue complete"
