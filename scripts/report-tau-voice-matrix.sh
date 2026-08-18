#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
matrix="${1:-${repository_root}/benchmarks/tau-voice/matrix-canonical-v1.json}"
if [[ $# -gt 0 ]]; then
  shift
fi
output="${1:-}"
if [[ $# -gt 0 ]]; then
  shift
fi
tau2_directory="${TAU2_DIR:-${repository_root}/.runtime/tau2-bench}"

if [[ ! -f "${matrix}" ]]; then
  echo "tau-Voice matrix does not exist: ${matrix}" >&2
  exit 1
fi
if [[ ! -d "${tau2_directory}" ]]; then
  echo "tau2 checkout does not exist: ${tau2_directory}" >&2
  exit 1
fi

arguments=(
  "${repository_root}/scripts/report-tau-voice-matrix.py"
  --repository-root "${repository_root}"
  --tau2-dir "${tau2_directory}"
  --matrix "$(realpath "${matrix}")"
)
if [[ -n "${output}" ]]; then
  arguments+=(--output "$(realpath -m "${output}")")
fi
arguments+=("$@")

PYTHONPATH="${tau2_directory}/src" \
  uv --directory "${tau2_directory}" run python "${arguments[@]}"
