#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

./scripts/reproduce_m5.sh

run_root="$(mktemp -d "$repo_root/artifacts/m6-reproduce.XXXXXX")"
trap 'rm -rf "$run_root"' EXIT

artifacts/openrealtime study build \
  --root "$repo_root" \
  --output "$run_root/study.json"
cmp "$run_root/study.json" benchmarks/releases/v0.1.0/study.json

artifacts/openrealtime release build \
  --root "$repo_root" \
  --output "$run_root/manifest.json"
cmp "$run_root/manifest.json" benchmarks/releases/v0.1.0/manifest.json

artifacts/openrealtime release verify \
  --root "$repo_root" \
  benchmarks/releases/v0.1.0/manifest.json

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
