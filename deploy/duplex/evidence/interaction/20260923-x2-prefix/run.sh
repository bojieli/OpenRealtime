#!/usr/bin/env bash
set -uo pipefail
cd /home/ubuntu/OpenRealtime
O=.runtime/duplex-plan/results/interaction/x2turn-prefix-20260923
LEASE_WAIT=14400 deploy/duplex/services/turn.sh start x2turn > $O/start.log 2>&1 || exit 1
timeout 1800 .runtime/duplex-plan/venvs/turn/bin/python tools/duplexmodels/endpoint_eval.py x2turn-timing --url http://127.0.0.1:9131 --work $O > $O/timing.log 2>&1
echo "timing exit $?" >> $O/timing.log
deploy/duplex/services/turn.sh stop x2turn >> $O/start.log 2>&1
