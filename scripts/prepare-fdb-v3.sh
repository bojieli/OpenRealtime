#!/usr/bin/env bash
#
# Prepares Full-Duplex-Bench v3: 100 tool-use recordings spoken with the
# disfluencies people actually produce, annotated with the calls that should
# result.
#
#   scripts/prepare-fdb-v3.sh

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/dataset-lib.sh
source "${repository_root}/scripts/dataset-lib.sh"

manifest="${repository_root}/datasets/manifests/full-duplex-bench-v3.json"
runtime_root="${FDBV3_RUNTIME_ROOT:-${repository_root}/.runtime/full-duplex-bench-v3}"
archive_root="${runtime_root}/archives"
dataset_root="${runtime_root}/dataset"

require_commands gdown jq sha256sum unzip
mkdir -p "${archive_root}" "${dataset_root}"

file_id="$(jq -r '.dataset_file_id' "${manifest}")"
expected_sha256="$(jq -r '.released_artifact.sha256 // .released_artifact.archive_sha256 // empty' "${manifest}")"
archive="${archive_root}/fdb_v3_data_released.zip"
if [[ -n "${expected_sha256}" ]]; then
  fetch_archive "gdrive:${file_id}" "${archive}" "${expected_sha256}"
elif [[ ! -f "${archive}" ]]; then
  gdown --quiet --id "${file_id}" --output "${archive}"
fi
unzip -q -o "${archive}" -d "${dataset_root}"
released_root="${dataset_root}/fdb_v3_data_released"

inspector="$(openrealtime_inspector "${repository_root}")"
trap 'rm -rf "$(dirname "${inspector}")"' EXIT

inspection="${runtime_root}/inspection.json"
"${inspector}" datasets inspect -root "${released_root}" -pattern input.wav >"${inspection}"
verify_sample_count "${inspection}" "$(jq -r '.released_artifact.audio_examples' "${manifest}")" "Full-Duplex-Bench v3"

# Sample directories are named <domain>_<index>_<scenario id>; the manifest
# pins how many belong to each domain.
domains="${runtime_root}/domains.json"
jq -c '[.groups[] | {name: (.name | split("_")[0]), count: .count}]
       | group_by(.name)
       | [.[] | {(.[0].name): (map(.count) | add)}]
       | add // {}' "${inspection}" >"${domains}"
observed_domains="$(jq -cS 'with_entries(.key |= (
    if . == "travel" then "travel_identity"
    elif . == "finance" then "finance_billing"
    elif . == "housing" then "housing_location"
    elif . == "ecommerce" then "ecommerce_support"
    else . end))' "${domains}")"
expected_domains="$(jq -cS '.released_artifact.domains' "${manifest}")"
if [[ "${observed_domains}" != "${expected_domains}" ]]; then
  echo "Full-Duplex-Bench v3 domain populations differ: got ${observed_domains}, pinned ${expected_domains}" >&2
  exit 1
fi

echo "Full-Duplex-Bench v3 ready: ${released_root} ($(jq -r '.count' "${inspection}") examples)"
echo "  openrealtime bench fdbv3 --dataset ${released_root}"
