#!/usr/bin/env bash
# Launch and measure the two local meeting-assistant foregrounds.
#
# Each long-running action stays in the foreground so process ownership is
# obvious. Use separate terminals (or a service manager) for policy,
# omni-sidecar, and serve. The script never kills an existing model process.

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

# shellcheck source=scripts/go-toolchain.sh
source "${repository_root}/scripts/go-toolchain.sh"
go_bin="$(openrealtime_go_bin)"

runtime_root="${MEETING_RUNTIME_ROOT:-${repository_root}/.runtime/meeting-assistant}"
binary="${runtime_root}/openrealtime"
python_bin="${MEETING_PYTHON:-${repository_root}/.runtime/qwen-asr/bin/python}"
policy_model="${MEETING_POLICY_MODEL:-Qwen/Qwen3-VL-8B-Instruct}"
policy_name="${MEETING_POLICY_NAME:-qwen-meeting-policy}"
policy_url="${MEETING_POLICY_URL:-http://127.0.0.1:8004/v1}"
visual_model="${MEETING_VISUAL_MODEL:-Qwen/Qwen3-VL-30B-A3B-Instruct-FP8}"
visual_name="${MEETING_VISUAL_NAME:-qwen-fast}"
visual_url="${MEETING_VISUAL_URL:-http://127.0.0.1:8000/v1}"
omni_model="${MEETING_OMNI_MODEL:-sammysun0711/Qwen3-Omni-30B-A3B-Instruct-FP8-Dynamic}"
omni_address="${MEETING_OMNI_ADDRESS:-127.0.0.1:9000}"
cascade_webrtc="${MEETING_CASCADE_WEBRTC_LISTEN:-127.0.0.1:28786}"
omni_webrtc="${MEETING_OMNI_WEBRTC_LISTEN:-127.0.0.1:28787}"
cascade_listen="${MEETING_CASCADE_LISTEN:-127.0.0.1:18786}"
omni_listen="${MEETING_OMNI_LISTEN:-127.0.0.1:18787}"
cascade_endpoint="${MEETING_CASCADE_ENDPOINT:-http://${cascade_webrtc}/v1/realtime}"
omni_endpoint="${MEETING_OMNI_ENDPOINT:-http://${omni_webrtc}/v1/realtime}"
meeting_transport="${MEETING_BENCH_TRANSPORT:-webrtc}"
slow_model="${MEETING_SLOW_MODEL:-gemini-3.5-flash}"
slow_provider="${MEETING_SLOW_PROVIDER:-google}"
slow_url="${MEETING_SLOW_URL:-}"
asr_url="${MEETING_ASR_URL:-http://127.0.0.1:8002/v1}"
asr_provider="${MEETING_ASR_PROVIDER:-sensevoice}"
asr_model="${MEETING_ASR_MODEL:-iic/SenseVoiceSmall}"
fish_url="${MEETING_FISH_URL:-http://127.0.0.1:8123/v1/tts}"
interaction_shadow="${MEETING_INTERACTION_SHADOW:-}"
interaction_liveness="${MEETING_INTERACTION_LIVENESS:-750ms}"
# The longest waveform pause is about 680 ms, but 100 ms frame/VAD boundaries
# made it roughly 890 ms online. Keep audible turn commitment above that while
# the independent visual/interaction trigger continues at 200 ms.
endpoint_silence_ms="${MEETING_ENDPOINT_SILENCE_MS:-1000}"
visual_reflex_timeout="${MEETING_VISUAL_REFLEX_TIMEOUT:-1000ms}"
visual_reflex_tokens="${MEETING_VISUAL_REFLEX_TOKENS:-96}"
cascade_reference_levels="F6=${slow_provider}:${slow_model}/high,F8=interaction:${policy_name},F9=voice:${policy_model}+visual:${visual_model},F12=${asr_provider}:${asr_model}"
omni_reference_levels="F6=${slow_provider}:${slow_model}/high,F8=interaction:${policy_name},F9=${omni_model},F12=${asr_provider}:${asr_model}"

