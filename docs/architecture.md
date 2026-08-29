# Architecture

This document describes the currently shipped architecture. The
[composable agent graph proposal](composable-agent-graph.md) defines the
proposed target architecture and refactoring plan for general multimodal
real-time agents. It deliberately revisits some binding, ownership, mandatory
audio, and slow-cognition constraints described below; until that migration is
implemented, this document remains the authority for current behavior.

Four subsystems over one session core. Three of them move and transform data;
the fourth decides *when* the other three act.

```text
┌────────────────────────────────────────────────────────────────┐
│ Gateway            OpenRealtime wire, session lifecycle        │
├────────────────────────────────────────────────────────────────┤
│ INTERACTION        the decisions about *when*                  │
│   control plane    trigger · preparation · endpoint & floor ·  │
│                    barge-in · fast/slow rollout · backchannel · │
│                    turn projection · commitment · repair ·     │
│                    deferral                                    │
│                    ── selected from engine predicates, an      │
│                       external policy, or a native model head ─│
├──────────────────┬──────────────────┬──────────────────────────┤
│ Perception       │ Cognition        │ Action                   │
│                  │                  │                          │
│ gate → observe   │ providers ·      │ plan → pace → commit     │
│ → persistent     │ authority ·      │ → speech · tools ·       │
│ text             │ trajectory       │   computer use           │
│                  │ continuation     │                          │
│ world → log      │ log → log        │ log → world              │
├──────────────────┴──────────────────┴──────────────────────────┤
│ Session core       trajectory · eventloop · duplex state       │
│                    append-only log, safe points, atomic commit,│
│                    cancellation, irreversibility ledger        │
├────────────────────────────────────────────────────────────────┤
│ Bindings           compose ownership + available capabilities │
│                    named presets are benchmark identities      │
└────────────────────────────────────────────────────────────────┘
```

## Perception and action are duals

Perception turns a continuous external stream into discrete commitments by
**gating** — discarding what does not matter. Action turns discrete commitments
back into a continuous external stream by **pacing** — holding back what is not
yet safe to emit. The same boundary, crossed in opposite directions.

Cognition is not a boundary at all: it neither reads from nor writes to the
outside world, it reads the log and appends to the log. What the three have in
common is only that data flows through them, which is the definition of a data
plane.

## Interaction carries no data

Trigger cadence, speculative pre-start, floor ownership, barge-in,
commit-versus-cancel — these are decisions *about when*, made over evidence
from all three. Nothing flows through them. The borrowing from networking is
exact rather than metaphorical: the data plane moves things, the control plane
decides how.

"Control plane" is a structural claim, not a ranking. Interaction may be an
engine policy, a native model head, or an engine policy handing typed acts to a
model that is also capable of native interaction. Full duplex, native
endpointing, and native interaction often arrive together, but none implies
the others. Treating that bundle as one model species prevents the controlled
combinations the architecture exists to measure.

Each row is a named policy with an interface and a shipped default, because a
policy that cannot be swapped cannot be measured:

| Policy | Decides | Default |
| --- | --- | --- |
| Trigger | when a decision opportunity opens | fixed 200 ms cadence |
| Preparation | whether to speculatively pre-start before the endpoint | off (`-preparation continuous` to enable) |
| Rollout | when each cognition provider fires; whether slow runs at all | fast, then slow when fast asks |
| Floor | when the user has finished; who holds the turn | engine, 500 ms silence |
| Barge-in | whether user speech over agent output cancels it | immediate |
| Commitment | how much to emit before certainty | complete safe points only |
| Repair | what to do when a commitment proves wrong | audible correction |
| Backchannel | whether to say "mm-hm" while the user speaks | off |
| Turn projection | anticipating the end of a turn before silence confirms it | silence only |
| Deferral | whether committed work may be acted on now | by duplex state |
| Overlap | what user speech over agent output is | unclassified |

Each of these is consulted on the live path, and the ones that need a model
degrade to a rule when none is configured. Four of them are worth stating
concretely, because "named policy" and "policy that changes what happens" are
not the same claim:

- **Turn projection** reaches the conversation through the floor. When it
  projects an ending, the runtime closes the acoustic gate and the turn ends
  before silence confirms it. Only a *projected* endpoint does this; an
  ordinary one is the gate's, so the two are never counted together — a
  projection that was wrong cut the user off, and a late endpoint only cost
  latency.
- **Backchannel** decides off the audio path, because a model call that made
  the recogniser wait would trade what makes a system feel alive for what makes
  it feel slow. A continuer it chooses is spoken, carries no assistant item, and
  does not trigger barge-in against itself.
