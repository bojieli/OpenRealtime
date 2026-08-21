#!/usr/bin/env bash
#
# Builds the release binaries.
#
#   ./scripts/build-release.sh [output-directory]
#
# Two properties matter here. The binaries are reproducible: the same source
# tree produces byte-identical output on any machine with the same Go
# toolchain, because the build paths are trimmed and no timestamp or hostname
# is baked in. And each binary can say what it was built from: the version
# command reports the release, the revision, and whether that revision had
# uncommitted changes, so a binary found running somewhere can be traced back
# to a source tree rather than guessed at.

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

output_directory="${1:-${repository_root}/dist}"
mkdir -p "${output_directory}"

# shellcheck source=scripts/go-toolchain.sh
source "${repository_root}/scripts/go-toolchain.sh"
go_bin="$(openrealtime_go_bin)"

release="$("${go_bin}" run ./cmd/openrealtime version | sed -n 's/^openrealtime \([^,]*\),.*/\1/p')"
if [[ -z "${release}" ]]; then
  release="$("${go_bin}" run ./cmd/openrealtime version | awk '{print $2}')"
fi

# The platforms a realtime server actually gets deployed to, plus the two
# desktop platforms someone runs a probe or a benchmark from.
platforms=(
  linux/amd64
  linux/arm64
  darwin/arm64
  darwin/amd64
  windows/amd64
)

echo "building openrealtime ${release} into ${output_directory}"

for platform in "${platforms[@]}"; do
  goos="${platform%%/*}"
  goarch="${platform##*/}"
  name="openrealtime-${release}-${goos}-${goarch}"
  binary="${output_directory}/${name}"
  [[ "${goos}" == "windows" ]] && binary="${binary}.exe"

  # CGO off keeps the binary static and the build reproducible; -trimpath
  # removes the build machine's directory layout from the panic traces and the
  # embedded paths, which is what makes two machines agree byte for byte.
  CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
    "${go_bin}" build -trimpath -ldflags="-s -w -buildid=" -o "${binary}" ./cmd/openrealtime

  printf '  %-40s %8s\n' "$(basename "${binary}")" "$(du -h "${binary}" | cut -f1)"
done

# A checksum file is the only part of a release anyone can verify without
# trusting the machine that produced it.
(cd "${output_directory}" && sha256sum openrealtime-"${release}"-* >"SHA256SUMS-${release}.txt")
echo
echo "checksums: ${output_directory}/SHA256SUMS-${release}.txt"
cat "${output_directory}/SHA256SUMS-${release}.txt"
