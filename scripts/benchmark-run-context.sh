#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
action="${1:-}"
output="${2:-}"
benchmark="${3:-}"

if [[ "${action}" != start && "${action}" != complete ]] || \
  [[ -z "${output}" || -z "${benchmark}" ]]; then
  echo "usage: scripts/benchmark-run-context.sh start|complete OUTPUT BENCHMARK" >&2
  exit 2
fi

mkdir -p "$(dirname "${output}")"
temporary="${output}.next.$$"
cleanup() {
  rm -f "${temporary}"
}
trap cleanup EXIT

gateway_health="$(curl --fail --silent --show-error http://127.0.0.1:8765/healthz)"
runtime_identity="$("${repository_root}/scripts/capture-local-runtime-identity.sh")"
revision="$(git -C "${repository_root}" rev-parse HEAD)"
expected_gateway_sha256="${OPENREALTIME_GATEWAY_SHA256:-}"
if [[ -n "${expected_gateway_sha256}" ]]; then
  if [[ ! "${expected_gateway_sha256}" =~ ^[0-9a-f]{64}$ ]]; then
    echo "OPENREALTIME_GATEWAY_SHA256 is invalid" >&2
    exit 1
  fi
  if ! jq -e --arg sha256 "${expected_gateway_sha256}" \
    '.components.gateway.executable_sha256 == $sha256' \
    <<<"${runtime_identity}" >/dev/null; then
    echo "gateway binary does not match OPENREALTIME_GATEWAY_SHA256" >&2
    exit 1
  fi
fi

if [[ "${action}" == start ]]; then
  if [[ -n "$(git -C "${repository_root}" status --porcelain=v1 --untracked-files=normal)" ]]; then
    echo "benchmark run context requires a clean OpenRealtime source tree" >&2
    exit 1
  fi
  invocation="$(jq -n \
    --arg benchmark "${benchmark}" \
    --arg started_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg revision "${revision}" \
    --arg study_gateway_sha256 "${expected_gateway_sha256}" \
    --argjson gateway_health "${gateway_health}" \
    --argjson runtime_identity "${runtime_identity}" \
    '{
      status:"running",
      started_at:$started_at,
      openrealtime_revision_start:$revision,
      source_worktree_clean_start:true,
      study_gateway_sha256:(if $study_gateway_sha256 == "" then null else $study_gateway_sha256 end),
      gateway_health_start:$gateway_health,
      runtime_identity_start:$runtime_identity
    }')"
  if [[ -f "${output}" ]]; then
    if ! jq -e \
      --arg benchmark "${benchmark}" \
      '.schema_version == "1.0.0" and .benchmark == $benchmark and
       (.invocations | type == "array")' "${output}" >/dev/null; then
      echo "prior benchmark run context is incompatible: ${output}" >&2
      exit 1
    fi
    jq \
      --arg interrupted_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --argjson invocation "${invocation}" \
      'if .status == "running" and .invocations[-1].status == "running" then
         .invocations[-1].status="interrupted" |
         .invocations[-1].interrupted_at=$interrupted_at
       else . end |
       .status="running" |
       .invocations += [$invocation]' \
      "${output}" >"${temporary}"
  else
    jq -n \
      --arg benchmark "${benchmark}" \
      --argjson invocation "${invocation}" \
      '{
        schema_version:"1.0.0",
        benchmark:$benchmark,
        status:"running",
        invocations:[$invocation]
      }' >"${temporary}"
  fi
else
  if [[ ! -f "${output}" ]]; then
    echo "benchmark run context is missing: ${output}" >&2
    exit 1
  fi
  if ! jq -e \
    --arg benchmark "${benchmark}" \
    --argjson runtime_identity "${runtime_identity}" \
    '.schema_version == "1.0.0" and .benchmark == $benchmark and
     .status == "running" and .invocations[-1].status == "running" and
     .invocations[-1].runtime_identity_start.host_boot_id == $runtime_identity.host_boot_id and
     .invocations[-1].runtime_identity_start.components == $runtime_identity.components' \
    "${output}" >/dev/null; then
    echo "benchmark runtime identity changed before completion" >&2
    exit 1
  fi
  if [[ -n "${expected_gateway_sha256}" ]] && ! jq -e \
    --arg sha256 "${expected_gateway_sha256}" \
    '.invocations[-1].study_gateway_sha256 == $sha256' \
    "${output}" >/dev/null; then
    echo "benchmark start was not bound to the current frozen gateway" >&2
    exit 1
  fi
  jq \
    --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg revision "${revision}" \
    --argjson gateway_health "${gateway_health}" \
    --argjson runtime_identity "${runtime_identity}" \
    '.status="complete" |
     .invocations[-1].status="complete" |
     .invocations[-1].completed_at=$completed_at |
     .invocations[-1].openrealtime_revision_at_completion=$revision |
     .invocations[-1].gateway_health_final=$gateway_health |
     .invocations[-1].runtime_identity_final=$runtime_identity' \
    "${output}" >"${temporary}"
fi

mv "${temporary}" "${output}"
trap - EXIT
