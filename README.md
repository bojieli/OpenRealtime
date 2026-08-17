# OpenRealtime

**Microturn Realtime Intelligence Engine**

OpenRealtime is an open research project investigating a specific question:

> Can an incremental, modular speech system achieve the perceived responsiveness and interactive behavior of native realtime speech models when perception, reasoning, and speech are scheduled at conversational cadence?

The project is not intended to be another configurable ASR–LLM–TTS wrapper. Its primary outputs are a falsifiable research program, a reproducible benchmark suite, an instrumented reference engine, and evidence about when microturn scheduling works—and when it does not.

The working idea is to let a system listen, revise its understanding, prepare responses, and manage speech continuously. Fast foreground behavior and slower background reasoning are separated, while speculation, cancellation, repair, barge-in, and full-duplex interaction are treated as first-class engineering problems.

## Status

M0 reproducibility through M3 duplex speech safety are complete; M4 fast/slow
cognition is next.
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

Both scripts create isolated build artifacts, validate every emitted message
against the official protocol schemas, checks trace causality, compares the
result byte-for-byte with the checked-in golden files, and runs race, test,
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
