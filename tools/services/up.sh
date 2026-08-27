#!/usr/bin/env bash
# Bring up every local model this system needs, and wait until each answers.
#
# There are four, they live in three different virtual environments, and the
# order matters only in that the largest should claim its memory first. Written
# down because a GPU fault took all four out at once and bringing them back was
# four commands nobody had recorded in one place - and because a service that
# is up but not yet answering looks exactly like a service that is down.
set -euo pipefail

repository="${REPOSITORY:-/home/ubuntu/or-interaction}"
shared="${SHARED:-/home/ubuntu/OpenRealtime}"
logs="${repository}/.runtime/services"
mkdir -p "${logs}"

wait_for() {
  local name="$1" url="$2" seconds="${3:-600}"
  local waited=0
  until curl -s -m 2 -o /dev/null "${url}" 2>/dev/null; do
    sleep 3
    waited=$((waited + 3))
    if [ "${waited}" -ge "${seconds}" ]; then
      echo "${name} did not answer ${url} within ${seconds}s; see ${logs}/${name}.log" >&2
      return 1
    fi
  done
  echo "${name} answering after ${waited}s"
}

running() { ss -ltn 2>/dev/null | grep -q "127.0.0.1:$1"; }

# The decider and the voice. Vision matters: -fast-sees hands it frames, and a
# text-only checkpoint here makes the visual scenarios unwinnable.
if ! running 8000; then
  ( setsid nohup env VLLM_WORKER_MULTIPROC_METHOD=spawn \
      "${shared}/.runtime/qwen-asr/bin/python" -m vllm.entrypoints.openai.api_server \
        --model Qwen/Qwen3-VL-30B-A3B-Instruct-FP8 \
        --served-model-name qwen-fast \
        --host 127.0.0.1 --port 8000 \
        --gpu-memory-utilization 0.50 \
        --max-model-len 40960 \
        --enable-auto-tool-choice \
        --tool-call-parser hermes \
      > "${logs}/qwen.log" 2>&1 & )
fi

# The recogniser.
if ! running 8002; then
  ( cd "${shared}/.runtime/sensevoice-server" && setsid nohup \
      "${shared}/.runtime/sensevoice/bin/python" -m uvicorn server:app \
        --host 127.0.0.1 --port 8002 --workers 1 --log-level info \
      > "${logs}/sensevoice.log" 2>&1 & )
fi

# The synthesiser. Startup compiles CUDA graphs and warms the decoder, about
# twenty seconds, and anything arriving before that would pay for it.
if ! running 8123; then
  ( cd "${repository}/.runtime/fish/fish-speech" && setsid nohup \
      env PYTHONPATH="${repository}/.runtime/fish/fish-speech" \
      "${shared}/.runtime/fish-env/bin/python" "${repository}/tools/fish15/server.py" \
        --port 8123 --voices "${repository}/.runtime/fish-voices" \
      > "${logs}/fish.log" 2>&1 & )
fi

# Who is speaking.
if ! running 8124; then
  ( cd "${repository}" && setsid nohup \
      "${shared}/.runtime/sensevoice/bin/python" tools/speakerid/server.py --port 8124 \
      > "${logs}/speakerid.log" 2>&1 & )
fi

wait_for speaker-id http://127.0.0.1:8124/health 120
wait_for synthesiser http://127.0.0.1:8123/health 300
wait_for recogniser http://127.0.0.1:8002/docs 300
wait_for decider http://127.0.0.1:8000/health 900
echo "all four answering"