- **Preparation** starts a continuation while the user is still talking,
  against what perception has heard so far. Nothing it produces is committed,
  spoken, or dispatched: the work is adopted at the endpoint only if the
  canonical observation says the same thing, and discarded otherwise. That is
  what makes it a latency policy — being wrong costs tokens and nothing else.
- **Repair** is reached when a later observation invalidates something the user
  already heard. The obligation is recorded in the ledger, raised into the
  trajectory at the next safe point, put to the slow provider as an
  instruction, and discharged against the correction it produces.

Two policies can contradict each other, and the runtime refuses the pair rather
than picking one: `stable-partial` observation with a deferral policy that
waits for the user to stop speaking would hold every partial until the endpoint
and buy nothing. A runtime that silently overrode one of them would report a
configuration it was not running.

## The session core

**One trajectory.** An append-only log of typed items: observations,
reasoning, assistant content, non-executable tool proposals, executable tool
calls, results, placeholders, and visibility transitions. Every provider
compiles a causally valid prefix of it, and output commits by
compare-and-append against the version it was computed from.

**One event loop**, with one invariant:

> Commit is unconditional; acting is conditional; every deferral has a
> wake-up. An event that has been committed but not yet acted upon must
> eventually cause a run. Nothing committed is ever silently dropped.

The third row of that rule is why it is an invariant rather than a table. An
agent that finishes speaking, with the user silent and a tool result sitting
unacted, has no natural trigger. A rule written case by case would have covered
the obvious cases and lost that one.

**One duplex state**, owning two facts: is the user speaking, and is the agent
speaking. Agent-speaking means *audio is reaching the user right now* — not
that a continuation is generating and not that speech was enqueued. Synthesised
audio takes real time to play, and a design that conflates the decision to
speak with the user hearing it gets every overlap decision wrong.

## Cognition roles and boundaries

> **Fast is proposal-only by default and can execute only an explicitly
> filtered bounded tool lane. Slow cannot speak.**

Both are properties of the provider descriptor, checked once at construction
and enforced where output commits. In the default arrangement a fast
provider's emitted call becomes a `tool_proposal` — structured working state
with no execution authority — and the dispatcher re-checks the trajectory
before any effect, so a proposal cannot become an action however it is routed.

The recommended voice+vision profile adds a separate silent visual reflex role.
It is not a second runtime and does not replace Fast: all roles read and commit
through the same trajectory and action boundary. The reflex receives a compact
projection—the latest user task, the newest image observation per source, and
only the exact direct action schemas selected by the live filter—and returns
one `act`, `wait`, or `abstain` decision under a hard timeout. It cannot speak,
iterate screenshots, sleep, or see arbitrary tools. Timeout and malformed
output fall through to the ordinary rollout; the default voice profile does not
instantiate it at all.

The older `-fast-computer-use` flag remains a compatible explicit exception
for deployments that intentionally use the voice model for visual reaction.
Both bounded lanes are narrower than granting a model tools in general:

- cognition attaches filtered tools to the reflex only on a committed visual
  observation; the compatible voice-model lane may also prepare work that can
  be adopted only by the matching observation. Neither opens the lane for a
  holding line, interjection, or background-result narration;
- only exact direct actions from the standard `computer.*` vocabulary qualify,
  not a name that merely shares the prefix; `computer.screenshot` and
  `computer.wait` remain slow-only observation control;
- server-owned actions must have an in-process dispatcher; client-owned actions
  must declare a non-empty target and `confirm: never`;
- undeclared and filtered calls are downgraded to proposals even though the
  provider descriptor has execution authority;
- local and client-executed calls both cross the same confirmation, ledger,
  trajectory-authority, and audit boundary.

That lane handles a simple click or keypress while its cue is still current.
Arbitrary business tools, confirmation-requiring client actions, ambiguity,
planning, and dependent multi-step work remain slow responsibilities.

The second rule is what makes the division of labour legible: one model owns
what the user hears, one owns what the system does. Slow's output is not an
assistant turn at all. It is recorded as background state and projected to
every provider as such, so nothing can mistake a written result for something
the user was told — and the next fast turn answers *from* it rather than
reciting it. Fast is therefore always the last writer before audio, and slow
cannot contradict, or repeat, something already said.

An observation runs slow under the reference fast+slow rollout. This is
deliberate: making slow conditional on a small fast model's marker lost
tool-using turns in measurement. A fast action result enters the shared
trajectory before slow continues, so the reasoner plans from what already
happened instead of repeating it. `fast-only` and `endpointed-slow-only` remain
separate rollout controls for measuring what each lane contributes.

## Bindings compose ownership and capabilities

