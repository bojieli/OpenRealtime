#!/usr/bin/env bash

set -euo pipefail

readonly TAU2_REVISION="c3398666e6559e3a063da3fc04b5acf7f941464e"
readonly TAU2_REPOSITORY="https://github.com/sierra-research/tau2-bench.git"
readonly PATCH_SHA256="f34fe88d5ca6342a272b7f64b13da690acb390ef9b30b951b9c6d97466d301ac"

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
tau2_directory="${TAU2_DIR:-${repository_root}/.runtime/tau2-bench}"
patch_file="${repository_root}/datasets/patches/0001-local-openai-fish-audio.patch"
verify=false

actual_patch_sha256="$(sha256sum "${patch_file}" | cut -d ' ' -f 1)"
if [[ "${actual_patch_sha256}" != "${PATCH_SHA256}" ]]; then
  echo "tau-Voice patch digest does not match the pinned manifest" >&2
  exit 1
fi

if [[ "${1:-}" == "--verify" ]]; then
  verify=true
elif [[ $# -ne 0 ]]; then
  echo "usage: scripts/prepare-tau-voice.sh [--verify]" >&2
  exit 2
fi

if [[ ! -d "${tau2_directory}/.git" ]]; then
  if [[ -e "${tau2_directory}" ]]; then
    echo "TAU2_DIR exists but is not a Git checkout: ${tau2_directory}" >&2
    exit 1
  fi
  git clone "${TAU2_REPOSITORY}" "${tau2_directory}"
  git -C "${tau2_directory}" checkout --detach "${TAU2_REVISION}"
fi

actual_revision="$(git -C "${tau2_directory}" rev-parse HEAD)"
if [[ "${actual_revision}" != "${TAU2_REVISION}" ]]; then
  echo "TAU2_DIR is at ${actual_revision}; expected ${TAU2_REVISION}" >&2
  exit 1
fi

if git -C "${tau2_directory}" apply --unidiff-zero --reverse --check "${patch_file}" >/dev/null 2>&1; then
  echo "OpenRealtime tau-Voice patch is already applied."
elif git -C "${tau2_directory}" diff --quiet && \
  [[ -z "$(git -C "${tau2_directory}" ls-files --others --exclude-standard)" ]]; then
  git -C "${tau2_directory}" apply --unidiff-zero --check "${patch_file}"
  git -C "${tau2_directory}" apply --unidiff-zero "${patch_file}"
  echo "Applied OpenRealtime tau-Voice patch."
else
  echo "TAU2_DIR has unrelated changes; refusing to overwrite them." >&2
  exit 1
fi

if [[ "${verify}" == true ]]; then
  uv --directory "${tau2_directory}" sync --extra voice --extra dev
  PYTHONPATH="${tau2_directory}/src" uv --directory "${tau2_directory}" run ruff check \
    src/tau2/agent/base/voice.py \
    src/tau2/agent/discrete_time_audio_native_agent.py \
    src/tau2/cli.py \
    src/tau2/data_model/simulation.py \
    src/tau2/data_model/voice.py \
    src/tau2/runner/batch.py \
    src/tau2/runner/build.py \
    src/tau2/voice/audio_native/adapter.py \
    src/tau2/voice/audio_native/openai \
    src/tau2/voice/synthesis \
    src/tau2/voice/utils/fish_audio_utils.py \
    src/tau2/voice_config.py \
    tests/test_voice/test_audio_native/test_openai_provider_config.py \
    tests/test_voice/test_fish_audio_synthesis.py
  PYTHONPATH="${tau2_directory}/src" uv --directory "${tau2_directory}" run pytest -q \
    tests/test_voice/test_audio_native/test_openai_provider_config.py \
    tests/test_voice/test_fish_audio_synthesis.py \
    tests/test_voice/test_audio_effects.py \
    tests/test_streaming/test_voice_streaming_user_simulator.py
fi

echo "tau-Voice checkout: ${tau2_directory}"
echo "revision: ${TAU2_REVISION}"
