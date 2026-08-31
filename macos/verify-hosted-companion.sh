#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repository_root="$(cd "${script_dir}/.." && pwd)"
application="${1:-${script_dir}/.build/OpenRealtime Developer.app}"

if [[ "$(uname -s)" != "Darwin" ]]; then
  printf '%s\n' "hosted companion smoke requires macOS" >&2
  exit 1
fi
if [[ ! -d "${application}" || -L "${application}" || \
      ! -x "${application}/Contents/MacOS/OpenRealtimeMac" || \
      -L "${application}/Contents/MacOS/OpenRealtimeMac" ]]; then
  printf '%s\n' "hosted companion smoke requires the exact assembled application" >&2
  exit 1
fi

runtime_root="$(mktemp -d "${TMPDIR%/}/openrealtime-hosted-companion.XXXXXX")"
companion_pid=""
application_pid=""
native_endpoint_file=""

cleanup() {
  if [[ -n "${application_pid}" ]] && kill -0 "${application_pid}" 2>/dev/null; then
    kill -TERM "${application_pid}" 2>/dev/null || true
    wait "${application_pid}" 2>/dev/null || true
  fi
  if [[ -n "${companion_pid}" ]] && kill -0 "${companion_pid}" 2>/dev/null; then
    kill -INT "${companion_pid}" 2>/dev/null || true
    wait "${companion_pid}" 2>/dev/null || true
  fi
  case "${runtime_root}" in
    "${TMPDIR%/}"/openrealtime-hosted-companion.*)
      rm -rf -- "${runtime_root}"
      ;;
    *)
      printf '%s\n' "refusing unexpected hosted companion cleanup path: ${runtime_root}" >&2
      ;;
  esac
}
trap cleanup EXIT INT TERM

server_address="127.0.0.1:18765"
webrtc_address="127.0.0.1:18766"
presentation_address="127.0.0.1:18767"
server_url="http://${server_address}"
presentation_url="http://${presentation_address}"
companion_log="${runtime_root}/companion.log"
browser_log="${runtime_root}/browser.log"
application_log="${runtime_root}/application.log"
binary="${runtime_root}/openrealtime"
proof_nonce="$(openssl rand -hex 32)"

cd "${repository_root}"
go build -trimpath -o "${binary}" ./cmd/openrealtime
export OPENREALTIME_HOSTED_COMPANION_TOKEN="hosted-companion-private-token"
"${binary}" companion \
  -server-listen "${server_address}" \
  -webrtc-listen "${webrtc_address}" \
  -presentation-listen "${presentation_address}" \
  -client none \
  -token-env OPENREALTIME_HOSTED_COMPANION_TOKEN \
  -ready-timeout 90s \
  -shutdown-timeout 10s \
  >"${companion_log}" 2>&1 &
companion_pid="$!"

wait_for_http() {
  local endpoint="$1"
  local status="$2"
  local observed=""
  for _ in $(seq 1 900); do
    if ! kill -0 "${companion_pid}" 2>/dev/null; then
      printf '%s\n' "companion stopped before ${endpoint} became ready" >&2
      sed -n '1,240p' "${companion_log}" >&2
      return 1
    fi
    observed="$(curl -sS -o /dev/null -w '%{http_code}' "${endpoint}" 2>/dev/null || true)"
    if [[ "${observed}" == "${status}" ]]; then
      return 0
    fi
    sleep 0.1
  done
  printf '%s\n' "timed out waiting for ${endpoint}: status=${observed}" >&2
  sed -n '1,240p' "${companion_log}" >&2
  return 1
}

metrics_match() {
  local started="$1"
  local completed="$2"
  local failed="$3"
  curl -fsS "${server_url}/metrics" 2>/dev/null | \
    python3 -c 'import json, sys
value = json.load(sys.stdin)
want = tuple(map(int, sys.argv[1:4]))
got = (value.get("sessions_started"), value.get("sessions_completed"), value.get("sessions_failed"))
raise SystemExit(0 if got == want else 1)' "${started}" "${completed}" "${failed}"
}

wait_for_metrics() {
  local started="$1"
  local completed="$2"
  local failed="$3"
  for _ in $(seq 1 600); do
    if metrics_match "${started}" "${completed}" "${failed}"; then
      return 0
    fi
    sleep 0.1
  done
  printf '%s\n' "timed out waiting for sessions started=${started} completed=${completed} failed=${failed}" >&2
  curl -fsS "${server_url}/metrics" >&2 || true
  sed -n '1,240p' "${companion_log}" >&2
  sed -n '1,240p' "${application_log}" >&2 || true
  return 1
}

