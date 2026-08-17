#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
mkdir -p artifacts

output_directory="${1:-$(mktemp -d "$repo_root/artifacts/m5-translation-demo.XXXXXX")}"
go run ./cmd/openrealtime benchmark m5 \
  --fixture tests/fixtures/m0-tone.wav \
  --demonstrations tests/fixtures/m5-demonstrations.json \
  --output "$output_directory" \
  --trials 30 \
  --seed 20260817

echo "Translation report: $output_directory/report.json"
echo "Translation timeline: $output_directory/timeline-translation-stable-trial-0000.html"
