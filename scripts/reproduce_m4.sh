#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

./scripts/reproduce_m3.sh

run_root="$(mktemp -d "$repo_root/artifacts/m4-reproduce.XXXXXX")"
trap 'rm -rf "$run_root"' EXIT

artifacts/openrealtime benchmark m4 \
  --workload tests/fixtures/m4-difficult-workload.json \
  --output "$run_root" \
  --trials 30 \
  --seed 20260817

cmp "$run_root/report.json" benchmarks/m4/reference/report.json
cmp "$run_root/frontier.html" benchmarks/m4/reference/frontier.html
