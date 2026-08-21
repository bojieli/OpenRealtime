#!/usr/bin/env bash
#
# Shared verification for a prepared dataset.
#
# The archives come from where their owners host them; this repository
# redistributes none of them. What it pins is the digest of each archive and
# the sample count that should result, which is what makes "the dataset is
# ready" a checkable statement rather than a hope.
#
# Sourced by the prepare-* scripts.

set -euo pipefail

# openrealtime_inspector builds the inventory tool into a temporary directory.
#
# It builds rather than assuming an installed binary: a preparation script that
# needs the thing it is preparing for only works on a machine already set up.
openrealtime_inspector() {
  local repository_root="$1"
  local directory
  directory="$(mktemp -d)"
  # shellcheck source=scripts/go-toolchain.sh
  source "${repository_root}/scripts/go-toolchain.sh"
  local go_binary
  go_binary="$(openrealtime_go_bin)"
  (cd "${repository_root}" && "${go_binary}" build -o "${directory}/openrealtime" ./cmd/openrealtime) >&2
  printf '%s\n' "${directory}/openrealtime"
}

# verify_sample_count compares what landed against what the manifest pins.
#
# A dataset that half-extracted is the failure this catches. Without it, the
# symptom is a benchmark cell that scores lower than it should for a reason
# nobody would think to look for.
verify_sample_count() {
  local inspection="$1" expected="$2" label="$3"
  local actual
  actual="$(jq -r '.count' "${inspection}")"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "${label}: found ${actual} samples, the manifest pins ${expected}" >&2
    return 1
  fi
  return 0
}

# fetch_archive downloads one archive and verifies its digest before use.
fetch_archive() {
  local url="$1" destination="$2" expected_sha256="$3"
  if [[ -f "${destination}" ]]; then
    local existing
    existing="$(sha256sum "${destination}" | cut -d ' ' -f 1)"
    if [[ "${existing}" == "${expected_sha256}" ]]; then
      return 0
    fi
    echo "cached archive ${destination} does not match its pinned digest; refetching" >&2
    rm -f "${destination}"
  fi
  local partial="${destination}.partial"
  if [[ "${url}" == gdrive:* ]]; then
    gdown --quiet --id "${url#gdrive:}" --output "${partial}"
  else
    curl --fail --location --silent --show-error --output "${partial}" "${url}"
  fi
  local actual
  actual="$(sha256sum "${partial}" | cut -d ' ' -f 1)"
  if [[ "${actual}" != "${expected_sha256}" ]]; then
    rm -f "${partial}"
    echo "archive digest mismatch for ${destination}: got ${actual}, pinned ${expected_sha256}" >&2
    return 1
  fi
  mv "${partial}" "${destination}"
}

require_commands() {
  local missing=()
  for command_name in "$@"; do
    command -v "${command_name}" >/dev/null || missing+=("${command_name}")
  done
  if (( ${#missing[@]} > 0 )); then
    echo "required commands are unavailable: ${missing[*]}" >&2
    return 1
  fi
}

# verify_group_counts compares the per-group inventory against a pinned map.
#
# The total alone would pass a dataset that extracted every file but dropped a
# whole partition and gained an equal number elsewhere. Cells are not
# interchangeable in any of these benchmarks, so the population of each is the
# thing worth pinning.
verify_group_counts() {
  local inspection="$1" expected_json="$2" label="$3"
  local actual
  actual="$(jq -cS '[.groups[] | {(.name): .count}] | add // {}' "${inspection}")"
  local expected
  expected="$(printf '%s' "${expected_json}" | jq -cS '.')"
  if [[ "${actual}" != "${expected}" ]]; then
    echo "${label}: per-group populations differ from the manifest" >&2
    diff <(printf '%s' "${expected}" | jq -S 'to_entries[] | "\(.key)\t\(.value)"' -r) \
         <(printf '%s' "${actual}" | jq -S 'to_entries[] | "\(.key)\t\(.value)"' -r) >&2 || true
    return 1
  fi
  return 0
}
