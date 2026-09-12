#!/usr/bin/env bash
set -euo pipefail
umask 077

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
browser_snapshot="${runtime_root}/browser-live.json"
native_snapshot="${runtime_root}/native-live.json"
binary="${runtime_root}/openrealtime"
proof_validator="${runtime_root}/companionproof"
process_validator="${runtime_root}/companionprocess"
proof_nonce="$(openssl rand -hex 32)"
gateway_token="$(openssl rand -hex 32)"

cd "${repository_root}"
go build -trimpath -o "${binary}" ./cmd/openrealtime
go build -trimpath -o "${proof_validator}" ./internal/cmd/companionproof
go build -trimpath -o "${process_validator}" ./internal/cmd/companionprocess
/usr/bin/codesign --force --sign - "${binary}"
/usr/bin/codesign --verify --strict "${binary}"
server_executable_digest="$(shasum -a 256 "${binary}" | awk '{print $1}')"
server_executable_file_identity="$(stat -f '%d:%i:%z:%m:%p' "${binary}")"
server_code_directory_hash="$(/usr/bin/codesign -d --verbose=4 "${binary}" 2>&1 | \
  sed -n 's/^CDHash=//p')"
if [[ ! "${server_executable_digest}" =~ ^[0-9a-f]{64}$ || \
      ! "${server_code_directory_hash}" =~ ^[0-9a-f]{40}$ ]]; then
  printf '%s\n' "temporary companion server lacks an exact signed executable identity" >&2
  exit 1
fi
# `-binding cascade` names an explicit server composition, and both halves of
# what that avoids matter on this runner.
#
# Without it the companion assembles the twelve-scenario room pipeline into a
# generated launch profile, and reading a profile file back securely is
# implemented for Linux only - so on macOS serve exits with "secure
# launch-profile file opening is unsupported on this platform" before it
# listens. That strict profile would also supersede the two provider flags
# below, which is the error this script hit first: "-launch-profile supersedes
# flags -slow-model, -slow-provider".
#
# Naming the binding restores what this smoke test always meant: compose the
# cascade from these flags, with a stand-in reasoner that needs no credential
# and is never dialled, because what is under test is supervision, routing, the
# native endpoint directory, and the gateway credential never becoming visible.
OPENREALTIME_HOSTED_COMPANION_TOKEN="${gateway_token}" "${binary}" companion \
  -server-listen "${server_address}" \
  -webrtc-listen "${webrtc_address}" \
  -presentation-listen "${presentation_address}" \
  -client none \
  -token-env OPENREALTIME_HOSTED_COMPANION_TOKEN \
  -ready-timeout 90s \
  -shutdown-timeout 10s \
  -- \
  -binding cascade \
  -slow-provider vllm \
  -slow-model hosted-companion-smoke \
  >"${companion_log}" 2>&1 &
companion_pid="$!"

# The server's health inventory and its metrics are served only to a caller
# that presents the gateway's bearer token, which this launch configures. The
# token goes in through a curl config file on stdin rather than on the command
# line, because this script also asserts that the credential never becomes
# visible - and argv is visible to every process on the machine.
server_curl() {
  curl --config - "$@" <<CURLRC
header = "Authorization: Bearer ${gateway_token}"
CURLRC
}

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
  server_curl -fsS "${server_url}/metrics" 2>/dev/null | \
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
  server_curl -fsS "${server_url}/metrics" >&2 || true
  sed -n '1,240p' "${companion_log}" >&2
  sed -n '1,240p' "${application_log}" >&2 || true
  return 1
}

