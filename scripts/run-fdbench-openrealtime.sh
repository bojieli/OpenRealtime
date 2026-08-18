#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime_root="${FDBENCH_RUNTIME_ROOT:-${repository_root}/.runtime/fd-bench}"
dataset_root="${runtime_root}/dataset"
upstream_root="${runtime_root}/upstream"
run_root="${FDBENCH_RUN_ROOT:-${repository_root}/.runtime/benchmark-runs/fd-bench/openrealtime-v1}"
output_root="${run_root}/output"

: "${OPENREALTIME_API_KEY:?OPENREALTIME_API_KEY must be set}"
cd "${repository_root}"
mkdir -p "${run_root}"
"${repository_root}/scripts/prepare-fdbench.sh"

/usr/local/go/bin/go run ./cmd/fdbench run \
  --dataset-root "${dataset_root}" \
  --output-root "${output_root}" \
  --run-root "${run_root}" \
  --endpoint "${OPENREALTIME_BASE_URL:-ws://127.0.0.1:8765/v1/realtime}" \
  --api-key "${OPENREALTIME_API_KEY}" \
  --model "${OPENREALTIME_MODEL:-openrealtime-local}" \
  --voice "${OPENREALTIME_VOICE:-tau-lisa-brenner-v1}" \
  --provider-label openrealtime \
  --chunk-duration 20ms \
  --tail-duration 10s \
  --trial-timeout 3m \
  --trial-attempts 3 \
  --retry-delay 1s \
  --resume=true \
  --continue-on-error=true \
  2>&1 | tee "${run_root}/run.log"

"${repository_root}/scripts/prepare-fdbench-evaluator.sh"
evaluator_python="${runtime_root}/evaluator-venv/bin/python"
"${evaluator_python}" "${repository_root}/scripts/finalize-fdbench.py" \
  --output-root "${output_root}" \
  --provider-label openrealtime \
  >"${run_root}/finalization.json"

if [[ "${FDBENCH_RUN_TIMING_EVALUATOR:-true}" == true ]]; then
  mkdir -p "${run_root}/metrics"
  while IFS= read -r trace; do
    cell="$(basename "$(dirname "${trace}")")"
    "${evaluator_python}" "${repository_root}/scripts/evaluate-fdbench.py" \
      --upstream-root "${upstream_root}" \
      --trace "${trace}" \
      --output "${run_root}/metrics/${cell}.json" \
      >"${run_root}/metrics/${cell}.log"
  done < <(find "${output_root}" -mindepth 2 -maxdepth 2 -type f -name 'openrealtime.txt' | sort)
fi

echo "FD-Bench full released matrix complete"
