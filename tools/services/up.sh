#!/usr/bin/env bash
# Bring up every local model this system needs, and wait until each answers.
#
# There are five, they live in four different virtual environments, and the
# order matters only in that the largest should claim its memory first. Written
# down because a GPU fault took all of them out at once and bringing them back
# was five commands nobody had recorded in one place - and because a service that
# is up but not yet answering looks exactly like a service that is down.
set -euo pipefail

# Every service started below is served from this repository - tools/whisper,
# tools/fish15, tools/speakerid, deploy/sensevoice - so all three roots default
# to this checkout and a clone runs the script without configuration. They stay
# separable because runtime state (virtualenvs, model checkouts, voice packs,
# logs) need not sit beside the sources, and on this machine it does not.
repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
repository="${REPOSITORY:-${repository_root}}"
shared="${SHARED:-${repository_root}}"
runtime="${RUNTIME_ROOT:-${shared}}"
logs="${repository}/.runtime/services"
mkdir -p "${logs}"

wait_for() {
  local name="$1" url="$2" seconds="${3:-600}"
  local waited=0
  until curl -fsS -m 2 -o /dev/null "${url}" 2>/dev/null; do
    sleep 3
    waited=$((waited + 3))
    if [ "${waited}" -ge "${seconds}" ]; then
      echo "${name} did not answer ${url} within ${seconds}s; see ${logs}/${name}.log" >&2
      return 1
    fi
  done
  echo "${name} answering after ${waited}s"
}

# Loopback specifically. "sport = :8000" also matches a listener bound to some
# other address - a docker-bridge socat on 172.17.0.1:8000 satisfied it here -
# and the script would then report every service up while never starting the
# one nothing was listening for.
running() { ss -ltnH "src 127.0.0.1:$1" 2>/dev/null | grep -q .; }

# The decider and the voice. Vision matters: -fast-sees hands it frames, and a
# text-only checkpoint here makes the visual scenarios unwinnable.
if ! running 8000; then
  qwen_repository="${HOME}/.cache/huggingface/hub/models--Qwen--Qwen3-VL-30B-A3B-Instruct-FP8"
  qwen_revision="$(tr -d '[:space:]' < "${qwen_repository}/refs/main")"
  qwen_snapshot="${qwen_repository}/snapshots/${qwen_revision}"
  if [[ ! "${qwen_revision}" =~ ^[0-9a-f]{40}$ ]] || [[ ! -d "${qwen_snapshot}" ]]; then
    echo "Qwen immutable snapshot revision is unavailable" >&2
    exit 1
  fi
  ( setsid nohup env VLLM_WORKER_MULTIPROC_METHOD=spawn \
      "${runtime}/.runtime/qwen-asr/bin/python" -m vllm.entrypoints.openai.api_server \
        --model "${qwen_snapshot}" \
        --served-model-name qwen-fast \
        --host 127.0.0.1 --port 8000 \
        --gpu-memory-utilization 0.50 \
        --max-model-len 40960 \
        --enable-auto-tool-choice \
        --tool-call-parser hermes \
      > "${logs}/qwen.log" 2>&1 & )
fi

# The recogniser.
if ! running 8002; then
  sensevoice_model_path="${SENSEVOICE_MODEL_PATH:-${HOME}/.cache/modelscope/models/iic--SenseVoiceSmall/snapshots/master}"
  if [[ ! -d "${sensevoice_model_path}" ]]; then
    echo "SenseVoice exact local model path is unavailable" >&2
    exit 1
  fi
  ( cd "${shared}/deploy/sensevoice" && setsid nohup \
      env SENSEVOICE_MODEL="iic/SenseVoiceSmall" SENSEVOICE_MODEL_PATH="${sensevoice_model_path}" \
      "${runtime}/.runtime/sensevoice/bin/python" -m uvicorn server:app \
        --host 127.0.0.1 --port 8002 --workers 1 --log-level info \
      > "${logs}/sensevoice.log" 2>&1 & )
fi

# A second recogniser, more accurate and faster than the first on anything
# with a proper noun in it. An existing port is adopted only after the service
# itself verifies the exact listener process, model material, and live health.
whisper_repository="${HOME}/.cache/huggingface/hub/models--mobiuslabsgmbh--faster-whisper-large-v3-turbo"
whisper_revision="$(tr -d '[:space:]' < "${whisper_repository}/refs/main")"
whisper_snapshot="${whisper_repository}/snapshots/${whisper_revision}"
if [[ ! "${whisper_revision}" =~ ^[0-9a-f]{40}$ ]] || [[ ! -d "${whisper_snapshot}" ]]; then
  echo "Whisper immutable snapshot revision is unavailable" >&2
  exit 1
fi
whisper_python="${runtime}/.runtime/sensevoice/bin/python"
whisper_service="${shared}/tools/whisper/server.py"
whisper_dependency_roots_value="${OPENREALTIME_CU_WHISPER_DEPENDENCY_ROOTS:-${HOME}/.local/lib/python3.10/site-packages:/usr/lib/python3/dist-packages}"
IFS=: read -r -a whisper_dependency_roots <<< "${whisper_dependency_roots_value}"
if [ "${#whisper_dependency_roots[@]}" -eq 0 ]; then
  echo "Whisper explicit dependency roots are unavailable" >&2
  exit 1
