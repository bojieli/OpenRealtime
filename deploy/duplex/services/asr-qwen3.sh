#!/usr/bin/env bash
# Qwen3-ASR 0.6B, the official start/chunk/finish streaming demo server
# (:9102; the room's copy stays on :8001). Baseline recogniser for cell C0.
# It cannot serve two chunks at once - one collision wedges it - so drive it
# with one session at a time. ~14 GB at 0.14 utilisation.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/vllm-common.sh"
NAME=asr-qwen3 PORT=${PORT:-9102} HEALTH=http://127.0.0.1:${PORT:-9102}/
COMMAND=("$VLLM_PYTHON" -m qwen_asr.cli.demo_streaming --asr-model-path Qwen/Qwen3-ASR-0.6B
  --host 127.0.0.1 --port "$PORT" --gpu-memory-utilization 0.14 --chunk-size-sec 0.2)
vllm_service "$@"
