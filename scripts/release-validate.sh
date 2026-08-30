#!/usr/bin/env bash
# Run the versioned release matrix without depending on whichever Go happens
# to be first on PATH. All machine-readable output comes from releasevalidate.
set -euo pipefail

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"

# shellcheck source=scripts/go-toolchain.sh
source "${repository_root}/scripts/go-toolchain.sh"
go_bin="$(openrealtime_go_bin)"

cd -- "${repository_root}"
exec "${go_bin}" run ./internal/releasevalidation/cmd/releasevalidate \
  -root "${repository_root}" -go "${go_bin}" "$@"
