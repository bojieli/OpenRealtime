#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
optimization_pid_file="${repository_root}/.runtime/benchmark-runs/voice-optimization-queue-v1/queue.pid"
optimization_log="${repository_root}/.runtime/benchmark-runs/voice-optimization-queue-v1/queue.log"
queue_root="${repository_root}/.runtime/benchmark-runs/fd-bench-queue-v1"

: "${OPENREALTIME_GATEWAY_TOKEN:?OPENREALTIME_GATEWAY_TOKEN must be set}"
cd "${repository_root}"
mkdir -p "${queue_root}"

if [[ ! -f "${optimization_pid_file}" ]]; then
  echo "voice optimization queue PID file is missing" >&2
  exit 1
fi
optimization_pid="$(<"${optimization_pid_file}")"
if [[ ! "${optimization_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "voice optimization queue PID is invalid" >&2
  exit 1
fi
if kill -0 "${optimization_pid}" 2>/dev/null; then
  echo "waiting for voice optimization queue ${optimization_pid}"
  tail --pid="${optimization_pid}" -f /dev/null
fi
if ! grep -Fx 'voice optimization queue complete' "${optimization_log}" >/dev/null; then
  echo "voice optimization queue did not complete; FD-Bench queue stopped" >&2
  exit 1
fi

echo "starting complete released FD-Bench matrix"
OPENREALTIME_API_KEY="${OPENREALTIME_GATEWAY_TOKEN}" \
  FDBENCH_RUN_TIMING_EVALUATOR=true \
  "${repository_root}/scripts/with-study-runtime.sh" \
  "${repository_root}/scripts/run-fdbench-openrealtime.sh" \
  2>&1 | tee "${queue_root}/fd-bench-openrealtime.log"
echo "FD-Bench queue complete"
