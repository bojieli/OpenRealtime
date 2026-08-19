#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

# The full-study launcher preflights `python3`, and `python` is absent from a
# stock Ubuntu PATH: it exists here only through a personal ~/bin symlink. A
# queue that invokes bare `python` therefore reproduces on this machine and
# nowhere else, and it fails at the end of a multi-day serial chain rather than
# at launch. Every documented and scripted invocation must name the
# preflighted interpreter, or an explicit venv interpreter path.
bare_python_sites() {
  local root="$1"
  grep -rnE '(^[[:space:]]*|[|(&;][[:space:]]*)python[[:space:]]' \
    --include='*.sh' --include='*.md' \
    --exclude-dir=.git --exclude-dir=.runtime \
    "${root}" 2>/dev/null |
    grep -vE 'python3|/bin/python|run[[:space:]]+python' || true
}

sites="$(bare_python_sites "${repository_root}")"
if [[ -n "${sites}" ]]; then
  echo "study scripts must invoke the preflighted python3 interpreter:" >&2
  printf '%s\n' "${sites}" >&2
  exit 1
fi

fixture_root="$(mktemp -d /tmp/openrealtime-interpreter-pinning-test.XXXXXX)"
trap 'rm -rf -- "${fixture_root}"' EXIT
printf '%s\n' \
  'python "${root}/scripts/report.py" --output out.json' \
  >"${fixture_root}/unpinned.sh"
printf '%s\n' \
  'if [[ true ]]; then' \
  '  python "${root}/scripts/judge.py" --strict' \
  'fi' \
  >"${fixture_root}/unpinned-indented.sh"
for fixture in unpinned unpinned-indented; do
  if [[ -z "$(bare_python_sites "${fixture_root}/${fixture}.sh")" ]]; then
    echo "an unpinned python invocation went undetected: ${fixture}" >&2
    exit 1
  fi
done
rm -f "${fixture_root}"/unpinned*.sh
printf '%s\n' \
  'python3 "${root}/scripts/report.py"' \
  '  "${root}/.runtime/evaluator-venv/bin/python" -m pip check' \
  '  uv --directory "${tau}" run python -m tau2 run' \
  >"${fixture_root}/pinned.sh"
if [[ -n "$(bare_python_sites "${fixture_root}")" ]]; then
  echo "a correctly pinned interpreter was reported as unpinned" >&2
  exit 1
fi

echo "study interpreter pinning tests pass"
