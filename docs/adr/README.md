# Architecture decisions

Architecture decision records (ADRs) explain a decision's context, chosen
approach, and consequences. They preserve the reasoning at the time of the
decision, including choices later revised.

For the current system, begin with [Architecture](../architecture.md). Read
ADR-0015 before treating an older voice topology as a requirement for every
graph: it supersedes the universal-topology portions of several earlier ADRs.
Each record's status and supersession notes define its scope.

| Decision | Topic |
| --- | --- |
| [ADR-0001](0001-go-production-engine.md) | Go production engine and protocol generator |
| [ADR-0002](0002-licensing-and-artifact-provenance.md) | Separate licenses and require artifact provenance |
| [ADR-0003](0003-canonical-trajectory-continuations.md) | Canonical trajectory with heterogeneous continuations |
| [ADR-0004](0004-safe-point-asynchronous-event-loop.md) | One safe-point event loop for asynchronous interaction |
| [ADR-0005](0005-data-plane-and-interaction-control-plane.md) | Separate the data plane from an interaction control plane |
| [ADR-0006](0006-two-cognition-boundaries.md) | Proposal-only fast cognition and silent slow cognition |
| [ADR-0007](0007-transports-above-the-protocol.md) | Transports are protocol clients, never second entrances |
| [ADR-0008](0008-sidecars-for-models-not-written-in-go.md) | Models not written in Go live behind a process boundary |
| [ADR-0009](0009-the-interaction-model.md) | One model decides what the agent does in an instant |
| [ADR-0010](0010-bounded-fast-computer-actions.md) | Bounded fast computer actions |
| [ADR-0011](0011-capabilities-not-model-species.md) | Compose capabilities instead of model species |
| [ADR-0012](0012-versioned-architecture-catalog.md) | Make evolving architectures first-class project objects |
| [ADR-0013](0013-interaction-evidence-is-a-capability-vector.md) | Attest interaction evidence as a capability vector |
| [ADR-0014](0014-controller-composition-requires-arbitration.md) | Controller composition requires explicit arbitration |
| [ADR-0015](0015-agent-topology-is-a-versioned-graph.md) | Agent topology is a versioned graph, not a binding invariant |
| [ADR-0016](0016-retain-speech-history-after-freshness-rejection.md) | Preserve speech history when model freshness expires |

For a substantial architecture or protocol proposal, add a record explaining
the problem, alternatives, selected design, compatibility impact, and remaining
questions. Keep detailed implementation progress in the relevant design
tracker, and user setup in a guide. See [Contributing](../../CONTRIBUTING.md).
