#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
matrix="${TAU_VOICE_MATRIX:-${repository_root}/benchmarks/tau-voice/matrix-v1.json}"
tau2_directory="${TAU2_DIR:-${repository_root}/.runtime/tau2-bench}"
selected_cell="${1:-}"
matrix_root="${repository_root}/.runtime/benchmark-runs/tau-voice/$(jq -r '.matrix_id' "${matrix}")"
invocation_id="${selected_cell:-all-cells}"
run_root="${matrix_root}/invocations/${invocation_id}"
run_status="initializing"

if [[ $# -gt 1 ]]; then
  echo "usage: scripts/run-tau-voice-matrix.sh [cell-id]" >&2
  exit 2
fi

for required in OPENAI_API_KEY GEMINI_API_KEY OPENREALTIME_GATEWAY_TOKEN; do
  if [[ -z "${!required:-}" ]]; then
    echo "required environment variable ${required} is unset" >&2
    exit 1
  fi
done

for command_name in curl git jq nvidia-smi sha256sum uv; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done

"${repository_root}/scripts/prepare-tau-voice.sh"

gateway_health="$(curl --fail --silent --show-error http://127.0.0.1:8765/healthz)" || {
  echo "OpenRealtime gateway is not healthy" >&2
  exit 1
}
if ! curl --fail --silent --show-error http://127.0.0.1:8081/health >/dev/null; then
  echo "Fish Audio is not healthy" >&2
  exit 1
fi
requires_local_fast="$(jq -r '.runtime_requirements.requires_local_fast // true' "${matrix}")"
if [[ "${requires_local_fast}" == true ]]; then
  if ! curl --fail --silent --show-error http://127.0.0.1:8000/health >/dev/null; then
    echo "Qwen vLLM is not healthy" >&2
    exit 1
  fi
fi
if ! curl --fail --silent --show-error http://127.0.0.1:8001/ >/dev/null; then
  echo "Qwen3-ASR is not healthy" >&2
  exit 1
fi
for phase in fast slow; do
  expected_provider="$(jq -r --arg phase "${phase}" '.runtime_requirements.gateway_profiles[$phase].provider // empty' "${matrix}")"
  if [[ -z "${expected_provider}" ]]; then
    continue
  fi
  expected_model="$(jq -r --arg phase "${phase}" '.runtime_requirements.gateway_profiles[$phase].model' "${matrix}")"
  expected_effort="$(jq -r --arg phase "${phase}" '.runtime_requirements.gateway_profiles[$phase].effort' "${matrix}")"
  expected_authority="$(jq -r --arg phase "${phase}" '.runtime_requirements.gateway_profiles[$phase].tool_authority' "${matrix}")"
  if ! jq -e \
    --arg phase "${phase}" \
    --arg provider "${expected_provider}" \
    --arg model "${expected_model}" \
    --arg effort "${expected_effort}" \
    --arg authority "${expected_authority}" \
    '.[$phase].provider == $provider and .[$phase].model == $model and .[$phase].effort == $effort and .[$phase].tool_authority == $authority' \
    <<<"${gateway_health}" >/dev/null; then
    echo "gateway ${phase} profile does not match the preregistered matrix" >&2
    exit 1
  fi
done

mkdir -p "${run_root}"
matrix_sha256="$(sha256sum "${matrix}" | cut -d ' ' -f 1)"
patch_sha256="$(sha256sum "${repository_root}/benchmarks/tau-voice/patches/0001-local-openai-fish-audio.patch" | cut -d ' ' -f 1)"
jq -n \
  --arg started_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg matrix "${matrix}" \
  --arg matrix_sha256 "${matrix_sha256}" \
  --arg patch_sha256 "${patch_sha256}" \
  --arg tau_revision "$(git -C "${tau2_directory}" rev-parse HEAD)" \
  --arg openrealtime_revision "$(git -C "${repository_root}" rev-parse HEAD)" \
  --arg selected_cell "${selected_cell}" \
  '{schema_version:"1.0.0",started_at:$started_at,matrix:$matrix,matrix_sha256:$matrix_sha256,patch_sha256:$patch_sha256,tau_revision:$tau_revision,openrealtime_revision:$openrealtime_revision,selected_cell:(if $selected_cell == "" then null else $selected_cell end),status:"running"}' \
  >"${run_root}/run.json"

telemetry="${run_root}/gpu.csv"
echo 'timestamp,index,name,memory_used_mib,utilization_gpu_percent,power_draw_watts,loadavg' >"${telemetry}"
(
  while true; do
    timestamp="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    loadavg="$(cut -d ' ' -f 1-3 /proc/loadavg | tr ' ' '/')"
    while IFS= read -r gpu_row; do
      printf '%s,%s,%s\n' "${timestamp}" "${gpu_row}" "${loadavg}" >>"${telemetry}"
    done < <(nvidia-smi --query-gpu=index,name,memory.used,utilization.gpu,power.draw \
      --format=csv,noheader,nounits) || true
    sleep 5
  done
) &
telemetry_pid=$!
cleanup() {
  kill "${telemetry_pid}" 2>/dev/null || true
  if [[ -f "${run_root}/run.json" && "${run_status}" != "complete" ]]; then
    jq --arg status "${run_status}" --arg stopped_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      '.status=$status | .stopped_at=$stopped_at' \
      "${run_root}/run.json" >"${run_root}/run.json.next" || return
    mv "${run_root}/run.json.next" "${run_root}/run.json"
  fi
}
trap cleanup EXIT
trap 'run_status="interrupted"; exit 130' INT
trap 'run_status="interrupted"; exit 143' TERM

mapfile -t cells < <(jq -r '.cells[].id' "${matrix}")
if [[ -n "${selected_cell}" ]] && ! printf '%s\n' "${cells[@]}" | grep -Fx "${selected_cell}" >/dev/null; then
  echo "unknown matrix cell: ${selected_cell}" >&2
  exit 2
fi
run_status="running"
for cell in "${cells[@]}"; do
  if [[ -n "${selected_cell}" && "${cell}" != "${selected_cell}" ]]; then
    continue
  fi
  speech_complexity="$(jq -r --arg cell "${cell}" '.cells[] | select(.id == $cell) | .speech_complexity' "${matrix}")"
  registry_relative="$(jq -r --arg cell "${cell}" '.cells[] | select(.id == $cell) | .voice_registry' "${matrix}")"
  registry="${repository_root}/${registry_relative}"

  voices="$(curl --fail --silent --show-error http://127.0.0.1:8081/v1/audio/voices?names_only=true)"
  while IFS= read -r voice; do
    if ! jq -e --arg voice "${voice}" '.uploaded_voice_names | index($voice) != null' <<<"${voices}" >/dev/null; then
      echo "cell ${cell} requires unregistered Fish voice ${voice}" >&2
      exit 1
    fi
  done < <(jq -r '.[]' "${registry}" | sort -u)

  while IFS=$'\t' read -r domain expected_tasks; do
    actual_split_tasks="$(jq '.base | length' "${tau2_directory}/data/tau2/domains/${domain}/split_tasks.json")"
    if [[ "${actual_split_tasks}" != "${expected_tasks}" ]]; then
      echo "matrix expects ${expected_tasks} ${domain} tasks, upstream split has ${actual_split_tasks}" >&2
      exit 1
    fi
    save_to="$(jq -r '.matrix_id' "${matrix}")-${cell}-${domain}-seed300"
    log_dir="${run_root}/${cell}"
    mkdir -p "${log_dir}"
    log_file="${log_dir}/${domain}.log"
    echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] start ${cell}/${domain} (${expected_tasks} tasks)" | tee -a "${log_file}"

    OPENAI_REALTIME_API_KEY="${OPENREALTIME_GATEWAY_TOKEN}" \
    PYTHONPATH="${tau2_directory}/src" \
    uv --directory "${tau2_directory}" run tau2 run \
      --domain "${domain}" \
      --task-split-name base \
      --num-trials 1 \
      --seed 300 \
      --max-concurrency 1 \
      --workers 0 \
      --max-retries 0 \
      --hallucination-retries 0 \
      --timeout 1500 \
      --max-steps-seconds 1200 \
      --audio-native \
      --audio-native-provider openai \
      --audio-native-model gpt-realtime-1.5 \
      --audio-native-base-url ws://127.0.0.1:8765/v1/realtime \
      --voice-synthesis-provider fish_audio \
      --fish-audio-endpoint http://127.0.0.1:8081/v1/audio/speech \
      --fish-audio-model fishaudio/s2-pro \
      --fish-audio-voice default \
      --fish-audio-voice-registry "${registry}" \
      --tick-duration 0.2 \
      --speech-complexity "${speech_complexity}" \
      --user-llm gpt-4.1-2025-04-14 \
      --save-to "${save_to}" \
      --auto-resume \
      --verbose-logs \
      --llm-log-mode latest \
      2>&1 | tee -a "${log_file}"
    simulation_dir="${tau2_directory}/data/simulations/${save_to}/simulations"
    completed_tasks="$(find "${simulation_dir}" -maxdepth 1 -type f -name '*.json' | wc -l)"
    if [[ "${completed_tasks}" != "${expected_tasks}" ]]; then
      echo "${cell}/${domain} returned successfully but saved ${completed_tasks}/${expected_tasks} simulations" >&2
      exit 1
    fi
    echo "[$(date -u +%Y-%m-%dT%H:%M:%SZ)] complete ${cell}/${domain}" | tee -a "${log_file}"
  done < <(jq -r '.benchmark.domains[] | [.name,(.tasks|tostring)] | @tsv' "${matrix}")
done

jq '.status="complete" | .completed_at=now | .completed_at |= todateiso8601' \
  "${run_root}/run.json" >"${run_root}/run.json.next"
mv "${run_root}/run.json.next" "${run_root}/run.json"
run_status="complete"
echo "tau-Voice matrix complete: ${run_root}"
