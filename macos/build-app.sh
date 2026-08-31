#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
configuration="${1:-release}"
cd "${script_dir}"
swift build -c "${configuration}"

binary_dir="$(swift build -c "${configuration}" --show-bin-path)"
app_dir="${script_dir}/.build/OpenRealtime Developer.app"
contents_dir="${app_dir}/Contents"
case "${app_dir}" in
  "${script_dir}"/.build/*.app) ;;
  *) printf '%s\n' "refusing unexpected app output path: ${app_dir}" >&2; exit 1 ;;
esac
rm -rf "${app_dir}"
mkdir -p "${contents_dir}/MacOS" "${contents_dir}/Resources"
cp "${binary_dir}/OpenRealtimeMac" "${contents_dir}/MacOS/OpenRealtimeMac"
cp "${script_dir}/Info.plist" "${contents_dir}/Info.plist"
resource_bundle="${binary_dir}/OpenRealtimeMac_OpenRealtimeMac.bundle"
if [[ ! -d "${resource_bundle}" ]]; then
  printf '%s\n' "missing SwiftPM resource bundle: ${resource_bundle}" >&2
  exit 1
fi
cp -R "${resource_bundle}" "${contents_dir}/Resources/"
# The WebRTC transport links @rpath/LiveKitWebRTC.framework, so the framework
# has to travel inside the bundle and the executable has to be told where to
# find it. Without both the app builds and then refuses to launch.
webrtc_framework="${binary_dir}/LiveKitWebRTC.framework"
if [[ ! -d "${webrtc_framework}" ]]; then
  printf '%s\n' "missing WebRTC framework: ${webrtc_framework}" >&2
  exit 1
fi
mkdir -p "${contents_dir}/Frameworks"
cp -R "${webrtc_framework}" "${contents_dir}/Frameworks/"
install_name_tool -add_rpath @executable_path/../Frameworks \
  "${contents_dir}/MacOS/OpenRealtimeMac"

bridge_resources="${contents_dir}/Resources/BrowserUseBridge"
mkdir -p "${bridge_resources}"
cp "${script_dir}/BrowserUseBridge/bridge.py" "${bridge_resources}/bridge.py"
cp "${script_dir}/BrowserUseBridge/pyproject.toml" "${bridge_resources}/pyproject.toml"
cp "${script_dir}/BrowserUseBridge/uv.lock" "${bridge_resources}/uv.lock"
# The framework is signed before the bundle that contains it: install_name_tool
# invalidated the executable's signature, and a nested framework signed after
# its container is a signature the loader rejects.
codesign --force --sign - "${contents_dir}/Frameworks/LiveKitWebRTC.framework"
codesign --force --deep --sign - "${app_dir}"
printf '%s\n' "Built ${app_dir}"
