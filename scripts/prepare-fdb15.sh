#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
manifest="${repository_root}/benchmarks/external/full-duplex-bench-v1.5.manifest.json"
runtime_root="${FDB15_RUNTIME_ROOT:-${repository_root}/.runtime/full-duplex-bench-v1.5}"
archive_root="${runtime_root}/archives"
dataset_root="${runtime_root}/dataset"

for command_name in gdown jq sha256sum unzip; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done

mkdir -p "${archive_root}" "${dataset_root}"

while IFS=$'\t' read -r scenario file_id expected_bytes expected_sha256; do
  archive="${archive_root}/${scenario}.zip"
  valid=false
  if [[ -f "${archive}" ]]; then
    actual_bytes="$(stat -c '%s' "${archive}")"
    actual_sha256="$(sha256sum "${archive}" | cut -d ' ' -f 1)"
    if [[ "${actual_bytes}" == "${expected_bytes}" && "${actual_sha256}" == "${expected_sha256}" ]]; then
      valid=true
    fi
  fi
  if [[ "${valid}" != true ]]; then
    partial="${archive}.part"
    rm -f "${partial}"
    echo "downloading ${scenario}"
    gdown "${file_id}" --output "${partial}"
    actual_bytes="$(stat -c '%s' "${partial}")"
    actual_sha256="$(sha256sum "${partial}" | cut -d ' ' -f 1)"
    if [[ "${actual_bytes}" != "${expected_bytes}" || "${actual_sha256}" != "${expected_sha256}" ]]; then
      echo "${scenario} archive identity mismatch" >&2
      exit 1
    fi
    mv "${partial}" "${archive}"
  fi
  unzip -q -o "${archive}" -d "${dataset_root}"
done < <(jq -r '.archives[] | [.scenario,.file_id,(.bytes|tostring),.sha256] | @tsv' "${manifest}")

inspection="${runtime_root}/inspection.json"
/usr/local/go/bin/go run ./cmd/livebench inspect --dataset-root "${dataset_root}" >"${inspection}"
expected="$(jq '.observed_complete_samples' "${manifest}")"
actual="$(jq '.count' "${inspection}")"
if [[ "${actual}" != "${expected}" ]]; then
  echo "discovered ${actual} Full-Duplex-Bench samples, expected ${expected}" >&2
  exit 1
fi

echo "Full-Duplex-Bench v1.5 ready: ${dataset_root} (${actual} samples)"
