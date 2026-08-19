#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
output="${1:-}"
runtime_root="${2:-${OPENREALTIME_LOCAL_CASCADE_RUNTIME_ROOT:-${repository_root}/.runtime/local-cascade}}"
interval_seconds="${OPENREALTIME_GPU_OWNERSHIP_INTERVAL_SECONDS:-5}"
capture_timeout_seconds="${OPENREALTIME_GPU_OWNERSHIP_CAPTURE_TIMEOUT_SECONDS:-15}"

for command_name in jq timeout; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done

if [[ -z "${output}" || $# -gt 2 ]]; then
  echo "usage: scripts/monitor-local-gpu-ownership.sh OUTPUT_JSONL [RUNTIME_ROOT]" >&2
  exit 2
fi
if [[ ! "${interval_seconds}" =~ ^[1-9][0-9]*$ ]]; then
  echo "OPENREALTIME_GPU_OWNERSHIP_INTERVAL_SECONDS must be a positive integer" >&2
  exit 2
fi
if [[ ! "${capture_timeout_seconds}" =~ ^[1-9][0-9]*$ ]]; then
  echo "OPENREALTIME_GPU_OWNERSHIP_CAPTURE_TIMEOUT_SECONDS must be a positive integer" >&2
  exit 2
fi
if [[ -e "${output}" || -e "${output}.active" ]]; then
  echo "GPU ownership evidence already exists: ${output}" >&2
  exit 1
fi

mkdir -p "$(dirname "${output}")"
printf '%s\n' "$$" >"${output}.active"
error_file="${output}.error.$$"
finished=false
finish() {
  local status=$?
  if [[ "${finished}" == false && -f "${output}.active" ]]; then
    jq -nc \
      --arg recorded_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --argjson exit_status "${status}" \
      '{type:"guard.completed",status:(if $exit_status == 0 then "complete" else "failed" end),recorded_at:$recorded_at,exit_status:$exit_status}' \
      >>"${output}"
    rm -f "${output}.active"
  fi
  rm -f "${error_file}"
}
trap finish EXIT
trap 'exit 0' TERM INT

jq -nc \
  --arg recorded_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson interval_seconds "${interval_seconds}" \
  --argjson capture_timeout_seconds "${capture_timeout_seconds}" \
  '{type:"guard.started",status:"running",recorded_at:$recorded_at,interval_seconds:$interval_seconds,capture_timeout_seconds:$capture_timeout_seconds}' \
  >"${output}"

while true; do
  if snapshot="$(timeout --signal=TERM "${capture_timeout_seconds}" \
    "${repository_root}/scripts/capture-local-gpu-ownership.sh" \
    "${runtime_root}" 2>"${error_file}")"; then
    jq -nc \
      --arg recorded_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --argjson snapshot "${snapshot}" \
      '{type:"guard.check",status:"ok",recorded_at:$recorded_at,ownership:$snapshot}' \
      >>"${output}"
  else
    message="$(<"${error_file}")"
    if [[ -z "${message}" ]]; then
      message="GPU ownership capture failed or timed out"
    fi
    jq -nc \
      --arg recorded_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --arg message "${message}" \
      '{type:"guard.check",status:"violation",recorded_at:$recorded_at,error:$message}' \
      >>"${output}"
    exit 1
  fi
  sleep "${interval_seconds}"
done
