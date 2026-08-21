#!/usr/bin/env bash
#
# Prepares Full-Duplex-Bench v1.5: 498 overlap recordings across four
# categories, two of which want the agent to yield and two of which want it to
# hold.
#
#   scripts/prepare-fdb15.sh
#
# The archives are fetched from the dataset's own hosting and verified against
# the digests pinned in datasets/manifests/. Nothing is redistributed here.

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/dataset-lib.sh
source "${repository_root}/scripts/dataset-lib.sh"

manifest="${repository_root}/datasets/manifests/full-duplex-bench-v1.5.json"
runtime_root="${FDB15_RUNTIME_ROOT:-${repository_root}/.runtime/full-duplex-bench-v1.5}"
archive_root="${runtime_root}/archives"
dataset_root="${runtime_root}/dataset"

require_commands gdown jq sha256sum unzip
mkdir -p "${archive_root}" "${dataset_root}"

while IFS=$'\t' read -r scenario file_id expected_sha256; do
  archive="${archive_root}/${scenario}.zip"
  fetch_archive "gdrive:${file_id}" "${archive}" "${expected_sha256}"
  unzip -q -o "${archive}" -d "${dataset_root}"
done < <(jq -r '.archives[] | [.scenario,.file_id,.sha256] | @tsv' "${manifest}")

inspector="$(openrealtime_inspector "${repository_root}")"
trap 'rm -rf "$(dirname "${inspector}")"' EXIT

inspection="${runtime_root}/inspection.json"
"${inspector}" datasets inspect -root "${dataset_root}" -pattern input.wav >"${inspection}"
verify_sample_count "${inspection}" "$(jq -r '.observed_complete_samples' "${manifest}")" "Full-Duplex-Bench v1.5"

echo "Full-Duplex-Bench v1.5 ready: ${dataset_root} ($(jq -r '.count' "${inspection}") samples)"
echo "  openrealtime bench fdb --dataset ${dataset_root}"
