# FD-Bench comparison performance evidence

`run_compare_fdbench6147_performance.sh` executes the checked performance
protocol for `BenchmarkCompareFDBench6147` and writes a canonical,
machine-readable candidate evidence file. The output is create-only: choose a
new path for every run.

```sh
bench/migration/run_compare_fdbench6147_performance.sh /tmp/fdbench-candidate.json
```

The default protocol uses the repository's Go 1.25.0 toolchain, pins the
benchmark to CPU 8 with `taskset`, sets `GOMAXPROCS=1`, performs one untimed
warmup, and then records 12 independent `-benchtime=1x` samples with
`-benchmem`. Set `OPENREALTIME_BENCH_AFFINITY` when CPU 8 is unavailable; that
change is recorded and makes the wall-clock comparison incomparable to the
checked baseline. `OPENREALTIME_GO_BIN` can select another Go binary, whose
reported version and target are also recorded.

The JSON contains the toolchain, platform, CPU model, GOMAXPROCS, enforced CPU
affinity, protocol counts, source revision and digest, every raw numeric
sample, derived summaries, and the SHA-256 of the exact baseline JSON bytes.
The source digest is the SHA-256 of the ordered `sha256sum` records for the
benchmark fixture and its direct comparison implementation files.

Acceptance deliberately has only two non-timing gates:

- Every sample must report one invariant artifact size, and that size must
  exactly equal the checked baseline's 23,462,253 bytes.
- Median `allocs/op` must not exceed the checked baseline median when the Go
  version, platform, GOMAXPROCS, benchtime, warmup count, sample count, and
  source-file scope all match.

An allocation or artifact regression exits 1. A run whose allocation protocol
or source-file scope differs from the baseline is unreportable and cannot be
accepted. A CPU or affinity difference only makes the descriptive timing
comparison incomparable. Malformed, partial, or mixed input and runner failures
exit 2. The candidate evidence is still written for a valid rejected
comparison so the result remains auditable.

`ns/op` has no release threshold: its median and ratio are explicitly
descriptive even when CPU and affinity match. Twelve wall-clock samples on one
machine do not establish a portable statistical margin. `B/op` is also
descriptive because garbage-collection cycle accounting can move it between
otherwise equivalent `-benchtime=1x` samples. The checked baseline must only be
replaced by a deliberate, reviewed recapture; the runner never updates it.
