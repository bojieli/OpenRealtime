#!/usr/bin/env bash
# Freeze-Omni (VITA-MLLM) native duplex engine: sidecar protocol v1 over TCP.
#
#   code     github.com/VITA-MLLM/Freeze-Omni @ 163a24880e533b2a07038fb8dcfe02dbbb8457e6
#            (.runtime/duplex-plan/src/Freeze-Omni, used unmodified)
#   weights  VITA-MLLM/Freeze-Omni @ c8b191874fee0f18fa6d207f26ca9843471726e2 (checkpoints/)
#            Qwen/Qwen2-7B-Instruct @ f2826a00ceef68f0f2b946d945ecc0477ce4450c (frozen backbone)
#   stack    torch 2.9.1+cu128, transformers 4.45.2 (upstream pins torch 2.2.0: no sm_120),
#            venv .runtime/duplex-plan/venvs/freezeomni (setup: .runtime/duplex-plan/setup-freezeomni.sh)
#   memory   ~17.4 GB resident on one GPU -> needs the large-model lease.
#
# The engine loads the weights once and serves one session at a time on
# tcp:127.0.0.1:$FREEZE_OMNI_PORT. It waits (without the lease) until the card
# has FREEZE_OMNI_NEED_MIB free, then holds the large-model lease (flock on
# .runtime/duplex-plan/gpu/large.lock) for exactly as long as it runs.
#
#   deploy/duplex/services/freeze-omni.sh start     # waits for the lease, then for readiness
#   deploy/duplex/services/freeze-omni.sh stop
#   deploy/duplex/services/freeze-omni.sh status
#
# Runtime side (per-session stdio relay, or connect directly):
#   openrealtime serve -binding duplex \
#     -sidecar "$VENV/bin/python sidecars/freeze_omni_sidecar.py --engine tcp:127.0.0.1:9141" ...
#   openrealtime serve -binding duplex -sidecar-address tcp:127.0.0.1:9141 ...
set -euo pipefail

ROOT=/home/ubuntu/OpenRealtime
PLAN=$ROOT/.runtime/duplex-plan
PORT=${FREEZE_OMNI_PORT:-9141}
VENV=${FREEZE_OMNI_VENV:-$PLAN/venvs/freezeomni}
SRC=${FREEZE_OMNI_SRC:-$PLAN/src/Freeze-Omni}
PIDFILE=$PLAN/pids/freeze-omni.pid
LOG=$PLAN/logs/freeze-omni.log
CONTROL_LOG=${FREEZE_OMNI_CONTROL_LOG:-$PLAN/logs/freeze-omni-control.jsonl}
LEASE_WAIT=${LEASE_WAIT:-14400}

running() { [[ -f $PIDFILE ]] && kill -0 "$(cat "$PIDFILE")" 2>/dev/null; }
# Readiness is the engine's own log line: a probe connection would open a session.
ready() { grep -q "serving sidecar sessions on tcp:127.0.0.1:$PORT" "$LOG" 2>/dev/null; }

case ${1:-status} in
  start)
    if running; then echo "already running (pid $(cat "$PIDFILE"))"; exit 0; fi
    mkdir -p "$PLAN/pids" "$PLAN/logs"
    cd "$ROOT"
    rm -f "$PIDFILE"
    # The launcher waits for GPU memory *without* the lease and takes it only
    # when the model can actually fit: holding the lease while waiting for
    # memory blocks every other large model behind a job that cannot start.
    # The lease then lives on fd 9, inherited by the engine, for exactly as
    # long as the engine runs. The recorded PID leads the process group.
    PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True HF_HUB_OFFLINE=1 setsid nohup \
      bash -c 'echo $$ > "$0"
        need=$1; lock=$2; shift 2; waited=0
        free_mib() {
          local raw
          raw=$(timeout 10 nvidia-smi --query-gpu=memory.used,memory.total --format=csv,noheader,nounits 2>&1 | head -1 | tr -d ",")
          read -r used total <<< "$raw"
          if [[ ! $used =~ ^[0-9]+$ || ! $total =~ ^[0-9]+$ ]]; then
            echo "nvidia-smi gave no usable reading: ${raw:-<empty/timeout>}" >&2
            used=0 total=0
            return 1
          fi
          return 0
        }
        while true; do
          if free_mib && (( total - used >= need )); then
            exec 9> "$lock"
            if flock -w 30 9; then
              if free_mib && (( total - used >= need )); then break; fi
              echo "lease taken but only $((total - used)) MiB free; releasing it again"
              exec 9>&-
            else
              echo "$((total - used)) MiB free; waiting for the lease (held by another job)"
              exec 9>&-
            fi
          fi
          (( waited % 60 == 0 )) && echo "waiting (no lease) for ${need} MiB free (now $((total - used)) MiB)"
          (( waited >= 14400 )) && { echo "gave up waiting for GPU memory" >&2; exit 1; }
          sleep 5; waited=$((waited + 5))
        done
        echo "lease acquired; starting with $((total - used)) MiB free"
        exec "$@"' \
      "$PIDFILE" "${FREEZE_OMNI_NEED_MIB:-19000}" "$PLAN/gpu/large.lock" \
      "$VENV/bin/python" sidecars/freeze_omni_sidecar.py --serve --listen "127.0.0.1:$PORT" \
        --src "$SRC" --control-log "$CONTROL_LOG" \
      > "$LOG" 2>&1 < /dev/null &
    for _ in $(seq 1 50); do [[ -s $PIDFILE ]] && break; sleep 0.1; done
    for _ in $(seq 1 $((LEASE_WAIT / 2 + 150))); do
      if ready; then echo "ready on tcp:127.0.0.1:$PORT (pid $(cat "$PIDFILE"))"; exit 0; fi
      running || { echo "exited; see $LOG"; exit 1; }
      sleep 2
    done
    echo "not ready; see $LOG"; exit 1 ;;
  stop)
    if running; then
      pid=$(cat "$PIDFILE")
      # flock leads its own process group (setsid): signal the group.
      kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
      for _ in $(seq 1 30); do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
      echo stopped
    else echo "not running"; fi
    rm -f "$PIDFILE" ;;
  status)
    if running && ready; then echo "serving on tcp:127.0.0.1:$PORT (pid $(cat "$PIDFILE"))"
    elif running; then echo "starting or waiting for the lease (pid $(cat "$PIDFILE"))"
    else echo "not running"; exit 1; fi ;;
  *) echo "usage: $0 start|stop|status"; exit 2 ;;
esac
