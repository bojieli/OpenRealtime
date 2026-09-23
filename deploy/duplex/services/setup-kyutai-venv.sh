#!/usr/bin/env bash
# (Re)build the kyutai venv used by Kyutai TTS/STT, Moshi and PersonaPlex.
# kyutai-requirements.txt is the exact freeze of the working venv
# (torch 2.9.1+cu128, moshi 0.2.13). The venv was lost to a disk cleanup on
# 2026-09-23 and had no setup record; this is it.
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
VENV=${KYUTAI_VENV:-$ROOT/.runtime/duplex-plan/venvs/kyutai}
[[ -x $VENV/bin/python ]] || uv venv --python 3.11 "$VENV"
uv pip install --python "$VENV/bin/python" --reinstall -r "$ROOT/deploy/duplex/services/kyutai-requirements.txt" \
  --extra-index-url https://download.pytorch.org/whl/cu128 --index-strategy unsafe-best-match
"$VENV/bin/python" -c "import torch, moshi.models.tts, fastapi; print('kyutai venv ok', torch.__version__)"
