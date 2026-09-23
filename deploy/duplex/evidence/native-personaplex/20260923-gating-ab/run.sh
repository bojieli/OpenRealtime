#!/usr/bin/env bash
# Matched PersonaPlex A/B: benchmark WebSocket replay gated vs ungated, ABBA order,
# lifecycle sidecar (per-session drop diagnostics), same fixtures and server binary.
set -uo pipefail
MAIN=/home/ubuntu/OpenRealtime; LC=/home/ubuntu/OpenRealtime-duplex-lifecycle-validation
PLAN=$MAIN/.runtime/duplex-plan; OUT=$PLAN/results/native/personaplex-gating-ab-20260923; mkdir -p "$OUT"
BIN=$PLAN/bin/openrealtime-9f6dd353
SNAP=$HOME/.cache/huggingface/hub/models--nvidia--personaplex-7b-v1/snapshots/fdaf4090a61cb315c138a1faee287ffd6c716309
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$OUT/run.log"; }
[[ "$(git -C "$LC" rev-parse HEAD)" == f6f0a3a43cb5797abc25d032de73dad2f1ff4368 ]] || { log "lifecycle checkout moved"; exit 1; }
echo "{\"sidecar_revision\": \"f6f0a3a4\", \"binary\": \"$BIN\", \"binary_sha256\": \"$(sha256sum $BIN | cut -d' ' -f1)\", \"started\": \"$(date -u +%FT%TZ)\", \"order\": \"fdb ungated, fdb gated, fdbench gated, fdbench ungated\"}" > "$OUT/provenance.json"
cd "$LC"
env -u HF_TOKEN HF_HUB_OFFLINE=1 PYTHONPATH="$PLAN/src/personaplex/moshi" setsid nohup \
  flock -w 14400 "$PLAN/gpu/large.lock" "$PLAN/venvs/kyutai/bin/python" sidecars/personaplex_sidecar.py \
  --snapshot "$SNAP" --voice "$PLAN/data/personaplex/voices/NATF2.pt" --listen tcp:127.0.0.1:9146 \
  --stats-file "$OUT/stats.jsonl" > "$OUT/sidecar.log" 2>&1 < /dev/null &
SIDECAR=$!
for i in $(seq 1 15000); do kill -0 $SIDECAR 2>/dev/null || { log "sidecar exited"; exit 1; }
  grep -Fq "sidecar listening on tcp:127.0.0.1:9146" "$OUT/sidecar.log" && break; sleep 1; done
log "sidecar ready"
cd "$MAIN"
( setsid "$BIN" serve -config "$LC/deploy/duplex/profiles/native-personaplex.yaml" -listen 127.0.0.1:9293 \
    -timeline-log "$OUT/timeline.log" > "$OUT/server.log" 2>&1 < /dev/null & echo $! > "$OUT/server.pid" )
for i in $(seq 1 60); do curl -sf http://127.0.0.1:9293/healthz > "$OUT/healthz.json" && break; sleep 1; done
run() { # name suite flag args...
  local name=$1 suite=$2 flag=$3; shift 3
  echo "$name $(wc -l < "$OUT/stats.jsonl" 2>/dev/null || echo 0)" >> "$OUT/stats-offsets.txt"
  timeout 3600 "$BIN" bench $suite "$@" $flag -endpoint ws://127.0.0.1:9293/v1/realtime -cell "personaplex-$name" \
    -out "$OUT/$name.json" > "$OUT/$name.log" 2>&1; log "$name exit $?"
}
run fdb-ungated fdb "" -categories user_interruption -limit 20
run fdb-gated fdb -wait-configured -categories user_interruption -limit 20
run fdbench-gated fdbench -wait-configured -conditions cosyvoice2-single-round-combine-med -limit 10
run fdbench-ungated fdbench "" -conditions cosyvoice2-single-round-combine-med -limit 10
echo "end $(wc -l < "$OUT/stats.jsonl")" >> "$OUT/stats-offsets.txt"
kill -TERM -- -$(cat "$OUT/server.pid") 2>/dev/null; sleep 3; kill -TERM -- -$SIDECAR 2>/dev/null
for i in $(seq 1 30); do kill -0 $SIDECAR 2>/dev/null || break; sleep 1; done
log done
