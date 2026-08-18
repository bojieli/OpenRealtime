#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
fast_pid_file="${repository_root}/.runtime/benchmark-runs/fast-provider-queue-v1/queue.pid"
fast_log="${repository_root}/.runtime/benchmark-runs/fast-provider-queue-v1/queue.log"
queue_root="${repository_root}/.runtime/benchmark-runs/tau-report-queue-v1"

mkdir -p "${queue_root}"
if [[ ! -f "${fast_pid_file}" ]]; then
  echo "fast-provider queue PID file is missing" >&2
  exit 1
fi
fast_pid="$(<"${fast_pid_file}")"
if [[ ! "${fast_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "fast-provider queue PID is invalid" >&2
  exit 1
fi
if kill -0 "${fast_pid}" 2>/dev/null; then
  echo "waiting for fast-provider queue ${fast_pid}"
  tail --pid="${fast_pid}" -f /dev/null
fi
if ! grep -Fx 'fast-provider queue complete' "${fast_log}" >/dev/null; then
  echo "fast-provider queue did not complete; reporting queue stopped" >&2
  exit 1
fi

for matrix_name in matrix-canonical-v1 matrix-asr17-v1 matrix-fast-gemini-v1; do
  echo "reporting complete ${matrix_name}"
  "${repository_root}/scripts/report-tau-voice-matrix.sh" \
    "${repository_root}/benchmarks/tau-voice/${matrix_name}.json" \
    2>&1 | tee "${queue_root}/${matrix_name}.log"
done
echo "tau reporting queue complete"
