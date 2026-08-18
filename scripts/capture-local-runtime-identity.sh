#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime_root="${1:-${repository_root}/.runtime/local-cascade}"
pid_root="${runtime_root}/pids"
captured_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
boot_id="$(< /proc/sys/kernel/random/boot_id)"

identity="$(jq -n \
  --arg captured_at "${captured_at}" \
  --arg boot_id "${boot_id}" \
  '{schema_version:"1.0.0",captured_at:$captured_at,host_boot_id:$boot_id,components:{}}')"

for component in gateway asr fish qwen; do
  pid_file="${pid_root}/${component}.pid"
  if [[ ! -f "${pid_file}" ]]; then
    if [[ "${component}" == qwen ]]; then
      continue
    fi
    echo "required ${component} PID file is missing: ${pid_file}" >&2
    exit 1
  fi
  pid="$(<"${pid_file}")"
  if [[ ! "${pid}" =~ ^[1-9][0-9]*$ ]] || ! kill -0 "${pid}" 2>/dev/null; then
    echo "required ${component} process is not alive: ${pid}" >&2
    exit 1
  fi
  executable="$(readlink -f "/proc/${pid}/exe")"
  executable_sha256="$(sha256sum "/proc/${pid}/exe" | cut -d ' ' -f 1)"
  command_sha256="$(sha256sum "/proc/${pid}/cmdline" | cut -d ' ' -f 1)"
  start_time_ticks="$(cut -d ' ' -f 22 "/proc/${pid}/stat")"
  argv="$(jq -Rs 'split("\u0000") | map(select(length > 0))' "/proc/${pid}/cmdline")"
  identity="$(jq \
    --arg component "${component}" \
    --argjson pid "${pid}" \
    --arg start_time_ticks "${start_time_ticks}" \
    --arg executable "${executable}" \
    --arg executable_sha256 "${executable_sha256}" \
    --arg command_sha256 "${command_sha256}" \
    --argjson argv "${argv}" \
    '.components[$component]={
      pid:$pid,
      proc_start_time_ticks:$start_time_ticks,
      executable:$executable,
      executable_sha256:$executable_sha256,
      command_sha256:$command_sha256,
      argv:$argv
    }' <<<"${identity}")"
done

jq . <<<"${identity}"
