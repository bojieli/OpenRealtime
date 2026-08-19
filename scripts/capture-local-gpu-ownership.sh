#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime_root="${1:-${repository_root}/.runtime/local-cascade}"
pid_root="${runtime_root}/pids"
proc_root="${OPENREALTIME_PROC_ROOT:-/proc}"
nvidia_smi="${OPENREALTIME_NVIDIA_SMI:-nvidia-smi}"

for command_name in awk jq sed seq; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done
if ! command -v "${nvidia_smi}" >/dev/null; then
  echo "nvidia-smi command is unavailable: ${nvidia_smi}" >&2
  exit 1
fi

process_start_ticks() {
  local pid="$1"
  local stat_line stat_tail
  stat_line="$(<"${proc_root}/${pid}/stat")"
  stat_tail="${stat_line##*) }"
  awk '{print $20}' <<<"${stat_tail}"
}

process_parent_pid() {
  local pid="$1"
  awk '$1 == "PPid:" {print $2; found=1} END {if (!found) exit 1}' \
    "${proc_root}/${pid}/status"
}

roots='{}'
for component in asr fish qwen; do
  pid_file="${pid_root}/${component}.pid"
  if [[ ! -f "${pid_file}" ]]; then
    if [[ "${component}" == qwen ]]; then
      continue
    fi
    echo "required GPU component PID file is missing: ${pid_file}" >&2
    exit 1
  fi
  pid="$(<"${pid_file}")"
  if [[ ! "${pid}" =~ ^[1-9][0-9]*$ || ! -r "${proc_root}/${pid}/stat" ]]; then
    echo "required GPU component ${component} is not alive: ${pid}" >&2
    exit 1
  fi
  start_ticks="$(process_start_ticks "${pid}")"
  if [[ ! "${start_ticks}" =~ ^[0-9]+$ ]]; then
    echo "required GPU component ${component} has invalid process identity" >&2
    exit 1
  fi
  roots="$(jq -c \
    --arg component "${component}" \
    --argjson pid "${pid}" \
    --arg start_ticks "${start_ticks}" \
    '.[$component]={pid:$pid,proc_start_time_ticks:$start_ticks}' \
    <<<"${roots}")"
done

rows="$("${nvidia_smi}" \
  --query-compute-apps=gpu_uuid,pid,used_gpu_memory \
  --format=csv,noheader,nounits)"
if [[ -z "${rows//[[:space:]]/}" ]]; then
  echo "nvidia-smi reported no GPU compute processes" >&2
  exit 1
fi

records='[]'
seen='{}'
while IFS=',' read -r raw_uuid raw_pid raw_memory; do
  gpu_uuid="$(sed 's/^[[:space:]]*//;s/[[:space:]]*$//' <<<"${raw_uuid}")"
  pid="$(sed 's/^[[:space:]]*//;s/[[:space:]]*$//' <<<"${raw_pid}")"
  memory="$(sed 's/^[[:space:]]*//;s/[[:space:]]*$//' <<<"${raw_memory}")"
  if [[ -z "${gpu_uuid}" || ! "${pid}" =~ ^[1-9][0-9]*$ || ! "${memory}" =~ ^[0-9]+$ ]]; then
    echo "nvidia-smi returned an invalid compute-process row" >&2
    exit 1
  fi
  if [[ ! -r "${proc_root}/${pid}/stat" ]]; then
    echo "GPU compute process ${pid} disappeared during ownership capture" >&2
    exit 1
  fi

  component=""
  component_pid=""
  cursor="${pid}"
  for _ in $(seq 1 128); do
    while IFS=$'\t' read -r name root_pid; do
      if [[ "${cursor}" == "${root_pid}" ]]; then
        component="${name}"
        component_pid="${root_pid}"
        break 2
      fi
    done < <(jq -r 'to_entries[] | [.key,(.value.pid|tostring)] | @tsv' <<<"${roots}")
    if [[ "${cursor}" == 1 || ! -r "${proc_root}/${cursor}/status" ]]; then
      break
    fi
    parent="$(process_parent_pid "${cursor}")"
    if [[ ! "${parent}" =~ ^[0-9]+$ || "${parent}" == "${cursor}" ]]; then
      break
    fi
    cursor="${parent}"
  done
  if [[ -z "${component}" ]]; then
    echo "GPU compute process ${pid} is not descended from a registered local component" >&2
    exit 1
  fi

  process_ticks="$(process_start_ticks "${pid}")"
  records="$(jq -c \
    --arg gpu_uuid "${gpu_uuid}" \
    --arg component "${component}" \
    --argjson component_pid "${component_pid}" \
    --argjson pid "${pid}" \
    --arg process_ticks "${process_ticks}" \
    --argjson memory "${memory}" \
    '. + [{gpu_uuid:$gpu_uuid,component:$component,component_pid:$component_pid,pid:$pid,proc_start_time_ticks:$process_ticks,used_memory_mib:$memory}]' \
    <<<"${records}")"
  seen="$(jq -c --arg component "${component}" '.[$component]=true' <<<"${seen}")"
done <<<"${rows}"

expected_components="$(jq -c 'keys | sort' <<<"${roots}")"
observed_components="$(jq -c 'keys | sort' <<<"${seen}")"
if [[ "${observed_components}" != "${expected_components}" ]]; then
  echo "GPU component occupancy mismatch: expected ${expected_components}, observed ${observed_components}" >&2
  exit 1
fi
gpu_uuids="$(jq -c '[.[].gpu_uuid] | unique | sort' <<<"${records}")"
if [[ "$(jq 'length' <<<"${gpu_uuids}")" != 1 ]]; then
  echo "registered local components are not co-located on exactly one GPU" >&2
  exit 1
fi

jq -n \
  --arg captured_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg boot_id "$(<"${proc_root}/sys/kernel/random/boot_id")" \
  --argjson expected_components "${expected_components}" \
  --argjson component_roots "${roots}" \
  --argjson gpu_uuids "${gpu_uuids}" \
  --argjson processes "$(jq -c 'sort_by(.gpu_uuid,.pid)' <<<"${records}")" \
  '{
    schema_version:"1.0.0",
    captured_at:$captured_at,
    host_boot_id:$boot_id,
    exclusive:true,
    expected_components:$expected_components,
    component_roots:$component_roots,
    gpu_uuids:$gpu_uuids,
    processes:$processes
  }'
