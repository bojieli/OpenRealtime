#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
registry="${repository_root}/benchmarks/tau-voice/voices/fish-s2pro-personas-v1.json"
provenance="${repository_root}/benchmarks/tau-voice/voices/fish-s2pro-personas-v1.provenance.json"
endpoint="${FISH_AUDIO_BASE_URL:-http://127.0.0.1:8081}"
artifact_dir="${repository_root}/.runtime/benchmark-runs/tau-voice/voices/fish-s2pro-personas-v1"
receipt="${artifact_dir}/receipt.json"

mkdir -p "${artifact_dir}"

if ! curl --fail --silent --show-error "${endpoint}/health" >/dev/null; then
  echo "Fish Audio is not healthy at ${endpoint}" >&2
  exit 1
fi

reference_text="$(jq -r '.reference_text' "${provenance}")"
consent="$(jq -r '.consent_statement' "${provenance}")"
entries_file="${artifact_dir}/entries.jsonl"
: >"${entries_file}"

while IFS=$'\t' read -r persona voice seed style_tag; do
  expected_voice="$(jq -r --arg persona "${persona}" '.[$persona]' "${registry}")"
  if [[ "${expected_voice}" != "${voice}" ]]; then
    echo "registry/provenance mismatch for ${persona}" >&2
    exit 1
  fi

  wav_path="${artifact_dir}/${persona}.wav"
  request_text="${style_tag} ${reference_text}"
  request_json="$(jq -n \
    --arg model "fishaudio/s2-pro" \
    --arg input "${request_text}" \
    --argjson seed "${seed}" \
    '{model:$model,input:$input,voice:"default",response_format:"wav",stream:false,seed:$seed}')"
  curl --fail --silent --show-error \
    -H 'Content-Type: application/json' \
    -d "${request_json}" \
    "${endpoint}/v1/audio/speech" \
    -o "${wav_path}"

  upload_response="$(curl --fail --silent --show-error \
    -F "audio_sample=@${wav_path};type=audio/wav" \
    -F "consent=${consent}" \
    -F "name=${voice}" \
    -F "ref_text=${request_text}" \
    -F "speaker_description=${style_tag}" \
    "${endpoint}/v1/audio/voices")"
  sha256="$(sha256sum "${wav_path}" | cut -d ' ' -f 1)"
  bytes="$(stat -c '%s' "${wav_path}")"
  jq -nc \
    --arg persona "${persona}" \
    --arg voice "${voice}" \
    --argjson seed "${seed}" \
    --arg style_tag "${style_tag}" \
    --arg request_text "${request_text}" \
    --arg sha256 "${sha256}" \
    --argjson bytes "${bytes}" \
    --argjson upload "${upload_response}" \
    '{persona:$persona,voice:$voice,seed:$seed,style_tag:$style_tag,request_text:$request_text,wav_sha256:$sha256,wav_bytes:$bytes,upload:$upload}' \
    >>"${entries_file}"
done < <(jq -r '.entries[] | [.persona,.voice,(.seed|tostring),.style_tag] | @tsv' "${provenance}")

jq -n \
  --arg generated_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg endpoint "${endpoint}" \
  --arg registry_sha256 "$(sha256sum "${registry}" | cut -d ' ' -f 1)" \
  --arg provenance_sha256 "$(sha256sum "${provenance}" | cut -d ' ' -f 1)" \
  --slurpfile entries "${entries_file}" \
  '{schema_version:"1.0.0",generated_at:$generated_at,endpoint:$endpoint,registry_sha256:$registry_sha256,provenance_sha256:$provenance_sha256,entries:$entries}' \
  >"${receipt}"

registered="$(curl --fail --silent --show-error "${endpoint}/v1/audio/voices?names_only=true")"
while IFS= read -r voice; do
  if ! jq -e --arg voice "${voice}" '.uploaded_voice_names | index($voice) != null' <<<"${registered}" >/dev/null; then
    echo "Fish Audio did not retain uploaded voice ${voice}" >&2
    exit 1
  fi
done < <(jq -r '.[]' "${registry}" | sort -u)

echo "Fish tau-Voice registry ready: ${receipt}"
