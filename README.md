# OpenRealtime

**Microturn Realtime Intelligence Engine**

OpenRealtime is an open research project investigating a specific question:

> Can an incremental, modular speech system achieve the perceived responsiveness and interactive behavior of native realtime speech models when perception, reasoning, and speech are scheduled at conversational cadence?

The project is not intended to be another configurable ASR–LLM–TTS wrapper. Its primary outputs are a falsifiable research program, a reproducible benchmark suite, an instrumented reference engine, and evidence about when microturn scheduling works—and when it does not.

The working idea is to let a system listen, revise its understanding, prepare responses, and manage speech continuously. Fast foreground behavior and slower background reasoning are separated, while speculation, cancellation, repair, barge-in, and full-duplex interaction are treated as first-class engineering problems.

## Status

M0 through M7 are complete. The stable component API is v1.0.0 and the complete
OpenAI protocol/provider conformance suite passes.
The repository contains a generated conformance layer for every event in the
pinned OpenAI Realtime OpenAPI specification, deterministic 24 kHz PCM replay, an original
redistributable audio fixture, and causal trace validation. There is no Python
runtime or build dependency. M1 adds production-shaped component contracts, a
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
symbolic latency/quality/compute frontier.

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

## Start here

Read [PLAN.md](PLAN.md) for the complete research questions, architecture, experimental design, milestones, and contribution roadmap.

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

The 2026-08-17 run completed 80/80 Gemini 3.1 Flash Live trials. GPT-4o and
Groq live cells are marked unavailable with their observed reason; historical
GPT-4o paper results remain separate from local measurements. Read the
[live-provider report](docs/research/live-provider-benchmark-2026-08.md) and
[external benchmark instructions](benchmarks/external/README.md).

## Principles

- Research claims must be measurable and falsifiable.
- The 200 ms microturn is an experimental reference point, not a universal constant.
- Latency, interaction quality, intelligence, cost, and failure behavior must be evaluated together.
- Native speech-to-speech models are legitimate baselines and optional components, not opponents to be dismissed.
- Provider adapters exist to support experiments; model aggregation is outside the project scope.
- Reproducibility and inspectable timing traces are part of the product.

## License

Code is licensed under Apache-2.0, documentation under CC BY 4.0, and original
project fixtures under CC0-1.0. Third-party benchmark data is not redistributed.
See [LICENSES.md](LICENSES.md) and
[ADR-0002](docs/adr/0002-licensing-and-artifact-provenance.md).
