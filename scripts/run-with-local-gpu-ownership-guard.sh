#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
stem="${1:-}"
if [[ -z "${stem}" || "${2:-}" != -- || $# -lt 3 ]]; then
  echo "usage: scripts/run-with-local-gpu-ownership-guard.sh EVIDENCE_STEM -- COMMAND [ARGUMENTS...]" >&2
  exit 2
fi
shift 2
log="${stem}.jsonl"
summary="${stem}.summary.json"
monitor_pid=""
command_pid=""

for command_name in jq python3 seq setsid; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done

stop_monitor() {
  if [[ -z "${monitor_pid}" ]]; then
    return 0
  fi
  if kill -0 "${monitor_pid}" 2>/dev/null; then
    kill -TERM "${monitor_pid}" 2>/dev/null || true
  fi
  local status=0
  wait "${monitor_pid}" || status=$?
  monitor_pid=""
  return "${status}"
}
terminate_command() {
  if [[ -z "${command_pid}" ]]; then
    return 0
  fi
  if kill -0 "${command_pid}" 2>/dev/null; then
    kill -TERM -- "-${command_pid}" 2>/dev/null || \
      kill -TERM "${command_pid}" 2>/dev/null || true
    for _ in $(seq 1 50); do
      if ! kill -0 "${command_pid}" 2>/dev/null; then
        break
      fi
      sleep 0.1
    done
    if kill -0 "${command_pid}" 2>/dev/null; then
      kill -KILL -- "-${command_pid}" 2>/dev/null || \
        kill -KILL "${command_pid}" 2>/dev/null || true
    fi
  fi
  wait "${command_pid}" 2>/dev/null || true
  command_pid=""
}
cleanup() {
  terminate_command || true
  stop_monitor || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# `jq -e FILTER FILE` exits 0 when FILE is EMPTY: the filter never runs, so jq
# reports "no output produced" rather than false. The guard log is created empty
# and appended to, so the readiness check used to pass on the very first poll --
# declaring exclusive GPU ownership verified before the monitor had written a
# single check. Slurp the file and require a real ok record.
guard_check_ok() {
  jq -en --slurpfile records "$1" \
    '($records | length) > 0 and
     ([$records[] | select(.type == "guard.check" and .status == "ok")] | length) > 0' \
    >/dev/null 2>&1
}

"${repository_root}/scripts/monitor-local-gpu-ownership.sh" "${log}" &
monitor_pid=$!
for _ in $(seq 1 100); do
  if [[ -f "${log}" ]] && guard_check_ok "${log}"; then
    break
  fi
  if ! kill -0 "${monitor_pid}" 2>/dev/null; then
    wait "${monitor_pid}" || true
    echo "GPU ownership guard failed before command launch: ${log}" >&2
    exit 1
  fi
  sleep 0.1
done
if ! guard_check_ok "${log}"; then
  echo "GPU ownership guard did not produce its initial check" >&2
  exit 1
fi

setsid --wait "$@" &
command_pid=$!
finished_pid=""
finished_status=0
if wait -n -p finished_pid "${command_pid}" "${monitor_pid}"; then
  finished_status=0
else
  finished_status=$?
fi
if [[ "${finished_pid}" == "${monitor_pid}" ]]; then
  monitor_pid=""
  terminate_command
  echo "GPU ownership guard ended during the measured command: ${log}" >&2
  exit 1
fi
if [[ "${finished_pid}" != "${command_pid}" ]]; then
  echo "ownership supervisor reaped an unknown process" >&2
  exit 1
fi
command_status="${finished_status}"
command_pid=""
guard_status=0
stop_monitor || guard_status=$?
if (( guard_status != 0 )); then
  echo "GPU ownership guard recorded a violation: ${log}" >&2
  exit 1
fi
python3 "${repository_root}/scripts/summarize-local-gpu-ownership.py" \
  --log "${log}" \
  --repository-root "${repository_root}" \
  --output "${summary}"
trap - EXIT
if (( command_status != 0 )); then
  exit "${command_status}"
fi
