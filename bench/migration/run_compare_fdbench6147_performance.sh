#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'usage: %s OUTPUT.json\n' "${0##*/}" >&2
}

if [[ $# -ne 1 ]]; then
  usage
  exit 2
fi

script_directory="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
repository_root="$(cd -- "${script_directory}/../.." && pwd -P)"
candidate_output="$1"
if [[ "${candidate_output}" != /* ]]; then
  candidate_output="$(pwd -P)/${candidate_output}"
fi
if [[ -e "${candidate_output}" ]]; then
  printf 'candidate evidence already exists: %s\n' "${candidate_output}" >&2
  exit 2
fi

go_binary="${OPENREALTIME_GO_BIN:-${repository_root}/.runtime/toolchains/go1.25.0/bin/go}"
affinity="${OPENREALTIME_BENCH_AFFINITY:-8}"
gomaxprocs=1
benchtime=1x
warmups=1
samples=12
baseline="${script_directory}/testdata/compare_fdbench6147_performance_baseline.json"
benchmark='^BenchmarkCompareFDBench6147$'
source_files=(
  bench/migration/adversarial_test.go
  bench/migration/canonical.go
  bench/migration/compare.go
  bench/migration/stats.go
  bench/migration/types.go
  bench/migration/validate.go
)

if [[ ! -x "${go_binary}" ]]; then
  printf 'Go toolchain is not executable: %s\n' "${go_binary}" >&2
  exit 2
fi
for required_command in git sha256sum taskset; do
  if ! command -v "${required_command}" >/dev/null 2>&1; then
    printf 'required command is unavailable: %s\n' "${required_command}" >&2
    exit 2
  fi
done
if ! taskset -c "${affinity}" true >/dev/null 2>&1; then
  printf 'requested CPU affinity is unavailable: %s\n' "${affinity}" >&2
  exit 2
fi

temporary_directory="$(mktemp -d "${TMPDIR:-/tmp}/openrealtime-fdbench6147.XXXXXXXX")"
raw_output="${temporary_directory}/benchmark.txt"
cleanup() {
  rm -f -- "${raw_output}"
  rmdir -- "${temporary_directory}" 2>/dev/null || true
}
trap cleanup EXIT

cd -- "${repository_root}"
benchmark_command=(
  taskset -c "${affinity}"
  env "GOMAXPROCS=${gomaxprocs}" GOFLAGS=
  "${go_binary}" test ./bench/migration
  -run '^$'
  -bench "${benchmark}"
  -benchtime "${benchtime}"
  -benchmem
)

for ((warmup = 0; warmup < warmups; warmup++)); do
  "${benchmark_command[@]}" -count=1 >/dev/null
done
"${benchmark_command[@]}" -count="${samples}" | tee "${raw_output}"

cpu="$(sed -n 's/^cpu: //p' "${raw_output}" | head -n 1)"
if [[ -z "${cpu}" ]]; then
  printf 'benchmark output did not report a CPU model\n' >&2
  exit 2
fi
go_version="$(GOFLAGS='' "${go_binary}" version | awk '{print $3}')"
goos="$(GOFLAGS='' "${go_binary}" env GOOS)"
goarch="$(GOFLAGS='' "${go_binary}" env GOARCH)"
source_revision="$(git rev-parse HEAD)"
source_digest="$(sha256sum "${source_files[@]}" | sha256sum | awk '{print $1}')"
source_modified=false
if [[ -n "$(git status --porcelain --untracked-files=all -- "${source_files[@]}")" ]]; then
  source_modified=true
fi
source_csv="$(IFS=,; printf '%s' "${source_files[*]}")"
modified_argument=()
if [[ "${source_modified}" == true ]]; then
  modified_argument=(-source-modified)
fi

GOFLAGS='' "${go_binary}" run ./bench/migration/perfprotocol \
  -baseline "${baseline}" \
  -benchmark-output "${raw_output}" \
  -output "${candidate_output}" \
  -go-version "${go_version}" \
  -goos "${goos}" \
  -goarch "${goarch}" \
  -cpu "${cpu}" \
  -gomaxprocs "${gomaxprocs}" \
  -affinity "${affinity}" \
  -benchtime "${benchtime}" \
  -warmups "${warmups}" \
  -samples "${samples}" \
  -source-revision "${source_revision}" \
  "${modified_argument[@]}" \
  -source-sha256 "${source_digest}" \
  -source-files "${source_csv}"
