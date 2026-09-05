#!/usr/bin/env bash
# Small deterministic adapters for release checks whose natural invocation is
# more than one argv vector. This script never downloads dependencies.
set -euo pipefail

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)"
action="${1:-}"
if [[ $# -ne 1 ]]; then
  printf 'usage: %s {gofmt|javascript-client|shell|swift-linux}\n' "${0##*/}" >&2
  exit 2
fi

# shellcheck source=scripts/go-toolchain.sh
source "${repository_root}/scripts/go-toolchain.sh"

case "${action}" in
  gofmt)
    go_bin="$(openrealtime_go_bin)"
    gofmt_bin="$(dirname -- "${go_bin}")/gofmt"
    cd -- "${repository_root}"
    # .runtime holds prepared environments and artifacts holds retained
    # benchmark evidence, whose support programs are copied in verbatim and
    # sealed by receipts; neither is source this gate may reformat or fail on.
    unformatted="$("${gofmt_bin}" -l . 2>/dev/null | grep -v -e '^\.runtime/' -e '^artifacts/' || true)"
    if [[ -n "${unformatted}" ]]; then
      printf 'these files are not gofmt-clean:\n%s\n' "${unformatted}" >&2
      exit 1
    fi
    ;;

  javascript-client)
    cd -- "${repository_root}"
    node client/reducer/javascript/conformance.mjs \
      client/reducer/testdata/reducer_vectors.json
    node --test client/reducer/javascript/reducer.test.mjs
    ;;

  shell)
    cd -- "${repository_root}"
    for script in scripts/*.sh macos/*.sh; do
      bash -n "${script}"
    done
    shellcheck --severity=warning scripts/*.sh macos/*.sh
    ;;

  swift-linux)
    docker image inspect swift:5.10-jammy >/dev/null
    exec docker run --rm --network none \
      -v "${repository_root}:/src:ro" swift:5.10-jammy bash -lc '
        set -euo pipefail
        cp -R /src/client/reducer /tmp/reducer
        rm -rf /tmp/reducer/swift/.build
        cd /tmp/reducer/swift
        swift test -c release -Xswiftc -warnings-as-errors
        swift build -c release \
          -Xswiftc -strict-concurrency=complete -Xswiftc -warnings-as-errors
      '
    ;;

  *)
    printf 'unknown local release gate: %s\n' "${action}" >&2
    exit 2
    ;;
esac
