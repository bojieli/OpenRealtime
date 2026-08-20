#!/usr/bin/env bash

# Regression suite for one defect class: an EMPTY evidence file read as success.
#
# `jq -e FILTER FILE` exits 0 when FILE is zero bytes. The filter never runs, so
# jq reports "no output produced" rather than false -- and every guard written as
# `jq -e '<must be true>' file` therefore ACCEPTS an empty file, for any filter,
# including a literal `false`. A truncated (invalid JSON) file is safe: jq exits
# 4. Only the empty case is the hole, which is exactly the case a crashed or
# just-created producer leaves behind.
#
# Each check below extracts the shipped predicate from its script and proves the
# same three things: an empty file is refused, genuine evidence is still
# accepted, and a negative case is still refused (so the guard did not become
# fail-closed-on-everything).

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
temporary="$(mktemp -d)"
cleanup() { rm -rf -- "${temporary}"; }
trap cleanup EXIT

failures=0
check() {
  local label="$1" expected="$2" actual="$3"
  if [[ "${expected}" == "${actual}" ]]; then
    printf 'ok   %s (%s)\n' "${label}" "${actual}"
  else
    printf 'FAIL %s: expected %s, got %s\n' "${label}" "${expected}" "${actual}" >&2
    failures=$((failures + 1))
  fi
}

: >"${temporary}/empty.json"
printf '{"type":"guard.check",' >"${temporary}/truncated.json"

# Establish the underlying jq behavior this suite exists to defend against, so a
# future jq upgrade that fixes it is visible here rather than silently making
# these guards redundant.
if jq -e 'false' "${temporary}/empty.json" >/dev/null 2>&1; then
  printf 'ok   jq -e still accepts an empty file for a false filter (hole present)\n'
else
  printf 'note jq -e no longer accepts an empty file; guards below remain correct\n'
fi
if jq -e '.anything' "${temporary}/truncated.json" >/dev/null 2>&1; then
  printf 'FAIL jq -e accepted a truncated file\n' >&2
  failures=$((failures + 1))
else
  printf 'ok   jq -e refuses a truncated file\n'
fi

verdict() { if "$@" >/dev/null 2>&1; then echo accept; else echo refuse; fi; }

# --- scripts/run-with-local-gpu-ownership-guard.sh: guard readiness ----------
# An empty log used to mean "exclusive GPU ownership verified", letting a scored
# benchmark start before the monitor had written a single check.
eval "$(sed -n '/^guard_check_ok()/,/^}/p' \
  "${repository_root}/scripts/run-with-local-gpu-ownership-guard.sh")"
printf '{"type":"guard.check","status":"ok"}\n' >"${temporary}/guard-ok.jsonl"
printf '{"type":"guard.check","status":"failed"}\n' >"${temporary}/guard-failed.jsonl"
printf '{"type":"guard.start","status":"ok"}\n{"type":"guard.check","status":"ok"}\n' \
  >"${temporary}/guard-multi.jsonl"
check "gpu guard refuses an empty log" refuse "$(verdict guard_check_ok "${temporary}/empty.json")"
check "gpu guard refuses a truncated log" refuse "$(verdict guard_check_ok "${temporary}/truncated.json")"
check "gpu guard refuses a failed-only log" refuse "$(verdict guard_check_ok "${temporary}/guard-failed.jsonl")"
check "gpu guard accepts a real ok record" accept "$(verdict guard_check_ok "${temporary}/guard-ok.jsonl")"
check "gpu guard accepts ok among other records" accept "$(verdict guard_check_ok "${temporary}/guard-multi.jsonl")"

# --- scripts/archive-tau-voice-artifacts.sh: used attempt must be indexed ----
indexed() {
  jq -en --slurpfile results "$1" --arg id "$2" \
    '($results | length) == 1 and
     ([$results[0].simulation_index[]? | select(.id == $id)] | length) > 0'
}
printf '{"simulation_index":[{"id":"sim-a"},{"id":"sim-b"}]}\n' >"${temporary}/index.json"
check "archive refuses an empty results.json" refuse "$(verdict indexed "${temporary}/empty.json" sim-a)"
check "archive accepts an indexed simulation" accept "$(verdict indexed "${temporary}/index.json" sim-a)"
check "archive refuses an unindexed simulation" refuse "$(verdict indexed "${temporary}/index.json" sim-zz)"