server_identity() {
  server_curl -fsS "${server_url}/healthz" | python3 -c 'import json, sys
value = json.load(sys.stdin)
profile = value.get("server_profile") or {}
entries = profile.get("entries") or {}
exports = profile.get("exports") or {}
expected_entries = {"gateway", "http-router", "inspection", "observability", "realtime", "session-api", "sessions"}
if value.get("binding") != "cascade" or value.get("model") != "openrealtime":
  raise SystemExit("companion server selected an unexpected binding or model")
if value.get("ownership") is None or value.get("capabilities") is None:
  raise SystemExit("companion server omitted ownership or capabilities")
protocol = value.get("protocol") or {}
if protocol.get("openai_realtime") != "pinned" or (protocol.get("openrealtime") or {}).get("version") != 1:
  raise SystemExit("companion server selected an unexpected protocol profile")
if set(entries) != expected_entries or set(exports) != {"realtime_http"} or profile.get("format_version") != 1 or profile.get("realm") != "server" or profile.get("state") != "active":
  raise SystemExit("companion server plugin population is not exact")
stable_entries = {}
runtime_digests = set()
for name, entry in entries.items():
  runtime = entry.get("runtime") or {}
  digest = runtime.get("digest", "")
  services = entry.get("services") or []
  if entry.get("state") != "active" or entry.get("desired") is not True or entry.get("error") not in (None, ""):
    raise SystemExit("companion server plugin is not active")
  if not isinstance(digest, str) or len(digest) != 71 or not digest.startswith("sha256:"):
    raise SystemExit("companion server plugin lacks runtime identity")
  runtime_digests.add(digest)
  stable_entries[name] = {
    "identity": entry.get("identity"), "implementation": entry.get("implementation"),
    "runtime": runtime, "state": entry.get("state"), "desired": entry.get("desired"),
    "services": services,
  }
if len(runtime_digests) != 1:
  raise SystemExit("linked companion server plugins do not share one executable identity")
stable_exports = {}
for name, export in exports.items():
  if export.get("available") is not True or not isinstance(export.get("revision"), int) or export["revision"] <= 0:
    raise SystemExit("companion server export is unavailable")
  stable_exports[name] = export
identity = {
  "binding": value.get("binding"),
  "model": value.get("model"),
  "ownership": value.get("ownership"),
  "capabilities": value.get("capabilities"),
  "protocol": value.get("protocol"),
  "server_profile": profile.get("fingerprint"),
  "entries": stable_entries,
  "exports": stable_exports,
}
print(json.dumps(identity, sort_keys=True, separators=(",", ":")))'
}

listener_owner() {
  /usr/sbin/lsof -nP -iTCP:"$1" -sTCP:LISTEN -Fp 2>/dev/null | \
    sed -n 's/^p//p' | sort -u
}

assert_frozen_companion_phase() {
  local phase="$1"
  local process_receipt=""
  if [[ "$(server_identity)" != "${initial_server_identity}" ]]; then
    printf '%s\n' "server identity changed ${phase}" >&2
    return 1
  fi
  if ! process_receipt="$("${process_validator}" -binary "${binary}" \
    -companion-pid "${companion_pid}" -runtime-digest "${server_runtime_digest}")"; then
    printf '%s\n' "companion process identity could not be revalidated ${phase}" >&2
    return 1
  fi
  if [[ "${process_receipt}" != "${initial_process_receipt}" ]]; then
    printf '%s\n' "companion process receipt changed ${phase}" >&2
    return 1
  fi
  if [[ "$(listener_owner 18765)" != "${server_pid}" || \
        "$(listener_owner 18766)" != "${server_pid}" || \
        "$(listener_owner 18767)" != "${presentation_pid}" || \
        -n "$(listener_owner 18768)" ]]; then
    printf '%s\n' "companion listener ownership changed ${phase}" >&2
    return 1
  fi
}

assert_no_private_material() {
  local files=("${companion_log}" "${browser_log}" "${application_log}" \
    "${browser_snapshot}" "${native_snapshot}")
  if ! printf '%s' "${gateway_token}" | python3 -c 'import re, sys
secret = sys.stdin.buffer.read()
for path in sys.argv[1:]:
  with open(path, "rb") as source: payload = source.read((64 << 20) + 1)
  if len(payload) > (64 << 20) or secret in payload or re.search(rb"mgmt_[A-Za-z0-9_-]{16,512}", payload):
    raise SystemExit(1)' "${files[@]}"; then
    printf '%s\n' "hosted companion evidence retained credential-shaped private material" >&2
    return 1
  fi
  if [[ "${management_receipt:-}" == *"${gateway_token}"* || \
        "${management_receipt:-}" == *"mgmt_"* ]]; then
    printf '%s\n' "hosted companion receipt retained private material" >&2
    return 1
  fi
}

