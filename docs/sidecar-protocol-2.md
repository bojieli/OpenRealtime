# The OpenRealtime sidecar protocol, version 2

**Status:** additive experimental contract.
**Base:** all framing and messages from [version 1](sidecar-protocol-1.md).

Version 2 adds a typed interaction-control seam. It does not change the v1
meaning of `respond` or `interrupt`, and v1 remains the engine default. A
deployment selects v2 explicitly with `sidecar.Config.ProtocolVersion`; the
sidecar must answer `ready` with version 2.

## Why a new version

An external interaction controller chooses *what kind of conversational act
happens*, not what the model says. Collapsing `answer`, `interrupt`, and
`speak-through` into `respond` discards who owned the floor. Collapsing
`stop-speaking` into `interrupt` discards why generation stopped. `listen`,
`keep-speaking`, and `act-silently` disappear completely in v1.

Those losses make the runtime work but make the architecture impossible to
conform, audit, or measure. Version 1 is frozen, so the richer contract has a
new version rather than silently changing what a v1 sidecar must accept.

## Capabilities

Version 2 adds two independent declarations:

| Capability | Meaning |
| --- | --- |
| `native_interaction` | the model can choose conversational acts from its own multimodal state |
| `interaction_acts` | the sidecar accepts typed plans from an external controller |

A sidecar may declare both. Capability describes what is available; the
binding's `ownership.interaction` says which provider is selected for this
session. Neither capability implies `full_duplex` or `native_vad`.

The reference Python base class provides the default mapping when a subclass
declares `interaction_acts`. Moshi declares `native_interaction` but does not
declare external act acceptance.

The v2 `hello` also carries `interaction_owner` and `floor_owner`, each one of
`engine`, `model`, or `remote`. These select providers from the capability set;
they do not add or remove capabilities. A sidecar declaring
`interaction_acts` must suppress its native interaction policy when the engine
is selected and apply the typed plans instead.

## `interaction_act`

The engine sends one JSON-only frame:

```json
{
  "type": "interaction_act",
  "act": "speak-through",
  "policy": "model:qwen3.6-35b-a3b",
  "evidence_ref": "omni_item_7:revision:12",
  "floor": "preserve",
  "deadline_ms": 1787821745123,
  "confidence": 0.84
}
```

The vocabulary and required floor semantics are fixed:

| Act | Floor | Sidecar behavior |
| --- | --- | --- |
| `listen` | `unchanged` | do not generate a turn |
| `speak-through` | `preserve` | generate while the other speaker retains the floor; offered only with concurrent I/O |
| `answer` | `take` | generate a turn after the floor became free |
| `interrupt` | `take` | generate a turn that intentionally takes the other speaker's floor |
| `act-silently` | `unchanged` | no model turn; engine slow cognition owns the action |
| `keep-speaking` | `unchanged` | continue current output |
| `stop-speaking` | `yield` | stop current output at the next safe point |

`policy` identifies the controller. `evidence_ref` identifies the live
revision, not a reconstruction made later. `deadline_ms` is when the moment
expires. `confidence` is in `[0,1]` where measured; `abstained` preserves an
explicit abstention. None of these fields contains prose for the model to say.

The sender and receiver reject a plan whose deadline has already passed. The
receiver must also reject an unknown act, a confidence outside `[0,1]`, or an
act/floor contradiction. `stop-speaking` is handled on the read path like v1
`interrupt`; typed `interrupt` also signals current generation immediately and
then queues the new answer. Queuing either interruption signal behind the
generation it should stop would make it too late to be an action.

## Compatibility

An engine-controlled turn model may still operate over v1. The engine
translates `answer`, `interrupt`, and `speak-through` to `respond`,
`stop-speaking` to `interrupt`, and keeps the remaining acts inside the engine.
This is a deployment fallback, not an equivalent measurement cell: the v1
wire has discarded policy identity, evidence, deadline, confidence, and floor
meaning. A model that also declares native interaction cannot be externally
selected through that ambiguous seam: it needs v2, `interaction_acts`, and the
v2 ownership selection so its native policy can be suppressed explicitly.

Conformance for a v2 sidecar verifies both ordinary turn generation and that a
typed `answer` produces a complete turn. The Python contract tests cover all
seven default act mappings.
