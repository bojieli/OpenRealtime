#!/usr/bin/env bash
# Lychee-FD native full-duplex engine (plan stage P5, cell N3): upstream
# realtime backend (lychee_fd.app, patched vLLM 0.6.5 multi-stream serving)
# plus the Step-Audio-2-mini Token2Wav server, both on one GPU - the upstream
# README's documented single-GPU configuration
# (TOKEN2WAV_CUDA_VISIBLE_DEVICES=0, BACKEND_CUDA_VISIBLE_DEVICES=0, a reduced
# LYCHEEFD_VLLM_GPU_MEMORY_UTILIZATION).
#
#   code     github.com/HITsz-TMG/Lychee-FD @ 7bb1068c59b578e1f8e65fb762cd26708c14cc6d
#   weights  HIT-TMG/Lychee-FD @ ac06087bef70b33d4dfebf63a3eadadb4e024f33 (12.8B params, 25.6 GB bf16)
#            stepfun-ai/Step-Audio-2-mini @ e36fdd5d71e0ea22f09dd94bbab9bfc544ca1e36 (token2wav/ only)
#   stack    upstream pins torch 2.5.1+cu124 / vLLM 0.6.5 / xformers 0.0.28.post3 / flash-attn 2.8.2,
#            which has no sm_120 kernels ("no kernel image is available for execution on the device").
#            Here: torch 2.7.1+cu128, xformers 0.0.31, the vendored patched vLLM 0.6.5 rebuilt from
#            source for sm_120 (.runtime/duplex-plan/build/lychee-vllm, patch
#            .runtime/duplex-plan/build/lychee-vllm-sm120.patch: arch list + no vllm-flash-attn),
#            attention through the XFORMERS backend. venv .runtime/duplex-plan/venvs/lychee
#            (.runtime/duplex-plan/setup-lychee.sh, then build/build-lychee-vllm.sh).
#
# Large model: both processes run under the GPU large-model lease, which is
# taken first; the launcher then waits (holding it) for enough free memory.
#
#   deploy/duplex/services/lychee-fd.sh start     # background; PID in .runtime/duplex-plan/pids/lychee-fd.pid
#   deploy/duplex/services/lychee-fd.sh stop
#   deploy/duplex/services/lychee-fd.sh status
#
# Runtime side (the engine keeps the acoustic floor; the model owns interaction):
#   openrealtime serve -binding duplex -floor engine \
#     -sidecar "$VENV/bin/python sidecars/lychee_fd_sidecar.py --backend http://127.0.0.1:9143" ...
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
PORT=${LYCHEE_FD_PORT:-9143}
T2W_PORT=${LYCHEE_FD_T2W_PORT:-9144}
VENV=${LYCHEE_FD_VENV:-$PLAN/venvs/lychee}
SRC=${LYCHEE_FD_SRC:-$PLAN/src/Lychee-FD}
VLLM_TREE=${LYCHEE_FD_VLLM_TREE:-$PLAN/build/lychee-vllm}
MODEL=${LYCHEE_FD_MODEL:-$HOME/.cache/huggingface/hub/models--HIT-TMG--Lychee-FD/snapshots/ac06087bef70b33d4dfebf63a3eadadb4e024f33}
T2W=${LYCHEE_FD_T2W:-$HOME/.cache/huggingface/hub/models--stepfun-ai--Step-Audio-2-mini/snapshots/e36fdd5d71e0ea22f09dd94bbab9bfc544ca1e36/token2wav}
# Fraction of the whole 96 GB card vLLM may take: weights 25.6 GB + activations + KV.
# 0.31 = ~30 GB, leaving ~3 GB of KV (8k tokens need ~0.7 GB).
GPU_UTIL=${LYCHEE_FD_GPU_UTIL:-0.31}
MAX_MODEL_LEN=${LYCHEE_FD_MAX_MODEL_LEN:-8192}
NEED_MIB=${LYCHEE_FD_NEED_MIB:-34000}
PIDFILE=$PLAN/pids/lychee-fd.pid
LOG=$PLAN/logs/lychee-fd.log
RUNTIME_LOGS=$PLAN/logs/lychee-fd-runtime

