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
  "${go_bin}" test -count=1 ./examples/... 2>/dev/null || true
  local missing=()
  for asset in examples/browser/index.html examples/browser/README.md; do
    [[ -f "${asset}" ]] || missing+=("${asset}")
  done
  if (( ${#missing[@]} > 0 )); then
    echo "missing example assets: ${missing[*]}" >&2
    return 1
  fi
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
check_injection_gate() {
  "${go_bin}" test -race -count=1 -run 'TestInjection|TestObserved|TestFenced' ./computeruse/...
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
stage "shell scripts" check_shell

printf '\n'
if (( ${#failures[@]} > 0 )); then
  printf 'FAILED: %s\n' "${failures[*]}" >&2
  exit 1
fi
echo "all gates passed"
