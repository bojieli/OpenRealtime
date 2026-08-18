#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
extended_pid_file="${repository_root}/.runtime/benchmark-runs/tau-extended-ablation-queue-v1/queue.pid"
extended_log="${repository_root}/.runtime/benchmark-runs/tau-extended-ablation-queue-v1/queue.log"
queue_root="${repository_root}/.runtime/benchmark-runs/tau-cognitive-control-queue-v1"

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done
mkdir -p "${queue_root}"
if [[ ! -f "${extended_pid_file}" ]]; then
  echo "tau extended-ablation queue PID file is missing" >&2
  exit 1
fi
extended_pid="$(<"${extended_pid_file}")"
if [[ ! "${extended_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "tau extended-ablation queue PID is invalid" >&2
  exit 1
fi
if kill -0 "${extended_pid}" 2>/dev/null; then
  echo "waiting for tau extended-ablation queue ${extended_pid}"
  tail --pid="${extended_pid}" -f /dev/null
fi
if ! grep -Fx 'tau extended ablation queue complete' "${extended_log}" >/dev/null; then
  echo "tau extended-ablation queue did not complete; cognitive controls stopped" >&2
  exit 1
fi

echo "starting complete context-projection tau-Voice controls"
"${repository_root}/scripts/with-study-runtime.sh" \
  "${repository_root}/scripts/run-tau-context-projection-ablation.sh" \
  2>&1 | tee "${queue_root}/context-projection.log"
echo "tau cognitive-control queue complete"
