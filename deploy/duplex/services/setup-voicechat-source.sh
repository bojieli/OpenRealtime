#!/usr/bin/env bash
# Fetch the last pre-framework serving revision retaining VoiceChat duplex.
# Uses the existing vllm-omni-native environment; this only prepares source.
set -euo pipefail
repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
source_dir="${repository}/.runtime/duplex-plan/src/vllm-omni-voicechat"
revision=9005d789033b8c3ec5876a7a68c4e2d9238f5c69
if [[ -e "${source_dir}" ]]; then
  [[ "$(git -C "${source_dir}" rev-parse HEAD)" == "${revision}" ]] || {
    echo "existing VoiceChat source has a different revision: ${source_dir}" >&2
    exit 1
  }
  [[ -z "$(git -C "${source_dir}" status --porcelain)" ]] || {
    echo "existing VoiceChat source has uncommitted changes" >&2
    exit 1
  }
else
  mkdir -p "${source_dir}"
  git -C "${source_dir}" init
  git -C "${source_dir}" remote add origin https://github.com/vllm-project/vllm-omni.git
  git -C "${source_dir}" fetch --depth 1 origin "${revision}"
  git -C "${source_dir}" checkout --detach FETCH_HEAD
fi
printf 'VoiceChat source ready: %s at %s\n' "${source_dir}" "${revision}"
