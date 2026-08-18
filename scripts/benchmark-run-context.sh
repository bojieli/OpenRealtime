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

if [[ "${action}" == start ]]; then
  if [[ -n "$(git -C "${repository_root}" status --porcelain=v1 --untracked-files=normal)" ]]; then
    echo "benchmark run context requires a clean OpenRealtime source tree" >&2
    exit 1
  fi
  jq -n \
    --arg benchmark "${benchmark}" \
    --arg started_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg revision "${revision}" \
    --argjson gateway_health "${gateway_health}" \
    --argjson runtime_identity "${runtime_identity}" \
    '{
      schema_version:"1.0.0",
      benchmark:$benchmark,
      status:"running",
      started_at:$started_at,
      openrealtime_revision_start:$revision,
      source_worktree_clean_start:true,
      gateway_health_start:$gateway_health,
      runtime_identity_start:$runtime_identity
    }' >"${temporary}"
else
  if [[ ! -f "${output}" ]]; then
    echo "benchmark run context is missing: ${output}" >&2
    exit 1
  fi
  if ! jq -e \
    --arg benchmark "${benchmark}" \
    --argjson runtime_identity "${runtime_identity}" \
    '.schema_version == "1.0.0" and .benchmark == $benchmark and
     .status == "running" and
     .runtime_identity_start.host_boot_id == $runtime_identity.host_boot_id and
     .runtime_identity_start.components == $runtime_identity.components' \
    "${output}" >/dev/null; then
    echo "benchmark runtime identity changed before completion" >&2
    exit 1
  fi
  jq \
    --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg revision "${revision}" \
    --argjson gateway_health "${gateway_health}" \
    --argjson runtime_identity "${runtime_identity}" \
    '.status="complete" |
     .completed_at=$completed_at |
     .openrealtime_revision_at_completion=$revision |
     .gateway_health_final=$gateway_health |
     .runtime_identity_final=$runtime_identity' \
    "${output}" >"${temporary}"
fi

mv "${temporary}" "${output}"
trap - EXIT
