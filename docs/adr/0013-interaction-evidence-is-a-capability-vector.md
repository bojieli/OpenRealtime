# ADR-0013: Attest interaction evidence as a capability vector

**Status:** accepted; amended by
[ADR-0015](0015-agent-topology-is-a-versioned-graph.md). Exact selected
evidence remains mandatory, while the vector is a compatibility/inspection
projection of graph ports and selected opaque capability boundaries rather
than a topology authority.

## Context

ADR-0011 made foreground behavior composable through ownership and stack
capabilities. ADR-0012 made evolving structural selections immutable catalog
objects. Their interaction boundary still represented evidence with one broad
string: `acoustic-predicates`, `transcript`, `native-multimodal`, or
`remote-multimodal`.

That string is a representation class, not a complete boundary. A transcript
policy may also receive acoustic activity, a silence clock, standing
instructions, conversation history, unresolved tools, speaker identity,
narrated vision, or pixels. A native interaction head may use opaque audio and
latent state that no external recognizer exposes. Those channels vary
independently.

The distinction became operational during an F52 diagnostic. One text-policy
cell was launched with direct vision while its immutable definition claimed
transcript-only evidence. Runtime status reported only `transcript`, so cell
authoring and per-task validation could not discover the confound. The scores
were mechanically valid but could not support an architecture claim.

Treating each combination as another cascade, Omni, or duplex species would
repeat the original taxonomy error at a lower layer. Treating extra evidence
as harmless would make controlled comparisons impossible: pixels can change
the decision even when ownership, foreground, and policy model remain equal.

## Decision

`binding.InteractionStatus` reports an exact selected
`InteractionEvidenceCapabilities` vector containing:

- transcript;
- acoustic activity/timing;
- silence clock;
- conversation and standing-instruction state;
- tool state;
- speaker identity;
- addressing;
- visual description;
- direct visual input;
- native model/audio/latent state.

These booleans describe what the active interaction owner receives, not every
channel the stack could theoretically provide. Stack capabilities remain a
lower bound with harmless extras; interaction-evidence capabilities are an
exact selection because an extra input changes the architecture treatment.

Current catalog revisions carry the exact vector in their interaction
boundary. Revisions written before this decision retain a missing vector so
their canonical fingerprints remain immutable. They remain resolvable and
runnable for historical inspection, but new F52 cells reject them. A new
experiment must use an exact-evidence revision.

The component runtime derives its live vector from composed components:

- predicates select acoustic activity and the silence clock;
- a text policy additionally selects transcript, conversation state, and tool
  state;
- speaker identity is selected only when an embedder is configured;
- visual description is selected only when a narrator is on the policy path;
- direct visual input is selected only when the decider is configured to
  receive retained images;
- addressing remains false until a component provides real addressing
  evidence.

The sidecar text-policy runtime reports transcript, acoustic activity, silence
clock, conversation state, and tool state. Native and upstream interaction
report native model state. They do not infer selected native evidence merely
from the foreground having a `native_interaction` capability.

Architecture startup, benchmark cell authoring, and per-task result validation
all require exact equality with the definition. Thus an undeclared direct
visual channel closes the live runtime rather than becoming mislabeled
evidence.

Speaker identity has an additional identity boundary. Live status names its
adapter and revision; experiment pins name the immutable embedding model and
adapter. Speaker identity and addressing remain separate: recognizing a voice
does not establish whether its utterance was directed at the agent.

The vector is ordinary data with union, missing, and satisfaction operations.
Supported combinations can be expressed in an external catalog without
adding a binding package or architecture-specific runtime branch. A changed
selection still receives a new immutable definition ID or revision.

## Consequences

The invalid visual P/T diagnostic cannot recur silently. It remains
inspectable and non-reportable; no ranking is recovered from it after the
fact.

The catalog now has distinct voice-only, narrated-visual, direct-visual, and
speaker-aware text-policy definitions. They share the same component topology
and runtime. These are evolving compositions, not model species.

An evidence producer becomes part of reproducibility. Transcript evidence pins
its recognizer, speaker evidence pins its embedder, and visual-description
evidence pins its narrator. Live status independently names each adapter path.
The observer and model identities remain deployment/benchmark facts rather
than being inferred from an architecture name.

Native model state is necessarily coarser than externally typed channels. It
states honestly that an opaque in-model path exists; it does not fabricate
claims about which latent cues a provider actually learned to use. Controlled
same-foreground experiments remain necessary.

The architectural question is no longer “cascade, Omni, or duplex?” It is:
which owner receives which evidence capabilities, through which act boundary,
while which foreground capabilities remain available?
