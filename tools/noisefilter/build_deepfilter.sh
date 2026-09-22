#!/usr/bin/env bash
set -euo pipefail
# Build libDF's C API (DeepFilterNet3 through the upstream Rust/tract real-time
# runtime) for `server.py --model deepfilternet`, pinned to the v0.5.6 release.
# Binaries and the model archive live under .runtime, out of the checkout.
#
#   bash tools/noisefilter/build_deepfilter.sh
#   python3 tools/noisefilter/server.py --model deepfilternet \
#     --library "$PWD/.runtime/deepfilter-filter/libdf.so" \
#     --deepfilter-model "$PWD/.runtime/deepfilter-filter/DeepFilterNet3_onnx.tar.gz"
#
# Requires a Rust toolchain (cargo >= 1.70) and Git.
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
out_dir="${1:-$root/.runtime/deepfilter-filter}"
src_dir="$out_dir/src"
revision=978576aa8400552a4ce9730838c635aa30db5e61  # tag v0.5.6
if [[ ! -d "$src_dir/.git" ]]; then
  git clone https://github.com/Rikorose/DeepFilterNet.git "$src_dir"
fi
if [[ "$(git -C "$src_dir" rev-parse HEAD)" != "$revision" ]]; then
  if [[ -n "$(git -C "$src_dir" status --porcelain)" ]]; then
    echo "Refusing to change a modified DeepFilterNet source directory" >&2
    exit 1
  fi
  git -C "$src_dir" checkout --detach "$revision"
fi
cd "$src_dir"
# v0.5.6 locks time 0.3.28, which no longer compiles on current rustc (E0282);
# 0.3.36 is the first release with the fix.
cargo update -p time --precise 0.3.36
CARGO_TARGET_DIR="$out_dir/target" cargo rustc --release -p deep_filter \
  --features capi --crate-type cdylib -j"${BUILD_JOBS:-8}"
cp "$out_dir/target/release/libdf.so" "$out_dir/libdf.so"
cp models/DeepFilterNet3_onnx.tar.gz "$out_dir/DeepFilterNet3_onnx.tar.gz"
printf 'DeepFilterNet library: %s/libdf.so\nDeepFilterNet model: %s/DeepFilterNet3_onnx.tar.gz\n' "$out_dir" "$out_dir"