running() { [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }
ready() { curl -sf -m 2 "http://127.0.0.1:$PORT/api/realtime/voices" > /dev/null 2>&1; }

start() {
  if running; then echo "lychee-fd already running (pid $(cat "$PIDFILE"))"; return 0; fi
  mkdir -p "$PLAN/pids" "$PLAN/logs" "$RUNTIME_LOGS"
  rm -f "$PIDFILE"
  # The recorded PID is flock's and it leads its own process group, so stop
  # can signal the whole group (flock, the launcher, both servers).
  setsid nohup bash -c 'echo $$ > "$0"; exec flock -w "$1" "$2" "$3" _run' \
    "$PIDFILE" "${LEASE_WAIT:-14400}" "$PLAN/gpu/large.lock" "$ROOT/deploy/duplex/services/lychee-fd.sh" \
    > "$LOG" 2>&1 < /dev/null &
  for _ in $(seq 1 50); do [[ -s $PIDFILE ]] && break; sleep 0.1; done
  echo "lychee-fd starting (pid $(cat "$PIDFILE"), log $LOG, port $PORT)"
}

_run() {
  local waited=0 used total
  while true; do
    read -r used total < <(nvidia-smi --query-gpu=memory.used,memory.total --format=csv,noheader,nounits | tr -d ',')
    if (( total - used >= NEED_MIB )); then break; fi
    if (( waited % 60 == 0 )); then echo "lease held; waiting for ${NEED_MIB} MiB free (now $((total - used)) MiB)"; fi
    if (( waited >= ${LYCHEE_FD_MEMORY_WAIT_S:-3600} )); then echo "gave up waiting for GPU memory" >&2; exit 1; fi
    sleep 5; waited=$((waited + 5))
  done
  echo "starting with $((total - used)) MiB free"
  export CUDA_VISIBLE_DEVICES=0 PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True
  export HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 TOKENIZERS_PARALLELISM=false
  export NUMBA_CACHE_DIR=$PLAN/logs/lychee-fd-numba OMP_NUM_THREADS=8 MKL_NUM_THREADS=8
  cd "$SRC"

  # 1. Token2Wav server (upstream scripts/start_token2wav_server.sh, same GPU).
  PYTHONPATH="$SRC/third_party/Step-Audio2:$SRC" "$VENV/bin/python" -u -m lychee_fd.token2wav_server \
    --host 127.0.0.1 --port "$T2W_PORT" --model-path "$T2W" > "$PLAN/logs/lychee-fd-token2wav.log" 2>&1 &
  local t2w=$!
  trap 'kill $t2w 2>/dev/null || true' EXIT
  local t2w_ready=0
  for _ in $(seq 1 180); do
    if curl -sf -m 2 -X POST -H 'Content-Type: application/json' -d '{}' \
        "http://127.0.0.1:$T2W_PORT/v1/token2wav/health" | grep -q '"ok":true'; then t2w_ready=1; break; fi
    kill -0 $t2w 2>/dev/null || { echo "token2wav exited; see $PLAN/logs/lychee-fd-token2wav.log" >&2; exit 1; }
    sleep 2
  done
  (( t2w_ready )) || { echo "token2wav not ready after 360 s" >&2; exit 1; }
  echo "token2wav ready on :$T2W_PORT"

  # 2. Realtime backend (upstream scripts/start_backend.sh stable vllm, README env).
  # The rebuilt patched vLLM tree goes first on PYTHONPATH; its native ops
  # were compiled in place, so the upstream "sync from installed wheel" step
  # does not apply.
  export PYTHONPATH="$VLLM_TREE:$SRC:$SRC/third_party/Step-Audio2"
  export VLLM_ATTENTION_BACKEND=XFORMERS
  export LYCHEEFD_USE_VLLM=1 LYCHEEFD_REALTIME_INCREMENTAL_BACKEND=auto
  export LYCHEEFD_REALTIME_STRICT_INFER_WINDOW=1 LYCHEEFD_STOKEN_DELAY_NUM=10
  export LYCHEEFD_TTS_VOCODER_HOP_SIZE=10 LYCHEEFD_T2W_STREAM_LOOKAHEAD_LEN=3
  export LYCHEEFD_T2W_REMOTE_ENABLED=1 LYCHEEFD_T2W_REMOTE_URL="http://127.0.0.1:$T2W_PORT" LYCHEEFD_T2W_REMOTE_FALLBACK=0
  export LYCHEEFD_VLLM_MAX_MODEL_LEN=$MAX_MODEL_LEN LYCHEEFD_VLLM_GPU_MEMORY_UTILIZATION=$GPU_UTIL
  export LYCHEEFD_VLLM_ENFORCE_EAGER=1 LYCHEEFD_VLLM_ENABLE_CHUNKED_PREFILL=1 LYCHEEFD_VLLM_ENABLE_PREFIX_CACHING=0
  export LYCHEEFD_VLLM_KEEP_ALIVE_SPEAKING=1 LYCHEEFD_VLLM_IGNORE_TEXT_EOS=1
  export LYCHEEFD_VLLM_SIDE_SUBSET_PROJ=1 LYCHEEFD_VLLM_SIDE_SUBSET_REQUIRE_PROCESSOR=1 LYCHEEFD_VLLM_SIDE_PARALLEL_BRANCH=0
  export LYCHEEFD_VLLM_MAX_NUM_SEQS=1 LYCHEEFD_VLLM_MAX_NUM_BATCHED_TOKENS=1024
  export LYCHEEFD_REALTIME_INFER_WINDOW_MS=400 LYCHEEFD_REALTIME_INFER_WINDOW_MIN_MS=160 LYCHEEFD_WINDOW_SECOND=0.4
  export LYCHEEFD_STOKEN_NO_REPEAT_NGRAM=4 LYCHEEFD_REALTIME_TTS_CHUNK_SIZE=1 LYCHEEFD_REALTIME_TRUE_INCREMENTAL_AUDIO=1
  export LYCHEEFD_STARTUP_TOKEN2WAV_FIRST=1 LYCHEEFD_STARTUP_WARMUP=1 LYCHEEFD_STARTUP_WARMUP_TOKEN=100
  export LYCHEEFD_REALTIME_STAGE_TIMING_LOG=1 LYCHEEFD_REALTIME_CONTROL_PROB_TRACE_LOG=1
  export LYCHEEFD_RUNTIME_LOG_DIR=$RUNTIME_LOGS
  export LYCHEEFD_REALTIME_STAGE_TIMING_LOG_DIR=$RUNTIME_LOGS/stage_timing
  export LYCHEEFD_REALTIME_CONTROL_PROB_TRACE_LOG_DIR=$RUNTIME_LOGS/control_prob
  export LYCHEEFD_FORCE_DISABLE_PROXY=1 NO_PROXY=127.0.0.1,localhost no_proxy=127.0.0.1,localhost
  unset http_proxy https_proxy HTTP_PROXY HTTPS_PROXY all_proxy ALL_PROXY
  "$VENV/bin/python" -c 'import vllm, sys; print("[lychee-fd] resolved_vllm=" + vllm.__file__, file=sys.stderr)'
  "$VENV/bin/python" -u -m lychee_fd.app --model_path "$MODEL" --token2wav_path "$T2W" \
    --server_name 127.0.0.1 --server_port "$PORT" --no-share
}

stop() {
  [[ -f $PIDFILE ]] || { echo "lychee-fd not running"; return 0; }
  local pid; pid=$(cat "$PIDFILE")
  kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 60); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill -KILL -- "-$pid" 2>/dev/null || true
  rm -f "$PIDFILE"
  echo "lychee-fd stopped"
}

case ${1:-status} in
  start) start ;;
  _run) _run ;;
  stop) stop ;;
  status)
    if running && ready; then echo "serving on http://127.0.0.1:$PORT (pid $(cat "$PIDFILE"))"
    elif running; then echo "starting or waiting for the lease (pid $(cat "$PIDFILE")); see $LOG"
    else echo "not running"; exit 1; fi ;;
  *) echo "usage: $0 start|stop|status"; exit 2 ;;
esac
