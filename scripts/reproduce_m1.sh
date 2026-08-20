#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
source "$repo_root/scripts/go-toolchain.sh"
go_bin="$(openrealtime_go_bin)"

mkdir -p artifacts
run_dir="$(mktemp -d "$repo_root/artifacts/m1-reproduce.XXXXXX")"
trap 'rm -rf "$run_dir"' EXIT

"$go_bin" mod download
"$go_bin" build -trimpath -ldflags='-s -w' -o artifacts/openrealtime ./cmd/openrealtime
./scripts/check_openai_realtime_spec.sh

artifacts/openrealtime benchmark m1 \
  --fixture tests/fixtures/m0-tone.wav \
  --manifest tests/fixtures/m1-reference-manifest.json \
  --output "$run_dir" \
  --trials 30 \
  --seed 20260817 \
  --frame-ms 20

for trace_file in "$run_dir"/traces/*.jsonl; do
  artifacts/openrealtime trace validate "$trace_file" >/dev/null
done

cmp "$run_dir/report.json" benchmarks/m1/reference/report.json
cmp "$run_dir/traces/trial-0000.jsonl" benchmarks/m1/reference/trial-0000.jsonl
cmp "$run_dir/timeline-trial-0000.html" benchmarks/m1/reference/timeline-trial-0000.html

"$go_bin" test -race ./...
"$go_bin" vet ./...
unformatted="$(gofmt -l .)"
if [[ -n "$unformatted" ]]; then
  echo "unformatted Go files:" >&2
  echo "$unformatted" >&2
  exit 1
fi
