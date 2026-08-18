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
canonical_data_root="$(realpath -m "${data_root}")"

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
      '.schema_version == "1.0.0" and
       .matrix.id == $matrix_id and .matrix.sha256 == $matrix_sha256 and
       .population.cell == $cell and .population.domain == $domain and
       .source.path == "artifacts" and .source.files > 0 and .source.bytes > 0 and
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
    '{
      schema_version:"1.0.0",
      created_at:$created_at,
      matrix:{path:$matrix,id:$matrix_id,sha256:$matrix_sha256},
      population:{cell:$cell,domain:$domain},
      source:{path:"artifacts",files:$source_files,bytes:$source_bytes},
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
