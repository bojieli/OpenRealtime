#!/usr/bin/env bash

# Sourced by the deterministic reproduction gates. The project requires Go
# 1.25+, but some hosts retain an older `/usr/bin/go` ahead of the installed
# toolchain on PATH. Prefer an explicit override, then the repository's
# documented installation path, and finally the ambient command.

openrealtime_go_bin() {
  local candidate version major minor
  local -a candidates=()
  if [[ -n "${OPENREALTIME_GO_BIN:-}" ]]; then
    candidates+=("${OPENREALTIME_GO_BIN}")
  fi
  candidates+=(/usr/local/go/bin/go)
  if candidate="$(command -v go 2>/dev/null)"; then
    candidates+=("${candidate}")
  fi

  for candidate in "${candidates[@]}"; do
    [[ -x "${candidate}" ]] || continue
    version="$(${candidate} version 2>/dev/null || true)"
    version="${version#go version go}"
    version="${version%% *}"
    major="${version%%.*}"
    if [[ "${version}" == "${major}" ]]; then
      continue
    fi
    minor="${version#*.}"
    minor="${minor%%.*}"
    if [[ "${major}" =~ ^[0-9]+$ && "${minor}" =~ ^[0-9]+$ ]] &&
      ((major > 1 || (major == 1 && minor >= 25))); then
      printf '%s\n' "${candidate}"
      return 0
    fi
  done
  echo "OpenRealtime requires Go 1.25 or newer; set OPENREALTIME_GO_BIN" >&2
  return 1
}