# --- scripts/benchmark-run-context.sh: GPU log bound to its summary ----------
bound() {
  jq -en --slurpfile summary "$1" --arg sha256 "$2" --argjson bytes "$3" \
    '($summary | length) == 1 and
     ($summary[0].log.sha256 == $sha256 and $summary[0].log.bytes == $bytes)'
}
printf '{"log":{"sha256":"abc","bytes":11}}\n' >"${temporary}/summary.json"
check "run context refuses an empty summary" refuse "$(verdict bound "${temporary}/empty.json" abc 11)"
check "run context accepts a bound log" accept "$(verdict bound "${temporary}/summary.json" abc 11)"
check "run context refuses a changed hash" refuse "$(verdict bound "${temporary}/summary.json" zzz 11)"
check "run context refuses a changed size" refuse "$(verdict bound "${temporary}/summary.json" abc 12)"

# --- scripts/run-tau-voice-matrix.sh: GPU summary completeness ---------------
complete_summary() {
  jq -en --slurpfile summary "$1" \
    '($summary | length) == 1 and
     ($summary[0] | .schema_version == "1.0.0" and .status == "complete" and .checks > 0)'
}
printf '{"schema_version":"1.0.0","status":"complete","checks":4}\n' >"${temporary}/gpu-good.json"
printf '{"schema_version":"1.0.0","status":"complete","checks":0}\n' >"${temporary}/gpu-zero.json"
printf '{"schema_version":"1.0.0","status":"running","checks":4}\n' >"${temporary}/gpu-running.json"
check "matrix refuses an empty gpu summary" refuse "$(verdict complete_summary "${temporary}/empty.json")"
check "matrix accepts a complete gpu summary" accept "$(verdict complete_summary "${temporary}/gpu-good.json")"
check "matrix refuses a zero-check summary" refuse "$(verdict complete_summary "${temporary}/gpu-zero.json")"
check "matrix refuses an unfinished summary" refuse "$(verdict complete_summary "${temporary}/gpu-running.json")"

# --- scripts/fetch-json-endpoint.sh: an empty 200 body is not evidence -------
# The same hole reaches jq through a shell string: every preregistration check is
# `jq -e '<must be true>' <<<"${body}"`, and an empty variable is empty input, so
# a gateway answering /healthz with a blank 200 would satisfy every runtime
# requirement a matrix declares. Assert the fetch refuses that at the source.
if jq -e 'false' <<<"" >/dev/null 2>&1; then
  printf 'ok   jq -e still accepts an empty shell string (hole present)\n'
else
  printf 'note jq -e no longer accepts empty stdin; the fetch guard remains correct\n'
fi

port=8913
python3 - "${port}" <<'SERVER' &
import http.server, json, sys
class Handler(http.server.BaseHTTPRequestHandler):
    bodies = {
        "/empty": b"",
        "/object": json.dumps({"asr": {"model": "Qwen/Qwen3-ASR-0.6B"}}).encode(),
        "/array": b"[]",
        "/empty-object": b"{}",
        "/null": b"null",
        "/text": b"not json",
    }
    def do_GET(self):
        body = self.bodies.get(self.path, b"")
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *arguments):
        pass
http.server.HTTPServer(("127.0.0.1", int(sys.argv[1])), Handler).serve_forever()
SERVER
server_pid=$!
stop_server() { kill "${server_pid}" 2>/dev/null || true; }
trap 'stop_server; cleanup' EXIT
for _ in $(seq 50); do
  if curl --silent --output /dev/null "http://127.0.0.1:${port}/object"; then break; fi
  sleep 0.1
done

fetch() {
  "${repository_root}/scripts/fetch-json-endpoint.sh" "http://127.0.0.1:${port}$1" "test$1"
}
check "fetch accepts a real JSON object" accept "$(verdict fetch /object)"
check "fetch refuses an empty 200 body" refuse "$(verdict fetch /empty)"
check "fetch refuses a JSON array" refuse "$(verdict fetch /array)"
check "fetch refuses an empty JSON object" refuse "$(verdict fetch /empty-object)"
check "fetch refuses a JSON null" refuse "$(verdict fetch /null)"
check "fetch refuses a non-JSON body" refuse "$(verdict fetch /text)"
check "fetch refuses an unreachable endpoint" refuse \
  "$(verdict "${repository_root}/scripts/fetch-json-endpoint.sh" http://127.0.0.1:9/x x 1)"
