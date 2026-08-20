#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

./scripts/reproduce_m4.sh

run_root="$(mktemp -d "$repo_root/artifacts/m5-reproduce.XXXXXX")"
trap 'rm -rf "$run_root"' EXIT

artifacts/openrealtime benchmark m5 \
  --fixture tests/fixtures/m0-tone.wav \
  --demonstrations tests/fixtures/m5-demonstrations.json \
  --output "$run_root" \
  --trials 30 \
  --seed 20260817

for trace_file in "$run_root"/traces/translation/*.jsonl "$run_root"/traces/game/*.jsonl; do
  artifacts/openrealtime trace validate "$trace_file" >/dev/null
done

cmp "$run_root/report.json" benchmarks/m5/reference/report.json
cmp "$run_root/demonstrations.html" benchmarks/m5/reference/demonstrations.html
for condition in endpointed stable_incremental aggressive_incremental; do
  cmp "$run_root/traces/translation/${condition}-trial-0000.jsonl" \
    "benchmarks/m5/reference/translation-${condition}-trial-0000.jsonl"
done
for condition in endpointed microturn_50ms; do
  cmp "$run_root/traces/game/${condition}-trial-0000.jsonl" \
    "benchmarks/m5/reference/game-${condition}-trial-0000.jsonl"
done
cmp "$run_root/timeline-translation-stable-trial-0000.html" \
  benchmarks/m5/reference/timeline-translation-stable-trial-0000.html
cmp "$run_root/timeline-game-microturn-trial-0000.html" \
  benchmarks/m5/reference/timeline-game-microturn-trial-0000.html

source "$repo_root/scripts/go-toolchain.sh"
go_bin="$(openrealtime_go_bin)"
"$go_bin" test -race ./...
"$go_bin" vet ./...
unformatted="$(gofmt -l .)"
if [[ -n "$unformatted" ]]; then
  echo "unformatted Go files:" >&2
  echo "$unformatted" >&2
  exit 1
fi
