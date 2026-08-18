# Milestone evidence

## M0 — Research specification and reproducibility scaffold

- Status: complete
- Completed: 2026-08-17
- Checkpoint command: `./scripts/reproduce_m0.sh`

### Deliverable evidence

| Requirement | Evidence |
| --- | --- |
| Ratified glossary | `docs/glossary.md` |
| Prospective hypotheses and baseline protocol | `docs/experiments.md` |
| Primary metric definitions | `docs/metrics.md` |
| Trace schema and causality rules | `schemas/trace-record-v0.2.schema.json`, `docs/protocol.md`, and `trace/` tests |
| OpenAI Realtime event compatibility | 133 generated profile/direction definitions, full nested schema closure, and conformance tests in `protocol/openai/` |
| Literature review | `docs/research/literature-review.md` |
| Benchmark inventory | `docs/research/benchmark-inventory.md` |
| Minimal audio replay | `internal/audio/`, `replay/`, and `cmd/openrealtime` |
| Fixture provenance | `docs/fixtures.md` and SHA-256-pinned CC0 fixture |
| License and governance decision | `LICENSE`, `LICENSES.md`, governance files, and ADR-0002 |
| Implementation-language decision | ADR-0001 |

### Exit-criteria evidence

The reproduction command builds a release binary, independently refetches and
hash-checks the pinned official OpenAI OpenAPI source, regenerates the 24 kHz
fixture, emits and validates 51 OpenAI-compatible client events, validates 51
causally ordered trace records, compares the WAV/event/trace artifacts byte-for-
byte with checked-in goldens, and runs the Go race, unit, vet, and formatting
gates.

The canonical fixture starts at monotonic zero and commits after exactly
1,000,000,000 ns. This is reproducibility evidence, not a latency or quality
claim.

## M1 — Endpointed reference baseline

- Status: complete
- Completed: 2026-08-17
- Checkpoint command: `./scripts/reproduce_m1.sh`

### Deliverable evidence

| Requirement | Evidence |
| --- | --- |
| Streaming perception adapter | Strict ordered-frame manifest adapter in `adapters/reference` |
| Language adapter | Final-revision-only fixed cognition adapter in `adapters/reference` |
| Speech adapter | Deterministic PCM16 signal adapter in `adapters/reference` |
| Conventional endpointing | `baseline/endpointed.go` forbids response creation before input commit and finalization |
| End-to-end visualization | Self-contained HTML/SVG renderer in `visualization/timeline` |
| Repeated distributions | 30 seeded raw trials plus P50/P90/P95/P99 summaries in `benchmarks/m1/reference/report.json` |
| Stage reconciliation | Observed output-playback-marker latency equals six measured stage/queue durations with 0 ns maximum error |
| Protocol conformance | Every one of 30 complete traces is validated against the pinned OpenAI Realtime schemas |

### Exit-criteria evidence

The checkpoint regenerates the reference report, all 30 complete traces, and
the timeline. It compares the report and representative evidence byte-for-byte,
then runs the OpenAI specification provenance check and Go race, unit, vet, and
formatting gates. The 30-trial simulated latency is 170,745,602–227,753,374 ns
with P50 201,525,834 ns and P95 222,334,515 ns; reconciliation error is 0 ns in
every trial.

M1 uses symbolic fixture-bound perception and deterministic signal speech. The
numbers prove instrumentation and causality only; they do not measure ASR,
speech naturalness, or production latency.

## M2 — Microturn engine

- Status: complete
- Completed: 2026-08-17
- Checkpoint command: `./scripts/reproduce_m2.sh`

### Deliverable evidence

| Requirement | Evidence |
| --- | --- |
| Fixed scheduler | 50/100/200/400/800 ms policies in `microturn/scheduler.go` |
| Event scheduler | Revision-triggered policy plus endpoint opportunity |
| Revision-aware state | Stable-prefix and monotonic-source invariants in `microturn/ledger.go` |
| Candidate lifecycle | Explicit prepare, supersede, cancel, and stale-result rejection |
| Same components as B0 | M1 reference perception, cognition, and speech adapters reused unchanged |
| Cadence ablation | Six paired 30-trial conditions in `benchmarks/m2/reference/report.json` |
| Attribution | Observed-minus-B0 equals negative planning overlap with 0 ns error in every trial |

