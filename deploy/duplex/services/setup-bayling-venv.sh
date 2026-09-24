#!/usr/bin/env bash
# (Re)build the BayLing-Duplex venv (plan P8 task extension). Upstream allows
# torch>=2.1; like the other venvs this uses torch 2.9.1+cu128 for sm_120, with
# the unmodified bayling_duplex package installed editable from the pinned
# source tree. The venv was lost to a disk cleanup on 2026-09-23.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
PLAN=$ROOT/.runtime/duplex-plan
VENV=${BAYLING_VENV:-$PLAN/venvs/bayling}
[[ -x $VENV/bin/python ]] || uv venv --python 3.11 "$VENV"
uv pip install --python "$VENV/bin/python" torch==2.9.1 torchaudio==2.9.1 --index-url https://download.pytorch.org/whl/cu128
uv pip install --python "$VENV/bin/python" "torch==2.9.1" "torchaudio==2.9.1" -r "$PLAN/src/BayLing-Duplex/requirements.txt" \
  -e "$PLAN/src/BayLing-Duplex" soundfile librosa --extra-index-url https://download.pytorch.org/whl/cu128 --index-strategy unsafe-best-match
"$VENV/bin/python" -c "import torch, bayling_duplex; print('bayling venv ok', torch.__version__, torch.cuda.is_available())"
