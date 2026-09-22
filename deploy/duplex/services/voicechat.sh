#!/usr/bin/env bash
# NemotronLabs VoiceChat 11B native full-duplex engine (plan cell N2).
#
# Pinned serving route: vLLM-Omni native duplex Realtime endpoint
# (`/v1/realtime?duplex=1`, recipes/NVIDIA/NemotronLabs-VoiceChat.md), the
# 3-stage thinker/talker/code2wav pipeline with an 80 ms frame clock and the
# model's function channel surfaced as Realtime function-call events.
#
#   vLLM-Omni  9ebef4b1d3cb69181bcc2eb159cc6ef35f272b6f (src/vllm-omni-native)
#   vLLM       0.29.0 (torch 2.13.0+cu130), venv .runtime/duplex-plan/venvs/vllm-omni-native
#   weights    nvidia/NVIDIA-NemotronLabs-VoiceChat-11B @ a4c40ca5b4fe77db13e9840ca4a2b91becf030c8
#   tokenizer  nvidia/NVIDIA-Nemotron-Nano-9B-v2 @ 6533e8de2c68e4536bf7c411d7a3ce5734111476 (tokenizer files only)
#
# The shipped duplex yaml asks for 0.62/0.12/0.06 of the GPU (~78 GB). This
# host shares its GPU, so an overlay keeps the upstream fast profile and only
# shrinks the per-stage memory budgets (~31 GB total).
#
# The engine is a large model: it runs under the GPU large-model lease and
# holds it for exactly as long as the server lives.
#
#   deploy/duplex/services/voicechat.sh start    # background, PID in .runtime/duplex-plan/pids/voicechat.pid
#   deploy/duplex/services/voicechat.sh stop
#   deploy/duplex/services/voicechat.sh status
#
# The OpenRealtime sidecar is a thin per-session client of this server:
#   openrealtime serve -binding duplex \
#     -sidecar ".runtime/duplex-plan/venvs/vllm-omni-native/bin/python sidecars/voicechat_sidecar.py --server ws://127.0.0.1:9140/v1/realtime" ...
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
PORT=${VOICECHAT_PORT:-9140}
VENV=${VOICECHAT_VENV:-$PLAN/venvs/vllm-omni-native}
SRC=${VOICECHAT_VLLM_OMNI:-$PLAN/src/vllm-omni-native}
CHECKPOINT=${VOICECHAT_CHECKPOINT:-$HOME/.cache/huggingface/hub/models--nvidia--NVIDIA-NemotronLabs-VoiceChat-11B/snapshots/a4c40ca5b4fe77db13e9840ca4a2b91becf030c8}
TOKENIZER=${NEMOTRON_VOICECHAT_LLM_PATH:-$HOME/.cache/huggingface/hub/models--nvidia--NVIDIA-Nemotron-Nano-9B-v2/snapshots/6533e8de2c68e4536bf7c411d7a3ce5734111476}
THINKER_MEM=${VOICECHAT_THINKER_MEM:-0.245}
TALKER_MEM=${VOICECHAT_TALKER_MEM:-0.055}
CODEC_MEM=${VOICECHAT_CODEC_MEM:-0.025}
THINKER_KV_BYTES=${VOICECHAT_THINKER_KV_BYTES:-1610612736}
TALKER_KV_BYTES=${VOICECHAT_TALKER_KV_BYTES:-1073741824}
PIDFILE=$PLAN/pids/voicechat.pid
LOG=$PLAN/logs/voicechat.log
OVERLAY=$PLAN/configs/voicechat-duplex-shared-gpu.yaml
BALLAST=$PLAN/configs/voicechat-ballast.py
BALLAST_FIRST_GIB=${VOICECHAT_BALLAST_FIRST_GIB:-6}
BALLAST_SECOND_GIB=${VOICECHAT_BALLAST_SECOND_GIB:-3}
ATTEMPTS=${VOICECHAT_ATTEMPTS:-4}

write_ballast() {
  mkdir -p "$(dirname "$BALLAST")"
  cat > "$BALLAST" <<'PY'
"""Hold GPU memory for the later pipeline stages while stage 0 loads.

The three engine stages start one after another over about a minute. On a
shared GPU the memory checked before the first stage is gone by the time the
talker and codec ask for theirs - five other processes claimed 20 GiB inside
one such window - and the deployment fails after paying for the 20 GiB
thinker load. This holds their share until stage 0 has taken its own, then
releases it in the order the stages need it.
"""
import sys, time, torch

log_path, first_gib, second_gib, timeout_s = sys.argv[1], float(sys.argv[2]), float(sys.argv[3]), float(sys.argv[4])
blocks = []


def hold(target_gib):
    while len(blocks) < int(target_gib):
        try:
            blocks.append(torch.empty(1 << 30, dtype=torch.uint8, device="cuda"))
        except torch.OutOfMemoryError:
            break
    print(f"ballast holding {len(blocks)} GiB", flush=True)


def release_to(keep_gib):
    while len(blocks) > int(keep_gib):
        blocks.pop()
    torch.cuda.empty_cache()
    print(f"ballast holding {len(blocks)} GiB", flush=True)


def count(marker):
    try:
        with open(log_path, "r", errors="ignore") as handle:
            return sum(marker in line for line in handle)
    except FileNotFoundError:
        return 0


def wait_for(marker, occurrence, deadline):
    while time.monotonic() < deadline:
        try:
            with open(log_path, "r", errors="ignore") as handle:
                if sum(marker in line for line in handle) >= occurrence:
                    return True
        except FileNotFoundError:
            pass
        time.sleep(1)
    return False


# A retry writes into the same log, so every marker is counted from where
# this attempt starts rather than from the beginning of the file.
base_kv, base_load = count("GPU KV cache size"), count("Model loading took")
hold(first_gib + second_gib)
deadline = time.monotonic() + timeout_s
# Stage 0 has taken its weights and paged KV once it prints its cache size.
wait_for("GPU KV cache size", base_kv + 1, deadline)
release_to(second_gib)
# Stage 1 (talker) has its weights once the second stage reports a load.
wait_for("Model loading took", base_load + 2, deadline)
release_to(0)
PY
}

