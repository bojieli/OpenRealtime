#!/usr/bin/env bash
set -euo pipefail
# Pin both the code and the model checksum selected by that revision's
# download_model.sh. Keep binaries and downloaded weights out of the checkout.
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
build_dir="${1:-$root/.runtime/rnnoise-filter}"
revision=70f1d256acd4b34a572f999a05c87bf00b67730d
if [[ ! -d "$build_dir/.git" ]]; then
  git clone https://github.com/xiph/rnnoise.git "$build_dir"
fi
if [[ "$(git -C "$build_dir" rev-parse HEAD)" != "$revision" ]]; then
  if [[ -n "$(git -C "$build_dir" status --porcelain)" ]]; then
    echo "Refusing to change a modified RNNoise build directory" >&2
    exit 1
  fi
  git -C "$build_dir" checkout --detach "$revision"
fi
cd "$build_dir"
./autogen.sh
./configure --disable-examples --disable-static
make -j"${BUILD_JOBS:-4}"
printf 'RNNoise library: %s/.libs/librnnoise.so\n' "$build_dir"
