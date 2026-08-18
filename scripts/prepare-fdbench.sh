#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
manifest="${repository_root}/benchmarks/external/fd-bench.manifest.json"
runtime_root="${FDBENCH_RUNTIME_ROOT:-${repository_root}/.runtime/fd-bench}"
archive_root="${runtime_root}/archives"
dataset_root="${runtime_root}/dataset"
upstream_root="${runtime_root}/upstream"

cd "${repository_root}"

for command_name in curl git jq sha256sum tar; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done

mkdir -p "${archive_root}" "${dataset_root}"
dataset_revision="$(jq -r '.dataset_revision' "${manifest}")"

while IFS=$'\t' read -r cell remote_path expected_bytes expected_sha256; do
  archive="${archive_root}/${cell}.tgz"
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
    echo "downloading ${cell}"
    curl --fail --location --retry 4 \
      --output "${partial}" \
      "https://huggingface.co/datasets/pengyizhou/FD-Bench-Audio-Input/resolve/${dataset_revision}/${remote_path}"
    actual_bytes="$(stat -c '%s' "${partial}")"
    actual_sha256="$(sha256sum "${partial}" | cut -d ' ' -f 1)"
    if [[ "${actual_bytes}" != "${expected_bytes}" || "${actual_sha256}" != "${expected_sha256}" ]]; then
      echo "${cell} archive identity mismatch" >&2
      exit 1
    fi
    mv "${partial}" "${archive}"
  fi
  tar -xzf "${archive}" -C "${dataset_root}"
done < <(jq -r '.archives[] | [.cell,.path,(.bytes|tostring),.sha256] | @tsv' "${manifest}")

upstream_revision="$(jq -r '.upstream_revision' "${manifest}")"
if [[ ! -d "${upstream_root}/.git" ]]; then
  git clone --filter=blob:none "$(jq -r '.upstream_repository' "${manifest}")" "${upstream_root}"
fi
git -C "${upstream_root}" fetch --quiet origin "${upstream_revision}"
git -C "${upstream_root}" checkout --quiet --detach "${upstream_revision}"
if [[ "$(git -C "${upstream_root}" rev-parse HEAD)" != "${upstream_revision}" ]]; then
  echo "FD-Bench upstream revision mismatch" >&2
  exit 1
fi
while IFS=$'\t' read -r relative_path expected_sha256; do
  actual_sha256="$(sha256sum "${upstream_root}/${relative_path}" | cut -d ' ' -f 1)"
  if [[ "${actual_sha256}" != "${expected_sha256}" ]]; then
    echo "FD-Bench source identity mismatch: ${relative_path}" >&2
    exit 1
  fi
done < <(jq -r '.source_files | to_entries[] | [.key,.value] | @tsv' "${manifest}")

inspection="${runtime_root}/inspection.json"
/usr/local/go/bin/go run ./cmd/fdbench inspect --dataset-root "${dataset_root}" >"${inspection}"
expected_cells="$(jq '.expected_cells' "${manifest}")"
expected_samples="$(jq '.expected_released_conversations' "${manifest}")"
actual_cells="$(jq '.cells | length' "${inspection}")"
actual_samples="$(jq '.samples' "${inspection}")"
if [[ "${actual_cells}" != "${expected_cells}" || "${actual_samples}" != "${expected_samples}" ]]; then
  echo "discovered ${actual_cells} FD-Bench cells/${actual_samples} conversations; expected ${expected_cells}/${expected_samples}" >&2
  exit 1
fi
if ! jq -e 'all(.cells[]; .samples == 291 and .missing_conversation_ids == [60,120])' "${inspection}" >/dev/null; then
  echo "FD-Bench released-cell population differs from the pinned discrepancy" >&2
  exit 1
fi

echo "FD-Bench ready: ${dataset_root} (${actual_cells} cells, ${actual_samples} conversations)"
