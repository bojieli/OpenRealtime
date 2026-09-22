#!/usr/bin/env bash
# Run the end-to-end matrix: one profile after another, never two at once.
#
#   deploy/duplex/run-matrix.sh [fdb-per-category] [fdbench-conversations] profile...
#
# Profiles run sequentially because they share the GPU and because two
# latency measurements taken at the same time measure each other. A profile
# whose services are not listening is skipped with a reason rather than
# recorded as a failure - an unavailable cell stays visible as unavailable.
#
# Every profile's port requirements are read from the profile itself (any
# 127.0.0.1:PORT it names), so a new profile needs no change here.
set -uo pipefail

per_category="${1:-10}"
conversations="${2:-12}"
shift 2 || true
repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "${repository}"
log="${repository}/.runtime/duplex-plan/results/e2e/matrix.log"
mkdir -p "$(dirname "${log}")"
mkdir -p "${repository}/.runtime/duplex-plan/logs"
status=0

listening() { ss -ltnH "src 127.0.0.1:$1" 2>/dev/null | grep -q .; }

for profile in "$@"; do
  config="deploy/duplex/profiles/${profile}.yaml"
  if [[ ! -f "${config}" ]]; then
    echo "$(date -Is) ${profile}: no such profile" | tee -a "${log}"
    status=1
    continue
  fi
  missing=""
  for port in $(grep -oE '127\.0\.0\.1:[0-9]+' "${config}" | cut -d: -f2 | sort -u); do
    listening "${port}" || missing="${missing} ${port}"
  done
  if [[ -n "${missing}" ]]; then
    echo "$(date -Is) ${profile}: SKIPPED, nothing listening on${missing}" | tee -a "${log}"
    status=1
    continue
  fi
  echo "$(date -Is) ${profile}: start (load $(cut -d' ' -f1 /proc/loadavg))" | tee -a "${log}"
  E2E_PORT="${E2E_PORT:-9290}" deploy/duplex/run-e2e.sh "${profile}" "${per_category}" "${conversations}" \
    >> "${repository}/.runtime/duplex-plan/logs/e2e-${profile}.log" 2>&1
  code=$?
  echo "$(date -Is) ${profile}: done (exit ${code})" | tee -a "${log}"
  if (( code != 0 )); then status=1; fi
done
echo "$(date -Is) matrix finished (exit ${status})" | tee -a "${log}"
exit "${status}"
