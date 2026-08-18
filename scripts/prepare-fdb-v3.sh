#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
manifest="${repository_root}/benchmarks/external/full-duplex-bench-v3.manifest.json"
runtime_root="${FDBV3_RUNTIME_ROOT:-${repository_root}/.runtime/full-duplex-bench-v3}"
archive_root="${runtime_root}/archives"
archive="${archive_root}/fdb-v3-data.zip"
dataset_root="${runtime_root}/dataset"
upstream_root="${FDB_UPSTREAM_ROOT:-${repository_root}/.runtime/full-duplex-bench}"

for command_name in gdown git jq sha256sum unzip; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done

upstream_revision="$(jq -r '.upstream_revision' "${manifest}")"
if [[ ! -d "${upstream_root}/.git" ]]; then
  git clone --filter=blob:none \
    "$(jq -r '.upstream_repository' "${manifest}")" "${upstream_root}"
fi
git -C "${upstream_root}" fetch --quiet origin "${upstream_revision}"
git -C "${upstream_root}" checkout --quiet --detach "${upstream_revision}"
if [[ "$(git -C "${upstream_root}" rev-parse HEAD)" != "${upstream_revision}" ]]; then
  echo "Full-Duplex-Bench v3 upstream revision mismatch" >&2
  exit 1
fi
while IFS=$'\t' read -r relative_path expected_sha256; do
  actual_sha256="$(sha256sum "${upstream_root}/${relative_path}" | cut -d ' ' -f 1)"
  if [[ "${actual_sha256}" != "${expected_sha256}" ]]; then
    echo "Full-Duplex-Bench v3 source identity mismatch: ${relative_path}" >&2
    exit 1
  fi
done < <(jq -r '
  [
    {path:.task_definition.path,sha256:.task_definition.sha256},
    {path:.official_harness.agent_path,sha256:.official_harness.agent_sha256},
    {path:.official_harness.mock_api_path,sha256:.official_harness.mock_api_sha256},
    {path:.official_harness.evaluator_path,sha256:.official_harness.evaluator_sha256},
    {path:.official_harness.runner_path,sha256:.official_harness.runner_sha256}
  ][] | [.path,.sha256] | @tsv' "${manifest}")

mkdir -p "${archive_root}" "${dataset_root}"
expected_bytes="$(jq -r '.released_artifact.bytes' "${manifest}")"
expected_sha256="$(jq -r '.released_artifact.sha256' "${manifest}")"
file_id="$(jq -r '.dataset_file_id' "${manifest}")"

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
  echo "downloading Full-Duplex-Bench v3 released audio"
  gdown "${file_id}" --output "${partial}"
  actual_bytes="$(stat -c '%s' "${partial}")"
  actual_sha256="$(sha256sum "${partial}" | cut -d ' ' -f 1)"
  if [[ "${actual_bytes}" != "${expected_bytes}" || "${actual_sha256}" != "${expected_sha256}" ]]; then
    echo "Full-Duplex-Bench v3 archive identity mismatch" >&2
    exit 1
  fi
  mv "${partial}" "${archive}"
fi

unzip -q -o "${archive}" -d "${dataset_root}"
released_root="${dataset_root}/fdb_v3_data_released"
inspection="${runtime_root}/inspection.json"
/usr/local/go/bin/go run ./cmd/fdbv3bench inspect --dataset-root "${released_root}" >"${inspection}"

for field in audio_examples unique_scenarios expected_tool_calls state_rollback_examples; do
  expected="$(jq -r ".released_artifact.${field}" "${manifest}")"
  actual="$(jq -r ".${field}" "${inspection}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "Full-Duplex-Bench v3 ${field} mismatch: got ${actual}, expected ${expected}" >&2
    exit 1
  fi
done

for domain in travel_identity finance_billing housing_location ecommerce_support; do
  expected="$(jq -r ".released_artifact.domains.${domain}" "${manifest}")"
  actual="$(jq -r ".domains.${domain}" "${inspection}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "Full-Duplex-Bench v3 domain ${domain} mismatch: got ${actual}, expected ${expected}" >&2
    exit 1
  fi
done

echo "Full-Duplex-Bench v3 ready: ${released_root} ($(jq -r '.audio_examples' "${inspection}") examples)"
