#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime_dir="${repository_root}/.runtime/local-cascade"
log_dir="${runtime_dir}/logs"
pid_dir="${runtime_dir}/pids"
action="${1:-status}"

mkdir -p "${log_dir}" "${pid_dir}" "${runtime_dir}/bin"

health() {
  curl --fail --silent --max-time 3 "$1" >/dev/null 2>&1
}

wait_for_health() {
  local name="$1"
  local url="$2"
  local pid="$3"
  local attempt
  for attempt in $(seq 1 180); do
    if health "${url}"; then
      echo "${name} ready at ${url}"
      return 0
    fi
    if ! kill -0 "${pid}" 2>/dev/null; then
      echo "${name} exited before becoming ready; see ${log_dir}/${name}.log" >&2
      tail -80 "${log_dir}/${name}.log" >&2 || true
      return 1
    fi
    if (( attempt % 6 == 0 )); then
      echo "waiting for ${name} (${attempt}/180)"
    fi
    sleep 5
  done
  echo "${name} did not become ready within 15 minutes" >&2
  return 1
}

start_process() {
  local name="$1"
  local url="$2"
  shift 2
  local pid_file="${pid_dir}/${name}.pid"
  if health "${url}"; then
    echo "${name} already healthy at ${url}"
    return 0
  fi
  if [[ -f "${pid_file}" ]]; then
    local old_pid
    old_pid="$(<"${pid_file}")"
    if kill -0 "${old_pid}" 2>/dev/null; then
      echo "${name} process ${old_pid} exists but is not healthy" >&2
      return 1
    fi
  fi
  nohup setsid "$@" >"${log_dir}/${name}.log" 2>&1 < /dev/null &
  local pid=$!
  echo "${pid}" >"${pid_file}"
  echo "started ${name} as process group ${pid}"
  wait_for_health "${name}" "${url}" "${pid}"
}

stop_process() {
  local name="$1"
  local pid_file="${pid_dir}/${name}.pid"
  if [[ ! -f "${pid_file}" ]]; then
    return 0
  fi
  local pid
  pid="$(<"${pid_file}")"
  if ! [[ "${pid}" =~ ^[1-9][0-9]*$ ]]; then
    echo "invalid PID file for ${name}: ${pid_file}" >&2
    return 1
  fi
  if kill -0 "${pid}" 2>/dev/null; then
    kill -TERM -- "-${pid}" 2>/dev/null || kill -TERM "${pid}" 2>/dev/null || true
    for _ in $(seq 1 30); do
      if ! kill -0 "${pid}" 2>/dev/null; then
        break
      fi
      sleep 1
    done
    if kill -0 "${pid}" 2>/dev/null; then
      kill -KILL -- "-${pid}" 2>/dev/null || kill -KILL "${pid}" 2>/dev/null || true
    fi
  fi
  rm -f "${pid_file}"
  echo "stopped ${name}"
}

status_process() {
  local name="$1"
  local url="$2"
  local pid_file="${pid_dir}/${name}.pid"
  local pid="none"
  if [[ -f "${pid_file}" ]]; then
    pid="$(<"${pid_file}")"
  fi
  if health "${url}"; then
    echo "${name}: healthy (pid ${pid}, ${url})"
  else
    echo "${name}: unavailable (pid ${pid}, ${url})"
  fi
}

start_asr() {
  local model="${OPENREALTIME_ASR_MODEL:-Qwen/Qwen3-ASR-0.6B}"
  local memory_utilization="${OPENREALTIME_ASR_GPU_MEMORY_UTILIZATION:-0.14}"
  local chunk_seconds="${OPENREALTIME_ASR_CHUNK_SECONDS:-0.2}"
  start_process asr http://127.0.0.1:8001/ \
    env VLLM_WORKER_MULTIPROC_METHOD=spawn \
    "${repository_root}/.runtime/qwen-asr/bin/python" -m qwen_asr.cli.demo_streaming \
      --asr-model-path "${model}" \
      --host 127.0.0.1 --port 8001 \
      --gpu-memory-utilization "${memory_utilization}" \
      --chunk-size-sec "${chunk_seconds}"
}

start_fast() {
  start_process qwen http://127.0.0.1:8000/health \
    env VLLM_WORKER_MULTIPROC_METHOD=spawn \
    "${repository_root}/.runtime/qwen-asr/bin/python" -m vllm.entrypoints.openai.api_server \
      --model Qwen/Qwen3-30B-A3B-FP8 \
      --revision d206ba732169f29bb77fbf80fc2c4b81d4d30782 \
      --served-model-name qwen-fast \
      --host 127.0.0.1 --port 8000 \
      --gpu-memory-utilization 0.38 \
      --max-model-len 40960 \
      --enable-auto-tool-choice \
      --tool-call-parser hermes
}