The revision-event condition has 71.583 ms P50 simulated output-marker latency
versus B0's 201.526 ms P50, with paired delta -128.333 ms. This is deterministic
orchestration evidence over symbolic cues, not real model or semantic-audio
performance. Coarser fixed conditions tying is explicitly retained as a fixture
effect rather than hidden.

## M3 — Incremental speech commitment and duplex behavior

- Status: complete
- Completed: 2026-08-17
- Checkpoint command: `./scripts/reproduce_m3.sh`

### Deliverable evidence

| Requirement | Evidence |
| --- | --- |
| Streaming synthesis | Five contiguous 20 ms reference chunks through `StreamingSpeechProvider` |
| Commit horizon | Bounded prepared/queued/played sample state in `speech/commit.go` |
| Truncation and cancellation | Exact discard accounting and OpenAI cancel/clear/truncate event traces |
| Played-history safety | Invalidated played audio blocks closure until explicit repair |
| Continuous input | Client audio append occurs while server output buffer is active |
| Overlap policy | Directed speech yields; backchannel and side-speech scenarios continue |
| Automated metrics | Stop latency, false/failure-to-stop, repairs, discard, and history violations |

The directed scenario's injected stop latency is P50 17 ms and P95 26 ms with
0 failures. Backchannel and side-speech scenarios have 0 false stops; all 30
invalidation trials record a repair; all 120 trials have 0 played-history
violations. These are deterministic safety checks over known labels and signal
audio, not live classifier or device-performance claims.

## M4 — Fast/slow cognition

- Status: complete
- Completed: 2026-08-17
- Checkpoint command: `./scripts/reproduce_m4.sh`

### Deliverable evidence

| Requirement | Evidence |
| --- | --- |
| Asynchronous contract | `DeliberationProvider`, streamed updates, and context-aware runner |
| Stale safety | Exact goal/revision matching and late callback rejection |
| Lifecycle closure | Completed, failed, explicitly cancelled, and parent-cancelled tests |
| Truthful foreground | Progress claims validated against actual slow state |
| Difficult workload | Three project-authored symbolic tasks in `tests/fixtures` |
| Cost/quality comparison | 270 raw trials and frontier in `benchmarks/m4/reference` |

Fast/slow moves P50 truthful progress from 386.242 ms to 34.909 ms while
retaining symbolic P50 quality 94 and 90/90 successes, at compute P50 97 versus
88. Fast-only is quicker and cheaper but has quality P50 38 and 0/90 successes.
All values are simulated/authored reference units, not model claims.

M4's foreground-decision and slow-update roles remain a historical baseline.
Plan version 0.2 uses them as the independent fast/slow control rather than the
target continuous-thinking interface. The target has one canonical trajectory
with fast and slow model continuations appending successive ordinary items.

## M5 — Translation and rapid-interaction demonstrations

- Status: complete
- Completed: 2026-08-17
- Checkpoint command: `./scripts/reproduce_m5.sh`

### Deliverable evidence

| Requirement | Evidence |
| --- | --- |
| Simultaneous translation | Endpointed, stable, and aggressive append-only policies in `translation/` |
| Complete translation lifecycle | 90 traces across all three client and six non-error server event types |
| Rapid audio game | Four-round Signal Match benchmark in `rapidgame/` |
| Public fixtures and demos | CC0 symbolic manifest plus two key-free scripts |
| Balanced reporting | Lag/reaction, exact task quality, failures, compute, and protocol record counts |

Stable incremental translation has 79.399 ms P50 authored mean lag and exact
quality 100 versus 586.616 ms for endpointed output, at higher compute. The
aggressive counterexample is quicker but has quality 60. Signal Match microturn
reaction P50 is 63.651 ms with zero deadline misses versus 152.303 ms and 102
misses endpointed. These are injected symbolic results, not language or device
performance.

## M6 — Comparative study and research release

- Status: complete for the open reference condition
- Completed: 2026-08-17
- Checkpoint command: `./scripts/reproduce_m6.sh`

### Deliverable evidence

