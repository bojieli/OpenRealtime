#!/usr/bin/env bash
# Faithful vs bounded DuplexCascade (b9302dd8), same gated smoke scope.
cd /home/ubuntu/OpenRealtime
PLAN=$PWD/.runtime/duplex-plan; D=$PLAN/results/native; LOG=$D/duplexcascade-ab-20260925.log
export OPENREALTIME_BIN=$PLAN/bin/openrealtime-b7b68848 E2E_BENCH_TIMEOUT=7200
log() { echo "$(date -u +%FT%TZ) $*" >> $LOG; }
cell() { # profile, bound
  DUPLEXCASCADE_MAX_BACKLOG_S=$2 deploy/duplex/services/duplexcascade.sh start >> $LOG 2>&1 || { log "$1 start failed"; return; }
  log "$1 started"
  E2E_PORT=$3 $PLAN/venvs/ellsa/bin/python tools/duplexmodels/e2e_run.py $1 10 12 >> $LOG 2>&1
  log "$1 campaign exit $?"
  deploy/duplex/services/duplexcascade.sh stop >> $LOG 2>&1; sleep 10
  cp $PLAN/logs/duplexcascade.log $D/duplexcascade-ab-$1.engine.log
}
cell native-duplexcascade "" 9295
cell native-duplexcascade-bounded 2 9296
log "ab done"
