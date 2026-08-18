#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
matrix="${1:-}"
tau2_directory="${TAU2_DIR:-${repository_root}/.runtime/tau2-bench}"
data_root="${tau2_directory}/data/simulations"

if [[ $# -ne 1 ]]; then
  echo "usage: scripts/archive-tau-voice-artifacts.sh MATRIX" >&2
  exit 2
fi
if [[ ! -f "${matrix}" ]]; then
  echo "tau-Voice matrix is missing: ${matrix}" >&2
  exit 1
fi
for command_name in find jq realpath sha256sum tar zstd; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done

matrix_id="$(jq -r '.matrix_id' "${matrix}")"
matrix_sha256="$(sha256sum "${matrix}" | cut -d ' ' -f 1)"
seed="$(jq -r '.benchmark.seed' "${matrix}")"
num_trials="$(jq -r '.benchmark.num_trials' "${matrix}")"
maximum_attempts="$(jq -r '.reporting.infrastructure_retry_policy.maximum_attempts' "${matrix}")"
retry_delay_seconds="$(jq -r '.reporting.infrastructure_retry_policy.retry_delay_seconds' "${matrix}")"
canonical_data_root="$(realpath -m "${data_root}")"

if [[ "${num_trials}" != "1" ]]; then
  echo "attempt-proof archiving currently requires exactly one trial per task" >&2
  exit 1
fi

while IFS=$'\t' read -r cell domain tasks; do
  experiment="${data_root}/${matrix_id}-${cell}-${domain}-seed${seed}"
  canonical_experiment="$(realpath -m "${experiment}")"
  if [[ "${canonical_experiment}" != "${canonical_data_root}/"* ]]; then
    echo "refusing to archive path outside the tau data root: ${experiment}" >&2
    exit 1
  fi
  expected_simulations="$((tasks * num_trials))"
  if [[ ! -f "${experiment}/results.json" || ! -d "${experiment}/simulations" ]]; then
    echo "complete tau experiment is missing: ${experiment}" >&2
    exit 1
  fi
  actual_simulations="$(find "${experiment}/simulations" -maxdepth 1 -type f -name '*.json' | wc -l)"
  if [[ "${actual_simulations}" != "${expected_simulations}" ]]; then
    echo "tau experiment is incomplete: ${cell}/${domain} ${actual_simulations}/${expected_simulations}" >&2
    exit 1
  fi

  artifacts="${experiment}/artifacts"
  archive="${experiment}/raw-artifacts.tar.zst"
  evidence="${experiment}/raw-artifacts-archive.json"
  if [[ -f "${archive}" && -f "${evidence}" ]]; then
    if ! jq -e \
      --arg matrix_id "${matrix_id}" \
      --arg matrix_sha256 "${matrix_sha256}" \
      --arg cell "${cell}" \
      --arg domain "${domain}" \
      --argjson expected_simulations "${expected_simulations}" \
      --argjson maximum_attempts "${maximum_attempts}" \
      --argjson retry_delay_seconds "${retry_delay_seconds}" \
      '.schema_version == "1.0.0" and
       .matrix.id == $matrix_id and .matrix.sha256 == $matrix_sha256 and
       .population.cell == $cell and .population.domain == $domain and
       .source.path == "artifacts" and .source.files > 0 and .source.bytes > 0 and
       .attempts.tasks == $expected_simulations and
       .attempts.successful == .attempts.tasks and
       .attempts.total == (.attempts.successful + .attempts.failed_infrastructure) and
       .attempts.maximum_allowed == $maximum_attempts and
       .attempts.maximum_observed <= .attempts.maximum_allowed and
       .attempts.retry_delay_seconds == $retry_delay_seconds and
       .attempts.seed_reused == true and
       .attempts.retry_scope == "exceptions_only" and
       .attempts.semantic_outcomes_retried == false and
       .archive.path == "raw-artifacts.tar.zst" and
       .archive.format == "deterministic-pax-tar+zstd"' \
      "${evidence}" >/dev/null; then
      echo "tau raw-artifact archive metadata is inconsistent: ${evidence}" >&2
      exit 1
    fi
    expected_archive_sha256="$(jq -r '.archive.sha256 // empty' "${evidence}")"
    actual_archive_sha256="$(sha256sum "${archive}" | cut -d ' ' -f 1)"
    if [[ -z "${expected_archive_sha256}" || "${actual_archive_sha256}" != "${expected_archive_sha256}" ]]; then
      echo "tau raw-artifact archive evidence is inconsistent: ${archive}" >&2
      exit 1
    fi
    if [[ "$(stat -c '%s' "${archive}")" != "$(jq -r '.archive.bytes' "${evidence}")" ]]; then
      echo "tau raw-artifact archive byte count is inconsistent: ${archive}" >&2
      exit 1
    fi
    tar --zstd -tf "${archive}" >/dev/null
    if [[ -d "${artifacts}" ]]; then
      find "${artifacts}" -depth -delete
      echo "removed verified duplicate expanded artifacts: ${artifacts}"
    fi
    echo "verified tau raw-artifact archive: ${archive}"
    continue
  fi
  if [[ -e "${archive}" || -e "${evidence}" ]]; then
    echo "incomplete tau raw-artifact archive pair: ${experiment}" >&2
    exit 1
  fi
  if [[ ! -d "${artifacts}" ]]; then
    echo "tau raw artifacts are missing and no verified archive exists: ${artifacts}" >&2
    exit 1
  fi

  task_directories="$(find "${artifacts}" -mindepth 1 -maxdepth 1 -type d -name 'task_*' | wc -l)"
  if [[ "${task_directories}" != "${expected_simulations}" ]]; then
    echo "tau attempt evidence has ${task_directories}/${expected_simulations} task directories: ${artifacts}" >&2
    exit 1
  fi
  successful_attempts=0
  failed_infrastructure_attempts=0
  retried_tasks=0
  maximum_observed_attempts=0
  while IFS= read -r task_directory; do
    mapfile -t statuses < <(find "${task_directory}" -mindepth 2 -maxdepth 2 -type f -name sim_status.json | sort)
    attempts="${#statuses[@]}"
    if (( attempts < 1 || attempts > maximum_attempts )); then
      echo "tau task attempt count is outside the preregistered bound: ${task_directory} (${attempts}/${maximum_attempts})" >&2
      exit 1
    fi
    if (( attempts > maximum_observed_attempts )); then
      maximum_observed_attempts="${attempts}"
    fi
    if (( attempts > 1 )); then
      retried_tasks=$((retried_tasks + 1))
    fi
    used_in_task=0
    for status in "${statuses[@]}"; do
      state="$(jq -r '.status // empty' "${status}")"
      if [[ "${state}" == "used" ]]; then
        used_in_task=$((used_in_task + 1))
        successful_attempts=$((successful_attempts + 1))
        simulation_id="$(basename "$(dirname "${status}")")"
        simulation_id="${simulation_id#sim_}"
        if [[ ! -f "${experiment}/simulations/${simulation_id}.json" ]] || \
          ! jq -e --arg id "${simulation_id}" '.simulation_index[] | select(.id == $id)' \
            "${experiment}/results.json" >/dev/null; then
          echo "used tau attempt is not the indexed scoring simulation: ${status}" >&2
          exit 1
        fi
      elif [[ "${state}" == "failed" ]]; then
        if ! jq -e \
          '.reason == "infrastructure_error" and
           (.error | type == "string" and length > 0) and
           (.error_type | type == "string" and length > 0)' \
          "${status}" >/dev/null; then
          echo "failed tau attempt is not typed infrastructure evidence: ${status}" >&2
          exit 1
        fi
        failed_infrastructure_attempts=$((failed_infrastructure_attempts + 1))
      else
        echo "unknown tau attempt status ${state@Q}: ${status}" >&2
        exit 1
      fi
    done
    if [[ "${used_in_task}" != "1" ]]; then
      echo "tau task must have exactly one used scoring attempt: ${task_directory}" >&2
      exit 1
    fi
  done < <(find "${artifacts}" -mindepth 1 -maxdepth 1 -type d -name 'task_*' | sort)
  total_attempts=$((successful_attempts + failed_infrastructure_attempts))

  source_files="$(find "${artifacts}" -type f | wc -l)"
  source_bytes="$(find "${artifacts}" -type f -printf '%s\n' | awk '{total += $1} END {print total + 0}')"
  if [[ "${source_files}" == 0 || "${source_bytes}" == 0 ]]; then
    echo "tau raw-artifact directory is empty: ${artifacts}" >&2
    exit 1
  fi
  temporary_archive="${archive}.next.$$"
  temporary_evidence="${evidence}.next.$$"
  cleanup() {
    rm -f -- "${temporary_archive}" "${temporary_evidence}"
  }
  trap cleanup EXIT
  tar \
    --sort=name \
    --mtime=@0 \
    --owner=0 \
    --group=0 \
    --numeric-owner \
    --format=pax \
    --pax-option=delete=atime,delete=ctime \
    --zstd \
    -cf "${temporary_archive}" \
    -C "${experiment}" artifacts
  tar --zstd -tf "${temporary_archive}" >/dev/null
  archive_sha256="$(sha256sum "${temporary_archive}" | cut -d ' ' -f 1)"
  archive_bytes="$(stat -c '%s' "${temporary_archive}")"
  jq -n \
    --arg created_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg matrix "$(realpath --relative-to="${repository_root}" "${matrix}")" \
    --arg matrix_id "${matrix_id}" \
    --arg matrix_sha256 "${matrix_sha256}" \
    --arg cell "${cell}" \
    --arg domain "${domain}" \
    --arg archive "raw-artifacts.tar.zst" \
    --arg archive_sha256 "${archive_sha256}" \
    --argjson archive_bytes "${archive_bytes}" \
    --argjson source_files "${source_files}" \
    --argjson source_bytes "${source_bytes}" \
    --argjson tasks "${expected_simulations}" \
    --argjson total_attempts "${total_attempts}" \
    --argjson successful_attempts "${successful_attempts}" \
    --argjson failed_infrastructure_attempts "${failed_infrastructure_attempts}" \
    --argjson retried_tasks "${retried_tasks}" \
    --argjson maximum_observed_attempts "${maximum_observed_attempts}" \
    --argjson maximum_attempts "${maximum_attempts}" \
    --argjson retry_delay_seconds "${retry_delay_seconds}" \
    '{
      schema_version:"1.0.0",
      created_at:$created_at,
      matrix:{path:$matrix,id:$matrix_id,sha256:$matrix_sha256},
      population:{cell:$cell,domain:$domain},
      source:{path:"artifacts",files:$source_files,bytes:$source_bytes},
      attempts:{
        tasks:$tasks,
        total:$total_attempts,
        successful:$successful_attempts,
        failed_infrastructure:$failed_infrastructure_attempts,
        retried_tasks:$retried_tasks,
        maximum_observed:$maximum_observed_attempts,
        maximum_allowed:$maximum_attempts,
        retry_delay_seconds:$retry_delay_seconds,
        seed_reused:true,
        retry_scope:"exceptions_only",
        semantic_outcomes_retried:false
      },
      archive:{
        path:$archive,
        format:"deterministic-pax-tar+zstd",
        sha256:$archive_sha256,
        bytes:$archive_bytes
      },
      restoration:"tar --zstd -xf raw-artifacts.tar.zst"
    }' >"${temporary_evidence}"
  mv "${temporary_archive}" "${archive}"
  mv "${temporary_evidence}" "${evidence}"
  trap - EXIT
  find "${artifacts}" -depth -delete
  echo "archived ${source_files} files (${source_bytes} bytes) to ${archive}; expanded artifacts removed and recoverable from the verified archive"
done < <(
  jq -r '
    .cells[].id as $cell |
    .benchmark.domains[] |
    [$cell, .name, (.tasks | tostring)] | @tsv
  ' "${matrix}"
)
