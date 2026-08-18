#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
dataset_root="${FDB15_DATASET_ROOT:-${repository_root}/.runtime/full-duplex-bench-v1.5/dataset}"
output_root="${FDB15_OUTPUT_ROOT:-${repository_root}/.runtime/benchmark-runs/fdb15-openrealtime-full-overlap}"

: "${OPENREALTIME_API_KEY:?OPENREALTIME_API_KEY must be set to the loopback gateway token}"
export OPENREALTIME_BASE_URL="${OPENREALTIME_BASE_URL:-ws://127.0.0.1:8765/v1/realtime}"

"${repository_root}/scripts/prepare-fdb15.sh"
if ! curl --fail --silent --show-error http://127.0.0.1:8765/healthz >/dev/null; then
  echo "OpenRealtime gateway is not healthy" >&2
  exit 1
fi

mkdir -p "${output_root}"
/usr/local/go/bin/go run ./cmd/livebench run \
  --dataset-root "${dataset_root}" \
  --output-root "${output_root}" \
  --provider openrealtime \
  --model openrealtime-local \
  --conditions overlap \
  --replicates 1 \
  --trial-attempts 3 \
  --retry-delay 1s \
  --trial-timeout 5m \
  --tail-duration 5s \
  --continue-on-error=true \
  --require-complete=true \
  --resume=true
