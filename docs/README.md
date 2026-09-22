# Documentation

[Project overview](../README.md) · [Demo gallery](demos.md) · [Quickstart](quickstart.md)

Start with a browser conversation, then choose a guide for the task you want to
complete. You do not need to read the architecture or research records first.
Commands assume the repository root unless a guide says otherwise. Examples
using `openrealtime` assume the built binary is on `PATH`; use `./openrealtime`
when running it from the repository root.

## Get started

| Task | Guide |
| --- | --- |
| Run a conversation without a GPU | [Quickstart](quickstart.md) |
| Play the twelve interaction scenarios | [Scenario gallery](demos.md) · [The default pipeline](room.md#the-default-pipeline) |
| Run local speech and language models | [Local stack](guides/local-stack.md) |
| Choose providers and credentials | [Providers](providers.md) |
| Connect an application and execute a tool | [SDK walkthrough](../examples/sdk-client/README.md) |
| Use the native developer app | [macOS client](../macos/README.md) |
| Reproduce the meeting evaluation setup | [Meeting assistant](guides/meeting-assistant.md) |

## Understand and extend

Read [Core concepts](concepts.md) for terminology, then
[Architecture](architecture.md) for how a session works and where to find the
implementation.

| Integration surface | Reference |
| --- | --- |
| WebSocket, WebRTC, and LiveKit clients | [Transports](transports.md) · [LiveKit integration](../integrations/livekit/README.md) |
| Supported Realtime API features and limitations | [Compatibility](openai-realtime-compatibility.md) |
| Video, observations, and computer-use events | [OpenRealtime Protocol v1](protocol/openrealtime-1.md) |
| GPT-Live: integration architecture and how it differs from Realtime | [GPT-Live on OpenRealtime](gptlive-integration.md) |
| Go provider interfaces | [Stable component API v1](api-v1.md) |
| External model processes | [Sidecar guide](sidecars.md) · Protocols [v1](sidecar-protocol-1.md), [v2](sidecar-protocol-2.md), [v3](sidecar-protocol-3.md), [v4](sidecar-protocol-4.md) |
| Existing voice adapters | [Bindings](bindings/README.md): [cascade](bindings/cascade.md), [upstream](bindings/upstream.md), [omni](bindings/omni.md), [duplex](bindings/duplex.md) |
| Typed graph preparation and dependencies | [Graph assembly](graph-native-assembly.md) |
| Computer-use application graph | [Realtime computer use](realtime-computer-use-graph.md) |
| Action authority and confirmation | [Safety model](safety.md) |

## Deploy and contribute

| Task | Guide |
| --- | --- |
| Build a container or release binary | [Deployment](../deploy/README.md) |
| Configure authentication, health, metrics, and resource limits | [Operations](operations.md) |
| Understand required checks and optional diagnostics | [Release validation](release-validation.md) · [Efficiency](efficiency.md) |
| Submit a change | [Contributing](../CONTRIBUTING.md) |
| Report a vulnerability | [Security policy](../SECURITY.md) |

## Research and design

The [research overview](research.md) introduces the questions, evaluation
methods, and evidence. Use the [benchmark guide](benchmarks.md) to run a suite
or investigate a failure.

| Record | Read it for |
| --- | --- |
| [Streaming and full-duplex survey and plan](full-duplex-streaming-plan.md) | September 2026 model/API survey, composable streaming contracts, and the RTX Pro evaluation roadmap |
| [Streaming and full-duplex integration: results](full-duplex-results.md) | What the plan's first execution pass built, measured on the RTX Pro, and could not run, with deployment profiles |
| [Expanded open-model survey](open-duplex-models-survey.md) | native duplex agents, turn prediction, speaker/overlap perception, audio understanding, and additional integration priorities |
| [Interaction findings](interaction-findings.md) | lessons about model inputs and timing decisions |
| [Response latency](latency.md) | waveform timing and measured bottlenecks |
| [The spoken boundary](spoken-boundary.md) | interruption and the words a listener actually heard |
| [Sub-turn study](subturn-benchmark-study.md) | the retained 12-case checkpoint and subsequent scoring corrections |
| [Architecture experiments](architecture-experiments.md) | controlled comparisons of policy placement and evidence |
| [Measurement record](measurement.md) | chronological experiments, including failures and revised conclusions |
| [Graph design](composable-agent-graph.md) | accepted design and implementation tracker |
| [Presentation design](composable-presentation.md) | client composition contracts and implementation tracker |
| [Architecture decisions](adr/) | the rationale and tradeoffs behind individual decisions |
| [Original v1 plan](openrealtime-v1-plan.md) | historical product and architecture intent |

## How to read status

**Guides** explain tasks. **References** specify current contracts, including
limits. **Evidence records** describe particular experiments and may be
superseded by later entries. **Design trackers** include both implemented and
unfinished work. **Historical plans and ADRs** preserve decisions in context.

A design target or a historical measurement is not a statement that a feature
is available in your build. Check the current reference, the binary's
`version` output, and the selected deployment's capabilities. Component and
protocol versions are independent of the binary version; see
[version boundaries](concepts.md#three-different-version-boundaries).
