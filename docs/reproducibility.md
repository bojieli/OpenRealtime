# M0 reproducibility

M0 proves that a contributor can recreate a timing trace from a publicly
redistributable audio fixture before any engine optimization.

## Prerequisites

- A POSIX shell
- Go 1.25 or newer
- `curl` for the pinned specification provenance check

## One-command reproduction

From the repository root:

```bash
./scripts/reproduce_m0.sh
```

The script downloads Go modules, builds a trimmed production binary, verifies
the pinned official OpenAI schema extraction, regenerates the one-second WAV
into ignored `artifacts/`, replays it in 20 ms frames, validates every OpenAI
event plus whole-trace causality, compares all artifacts byte-for-byte with the
CC0 golden files, and runs race, test, vet, and formatting gates.

The canonical replay contains 50 OpenAI `input_audio_buffer.append` client
events followed by one `input_audio_buffer.commit` event. It starts at
monotonic time zero and commits at exactly 1,000,000,000 ns. The input is mono
PCM16 at the OpenAI-required 24 kHz rate, and each wire event carries standard
base64 audio.

## Manual commands

```bash
go build -trimpath -o artifacts/openrealtime ./cmd/openrealtime
artifacts/openrealtime fixture generate artifacts/m0-tone.wav
artifacts/openrealtime replay artifacts/m0-tone.wav \
  --events artifacts/m0-openai-events.jsonl \
  --trace artifacts/m0-trace.jsonl --session-id m0-replay --frame-ms 20
artifacts/openrealtime protocol validate \
  --profile realtime --direction client artifacts/m0-openai-events.jsonl
artifacts/openrealtime trace validate artifacts/m0-trace.jsonl
artifacts/openrealtime trace summarize artifacts/m0-trace.jsonl
```

The fixture is procedurally generated square-wave audio. It contains no speech,
personal data, imported recording, or synthesized voice. Its provenance and
license are recorded in [fixtures.md](fixtures.md).

## M1 endpointed reference condition

Run:

```bash
./scripts/reproduce_m1.sh
```

This command builds the production binary, rechecks the pinned official
protocol source, runs 30 seeded endpointed trials, validates every generated
trace, and compares the report, trial-0000 trace, and HTML timeline with the
checked reference artifacts. It then runs the same race, vet, and formatting
gates as M0. See [m1-baseline.md](m1-baseline.md) for the stage equation,
reported distribution, and limitations.

## M2 cadence ablation

Run:

```bash
./scripts/reproduce_m2.sh
```

The command regression-checks all M1 reference traces, then regenerates the six
paired M2 scheduling conditions, complete action ledger, and HTML ablation
view. It compares the report and visualization byte-for-byte and runs the full
race, test, vet, and formatting gates. See [m2-engine.md](m2-engine.md) for the
attribution equation and interpretation limits.

## M3 duplex and repair scenarios

Run:

```bash
./scripts/reproduce_m3.sh
```

The command first executes the complete M2 regression gate. It then regenerates
120 M3 scenario traces, validates every OpenAI event and causal envelope, and
compares the raw report plus four representative traces and timelines
byte-for-byte. See [m3-duplex.md](m3-duplex.md) for horizon semantics and result
limits.

## M4 fast/slow cognition

Run:

```bash
./scripts/reproduce_m4.sh
```

The command executes the complete M3 regression gate, including OpenAI protocol
validation and race tests, then regenerates the 270-trial fast/slow report and
HTML frontier byte-for-byte. See [m4-fast-slow.md](m4-fast-slow.md) for lifecycle
semantics and interpretation limits.

## M5 translation and rapid interaction

Run:

```bash
./scripts/reproduce_m5.sh
```

The command executes the complete M4 regression hierarchy, then regenerates 90
OpenAI Translation-profile traces and 60 GA Realtime game traces. It validates
all 150 causal traces, compares the report, dashboard, representative traces,
and timelines byte-for-byte, then runs race, vet, and formatting gates. No API
key, provider SDK, browser client, or proprietary fixture is required. See
[m5-demonstrations.md](m5-demonstrations.md) for metric semantics and limits.

## M6 comparative reference release

Run:

```bash
./scripts/reproduce_m6.sh
```

After the complete M5 hierarchy, this command rebuilds the machine-readable
comparative study, compares it byte-for-byte, rebuilds the 41-file release
manifest, verifies every listed size and SHA-256 digest, and repeats the full Go
quality gates. The native and human-study rows are deliberately `not_run`; see
the technical report and prospective human-study protocol in `docs/research/`.

## M7 stable v1.0.0 release

Run:

```bash
./scripts/reproduce_m7.sh
```

The final gate executes M0–M6, compares the combined protocol/provider
conformance result and direct-consumer example byte-for-byte, compiles the
maintained example, and repeats race, vet, and formatting checks. It validates
133 OpenAI event definitions and all five stable provider roles. No Python,
provider account, microphone, browser, or proprietary client is required.

