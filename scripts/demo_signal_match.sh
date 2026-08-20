#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
source "$repo_root/scripts/go-toolchain.sh"
go_bin="$(openrealtime_go_bin)"
mkdir -p artifacts

output_directory="${1:-$(mktemp -d "$repo_root/artifacts/m5-signal-match-demo.XXXXXX")}"
"$go_bin" run ./cmd/openrealtime benchmark m5 \
  --fixture tests/fixtures/m0-tone.wav \
  --demonstrations tests/fixtures/m5-demonstrations.json \
  --output "$output_directory" \
  --trials 30 \
  --seed 20260817

echo "Signal Match report: $output_directory/report.json"
echo "Signal Match timeline: $output_directory/timeline-game-microturn-trial-0000.html"