mkdir -p "${runtime_root}" "${runtime_root}/results"

slow_endpoint_args=()
if [[ -n "${slow_url}" ]]; then
  slow_endpoint_args=(-slow-url "${slow_url}")
fi

interaction_shadow_args=()
if [[ -n "${interaction_shadow}" ]]; then
  interaction_shadow_args=(-interaction-shadow "${interaction_shadow}")
fi

build() {
  "${go_bin}" build -o "${binary}" ./cmd/openrealtime
}

need_python() {
  if [[ ! -x "${python_bin}" ]]; then
    echo "Python environment not found at ${python_bin}; set MEETING_PYTHON" >&2
    exit 1
  fi
}

need_slow_key() {
	case "${slow_provider}" in
		google|gemini) ;;
		*) return 0 ;;
	esac
  if [[ -z "${OPENREALTIME_SLOW_API_KEY:-}" && -z "${GEMINI_API_KEY:-}" && -z "${GOOGLE_API_KEY:-}" ]]; then
    echo "Gemini slow cognition needs OPENREALTIME_SLOW_API_KEY, GEMINI_API_KEY, or GOOGLE_API_KEY" >&2
    exit 1
  fi
}

wait_for() {
  local name="$1" url="$2" attempts="${3:-60}"
  local count
  for ((count = 0; count < attempts; count++)); do
    if curl -fsS --max-time 2 -o /dev/null "${url}"; then
      return 0
    fi
    sleep 1
  done
  echo "${name} did not answer ${url}" >&2
  return 1
}

verify_common() {
  wait_for "meeting ASR" "${asr_url%/v1}/health" 10
  wait_for "meeting policy/VLM" "${policy_url}/models" 10
	wait_for "meeting visual actor" "${visual_url}/models" 10
}

