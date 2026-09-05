#!/usr/bin/env bash
#
# What the untracked working directories hold, and what is safe to remove.
#
#   ./scripts/workspace-inventory.sh              # report only
#   ./scripts/workspace-inventory.sh -older-than 30  # report, marking older runs
#   ./scripts/workspace-inventory.sh -older-than 30 -delete
#
# `.runtime` and `artifacts` are gitignored, so nothing here is under version
# control and nothing here is recoverable. That is the whole reason this script
# reports by default and deletes only when told: the directories hold the
# evidence behind measurement claims, and "it was only cache" is a thing people
# say afterwards.
#
# What it will not offer to delete:
#
#   - anything a tracked file names. A prepare script, a document, or a test
#     that reads `.runtime/tau2-bench` is a live dependency, and a run that is
#     cited as evidence is the evidence.
#   - anything newer than the age given, which defaults to keeping everything.
#
# Model weights and datasets are the bulk of `.runtime` and are exactly what a
# prepare script re-downloads, so they are named as dependencies rather than
# treated as spoil.

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
cd "${repository_root}"

older_than_days=""
delete=0
while (($#)); do
  case "$1" in
    -older-than)
      shift
      older_than_days="${1:-}"
      [[ "${older_than_days}" =~ ^[0-9]+$ ]] || {
        echo "-older-than takes a number of days" >&2
        exit 2
      }
      ;;
    -delete) delete=1 ;;
    -h | -help | --help)
      sed -n '2,26p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      exit 2
      ;;
  esac
  shift
done

if ((delete)) && [[ -z "${older_than_days}" ]]; then
  echo "-delete requires -older-than: deleting everything is not a retention policy" >&2
  exit 2
fi

# referenced collects every .runtime and artifacts path named by a tracked
# file. git ls-files rather than a find, so an untracked scratch note cannot
# protect a directory by mentioning it.
referenced_paths="$(mktemp)"
trap 'rm -f "${referenced_paths}"' EXIT
git ls-files -z |
  xargs -0 grep -hoE '(\.runtime|artifacts)/[A-Za-z0-9._-]+' 2>/dev/null |
  sort -u >"${referenced_paths}" || true

referenced() {
  grep -qxF "$1" "${referenced_paths}"
}

total_bytes=0
removable_bytes=0
removable=()

report_tree() {
  local tree="$1" entry name bytes age marker
  [[ -d "${tree}" ]] || return 0
  printf '\n%s\n' "${tree}"
  for entry in "${tree}"/*; do
    [[ -e "${entry}" ]] || continue
    name="${entry#./}"
    bytes="$(du -sb "${entry}" 2>/dev/null | cut -f1)"
    bytes="${bytes:-0}"
    total_bytes=$((total_bytes + bytes))
    marker="   "
    if referenced "${name}"; then
      marker="dep"
    elif [[ -n "${older_than_days}" ]] &&
      [[ -n "$(find "${entry}" -maxdepth 0 -mtime "+${older_than_days}" -print -quit)" ]]; then
      marker="old"
      removable+=("${entry}")
      removable_bytes=$((removable_bytes + bytes))
    fi
    age="$(date -r "${entry}" +%Y-%m-%d 2>/dev/null || echo "?")"
    printf '  %s  %10s  %s  %s\n' "${marker}" "$(numfmt --to=iec "${bytes}")" "${age}" "${name}"
  done
}

report_tree .runtime
report_tree artifacts

printf '\ntotal %s' "$(numfmt --to=iec "${total_bytes}")"
if [[ -n "${older_than_days}" ]]; then
  printf ', %s in %d entries older than %s days and named by no tracked file' \
    "$(numfmt --to=iec "${removable_bytes}")" "${#removable[@]}" "${older_than_days}"
fi
printf '\n'
printf 'dep = named by a tracked file, so something in this repository depends on it\n'

if ((delete)); then
  if ((${#removable[@]} == 0)); then
    echo "nothing to remove"
    exit 0
  fi
  printf 'removing %d entries\n' "${#removable[@]}"
  rm -rf -- "${removable[@]}"
  printf 'removed %s\n' "$(numfmt --to=iec "${removable_bytes}")"
fi
