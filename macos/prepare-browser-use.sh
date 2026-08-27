#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
browser_environment="${HOME}/Library/Application Support/OpenRealtime/BrowserUse-0.12.6"

mkdir -p "$(dirname "${browser_environment}")"
UV_PROJECT_ENVIRONMENT="${browser_environment}" \
  uv sync --frozen --project "${script_dir}/BrowserUseBridge"

printf '%s\n' "Prepared browser-use in ${browser_environment}"
