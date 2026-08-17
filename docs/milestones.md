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

- Status: next

Next evidence must cover an asynchronous deliberation stream, truthful
foreground acknowledgement, cancellation and failure, stale goal/revision
rejection, and a difficult-question cost/quality comparison.
