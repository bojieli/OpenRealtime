#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
mkdir -p "${repository_root}/.runtime"
temporary="$(mktemp -d "${repository_root}/.runtime/gpu-guard-test.XXXXXX")"
cleanup() {
  rm -rf -- "${temporary}"
}
trap cleanup EXIT

proc_root="${temporary}/proc"
runtime_root="${temporary}/runtime"
mkdir -p "${proc_root}/sys/kernel/random" "${runtime_root}/pids"
printf 'guard-test-boot\n' >"${proc_root}/sys/kernel/random/boot_id"

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
write_process 40 1 400
write_process 41 40 410
printf '10\n' >"${runtime_root}/pids/fish.pid"
printf '20\n' >"${runtime_root}/pids/asr.pid"

rows="${temporary}/rows"
printf 'GPU-test, 11, 10\nGPU-test, 21, 20\n' >"${rows}"
fake_smi="${temporary}/nvidia-smi"
printf '#!/usr/bin/env bash\ncat "%s"\n' "${rows}" >"${fake_smi}"
chmod +x "${fake_smi}"

export OPENREALTIME_PROC_ROOT="${proc_root}"
export OPENREALTIME_NVIDIA_SMI="${fake_smi}"
export OPENREALTIME_LOCAL_CASCADE_RUNTIME_ROOT="${runtime_root}"
export OPENREALTIME_GPU_OWNERSHIP_INTERVAL_SECONDS=1

good_stem="${temporary}/good"
"${repository_root}/scripts/run-with-local-gpu-ownership-guard.sh" \
  "${good_stem}" -- bash -c 'sleep 2.2'
# Assert through --slurpfile: a guard that wrote an empty summary would satisfy
# `jq -e FILTER FILE` for any filter, so this test would pass vacuously.
jq -en --slurpfile summary "${good_stem}.summary.json" '
  ($summary | length) == 1 and ($summary[0] |
  .status == "complete" and .checks >= 2 and
  .expected_components == ["asr","fish"] and .gpu_uuids == ["GPU-test"])
' >/dev/null

failure_stem="${temporary}/command-failure"
set +e
"${repository_root}/scripts/run-with-local-gpu-ownership-guard.sh" \
  "${failure_stem}" -- bash -c 'exit 23'
failure_status=$?
set -e
if [[ "${failure_status}" != 23 ]]; then
  echo "measured command status was not preserved: ${failure_status}" >&2
  exit 1
fi
jq -en --slurpfile summary "${failure_stem}.summary.json" \
  '($summary | length) == 1 and
   ($summary[0] | .status == "complete" and .checks >= 1)' >/dev/null

printf 'GPU-test, 11, 10\nGPU-test, 21, 20\n' >"${rows}"
bad_stem="${temporary}/bad"
bad_command_completed="${temporary}/bad-command-completed"
if ROWS="${rows}" COMPLETED="${bad_command_completed}" \
  "${repository_root}/scripts/run-with-local-gpu-ownership-guard.sh" \
  "${bad_stem}" -- bash -c '
    sleep 0.3
    printf "GPU-test, 11, 10\nGPU-test, 21, 20\nGPU-test, 41, 5\n" >"$ROWS"
    sleep 10
    : >"$COMPLETED"
  ' >/dev/null 2>&1; then
  echo "transient unregistered GPU process was accepted" >&2
  exit 1
fi
if [[ -e "${bad_command_completed}" ]]; then
  echo "measured command was not stopped when GPU ownership failed" >&2
  exit 1
fi
# `-s` is safe against the empty-file hole: slurp reads no input as [] and the
# filter still runs, so an empty log yields false rather than "no output".
jq -e -s 'any(.[]; .type == "guard.check" and .status == "violation")' \
  "${bad_stem}.jsonl" >/dev/null

echo "local GPU ownership guard tests pass"
