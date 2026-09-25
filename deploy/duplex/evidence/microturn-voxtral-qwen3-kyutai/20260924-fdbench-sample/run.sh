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

# 5. Sampled FD-Bench coverage: micro-turn cascade.
O=$D/fdbench-sample-microturn-20260924; mkdir -p $O
deploy/duplex/services/asr-voxtral.sh start >> $O/run.log 2>&1 && serve_and_wait deploy/duplex/profiles/microturn-voxtral-qwen3-kyutai.yaml 9301 $O && {
  timeout 172800 $BIN bench fdbench -conditions "$CONDS" -sample-per-condition 50 -sample-seed 20260924 -wait-configured     -endpoint ws://127.0.0.1:9301/v1/realtime -cell microturn-sample -out $O/fdbench-sample.json > $O/fdbench-sample.log 2>&1
  log "microturn sample exit $?"; stop_server; } || log "microturn sample start failed"
deploy/duplex/services/asr-voxtral.sh stop >> $O/run.log 2>&1