server_identity() {
  curl -fsS "${server_url}/healthz" | python3 -c 'import json, sys
value = json.load(sys.stdin)
identity = {
  "binding": value.get("binding"),
  "model": value.get("model"),
  "protocol": value.get("protocol"),
  "server_profile": (value.get("server_profile") or {}).get("fingerprint"),
}
print(json.dumps(identity, sort_keys=True, separators=(",", ":")))'
}

companion_children() {
  ps -axo pid=,ppid= | awk -v parent="${companion_pid}" '$2 == parent { print $1 }' | sort -n | tr '\n' ' '
}

wait_for_http "${server_url}/healthz" 200
wait_for_http "${presentation_url}/client/v1/manifest" 200
wait_for_http "${server_url}/" 404
wait_for_http "${server_url}/client/v1/manifest" 404

for _ in $(seq 1 100); do
  native_endpoint_file="$(sed -n 's/^  native file  //p' "${companion_log}" | tail -n 1)"
  if [[ -n "${native_endpoint_file}" && -f "${native_endpoint_file}" && ! -L "${native_endpoint_file}" ]]; then
    break
  fi
  sleep 0.1
done
if [[ -z "${native_endpoint_file}" || ! -f "${native_endpoint_file}" || -L "${native_endpoint_file}" ]]; then
  printf '%s\n' "companion did not publish its generated exact native endpoint directory" >&2
  sed -n '1,240p' "${companion_log}" >&2
  exit 1
fi
python3 -c 'import os, stat, sys
path = sys.argv[1]
info = os.stat(path, follow_symlinks=False)
valid = (
    os.path.isabs(path) and stat.S_ISREG(info.st_mode) and
    stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1 and
    os.path.basename(path) == "native-observer-endpoints.json"
)
if not valid: raise SystemExit("generated native endpoint directory is not one private exact file")' \
  "${native_endpoint_file}"

initial_server_identity="$(server_identity)"
initial_children="$(companion_children)"
if [[ "$(wc -w <<<"${initial_children}")" -ne 2 ]]; then
  printf '%s\n' "companion child population is not exact: ${initial_children}" >&2
  exit 1
fi

chromium=""
for candidate in \
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
  "/Applications/Chromium.app/Contents/MacOS/Chromium"; do
  if [[ -x "${candidate}" ]]; then
    chromium="${candidate}"
    break
  fi
done
if [[ -z "${chromium}" ]]; then
  printf '%s\n' "hosted macOS image has no supported Chromium executable" >&2
  exit 1
fi

browser_manifest="$(curl -fsS "${presentation_url}/client/v1/manifest")"
browser_manifest_fingerprint="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["fingerprint"])' <<<"${browser_manifest}")"
browser_plan_fingerprint="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["plan"]["fingerprint"])' <<<"${browser_manifest}")"
OPENREALTIME_COMPANION_PROOF_NONCE="${proof_nonce}" \
  CHROMIUM="${chromium}" CDP_PORT=18768 \
  node cmd/openrealtime/testdata/companion_browser.mjs "${presentation_url}" | tee "${browser_log}"
wait_for_metrics 1 1 0

browser_proof="$(sed -n 's/^OPENREALTIME_COMPANION_BROWSER_PROOF //p' "${browser_log}" | tail -n 1)"
browser_session_id="$(python3 -c 'import json, re, sys
value = json.loads(sys.argv[1])
valid = (
  value.get("schema") == "openrealtime/browser/hosted-companion-proof/v1" and
  value.get("nonce") == sys.argv[2] and
  value.get("transport") == "webrtc" and
  value.get("manifest_fingerprint") == sys.argv[3] and
  value.get("plan_fingerprint") == sys.argv[4] and
  value.get("endpoint") == sys.argv[5] and
  re.fullmatch(r"sess_[A-Za-z0-9_-]{1,128}", value.get("session_id", ""))
)
if not valid: raise SystemExit("browser proof is invalid")
print(value["session_id"])' "${browser_proof}" "${proof_nonce}" "${browser_manifest_fingerprint}" \
  "${browser_plan_fingerprint}" "${presentation_url}")"

manifest_resource="$(find "${application}/Contents/Resources" -type f \
  -name native-observer-client-manifest.json -print)"
if [[ "$(wc -l <<<"${manifest_resource}")" -ne 1 || ! -f "${manifest_resource}" ]]; then
  printf '%s\n' "assembled app does not contain exactly one observer manifest" >&2
  exit 1
fi
expected_manifest_fingerprint="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["manifest_fingerprint"])' "${manifest_resource}")"
expected_endpoint_fingerprint="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["fingerprint"])' "${native_endpoint_file}")"
expected_native_endpoint="ws://${presentation_address}/client/v1/realtime"