wait_for_http "${server_url}/healthz" 200
wait_for_http "${presentation_url}/client/v1/manifest" 200
wait_for_http "${server_url}/" 404
wait_for_http "${server_url}/client/v1/manifest" 404
wait_for_metrics 0 0 0

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
server_identity_digest="sha256:$(printf '%s' "${initial_server_identity}" | shasum -a 256 | awk '{print $1}')"
server_runtime_digest="$(python3 -c 'import json,sys
value=json.loads(sys.argv[1]); entries=value.get("entries") or {}
digests={((entry.get("runtime") or {}).get("digest")) for entry in entries.values()}
if len(digests) != 1: raise SystemExit("server runtime digest is not unique")
print(next(iter(digests)))' "${initial_server_identity}")"
initial_process_receipt="$("${process_validator}" -binary "${binary}" \
  -companion-pid "${companion_pid}" -runtime-digest "${server_runtime_digest}")"
if [[ "${initial_process_receipt}" != OPENREALTIME_COMPANION_PROCESS_RECEIPT\ * ]]; then
  printf '%s\n' "Darwin process validator omitted its exact receipt" >&2
  exit 1
fi
process_roles="$(python3 -c 'import json,sys
value=json.loads(sys.argv[1].split(" ",1)[1])
if value.get("schema") != "openrealtime/companion/process-receipt/v2":
  raise SystemExit("Darwin process receipt schema is not exact")
def validate(role, expected):
  process=value.get(role) or {}
  listeners=process.get("tcp_listeners")
  if not isinstance(listeners,list) or len(listeners) != len(expected):
    raise SystemExit(role+" TCP listener count is not exact")
  observed=set()
  descriptors=set()
  for listener in listeners:
    if set(listener) != {"fd","network","address","port","generation"}:
      raise SystemExit(role+" TCP listener receipt is not strict")
    fd=listener.get("fd"); generation=listener.get("generation")
    if not isinstance(fd,int) or fd < 0 or fd in descriptors or not isinstance(generation,int) or generation <= 0:
      raise SystemExit(role+" TCP listener identity is invalid")
    descriptors.add(fd)
    observed.add((listener.get("network"),listener.get("address"),listener.get("port")))
  if observed != expected:
    raise SystemExit(role+" TCP listener endpoints are not exact")
  pid=process.get("pid")
  if not isinstance(pid,int) or pid <= 1:
    raise SystemExit(role+" PID is invalid")
  return pid
companion=validate("companion",set())
server=validate("server",{("tcp4","127.0.0.1",18765),("tcp4","127.0.0.1",18766)})
presentation=validate("presentation",{("tcp4","127.0.0.1",18767)})
print(server,presentation)' "${initial_process_receipt}")"
read -r server_pid presentation_pid <<<"${process_roles}"

# The receipt above is kernel evidence about the companion's own descriptors: it
# proves these three processes hold exactly these listening sockets, identified
# by descriptor and kernel generation. It cannot see a *fourth* process, so it
# cannot answer "is anyone else on this port" or "is the debugger port free".
# lsof answers exactly that, and only that, which is why both checks are here.
if [[ "$(listener_owner 18765)" != "${server_pid}" || \
      "$(listener_owner 18766)" != "${server_pid}" || \
      "$(listener_owner 18767)" != "${presentation_pid}" || \
      -n "$(listener_owner 18768)" ]]; then
  printf '%s\n' "companion listener ownership is not exclusive before client launch" >&2
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
env -u OPENREALTIME_HOSTED_COMPANION_TOKEN \
  OPENREALTIME_COMPANION_PROOF_NONCE="${proof_nonce}" \
  OPENREALTIME_COMPANION_BROWSER_SNAPSHOT="${browser_snapshot}" \
  OPENREALTIME_COMPANION_BROWSER_PROFILE_PARENT="${runtime_root}" \
  CHROMIUM="${chromium}" CDP_PORT=18768 \
  node cmd/openrealtime/testdata/companion_browser.mjs "${presentation_url}" | tee "${browser_log}"
wait_for_metrics 1 1 0

