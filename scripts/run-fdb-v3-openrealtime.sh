#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime_root="${FDBV3_RUNTIME_ROOT:-${repository_root}/.runtime/full-duplex-bench-v3}"
dataset_root="${runtime_root}/dataset/fdb_v3_data_released"
run_root="${FDBV3_RUN_ROOT:-${repository_root}/.runtime/benchmark-runs/fdb-v3/openrealtime-v1}"
upstream_root="${FDB_UPSTREAM_ROOT:-${repository_root}/.runtime/full-duplex-bench}"

: "${OPENREALTIME_API_KEY:?OPENREALTIME_API_KEY must be set to the loopback gateway token}"
export OPENREALTIME_BASE_URL="${OPENREALTIME_BASE_URL:-ws://127.0.0.1:8765/v1/realtime}"

"${repository_root}/scripts/prepare-fdb-v3.sh"
if ! curl --fail --silent --show-error http://127.0.0.1:8765/healthz >/dev/null; then
  echo "OpenRealtime gateway is not healthy" >&2
  exit 1
fi
if [[ "$(git -C "${upstream_root}" rev-parse HEAD)" != "3e799c45a045256f47d5f1c9cda90157e2d2ec9e" ]]; then
  echo "Full-Duplex-Bench upstream revision is not pinned" >&2
  exit 1
fi

mkdir -p "${run_root}"
/usr/local/go/bin/go run ./cmd/fdbv3bench run \
  --dataset-root "${dataset_root}" \
  --profile benchmarks/fdb-v3/openrealtime-v1.json \
  --run-root "${run_root}" \
  --endpoint "${OPENREALTIME_BASE_URL}" \
  --api-key "${OPENREALTIME_API_KEY}" \
  --model openrealtime-local \
  --voice tau-lisa-brenner-v1 \
  --provider-label openrealtime \
  --chunk-duration 20ms \
  --tail-duration 45s \
  --trial-timeout 3m \
  --trial-attempts 3 \
  --retry-delay 1s \
  --continue-on-error=true \
  --require-complete=true \
  --resume=true

python "${upstream_root}/v3/evaluate_tool_calls.py" \
  --benchmark "${upstream_root}/v3/benchmark_data_v2.json" \
  --results-dir "${dataset_root}" \
  --provider openrealtime \
  --output "${run_root}/evaluation-exact.json"

if [[ "${FDBV3_USE_LLM_JUDGE:-true}" == true ]]; then
  : "${OPENAI_API_KEY:?OPENAI_API_KEY must be set for the official FDB v3 GPT-4o judge}"
  python "${upstream_root}/v3/evaluate_tool_calls.py" \
    --benchmark "${upstream_root}/v3/benchmark_data_v2.json" \
    --results-dir "${dataset_root}" \
    --provider openrealtime \
    --output "${run_root}/evaluation-gpt4o.json" \
    --use-llm
fi
