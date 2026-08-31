#!/usr/bin/env bash
#
# Prepare the local SenseVoice recogniser.
#
#   ./scripts/prepare-sensevoice.sh           # venv, FunASR, weights
#   ./scripts/prepare-sensevoice.sh --serve   # and then serve it
#
# The venv borrows the system torch rather than installing its own: a CUDA
# build that works on the machine's GPU is the hard part of this setup, and
# resolving a second one is how a working environment acquires two.
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

venv="${SENSEVOICE_VENV:-${repository_root}/.runtime/sensevoice}"
host="${SENSEVOICE_HOST:-127.0.0.1}"
port="${SENSEVOICE_PORT:-8002}"
model="${SENSEVOICE_MODEL:-iic/SenseVoiceSmall}"
model_path="${SENSEVOICE_MODEL_PATH:-${HOME}/.cache/modelscope/models/iic--SenseVoiceSmall/snapshots/master}"

if ! python3 -c 'import torch' >/dev/null 2>&1; then
  echo "this needs a working torch on the system interpreter; none imports" >&2
  exit 1
fi

if [[ ! -x "${venv}/bin/python" ]]; then
  echo "creating ${venv}"
  python3 -m venv --system-site-packages "${venv}"
fi

echo "installing FunASR and the server's dependencies"
"${venv}/bin/pip" install --quiet --upgrade pip
"${venv}/bin/pip" install --quiet funasr==1.2.6 modelscope soundfile fastapi \
  'uvicorn[standard]' python-multipart

# Fetching the weights here rather than on the first request means a cold
# start is a slow script rather than a session that times out.
echo "fetching ${model}"
SENSEVOICE_MODEL="${model}" "${venv}/bin/python" - <<'PY'
import os
from funasr import AutoModel
AutoModel(model=os.environ["SENSEVOICE_MODEL"], trust_remote_code=False,
          vad_model=None, device="cpu", disable_update=True)
print("weights present")
PY

echo
echo "ready. serve it with:"
echo "  SENSEVOICE_MODEL_PATH=${model_path} ${venv}/bin/python -m uvicorn server:app --host ${host} --port ${port} \\"
echo "    --app-dir ${repository_root}/deploy/sensevoice"
echo "then:"
echo "  openrealtime serve -asr-provider sensevoice"

if [[ "${1:-}" == "--serve" ]]; then
  echo
  if [[ ! -d "${model_path}" ]]; then
    echo "exact SenseVoice model path is unavailable: ${model_path}" >&2
    exit 1
  fi
  SENSEVOICE_MODEL_PATH="${model_path}" exec "${venv}/bin/python" -m uvicorn server:app --host "${host}" --port "${port}" \
    --app-dir "${repository_root}/deploy/sensevoice"
fi
