#!/usr/bin/env bash
# Hibiki simultaneous French->English speech translation (plan cell X0).
#
#   weights  kyutai/hibiki-2b-pytorch-bf16 @ bd71144c96f26040612f6414716f5f48ee4fce69 (CC-BY-4.0)
#   stack    moshi 0.2.13, torch 2.9.1+cu128, venv .runtime/duplex-plan/venvs/kyutai
#   memory   ~6.7 GB resident -> a small model (no large-model lease; needs >= 10 GB free)
#
# Hibiki is a translation task profile, not a conversation: its text stream is
# the English it is speaking, it never transcribes the French source, and it
# owns no floor. The sidecar speaks protocol v1 on stdin/stdout and is spawned
# per session (there is no long-running service to start):
#
#   deploy/duplex/services/kyutai-hibiki.sh sidecar [--mock] [--cfg-coef 1] [--flush-pace realtime|fast]
#   deploy/duplex/services/kyutai-hibiki.sh conformance     # mock, then the real model
#   deploy/duplex/services/kyutai-hibiki.sh eval            # FLEURS fr->en BLEU + lag, 30 utts at real time
#
# `commit` ends a source segment: the sidecar feeds Hibiki's end-of-stream
# marker, flushes the rest of the translation, sends text_done + turn_done and
# resets. Without commit it translates a continuous stream.
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
VENV=${HIBIKI_VENV:-$PLAN/venvs/kyutai}
PY=$VENV/bin/python
BIN=${OPENREALTIME_BIN:-$PLAN/bin/openrealtime}
RESULTS=$PLAN/results/translation
export PYTORCH_CUDA_ALLOC_CONF=${PYTORCH_CUDA_ALLOC_CONF:-expandable_segments:True}
cd "$ROOT"

free_mib() { nvidia-smi --query-gpu=memory.total,memory.used --format=csv,noheader,nounits | awk -F', ' '{print $1-$2}'; }
need_gpu() {
  local free
  free=$(free_mib)
  if (( free < 10240 )); then
    echo "only ${free} MiB free on the GPU; Hibiki needs ~6.7 GB and the plan requires >= 10 GB free" >&2
    exit 1
  fi
}

case ${1:-help} in
  sidecar)
    shift
    exec "$PY" sidecars/hibiki_sidecar.py "$@"
    ;;
  conformance)
    mkdir -p "$RESULTS"
    timeout 300 "$BIN" conformance sidecar -out "$RESULTS/hibiki-conformance-mock.json" -- "$PY" sidecars/hibiki_sidecar.py --mock
    need_gpu
    timeout 590 "$BIN" conformance sidecar -out "$RESULTS/hibiki-conformance-real.json" -- "$PY" sidecars/hibiki_sidecar.py
    ;;
  eval)
    need_gpu
    "$PY" tools/duplexmodels/translation_eval.py download
    out=$RESULTS/hibiki-fleurs-fr-en.jsonl
    rm -f "$out"
    # Two halves so each foreground run stays under ten minutes.
    timeout 590 "$PY" tools/duplexmodels/translation_eval.py run --offset 0 --limit 15 --out "$out"
    timeout 590 "$PY" tools/duplexmodels/translation_eval.py run --offset 15 --limit 15 --out "$out"
    "$PY" tools/duplexmodels/translation_eval.py summarize --jsonl "$out" --out "$RESULTS/hibiki-fleurs-fr-en-summary.json"
    ;;
  *)
    sed -n '2,20p' "$0"
    ;;
esac
