#!/usr/bin/env bash
# Lychee stall reproduction (handoff item 6): same fixtures as the 2026-09-22
# stalls, audio_delivery sidecar events, upstream stage timing, received PCM.
set -uo pipefail
MAIN=/home/ubuntu/OpenRealtime; PLAN=$MAIN/.runtime/duplex-plan
OUT=$PLAN/results/native/lychee-stall-20260923; mkdir -p "$OUT"
BIN=$PLAN/bin/openrealtime-68c15b3e
PY=$PLAN/venvs/lychee/bin/python
PROBE="$PLAN/venvs/minicpm-duplex/bin/python tools/duplexmodels/native_probe.py"
SIDECAR="$PY sidecars/lychee_fd_sidecar.py --backend http://127.0.0.1:9143 --control-log $OUT/control.jsonl"
FDB=$MAIN/.runtime/full-duplex-bench-v1.5/dataset/user_interruption
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$OUT/run.log"; }
cd "$MAIN"
echo "{\"revision\": \"$(git rev-parse HEAD)\", \"sidecar_sha256\": \"$(sha256sum sidecars/lychee_fd_sidecar.py | cut -d' ' -f1)\", \"binary_sha256\": \"$(sha256sum $BIN | cut -d' ' -f1)\", \"started\": \"$(date -u +%FT%TZ)\"}" > "$OUT/provenance.json"
date +%s%3N > "$OUT/start_epoch_ms"
deploy/duplex/services/lychee-fd.sh start | tee -a "$OUT/run.log"
for i in $(seq 1 3000); do curl -sf -m 2 http://127.0.0.1:9143/api/realtime/voices > /dev/null && break
  deploy/duplex/services/lychee-fd.sh status > /dev/null 2>&1 || { log "lychee exited during startup"; exit 1; }; sleep 5; done
log "lychee ready"
# A. Same 8 FDB interruption recordings through the public server.
( setsid "$BIN" serve -binding duplex -floor engine -sidecar "$SIDECAR" -listen 127.0.0.1:9292 \
    -timeline-log "$OUT/timeline.log" > "$OUT/server.log" 2>&1 < /dev/null & echo $! > "$OUT/server.pid" )
for i in $(seq 1 60); do curl -sf http://127.0.0.1:9292/healthz > "$OUT/healthz.json" && break; sleep 1; done
timeout 1800 "$BIN" bench fdb -categories user_interruption -limit 8 -endpoint ws://127.0.0.1:9292/v1/realtime \
  -cell lychee-stall -out "$OUT/fdb.json" > "$OUT/fdb.log" 2>&1; log "fdb exit $?"
kill -TERM -- -$(cat "$OUT/server.pid") 2>/dev/null; sleep 3
# B. Finite input (stop sending 1 s after the question) vs continuous paced silence.
for n in 1 4; do for mode in continuous finite; do
  extra=""; [[ $mode == finite ]] && extra="--stop-after 1.0"
  PYTHONPATH=sidecars:tools/duplexmodels timeout 300 $PROBE --sidecar "$SIDECAR" --stderr "$OUT/probe-ui$n-$mode.stderr" \
    --scenario question --question "$FDB/$n/input.wav" --lead-silence 1.0 --duration 45 $extra \
    --label "ui$n-$mode" --out "$OUT/probe-ui$n-$mode.json" > "$OUT/probe-ui$n-$mode.log" 2>&1
  log "probe ui$n $mode exit $?"
done; done
# C. The 600 s continuous input that produced 18 of the 26 original stalls.
PYTHONPATH=sidecars:tools/duplexmodels timeout 900 $PROBE --sidecar "$SIDECAR" --stderr "$OUT/probe-long.stderr" \
  --scenario question --question $PLAN/build/long-input-600s.wav --lead-silence 0 --duration 605 \
  --label long --out "$OUT/probe-long.json" > "$OUT/probe-long.log" 2>&1
log "probe long exit $?"
deploy/duplex/services/lychee-fd.sh stop | tee -a "$OUT/run.log"
S=$(cat "$OUT/start_epoch_ms"); mkdir -p "$OUT/stage_timing" "$OUT/control_prob"
find $PLAN/logs/lychee-fd-runtime/stage_timing -newermt "@$((S/1000))" -type f -exec cp {} "$OUT/stage_timing/" \;
find $PLAN/logs/lychee-fd-runtime/control_prob -newermt "@$((S/1000))" -type f -exec cp {} "$OUT/control_prob/" \;
log done
