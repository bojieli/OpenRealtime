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

# 0. MiniCPM-o's selected FD-Bench condition: the campaign runner stopped before
# it because sidecar sources were edited mid-run (by this session). Same scope,
# gated, as the e2e runner would have run it.
O=$D/fdbench-minicpm-o-20260924; mkdir -p $O
MINICPM_DUPLEX_MEMORY_WAIT_S=86400 deploy/duplex/services/minicpm-o-duplex.sh start >> $O/run.log 2>&1
for i in $(seq 1 2880); do grep -q "listening" $PLAN/logs/minicpm-o-duplex.log && break; grep -q "gave up" $PLAN/logs/minicpm-o-duplex.log && break; sleep 30; done
if serve_and_wait deploy/duplex/profiles/native-minicpm-o.yaml 9302 $O; then
  timeout 86400 $BIN bench fdbench -conditions cosyvoice2-single-round-combine-med -wait-configured \
    -endpoint ws://127.0.0.1:9302/v1/realtime -cell native-minicpm-o -out $O/fdbench.json > $O/fdbench.log 2>&1
  log "minicpm fdbench exit $?"; stop_server; else log "minicpm serve failed"; fi
deploy/duplex/services/minicpm-o-duplex.sh stop >> $O/run.log 2>&1
