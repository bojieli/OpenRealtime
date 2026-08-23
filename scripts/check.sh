#!/usr/bin/env bash
#
# The verification gate. One command, no arguments, no network, no GPU:
#
#   ./scripts/check.sh
#
# Everything here has to pass before a change lands. It is deliberately the
# whole gate rather than a fast subset - a gate people run a piece of is a gate
# that stops meaning anything, and the full run is minutes, not hours.
#
# What needs a model, a dataset, or a GPU is not here. Those are the
# measurement suites (`openrealtime bench`, `scripts/prepare-*.sh`), which are
# reported rather than gated, because a gate that cannot run offline is a gate
# that fails for reasons unrelated to the change.

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

# shellcheck source=scripts/go-toolchain.sh
source "${repository_root}/scripts/go-toolchain.sh"
go_bin="$(openrealtime_go_bin)"
gofmt_bin="$(dirname "${go_bin}")/gofmt"

failures=()

# stage runs one named check and records the outcome without stopping.
#
# Running to the end matters more than stopping early: someone who has broken
# formatting and a test wants to see both, not to fix one and rerun for four
# minutes to discover the other.
stage() {
  local name="$1"
  shift
  printf '\n=== %s\n' "${name}"
  if "$@"; then
    printf '=== %s: ok\n' "${name}"
  else
    printf '=== %s: FAILED\n' "${name}" >&2
    failures+=("${name}")
  fi
}

check_formatting() {
  local unformatted
  unformatted="$("${gofmt_bin}" -l . 2>/dev/null | grep -v '^\.runtime/' || true)"
  if [[ -n "${unformatted}" ]]; then
    echo "these files are not gofmt-clean:" >&2
    printf '  %s\n' ${unformatted} >&2
    return 1
  fi
}

# check_module runs the language-level gates over one Go module.
#
# The repository is several modules, not one: the LiveKit integration is
# separate so that a server build does not inherit its dependency tree, and a
# gate that only covered the root module would leave it to rot.
check_module() {
  local module_directory="$1"
  (cd "${module_directory}" && "${go_bin}" vet ./... && "${go_bin}" test -race -count=1 ./...)
}

