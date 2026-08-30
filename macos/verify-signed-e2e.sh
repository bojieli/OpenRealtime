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
case "${app}" in
  /*.app) ;;
  *) printf '%s\n' "OPENREALTIME_SIGNED_APP must be an absolute .app path" >&2; exit 1 ;;
esac
case "${runner}" in
  /*) ;;
  *) printf '%s\n' "OPENREALTIME_MACOS_E2E_RUNNER must be an absolute path" >&2; exit 1 ;;
esac
if [[ ! -d "${app}" || ! -x "${runner}" ]]; then
  printf '%s\n' "the signed app or native E2E runner is unavailable" >&2
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

# The trusted runner must exercise launch, profile identity display, connection,
# microphone/camera/screen permission paths, interruption/tool continuation,
# disconnect, and termination. Its nonzero exit is the release-gate result.
exec "${runner}" "${app}"
