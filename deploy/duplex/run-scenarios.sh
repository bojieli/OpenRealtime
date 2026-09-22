#!/usr/bin/env bash
# Run the project's twelve interaction scenarios against one duplex profile.
#
#   deploy/duplex/run-scenarios.sh <profile> [repeat] [case ...]
#
# The scenarios are the repository's own end-to-end check: a scripted
# participant speaks into the agent's session and the run is scored on whether
# the agent did the right thing at the right moment. They need two services
# besides the profile's own: a voice for the participants and a recogniser to
# score what the agent was audibly saying.
#
#   SCENARIO_SPEECH_URL     default http://127.0.0.1:9125/v1/audio/speech (Kyutai TTS)
#   SCENARIO_TRANSCRIBE_URL default http://127.0.0.1:9105/v1             (SenseVoice)
#
# Audio-only profiles cannot pass the case that requires sight; it is reported
# as a failure rather than hidden, because the suite is the same for every
# profile.
set -uo pipefail

profile="${1:?profile name}"
repeat="${2:-1}"
shift 2 2>/dev/null || shift 1 2>/dev/null || true
repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "${repository}"
config="deploy/duplex/profiles/${profile}.yaml"
[[ -f "${config}" ]] || { echo "no profile ${config}" >&2; exit 2; }
binary="${OPENREALTIME_BIN:-${repository}/.runtime/duplex-plan/bin/openrealtime}"
port="${SCENARIO_PORT:-9291}"
speech="${SCENARIO_SPEECH_URL:-http://127.0.0.1:9125/v1/audio/speech}"
speech_model="${SCENARIO_SPEECH_MODEL:-kyutai/tts-1.6b-en_fr}"
transcribe="${SCENARIO_TRANSCRIBE_URL:-http://127.0.0.1:9105/v1}"
transcribe_model="${SCENARIO_TRANSCRIBE_MODEL:-iic/SenseVoiceSmall}"
out="${repository}/.runtime/duplex-plan/results/scenario/${profile}"
mkdir -p "${out}"
rm -f "${out}"/record.jsonl "${out}"/run.log

cat > "${out}/run.json" <<JSON
{"profile": "${profile}", "revision": "$(git rev-parse HEAD)", "started": "$(date -Is)",
 "load_average": "$(cut -d' ' -f1-3 /proc/loadavg)", "repeat": ${repeat},
 "participant_voice": "${speech} ${speech_model}", "scoring_recogniser": "${transcribe} ${transcribe_model}"}
JSON

"${binary}" serve -config "${config}" -listen "127.0.0.1:${port}" > "${out}/server.log" 2>&1 &
server=$!
trap 'kill -- -${server} 2>/dev/null || kill ${server} 2>/dev/null || true' EXIT
for _ in $(seq 1 300); do
  curl -fsS -m 2 -o /dev/null "http://127.0.0.1:${port}/healthz" 2>/dev/null && break
  kill -0 "${server}" 2>/dev/null || { echo "server exited; see ${out}/server.log" >&2; exit 1; }
  sleep 1
done

arguments=(-url "ws://127.0.0.1:${port}/v1/realtime" -repeat "${repeat}"
  -speech-url "${speech}" -speech-model "${speech_model}"
  -transcribe-url "${transcribe}" -transcribe-model "${transcribe_model}"
  -record "${out}/record.jsonl")
for name in "$@"; do
  arguments+=(-case "${name}")
done
"${binary}" scenario "${arguments[@]}" 2>&1 | tee "${out}/run.log"
echo "{\"finished\": \"$(date -Is)\"}" > "${out}/finished.json"
echo "results in ${out}"
