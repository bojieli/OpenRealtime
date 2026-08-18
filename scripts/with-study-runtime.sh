#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
manifest="${OPENREALTIME_STUDY_RUNTIME_MANIFEST:-${repository_root}/benchmarks/runtime/canonical-gateway-v1.json}"
if [[ $# -eq 0 ]]; then
  echo "usage: scripts/with-study-runtime.sh command [arguments ...]" >&2
  exit 2
fi

gateway_binary="$("${repository_root}/scripts/prepare-study-gateway.sh")"
gateway_sha256="$(jq -r '.binary_sha256' "${manifest}")"
export OPENREALTIME_GATEWAY_BINARY="${gateway_binary}"
export OPENREALTIME_GATEWAY_SHA256="${gateway_sha256}"
exec "$@"