stop_server
trap cleanup EXIT

# Every assertion that reads a fetched body must come from the guarded fetch, so
# a future caller cannot reintroduce a raw curl whose body feeds `jq -e`.
# Judge each curl by its whole command, not one line: a body written to a file
# (`-o`/`--output`) or discarded (`>/dev/null`) never reaches a jq assertion, and
# downloads of external inputs are not runtime evidence.
raw=0
while IFS= read -r occurrence; do
  printf 'FAIL raw curl body feeds an assertion: %s\n' "${occurrence}" >&2
  raw=$((raw + 1))
done < <(
  for script in "${repository_root}"/scripts/*.sh; do
    case "${script}" in
      */fetch-json-endpoint.sh) continue ;;
      */test_empty_evidence_guards.sh) continue ;;
      */check_openai_realtime_spec.sh|*/prepare-fdbench.sh) continue ;;
    esac
    awk -v file="${script}" '
      /curl / { collecting = 1; start = FNR; text = "" }
      collecting { text = text " " $0 }
      collecting && !/\\$/ {
        collecting = 0
        if (text ~ /fetch-json-endpoint/) next
        # A multipart upload cannot use the GET helper; it validates its own
        # response in place, which the check below the scan proves.
        if (file ~ /register-fish-tau-voices\.sh$/ && text ~ /audio_sample=@/) next
        if (text ~ />\/dev\/null/ || text ~ / -o / || text ~ /--output/) next
        # Only a body captured into a shell variable can reach `jq -e <<<`.
        if (text ~ /[A-Za-z_][A-Za-z0-9_]*="\$\( *curl/) {
          printf "%s:%d\n", file, start
        }
      }
    ' "${script}"
  done
)
check "no raw curl body feeds an assertion" 0 "${raw}"

# --- no unhardened `jq -e FILTER FILE` remains in the gate scripts ----------
# Reads from a shell string (`<<<`) cannot hit the empty-file hole, and `-en`
# with --slurpfile is the hardened form; anything else reading a FILE is a
# regression.
# Scan with awk so a `jq -e` invocation is judged by the whole command, not one
# line: the risky form ends in a FILE argument, while `<<<` and a pipe read a
# shell string or stdin and cannot hit the empty-file hole. This suite's own
# prose and deliberate demonstrations are excluded by path.
unhardened=0
while IFS= read -r occurrence; do
  printf 'FAIL unhardened jq -e reads a file: %s\n' "${occurrence}" >&2
  unhardened=$((unhardened + 1))
done < <(
  for script in "${repository_root}"/scripts/*.sh; do
    case "${script}" in
      */test_empty_evidence_guards.sh) continue ;;
    esac
    awk -v file="${script}" '
      # Accumulate a jq -e invocation until its command terminator, then decide.
      /jq -e[rn]* / { collecting = 1; start = FNR; text = "" }
      collecting { text = text " " $0 }
      collecting && (/>\/dev\/null/ || /; then$/ || /\|\| / || /&& /) {
        collecting = 0
        if (text ~ /<<</ || text ~ /--slurpfile/ || text ~ /jq -e[rn]* [^|]*\| *jq/) next
        # Slurp mode reads an empty file as [] and still evaluates the filter,
        # so `jq -e -s` cannot hit the empty-file hole.
        if (text ~ /jq -e[rn]* -s/ || text ~ /jq -[a-z]*s[a-z]* /) next
        if (text ~ /" *(>\/dev\/null|>&)/ || text ~ /\$\{[A-Za-z_][A-Za-z0-9_]*\}" *>/) {
          printf "%s:%d: %s\n", file, start, substr(text, 2, 90)
        }
      }
    ' "${script}"
  done
)
check "no unhardened jq -e file reads remain" 0 "${unhardened}"

if ((failures > 0)); then
  printf '\n%s check(s) failed\n' "${failures}" >&2
  exit 1
fi
printf '\nall empty-evidence guard checks passed\n'
