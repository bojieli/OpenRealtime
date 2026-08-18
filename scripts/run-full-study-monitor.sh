#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
output="${OPENREALTIME_STUDY_PROGRESS_OUTPUT:-${repository_root}/.runtime/benchmark-runs/full-study-v1/progress.json}"
interval_seconds="${OPENREALTIME_STUDY_MONITOR_INTERVAL_SECONDS:-60}"

if [[ ! "${interval_seconds}" =~ ^[0-9]+$ ]] || ((interval_seconds < 10)); then
  echo "OPENREALTIME_STUDY_MONITOR_INTERVAL_SECONDS must be an integer of at least 10" >&2
  exit 2
fi

while true; do
  python3 "${repository_root}/scripts/report-full-study-progress.py" \
    --repository-root "${repository_root}" \
    --manifest "${repository_root}/benchmarks/full-study-v1.json" \
    --output "${output}"
  sleep "${interval_seconds}"
done
