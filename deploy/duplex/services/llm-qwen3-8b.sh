#!/usr/bin/env bash
# Qwen3-8B on vLLM (:9100, served as qwen3-8b): the cascade foreground and
# background model and the orchestrated micro-turn controller. ~22 GB.
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/vllm-common.sh"
NAME=llm-qwen3-8b PORT=${PORT:-9100} HEALTH=http://127.0.0.1:${PORT:-9100}/health
MODEL=$(snapshot Qwen/Qwen3-8B)
COMMAND=("$VLLM_PYTHON" -m vllm.entrypoints.openai.api_server --model "$MODEL" --served-model-name qwen3-8b
  --host 127.0.0.1 --port "$PORT" --gpu-memory-utilization 0.22 --max-model-len 16384
  --enable-prefix-caching --enable-auto-tool-choice --tool-call-parser hermes)
vllm_service "$@"