executable="${application}/Contents/MacOS/OpenRealtimeMac"
executable_digest="$(shasum -a 256 "${executable}" | awk '{print $1}')"
OPENREALTIME_NATIVE_PROFILE=observer-developer \
OPENREALTIME_NATIVE_ENDPOINT_DIRECTORY="${native_endpoint_file}" \
  "${executable}" "--openrealtime-hosted-smoke=${proof_nonce}" \
  >"${application_log}" 2>&1 &
application_pid="$!"

native_proof=""
for _ in $(seq 1 900); do
  native_proof="$(sed -n 's/^OPENREALTIME_HOSTED_COMPANION_PROOF //p' "${application_log}" | tail -n 1)"
  if [[ -n "${native_proof}" ]]; then
    break
  fi
  if ! kill -0 "${application_pid}" 2>/dev/null; then
    printf '%s\n' "assembled native app exited before emitting its connection proof" >&2
    sed -n '1,240p' "${application_log}" >&2
    exit 1
  fi
  sleep 0.1
done
if [[ -z "${native_proof}" ]]; then
  printf '%s\n' "timed out waiting for the assembled native app connection proof" >&2
  sed -n '1,240p' "${application_log}" >&2
  exit 1
fi
native_session_id="$(python3 -c 'import json, re, sys
value = json.loads(sys.argv[1])
valid = (
  value.get("schema") == "openrealtime/macos/hosted-companion-proof/v1" and
  value.get("nonce") == sys.argv[2] and
  value.get("transport") == "websocket" and
  value.get("distribution") == "observer-developer" and
  value.get("manifest_fingerprint") == sys.argv[3] and
  value.get("endpoint_fingerprint") == sys.argv[4] and
  value.get("endpoint") == sys.argv[5] and
  re.fullmatch(r"sess_[A-Za-z0-9_-]{1,128}", value.get("session_id", ""))
)
if not valid: raise SystemExit("native proof is invalid")
print(value["session_id"])' "${native_proof}" "${proof_nonce}" "${expected_manifest_fingerprint}" \
  "${expected_endpoint_fingerprint}" "${expected_native_endpoint}")"
if [[ "${browser_session_id}" == "${native_session_id}" ]]; then
  printf '%s\n' "browser and native app reported the same session identity" >&2
  exit 1
fi

for _ in $(seq 1 300); do
  if ! kill -0 "${application_pid}" 2>/dev/null; then
    break
  fi
  sleep 0.1
done
if kill -0 "${application_pid}" 2>/dev/null; then
  printf '%s\n' "native hosted smoke did not shut down after its one connection" >&2
  exit 1
fi
if ! wait "${application_pid}"; then
  printf '%s\n' "assembled native app exited unsuccessfully" >&2
  sed -n '1,240p' "${application_log}" >&2
  exit 1
fi
application_pid=""
wait_for_metrics 2 2 0

wait_for_http "${server_url}/" 404
wait_for_http "${server_url}/client/v1/manifest" 404
wait_for_http "${presentation_url}/client/v1/manifest" 200
if [[ "$(server_identity)" != "${initial_server_identity}" || \
      "$(companion_children)" != "${initial_children}" ]]; then
  printf '%s\n' "server or companion process identity changed between browser and native sessions" >&2
  exit 1
fi

kill -INT "${companion_pid}"
if ! wait "${companion_pid}"; then
  printf '%s\n' "companion supervisor did not stop cleanly" >&2
  sed -n '1,240p' "${companion_log}" >&2
  exit 1
fi
companion_pid=""
if [[ -e "${native_endpoint_file}" ]]; then
  printf '%s\n' "companion retained its generated native endpoint directory after shutdown" >&2
  exit 1
fi
listeners_free() {
  python3 -c 'import socket, sys
for raw in sys.argv[1:]:
    host, port = raw.rsplit(":", 1)
    sock = socket.socket()
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.bind((host, int(port)))
    sock.close()' "${server_address}" "${webrtc_address}" "${presentation_address}" "127.0.0.1:18768"
}
for _ in $(seq 1 100); do
  if listeners_free; then
    break
  fi
  sleep 0.1
done
if ! listeners_free; then
  printf '%s\n' "companion, browser, or native listener survived bounded shutdown" >&2
  exit 1
fi

printf '%s\n' "hosted companion browser and assembled native app passed on one clean server"
printf '%s\n' "browser session=${browser_session_id} native session=${native_session_id}"
printf '%s\n' "native executable sha256=${executable_digest}"
