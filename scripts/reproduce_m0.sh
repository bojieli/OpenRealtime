#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
source "$repo_root/scripts/go-toolchain.sh"
go_bin="$(openrealtime_go_bin)"

mkdir -p artifacts
"$go_bin" mod download
"$go_bin" build -trimpath -ldflags='-s -w' -o artifacts/openrealtime ./cmd/openrealtime
./scripts/check_openai_realtime_spec.sh

artifacts/openrealtime fixture generate artifacts/m0-tone.wav
artifacts/openrealtime replay artifacts/m0-tone.wav \
  --events artifacts/m0-openai-events.jsonl \
  --trace artifacts/m0-trace.jsonl \
  --session-id m0-replay \
  --frame-ms 20
artifacts/openrealtime protocol validate \
  --profile realtime \
  --direction client \
  artifacts/m0-openai-events.jsonl
artifacts/openrealtime trace validate artifacts/m0-trace.jsonl
artifacts/openrealtime trace summarize artifacts/m0-trace.jsonl

cmp artifacts/m0-tone.wav tests/fixtures/m0-tone.wav
cmp artifacts/m0-openai-events.jsonl tests/golden/m0-openai-events.jsonl
cmp artifacts/m0-trace.jsonl tests/golden/m0-trace.jsonl

"$go_bin" test -race ./...
"$go_bin" vet ./...
unformatted="$(gofmt -l .)"
if [[ -n "$unformatted" ]]; then
  echo "unformatted Go files:" >&2
  echo "$unformatted" >&2
  exit 1
fi
