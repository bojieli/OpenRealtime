#!/usr/bin/env bash
# Full upstream tau2 validation. This is provisioned and opt-in because it may
# resolve Python dependencies; the release matrix reports a missing checkout
# or uv as blocked rather than pretending this gate passed offline.
set -euo pipefail

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
tau2_directory="${TAU2_DIR:-${repository_root}/.runtime/tau2-bench}"

if [[ ! -d "${tau2_directory}/.git" ]]; then
  printf 'prepared tau2 checkout is unavailable: %s\n' "${tau2_directory}" >&2
  exit 1
fi

TAU2_DIR="${tau2_directory}" "${repository_root}/scripts/prepare-tau-voice.sh" --verify

cd -- "${tau2_directory}"
uv sync --all-extras
uv run tau2 check-data
uv run ruff check .
uv run ruff format --check .
make test-all
uv run tests/test_voice/test_audio_native/run_provider_suite.py

# The upstream summarizer intentionally exits zero when every configured
# provider passes even if other provider rows were skipped. A complete release
# gate provisions the entire horizontal matrix; absent credentials or toggles
# must therefore remain blocked by the checked prerequisites, and any other
# skip is a failure rather than a silent pass.
provider_results="${tau2_directory}/tests/test_voice/test_audio_native/provider_suite_results.txt"
if ! tail -n 1 "${provider_results}" | grep -Eq '^[0-9]+ passed, 0 failed, 0 skipped, 0 errors in [0-9]+s$'; then
  printf 'provider suite was not complete; final summary: ' >&2
  tail -n 1 "${provider_results}" >&2
  exit 1
fi