check_examples() {
  # The browser example is HTML and JavaScript, so what can be checked is that
  # it is served by a real handler and refers to events the protocol defines.
  # The Go tests themselves already ran under the root module, with -race.
  local missing=()
  for asset in examples/browser/index.html examples/browser/README.md; do
    [[ -f "${asset}" ]] || missing+=("${asset}")
  done
  if (( ${#missing[@]} > 0 )); then
    echo "missing example assets: ${missing[*]}" >&2
    return 1
  fi
}

# check_official_client is the compatibility claim, and it is the one claim in
# this gate that can quietly go unmade.
#
# Section 8 says an official OpenAI Realtime client completes a tool-using
# session unmodified, over WebSocket and over WebRTC. The tests that establish
# it need node, the SDK, and a browser, and they skip when those are absent -
# at which point `go test` prints ok for a package that verified nothing, and
# the gate that inherits it prints ok too. A release cut on that machine would
# ship the claim untested with every stage green.
#
# So the skip is named rather than swallowed. An ordinary run says out loud
# which claim was not made; a release run fails, because the one thing a
# release gate may not do is report a claim it did not check.
check_official_client() {
  local output status
  output="$("${go_bin}" test -count=1 -v -run 'TestTheOfficialSDKCompletesAToolUsingSession' \
    ./examples/sdk-client/ 2>&1)" && status=0 || status=$?
  if (( status != 0 )); then
    echo "${output}" >&2
    return 1
  fi

  local unverified=() transport
  for transport in WebSocket WebRTC; do
    grep -q -- "--- PASS: TestTheOfficialSDKCompletesAToolUsingSessionOver${transport}" \
      <<<"${output}" || unverified+=("${transport}")
  done
  if (( ${#unverified[@]} == 0 )); then
    echo "the official SDK completed a tool-using session over WebSocket and WebRTC"
    return 0
  fi

  echo "NOT VERIFIED: the official OpenAI Realtime client over ${unverified[*]}" >&2
  # The skip lines carry the cause. This used to assert one instead - install
  # node and chromium - which was the case it was written for and not the only
  # one: a bundle that fails to build skips too, on a machine that has both,
  # and being told to install what is already installed sends the reader past
  # the reason printed directly above it.
  # -B2 because go test -v prints the reason on the lines before --- SKIP,
  # and the reason is the whole point: the test name alone says a claim was
  # not checked without saying what stopped it.
  grep -E -B 2 -- '--- SKIP' <<<"${output}" | grep -v '^--$' | sed 's/^/  /' >&2
  echo "  each skip above says what it needed; a missing SDK is fixed with" >&2
  echo "  (cd examples/sdk-client && npm install)" >&2
  if [[ -n "${OPENREALTIME_RELEASE_GATE:-}" ]]; then
    echo "  this is a release run, and section 8 requires this claim to be checked" >&2
    return 1
  fi
  echo "  not fatal for an ordinary run; set OPENREALTIME_RELEASE_GATE=1 to require it" >&2
}

# check_protocol_conformance runs the wire-level suite against the pinned
# OpenAI Realtime schema and the OpenRealtime extension.
check_protocol_conformance() {
  "${go_bin}" run ./cmd/openrealtime conformance protocol
}

# check_injection_gate is the safety release gate for computer use.
#
# It is called out separately from the test run it is part of because it is the
# one gate whose failure means "do not ship", not "fix a test".
#
# It used to select the tests with -run 'TestInjection|TestObserved|TestFenced'.
# Two things were wrong with that, and they compounded. `TestInjection` matched
# nothing - the test is named TestInjectedActionsCannotLeaveTheDeclaredTarget -
# and `TestFenced` matched nothing either, because the word is in the middle of
# TestObservedContentReachesProvidersAsFencedData, which `TestObserved` already
# caught. So two thirds of the pattern were dead and the gate that means "do not
# ship" ran exactly one of the five authority tests.
#
# What made that survivable is the same shape check_official_client guards
# against: `go test -run` prints ok and exits 0 for a package where the pattern
# matched nothing. A typo in the pattern is therefore indistinguishable from a
# passing gate, and the gate whose failure means "do not ship" is the worst place
# in this file for a green line that verified nothing.
#
# So the pattern is gone. The authority claim lives in one package, and the gate
# runs that package whole - a new authority test is covered by writing it, not by
# also remembering to widen a regex here. The count is then asserted against the
# functions actually declared in the package, so an empty or partial run fails
# instead of reporting ok.
check_injection_gate() {
  local package="./computeruse/injection"
  local source_directory="computeruse/injection"

  local declared
  declared="$(grep -rhoE '^func (Test[A-Za-z0-9_]+)' "${source_directory}"/*_test.go 2>/dev/null | wc -l)"
  if (( declared == 0 )); then
    echo "no authority tests found in ${source_directory}: the package moved or the glob is wrong" >&2
    return 1
  fi

  local output status
  output="$("${go_bin}" test -race -count=1 -v "${package}" 2>&1)" && status=0 || status=$?
  if (( status != 0 )); then
    echo "${output}" >&2
    return 1
  fi

  # -c on the anchored form, so a subtest named PASS in its own output cannot
  # inflate the count past the top-level functions it is compared against.
  local passed
  passed="$(grep -cE '^--- PASS: Test' <<<"${output}" || true)"
  if (( passed != declared )); then
    echo "the injection authority gate ran ${passed} of ${declared} tests in ${source_directory}" >&2
    grep -E '^--- (SKIP|FAIL)' <<<"${output}" | sed 's/^/  /' >&2
    echo "  section 8 requires the injection-authority test to pass, and a skipped" >&2
    echo "  or unmatched authority test is not a passing one" >&2
    return 1
  fi
  echo "the injection authority gate passed ${passed} of ${declared} tests"
}

check_shell() {
  local script
  for script in scripts/*.sh; do
    bash -n "${script}" || return 1
  done
  if command -v shellcheck >/dev/null; then
    shellcheck --severity=warning scripts/*.sh || return 1
  fi
}

stage "gofmt" check_formatting
stage "go vet and go test -race (root module)" check_module .
for module_directory in integrations/*/; do
  [[ -f "${module_directory}/go.mod" ]] || continue
  stage "go vet and go test -race (${module_directory%/})" check_module "${module_directory}"
done
stage "protocol conformance" check_protocol_conformance
stage "prompt-injection release gate" check_injection_gate
stage "examples" check_examples
stage "official Realtime client" check_official_client
stage "shell scripts" check_shell

printf '\n'
if (( ${#failures[@]} > 0 )); then
  printf 'FAILED: %s\n' "${failures[*]}" >&2
  exit 1
fi
echo "all gates passed"
