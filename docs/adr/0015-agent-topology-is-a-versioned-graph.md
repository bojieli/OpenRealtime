# ADR-0015: Agent topology is a versioned graph, not a binding invariant

**Status:** accepted.

**Date:** 2026-09-01

**Supersedes:** the universal-topology portions of ADR-0004, ADR-0005,
ADR-0006, ADR-0009, and ADR-0010. It amends the representation selected by
ADR-0011 through ADR-0014 without weakening their capability, evidence,
identity, or arbitration requirements.

## Context

The early runtime needed one concrete architecture before it had a general
composition language. Its ADRs therefore mixed two different kinds of
decision:

1. safety and reproducibility invariants that every valid agent must preserve;
2. one useful voice-agent topology that happened to preserve them.

That topology used one safe-point event loop, a perception/cognition/action
data plane plus an interaction control plane, fast and slow cognition with
fixed speech and tool roles, and a selected predicate, model, native, or
composed interaction controller. Those choices produced real evidence and
remain useful reference architectures. They are not requirements of a silent
computer-use agent, a text-and-files agent, a speech-to-speech foreground, or
a graph in which deliberative output speaks directly.

Treating the choices as kernel invariants has concrete costs. A new composition
requires a binding species or a flag branch; ownership booleans stand in for
actual connections; interaction is said to carry no data even though acts,
interrupts, deadlines, and acknowledgements cross typed boundaries; and the
architecture catalog derives topology rather than identifying the exact graph
that actually runs.

The accepted composable-agent design now provides the missing authority: an
immutable, descriptor-checked Graph IR compiled from `.ortg` or normalized
artifacts, with separate values, deployment, secrets, and empirical evidence.

## Decision

### The graph plan is the topology authority

Production topology is the exact immutable graph plan: its nodes, typed ports,
edges, triggers, channel policies, effects, resolutions, configuration
identities, and fingerprint. A launch profile selects that plan and exact
runtime implementations. The generic runtime mounts the selected contracts; it
does not infer an agent shape from `cascade`, `omni`, `duplex`, `fast`, `slow`,
an ownership vector, or a structural serve flag.

Named architectures remain valuable discovery, lineage, rollout, and benchmark
identities. Their production form is a revisioned graph/config/profile entry,
not a second topology authority. Historical architecture definitions and
results keep their original fingerprints and interpretation.

### Safety invariants remain independent of topology

This ADR does not turn correctness into optional wiring. The following remain
mandatory contracts, enforced by element descriptors, graph validation,
runtime admission, and live evidence as appropriate:

- canonical trajectory identity, compare-and-append, and causal provenance;
- immutable invocation prefixes and rejection of stale publications;
- typed interruption, cancellation, timeout, failure, and terminal behavior;
- bounded channels, explicit loss, and backpressure rather than silent semantic
  mutation or overwrite;
- exact capability and selected-evidence attestation;
- explicit arbitration wherever multiple producers or controllers can affect
  one decision;
- separate proposal, authority, confirmation, target, ledger, dispatch, audit,
  and result-commit boundaries for external effects;
- immutable graph, configuration, implementation, profile, and evidence
  identities.

A graph may omit a mechanism only when its exported contract and validation
profile permit that omission. It may not bypass an applicable authority or
falsify missing cancellation, loss, or evidence as supported behavior.

### Earlier architecture decisions become explicit reference choices

| Earlier record | Decision that remains | Choice represented by a graph/profile |
| --- | --- | --- |
| ADR-0004 | Typed asynchronous events, immutable source versions, safe-point publication, stale-result refusal, bounded ingress, and explicit interruption | One global event-loop owner and its exact fast/slow scheduling rules |
| ADR-0005 | Named, replaceable, measurable policies and separation of content from authority | A mandatory perception/cognition/action plus interaction-plane partition, including the claim that interaction carries no typed data |
| ADR-0006 and ADR-0010 | Model output is not external-effect authority; confirmation, fencing, ledger, and audit remain independent | Proposal-only fast cognition, silent slow cognition, and the `fast-computer-use` binding/flag lane |
| ADR-0009 and ADR-0014 | Enumerated interaction acts, inertial failure behavior, selected evidence, and explicit controller arbitration | Replacing predicates with one interaction model, or selecting a particular predicate/model/native controller wiring |
| ADR-0011 and ADR-0013 | Capabilities are not model species; availability and selected evidence must be reported honestly and exactly | Ownership and evidence vectors as topology authorities rather than compatibility/inspection projections of typed graph contracts |
| ADR-0012 | Immutable revisions, lineage, fingerprints, maturity, and exact live/benchmark attestation | An architecture definition deriving binding topology from names and structural flags instead of cataloging the selected graph plan |

This distinction is intentionally asymmetric. A reference graph can select one
event loop, a silent deliberative lane, or a single interaction model. The
kernel cannot require those selections merely because a shipped reference uses
them.

### Capabilities and evidence are derived from exact contracts

Capability availability comes from element descriptors and live resolution.
Selected observation/action spaces and interaction evidence come from exact
ports, adapters, controller/arbitration elements, and launch-profile
selections. Compatibility status may still project those facts into the legacy
ownership, stack-capability, and evidence-capability vectors while old protocol
and benchmark consumers remain, but those vectors cannot create or override
topology.

An opaque native-model boundary may truthfully expose a coarser capability
than a decomposed graph. It still has an exact typed boundary and runtime
identity; the system does not invent internal ports the provider cannot attest.

## Consequences

Fast/slow, foreground/background, native/external interaction, direct or
mediated speech, and silent or multimodal action can be rewired without kernel
branches when their element contracts are compatible. `Tee`, `Mux`, merge,
mixer, and arbiter elements make fan-out and multiple-writer semantics visible.

The architecture and binding compatibility surfaces cannot be deleted in the
same documentation change. They remain reference/evaluation inputs until the
corresponding graph-native production profiles, catalogs, and release evidence
are complete. Their eventual deletion is tracked separately; this ADR supplies
the decision boundary and does not claim that migration has already happened.

Old measurements remain interpretable. An ADR-0009 interaction-model result or
an ADR-0010 fast-computer-use result still names the topology it actually ran.
The project does not rewrite those results as generic graph evidence after the
fact.

The cost is that a valid graph and profile must carry more exact information
than a binding name or ownership vector. Authoring, catalog, inspection, and
benchmark tooling must all preserve the same immutable plan identity. That is
deliberate: one inspectable authority is preferable to several compact but
competing descriptions.

## Alternatives considered

**Keep the old ADRs as universal rules and add exceptions.** Rejected because
each new modality or controller arrangement would add another exception while
the runtime retained the original taxonomy.

**Discard all earlier decisions.** Rejected because canonical state, bounded
asynchrony, authority, exact evidence, and explicit arbitration are earned
invariants, not artifacts of the old topology.

**Treat architecture definitions and Graph IR as equal authorities.** Rejected
because drift between them would be unresolvable. A definition may select and
describe a graph revision; the prepared plan and live resolution attest what
runs.

**Infer missing adapters or authority paths automatically.** Rejected because
semantic adapters and enforcement paths change behavior. They must be explicit
nodes or profile-selected implementations with reviewable identities.
