#!/usr/bin/env bash
# Run the end-to-end voice-agent check for one duplex-plan profile.
#
#   deploy/duplex/run-e2e.sh <profile> [fdb-per-category] [fdbench-conversations]
#
# <profile> names deploy/duplex/profiles/<profile>.yaml, an ordinary
# `openrealtime serve -config` file. The model services it points at must
# already be running (deploy/duplex/services/). The script starts the server
# on a private port, drives a stratified FDB v1.5 subset (the same number of
# recordings from each of its four categories) and an FD-Bench slice through
# the public Realtime protocol, and stops the server.
#
# Everything lands in .runtime/duplex-plan/results/e2e/<profile>/: the bench
# JSON, the server log and timeline, and run.json recording the revision,
# profile digest, host load and GPU memory at the start - a latency number
# measured on a starved machine must say so.
#
# A subset is a smoke measurement, not a reportable campaign; the bench says
# NOT REPORTABLE and so does this script's summary.
set -euo pipefail

profile="${1:?profile name}"
per_category="${2:-10}"
conversations="${3:-12}"
repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
config="${repository}/deploy/duplex/profiles/${profile}.yaml"
[[ -f "${config}" ]] || { echo "no profile ${config}" >&2; exit 2; }
binary="${OPENREALTIME_BIN:-${repository}/.runtime/duplex-plan/bin/openrealtime}"
port="${E2E_PORT:-9290}"
out="${repository}/.runtime/duplex-plan/results/e2e/${profile}"
mkdir -p "${out}"
cd "${repository}"

gpu_memory="$(nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits 2>/dev/null | head -1 || echo unknown)"
load="$(cut -d' ' -f1-3 /proc/loadavg)"
cat > "${out}/run.json" <<JSON
{"profile": "${profile}", "profile_sha256": "$(sha256sum "${config}" | cut -d' ' -f1)",
 "revision": "$(git rev-parse HEAD)", "tree_modified": $([[ -n "$(git status --porcelain -- . ':!.runtime')" ]] && echo true || echo false),
 "started": "$(date -Is)", "load_average": "${load}", "gpu_memory_mib": "${gpu_memory}",
 "fdb_per_category": ${per_category}, "fdbench_conversations": ${conversations}}
JSON

"${binary}" serve -config "${config}" -listen "127.0.0.1:${port}" \
  -timeline-log "${out}/timeline.log" > "${out}/server.log" 2>&1 &
server=$!
trap 'kill ${server} 2>/dev/null || true; wait ${server} 2>/dev/null || true' EXIT
for _ in $(seq 1 300); do
  if curl -fsS -m 2 "http://127.0.0.1:${port}/healthz" > "${out}/healthz.json" 2>/dev/null; then
    break
  fi
  if ! kill -0 "${server}" 2>/dev/null; then
    echo "server exited; see ${out}/server.log" >&2
    exit 1
  fi
  sleep 1
done

endpoint="ws://127.0.0.1:${port}/v1/realtime"
for category in user_interruption user_backchannel background_speech talking_to_other; do
  "${binary}" bench fdb -endpoint "${endpoint}" -categories "${category}" -limit "${per_category}" \
    -cell "${profile}" -out "${out}/fdb-${category}.json" > "${out}/fdb-${category}.log" 2>&1 || true
done
if [[ "${conversations}" -gt 0 ]]; then
  "${binary}" bench fdbench -endpoint "${endpoint}" -conditions cosyvoice2-single-round-combine-med \
    -limit "${conversations}" -cell "${profile}" -out "${out}/fdbench.json" > "${out}/fdbench.log" 2>&1 || true
fi
echo "{\"finished\": \"$(date -Is)\", \"load_average_end\": \"$(cut -d' ' -f1-3 /proc/loadavg)\"}" > "${out}/finished.json"
echo "results in ${out}"
