#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
manifest="${OPENREALTIME_STUDY_RUNTIME_MANIFEST:-${repository_root}/benchmarks/runtime/canonical-gateway-v1.json}"

for command_name in git install jq sha256sum tar; do
  if ! command -v "${command_name}" >/dev/null; then
    echo "required command ${command_name} is unavailable" >&2
    exit 1
  fi
done
if [[ ! -f "${manifest}" ]]; then
  echo "study runtime manifest is missing: ${manifest}" >&2
  exit 1
fi
# An empty manifest makes `jq -e` exit 0 for any filter, so a zero-byte file
# would read as a valid frozen-runtime declaration.
if ! jq -en --slurpfile manifest "${manifest}" '
  ($manifest | length) == 1 and
  ($manifest[0] |
    .schema_version == "1.0.0" and
    (.source_revision | type == "string" and test("^[0-9a-f]{40}$")) and
    (.binary_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
    .build.go == "/usr/local/go/bin/go" and
    .build.command == "go build -trimpath -buildvcs=false ./cmd/realtimegateway" and
    (.runtime_path | type == "string" and startswith(".runtime/")))
' >/dev/null; then
  echo "study runtime manifest is invalid: ${manifest}" >&2
  exit 1
fi

source_revision="$(jq -r '.source_revision' "${manifest}")"
expected_sha256="$(jq -r '.binary_sha256' "${manifest}")"
runtime_path="$(jq -r '.runtime_path' "${manifest}")"
destination="${repository_root}/${runtime_path}"
if ! git -C "${repository_root}" cat-file -e "${source_revision}^{commit}"; then
  echo "study runtime source revision is unavailable: ${source_revision}" >&2
  exit 1
fi
if [[ -x "${destination}" ]] && \
  [[ "$(sha256sum "${destination}" | cut -d ' ' -f 1)" == "${expected_sha256}" ]]; then
  printf '%s\n' "${destination}"
  exit 0
fi

build_root="$(mktemp -d)"
cleanup() {
  rm -rf -- "${build_root}"
}
trap cleanup EXIT
git -C "${repository_root}" archive "${source_revision}" | tar -x -C "${build_root}"
(
  cd "${build_root}"
  /usr/local/go/bin/go build -trimpath -buildvcs=false -o realtimegateway ./cmd/realtimegateway
)
actual_sha256="$(sha256sum "${build_root}/realtimegateway" | cut -d ' ' -f 1)"
if [[ "${actual_sha256}" != "${expected_sha256}" ]]; then
  echo "reproduced gateway hash mismatch: expected ${expected_sha256}, found ${actual_sha256}" >&2
  exit 1
fi
mkdir -p "$(dirname "${destination}")"
install -m 0755 "${build_root}/realtimegateway" "${destination}"
printf '%s\n' "${destination}"
