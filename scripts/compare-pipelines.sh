#!/usr/bin/env bash
#
# Play the twelve live scenarios against several room pipelines and report
# them side by side.
#
#   scripts/compare-pipelines.sh [PIPELINE[:CONFIG] ...]
#
# Each argument names a pipeline from `openrealtime pipelines`, optionally
# followed by a -pipeline-config YAML applied over it:
#
#   scripts/compare-pipelines.sh room room-flux room-flux:eager.yaml
#
# With no arguments it compares room and room-flux. Every pipeline gets its own
# companion on its own ports, and the pipelines play one after another rather
# than at once: they share the local policy model and synthesiser, and a
# comparison run concurrently would measure the contention as much as the
# pipelines. Each pass plays the scenarios through TestLiveRoomTwelveScenarios,
# the same diagnostic docs/room.md describes. OPENREALTIME_LIVE_SCENARIOS
# narrows it to a comma-separated subset.
#
# It needs DEEPGRAM_API_KEY and GEMINI_API_KEY, and the room's local services
# (docs/room.md). Reports land under .runtime/pipeline-compare/<stamp>/.

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

# shellcheck source=scripts/go-toolchain.sh
source "${repository_root}/scripts/go-toolchain.sh"
go_bin="$(openrealtime_go_bin)"

pipelines=("$@")
if (( ${#pipelines[@]} == 0 )); then
  pipelines=(room room-flux)
fi
for variable in DEEPGRAM_API_KEY GEMINI_API_KEY; do
  if [[ -z "${!variable:-}" ]]; then
    echo "${variable} is not set" >&2
    exit 2
  fi
done

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
out="${repository_root}/.runtime/pipeline-compare/${stamp}"
mkdir -p "${out}"
"${go_bin}" build -o "${out}/openrealtime" ./cmd/openrealtime
"${go_bin}" test -c -o "${out}/live.test" ./cmd/openrealtime

companion_pid=""
stop_companion() {
  if [[ -n "${companion_pid}" ]] && kill -0 "${companion_pid}" 2>/dev/null; then
    kill -TERM "${companion_pid}" 2>/dev/null || true
    for _ in $(seq 1 50); do
      kill -0 "${companion_pid}" 2>/dev/null || break
      sleep 0.2
    done
    kill -KILL "${companion_pid}" 2>/dev/null || true
  fi
  companion_pid=""
}
trap stop_companion EXIT

labels=()
index=0
for entry in "${pipelines[@]}"; do
  name="${entry%%:*}"
  config=""
  label="${name}"
  if [[ "${entry}" == *:* ]]; then
    config="$(realpath "${entry#*:}")"
    label="${name}+$(basename "${config}" .yaml)"
  fi
  labels+=("${label}")
  base=$((18900 + index * 10))
  index=$((index + 1))
  log="${out}/${label}.companion.log"
  arguments=(companion -client none -pipeline "${name}"
    -server-listen "127.0.0.1:${base}"
    -webrtc-listen "127.0.0.1:$((base + 1))"
    -presentation-listen "127.0.0.1:$((base + 2))")
  if [[ -n "${config}" ]]; then
    arguments+=(-pipeline-config "${config}")
  fi
  # The turn timeline is how a failure is read back: which utterance ended
  # when, what the policy chose on each partial, when the voice spoke.
  arguments+=(-- -timeline-log "${out}/${label}.timeline.log")

  echo "=== ${label}: starting a companion on 127.0.0.1:${base}"
  "${out}/openrealtime" "${arguments[@]}" >"${log}" 2>&1 &
  companion_pid=$!
  ready=""
  for _ in $(seq 1 360); do
    if grep -q "OpenRealtime companion ready" "${log}"; then
      ready=1
      break
    fi
    kill -0 "${companion_pid}" 2>/dev/null || break
    sleep 0.5
  done
  if [[ -z "${ready}" ]]; then
    # Recorded rather than skipped: a pipeline that cannot start is a result,
    # and a summary that left it out would read as a pipeline not asked for.
    echo "${label}: the companion did not become ready; see ${log}" >&2
    echo "NOT STARTED: the companion did not become ready; see ${log}" >"${out}/${label}.log"
    stop_companion
    continue
  fi

  echo "=== ${label}: playing the scenarios"
  (
    cd cmd/openrealtime
    OPENREALTIME_ROOM_TEST_ENDPOINT="ws://127.0.0.1:${base}/v1/realtime" \
      "${out}/live.test" -test.run '^TestLiveRoomTwelveScenarios$' -test.count=1 -test.v -test.timeout 30m
  ) >"${out}/${label}.log" 2>&1 || true
  stop_companion
done

python3 - "${out}" "${labels[@]}" <<'PYTHON'
import pathlib
import re
import sys

out = pathlib.Path(sys.argv[1])
labels = sys.argv[2:]
result = re.compile(r"--- (PASS|FAIL|SKIP): TestLiveRoomTwelveScenarios/(\S+) \(([\d.]+)s\)")
outcomes, scenarios, notes = {}, [], {}
for label in labels:
    text = (out / f"{label}.log").read_text(errors="replace")
    if text.startswith("NOT STARTED"):
        notes[label] = "not started"
    elif "no tests to run" in text or "--- SKIP: TestLiveRoomTwelveScenarios " in text:
        notes[label] = "skipped"
    for status, scenario, seconds in result.findall(text):
        outcomes[(label, scenario)] = f"{status} {float(seconds):.0f}s"
        if scenario not in scenarios:
            scenarios.append(scenario)

lines = ["| scenario | " + " | ".join(labels) + " |", "|---" * (len(labels) + 1) + "|"]
for scenario in scenarios:
    cells = [outcomes.get((label, scenario), "-") for label in labels]
    lines.append(f"| {scenario} | " + " | ".join(cells) + " |")
totals = []
for label in labels:
    if label in notes:
        totals.append(notes[label])
        continue
    played = [value for (name, _), value in outcomes.items() if name == label]
    passed = sum(value.startswith("PASS") for value in played)
    totals.append(f"{passed}/{len(played)} passed")
lines.append("| total | " + " | ".join(totals) + " |")
summary = "\n".join(lines) + "\n"
(out / "summary.md").write_text(summary)
print(summary)
print(f"logs and summary: {out}")
PYTHON
