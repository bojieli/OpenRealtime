#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
helper="${repository_root}/scripts/matrix-requires-local-fast.sh"

if [[ "$("${helper}" "${repository_root}/benchmarks/tau-voice/matrix-fast-gemini-v1.json")" != false ]]; then
  echo "Gemini-fast matrix did not preserve its explicit false declaration" >&2
  exit 1
fi
if [[ "$("${helper}" "${repository_root}/benchmarks/tau-voice/matrix-canonical-v1.json")" != true ]]; then
  echo "canonical matrix did not retain its local-fast requirement" >&2
  exit 1
fi

fixture_root="$(mktemp -d /tmp/openrealtime-matrix-local-fast-test.XXXXXX)"
trap 'rm -rf -- "${fixture_root}"' EXIT
printf '%s\n' '{"runtime_requirements":{}}' >"${fixture_root}/omitted.json"
printf '%s\n' '{"runtime_requirements":{"requires_local_fast":"false"}}' >"${fixture_root}/invalid.json"
if [[ "$("${helper}" "${fixture_root}/omitted.json")" != true ]]; then
  echo "omitted local-fast declaration did not select the compatible default" >&2
  exit 1
fi
if "${helper}" "${fixture_root}/invalid.json" >/dev/null 2>&1; then
  echo "non-Boolean local-fast declaration was accepted" >&2
  exit 1
fi

echo "matrix local-fast requirement tests pass"
