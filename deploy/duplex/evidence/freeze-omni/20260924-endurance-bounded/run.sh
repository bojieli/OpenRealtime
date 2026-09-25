#!/usr/bin/env bash
# Excerpt of the 2026-09-24 GPU queue: its definitions and this step.
cd /home/ubuntu/OpenRealtime
PLAN=$PWD/.runtime/duplex-plan; BIN=$PLAN/bin/openrealtime-b7b68848; D=$PLAN/results/native
LOG=$D/gpu-queue-20260924.log
log() { echo "$(date -u +%FT%TZ) $*" >> $LOG; }
CONDS="chattts-single-round-combine-easy,chattts-single-round-combine-hard,chattts-single-round-combine-med,cosyvoice2-single-round-combine-easy,cosyvoice2-single-round-combine-easy-noisy-bg-0dB,cosyvoice2-single-round-combine-easy-noisy-bg-10dB,cosyvoice2-single-round-combine-easy-noisy-bg-20dB,cosyvoice2-single-round-combine-easy-noisy-gap-0dB,cosyvoice2-single-round-combine-easy-noisy-gap-10dB,cosyvoice2-single-round-combine-easy-noisy-gap-20dB,cosyvoice2-single-round-combine-hard,f5tts-single-round-combine-easy,f5tts-single-round-combine-easy-noisy-bg-0dB,f5tts-single-round-combine-easy-noisy-bg-10dB,f5tts-single-round-combine-easy-noisy-bg-20dB,f5tts-single-round-combine-easy-noisy-gap-0dB,f5tts-single-round-combine-easy-noisy-gap-10dB,f5tts-single-round-combine-easy-noisy-gap-20dB,f5tts-single-round-combine-hard,f5tts-single-round-combine-med"
serve_and_wait() { # profile port outdir
  setsid $BIN serve -config "$1" -listen 127.0.0.1:$2 -timeline-log "$3/timeline.log" > "$3/server.log" 2>&1 < /dev/null &
  SERVER=$!; for i in $(seq 1 120); do curl -sf http://127.0.0.1:$2/healthz > /dev/null && return 0; sleep 1; done; return 1; }
stop_server() { kill -TERM -- -$SERVER 2>/dev/null; kill $SERVER 2>/dev/null; sleep 3; }

# 1. Freeze-Omni endurance with the allocator release and context cap (f636ac17).
O=$D/freeze-endurance-20260924; mkdir -p $O
FREEZE_OMNI_CONTROL_LOG=$O/control.jsonl LEASE_WAIT=14400 deploy/duplex/services/freeze-omni.sh start >> $O/run.log 2>&1 && {
  ( while pgrep -f "freeze_omni_sidecar.py --serve" > /dev/null; do echo "$(date +%s),$(nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader,nounits | tr '\n' ' ')"; sleep 5; done > $O/vram.csv ) &
  PYTHONPATH=sidecars:tools/duplexmodels timeout 900 $PLAN/venvs/minicpm-duplex/bin/python tools/duplexmodels/native_probe.py --address tcp:127.0.0.1:9141     --scenario question --question $PLAN/build/long-input-600s.wav --lead-silence 0 --duration 605 --label freeze-endurance --out $O/probe.json > $O/probe.log 2>&1
  log "freeze endurance exit $?"; deploy/duplex/services/freeze-omni.sh stop >> $O/run.log 2>&1; cp $PLAN/logs/freeze-omni.log $O/engine.log; } || log "freeze start failed"
