#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

# The full study is one serial chain of ten detached queues. Each successor
# waits for its predecessor's PID and then requires an exact completion marker
# in that predecessor's log. The marker is a bare string echoed by one script
# and grepped by another, so a reworded message links nothing: the chain halts
# at that seam after days of GPU work, with a queue reporting success and its
# successor refusing to start. Verify the seams statically instead.
verify_chain() {
  local root="$1"
  local launcher="${root}/scripts/launch-full-study-queues.sh"
  local -a queue_ids=()
  local -A queue_scripts=() queue_roots=()
  local -A emitted=() required=() predecessor_root=() position=()

  # The launcher is the single source of truth for the chain's membership.
  eval "$(sed -n '/^queue_ids=(/,/^)/p' "${launcher}")"
  eval "$(sed -n '/^declare -A queue_scripts=(/,/^)/p' "${launcher}")"
  eval "$(sed -n '/^declare -A queue_roots=(/,/^)/p' "${launcher}")"

  local id index=0 script value
  for id in "${queue_ids[@]}"; do
    position[$id]="${index}"
    index=$((index + 1))
    script="${root}/${queue_scripts[$id]}"
    if [[ ! -f "${script}" ]]; then
      echo "queue ${id} script is missing: ${queue_scripts[$id]}" >&2
      return 1
    fi
    # The completion marker a queue publishes is its final unconditional echo.
    emitted[$id]="$(sed -n 's/^echo "\(.*\)"$/\1/p' "${script}" | tail -1)"
    if [[ -z "${emitted[$id]}" ]]; then
      echo "queue ${id} publishes no completion marker" >&2
      return 1
    fi
    required[$id]="$(sed -n "s/^if ! grep -Fx '\(.*\)' .*/\1/p" "${script}" | head -1)"
    value="$(sed -n 's/^[a-z_]*_log="\(.*\)"$/\1/p' "${script}" | head -1)"
    value="${value#\$\{repository_root\}/}"
    predecessor_root[$id]="${value%/queue.log}"
  done

  local head_seen=false predecessor found
  for id in "${queue_ids[@]}"; do
    if [[ -z "${required[$id]}" ]]; then
      if [[ -n "${predecessor_root[$id]}" ]]; then
        echo "queue ${id} watches a predecessor log but requires no marker" >&2
        return 1
      fi
      if [[ "${position[$id]}" != 0 ]]; then
        echo "queue ${id} has no predecessor but is not launched first" >&2
        return 1
      fi
      head_seen=true
      continue
    fi
    # The queue whose log this successor reads must be the one that emits the
    # marker it requires, and must already be running when it starts.
    predecessor=""
    for found in "${queue_ids[@]}"; do
      if [[ "${queue_roots[$found]}" == "${predecessor_root[$id]}" ]]; then
        predecessor="${found}"
        break
      fi
    done
    if [[ -z "${predecessor}" ]]; then
      echo "queue ${id} waits on an unregistered log: ${predecessor_root[$id]}" >&2
      return 1
    fi
    if [[ "${emitted[$predecessor]}" != "${required[$id]}" ]]; then
      echo "queue ${id} requires a marker its predecessor never publishes:" >&2
      echo "  ${predecessor} publishes: ${emitted[$predecessor]}" >&2
      echo "  ${id} requires        : ${required[$id]}" >&2
      return 1
    fi
    if [[ "${position[$predecessor]}" -ge "${position[$id]}" ]]; then
      echo "queue ${id} is launched before its predecessor ${predecessor}" >&2
      return 1
    fi
  done
  if [[ "${head_seen}" != true ]]; then
    echo "the queue chain declares no starting queue" >&2
    return 1
  fi
}

if ! verify_chain "${repository_root}"; then
  echo "full-study queue chain is not consistently linked" >&2
  exit 1
fi

fixture_root="$(mktemp -d /tmp/openrealtime-queue-chain-test.XXXXXX)"
trap 'rm -rf -- "${fixture_root}"' EXIT
mkdir -p "${fixture_root}/scripts"
cp "${repository_root}"/scripts/run-*-queue.sh "${fixture_root}/scripts/"
cp "${repository_root}/scripts/launch-full-study-queues.sh" "${fixture_root}/scripts/"
if ! verify_chain "${fixture_root}"; then
  echo "an unmodified copy of the chain was rejected" >&2
  exit 1
fi
# Reword one queue's completion marker: its successor now requires a string
# nothing publishes, which is the silent multi-day halt this test exists for.
sed -i 's/^echo "FD-Bench queue complete"$/echo "FD-Bench queue finished"/' \
  "${fixture_root}/scripts/run-fdbench-queue.sh"
if verify_chain "${fixture_root}" 2>/dev/null; then
  echo "a reworded completion marker went undetected" >&2
  exit 1
fi

# Every regression suite in scripts/ must be invoked by the launcher preflight.
# A suite that exists but gates nothing protects nothing, and its absence from
# the preflight is invisible: the launch still succeeds, the suite still passes
# when run by hand, and only an unguarded defect reaching a multi-day run reveals
# that nothing ever ran it. This is the same absence-read-as-success shape the
# suites themselves exist to catch, so assert coverage rather than assume it.
verify_preflight_covers_every_suite() {
  local root="$1"
  local launcher="${root}/scripts/launch-full-study-queues.sh"
  local missing=() present=0 suite name
  for suite in "${root}"/scripts/test_*.sh; do
    [[ -f "${suite}" ]] || continue
    present=$((present + 1))
    name="${suite##*/}"
    grep -q "scripts/${name}\"" "${launcher}" || missing+=("${name}")
  done
  if (( present == 0 )); then
    echo "no regression suites found; the glob or the layout changed" >&2
    return 1
  fi
  if (( ${#missing[@]} > 0 )); then
    printf 'suite not invoked by the launcher preflight: %s\n' "${missing[@]}" >&2
    return 1
  fi
  return 0
}

if ! verify_preflight_covers_every_suite "${repository_root}"; then
  echo "the launcher preflight does not run every regression suite" >&2
  exit 1
fi
# Negative control: a suite the preflight does not name must be caught. Without
# this, a broken glob would report success for every revision.
cp "${repository_root}/scripts/test_study_queue_chain.sh" \
  "${fixture_root}/scripts/test_never_gated_probe.sh"
if verify_preflight_covers_every_suite "${fixture_root}" 2>/dev/null; then
  echo "an ungated suite went undetected" >&2
  exit 1
fi

echo "full-study queue chain tests pass"
