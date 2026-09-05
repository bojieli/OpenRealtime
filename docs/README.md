# OpenRealtime documentation

[← Project overview](../README.md)

Welcome. You do not need to read this repository in filename order.

If this is your first visit, start with the [quickstart](quickstart.md). It gets
you from a clone to a browser conversation, then points to the right guide for
local models, custom clients, or production deployment.

## Know what you are reading

The repository contains both product documentation and the engineering record
that produced it. Every document belongs to one of these groups:

| Type | Meaning |
| --- | --- |
| **Guide** | task-oriented instructions for the current release |
| **Reference** | a current API, protocol, architecture, or operational contract |
| **Evidence** | measurements and findings preserved for reproducibility; often chronological |
| **Proposal** | accepted or active design work that may describe a target beyond the shipped release |
| **Historical** | provenance retained for context; never the source of truth for current behavior |

When a proposal or historical note conflicts with a current reference, follow
the current reference. The long evidence logs are intentionally complete, but
they are not required reading to run or extend OpenRealtime.

## Start here

| Goal | Read |
| --- | --- |
| Have a conversation in a browser | [Quickstart](quickstart.md) |
| Run ASR, language, and speech models locally | [Local voice stack](guides/local-stack.md) |
| Choose hosted or local providers | [Provider catalog](providers.md) |
| Connect an existing Realtime client | [Transports](transports.md) and the [official SDK example](../examples/sdk-client/README.md) |
| Run the native macOS client | [macOS developer app](../macos/README.md) |
| Deploy a server | [Operations](operations.md) and [deployment](../deploy/README.md) |
| Contribute | [Contributing guide](../CONTRIBUTING.md) |

## Understand the system

Read these in order if you want the conceptual model:

1. [Architecture](architecture.md) — perception, cognition, action, the
   interaction control plane, and the shared session core.
2. [Bindings and capability composition](bindings/README.md) — how cascade,
   upstream, omni, duplex, and sidecars select model ownership.
3. [OpenRealtime Protocol v1](protocol/openrealtime-1.md) — the normative video,
   observation, and computer-use extension.
4. [Safety model](safety.md) — authority, confirmation, observed-content
   isolation, bounded effects, and audit.

The binding-specific references are:

- [Cascade](bindings/cascade.md): streaming ASR → language model → speech
  synthesis.
- [Upstream](bindings/upstream.md): a remote Realtime voice with OpenRealtime
  cognition behind it.
- [Omni](bindings/omni.md): speech-to-speech generation with engine policies.
- [Duplex](bindings/duplex.md): native concurrent input/output, floor, and
  interaction.

## Build and extend

| Surface | Status | Document |
| --- | --- | --- |
| Go component interfaces | Stable reference | [Component API v1](api-v1.md) |
| Realtime wire compatibility | Current reference | [OpenAI Realtime compatibility](openai-realtime-compatibility.md) |
| OpenRealtime extension | Stable reference | [Protocol v1](protocol/openrealtime-1.md) |
| External model process boundary | Stable base | [Sidecar protocol v1](sidecar-protocol-1.md) |
| Interaction acts over sidecars | Experimental additive reference | [Sidecar protocol v2](sidecar-protocol-2.md) |
| Direct images and bounded fast actions | Experimental additive reference | [Sidecar protocol v3](sidecar-protocol-3.md) |
| Typed element ports and attested readiness | Current reference for graph-native models | [Sidecar protocol v4](sidecar-protocol-4.md) |
| Graph launch and inspection | Current reference | [Graph-native assembly](graph-native-assembly.md) |
| Client and presentation composition | Shipped design | [Composable presentation](composable-presentation.md) |
| LiveKit adapter | Integration guide | [LiveKit integration](../integrations/livekit/README.md) |

Substantial architecture or protocol changes begin with an [architecture
decision record](adr/). Existing ADRs explain why the runtime is Go, why
transports sit above the protocol, why model sidecars are process boundaries,
and how interaction, authority, and versioned topology are composed.

## Operate and secure

- [Operations](operations.md) covers authentication, health, metrics, logs,
  resources, failure behavior, and support policy.
- [Deployment](../deploy/README.md) covers the distroless container and
  reproducible release binaries.
- [Safety](safety.md) defines model authority and the effect boundary.
- [Security policy](../SECURITY.md) explains supported versions and private
  vulnerability reporting.
- [Release validation](release-validation.md) maps release claims to their
  machine-readable gates.
- [Efficiency gates](efficiency.md) state the resource and latency budgets the
  release checks.

## Benchmarks and evidence

Start with [the benchmark harness](benchmarks.md) for commands, suite structure,
result interpretation, and adding a suite.

The documents below are evidence records. They preserve false starts, negative
results, corrections, and dated experiment IDs because removing that history
would make the conclusions impossible to audit. They are intentionally not
written as tutorials.

| Record | What it contains |
| --- | --- |
| [Measurement](measurement.md) | the full chronological measurement program and claim boundaries |
| [Response latency](latency.md) | waveform-based methodology, repeated measurements, and bottleneck findings |
| [Interaction findings](interaction-findings.md) | lessons from building and evaluating the interaction model |
| [The spoken boundary](spoken-boundary.md) | which of the agent's own words the user actually heard, and what reads it |
| [Sub-turn benchmark study](subturn-benchmark-study.md) | retained twelve-case Deepgram/Qwen/Gemini evidence and the focused FDB comparison |
| [Architecture experiments](architecture-experiments.md) | controlled placement/evidence/controller experiments and attestation gates |
| [Realtime computer-use graph](realtime-computer-use-graph.md) | the locked production graph and its sixteen-case validation suite |

Evidence tells you why a decision was made. Current reference tells you what
the released system does now.

## Proposals and design history

- [Composable real-time agent graph](composable-agent-graph.md) is the active
  typed-element graph proposal and implementation tracker. It includes target
  architecture that may be ahead of the release.
- [OpenRealtime v1 plan](openrealtime-v1-plan.md) is the historical product and
  architecture plan that led to v1.0. It is retained for provenance, not setup
  guidance.
- [Composable presentation](composable-presentation.md) records the accepted
  presentation architecture and its remaining release work.

## Specialized guides

- [Local voice and vision stack](guides/local-stack.md)
- [Reproducible meeting-assistant deployment](guides/meeting-assistant.md)
- [Native macOS developer app](../macos/README.md)
- [Provider deployment examples](providers.md#examples)
- [Official SDK client example](../examples/README.md)

If a page sent you here and you are still unsure where to go, the
[quickstart](quickstart.md) is the safest next click.
