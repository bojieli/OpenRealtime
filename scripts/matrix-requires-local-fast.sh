#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: scripts/matrix-requires-local-fast.sh matrix.json" >&2
  exit 2
fi
matrix="$1"
if [[ ! -f "${matrix}" ]]; then
  echo "matrix is missing: ${matrix}" >&2
  exit 1
fi

# jq's alternative operator treats both null and false as absent. Use an
# explicit key test so a declared false remains false while an omitted field
# keeps the backward-compatible local-fast default.
jq -r '
  if (.runtime_requirements | type) != "object" then
    error("runtime_requirements must be an object")
  elif (.runtime_requirements | has("requires_local_fast") | not) then
    true
  elif (.runtime_requirements.requires_local_fast | type) != "boolean" then
    error("runtime_requirements.requires_local_fast must be boolean")
  else
    .runtime_requirements.requires_local_fast
  end
' "${matrix}"
