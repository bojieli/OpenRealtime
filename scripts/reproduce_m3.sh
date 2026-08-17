#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

./scripts/reproduce_m2.sh

run_root="$(mktemp -d "$repo_root/artifacts/m3-reproduce.XXXXXX")"
trap 'rm -rf "$run_root"' EXIT

artifacts/openrealtime benchmark m3 \
  --fixture tests/fixtures/m0-tone.wav \
  --manifest tests/fixtures/m1-reference-manifest.json \
  --output "$run_root" \
  --trials 30 \
  --seed 20260817 \
  --frame-ms 20

for trace_file in "$run_root"/traces/*.jsonl; do
  artifacts/openrealtime trace validate "$trace_file" >/dev/null
done

cmp "$run_root/report.json" benchmarks/m3/reference/report.json
for scenario in directed_interruption listener_backchannel side_speech invalidation_repair; do
  cmp "$run_root/traces/${scenario}-trial-0000.jsonl" \
    "benchmarks/m3/reference/${scenario}-trial-0000.jsonl"
  cmp "$run_root/timeline-${scenario}-trial-0000.html" \
    "benchmarks/m3/reference/timeline-${scenario}-trial-0000.html"
done