case "${1:-}" in
  policy)
    need_python
    # 8B is intentional. It leaves enough of a 96 GB card for the FP8 Omni
    # foreground in the native-audio cell, so both treatments use this exact
    # interaction-policy revision rather than changing the control model too.
    exec "${python_bin}" -m vllm.entrypoints.openai.api_server \
      --model "${policy_model}" \
      --served-model-name "${policy_name}" \
      --host 127.0.0.1 --port 8004 \
      --gpu-memory-utilization "${MEETING_POLICY_GPU_FRACTION:-0.20}" \
      --max-model-len "${MEETING_POLICY_CONTEXT:-16384}" \
      --enable-auto-tool-choice --tool-call-parser hermes
    ;;

  omni-sidecar)
    need_python
    # The TCP form loads one checkpoint once and shares it across benchmark
    # sessions. Starting the sidecar as a child per session would reload 35 GB
    # of weights four times and measure model loading rather than interaction.
    exec "${python_bin}" sidecars/qwen3_omni_sidecar.py \
      --model "${omni_model}" --listen "${omni_address}" \
      --max-new-tokens "${MEETING_OMNI_MAX_TOKENS:-192}"
    ;;

  cascade)
    need_slow_key
    verify_common
    wait_for "Fish speech" "${fish_url%/v1/tts}/health" 10
    build
    exec "${binary}" serve \
      -listen "${cascade_listen}" \
      -webrtc-listen "${cascade_webrtc}" \
      -binding cascade -profile voice+vision \
      -observers audio+video -observer-components keyframe \
      -asr-provider "${asr_provider}" -asr-url "${asr_url}" \
      -asr-model "${asr_model}" -asr-cadence 200ms -asr-partial-interval 200ms \
      -endpoint-silence "${endpoint_silence_ms}" \
      -fast-provider vllm -fast-url "${policy_url}" -fast-model "${policy_name}" \
      -fast-sees -fast-background-tools \
      -visual-reflex-provider vllm -visual-reflex-url "${visual_url}" \
      -visual-reflex-model "${visual_name}" -visual-reflex-timeout "${visual_reflex_timeout}" \
      -visual-reflex-max-tokens "${visual_reflex_tokens}" \
      -policy-models interaction -policy-url "${policy_url}" -policy-model "${policy_name}" \
      -interaction-floor -interaction-sees -policy-reasoning chat_template_kwargs \
      -interaction-liveness "${interaction_liveness}" \
      "${interaction_shadow_args[@]}" -profile-turns \
      -slow-provider "${slow_provider}" "${slow_endpoint_args[@]}" \
      -slow-model "${slow_model}" -slow-effort high \
      -tts-provider fish-audio -tts-url "${fish_url}" \
      -tts-model fishaudio/fish-speech-1.5 -speak-by-sentence \
      -trigger-cadence 200ms
    ;;

  omni)
    need_slow_key
    verify_common
    build
    exec "${binary}" serve \
      -listen "${omni_listen}" \
      -webrtc-listen "${omni_webrtc}" \
      -binding omni+text-policy -profile voice+vision \
      -observers audio+video \
      -sidecar-address "tcp:${omni_address}" -sidecar-protocol 3 \
      -sidecar-capabilities audio-input,audio-output,visual-input,turn-generation,interaction-acts,text-injection \
      -fast-computer-use \
      -asr-provider "${asr_provider}" -asr-url "${asr_url}" \
      -asr-model "${asr_model}" -asr-cadence 200ms -asr-partial-interval 200ms \
      -endpoint-silence "${endpoint_silence_ms}" \
      -policy-models interaction -policy-url "${policy_url}" -policy-model "${policy_name}" \
      -interaction-floor -interaction-sees -policy-reasoning chat_template_kwargs \
      -interaction-liveness "${interaction_liveness}" \
      "${interaction_shadow_args[@]}" -profile-turns \
      -slow-provider "${slow_provider}" "${slow_endpoint_args[@]}" \
      -slow-model "${slow_model}" -slow-effort high \
      -trigger-cadence 200ms
    ;;

  bench-cascade)
    build
    exec "${binary}" bench meeting -foreground cascade \
      -transport "${meeting_transport}" -endpoint "${cascade_endpoint}" \
      -reference-levels "${cascade_reference_levels}" \
      -fps 5 -analysis-delay 8s \
      -out "${runtime_root}/results/cascade.json" "${@:2}"
    ;;

  bench-omni)
    build
    exec "${binary}" bench meeting -foreground omni \
      -transport "${meeting_transport}" -endpoint "${omni_endpoint}" \
      -reference-levels "${omni_reference_levels}" \
      -fps 5 -analysis-delay 8s \
      -out "${runtime_root}/results/omni.json" "${@:2}"
    ;;

  conformance-omni)
    build
    exec "${binary}" conformance sidecar -protocol-version 3 \
      -address "tcp:${omni_address}"
    ;;

  *)
    cat >&2 <<'USAGE'
usage: scripts/meeting-assistant.sh <action> [benchmark flags]

Run long-lived actions in separate terminals:
  policy             local Qwen3-VL-8B interaction/control endpoint
  omni-sidecar       persistent Qwen3-Omni FP8 native-audio/direct-vision service
  cascade            ASR -> Qwen3-VL -> Fish foreground, Gemini slow
  omni               native-audio Omni foreground, same policy/ASR/Gemini slow

Measure or verify:
  bench-cascade      deterministic four-case recording suite
  bench-omni         the same suite and evaluator
  conformance-omni   protocol-v3 audio/image/tool boundary checks

The script never kills an existing GPU service. On a 96 GB card, stop an
unrelated large model server before starting the 8B policy and 35 GB FP8 Omni
services if nvidia-smi does not show sufficient free memory.
USAGE
    exit 2
    ;;
esac
