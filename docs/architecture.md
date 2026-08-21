# Architecture

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
│                    ── the only source of interaction quality   │
│                       for cascade and omni bindings ──         │
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
│ Bindings           declare which subsystems the model owns     │
│                    cascade │ omni │ duplex │ upstream          │
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

"Control plane" is a structural claim, not a ranking. Interaction is the most
important subsystem here. A full-duplex model has turn-taking trained into its
weights; a cascade and an Omni model have none whatsoever, so for two of the
four bindings every bit of responsiveness the system exhibits is manufactured
in `interaction` and nowhere else.

Each row is a named policy with an interface and a shipped default, because a
policy that cannot be swapped cannot be measured:

| Policy | Decides | Default |
| --- | --- | --- |
| Trigger | when a decision opportunity opens | fixed 200 ms cadence |
| Preparation | whether to speculatively pre-start before the endpoint | continuous |
| Rollout | when each cognition provider fires; whether slow supersedes | fast then slow |
| Floor | when the user has finished; who holds the turn | engine, 500 ms silence |
| Barge-in | whether user speech over agent output cancels it | immediate |
| Commitment | how much to emit before certainty | complete safe points only |
| Repair | what to do when a commitment proves wrong | audible correction |
| Backchannel | whether to say "mm-hm" while the user speaks | off |
| Turn projection | anticipating the end of a turn before silence confirms it | silence only |
| Deferral | whether committed work may be acted on now | by duplex state |

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

## The two cognition boundaries

> **The fast provider cannot call tools. The slow provider cannot speak.**

Both are properties of the provider descriptor, checked once at construction
and enforced where output commits. A fast provider's emitted call becomes a
`tool_proposal` — structured working state with no execution authority — and
the dispatcher re-checks the trajectory before any effect, so a proposal cannot
become an action however it is routed.

The second rule costs a short extra hop and buys three things: fast is always
the last writer before audio, so slow can no longer contradict something
already said; fast can condense a long written answer into something worth
listening to, which is a different job from producing it; and the division of
labour is legible — one model owns what the user hears, one owns what the
system does.

## Bindings declare ownership

A binding is not a pipeline. It is a declaration of which subsystems the model
owns.

| Binding | Perception | Fast | Slow | Action | Floor |
| --- | --- | --- | --- | --- | --- |
| `cascade` | engine | engine | engine | engine | engine |
| `omni` | model | model | **engine** | model | engine |
| `duplex` | model | model | **engine** | model | model |
| `upstream` | remote | remote | **engine** | remote | remote or engine |

The slow column never varies. No binding delegates slow cognition, because no
foreground model provides it — and supplying it over a shared trajectory is
what this project adds to whatever stack it is given.

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
and the sidecar protocol are what third parties write against. They are
versioned from v1.0: a binding written against v1.0 keeps working.