case "${action}" in
  start)
    : "${OPENREALTIME_GATEWAY_TOKEN:?OPENREALTIME_GATEWAY_TOKEN must be set}"
    : "${GEMINI_API_KEY:?GEMINI_API_KEY must be set}"
    fast_provider="${OPENREALTIME_FAST_PROVIDER:-vllm}"
    asr_model="${OPENREALTIME_ASR_MODEL:-Qwen/Qwen3-ASR-0.6B}"
    if [[ "${fast_provider}" != vllm && "${fast_provider}" != gemini ]]; then
      echo "OPENREALTIME_FAST_PROVIDER must be vllm or gemini" >&2
      exit 1
    fi
    gateway_binary="${OPENREALTIME_GATEWAY_BINARY:-}"
    gateway_sha256="${OPENREALTIME_GATEWAY_SHA256:-}"
    if [[ -n "${gateway_binary}" ]]; then
      if [[ "${gateway_binary}" != /* || ! -x "${gateway_binary}" ]]; then
        echo "OPENREALTIME_GATEWAY_BINARY must name an absolute executable file" >&2
        exit 1
      fi
      if [[ ! "${gateway_sha256}" =~ ^[0-9a-f]{64}$ ]]; then
        echo "OPENREALTIME_GATEWAY_SHA256 must pin the supplied gateway binary" >&2
        exit 1
      fi
      actual_gateway_sha256="$(sha256sum "${gateway_binary}" | cut -d ' ' -f 1)"
      if [[ "${actual_gateway_sha256}" != "${gateway_sha256}" ]]; then
        echo "frozen gateway hash mismatch: expected ${gateway_sha256}, found ${actual_gateway_sha256}" >&2
        exit 1
      fi
      if health http://127.0.0.1:8765/healthz && [[ -f "${pid_dir}/gateway.pid" ]]; then
        live_gateway_pid="$(<"${pid_dir}/gateway.pid")"
        if [[ "${live_gateway_pid}" =~ ^[1-9][0-9]*$ ]] && kill -0 "${live_gateway_pid}" 2>/dev/null; then
          live_gateway_sha256="$(sha256sum "/proc/${live_gateway_pid}/exe" | cut -d ' ' -f 1)"
          if [[ "${live_gateway_sha256}" != "${gateway_sha256}" ]]; then
            echo "healthy gateway ${live_gateway_pid} is not the requested frozen binary" >&2
            exit 1
          fi
        fi
      fi
    else
      gateway_binary="${runtime_dir}/bin/realtimegateway"
      /usr/local/go/bin/go build -o "${gateway_binary}" ./cmd/realtimegateway
    fi

    cuda_home="${repository_root}/.runtime/sglang-omni/lib/python3.12/site-packages/nvidia/cu13"
    start_process fish http://127.0.0.1:8081/health \
      env \
        CUDA_HOME="${cuda_home}" \
        LD_LIBRARY_PATH="${cuda_home}/lib:${repository_root}/.runtime/sglang-omni/lib/python3.12/site-packages/torch/lib" \
        FLASHINFER_WORKSPACE_BASE="${repository_root}/.runtime/flashinfer-absolute" \
      "${repository_root}/.runtime/sglang-omni/bin/sgl-omni" serve \
        --config "${repository_root}/deploy/sglang-omni/s2pro-colocated-96gb.yaml" \
        --host 127.0.0.1 --port 8081 \
        --model-name fishaudio/s2-pro

    if [[ "${fast_provider}" == vllm ]]; then
      start_fast
    fi

    start_asr

    start_process gateway http://127.0.0.1:8765/healthz \
      env \
        OPENREALTIME_GATEWAY_TOKEN="${OPENREALTIME_GATEWAY_TOKEN}" \
        GEMINI_API_KEY="${GEMINI_API_KEY}" \
      "${gateway_binary}" \
        --asr-model "${asr_model}" \
        --asr-provider-chunk "${OPENREALTIME_ASR_PROVIDER_CHUNK:-200ms}" \
        --asr-provider-max-chunk "${OPENREALTIME_ASR_PROVIDER_MAX_CHUNK:-0s}" \
        --fast-provider "${fast_provider}" \
        --fast-model "${OPENREALTIME_FAST_MODEL:-}" \
        --fast-endpoint "${OPENREALTIME_FAST_ENDPOINT:-}" \
        --slow-effort "${OPENREALTIME_SLOW_EFFORT:-high}" \
        --preparation-policy "${OPENREALTIME_PREPARATION_POLICY:-continuous}" \
        --slow-context "${OPENREALTIME_SLOW_CONTEXT_POLICY:-canonical}"
    ;;
  stop)
    stop_process gateway
    stop_process asr
    stop_process qwen
    stop_process fish
    ;;
  restart-asr)
    stop_process asr
    start_asr
    ;;
  restart-fast)
    stop_process qwen
    start_fast
    ;;
  status)
    status_process fish http://127.0.0.1:8081/health
    status_process qwen http://127.0.0.1:8000/health
    status_process asr http://127.0.0.1:8001/
    status_process gateway http://127.0.0.1:8765/healthz
    ;;
  *)
    echo "usage: scripts/local-cascade.sh {start|stop|restart-asr|restart-fast|status}" >&2
    exit 2
    ;;
esac
