#!/usr/bin/env bash
set -euo pipefail

if [[ "$(uname -s)" != "Darwin" ]]; then
  printf '%s\n' "signed macOS E2E requires a macOS runner" >&2
  exit 1
fi
if [[ "${OPENREALTIME_SIGNED_MACOS_E2E:-}" != "1" ]]; then
  printf '%s\n' "set OPENREALTIME_SIGNED_MACOS_E2E=1 only in the signed native E2E job" >&2
  exit 1
fi
app="${OPENREALTIME_SIGNED_APP:-}"
runner="${OPENREALTIME_MACOS_E2E_RUNNER:-}"
contract="${OPENREALTIME_MACOS_E2E_CONTRACT:-}"
receipt="${OPENREALTIME_MACOS_E2E_RUNNER_RECEIPT:-}"
nonce="${OPENREALTIME_MACOS_E2E_NONCE:-}"
case "${app}" in
  /*.app) ;;
  *) printf '%s\n' "OPENREALTIME_SIGNED_APP must be an absolute .app path" >&2; exit 1 ;;
esac
case "${runner}" in
  /*) ;;
  *) printf '%s\n' "OPENREALTIME_MACOS_E2E_RUNNER must be an absolute path" >&2; exit 1 ;;
esac
case "${contract}" in
  /*) ;;
  *) printf '%s\n' "OPENREALTIME_MACOS_E2E_CONTRACT must be an absolute path" >&2; exit 1 ;;
esac
case "${receipt}" in
  /*) ;;
  *) printf '%s\n' "OPENREALTIME_MACOS_E2E_RUNNER_RECEIPT must be an absolute path" >&2; exit 1 ;;
esac
if [[ ! "${nonce}" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  printf '%s\n' "OPENREALTIME_MACOS_E2E_NONCE must be the gate challenge" >&2
  exit 1
fi
if [[ ! -d "${app}" || ! -x "${runner}" ]]; then
  printf '%s\n' "the signed app or native E2E runner is unavailable" >&2
  exit 1
fi
if [[ ! -f "${contract}" || -L "${contract}" || -e "${receipt}" || -L "${receipt}" ]]; then
  printf '%s\n' "the gate contract must be regular and the runner receipt must not exist" >&2
  exit 1
fi
if [[ "$(stat -f '%Lp' "${contract}")" != "600" ||
      "$(stat -f '%Lp' "$(dirname "${contract}")")" != "700" ||
      "$(dirname "${contract}")" != "$(dirname "${receipt}")" ]]; then
  printf '%s\n' "the runner contract and receipt must share the gate-owned private directory" >&2
  exit 1
fi

codesign --verify --deep --strict --verbose=2 "${app}"
signature="$(codesign --display --verbose=4 "${app}" 2>&1)"
if grep -q '^Signature=adhoc$' <<<"${signature}" || ! grep -q '^Authority=' <<<"${signature}"; then
  printf '%s\n' "native E2E refuses an ad-hoc or authority-less signature" >&2
  exit 1
fi
manifest_count="$(find "${app}/Contents/Resources" -type f -name native-client-manifest.json -print | wc -l | tr -d ' ')"
if [[ "${manifest_count}" != "1" ]]; then
  printf '%s\n' "signed app must contain exactly one native client manifest" >&2
  exit 1
fi

# The trusted runner must exercise the exact nonce-bound contract: launch the
# supplied server executable with its exact launch profile, drive browser first
# and the signed native app second, and publish the canonical create-only
# receipt. Go independently reopens and verifies every binding after return.
"${runner}" \
  --app "${app}" \
  --contract "${contract}" \
  --receipt "${receipt}" \
  --nonce "${nonce}"

if [[ ! -f "${receipt}" || -L "${receipt}" || "$(stat -f '%Lp' "${receipt}")" != "600" ]]; then
  printf '%s\n' "the trusted runner did not publish one private regular receipt" >&2
  exit 1
fi
