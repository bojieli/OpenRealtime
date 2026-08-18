# OpenRealtime

**Microturn Realtime Intelligence Engine**

OpenRealtime is an open research project investigating a specific question:

> Can an incremental, modular speech system combine the responsiveness of native realtime interaction with the intelligence of a high-reasoning, tool-using agent?

The project is not intended to be another configurable ASR–LLM–TTS wrapper. Its primary outputs are a falsifiable research program, a reproducible benchmark suite, an instrumented reference engine, and evidence about when microturn scheduling works—and when it does not.

The working idea has two orthogonal parts. Microturn scheduling lets streaming ASR, language generation, and TTS advance before a VAD-defined turn has ended. Heterogeneous interleaved thinking lets a low-latency model speak first and a higher-reasoning model continue the same canonical trajectory—including earlier reasoning, audible assistant content, tool calls, and tool results. Together they target high responsiveness and high intelligence without treating fast and slow models as separate agents.

## Target architecture

```text
continuous audio → incremental perception → microturn/event scheduler
                                             │
                                             ▼
                                  one canonical trajectory
                                  ├── fast continuation
                                  ├── slow continuation
                                  ├── fast tool proposals (never executable)
                                  ├── slow tool calls/results
                                  └── assistant content → streaming TTS
```

A 200 ms tick is a decision opportunity, not a command to restart every model. ASR and model sessions retain incremental state; event arrivals such as recognition revisions, user interruption, tool completion, or slow continuation may trigger work between ticks. Fast and slow inference append successive segments to one trajectory rather than exchanging an advisory summary. Both see the real tool schemas: fast calls become non-executable proposals, while only slow calls reach the tool runtime.

Concurrent sources do not mutate that trajectory directly. One safe-point
event loop commits structured event batches, model continuations, and complete
tool-result batches with versioned atomic transactions. Routine events wait for
the next boundary; a trusted typed interruption requests cancellation. No
keyword router or model-authored goal/status machine controls the transition.

The comparative study keeps three categories distinct: endpoint/VAD-triggered
online speech models, persistent short-block interaction models such as Moshi
or Thinking Machines Lab Interaction Models, and this modular microturn
cascade. Matching one latency number is not treated as architectural parity;
tool quality, overlap, prosody, repair, compute, and tail behavior remain part
of the comparison.

## Status

M0 through M7 are complete. The stable component API is v1.0.0 and the complete
OpenAI protocol/provider conformance suite passes.
The repository contains a generated conformance layer for every event in the
pinned OpenAI Realtime OpenAPI specification, deterministic 24 kHz PCM replay, an original
redistributable audio fixture, and causal trace validation. The core Go build
has no Python dependency; optional live Qwen and SGLang-Omni model sidecars use
their own Python environments. M1 adds production-shaped component contracts, a
deterministic endpointed control, exact stage/queue reconciliation, and a
self-contained timeline. Its simulated timings are instrumentation evidence,
not deployed performance claims.

M2 adds fixed and revision-triggered schedulers, immutable stable-prefix
semantics, explicit candidate supersession/cancellation, stale-result rejection,
and paired latency attribution against B0.

M3 adds streaming speech chunks, bounded prepared/queued horizons, irreversible
played history, explicit invalidation repair, continuous input during output,
and schema-valid OpenAI cancellation/clear/truncation traces.

M4 adds goal/revision-scoped asynchronous deliberation, truthful progress
claims, cancellation and failure handling, stale callback rejection, and a
symbolic latency/quality/compute frontier. This remains a reproducible
historical baseline. The plan now targets a canonical-trajectory continuation
interface in which fast and slow models generate successive portions of one
rollout rather than separate foreground decisions and slow advice.

M5 adds the OpenAI Translation session lifecycle, 200 ms PCM16 translation
frames, append-only simultaneous-translation policies, and a GA Realtime rapid
audio game. Both key-free demonstrations publish latency, symbolic task quality,
failures, compute units, and protocol-valid causal traces.

M6 adds deterministic paired-bootstrap analysis, explicit comparability groups,
evidence-scoped claims and counterexamples, a prospective human-study protocol,
and a SHA-256-verified `openrealtime-benchmarks-v0.1.0` release. Native and
participant comparisons are honestly marked not run.

M7 freezes `api/v1`, ships versioned reference adapters and a maintained direct
consumer example, and audits all 133 OpenAI profile/direction definitions plus
their 178-definition schema closure. The suite verifies 66 unique wire names,
strict direction/profile/required-field faults, provider cancellation, revision
and PCM continuity, and deliberation closure.

M8/M9 are in progress. The repository now has experimental stateful Qwen3-ASR,
local vLLM Qwen, Gemini 3.5 Flash, and Fish S2-Pro adapters; a 50 ms scheduler
with 200 ms provider buffering; exact-match latest-revision fast→slow private
preparation; a content-independent slow-launch pacer whose wait is bypassed by
exact commit; proposal-versus-execute tool authority; canonical continuation;
exact tool-trajectory scoring; and a priority/capacity admission governor. A
single-owner asynchronous event loop now adds source-versus-commit provenance,
typed interruption, stale-prefix rejection, atomic external tool results, and
cancellation-aware audible-history projection. A
real co-located audio-to-audio tool trial committed and replayed both prepared
stages, had zero additional endpoint-time fast delay, made one fast proposal
and one independently authorized slow call, and returned the correct grounded
answer. Same-fixture endpointed and fast-only controls, exact call/result
scoring, failed runs, and discarded work remain published. The single-sample
controls expose stage movement, and a one-second pacing ablation reduced
speculative slow launches from 43 to 12 once without changing exact task
quality. These runs do not establish a latency/cost distribution or
native-model parity. See the
[live cascade design and evidence](docs/live-cascade.md).

