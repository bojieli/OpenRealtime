#!/usr/bin/env bash
#
# Prepare the local word-timing recogniser.
#
#   ./scripts/prepare-wordtimings.sh           # venv, faster-whisper, weights
#   ./scripts/prepare-wordtimings.sh --serve   # and then serve it
#
# This is what turns "the agent was audible for 2.1 seconds" into "the agent
# said these six words and not the four after them". Without it the runtime
# still finds the boundary, proportionally, and records that nothing measured
# it; with it the boundary comes from the audio.
#
# The venv borrows the system torch rather than installing its own, for the
# same reason prepare-sensevoice.sh does: a CUDA build that works on the
# machine's GPU is the hard part of this setup, and resolving a second one is
# how a working environment acquires two.
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

venv="${WORD_TIMINGS_VENV:-${repository_root}/.runtime/wordtimings}"
host="${WORD_TIMINGS_HOST:-127.0.0.1}"
port="${WORD_TIMINGS_PORT:-8003}"
model="${WORD_TIMINGS_MODEL:-Systran/faster-whisper-base.en}"
device="${WORD_TIMINGS_DEVICE:-cuda}"

if [[ ! -x "${venv}/bin/python" ]]; then
  echo "creating ${venv}"
  python3 -m venv --system-site-packages "${venv}"
fi

echo "installing faster-whisper and the server's dependencies"
"${venv}/bin/pip" install --quiet --upgrade pip
"${venv}/bin/pip" install --quiet faster-whisper soundfile numpy fastapi \
  'uvicorn[standard]' python-multipart

# Fetching the weights here rather than on the first request means a cold start
# is a slow script rather than a session whose first interruption is timed by
# an estimate because the model was still downloading.
echo "fetching ${model}"
WORD_TIMINGS_MODEL="${model}" "${venv}/bin/python" - <<'PY'
import os
from faster_whisper import WhisperModel
WhisperModel(os.environ["WORD_TIMINGS_MODEL"], device="cpu", compute_type="int8")
print("weights present")
PY

echo
echo "ready. serve it with:"
echo "  WORD_TIMINGS_MODEL=${model} WORD_TIMINGS_DEVICE=${device} \\"
echo "    ${venv}/bin/python -m uvicorn server:app --host ${host} --port ${port} \\"
echo "    --app-dir ${repository_root}/deploy/wordtimings"
echo "then point the runtime at it:"
echo "  openrealtime serve -word-timings-url http://${host}:${port}/v1/audio/transcriptions"

if [[ "${1:-}" == "--serve" ]]; then
  echo
  WORD_TIMINGS_MODEL="${model}" WORD_TIMINGS_DEVICE="${device}" \
    exec "${venv}/bin/python" -m uvicorn server:app --host "${host}" --port "${port}" \
    --app-dir "${repository_root}/deploy/wordtimings"
fi
