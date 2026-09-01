# ADR-0012: Make evolving architectures first-class project objects

**Status:** accepted; amended by
[ADR-0015](0015-agent-topology-is-a-versioned-graph.md). Immutable revision,
lineage, fingerprint, and maturity semantics remain; production topology is
selected by an exact graph plan rather than derived from binding names or
structural flags.

## Context

ADR-0011 separated capabilities from model species, but one structural choice
still had two informal representations. `serve` assembled ownership and
capabilities from flags and named presets, while F52 experiment manifests
copied the intended architecture into benchmark JSON. A reviewer could compare
the copies, but neither was the authority for the other.

That is tolerable for one experiment and wrong for a project expected to carry
several architectures over time. A launch script is not an architecture: it
also contains endpoints, credentials, ports, and model choices. A benchmark
cell is not an architecture either: it additionally pins checkpoints,
adapters, policies, fixtures, and hardware. Editing either one cannot express
lineage, and reusing a name after changing its meaning makes old evidence
ambiguous.

## Decision

OpenRealtime owns a versioned architecture catalog in `architecture/`.

Each immutable definition records:

- an exact `id@revision`, family, maturity stage, summary, and revision note;
- explicit `derived_from` lineage;
- the six-column ownership vector;
- required capabilities, which are a lower bound rather than an exact type;
- interaction mode, broad evidence source, exact selected evidence-capability
  vector for current revisions, transport, protocol, typed/direct/no-act
  handoff, and any native-interaction suppression contract.

There is deliberately no mutable `latest` runtime reference. Changing an
existing idea creates a later revision; forking it creates another ID with a
lineage edge. The canonical JSON fingerprint prevents an entry from being
edited in place while retaining the same apparent revision.

`openrealtime serve -architecture id@revision` resolves the definition and
derives the component, sidecar, or upstream topology plus structural ownership,
capability, interaction, floor, and protocol selections. Deployment flags
still choose concrete providers, endpoints, credentials, model checkpoints,
voices, and operating limits. An explicitly supplied structural flag that
contradicts the definition is refused.

The selected definition is attached to the binding. After the foreground
handshake, startup validates the live ownership, required capability subset,
evidence source and selected capability vector, transport, protocol, act
handoff, and suppression contract.
Every session status then attests the architecture ID, revision, and definition
fingerprint without erasing the concrete binding name.

F52 cells are authored from three independent authorities:

1. the catalog definition supplies structure and lineage;
2. a normal post-handshake session supplies negotiated runtime status;
3. a pins file supplies immutable model, adapter, recognizer, and instruction
   identities that a live protocol cannot discover honestly.

The resulting cell embeds the complete definition and must match its live
identity. Experiment manifests assemble those reviewed cells. Results remain
subject to ordinary completeness, provenance, fixture, and pairing gates.

## Consequences

The project now contains multiple architectures rather than several names for
bindings. Several definitions may use the same sidecar runtime, and one
native-capable foreground may be selected under predicates, an external text
policy, a composed predicate/text controller with explicit arbitration, or its
native interaction head without a new binding species.

Capabilities are requirements, not discriminated-union tags. A foreground
with native interaction is allowed to realise an architecture that selects an
external policy; the unselected capability remains visible. This is necessary
for honest same-foreground P/T/C/N experiments.

Catalog maturity (`experimental`, `candidate`, `stable`, `retired`) describes
operational confidence, never measured superiority. Evidence promotes claims,
not definitions. A stable architecture may be slower than an experimental one;
that does not change either stage automatically.

An external catalog is supported for deployments extending the project. It is
parsed and validated by the same production package and retains exact revision
and fingerprint semantics.

Legacy `-binding` launches remain supported, but their session status carries
no architecture identity and architecture inspection refuses to treat them as
catalog evidence.

## Alternatives considered

**Keep architecture shell scripts.** Rejected because scripts mix structure
with deployment secrets and cannot provide typed lineage or live attestation.

**Use benchmark manifests as the catalog.** Rejected because a benchmark cell
contains treatment-specific model and fixture identities; runtime deployment
would then depend on a measurement artifact.

**Make capabilities equal exactly.** Rejected because an external-policy cell
over a native-capable foreground would have to lie that the native capability
disappeared, destroying the controlled comparison.

**Allow unversioned names.** Rejected because an old result would silently
change meaning whenever the catalog evolved.
