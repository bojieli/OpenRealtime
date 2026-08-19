#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
helper="${repository_root}/scripts/matrix-requirement.sh"
manifest="${repository_root}/benchmarks/full-study-v1.json"
canonical="${repository_root}/benchmarks/tau-voice/matrix-canonical-v1.json"

# The runtime requirements run-tau-voice-matrix.sh compares against the live
# gateway. Every matrix the full study admits must preregister all of them, or
# the launcher would leave that part of the runtime unconstrained.
required=(
  gateway.executable_sha256
  asr.model
  asr.provider_chunk_ms
  gateway_profiles.fast.provider
  gateway_profiles.fast.model
  gateway_profiles.fast.effort
  gateway_profiles.fast.tool_authority
  gateway_profiles.slow.provider
  gateway_profiles.slow.model
  gateway_profiles.slow.effort
  gateway_profiles.slow.tool_authority
  slow_context
  preparation_policy
  speech.name
  speech.version
)

matrix_count=0
while IFS= read -r matrix_relative; do
  matrix="${repository_root}/${matrix_relative}"
  matrix_count=$((matrix_count + 1))
  for requirement in "${required[@]}"; do
    if ! "${helper}" "${matrix}" "${requirement}" >/dev/null; then
      echo "admitted matrix omits ${requirement}: ${matrix_relative}" >&2
      exit 1
    fi
  done
done < <(jq -r '.tau_voice.matrices[].path' "${manifest}")
if [[ "${matrix_count}" -lt 1 ]]; then
  echo "full-study manifest admitted no tau-Voice matrix" >&2
  exit 1
fi

if [[ "$("${helper}" "${canonical}" speech.name)" \
  != "openai-speech-streaming/fishaudio/s2-pro" ]]; then
  echo "canonical matrix did not report its preregistered speech backend" >&2
  exit 1
fi
if [[ "$("${helper}" "${canonical}" asr.provider_chunk_ms)" != 200 ]]; then
  echo "numeric requirement was not returned as a comparable scalar" >&2
  exit 1
fi

fixture_root="$(mktemp -d /tmp/openrealtime-matrix-requirement-test.XXXXXX)"
trap 'rm -rf -- "${fixture_root}"' EXIT
jq 'del(.runtime_requirements.speech)' "${canonical}" >"${fixture_root}/absent.json"
jq '.runtime_requirements.speech.name = ""' "${canonical}" >"${fixture_root}/empty.json"
jq '.runtime_requirements.speech.name = null' "${canonical}" >"${fixture_root}/null.json"
jq '.runtime_requirements.speech.name = false' "${canonical}" >"${fixture_root}/boolean.json"
jq '.runtime_requirements.speech.name = {"model": "x"}' "${canonical}" >"${fixture_root}/object.json"
jq '.runtime_requirements.speech = "fishaudio"' "${canonical}" >"${fixture_root}/scalar.json"
jq 'del(.runtime_requirements)' "${canonical}" >"${fixture_root}/no-requirements.json"
for rejected in absent empty null boolean object scalar no-requirements; do
  if "${helper}" "${fixture_root}/${rejected}.json" speech.name >/dev/null 2>&1; then
    echo "unusable speech declaration was accepted: ${rejected}" >&2
    exit 1
  fi
done
if "${helper}" "${fixture_root}/missing-file.json" speech.name >/dev/null 2>&1; then
  echo "missing matrix file was accepted" >&2
  exit 1
fi
if "${helper}" "${canonical}" "" >/dev/null 2>&1; then
  echo "empty requirement path was accepted" >&2
  exit 1
fi
if "${helper}" "${canonical}" >/dev/null 2>&1; then
  echo "helper accepted a call with no requirement argument" >&2
  exit 1
fi

# The refusal has to name the offending field: an unattended queue records
# only this line, and "the matrix is wrong" is not a reproducible report.
refusal="$("${helper}" "${fixture_root}/absent.json" speech.name 2>&1 || true)"
if [[ "${refusal}" != *runtime_requirements.speech.name* ]]; then
  echo "refusal did not name the absent requirement: ${refusal}" >&2
  exit 1
fi

echo "matrix requirement tests pass"
