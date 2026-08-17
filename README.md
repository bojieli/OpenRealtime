# OpenRealtime

**Microturn Realtime Intelligence Engine**

OpenRealtime is an open research project investigating a specific question:

> Can an incremental, modular speech system achieve the perceived responsiveness and interactive behavior of native realtime speech models when perception, reasoning, and speech are scheduled at conversational cadence?

The project is not intended to be another configurable ASR–LLM–TTS wrapper. Its primary outputs are a falsifiable research program, a reproducible benchmark suite, an instrumented reference engine, and evidence about when microturn scheduling works—and when it does not.

The working idea is to let a system listen, revise its understanding, prepare responses, and manage speech continuously. Fast foreground behavior and slower background reasoning are separated, while speculation, cancellation, repair, barge-in, and full-duplex interaction are treated as first-class engineering problems.

## Status

Planning phase. No implementation or performance claims exist yet.

## Start here

Read [PLAN.md](PLAN.md) for the complete research questions, architecture, experimental design, milestones, and contribution roadmap.

## Principles

- Research claims must be measurable and falsifiable.
- The 200 ms microturn is an experimental reference point, not a universal constant.
- Latency, interaction quality, intelligence, cost, and failure behavior must be evaluated together.
- Native speech-to-speech models are legitimate baselines and optional components, not opponents to be dismissed.
- Provider adapters exist to support experiments; model aggregation is outside the project scope.
- Reproducibility and inspectable timing traces are part of the product.

## License

The license will be selected before the first public code release. The project plan recommends a permissive, patent-aware open-source license and records the decision as an explicit milestone.
