#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
temporary="$(mktemp -d)"
cleanup() {
  rm -rf -- "${temporary}"
}
trap cleanup EXIT

proc_root="${temporary}/proc"
runtime_root="${temporary}/runtime"
mkdir -p "${proc_root}/sys/kernel/random" "${runtime_root}/pids"
printf 'test-boot\n' >"${proc_root}/sys/kernel/random/boot_id"

write_process() {
  local pid="$1"
  local parent="$2"
  local start="$3"
  mkdir -p "${proc_root}/${pid}"
  printf 'Name:\ttest\nPPid:\t%s\n' "${parent}" >"${proc_root}/${pid}/status"
  local stat_line="${pid} (test process) S ${parent}"
  for _ in $(seq 1 17); do
    stat_line+=" 0"
  done
  printf '%s %s 0\n' "${stat_line}" "${start}" >"${proc_root}/${pid}/stat"
}

write_process 1 0 1
write_process 10 1 100
write_process 11 10 110
write_process 20 1 200
write_process 21 20 210
write_process 30 1 300
write_process 31 30 310
printf '10\n' >"${runtime_root}/pids/fish.pid"
printf '20\n' >"${runtime_root}/pids/asr.pid"
printf '30\n' >"${runtime_root}/pids/qwen.pid"

rows="${temporary}/rows"
printf 'GPU-test, 11, 10\nGPU-test, 21, 20\nGPU-test, 31, 30\n' >"${rows}"
fake_smi="${temporary}/nvidia-smi"
printf '#!/usr/bin/env bash\ncat "%s"\n' "${rows}" >"${fake_smi}"
chmod +x "${fake_smi}"

capture() {
  OPENREALTIME_PROC_ROOT="${proc_root}" \
  OPENREALTIME_NVIDIA_SMI="${fake_smi}" \
    "${repository_root}/scripts/capture-local-gpu-ownership.sh" "${runtime_root}"
}

snapshot="$(capture)"
jq -e '
  .exclusive == true and .host_boot_id == "test-boot" and
  .expected_components == ["asr","fish","qwen"] and
  .gpu_uuids == ["GPU-test"] and (.processes | length) == 3 and
  .component_roots.asr.proc_start_time_ticks == "200" and
  (.processes[] | select(.pid == 31).proc_start_time_ticks) == "310"
' <<<"${snapshot}" >/dev/null

write_process 40 1 400
write_process 41 40 410
printf 'GPU-test, 11, 10\nGPU-test, 21, 20\nGPU-test, 31, 30\nGPU-test, 41, 5\n' >"${rows}"
if capture >/dev/null 2>&1; then
  echo "unregistered GPU process was accepted" >&2
  exit 1
fi

printf 'GPU-test, 11, 10\nGPU-other, 21, 20\nGPU-test, 31, 30\n' >"${rows}"
if capture >/dev/null 2>&1; then
  echo "multi-GPU local cascade was accepted" >&2
  exit 1
fi

rm "${runtime_root}/pids/qwen.pid"
printf 'GPU-test, 11, 10\nGPU-test, 21, 20\n' >"${rows}"
snapshot="$(capture)"
jq -e '.expected_components == ["asr","fish"] and (.processes | length) == 2' \
  <<<"${snapshot}" >/dev/null

echo "local GPU ownership tests pass"
