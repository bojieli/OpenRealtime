#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

./scripts/reproduce_m6.sh

run_root="$(mktemp -d "$repo_root/artifacts/m7-reproduce.XXXXXX")"
trap 'rm -rf "$run_root"' EXIT

artifacts/openrealtime conformance all \
  --fixture tests/fixtures/m0-tone.wav \
  --manifest tests/fixtures/m1-reference-manifest.json \
  --workload tests/fixtures/m4-difficult-workload.json \
  >"$run_root/conformance.json"
cmp "$run_root/conformance.json" tests/golden/m7-conformance.json

go build -o "$run_root/reference-example" ./examples/v1/reference
go run ./examples/v1/reference >"$run_root/example.json"
cmp "$run_root/example.json" tests/golden/m7-reference-example.json

go test -race ./...
go vet ./...
unformatted="$(gofmt -l .)"
if [[ -n "$unformatted" ]]; then
  echo "unformatted Go files:" >&2
  echo "$unformatted" >&2
  exit 1
fi
