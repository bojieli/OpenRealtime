#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime_root="${FDBENCH_RUNTIME_ROOT:-${repository_root}/.runtime/fd-bench}"
environment_root="${runtime_root}/evaluator-venv"

cd "${repository_root}"

python3 -m venv --system-site-packages "${environment_root}"
"${environment_root}/bin/python" -m pip install --disable-pip-version-check --no-deps \
  ipdb==0.13.13 silero-vad==6.2.1 whisperx==3.3.1
"${environment_root}/bin/python" - <<'PY'
import ipdb
import silero_vad
import whisperx
print("FD-Bench timing evaluator imports are ready")
PY
