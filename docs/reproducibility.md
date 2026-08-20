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
comparative study, compares it byte-for-byte, rebuilds the 37-file release
manifest, verifies every listed size and SHA-256 digest, and repeats the full Go
quality gates. The native and human-study rows are deliberately `not_run`; see
the technical report and prospective human-study protocol in `docs/research/`.

The manifest is derived, not frozen: `release build` regenerates it from the
tree, and the checked-in copy is expected to match on every revision. It covers
evidence and version-stamped documents only — traces, reports, fixtures,
schemas, generated protocol bindings, and the `docs/research/` reports. Living
prose such as `PLAN.md` is deliberately outside it, so editing a document never
looks like evidence tampering. When evidence legitimately changes, re-cut the
manifest in the same commit:

```bash
go run ./cmd/openrealtime release build --root . \
  --output benchmarks/releases/v0.1.0/manifest.json
```

A `cmp` or `release verify` failure that you did not intend means an evidence
artifact moved, and that is the one signal this manifest exists to carry.

## M7 stable v0.1.0 release

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
refuses to install a binary whose SHA-256 differs. The same manifest freezes
the Qwen fast server at vLLM 0.19.0, model revision
`d206ba732169f29bb77fbf80fc2c4b81d4d30782`, and its native 40,960-token
window. Long benchmark launchers run through `scripts/with-study-runtime.sh`;
the local cascade refuses to reuse a healthy gateway with a different hash.
The manifest also freezes an exclusive process-ancestry GPU policy. Preflight
rejects a compute PID outside the registered local service trees, and every
scored provider invocation runs under a five-second ownership guard. Its
append-only checks and content-addressed summary are retained with the run;
even a transient violation invalidates that invocation. This prevents an
unrelated colocated workload from silently changing latency or memory pressure.
The earlier `matrix-v1` population is a preserved pre-freeze pilot and is
intentionally absent from the full-study manifest.

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

After the local Qwen, Gemini-backed gateway, ASR, and Fish services are healthy,
launch the entire dependency-ordered queue chain once with:

```bash
scripts/launch-full-study-queues.sh
```

Use `--check` to run the complete preflight without starting processes. The
launcher refuses a dirty source tree, missing scripts, or any live duplicate
queue. It also refuses a pre-existing causal τ experiment/invocation or
external-suite run root, preventing process-level resume from silently mixing
source revisions. The launcher records the exact source, script hashes, and
PIDs; preserves prior queue
logs; and detaches all ten queues. Each successor waits for the current-run
completion marker from its predecessor, so a failed stage halts the chain
rather than skipping ahead.

The launcher preflights `python3`, so every queue and every documented command
invokes `python3` or an explicit virtual-environment interpreter path, never a
bare `python`. A stock Ubuntu PATH has no `python`, so an unpinned invocation
reproduces only on a machine that happens to provide one, and it fails at the
end of a multi-day serial chain instead of at launch.
`scripts/test_study_interpreter_pinning.sh` enforces this and runs inside the
launcher preflight.

The chain's ten seams are string-matched, not structural: each queue publishes
a completion marker with `echo` and its successor requires that exact line with
`grep -Fx`. A reworded marker links nothing, and the chain would halt at that
seam mid-run with the predecessor reporting success.
`scripts/test_study_queue_chain.sh` reads the launcher as the source of truth
for chain membership and checks every seam statically: each queue publishes a
marker, each successor requires the marker its own watched predecessor
publishes, and no queue is launched before the queue it waits on. It also runs
inside the launcher preflight.

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

Queue liveness distinguishes existence from progress. Each queue reports its
kernel `state` alongside `progressing`, because a supervisor that has been
stopped (`T`) or left unreaped (`Z`) keeps its pid and answers an existence
check while advancing nothing. The study is one serial dependency chain, so
such a supervisor silently blocks every queue behind it. A non-complete queue
whose supervisor is stopped or unreaped raises a warning and moves the monitor
to `attention`; resume it with `kill -CONT` and confirm the state returns to
`S` or `R`. Terminal supervisor loss is still reported separately as a queue
with no matching live process.

After the long-running serial benchmark queue completes, run:

```bash
python3 scripts/report-full-study.py \
  --repository-root . \
  --manifest benchmarks/full-study-v1.json
```