write_overlay() {
  mkdir -p "$(dirname "$OVERLAY")"
  cat > "$OVERLAY" <<EOF
# Generated by deploy/duplex/services/voicechat.sh - do not edit.
base_config: $SRC/vllm_omni/deploy/nemotron_labs_voicechat_duplex.yaml
stages:
  - stage_id: 0
    gpu_memory_utilization: $THINKER_MEM
    # The duplex prompt (instructions + at most five tools) is far below this;
    # a smaller prefill budget shrinks the activation peak on a shared GPU.
    max_num_batched_tokens: 2048
    # Size the paged KV/Mamba cache explicitly. Left to profiling on a shared
    # GPU it claims ~6.8 GiB (197k tokens) for one 8k-token session and
    # starves the talker, which then fails to load.
    kv_cache_memory_bytes: $THINKER_KV_BYTES
  - stage_id: 1
    gpu_memory_utilization: $TALKER_MEM
    kv_cache_memory_bytes: $TALKER_KV_BYTES
  - stage_id: 2
    gpu_memory_utilization: $CODEC_MEM
EOF
}

start() {
  if [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "voicechat already running (pid $(cat "$PIDFILE"))"; return 0
  fi
  write_overlay
  write_ballast
  mkdir -p "$PLAN/pids" "$PLAN/logs"
  # The lease is taken first; the engine then waits (holding it) until the
  # shared GPU has room, rather than failing and handing the lease on.
  setsid nohup flock -w 14400 "$PLAN/gpu/large.lock" "$ROOT/deploy/duplex/services/voicechat.sh" _run \
    > "$LOG" 2>&1 < /dev/null &
  echo $! > "$PIDFILE"
  echo "voicechat starting (pid $(cat "$PIDFILE"), log $LOG, port $PORT)"
}

_run() {
  local attempt=1
  while (( attempt <= ATTEMPTS )); do
    echo "=== attempt $attempt of $ATTEMPTS"
    _wait_for_memory || return 1
    "$VENV/bin/python" "$BALLAST" "$LOG" "$BALLAST_FIRST_GIB" "$BALLAST_SECOND_GIB" 900 &
    local ballast=$!
    _serve
    kill "$ballast" 2>/dev/null || true
    attempt=$((attempt + 1))
    echo "=== engine exited; retrying under the same lease"
    sleep 10
  done
  echo "gave up after $ATTEMPTS attempts" >&2
  return 1
}

_wait_for_memory() {
  local need=${VOICECHAT_NEED_MIB:-30000} waited=0
  while true; do
    local used total
    read -r used total < <(nvidia-smi --query-gpu=memory.used,memory.total --format=csv,noheader,nounits | tr -d ',')
    if (( total - used >= need )); then break; fi
    if (( waited % 60 == 0 )); then echo "lease held; waiting for ${need} MiB free (now $((total - used)) MiB)"; fi
    if (( waited >= ${VOICECHAT_MEMORY_WAIT_S:-3600} )); then echo "gave up waiting for GPU memory" >&2; return 1; fi
    sleep 5; waited=$((waited + 5))
  done
  echo "starting engine with $((total - used)) MiB free"
}

_serve() {
  cd "$SRC"
  # HF_HUB_OFFLINE: the weights and tokenizer are pinned in the local cache.
  # FlashInfer normalises sm_120 against the system CUDA toolkit (12.8 here)
  # and raises "SM 12.x requires CUDA >= 12.9"; its import-time probe swallows
  # that and later fails as "FlashInfer requires GPUs with sm75 or higher".
  # An explicit suffixed arch skips the normalisation, and vLLM's sampler is
  # kept on its PyTorch path (decoding is greedy, so the choice is moot).
  export FLASHINFER_CUDA_ARCH_LIST=12.0a VLLM_USE_FLASHINFER_SAMPLER=0
  export HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1
  export NEMOTRON_VOICECHAT_LLM_PATH=$TOKENIZER VLLM_WORKER_MULTIPROC_METHOD=spawn
  "$VENV/bin/vllm-omni" serve "$CHECKPOINT" \
      --omni --host 127.0.0.1 --port "$PORT" \
      --served-model-name nemotron-voicechat \
      --deploy-config "$OVERLAY"
}

stop() {
  [[ -f $PIDFILE ]] || { echo "voicechat not running"; return 0; }
  local pid; pid=$(cat "$PIDFILE")
  # setsid made the flock wrapper a session leader: signal the whole group so
  # the three engine-core processes go with it and the lease is released.
  kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 60); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
  kill -KILL -- "-$pid" 2>/dev/null || true
  rm -f "$PIDFILE"
  echo "voicechat stopped"
}

status() {
  if [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; then
    echo "running pid $(cat "$PIDFILE")"
    curl -s -m 3 "http://127.0.0.1:$PORT/health" && echo
  else
    echo "not running"
  fi
}

case "${1:-start}" in
  start) start ;;
  _run) _run ;;
  stop) stop ;;
  status) status ;;
  *) echo "usage: $0 start|stop|status" >&2; exit 2 ;;
esac
