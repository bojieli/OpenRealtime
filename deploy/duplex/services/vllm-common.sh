#!/usr/bin/env bash
# Shared start/stop/status for the vLLM-served duplex-plan services. Sourced by
# llm-qwen3-8b.sh, asr-voxtral.sh and asr-qwen3.sh; each sets NAME, PORT,
# HEALTH and a COMMAND array before calling vllm_service "$@".
ROOT=${ROOT:-/home/ubuntu/OpenRealtime}
PLAN=$ROOT/.runtime/duplex-plan
# vLLM 0.19 (torch 2.10 cu128) from the Qwen-ASR runtime; it serves the
# realtime transcription route as well as chat.
VLLM_PYTHON=${VLLM_PYTHON:-$ROOT/.runtime/qwen-asr/bin/python}

snapshot() {
  local repository="$HOME/.cache/huggingface/hub/models--${1//\//--}"
  local revision
  revision="$(tr -d '[:space:]' < "$repository/refs/main")"
  [[ "$revision" =~ ^[0-9a-f]{40}$ && -d "$repository/snapshots/$revision" ]] ||
    { echo "no immutable snapshot of $1; hf download $1" >&2; return 1; }
  printf '%s\n' "$repository/snapshots/$revision"
}

vllm_service() {
  local pidfile=$PLAN/pids/$NAME.pid log=$PLAN/logs/$NAME.log
  running() { [[ -f $pidfile ]] && kill -0 "$(cat "$pidfile")" 2>/dev/null; }
  case ${1:-status} in
    start)
      if running; then echo "$NAME already running (pid $(cat "$pidfile"))"; return 0; fi
      mkdir -p "$PLAN/pids" "$PLAN/logs"
      setsid nohup env VLLM_WORKER_MULTIPROC_METHOD=spawn "${COMMAND[@]}" > "$log" 2>&1 < /dev/null &
      echo $! > "$pidfile"
      for _ in $(seq 1 600); do
        if curl -fsS -m 2 -o /dev/null "$HEALTH" 2>/dev/null; then echo "$NAME ready on :$PORT"; return 0; fi
        running || { echo "$NAME exited; see $log" >&2; return 1; }
        sleep 1
      done
      echo "$NAME did not become ready; see $log" >&2; return 1 ;;
    stop)
      # vLLM's engine core is a separate process that outlives its API
      # server when only the server is signalled, holding the GPU memory; the
      # whole process group goes (the server was started with setsid).
      if running; then kill -- -"$(cat "$pidfile")" 2>/dev/null || kill "$(cat "$pidfile")"; echo "$NAME stopped"
      else echo "$NAME not running"; fi ;;
    status)
      if running && curl -fsS -m 2 -o /dev/null "$HEALTH"; then echo "$NAME up on :$PORT"; else echo "$NAME down"; return 1; fi ;;
    *) echo "usage: $0 start|stop|status" >&2; return 2 ;;
  esac
}