browser_proof="$(sed -n 's/^OPENREALTIME_COMPANION_BROWSER_PROOF //p' "${browser_log}" | tail -n 1)"
browser_session_id="$(python3 -c 'import hashlib, json, os, re, stat, sys, urllib.parse
value = json.loads(sys.argv[1])
management = value.get("management") or {}
digest = re.compile(r"sha256:[0-9a-f]{64}")
session = value.get("session_id", "")
parsed = urllib.parse.urlsplit(management.get("url", ""))
info = os.stat(sys.argv[6], follow_symlinks=False)
with open(sys.argv[6], "rb") as source: payload = source.read((32 << 20) + 1)
valid = (
  value.get("schema") == "openrealtime/browser/hosted-companion-proof/v3" and
  value.get("nonce") == sys.argv[2] and
  value.get("transport") == "webrtc" and
  value.get("manifest_fingerprint") == sys.argv[3] and
  value.get("plan_fingerprint") == sys.argv[4] and
  value.get("endpoint") == sys.argv[5] and
  re.fullmatch(r"sess_[A-Za-z0-9_-]{1,128}", session) and
  management.get("session_id") == session and management.get("resource") == "live" and
  parsed.scheme == "http" and parsed.netloc == "127.0.0.1:18767" and
  parsed.path == "/client/v1/management/sessions/" + urllib.parse.quote(session, safe="") + "/live" and
  not parsed.query and not parsed.fragment and
  stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1 and
  0 < len(payload) <= (32 << 20) and management.get("payload_bytes") == len(payload) and
  digest.fullmatch(management.get("payload_digest", "")) is not None and
  management["payload_digest"] == "sha256:" + hashlib.sha256(payload).hexdigest()
)
if not valid: raise SystemExit("browser proof is invalid")
print(session)' "${browser_proof}" "${proof_nonce}" "${browser_manifest_fingerprint}" \
  "${browser_plan_fingerprint}" "${presentation_url}" "${browser_snapshot}")"
assert_frozen_companion_phase "after browser completion"

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
assert_frozen_companion_phase "immediately before native launch"
env -u OPENREALTIME_HOSTED_COMPANION_TOKEN \
OPENREALTIME_NATIVE_PROFILE=observer-developer \
OPENREALTIME_NATIVE_ENDPOINT_DIRECTORY="${native_endpoint_file}" \
OPENREALTIME_HOSTED_MANAGEMENT_SNAPSHOT="${native_snapshot}" \
  "${executable}" "--openrealtime-hosted-smoke=${proof_nonce}" \
  >"${application_log}" 2>&1 &
application_pid="$!"

native_proof=""
for _ in $(seq 1 1000); do
  native_proof="$(sed -n 's/^OPENREALTIME_HOSTED_COMPANION_PROOF //p' "${application_log}" | tail -n 1)"
  if [[ -n "${native_proof}" ]]; then
    break
  fi
  if ! kill -0 "${application_pid}" 2>/dev/null; then
    printf '%s\n' "assembled native app exited before emitting its connection proof" >&2
    sed -n '1,240p' "${application_log}" >&2
    server_curl -fsS "${server_url}/metrics" >&2 || true
    exit 1
  fi
  sleep 0.1
done
if [[ -z "${native_proof}" ]]; then
  printf '%s\n' "timed out waiting for the assembled native app connection proof" >&2
  sed -n '1,240p' "${application_log}" >&2
  server_curl -fsS "${server_url}/metrics" >&2 || true
  exit 1