A binding is not a pipeline or a model taxonomy. It reports two independent
things: the selected owner for each subsystem, and the capabilities available
in the composed stack whether or not they are selected in this cell. That
distinction permits a native-interaction, concurrent model to run under an
engine policy without pretending those native capabilities ceased to exist.
Runtime behavior keys off these fields, never off the string `omni` or
`duplex`.

| Preset | Perception | Fast | Slow | Action | Interaction | Floor |
| --- | --- | --- | --- | --- | --- | --- |
| `cascade` | engine | engine | engine | engine | engine | engine |
| `omni` | model | model | **engine** | model | engine | engine |
| `omni+text-policy` | model | model | **engine** | model | engine | engine |
| `duplex` | model | model | **engine** | model | model | model |
| `upstream` | remote | remote | **engine** | remote | remote | remote or engine |

The available capability vector is orthogonal: audio input/output,
transcription, explicit turn generation, concurrent input/output, native
floor, native interaction, typed interaction-act acceptance, and text
injection. `sidecarbinding.Spec` is the generic composition surface. The named
constructors are compatibility presets and evidence labels over it.

The slow column never varies. No binding delegates slow cognition, because no
foreground model provides it — and supplying it over a shared trajectory is
what this project adds to whatever stack it is given.

## Architecture definitions evolve above bindings

The repository-owned `architecture` package is the structural authority above
the adapter layer. A definition is an immutable `id@revision` containing the
ownership vector, required capability lower bound, interaction evidence,
selected controllers and arbitration, handoff boundary, maturity stage, and
explicit lineage. For example,
`omni.external-policy@2` and `omni.native-policy@1` require the same available
capabilities and differ in the selected interaction owner. Current exact-
evidence revisions `omni.external-policy@3` and `omni.native-policy@2` preserve
that relationship while replacing the old coarse evidence label. Current
controller-attested revisions `omni.external-policy@4` and
`omni.native-policy@3` additionally prove which selector is in force.

Controller composition is another independent axis. `cascade.text-policy@3`
selects one external act policy for endpoint, overlap, semantic, visual, quiet,
and silent-tool decisions. `cascade.composed-policy@1` selects the same text
policy plus narrow predicates under `predicate-floor` arbitration: predicates
retain endpoint and overlap decisions while the text policy owns the remaining
acts. Both use the same component topology. This is the controlled T/C question
represented directly in the project, not a new binding species.

Bindings still do the work. A definition derives one of three provider
topologies—components, sidecar, or upstream—and several definitions can use
the same topology. Deployment configuration supplies exact models and
endpoints. After the live provider handshake, the architecture wrapper refuses
missing requirements or a different evidence/controller/arbitration/protocol/
handoff boundary and attests the definition fingerprint in session status.

This gives the layers distinct responsibilities:

| Layer | Authority |
| --- | --- |
| architecture catalog | structural selection and evolution lineage |
| deployment configuration | model endpoints, credentials, voices, operating limits |
| binding | concrete adapter and session machinery |
| benchmark cell | immutable deployment pins, policies, fixtures, and measured evidence |

Adding a capability combination therefore does not require another binding
package. Changing an architecture does require a new revision, so old runtime
and benchmark artifacts never silently acquire a new meaning.

Interaction evidence is itself a capability vector, independent of the voice
stack vector. It records transcript, acoustic activity, silence clock,
conversation and tool state, speaker identity, addressing, narrated vision,
direct pixels, and native model state separately. It is an exact selection,
not a lower bound: extra live evidence is a different architecture and startup
refuses it. This is what caught the otherwise invisible difference between a
text policy launched with and without direct vision.

## Where things live

| Package | Owns |
| --- | --- |
| `trajectory` | the canonical log and its invariants |
| `eventloop` | ingress, safe points, deferral, wake-ups, the parallel branch |
| `session` | duplex state, bounded media store |
| `perception` | observers, gates, narrators |
| `continuation` | the streamed provider contract and its commit transaction |
| `cognition` | the provider arrangement and the two boundaries |
| `interaction` | every policy about *when* |
| `action` | the irreversibility ledger, speech pacing, tool dispatch |
| `binding` | the seam, and the four implementations under it |
| `gateway` | the protocol server: a renderer, deciding nothing |
| `protocol/openai` | the pinned base wire registry and validator |
| `protocol/openrealtime` | the extension |
| `sidecar` | the process boundary for models not written in Go |
| `computeruse` | the action vocabulary, targets, and dispatch |
| `transport/webrtc` | a protocol client that terminates media |

## Extension points

`Binding`, `Observer`, `Narrator`, `Vision`, `Decider`, `computeruse.Surface`,
and the sidecar protocol are what third parties write against. Sidecar protocol
v1 remains frozen and is still the default. Typed interaction plans use the
explicitly selected v2 contract.
