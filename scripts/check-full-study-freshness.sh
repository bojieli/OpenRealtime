#!/usr/bin/env bash

set -euo pipefail

default_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
manifest="${1:-${default_root}/benchmarks/full-study-v1.json}"
repository_root="${2:-${default_root}}"

if [[ $# -gt 2 ]]; then
  echo "usage: scripts/check-full-study-freshness.sh [MANIFEST [REPOSITORY_ROOT]]" >&2
  exit 2
fi
repository_root="$(cd "${repository_root}" && pwd -P)"
if [[ "${manifest}" != /* ]]; then
  manifest="${repository_root}/${manifest}"
fi
if [[ ! -f "${manifest}" ]]; then
  echo "full-study manifest is missing: ${manifest}" >&2
  exit 1
fi

while IFS= read -r matrix_relative; do
  matrix="${repository_root}/${matrix_relative}"
  matrix_id="$(jq -r '.matrix_id' "${matrix}")"
  seed="$(jq -r '.benchmark.seed' "${matrix}")"
  run_root="${repository_root}/.runtime/benchmark-runs/tau-voice/${matrix_id}"
  if [[ -d "${run_root}" ]] && \
    [[ -n "$(find "${run_root}" -type f -name run.json -print -quit)" ]]; then
    echo "full-study tau invocation evidence already exists: ${run_root}" >&2
    exit 1
  fi
  while IFS=$'\t' read -r cell domain; do
    experiment="${repository_root}/.runtime/tau2-bench/data/simulations/${matrix_id}-${cell}-${domain}-seed${seed}"
    if [[ -e "${experiment}" ]]; then
      echo "full-study tau population is not fresh: ${experiment}" >&2
      exit 1
    fi
  done < <(jq -r \
    '.cells[].id as $cell | .benchmark.domains[].name as $domain | [$cell,$domain] | @tsv' \
    "${matrix}")
done < <(jq -r '.tau_voice.matrices[].path' "${manifest}")

for suite in full_duplex_bench_v1_5 full_duplex_bench_v3 fd_bench; do
  context_relative="$(jq -r --arg suite "${suite}" '.[$suite].run_context' "${manifest}")"
  run_root="$(dirname "${repository_root}/${context_relative}")"
  if [[ -e "${run_root}" ]]; then
    echo "full-study ${suite} run root is not fresh: ${run_root}" >&2
    exit 1
  fi
done

echo "full-study population roots are fresh"
