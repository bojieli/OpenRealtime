#!/usr/bin/env bash
# Rebuild Lychee-FD's vendored patched vLLM 0.6.5 for sm_120 (RTX PRO 6000).
# Upstream pins torch 2.5.1/cu124, which has no Blackwell kernels; the lychee
# venv runs torch 2.7.1+cu128. The tree is copied out of the pinned Lychee-FD
# source, patched (lychee-vllm-sm120.patch: sm_120 in the arch list, no
# vllm-flash-attn - Lychee uses XFORMERS), and its native ops compiled in place.
# lychee-fd.sh puts the result first on PYTHONPATH.
#
#   deploy/duplex/services/build-lychee-vllm.sh     # MAX_JOBS (default 6) bounds nvcc memory
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
PLAN=$ROOT/.runtime/duplex-plan
SRC=$PLAN/src/Lychee-FD/third_party/vllm
OUT=${LYCHEE_FD_VLLM_TREE:-$PLAN/build/lychee-vllm}
VENV=${LYCHEE_FD_VENV:-$PLAN/venvs/lychee}
CUDA=${CUDA_HOME:-/usr/local/cuda-12.8}
[[ "$(git -C "$PLAN/src/Lychee-FD" rev-parse HEAD)" == 7bb1068c59b578e1f8e65fb762cd26708c14cc6d ]] || { echo "unexpected Lychee-FD revision" >&2; exit 1; }
rm -rf "$OUT.partial"
mkdir -p "$(dirname "$OUT")"
cp -a "$SRC" "$OUT.partial"
patch -d "$OUT.partial" -p1 < "$ROOT/deploy/duplex/services/lychee-vllm-sm120.patch"
cd "$OUT.partial"
PATH="$VENV/bin:$CUDA/bin:$PATH" CUDA_HOME=$CUDA CUDACXX=$CUDA/bin/nvcc VLLM_TARGET_DEVICE=cuda TORCH_CUDA_ARCH_LIST=12.0 \
  MAX_JOBS=${MAX_JOBS:-6} SETUPTOOLS_SCM_PRETEND_VERSION=0.6.5 \
  "$VENV/bin/python" setup.py build_ext --inplace
ls vllm/_C.abi3.so vllm/_moe_C.abi3.so > /dev/null
rm -rf "$OUT"; mv "$OUT.partial" "$OUT"
echo "built $OUT"
