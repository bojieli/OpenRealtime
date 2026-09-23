#!/usr/bin/env bash
# Player validation on native Moshi after fe51439a (interrupted turns reported).
cd /home/ubuntu/OpenRealtime
PLAN=$PWD/.runtime/duplex-plan; BIN=$PLAN/bin/openrealtime-d0225ace
OUT=$PLAN/results/native/validate-player-moshi; mkdir -p $OUT
log() { echo "$(date -u +%FT%TZ) $*" >> $OUT/run.log; }
env -u HF_TOKEN HF_HUB_OFFLINE=1 setsid nohup flock -w 1800 $PLAN/gpu/large.lock $PLAN/venvs/kyutai/bin/python sidecars/moshi_sidecar.py \
  --repository kyutai/moshiko-pytorch-bf16 --revision 2bfc9ae6e89079a5cc7ed2a68436010d91a3d289 \
  --listen tcp:127.0.0.1:9148 --stats-file $OUT/stats.jsonl > $OUT/sidecar.log 2>&1 < /dev/null & SIDECAR=$!
for i in $(seq 1 600); do kill -0 $SIDECAR 2>/dev/null || { log "sidecar exited"; exit 1; }; grep -q "listening on tcp:127.0.0.1:9148" $OUT/sidecar.log && break; sleep 2; done
log "moshi ready"
setsid $BIN serve -config deploy/duplex/profiles/native-moshi.yaml -listen 127.0.0.1:9298 -timeline-log $OUT/timeline.log > $OUT/server.log 2>&1 < /dev/null & SERVER=$!
for i in $(seq 1 60); do curl -sf http://127.0.0.1:9298/healthz > /dev/null && break; sleep 1; done
timeout 3600 $BIN bench fdb -categories user_interruption -limit 20 -wait-configured -player \
  -endpoint ws://127.0.0.1:9298/v1/realtime -cell moshi-player -transcripts $OUT/transcripts \
  -out $OUT/player-fdb.json > $OUT/player-fdb.log 2>&1; log "player fdb exit $?"
kill -TERM -- -$SERVER 2>/dev/null; kill $SERVER 2>/dev/null; sleep 2
kill -TERM -- -$SIDECAR 2>/dev/null; for i in $(seq 1 30); do kill -0 $SIDECAR 2>/dev/null || break; sleep 1; done
log done