| Requirement | Evidence |
| --- | --- |
| Endpointed/microturn comparison | Paired raw trials and deterministic 10,000-resample median intervals |
| Native/hybrid scope | Native explicitly `not_run`; hybrid fast/slow retained in a separate task group |
| Predefined ablations | M2 cadence, M3 duplex, M4 cognition, and M5 translation/game groups |
| Human study | Prospective preregistration, ethics gate, exclusions, randomization, and analysis protocol; not run |
| Technical report | `docs/research/technical-report-v0.1.md` with claims and counterexamples |
| Versioned release | 41-file SHA-256 manifest plus machine-readable `study.json` |

All claims point to published conditions and name their scope. Incomparable
workloads are never combined into a leaderboard. Native provider and human
participant results remain blocked, visible, and unclaimed.

## M7 — Stable research engine

- Status: complete
- Completed: 2026-08-17
- Checkpoint command: `./scripts/reproduce_m7.sh`

### Deliverable evidence

| Requirement | Evidence |
| --- | --- |
| Versioned component API | Independent semantic import path `api/v1`, version 1.0.0 |
| Stable adapters | Five provider roles bridged in `adapters/reference/v1` |
| Protocol conformance | All 133 registry definitions, 178 schemas, 66 unique names, and strict fault probes |
| Provider conformance | Descriptor, capability, cancellation, revision, PCM, decision, and deliberation checks |
| Contributor documentation | `docs/api-v1.md`, compatibility policy, release notes, and updated contribution gate |
| Maintained examples | Direct external-style consumer compiled and byte-compared by M7 |

Research internals remain explicitly experimental; breaking stable provider
changes require a new `api/v2` semantic import path. Downstream consumers can
pin the v1.0.0 release and run the same public conformance suite.

## M8 — Live local microturn cascade

- Status: in progress; first live slice completed 2026-08-18
- Implemented: stateful Qwen3-ASR 0.6B, local Qwen3-30B-A3B-FP8, streaming
  Fish S2-Pro, 50 ms scheduler/200 ms provider buffering, exact-match fast
  → slow private preparation, content-independent slow-launch pacing with
  exact-commit bypass, and priority/capacity admission on one 96 GB GPU
- Evidence: `docs/live-cascade.md`, the exact-scored
  `realtime-benchmark-v0.6` background result, v0.7 endpointed/fast-only
  controls, the v0.8 one-second pacing ablation, and preserved negative results
  in `benchmarks/results/`
- Required comparisons: endpointed, 50/100/200/400/800 ms, revision-event, and
  adaptive scheduling using the same component versions
- Boundary: fixed ticks are opportunities; actual component invocations are
  reported separately

## M9 — Canonical trajectory and interleaved thinking

- Status: in progress; first heterogeneous tool-grounding slice completed
  2026-08-18
- Implemented: append-only trajectory, Qwen/Gemini context compilers,
  proposal-only fast calls, execute-authority slow calls, exact causal results,
  unconditional slow continuation, private fast→slow preparation, exact
  per-stage replay/live fallback, temporal launch pacing that cannot delay
  exact commit, tool-result continuation, and exact name-plus-JSON-argument
  scoring; one structured safe-point event-loop owner, versioned atomic model
  commits, typed interruption, complete asynchronous result transactions,
  common agent policy, and cancellation-aware audible-history projection
- Target: fast and slow models continue one trajectory containing observations,
  reusable reasoning, assistant content, proposals, executable calls, and
  results
- Primary conditions: Gemini 3.5 Flash minimal→medium/high thinking and local
  Qwen instruct→Gemini 3.5 Flash medium/high thinking
- Control: M4-style independent foreground/background advice
- Compatibility: experimental internals first; stable replacement requires
  `api/v2`

## Planned M10 — Joint responsiveness–intelligence study

- Status: planned
- Target: attribute gains separately to microturn timing and canonical-trajectory
  continuation, then test whether they compose in one live condition
- Comparators: endpoint/VAD-triggered online speech models and persistent
  short-block native interaction models are separate conditions
- Required outcomes: first semantic audio, final reasoning/tool quality,
  contradiction and capability consistency, repair, compute, queueing, and cost
- Primary joint harness: pinned τ-Voice full airline/retail/telecom matrix in
  control and regular speech conditions; local-endpoint/Fish harness patch is
  verified, while the persistent gateway, provider conformance, and disclosed
  regular-condition Fish voices remain pending, so no local score is claimed