fi
whisper_arguments=(
  --model "${whisper_snapshot}" --device cuda --compute-type int8
  --language en
)
for dependency_root in "${whisper_dependency_roots[@]}"; do
  if [[ "${dependency_root}" != /* ]] || [[ ! -d "${dependency_root}" ]] ||
      [[ "$(readlink -f -- "${dependency_root}")" != "${dependency_root}" ]]; then
    echo "Whisper dependency root is not one exact canonical directory" >&2
    exit 1
  fi
  whisper_arguments+=(--dependency-root "${dependency_root}")
done
whisper_arguments+=(--port 8003)
whisper_seal_exec='
import fcntl, hashlib, os, stat, sys
service, python = sys.argv[1:3]
source = os.open(service, os.O_RDONLY | os.O_CLOEXEC | os.O_NOFOLLOW)
try:
    before = os.fstat(source)
    if not stat.S_ISREG(before.st_mode) or before.st_size > 4 << 20:
        raise RuntimeError("Whisper service source is not a bounded regular file")
    payload = b""
    while len(payload) <= 4 << 20:
        block = os.read(source, min(1024 * 1024, (4 << 20) + 1 - len(payload)))
        if not block:
            break
        payload += block
    after = os.fstat(source)
finally:
    os.close(source)
identity = lambda value: (value.st_dev, value.st_ino, value.st_size, value.st_mtime_ns, value.st_ctime_ns)
if identity(before) != identity(after) or len(payload) != before.st_size:
    raise RuntimeError("Whisper service source changed while sealing")
if "sha256:" + hashlib.sha256(payload).hexdigest() != "sha256:9187b0f04d47ea36d94b1decf25c32f2df095ad098364c2026e3ce49d8637d4a":
    raise RuntimeError("Whisper service source differs from the pinned reviewed implementation")
handle = os.memfd_create("openrealtime-whisper-service-v2", os.MFD_ALLOW_SEALING)
offset = 0
while offset < len(payload):
    offset += os.write(handle, payload[offset:])
os.lseek(handle, 0, os.SEEK_SET)
fcntl.fcntl(handle, fcntl.F_ADD_SEALS,
            fcntl.F_SEAL_SEAL | fcntl.F_SEAL_SHRINK | fcntl.F_SEAL_GROW | fcntl.F_SEAL_WRITE)
if handle != 3:
    os.dup2(handle, 3)
    os.close(handle)
os.set_inheritable(3, True)
os.execv(python, [python, "-I", "-S", "-B", "/proc/self/fd/3", *sys.argv[3:]])
'
if running 8003; then
  env -u PYTHONHOME -u PYTHONPATH "${whisper_python}" -I -S -B -c "${whisper_seal_exec}" \
    "${whisper_service}" "${whisper_python}" "${whisper_arguments[@]}" \
    --verify-listener --working-directory "${shared}"
else
  ( cd "${shared}" && setsid nohup \
      env -u PYTHONHOME -u PYTHONPATH "${whisper_python}" -I -S -B -c "${whisper_seal_exec}" \
        "${whisper_service}" "${whisper_python}" "${whisper_arguments[@]}" \
      > "${logs}/whisper.log" 2>&1 & )
fi

# The synthesiser. Startup compiles CUDA graphs and warms the decoder, about
# twenty seconds, and anything arriving before that would pay for it.
if ! running 8123; then
  fish_model_repository="${FISH_SPEECH_MODEL_REPOSITORY:-${HOME}/.cache/huggingface/hub/models--fishaudio--fish-speech-1.5}"
  fish_revision="$(tr -d '[:space:]' < "${fish_model_repository}/refs/main")"
  fish_snapshot="${fish_model_repository}/snapshots/${fish_revision}"
  fish_source="${repository}/.runtime/fish/fish-speech"
  if [[ ! "${fish_revision}" =~ ^[0-9a-f]{40}$ ]] || [[ ! -d "${fish_snapshot}" ]]; then
    echo "Fish Speech immutable snapshot revision is unavailable" >&2
    exit 1
  fi
  ( cd "${fish_source}" && setsid nohup \
      env PYTHONPATH="${fish_source}" \
      "${runtime}/.runtime/fish-env/bin/python" "${shared}/tools/fish15/server.py" \
        --checkpoint "${fish_snapshot}" --device cuda \
        --port 8123 --voices "${repository}/.runtime/fish-voices" \
      > "${logs}/fish.log" 2>&1 & )
fi

# Where each word of the agent's own speech fell, so an interruption resumes
# from the word the person heard rather than from a proportional guess. This is
# a different question from the recogniser's, and a different service: :8003
# answers this route with text and no word array, which the runtime reports as
# ErrNoWordTimes and then falls back to the estimate. Nothing used to start
# this, so the default room never measured a boundary.
if ! running 8127; then
  ( cd "${shared}/deploy/wordtimings" && setsid nohup \
      env WORD_TIMINGS_MODEL="${WORD_TIMINGS_MODEL:-Systran/faster-whisper-base.en}" \
          WORD_TIMINGS_DEVICE="${WORD_TIMINGS_DEVICE:-cuda}" \
          WORD_TIMINGS_LANGUAGE="${WORD_TIMINGS_LANGUAGE:-en}" \
      "${runtime}/.runtime/wordtimings/bin/python" -m uvicorn server:app \
        --host 127.0.0.1 --port 8127 --log-level info \
      > "${logs}/wordtimings.log" 2>&1 & )
fi

# Who is speaking.
if ! running 8124; then
  ( cd "${repository}" && setsid nohup \
      "${runtime}/.runtime/sensevoice/bin/python" tools/speakerid/server.py --port 8124 \
      > "${logs}/speakerid.log" 2>&1 & )
fi

wait_for speaker-id http://127.0.0.1:8124/health 120
wait_for synthesiser http://127.0.0.1:8123/health 300
wait_for recogniser http://127.0.0.1:8003/health 300
wait_for sensevoice http://127.0.0.1:8002/health 300
wait_for word-timing http://127.0.0.1:8127/health 300
wait_for decider http://127.0.0.1:8000/health 900
echo "all five answering"