fi
native_session_id="$(python3 -c 'import hashlib, json, os, re, stat, sys, urllib.parse
value = json.loads(sys.argv[1])
management = value.get("management") or {}
digest = re.compile(r"sha256:[0-9a-f]{64}")
session = value.get("session_id", "")
info = os.stat(sys.argv[6], follow_symlinks=False)
parsed = urllib.parse.urlsplit(management.get("response_url", ""))
with open(sys.argv[6], "rb") as source: payload = source.read((32 << 20) + 1)
valid = (
  value.get("schema") == "openrealtime/macos/hosted-companion-proof/v3" and
  value.get("nonce") == sys.argv[2] and
  value.get("transport") == "websocket" and
  value.get("distribution") == "observer-developer" and
  value.get("manifest_fingerprint") == sys.argv[3] and
  value.get("endpoint_fingerprint") == sys.argv[4] and
  value.get("endpoint") == sys.argv[5] and
  re.fullmatch(r"sess_[A-Za-z0-9_-]{1,128}", session) and
  management.get("session_id") == session and management.get("resource") == "live" and
  parsed.scheme == "http" and parsed.netloc == "127.0.0.1:18767" and
  parsed.path == "/client/v1/management/sessions/" + urllib.parse.quote(session, safe="") + "/live" and
  not parsed.query and not parsed.fragment and
  stat.S_ISREG(info.st_mode) and stat.S_IMODE(info.st_mode) == 0o600 and info.st_nlink == 1 and
  0 < len(payload) <= (32 << 20) and management.get("payload_bytes") == len(payload) and
  digest.fullmatch(management.get("payload_digest", "")) is not None and
  management["payload_digest"] == "sha256:" + hashlib.sha256(payload).hexdigest()
)
if not valid: raise SystemExit("native proof is invalid")
print(session)' "${native_proof}" "${proof_nonce}" "${expected_manifest_fingerprint}" \
  "${expected_endpoint_fingerprint}" "${expected_native_endpoint}" "${native_snapshot}")"
if [[ "${browser_session_id}" == "${native_session_id}" ]]; then
  printf '%s\n' "browser and native app reported the same session identity" >&2
  exit 1
fi
management_receipt="$("${proof_validator}" \
  -browser "${browser_snapshot}" -browser-session "${browser_session_id}" \
  -native "${native_snapshot}" -native-session "${native_session_id}" \
  -nonce "${proof_nonce}" -server-identity-digest "${server_identity_digest}")"
if [[ "${management_receipt}" != OPENREALTIME_COMPANION_MANAGEMENT_RECEIPT\ * ]]; then
  printf '%s\n' "companion management validator omitted its exact receipt" >&2
  exit 1
fi
assert_no_private_material

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
if [[ "$(server_identity)" != "${initial_server_identity}" ]]; then
  printf '%s\n' "server identity changed between browser and native sessions" >&2
  exit 1
fi
final_process_receipt="$("${process_validator}" -binary "${binary}" \
  -companion-pid "${companion_pid}" -runtime-digest "${server_runtime_digest}")"
if [[ "${final_process_receipt}" != "${initial_process_receipt}" || \
      "$(listener_owner 18765)" != "${server_pid}" || \
      "$(listener_owner 18766)" != "${server_pid}" || \
      "$(listener_owner 18767)" != "${presentation_pid}" || \
      -n "$(listener_owner 18768)" || \
      "$(shasum -a 256 "${binary}" | awk '{print $1}')" != "${server_executable_digest}" || \
      "$(stat -f '%d:%i:%z:%m:%p' "${binary}")" != "${server_executable_file_identity}" ]]; then
  printf '%s\n' "companion process, executable, argv, code, kernel socket, or listener identity changed" >&2
  exit 1
fi
/usr/bin/codesign --verify --strict "${binary}"
if [[ "$(/usr/bin/codesign -d --verbose=4 "${binary}" 2>&1 | sed -n 's/^CDHash=//p')" != \
      "${server_code_directory_hash}" ]]; then
  printf '%s\n' "companion executable CodeDirectory changed" >&2
  exit 1
fi

kill -INT "${companion_pid}"
if ! wait "${companion_pid}"; then
  printf '%s\n' "companion supervisor did not stop cleanly" >&2
  sed -n '1,240p' "${companion_log}" >&2
  exit 1
fi
companion_pid=""
assert_no_private_material
for stopped_pid in "${server_pid}" "${presentation_pid}"; do
  if kill -0 "${stopped_pid}" 2>/dev/null; then
    printf '%s\n' "companion child PID survived supervisor shutdown" >&2
    exit 1
  fi
done
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
printf '%s\n' "${management_receipt}"
printf '%s\n' "${initial_process_receipt}"
printf '%s\n' "native executable sha256=${executable_digest}"
printf '%s\n' "server executable sha256=${server_executable_digest} cdhash=${server_code_directory_hash}"
