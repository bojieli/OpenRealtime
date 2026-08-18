#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cognitive_pid_file="${repository_root}/.runtime/benchmark-runs/tau-cognitive-control-queue-v1/queue.pid"
cognitive_log="${repository_root}/.runtime/benchmark-runs/tau-cognitive-control-queue-v1/queue.log"
queue_root="${repository_root}/.runtime/benchmark-runs/tau-event-adaptive-queue-v1"

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done
mkdir -p "${queue_root}"
if [[ ! -f "${cognitive_pid_file}" ]]; then
  echo "tau cognitive-control queue PID file is missing" >&2
  exit 1
fi
cognitive_pid="$(<"${cognitive_pid_file}")"
if [[ ! "${cognitive_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "tau cognitive-control queue PID is invalid" >&2
  exit 1
fi
if kill -0 "${cognitive_pid}" 2>/dev/null; then
  echo "waiting for tau cognitive-control queue ${cognitive_pid}"
  tail --pid="${cognitive_pid}" -f /dev/null
fi
if ! grep -Fx 'tau cognitive-control queue complete' "${cognitive_log}" >/dev/null; then
  echo "tau cognitive-control queue did not complete; event-adaptive controls stopped" >&2
  exit 1
fi

echo "starting complete revision-event and adaptive tau-Voice controls"
"${repository_root}/scripts/run-tau-event-adaptive-ablation.sh" \
  2>&1 | tee "${queue_root}/event-adaptive.log"
echo "tau event-adaptive queue complete"