## Start here

Read [PLAN.md](PLAN.md) for the complete research questions, architecture,
experimental design, milestones, and contribution roadmap. The focused
[canonical trajectory design](docs/canonical-trajectory.md) specifies how
microturn timing, asynchronous events, fast/slow continuation, tools, and
speech commitment fit together without changing the external Realtime wire
protocol. The normative synchronization state machine is in the
[safe-point event-loop design](docs/safe-point-event-loop.md).

To reproduce the M0 timing trace from a clean checkout:

```bash
./scripts/reproduce_m0.sh
```

To reproduce the 30-trial M1 endpointed reference condition:

```bash
./scripts/reproduce_m1.sh
```

To reproduce the paired M2 cadence ablation:

```bash
./scripts/reproduce_m2.sh
```

To reproduce the M3 duplex and repair scenarios:

```bash
./scripts/reproduce_m3.sh
```

To reproduce the M4 fast/slow comparison:

```bash
./scripts/reproduce_m4.sh
```

To reproduce the M5 demonstrations:

```bash
./scripts/reproduce_m5.sh
```

To reproduce and verify the M6 benchmark release:

```bash
./scripts/reproduce_m6.sh
```

To reproduce the complete stable v1.0.0 release:

```bash
./scripts/reproduce_m7.sh
```

These scripts create isolated build artifacts, validate every emitted message
against the official protocol schemas, check trace causality, compare the
result byte-for-byte with the checked-in golden files, and run race, test,
vet, and formatting gates. See
[docs/reproducibility.md](docs/reproducibility.md) for the expected output and
manual commands.

## OpenAI Realtime compatibility

`protocol/openai` is generated from a pinned revision of OpenAI's official
OpenAPI 3.1 specification. It covers GA Realtime, transcription, translation,
and legacy beta event profiles. Wire messages are never renamed or wrapped;
the optional OpenRealtime research trace stores the complete message in a
separate timing envelope.

```bash
go run ./cmd/openrealtime protocol inventory
go run ./cmd/openrealtime protocol validate \
  --profile realtime --direction client path/to/events.jsonl
```

See [docs/openai-realtime-compatibility.md](docs/openai-realtime-compatibility.md)
for precise coverage and the update policy.

Stable adapter authors should start with [docs/api-v1.md](docs/api-v1.md) and
the compiled [v1 reference example](examples/v1/reference/main.go).

## External live-provider benchmark

The no-Python `cmd/livebench` runner streams pinned Full-Duplex-Bench v1.5
audio at wall-clock speed through exact OpenAI, Gemini, or explicitly cascaded
Groq profiles. It produces aligned WAVs, hashes, secret-free traces, resumable
atomic manifests, offline rescoring, and paired bootstrap summaries.

The 2026-08-17–18 run completed the entire 498-trial official overlap
population plus an 80-trial paired sensitivity cell on Gemini 3.1 Flash Live,
with 578 unique outputs and no terminal failure. GPT-4o and Groq live cells are
marked unavailable with their observed reason; historical GPT-4o paper results
remain separate from local measurements. Pinned published FDB-v3 tool-use and
LiveKit endpointing tables add broader context without being represented as
local runs. Read the
[live-provider report](docs/research/live-provider-benchmark-2026-08.md) and
[external benchmark instructions](benchmarks/external/README.md).

τ-Voice is pinned as the primary M10 joint intelligence/interaction benchmark.
Its standard OpenAI adapter can now target the planned local endpoint, and Fish
Audio replaces ElevenLabs for local caller synthesis in a verified pinned
patch. The persistent gateway and live provider conformance are still pending,
so this repository does not claim a local τ-Voice score. See the
[τ-Voice evaluation plan](benchmarks/tau-voice/README.md).

## Principles

- Research claims must be measurable and falsifiable.
- The 200 ms microturn is an experimental reference point, not a universal constant.
- A trigger opens an opportunity; it does not require stateless ASR, LLM, and TTS reinference.
- Fast and slow models are compute phases of one agent and must consume one canonical trajectory.
- Tool capability awareness is shared even when execution authority is restricted to the slow continuation.
- Latency, interaction quality, intelligence, cost, and failure behavior must be evaluated together.
- Native speech-to-speech models are legitimate baselines and optional components, not opponents to be dismissed.
- Provider adapters exist to support experiments; model aggregation is outside the project scope.
- Reproducibility and inspectable timing traces are part of the product.

## License

Code is licensed under Apache-2.0, documentation under CC BY 4.0, and original
project fixtures under CC0-1.0. Third-party benchmark data is not redistributed.
See [LICENSES.md](LICENSES.md) and
[ADR-0002](docs/adr/0002-licensing-and-artifact-provenance.md).
