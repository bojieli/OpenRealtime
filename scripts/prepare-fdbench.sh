#!/usr/bin/env bash
#
# Prepares FD-Bench: 6,147 conversations across synthesiser, difficulty, and
# noise partitions, with sample-accurate turn boundaries.
#
#   scripts/prepare-fdbench.sh
#
# The partitions are not interchangeable and the benchmark runner requires one
# to be named: a result from the clean set says nothing about the 0 dB set.

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/dataset-lib.sh
source "${repository_root}/scripts/dataset-lib.sh"

manifest="${repository_root}/datasets/manifests/fd-bench.json"
runtime_root="${FDBENCH_RUNTIME_ROOT:-${repository_root}/.runtime/fd-bench}"
archive_root="${runtime_root}/archives"
dataset_root="${runtime_root}/dataset"

require_commands curl jq sha256sum tar
mkdir -p "${archive_root}" "${dataset_root}"

dataset_repository="$(jq -r '.dataset_repository' "${manifest}")"
dataset_revision="$(jq -r '.dataset_revision' "${manifest}")"

while IFS=$'\t' read -r path expected_sha256; do
  archive="${archive_root}/$(basename "${path}")"
  fetch_archive "${dataset_repository}/resolve/${dataset_revision}/${path}" "${archive}" "${expected_sha256}"
  tar -xf "${archive}" -C "${dataset_root}"
done < <(jq -r '.archives[] | [.path,.sha256] | @tsv' "${manifest}")

inspector="$(openrealtime_inspector "${repository_root}")"
trap 'rm -rf "$(dirname "${inspector}")"' EXIT

inspection="${runtime_root}/inspection.json"
"${inspector}" datasets inspect -root "${dataset_root}" -pattern '*.wav' >"${inspection}"
verify_sample_count "${inspection}" "$(jq -r '.expected_released_conversations' "${manifest}")" "FD-Bench"
verify_group_counts "${inspection}" "$(jq -c '.expected_cell_populations' "${manifest}")" "FD-Bench"

echo "FD-Bench ready: ${dataset_root} ($(jq -r '.count' "${inspection}") conversations)"
echo "  openrealtime bench fdbench --list"
