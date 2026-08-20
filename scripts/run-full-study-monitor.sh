#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
output="${OPENREALTIME_STUDY_PROGRESS_OUTPUT:-${repository_root}/.runtime/benchmark-runs/full-study-v1/progress.json}"
interval_seconds="${OPENREALTIME_STUDY_MONITOR_INTERVAL_SECONDS:-60}"

if [[ ! "${interval_seconds}" =~ ^[0-9]+$ ]] || ((interval_seconds < 10)); then
  echo "OPENREALTIME_STUDY_MONITOR_INTERVAL_SECONDS must be an integer of at least 10" >&2
  exit 2
fi
if ! command -v jq >/dev/null; then
  echo "required command jq is unavailable" >&2
  exit 1
fi

while true; do
  if python3 "${repository_root}/scripts/report-full-study-progress.py" \
    --repository-root "${repository_root}" \
    --manifest "${repository_root}/benchmarks/full-study-v1.json" \
    --output "${output}"; then
    # An empty progress report makes `jq -e` exit 0 for any filter, which would
    # stop the monitor by declaring the study published.
    if jq -en --slurpfile progress "${output}" \
      '($progress | length) == 1 and $progress[0].publication_complete == true' \
      >/dev/null; then
      echo "full study publication is complete; monitor stopped"
      exit 0
    fi
  else
    echo "full study progress refresh failed; retrying after ${interval_seconds} seconds" >&2
  fi
  sleep "${interval_seconds}"
done
