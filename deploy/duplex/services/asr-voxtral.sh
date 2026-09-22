#!/usr/bin/env bash
# Voxtral Mini 4B Realtime 2602 on vLLM's /v1/realtime route (:9101, served as
# voxtral-realtime; 480 ms transcription delay from the checkpoint's
# params.json). ~20 GB. max-model-len 4096 text tokens = 5.5 min per stream at
# 80 ms per token; the micro-turn sidecar restarts its stream every 4 min.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/vllm-common.sh"
NAME=asr-voxtral PORT=${PORT:-9101} HEALTH=http://127.0.0.1:${PORT:-9101}/health
MODEL=$(snapshot mistralai/Voxtral-Mini-4B-Realtime-2602)
COMMAND=(env VLLM_DISABLE_COMPILE_CACHE=1 "$VLLM_PYTHON" -m vllm.entrypoints.openai.api_server --model "$MODEL"
  --served-model-name voxtral-realtime --tokenizer-mode mistral --config-format mistral --load-format mistral
  --host 127.0.0.1 --port "$PORT" --gpu-memory-utilization 0.2 --max-model-len 4096
  --compilation_config '{"cudagraph_mode": "PIECEWISE"}')
vllm_service "$@"
