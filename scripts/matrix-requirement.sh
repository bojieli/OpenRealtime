#!/usr/bin/env bash

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: scripts/matrix-requirement.sh matrix.json runtime-requirement-path" >&2
  exit 2
fi
matrix="$1"
requirement="$2"
if [[ ! -f "${matrix}" ]]; then
  echo "matrix is missing: ${matrix}" >&2
  exit 1
fi

# Read one preregistered runtime requirement, or refuse and name it.
#
# jq's alternative operator treats an explicit false and null as absent, so a
# `// empty` read followed by a skipped comparison is how an undeclared
# requirement used to read as a satisfied one: the launcher left that part of
# the runtime unconstrained and any gateway passed as a matching one. Walk the
# path with has() instead, so a missing, misplaced, or empty field is named
# and refused rather than defaulted.
if ! value="$(jq -er --arg requirement "${requirement}" '
  def walk($node; $path):
    if ($path | length) == 0 then $node
    elif ($node | type) != "object" then
      error("runtime_requirements.\($requirement) is unreachable: \($path[0]) is not under an object")
    elif ($node | has($path[0]) | not) then
      error("matrix does not preregister runtime_requirements.\($requirement)")
    else walk($node[$path[0]]; $path[1:])
    end;
  if ($requirement | length) == 0 then error("no runtime requirement was named")
  elif (.runtime_requirements | type) != "object" then
    error("runtime_requirements must be an object")
  else walk(.runtime_requirements; ($requirement | split("."))) end
  | if type == "string" then
      if . == "" then error("runtime_requirements.\($requirement) is empty") else . end
    elif type == "number" then tostring
    else error("runtime_requirements.\($requirement) must be a string or number, not \(type)")
    end
' "${matrix}" 2>&1)"; then
  echo "${value##*): } (${matrix})" >&2
  exit 1
fi
printf '%s\n' "${value}"
