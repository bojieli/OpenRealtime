#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
primary_pid_file="${repository_root}/.runtime/benchmark-runs/voice-benchmark-queue-v1/queue.pid"
primary_log="${repository_root}/.runtime/benchmark-runs/voice-benchmark-queue-v1/queue.log"
optimization_root="${repository_root}/.runtime/benchmark-runs/voice-optimization-queue-v1"

: "${OPENAI_API_KEY:?OPENAI_API_KEY must be set for the tau user simulator}"
: "${GEMINI_API_KEY:?GEMINI_API_KEY must be set for slow reasoning}"
: "${OPENREALTIME_GATEWAY_TOKEN:?OPENREALTIME_GATEWAY_TOKEN must be set}"
mkdir -p "${optimization_root}"

if [[ ! -f "${primary_pid_file}" ]]; then
  echo "primary benchmark queue PID file is missing" >&2
  exit 1
fi
primary_pid="$(<"${primary_pid_file}")"
if [[ ! "${primary_pid}" =~ ^[1-9][0-9]*$ ]]; then
  echo "primary benchmark queue PID is invalid" >&2
  exit 1
fi
if kill -0 "${primary_pid}" 2>/dev/null; then
  echo "waiting for primary voice benchmark queue ${primary_pid}"
  tail --pid="${primary_pid}" -f /dev/null
fi
if ! grep -Fx 'voice benchmark queue complete' "${primary_log}" >/dev/null; then
  echo "primary voice benchmark queue did not complete; optimization queue stopped" >&2
  exit 1
fi

echo "starting complete paired Qwen3-ASR-1.7B tau-Voice ablation"
"${repository_root}/scripts/with-study-runtime.sh" \
  "${repository_root}/scripts/run-tau-asr-ablation.sh" \
  2>&1 | tee "${optimization_root}/tau-asr17.log"
echo "voice optimization queue complete"
