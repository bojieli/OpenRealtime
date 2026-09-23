#!/usr/bin/env bash
# PersonaPlex lifecycle follow-up (handoff 2026-09-23 items 2-4): new-diagnostics
# sidecar from the clean lifecycle checkout, real GPU reconnect, and matched
# gated/ungated readiness replay through the campaign's server binary.
set -uo pipefail
MAIN=/home/ubuntu/OpenRealtime
LC=/home/ubuntu/OpenRealtime-duplex-lifecycle-validation
PLAN=$MAIN/.runtime/duplex-plan
OUT=$PLAN/results/native/personaplex-lifecycle-20260923
BIN=$PLAN/bin/openrealtime-68c15b3e
PROBE_PY=$PLAN/venvs/minicpm-duplex/bin/python
SNAP=$HOME/.cache/huggingface/hub/models--nvidia--personaplex-7b-v1/snapshots/fdaf4090a61cb315c138a1faee287ffd6c716309
RQ=$PLAN/results/native/voicechat-resume-question.wav
SQ=$PLAN/src/Freeze-Omni/assets/question.wav
mkdir -p "$OUT"
log() { echo "$(date -u +%FT%TZ) $*" | tee -a "$OUT/run.log"; }
[[ "$(git -C "$LC" rev-parse HEAD)" == f6f0a3a43cb5797abc25d032de73dad2f1ff4368 ]] || { log "lifecycle checkout moved"; exit 1; }
[[ -z "$(git -C "$LC" status --porcelain -- sidecars tools)" ]] || { log "lifecycle checkout modified"; exit 1; }
{ echo "{"; echo "\"revision\": \"$(git -C "$LC" rev-parse HEAD)\","; echo "\"binary_sha256\": \"$(sha256sum "$BIN" | cut -d' ' -f1)\","
  echo "\"reconnect_question_sha256\": \"$(sha256sum "$RQ" | cut -d' ' -f1)\","; echo "\"readiness_question_sha256\": \"$(sha256sum "$SQ" | cut -d' ' -f1)\","
  echo "\"sidecar_sha256\": \"$(sha256sum "$LC/sidecars/personaplex_sidecar.py" | cut -d' ' -f1)\","; echo "\"started\": \"$(date -u +%FT%TZ)\"}"; } > "$OUT/provenance.json"
cd "$LC"
env -u HF_TOKEN HF_HUB_OFFLINE=1 PYTHONPATH="$PLAN/src/personaplex/moshi" setsid nohup \
  flock -w 14400 "$PLAN/gpu/large.lock" "$PLAN/venvs/kyutai/bin/python" sidecars/personaplex_sidecar.py \
  --snapshot "$SNAP" --voice "$PLAN/data/personaplex/voices/NATF2.pt" --listen tcp:127.0.0.1:9146 \
  --stats-file "$OUT/stats.jsonl" > "$OUT/sidecar.log" 2>&1 < /dev/null &
SIDECAR=$!; echo $SIDECAR > "$OUT/sidecar.pid"; log "sidecar launcher $SIDECAR"
for i in $(seq 1 15000); do
  kill -0 $SIDECAR 2>/dev/null || { log "sidecar exited during startup"; exit 1; }
  grep -Fq "sidecar listening on tcp:127.0.0.1:9146" "$OUT/sidecar.log" && break; sleep 1
done
grep -Fq "sidecar listening" "$OUT/sidecar.log" || { log "sidecar not ready"; kill -TERM -- -$SIDECAR; exit 1; }
log "sidecar ready"
for i in 1 2 3; do
  PYTHONPATH=sidecars:tools/duplexmodels timeout 300 "$PROBE_PY" tools/duplexmodels/native_reconnect_probe.py \
    --address tcp:127.0.0.1:9146 --question "$RQ" --timeout 40 --out "$OUT/reconnect-$i.json" > "$OUT/reconnect-$i.log" 2>&1
  log "reconnect $i exit $?"
done
( cd "$MAIN" && setsid "$BIN" serve -config "$LC/deploy/duplex/profiles/native-personaplex.yaml" -listen 127.0.0.1:9291 \
    -timeline-log "$OUT/timeline.log" > "$OUT/server.log" 2>&1 < /dev/null & echo $! > "$OUT/server.pid" )
SERVER=$(cat "$OUT/server.pid")
for i in $(seq 1 120); do curl -sf http://127.0.0.1:9291/healthz > "$OUT/healthz.json" && break; sleep 1; done
log "server $SERVER healthz $?"
for i in 1 2 3 4 5 6; do
  for mode in gated ungated; do
    flag=""; [[ $mode == gated ]] && flag=--wait-configured
    timeout 120 "$PROBE_PY" tools/duplexmodels/realtime_probe.py --endpoint ws://127.0.0.1:9291/v1/realtime \
      --wav "$SQ" --lead 1.0 --duration 20 $flag --out "$OUT/readiness-$mode-$i.jsonl" > "$OUT/readiness-$mode-$i.log" 2>&1
    log "readiness $mode $i exit $?"
    sleep 2
  done
done
kill -TERM -- -$SERVER 2>/dev/null; sleep 3
kill -TERM -- -$SIDECAR 2>/dev/null
for i in $(seq 1 30); do kill -0 $SIDECAR 2>/dev/null || break; sleep 1; done
kill -0 $SIDECAR 2>/dev/null && log "sidecar still alive after TERM" || log "sidecar stopped"
log done
