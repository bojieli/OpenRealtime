#!/usr/bin/env bash
# (Re)build the seamless-cu128 venv used by tools/duplexmodels/seamless_eval.py.
# SeamlessStreaming's pinned stack (fairseq2 0.2.1, torch 2.2/cu121) has no
# sm_120 kernels, so this venv runs torch 2.9.1+cu128 with fairseq2n 0.2.1
# built from the vendored source, whose torch type caster carries a one-line
# patch for the torch >= 2.4 THPVariable_Wrap signature. uv reuses its cached
# wheel of that build; otherwise it compiles it (CMake + CUDA 12.8).
# seamless-cu128-requirements.txt is the rest of the original freeze, without
# torch's own CUDA packages.
# The venv was lost to a disk cleanup on 2026-09-23.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
PLAN=$ROOT/.runtime/duplex-plan
VENV=${SEAMLESS_VENV:-$PLAN/venvs/seamless-cu128}
FAIRSEQ2N=$PLAN/src/fairseq2-v0.2.1/fairseq2n/python
grep -q "THPVariable_Wrap(const at::TensorBase &var)" \
  "$FAIRSEQ2N/src/fairseq2n/bindings/type_casters/torch.cc" || { echo "fairseq2n torch 2.9 patch missing" >&2; exit 1; }
[[ -x $VENV/bin/python ]] || uv venv --python 3.11 "$VENV"
uv pip install --python "$VENV/bin/python" torch==2.9.1 torchaudio==2.9.1 --index-url https://download.pytorch.org/whl/cu128
uv pip install --python "$VENV/bin/python" --no-deps -r "$ROOT/deploy/duplex/services/seamless-cu128-requirements.txt"
# The freeze came from a torch 2.2 venv; torch 2.9.1 brings its own CUDA
# libraries (nvidia-*, triton), so those pins are excluded and restored here.
uv pip install --python "$VENV/bin/python" torch==2.9.1 torchaudio==2.9.1 --index-url https://download.pytorch.org/whl/cu128 --reinstall-package nvidia-cudnn-cu12
uv pip install --python "$VENV/bin/python" setuptools wheel
uv pip install --python "$VENV/bin/python" --no-deps --no-build-isolation "$FAIRSEQ2N"
uv pip install --python "$VENV/bin/python" --no-deps -e "$PLAN/src/seamless_communication"
# setuptools/wheel above pull a newer packaging than fairseq2 accepts.
uv pip install --python "$VENV/bin/python" --no-deps packaging==23.2
"$VENV/bin/python" -c "import torch, fairseq2, fairseq2n, seamless_communication, simuleval; print('seamless-cu128 ok', torch.__version__, torch.cuda.is_available(), fairseq2.__version__)"
