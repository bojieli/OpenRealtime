#!/usr/bin/env bash
# Fetch the last pre-framework serving revision retaining VoiceChat duplex.
# Uses the existing vllm-omni-native environment; this only prepares source.
set -euo pipefail
repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
source_dir="${repository}/.runtime/duplex-plan/src/vllm-omni-voicechat"
revision=9005d789033b8c3ec5876a7a68c4e2d9238f5c69
mode="${1:-base}"
[[ "$mode" == base || "$mode" == --boundary-diagnostics ]] || {
  echo "usage: $0 [--boundary-diagnostics]" >&2; exit 2;
}
patch_file="$repository/deploy/duplex/patches/voicechat-boundary-diagnostics.patch"
if [[ -e "${source_dir}" ]]; then
  [[ "$(git -C "${source_dir}" rev-parse HEAD)" == "${revision}" ]] || {
    echo "existing VoiceChat source has a different revision: ${source_dir}" >&2
    exit 1
  }
  if [[ -n "$(git -C "${source_dir}" status --porcelain)" ]]; then
    # Accept only the exact retained diagnostic patch; never overwrite other
    # local edits or silently treat a different runtime as this experiment.
    if [[ "$mode" == --boundary-diagnostics ]] &&
       [[ -z "$(git -C "$source_dir" ls-files --others --exclude-standard)" ]] &&
       [[ -z "$(git -C "$source_dir" diff --cached)" ]] &&
       diff -q <(git -C "$source_dir" diff --no-ext-diff --binary) "$patch_file" >/dev/null; then
      echo "VoiceChat boundary diagnostics already applied"
      exit 0
    fi
    echo "existing VoiceChat source has uncommitted changes beyond the requested setup" >&2
    exit 1
  fi
else
  mkdir -p "${source_dir}"
  git -C "${source_dir}" init
  git -C "${source_dir}" remote add origin https://github.com/vllm-project/vllm-omni.git
  git -C "${source_dir}" fetch --depth 1 origin "${revision}"
  git -C "${source_dir}" checkout --detach FETCH_HEAD
fi
if [[ "$mode" == --boundary-diagnostics ]]; then
  git -C "$source_dir" apply --check "$patch_file"
  git -C "$source_dir" apply "$patch_file"
fi
printf 'VoiceChat source ready: %s at %s\n' "${source_dir}" "${revision}"