## M10 full-study publication gate

First reproduce or verify the exact gateway under test:

```bash
scripts/prepare-study-gateway.sh
```

The command checks
[`benchmarks/runtime/canonical-gateway-v1.json`](../benchmarks/runtime/canonical-gateway-v1.json),
archives its pinned source revision, applies the declared trimmed build, and
refuses to install a binary whose SHA-256 differs. Long benchmark launchers run
through `scripts/with-study-runtime.sh`; the local cascade refuses to reuse a
healthy gateway with a different hash. The earlier `matrix-v1` population is a
preserved pre-freeze pilot and is intentionally absent from the full-study
manifest.

Each completed τ matrix then runs:

```bash
scripts/archive-tau-voice-artifacts.sh benchmarks/tau-voice/MATRIX.json
```

The scorer-required `results.json` and simulation trajectories remain
expanded. Duplicate raw WAV/task-log trees are removed only after a
deterministic PAX+Zstandard archive is readable and its SHA-256, size, source
counts, matrix hash, cell, and domain have been atomically recorded. The final
publication gate rehashes every archive and refuses residual expanded copies;
each archive restores the original tree losslessly with `tar --zstd -xf`.

For operational progress during the multi-day run, use:

```bash
python3 scripts/report-full-study-progress.py \
  --repository-root . \
  --output .runtime/benchmark-runs/full-study-v1/progress.json
```

`run-full-study-monitor.sh` refreshes the same JSON every 60 seconds. It counts
the full 7,784 τ task trials, 84 raw archives, 498 FDB1.5 samples, 100 FDBv3
samples, 6,147 FD-Bench conversations, queue liveness, terminal failures, and
disk headroom. This status file never scores partial populations and cannot
declare completion: only a complete report from `report-full-study.py` can set
`publication_complete`.

After the long-running serial benchmark queue completes, run:

```bash
python scripts/report-full-study.py \
  --repository-root . \
  --manifest benchmarks/full-study-v1.json
```

The command has no partial mode. It first verifies the pinned runtime manifest,
then checks every frozen tau-Voice task/trial population and execution record,
including the host boot identity, process start identity, executable hash, and
argv hash of the long-lived local runtime. Both the initial and final identity
must carry the study gateway hash and describe the same processes;
then it checks the paired endpoint-preparation manipulation,
all 498 FDB v1.5 overlap trials and their deterministic aggregate, all 100 FDB
v3 tool-use examples in both official exact and GPT-4o evaluations, and all
6,147 FD-Bench conversations, Silero traces, and 21 timing reports. WER, CPPL,
and the subjective GPT score remain explicitly not evaluated because the
released non-Moshi path does not produce their required inputs. The output is
`.runtime/benchmark-runs/full-study-v1/report.json`; any missing population,
terminal failure, hash mismatch, or evaluator gap prevents that file from
being published.

The evidence panel commits to raw outputs as deterministic
`path\0size\0sha256` trees. The terminal reporter directly rehashes all FDB
v1.5 and FDB v3 result/audio files. FD-Bench avoids a redundant scan of its
much larger audio corpus: the mandatory Silero finalizer already verifies each
WAV against its result record and now emits separate raw-result and audio tree
roots during that pass; the terminal gate validates and retains both roots.

The official FDB v3 evaluator normally falls back to exact argument matching
when a GPT-4o call fails. The full run therefore uses a fail-closed harness
around the pinned `evaluate_all_v2(use_llm=True)` function. It calculates the
judge opportunities from the exact scenario/result population, records only
valid JSON judge responses, and requires every expected call before writing
the GPT-4o report. Each successful-call receipt binds the complete request,
raw response, parsed response, unique response ID, returned GPT-4o model, and
token usage. The judge client is pinned to `https://api.openai.com/v1`, so a
process-level SDK endpoint override cannot silently redirect this independent
evaluation to the local adapter. The separate exact evaluation remains the
unmodified official CLI output.

The three external runners also create a `run-context.json` sidecar before
their first adapter call. Launch refuses a dirty OpenRealtime worktree. The
sidecar binds the runner revision, declared study-gateway hash, and live
component identities, and completion is refused if the gateway, ASR, Fish, or
local Qwen process changed during the invocation. Completion also requires a
clean worktree at the same source revision recorded at launch. A resumed
launcher appends a new invocation and marks an unfinished
predecessor interrupted instead of overwriting it. The terminal gate requires
the last invocation to complete, validates every identity, preserves
interrupted attempts, and hashes each sidecar.

τ-Voice launch evidence follows the same preservation rule. Every cell or
all-cell process writes `run.json` and GPU telemetry below a unique
timestamp/PID attempt directory. Resuming the benchmark population can reuse
validated simulation files, but it cannot overwrite a failed, interrupted, or
completed launcher attempt. A complete attempt records clean source at both
boundaries and matching initial/final OpenRealtime revisions; the terminal
report rejects any complete population lacking that evidence.
