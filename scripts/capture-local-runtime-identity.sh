#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime_root="${1:-${repository_root}/.runtime/local-cascade}"
pid_root="${runtime_root}/pids"
captured_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
boot_id="$(< /proc/sys/kernel/random/boot_id)"

process_start_ticks() {
  local pid="$1"
  local stat_line stat_tail
  stat_line="$(<"/proc/${pid}/stat")"
  stat_tail="${stat_line##*) }"
  awk '{print $20}' <<<"${stat_tail}"
}

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
  start_time_ticks="$(process_start_ticks "${pid}")"
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

if jq -e '.components.qwen != null' <<<"${identity}" >/dev/null; then
  qwen_version="$("${repository_root}/scripts/fetch-json-endpoint.sh" \
    http://127.0.0.1:8000/version "qwen /version" 3)"
  qwen_models="$("${repository_root}/scripts/fetch-json-endpoint.sh" \
    http://127.0.0.1:8000/v1/models "qwen /v1/models" 3)"
  if ! jq -e '.version | type == "string" and length > 0' <<<"${qwen_version}" >/dev/null; then
    echo "Qwen vLLM version endpoint is invalid" >&2
    exit 1
  fi
  if ! jq -e '
    .data | length == 1 and
    .[0].id == "qwen-fast" and
    .[0].root == "Qwen/Qwen3-30B-A3B-FP8" and
    (.[0].max_model_len | type == "number")
  ' <<<"${qwen_models}" >/dev/null; then
    echo "Qwen vLLM model endpoint is invalid" >&2
    exit 1
  fi
  identity="$(jq \
    --arg version "$(jq -r '.version' <<<"${qwen_version}")" \
    --arg served_model "$(jq -r '.data[0].id' <<<"${qwen_models}")" \
    --arg model "$(jq -r '.data[0].root' <<<"${qwen_models}")" \
    --argjson max_model_len "$(jq '.data[0].max_model_len' <<<"${qwen_models}")" \
    '.components.qwen.service={
      implementation:"vllm",
      version:$version,
      served_model:$served_model,
      model:$model,
      max_model_len:$max_model_len
    }' <<<"${identity}")"
fi

gpu_ownership="$("${repository_root}/scripts/capture-local-gpu-ownership.sh" "${runtime_root}")"
if ! jq -e \
  --argjson identity "${identity}" '
  .host_boot_id == $identity.host_boot_id and
  all(.component_roots | to_entries[];
    $identity.components[.key].pid == .value.pid and
    $identity.components[.key].proc_start_time_ticks == .value.proc_start_time_ticks)
' <<<"${gpu_ownership}" >/dev/null; then
  echo "GPU ownership does not match the captured local component identity" >&2
  exit 1
fi
identity="$(jq --argjson gpu_ownership "${gpu_ownership}" \
  '.gpu_ownership=$gpu_ownership' <<<"${identity}")"

jq . <<<"${identity}"