The command has no partial mode. It first verifies the pinned runtime manifest,
then checks every frozen tau-Voice task/trial population and execution record,
including the host boot identity, process start identity, executable hash, and
argv hash of the long-lived local runtime. Both the initial and final identity
must carry the study gateway hash and describe the same processes;
the reporter also rehashes every ownership log, verifies every interval check,
and requires exact guard coverage for every τ cell/domain and every completed
external-benchmark invocation;
each matrix or external suite must come from one clean process invocation, and
all 17 populations must report the same OpenRealtime orchestration revision;
process-level resume across source revisions is therefore not publishable,
while bounded exception-only retries inside the frozen invocation remain valid;
the terminal reporter itself must run from that revision with a clean worktree;
local-fast identities must additionally report vLLM 0.19.0, the pinned Qwen
model and served-model names, and the frozen 40,960-token native context window;
each tau-Voice raw archive must also prove the preregistered exception-only
retry bound, exactly one scoring attempt per task, and retention of any failed
infrastructure attempts;
then it checks the paired endpoint-preparation manipulation,
all 498 FDB v1.5 overlap trials and their deterministic aggregate, all 100 FDB
v3 tool-use examples in both official exact and GPT-4o evaluations, and all
6,147 FD-Bench conversations, Silero traces, and 21 timing reports. For each of
these three external suites it also requires the preregistered three-attempt
lifetime budget, a contiguous attempt ledger for every planned trial, and
exactly one terminal successful attempt; restarting a runner never refreshes
that budget. Each of those run manifests must also record its terminal-failure
ledger explicitly, as a list. A missing ledger is refused rather than read as
zero: a runner that stopped before recording its failures would otherwise
write a manifest indistinguishable from a clean run, so absence would be
published as success. WER, CPPL, and the subjective GPT score remain explicitly not
evaluated because the released non-Moshi path does not produce their required
inputs. The output is `.runtime/benchmark-runs/full-study-v1/report.json`; any
missing population, terminal failure, retry-provenance violation, hash
mismatch, or evaluator gap prevents that file from being published. The preregistered scope may not be empty
either: a manifest declaring no tau-Voice matrix, or an external suite whose
population is zero, verifies nothing and is refused rather than published as a
complete panel over nothing. As a final invariant the panel itself may not
carry a null: panel values are copied out of
upstream artifacts, so a field a runner never wrote would otherwise be
published as a result. The single exception is the official exact evaluation's
response-quality aggregate, which this gate independently requires to be
empty because that evaluation produces no LLM score.

The evidence panel commits to raw outputs as deterministic
`path\0size\0sha256` trees. The terminal reporter directly rehashes all FDB
v1.5 and FDB v3 result/audio files. Every scored audio artifact must arrive
with the SHA-256 its runner recorded: a tree entry with no declared hash would
still be committed, and would still look authoritative in the panel, while
being bound to nothing. A tau-Voice archive must likewise record how many
infrastructure attempts failed, so that a run with nothing retained is
distinguishable from a run that never wrote the count. FD-Bench avoids a redundant scan of its
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

## Post-hoc analysis of recorded populations

Three read-only tools describe recorded populations. None participates in
scoring, none writes into a population, and each refuses an unscored or
internally inconsistent input rather than reporting around it. They exist
because the publication gate reports counts and means, and three properties of
the pilot are invisible in both.

Each tool reports the scope of what it read. `results.json` records the task
list and `num_trials` a run was launched with, so a population that stopped
early -- or has not finished yet -- states its own shortfall, and the tools
reconcile that declaration against what the population holds. Completeness is
the agreement of three counts: the declared scope, the simulation index, and
the files on disk. The first two are assertions inside `results.json`; only the
third is the population a tool actually reads, so an index naming fifty entries
over six files would otherwise pass as a whole cell. A population declaring no
scope reports `declared: false` and claims no completeness at all.

Failure mechanism, rather than a termination-reason count:

```bash
scripts/classify-tau-voice-failures.py --population PATH [--population PATH ...]
```

About 10.7% of pilot simulations end at the tau2 environment-error budget
(`max_errors = 10`). All of them are unresolved spoken identifiers, so a mean
they lowered looks exactly like one a policy difference lowered. The classifier
names a mechanism only from evidence in the record and reports `unclassified`
otherwise. A failure filed under the nearest plausible bucket is worse than one
left unnamed.

Response-latency tails, rather than a mean latency:

```bash
scripts/analyze-tau-voice-tails.py --population PATH [--population PATH ...]
```

Median response latency is 1.40 s in all four pilot populations while p90 is
about 9 s. The worst observation is a 106.4 s silence, with no agent tool call
during the gap, in a task that still scored reward 1.0 -- so reward cannot see
it. Latency is measured on the harness tick clock rather than wall time, and a
population mixing two tick durations is refused instead of averaged across two
clocks.

How much of a paired difference survives task sampling:

```bash
scripts/paired-task-inference.py --baseline PATH --treatment PATH
```

Pairing is by task, not by trial, so this applies at one trial per task. On the
two airline populations the canonical condition leads by roughly ten points,
which reads as a win, but fewer than a third of paired tasks are discordant,
the exact sign test clears p = 0.2, and the bootstrap interval spans zero. The
size of that lead is itself unsettled: the canonical cell was still being
written, and its margin moved from +0.095 over 42 paired tasks to +0.133 over
45 as the run progressed. That is why each side reports its own completeness
beside the interval -- quoting a difference from a growing cell invites the
reader to treat it as a finished one. The output also states the limit of the
estimator: it bounds task-sampling variation and not run-to-run variation,
which needs repeated trials the current matrices do not run.
