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

- Status: in progress

Next evidence must cover deterministic reference perception, cognition, and
speech adapters; conventional endpointing; stage/queue latency reconciliation;
repeated prerecorded distributions; and a timeline visualization.
