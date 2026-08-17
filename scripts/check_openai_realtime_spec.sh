#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
revision="2186421dca0cca7c1e67caa7739005e8b1ccc4dd"
source_sha256="542299d304cdeb78deff4172b3790d52c7e7e75fb2b517e9c2787c52f1424acc"
source_url="https://github.com/openai/openai-openapi/blob/${revision}/openapi.json"
raw_url="https://raw.githubusercontent.com/openai/openai-openapi/${revision}/openapi.json"
temporary_dir="$(mktemp -d)"
trap 'rm -rf "$temporary_dir"' EXIT

curl --fail --silent --show-error --location "$raw_url" \
  --output "$temporary_dir/openapi.json"

cd "$repo_root"
go run ./cmd/specsync \
  --source "$temporary_dir/openapi.json" \
  --source-url "$source_url" \
  --revision "$revision" \
  --sha256 "$source_sha256" \
  --retrieved-at 2026-08-17 \
  --schema-out "$temporary_dir/openai-realtime-events.schema.json" \
  --go-out "$temporary_dir/events_gen.go"

cmp "$temporary_dir/openai-realtime-events.schema.json" \
  protocol/openai/openai-realtime-events.schema.json
cmp "$temporary_dir/events_gen.go" protocol/openai/events_gen.go
