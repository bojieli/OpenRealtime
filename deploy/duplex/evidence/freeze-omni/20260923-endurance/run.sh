#!/usr/bin/env bash
# Freeze-Omni endurance reproduction with the KV storage/allocator diagnostics
# (b934979b). Same 600 s input as freeze-longsession.json; no cache truncation.
set -uo pipefail
MAIN=/home/ubuntu/OpenRealtime; PLAN=$MAIN/.runtime/duplex-plan
OUT=$PLAN/results/native/freeze-endurance-20260923; mkdir -p "$OUT"
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$OUT/run.log"; }
cd "$MAIN"
echo "{\"revision\": \"$(git rev-parse HEAD)\", \"sidecar_sha256\": \"$(sha256sum sidecars/freeze_omni_sidecar.py | cut -d' ' -f1)\", \"input_sha256\": \"$(sha256sum $PLAN/build/long-input-600s.wav | cut -d' ' -f1)\", \"started\": \"$(date -u +%FT%TZ)\"}" > "$OUT/provenance.json"
FREEZE_OMNI_CONTROL_LOG=$OUT/control.jsonl LEASE_WAIT=14400 deploy/duplex/services/freeze-omni.sh start 2>&1 | tee -a "$OUT/run.log" || { log "start failed"; exit 1; }
PID=$(cat $PLAN/pids/freeze-omni.pid)
( while kill -0 $PID 2>/dev/null; do
    echo "$(date +%s),$(nvidia-smi --query-compute-apps=pid,used_memory --format=csv,noheader,nounits | awk -F', ' -v p="$(pgrep -g $PID -f freeze_omni_sidecar | head -1)" '$1==p{print $2}'),$(nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits)"
    sleep 5; done > "$OUT/vram.csv" ) &
SAMPLER=$!
PYTHONPATH=sidecars:tools/duplexmodels timeout 900 $PLAN/venvs/minicpm-duplex/bin/python tools/duplexmodels/native_probe.py --address tcp:127.0.0.1:9141 \
  --scenario question --question $PLAN/build/long-input-600s.wav --lead-silence 0 --duration 605 \
  --label freeze-endurance --out "$OUT/probe.json" > "$OUT/probe.log" 2>&1
log "probe exit $?"
deploy/duplex/services/freeze-omni.sh stop 2>&1 | tee -a "$OUT/run.log"
kill $SAMPLER 2>/dev/null; cp $PLAN/logs/freeze-omni.log "$OUT/engine.log"
log done
