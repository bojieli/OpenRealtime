#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
receipt_root="${repository_root}/.runtime/benchmark-runs/full-study-queue-launches-v1"
check_only=false

if [[ "${1:-}" == "--check" ]]; then
  check_only=true
elif [[ $# -ne 0 ]]; then
  echo "usage: scripts/launch-full-study-queues.sh [--check]" >&2
  exit 2
fi

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done
for command_name in bash curl git jq nvidia-smi nohup python3 readlink setsid sha256sum stat timeout; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done
if [[ -n "$(git -C "${repository_root}" status --porcelain=v1 --untracked-files=normal)" ]]; then
  echo "full-study queue launch requires a clean OpenRealtime source tree" >&2
  exit 1
fi
"${repository_root}/scripts/test_matrix_requires_local_fast.sh" >/dev/null
"${repository_root}/scripts/test_matrix_requirement.sh" >/dev/null
"${repository_root}/scripts/test_capture_local_gpu_ownership.sh" >/dev/null
"${repository_root}/scripts/test_local_gpu_ownership_guard.sh" >/dev/null
"${repository_root}/scripts/test_full_study_freshness.sh" >/dev/null
"${repository_root}/scripts/test_study_interpreter_pinning.sh" >/dev/null
"${repository_root}/scripts/test_study_queue_chain.sh" >/dev/null
"${repository_root}/scripts/check-full-study-freshness.sh" \
  "${repository_root}/benchmarks/full-study-v1.json" "${repository_root}" >/dev/null

runtime_manifest="${repository_root}/benchmarks/runtime/canonical-gateway-v1.json"
runtime_identity="$("${repository_root}/scripts/capture-local-runtime-identity.sh")"
if ! jq -e \
  --arg version "$(jq -r '.local_fast.version' "${runtime_manifest}")" \
  --arg model "$(jq -r '.local_fast.model' "${runtime_manifest}")" \
  --arg revision "$(jq -r '.local_fast.revision' "${runtime_manifest}")" \
  --arg served_model "$(jq -r '.local_fast.served_model' "${runtime_manifest}")" \
  --arg memory "$(jq -r '.local_fast.gpu_memory_utilization' "${runtime_manifest}")" \
  --arg max_context "$(jq -r '.local_fast.configured_context_tokens' "${runtime_manifest}")" '
  def option_values($name):
    .components.qwen.argv as $argv |
    [$argv | to_entries[] | select(.value == $name) | $argv[.key + 1]];
  def exact_option($name; $value):
    option_values($name) == [$value];
  .components.qwen.service == {
    implementation:"vllm",
    version:$version,
    served_model:$served_model,
    model:$model,
    max_model_len:($max_context | tonumber)
  } and
  exact_option("--model"; $model) and
  exact_option("--revision"; $revision) and
  exact_option("--served-model-name"; $served_model) and
  exact_option("--gpu-memory-utilization"; $memory) and
  exact_option("--max-model-len"; $max_context) and
  exact_option("--tool-call-parser"; "hermes") and
  ([.components.qwen.argv[] | select(. == "--enable-auto-tool-choice")] | length) == 1
' <<<"${runtime_identity}" >/dev/null; then
  echo "local-fast runtime does not match the frozen full-study contract" >&2
  exit 1
fi

queue_ids=(
  primary
  asr
  fd_bench
  fast
  tau_reports
  cadence_and_effort
  context
  event_adaptive
  endpoint_preparation
  publication
)
declare -A queue_scripts=(
  [primary]="scripts/run-voice-benchmark-queue.sh"
  [asr]="scripts/run-voice-optimization-queue.sh"
  [fd_bench]="scripts/run-fdbench-queue.sh"
  [fast]="scripts/run-fast-provider-queue.sh"
  [tau_reports]="scripts/run-tau-report-queue.sh"
  [cadence_and_effort]="scripts/run-tau-extended-ablation-queue.sh"
  [context]="scripts/run-tau-cognitive-control-queue.sh"
  [event_adaptive]="scripts/run-tau-event-adaptive-queue.sh"
  [endpoint_preparation]="scripts/run-tau-endpoint-preparation-queue.sh"
  [publication]="scripts/run-full-study-report-queue.sh"
)
declare -A queue_roots=(
  [primary]=".runtime/benchmark-runs/voice-benchmark-queue-v1"
  [asr]=".runtime/benchmark-runs/voice-optimization-queue-v1"
  [fd_bench]=".runtime/benchmark-runs/fd-bench-queue-v1"
  [fast]=".runtime/benchmark-runs/fast-provider-queue-v1"
  [tau_reports]=".runtime/benchmark-runs/tau-report-queue-v1"
  [cadence_and_effort]=".runtime/benchmark-runs/tau-extended-ablation-queue-v1"
  [context]=".runtime/benchmark-runs/tau-cognitive-control-queue-v1"
  [event_adaptive]=".runtime/benchmark-runs/tau-event-adaptive-queue-v1"
  [endpoint_preparation]=".runtime/benchmark-runs/tau-endpoint-preparation-queue-v1"
  [publication]=".runtime/benchmark-runs/full-study-report-queue-v1"
)

declare -A queue_script_hashes=()
for id in "${queue_ids[@]}"; do
  relative_script="${queue_scripts[$id]}"
  script="${repository_root}/${relative_script}"
  if [[ ! -f "${script}" || ! -r "${script}" ]]; then
    echo "queue ${id} script is missing or unreadable: ${relative_script}" >&2
    exit 1
  fi
  if ! bash -n "${script}"; then
    echo "queue ${id} script has invalid Bash syntax: ${relative_script}" >&2
    exit 1
  fi
  queue_script_hashes[$id]="$(sha256sum "${script}" | cut -d ' ' -f 1)"
done

for id in "${queue_ids[@]}"; do
  pid_file="${repository_root}/${queue_roots[$id]}/queue.pid"
  if [[ ! -f "${pid_file}" ]]; then
    continue
  fi
  pid="$(<"${pid_file}")"
  if [[ "${pid}" =~ ^[1-9][0-9]*$ ]] && kill -0 "${pid}" 2>/dev/null; then
    echo "queue ${id} already has live PID ${pid}; refusing a duplicate launch" >&2
    exit 1
  fi
done

if [[ "${check_only}" == true ]]; then
  echo "full-study queue launch preconditions satisfied"
  exit 0
fi

revision="$(git -C "${repository_root}" rev-parse HEAD)"
launched_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
launch_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
mkdir -p "${receipt_root}"
partial_receipt="${receipt_root}/${launch_id}.launching.json"
receipt="${receipt_root}/${launch_id}.json"
queues='[]'
for id in "${queue_ids[@]}"; do
  relative_script="${queue_scripts[$id]}"
  script="${repository_root}/${relative_script}"
  root="${repository_root}/${queue_roots[$id]}"
  pid_file="${root}/queue.pid"
  log="${root}/queue.log"
  mkdir -p "${root}"
  script_sha256="${queue_script_hashes[$id]}"
  if [[ -f "${log}" ]]; then
    mv "${log}" "${root}/queue.before-${launch_id}.log"
  fi
  printf '[%s] launch %s revision=%s script_sha256=%s\n' \
    "${launched_at}" "${id}" "${revision}" "${script_sha256}" >"${log}"
  nohup setsid bash "${script}" >>"${log}" 2>&1 < /dev/null &
  pid=$!
  printf '%s\n' "${pid}" >"${pid_file}"
  if ! kill -0 "${pid}" 2>/dev/null; then
    echo "queue ${id} failed during launch; see ${log}" >&2
    exit 1
  fi
  queues="$(
    jq -c \
      --arg id "${id}" \
      --arg script "${relative_script}" \
      --arg script_sha256 "${script_sha256}" \
      --arg pid_file "${queue_roots[$id]}/queue.pid" \
      --arg log "${queue_roots[$id]}/queue.log" \
      --argjson pid "${pid}" \
      '. + [{id:$id,script:$script,script_sha256:$script_sha256,pid:$pid,pid_file:$pid_file,log:$log}]' \
      <<<"${queues}"
  )"
  jq -n \
    --arg launched_at "${launched_at}" \
    --arg revision "${revision}" \
    --arg state "launching" \
    --argjson queues "${queues}" \
    '{schema_version:"1.0.0",state:$state,launched_at:$launched_at,openrealtime_revision:$revision,queues:$queues}' \
    >"${partial_receipt}.tmp"
  mv "${partial_receipt}.tmp" "${partial_receipt}"
  echo "launched ${id} queue as PID ${pid}"
done

jq -n \
  --arg launched_at "${launched_at}" \
  --arg revision "${revision}" \
  --arg state "launched" \
  --argjson queues "${queues}" \
  '{schema_version:"1.0.0",state:$state,launched_at:$launched_at,openrealtime_revision:$revision,queues:$queues}' \
  >"${receipt}.tmp"
mv "${receipt}.tmp" "${receipt}"
rm -f "${partial_receipt}"
echo "full-study queue launch receipt: ${receipt}"
