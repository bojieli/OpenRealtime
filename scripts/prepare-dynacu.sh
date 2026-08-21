#!/usr/bin/env bash
#
# Prepares DynaCU-Bench: 150 browser tasks - 100 dynamic across ten categories
# that a screenshot-only agent cannot solve, and a static 50 that says whether
# perception cost anything where there was nothing to perceive.
#
#   scripts/prepare-dynacu.sh
#
# The benchmark stays in its own repository. This clones it at the revision
# this project measures against, checks that the environment can actually run
# it, and writes a manifest. Nothing is copied into this repository and nothing
# in the benchmark is patched: OpenRealtime is a strict superset of the GA
# Realtime protocol, so the suite's own provider-agnostic baseline points at it
# with a flag.

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/dataset-lib.sh
source "${repository_root}/scripts/dataset-lib.sh"

upstream="${DYNACU_UPSTREAM:-https://github.com/19PINE-AI/aoi.git}"
revision="$(grep -o 'PinnedRevision = "[0-9a-f]*"' "${repository_root}/bench/dynacu/dynacu.go" |
  head -1 | cut -d'"' -f2)"
runtime_root="${DYNACU_RUNTIME_ROOT:-${repository_root}/.runtime/dynacu-bench}"
checkout="${runtime_root}/aoi"
manifest="${runtime_root}/manifest.json"

if [[ -z "${revision}" ]]; then
  echo "could not read the pinned revision from bench/dynacu/dynacu.go" >&2
  exit 1
fi

require_commands git python3 jq

mkdir -p "${runtime_root}"

# A local clone source is honoured, because the environment may already be on
# this machine and cloning it again over the network is a slow way to get the
# same tree.
if [[ -n "${DYNACU_LOCAL_SOURCE:-}" && -d "${DYNACU_LOCAL_SOURCE}/.git" ]]; then
  upstream="${DYNACU_LOCAL_SOURCE}"
fi

if [[ ! -d "${checkout}/.git" ]]; then
  echo "cloning ${upstream}"
  git clone --quiet "${upstream}" "${checkout}"
fi

echo "pinning ${checkout} at ${revision}"
git -C "${checkout}" fetch --quiet --all --tags || true
if ! git -C "${checkout}" cat-file -e "${revision}^{commit}" 2>/dev/null; then
  echo "revision ${revision} is not in ${checkout}; is the clone source current?" >&2
  exit 1
fi
git -C "${checkout}" checkout --quiet --detach "${revision}"

actual="$(git -C "${checkout}" rev-parse HEAD)"
if [[ "${actual}" != "${revision}" ]]; then
  echo "checkout is at ${actual}, expected ${revision}" >&2
  exit 1
fi

# Everything below fails closed. Each of these is a way a run produces numbers
# that look fine and mean nothing, and finding out after six hours of browser
# automation is the expensive way to find out.
for required in benchmark_env/html_tasks dynacubench/tasks_v3.py aoi/realtime_baselines.py; do
  if [[ ! -e "${checkout}/${required}" ]]; then
    echo "the checkout is missing ${required}" >&2
    exit 1
  fi
done

pages="$(find "${checkout}/benchmark_env/html_tasks" -name '*.html' | wc -l | tr -d ' ')"
echo "task pages: ${pages}"

python_bin="${DYNACU_PYTHON:-python3}"
if [[ -x "${checkout}/.venv/bin/python" ]]; then
  python_bin="${checkout}/.venv/bin/python"
fi

declared="$(cd "${checkout}" && PYTHONPATH="${checkout}" "${python_bin}" - <<'PY'
import json
from pathlib import Path
from dynacubench.tasks_v3 import DynaCUBenchV3

bench = DynaCUBenchV3(html_tasks_dir=Path("benchmark_env/html_tasks"))
categories = {}
for task in bench:
    categories[task.category.value] = categories.get(task.category.value, 0) + 1
print(json.dumps({"declared": len(bench), "categories": categories}))
PY
)"

count="$(echo "${declared}" | jq -r '.declared')"
if [[ "${count}" != "150" ]]; then
  echo "the checkout declares ${count} tasks; this project measures a 150-task suite" >&2
  exit 1
fi
echo "tasks declared: ${count}"

missing=""
for module in playwright.sync_api websocket numpy PIL; do
  if ! PYTHONPATH="${checkout}" "${python_bin}" -c "import ${module}" >/dev/null 2>&1; then
    missing="${missing} ${module}"
  fi
done
if [[ -n "${missing}" ]]; then
  cat >&2 <<EOF
the interpreter ${python_bin} cannot run the suite; missing:${missing}

  pip install -r ${checkout}/requirements.txt websocket-client
  ${python_bin} -m playwright install chromium

Audio tasks additionally need PulseAudio and ffmpeg on the host.
EOF
  exit 1
fi

if ! PYTHONPATH="${checkout}" "${python_bin}" - <<'PY' >/dev/null 2>&1; then
from playwright.sync_api import sync_playwright
with sync_playwright() as play:
    browser = play.chromium.launch()
    browser.close()
PY
  echo "Chromium is not installed for Playwright; run: ${python_bin} -m playwright install chromium" >&2
  exit 1
fi
echo "chromium: ok"

jq -n \
  --arg upstream "${upstream}" \
  --arg revision "${revision}" \
  --arg checkout "${checkout}" \
  --arg python "${python_bin}" \
  --arg prepared_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson pages "${pages}" \
  --argjson registry "${declared}" \
  '{upstream: $upstream, revision: $revision, checkout: $checkout, python: $python,
    prepared_at: $prepared_at, html_pages: $pages, registry: $registry}' >"${manifest}"

cat <<EOF

DynaCU-Bench is prepared.

  checkout : ${checkout}
  revision : ${revision}
  manifest : ${manifest}

Run it against a server:

  openrealtime serve &
  openrealtime bench dynacu -aoi-dir ${checkout} -verify
  openrealtime bench dynacu -aoi-dir ${checkout} -out results/dynacu.json
EOF
