#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
temporary="$(mktemp -d)"
cleanup() {
  rm -rf -- "${temporary}"
}
trap cleanup EXIT

mkdir -p "${temporary}/benchmarks/tau"
jq -n '{
  matrix_id:"fresh-matrix",
  benchmark:{seed:300,domains:[{name:"airline"}]},
  cells:[{id:"control"}]
}' >"${temporary}/benchmarks/tau/matrix.json"
jq -n '{
  tau_voice:{matrices:[{path:"benchmarks/tau/matrix.json"}]},
  full_duplex_bench_v1_5:{run_context:".runtime/fdb15/run-context.json"},
  full_duplex_bench_v3:{run_context:".runtime/fdbv3/run-context.json"},
  fd_bench:{run_context:".runtime/fd/run-context.json"}
}' >"${temporary}/manifest.json"

check() {
  "${repository_root}/scripts/check-full-study-freshness.sh" \
    "${temporary}/manifest.json" "${temporary}"
}

check >/dev/null
experiment="${temporary}/.runtime/tau2-bench/data/simulations/fresh-matrix-control-airline-seed300"
mkdir -p "${experiment}"
if check >/dev/null 2>&1; then
  echo "pre-existing tau population was accepted" >&2
  exit 1
fi
rmdir "${experiment}"
mkdir -p "${temporary}/.runtime/fdbv3"
if check >/dev/null 2>&1; then
  echo "pre-existing external run root was accepted" >&2
  exit 1
fi

echo "full-study freshness tests pass"
