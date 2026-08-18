#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
fd_pid_file="${repository_root}/.runtime/benchmark-runs/fd-bench-queue-v1/queue.pid"
fd_log="${repository_root}/.runtime/benchmark-runs/fd-bench-queue-v1/queue.log"
queue_root="${repository_root}/.runtime/benchmark-runs/fast-provider-queue-v1"

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done
mkdir -p "${queue_root}"

if [[ ! -f "${fd_pid_file}" ]]; then
  echo "FD-Bench queue PID file is missing" >&2
  exit 1
fi
fd_pid="$(<"${fd_pid_file}")"
if [[ ! "${fd_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "FD-Bench queue PID is invalid" >&2
  exit 1
fi
if kill -0 "${fd_pid}" 2>/dev/null; then
  echo "waiting for FD-Bench queue ${fd_pid}"
  tail --pid="${fd_pid}" -f /dev/null
fi
if ! grep -Fx 'FD-Bench queue complete' "${fd_log}" >/dev/null; then
  echo "FD-Bench queue did not complete; fast-provider queue stopped" >&2
  exit 1
fi

echo "starting complete paired Gemini-fast tau-Voice ablation"
"${repository_root}/scripts/with-study-runtime.sh" \
  "${repository_root}/scripts/run-tau-fast-ablation.sh" \
  2>&1 | tee "${queue_root}/tau-gemini-fast.log"
echo "fast-provider queue complete"
