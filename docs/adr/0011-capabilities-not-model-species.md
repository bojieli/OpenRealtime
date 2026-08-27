# ADR-0011: Compose capabilities instead of model species

**Status:** accepted.

## Context

The original sidecar binding represented `omni` and `duplex` with a floor
owner plus a `FullDuplex` boolean. Runtime behavior then switched on those
fields as if turn generation and full interaction were mutually exclusive
model kinds.

They are not. A model may support explicit turn generation, concurrent input
and output, native endpointing, native interaction, external typed acts,
transcription, and text injection in any combination. A benchmark may also
select an engine controller over a model that retains a native interaction
head. Bundling availability and selection makes that controlled cell
unrepresentable and ensures every new combination needs another binding name
and another code branch.

Floor and interaction are different decisions. Floor establishes a boundary;
interaction chooses whether to listen, speak through, answer, interrupt, act
silently, continue, or yield. Standard speech-to-speech turn generation is not
proof of native interaction, and full duplex is not proof that the native
interaction policy is best for a task.

## Decision

Bindings report:

- an ownership vector for perception, fast cognition, slow cognition, action,
  interaction, and floor;
- an independent stack capability vector for audio input/output,
  transcription, turn generation, concurrent I/O, native floor, native
	interaction, typed interaction acts, and text injection.

Later ADRs refine the selected interaction boundary with independently
composable evidence capabilities (ADR-0013) and selected controller mechanisms
plus explicit arbitration (ADR-0014). Those are further capability axes, not
new cascade/Omni/duplex kinds.

Capabilities say what is available. Ownership says what is selected. Runtime
behavior depends on these fields, not on the preset name.

`cascade`, `omni`, `omni+text-policy`, `duplex`, and `upstream` remain named
presets because configuration and evidence need stable identities. The generic
`sidecarbinding.Spec` composes combinations without new species.

External interaction plans use sidecar protocol v2. Version 1 remains frozen
and default. A plan carries an enumerated act, policy identity, evidence
reference, floor semantics, deadline, confidence, and abstention; it never
carries prose to say.

For the first engine-controlled speech-to-speech cell, a policy-only streaming
recognizer supplies timely text evidence. Raw audio still reaches the speech
model directly and the model still emits speech directly. This cell is named
`omni+text-policy` because its controller is transcript-limited; it must not be
reported as audio-native interaction.

## Consequences

The immediate practical architecture is a speech-to-speech foreground plus an
engine interaction controller. It preserves prosody and audio information for
content generation while making the policy cheap, replaceable, auditable, and
independently measurable.

The highest-ceiling foreground remains an interaction-native multimodal model,
ideally a shared trunk with an explicit interaction head. It avoids the policy
ASR's information-loss boundary. That is a capability combination, not a new
runtime species, and it can be evaluated with native or external interaction
selected.

Neither foreground absorbs trajectory, tool authorization, audit, session
lifecycle, or asynchronous slow reasoning. Putting those into one foreground
model removes concurrency and authority boundaries without supplying an
independent slow process.

Named architecture rankings are invalid unless paired cells hold the actual
model, evidence source, floor selection, tool authority, and hardware fixed.
Comparing Qwen `omni+text-policy` to Moshi native interaction measures models
and architecture together.

F52 therefore uses a versioned architecture manifest rather than another
binding-name switch. The manifest records desired runnable and unavailable
cells; each measured task attests the post-handshake runtime ownership,
capabilities, policies, evidence/controller/arbitration/act boundary, component
identities, and tool authority. A comparison is labeled `architecture-only`
only for a controlled adjacent P/T, T/C, or T/N pair. Other complete
comparisons remain `system` evidence and retain their confounders.

## Alternatives considered

**Keep adding binding species.** Rejected because every hybrid requires code
changes and capability combinations remain untestable.

**Use only ownership.** Rejected because selecting an engine controller would
erase the fact that the model still supports native interaction, making status
and evidence unable to describe the control condition.

**Infer interaction from full duplex or floor ownership.** Rejected because
concurrent media, endpoint detection, and conversational policy answer three
different questions.

**Put all slow reasoning and authority inside the interaction-native model.**
Rejected. A single foreground model cannot be independently fast and slow at
the same instant, and model output is not an authorization or audit boundary.
