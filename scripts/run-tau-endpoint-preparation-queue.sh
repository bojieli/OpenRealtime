#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
event_pid_file="${repository_root}/.runtime/benchmark-runs/tau-event-adaptive-queue-v1/queue.pid"
event_log="${repository_root}/.runtime/benchmark-runs/tau-event-adaptive-queue-v1/queue.log"
queue_root="${repository_root}/.runtime/benchmark-runs/tau-endpoint-preparation-queue-v1"

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done
mkdir -p "${queue_root}"
if [[ ! -f "${event_pid_file}" ]]; then
  echo "tau event-adaptive queue PID file is missing" >&2
  exit 1
fi
event_pid="$(<"${event_pid_file}")"
if [[ ! "${event_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "tau event-adaptive queue PID is invalid" >&2
  exit 1
fi
if kill -0 "${event_pid}" 2>/dev/null; then
  echo "waiting for tau event-adaptive queue ${event_pid}"
  tail --pid="${event_pid}" -f /dev/null
fi
if ! grep -Fx 'tau event-adaptive queue complete' "${event_log}" >/dev/null; then
  echo "tau event-adaptive queue did not complete; endpoint preparation control stopped" >&2
  exit 1
fi

echo "starting complete endpoint-preparation tau-Voice control"
"${repository_root}/scripts/with-study-runtime.sh" \
  "${repository_root}/scripts/run-tau-endpoint-preparation-ablation.sh" \
  2>&1 | tee "${queue_root}/endpoint-preparation.log"
echo "tau endpoint-preparation queue complete"
