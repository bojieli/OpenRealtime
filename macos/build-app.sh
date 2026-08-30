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
bridge_resources="${contents_dir}/Resources/BrowserUseBridge"
mkdir -p "${bridge_resources}"
cp "${script_dir}/BrowserUseBridge/bridge.py" "${bridge_resources}/bridge.py"
cp "${script_dir}/BrowserUseBridge/pyproject.toml" "${bridge_resources}/pyproject.toml"
cp "${script_dir}/BrowserUseBridge/uv.lock" "${bridge_resources}/uv.lock"
codesign --force --deep --sign - "${app_dir}"
printf '%s\n' "Built ${app_dir}"
