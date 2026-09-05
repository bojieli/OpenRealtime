# OpenRealtime v1.0: Product, Architecture, Protocol, and Measurement Plan

> [!WARNING]
> **Historical design plan.** This document predates the v1.0 release. Parts of
> it were implemented, parts evolved, and parts were superseded; unchecked or
> future-tense text here is not a statement about current behavior. Use the
> [quickstart](quickstart.md), [current architecture](architecture.md),
> [Protocol v1](protocol/openrealtime-1.md), and [stable component API](api-v1.md)
> as the released sources of truth.

> Historical note: the mutually exclusive cascade/Omni/duplex taxonomy in this
> proposal is superseded by [ADR-0011](adr/0011-capabilities-not-model-species.md).
> Current bindings compose an ownership vector and independent capabilities;
> the old names remain presets and evidence labels only.

> Two documents this plan cites were never carried into the repository:
> `docs/api-v2-proposal.md` (§6) and the human-study protocol
> `docs/research/human-study-protocol-v0.1.md` (§9). The stable component
> contract shipped as [Component API v1](api-v1.md) instead, and the human
> preference study remains an unstarted, separate program; the
> [measurement record](measurement.md#what-it-cannot-settle) says so without
> naming a file.

**Contents** — [1 What it is](#1-what-openrealtime-is) · [2 Architecture](#2-architecture-a-data-plane-and-an-interaction-control-plane) · [3 Protocol & transports](#3-the-openrealtime-protocol-version-1) · [4 Efficiency](#4-efficiency) · [5 Safety](#5-safety-and-authority) · [6 Exists vs new](#6-what-exists-versus-what-is-new) · [7 Build phases](#7-build-phases) · [8 Definition of done](#8-definition-of-done-for-v10) · [9 Measurement](#9-measurement-program-post-launch-continuous) · [10 Risks](#10-risks) · [11 Decisions](#11-decisions-taken-and-what-remains-open)

## 1. What OpenRealtime is

> **An open, self-hostable implementation of the OpenAI Realtime API that runs any
> voice stack, adds a background reasoner, and extends the protocol — minimally
> and compatibly — to realtime video and computer use.**

1. **It is the Realtime API.** An official client connects unchanged. Browsers
   and mobile reach the same protocol through a WebRTC adapter, and an existing
   LiveKit deployment through an agent participant — both of which are clients of
   the protocol, not alternative ways in.
2. **It runs any voice stack.** Cascade, Omni, or full-duplex interaction model.
3. **It makes any of them smarter.** A background reasoner shares one trajectory
   with the foreground model. A single-model server cannot do this by
   construction: there is no second model and no shared log to put it on.
4. **It sees and acts.** The OpenRealtime Protocol adds realtime video and computer
   use over the same session, as a strict backward-compatible superset.

### 1.1 The motivating scenario

A design meeting with an agent.

You talk; it answers promptly and briefly, and expands when you ask. Meanwhile it
is editing the document, running the change, and fixing what broke — in the
background, in parallel, without going quiet while it works and without making you
wait for all of it before it responds to the thing you just said.

That scenario needs every part of this plan at once: voice for the conversation,
computer use for the work, fast/slow for the division of labour, and an
interaction plane that keeps the foreground alive while the background runs. It is
also the demo, and a better one than "an agent that watches your screen and
clicks," because it is the shape of the problem rather than a capability display.

### 1.2 Scope

**Primary: voice.** Every design decision is settled in favour of the voice path
when the two conflict.

**Transports are in scope, not assumed.** A WebSocket Realtime endpoint is not
enough on its own: most clients find it hard to integrate, because raw PCM over a
socket leaves the client to implement echo cancellation, jitter buffering, packet
loss concealment, and adaptive bitrate itself. Over a real network that is the
difference between a demo and a product. So OpenRealtime ships a **WebRTC**
adapter and a **LiveKit** agent participant. Both sit strictly above the protocol
and speak it like any other client (§3.6); neither is a second way into the
session core.

**Secondary: computer use, jointly with voice.** Not a separate product line. The
justification for their sharing a runtime is AOI's own result — a streaming voice
model perceives and cannot act, a computer-use agent acts and cannot hear, and the
value is one session doing both over one trajectory.

**Out of scope:** robotics. Telephony infrastructure. Training or fine-tuning
models. Acting as a provider marketplace or billing aggregator.

## 2. Architecture: a data plane and an interaction control plane

### 2.1 Structure

Four subsystems over one session core. Three of them move and transform data; the
fourth decides *when* the other three act.

```text
┌────────────────────────────────────────────────────────────────┐
│ Gateway            OpenRealtime wire, session lifecycle        │
├────────────────────────────────────────────────────────────────┤
│ INTERACTION        the decisions about *when*                  │
│   control plane    trigger · preparation · endpoint & floor ·  │
│                    barge-in · fast/slow rollout · backchannel · │
│                    turn projection · holding · commitment ·    │
│                    repair · admission                          │
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
│ Session core       trajectory · eventloop                      │
│                    append-only log, safe points, atomic commit,│
│                    cancellation, irreversibility ledger        │
├────────────────────────────────────────────────────────────────┤
│ Bindings           declare which subsystems the model owns     │
│                    cascade │ omni │ duplex │ upstream          │
└────────────────────────────────────────────────────────────────┘
```

**Perception and action are duals.** Perception turns a continuous external stream
into discrete commitments by *gating* — discarding what does not matter. Action
turns discrete commitments back into a continuous external stream by *pacing* —
holding back what is not yet safe to emit. The same boundary, crossed in opposite
directions.

**Cognition is not a boundary at all.** It neither reads from nor writes to the
outside world: it reads the log and appends to the log. Grouping it with
perception and action is a convenience, not a symmetry, and it is worth saying so
rather than implying a parallelism that does not exist. What the three have in
common is only that data flows through them — which is exactly the definition of a
data plane.

**Interaction carries no data.** Trigger cadence, speculative pre-start, floor
ownership, barge-in, commit-versus-cancel — these are *decisions about when*, made
over evidence from all three. Nothing flows through them. That is the definition
of a control plane, and the borrowing from networking is exact rather than
metaphorical: the data plane moves things, the control plane decides how.

Keeping them separate is not an aesthetic preference. `realtimegateway/session.go`
is 819 lines precisely because it interleaves data transformation with timing
policy, and that is why adding a second speech path currently means forking it.

**"Control plane" is a structural claim, not a ranking.** Interaction is the most
important subsystem in this project. A duplex model has interaction trained into
its weights; cascade and Omni models have none whatsoever, so for two of the four
bindings every bit of responsiveness and naturalness the system exhibits is
manufactured there and nowhere else (§2.5). It sits above the data plane because
it commands it, not because it is peripheral to it.


The split throughout is mechanism versus policy. The session core provides safe
points, atomic commit, and cancellation delivery — mechanism. The interaction
plane decides when to *request* them — policy. Policy that is welded into
mechanism cannot be measured or replaced, and measuring and replacing policy is
what this project is for.

### 2.2 Perception

AOI's mechanism generalises, and it is why video is an addition rather than a
second architecture:

> **An observer converts a continuous stream into sparse, persistent text
> appended to the trajectory, behind a gate that costs almost nothing and
> produces nothing on unchanged input.**

OpenRealtime already has one and does not name it as such: acoustic VAD →
`asrbuffer` → Qwen3-ASR → typed revisions → canonical observations.

| | Audio observer (exists) | Video observer (new) |
| --- | --- | --- |
| Gate | acoustic VAD, prefix/silence hysteresis | pixel-change, ~3 Hz sampling |
| Extract | Qwen3-ASR streaming | keyframe capture |
| To text | transcript revisions | narration |
| Commit | canonical observation | canonical observation |

```go
type Observer interface {
    Name() string
    Gate(Frame) bool                                   // sub-millisecond, no I/O
    Observe(context.Context, []Frame) ([]Observation, error)
}
```

Two AOI findings are binding design constraints:

- **Narration is the value, not keyframe selection.** Persistent text is what
  survives after images are pruned, so the contract returns text and images are an
  optional attachment. This also keeps `trajectory.Snapshot` cheap, since it is
  copied for every continuation request.
- **Components must be selected per model.** Gemini 3 Flash regresses when given
  the keyframe stream, through image-token dilution. Observers are therefore
  independently switchable per session, defaults are per binding, and §9 measures
  the observer set per model rather than assuming one bundle.

**Narration is composed, not fixed.** The primary configuration is AOI's own: the
session's model narrates new visual content as a side-output, which is what the
measured result used and what avoids a second model in the loop. But it presumes
the foreground model can see, and `duplex` breaks that presumption outright —
Moshi has no vision at all. So the narrator is a composition slot:

```go
// Narrator turns admitted keyframes into the persistent text that survives
// after the images themselves are pruned.
type Narrator interface {
    Narrate(context.Context, []Frame, trajectory.Snapshot) (string, error)
}

//   sessionNarrator   — the binding's own model, as a side-output   (default)
//   dedicatedNarrator — a separate vision-language model            (fallback)
```

The choice is a session-level composition, not a build-time decision. `cascade`,
`omni`, and `upstream` default to `session`; `duplex` requires `dedicated`, and
any binding may be configured either way — which is also what makes the
narrator a measurable factor rather than an assumption.

### 2.3 Cognition

Cognition owns the *structure* of model work over the log, and nothing about its
timing:

- **Providers** — the streamed continuation contract (`continuation.Provider`),
  provider-neutral, one interface for a local model, a hosted model, or a
  binding's own foreground model.
- **Authority** — which providers may propose a tool call and which may execute
  one. A fast provider's calls are structurally non-executable; only an
  authoritative continuation reaches the tool runtime.
- **Trajectory continuation** — how a provider's output commits: versioned
  compare-and-append, retained provider state, stale-prefix rejection, and the
  context projections used as experimental controls.

#### The two boundaries

One trajectory, two providers, and exactly two rules about what each may do:

> **The fast provider cannot call tools. The slow provider cannot speak.**

Fast does minimal or no thinking and produces an assistant message quickly. That
message triggers the slow provider — a larger, more capable model with a real
thinking budget — which reasons further and issues the tool calls. Results append,
and fast speaks again. Fast is the voice; slow is the brain.

The first rule exists today: fast output becomes a `tool_proposal`, structurally
non-executable. **The second is a change from current behaviour** —
`session.go:493` enqueues both phases for speech, and slow supersedes fast. Under
this design slow's output appends to the trajectory and a fast continuation voices
it.

That costs a short extra hop and buys three things. It removes the race where slow
contradicts something fast already said, since fast is always the last writer
before audio. It lets fast condense a long written answer into something worth
listening to, which is a different job from producing it. And it makes the
division of labour legible: one model owns what the user hears, one owns what the
system does.

Neither rule is a routing decision or a prompt convention. Both are properties of
the provider descriptor, enforced where output commits.

#### The trajectory is shared, and asynchronous

Fast and slow share **one** trajectory. This is interleaved thinking, not two
agents exchanging summaries: slow continues the exact prefix fast produced,
including its reasoning and its non-executable proposal.

Sharing it forces the log to be genuinely asynchronous. Four requirements follow,
and they are the standard ones for any agent whose environment does not wait
(Li, *AI Agent*, ch. 6, "Async and Event-Driven"):

- A tool call is appended the moment it is produced. Its result is appended only
  when the call completes, so a prefix in which a call has no result is a valid
  state rather than a corrupt one.
- Other events — a new user utterance, a revision, a second tool returning — may
  legitimately land between a call and its result.
- An interruption during tool execution must leave the log well-formed rather
  than dangling, so an unfinished call needs a typed placeholder its eventual
  result supersedes.
- Batched events must be legible as a batch. A model handed four events at a safe
  point attends to the last one unless the batch is marked as a batch.

The first two exist (`PendingToolCall`, atomic result batches, stale rejection).
The last two do not, and are the work.

#### Providers versus arrangement

**Fast and slow are cognition *providers*; the fast/slow arrangement is an
interaction policy.** This distinction matters, and the earlier drafts of this
document had it wrong — they listed `interleave` under cognition while listing
`preparation`, which is the speculative launch policy for exactly those two
providers, under interaction. That is incoherent: the same decision cannot live in
two subsystems.

The resolution follows the mechanism/policy split used everywhere else:

| Concern | Owner |
| --- | --- |
| There are providers with different latency and quality profiles | cognition |
| One may propose tools, the other may execute them | cognition |
| Output commits atomically against a version | cognition |
| When fast fires; whether slow pre-starts speculatively | **interaction** |
| Whether slow runs unconditionally or only on some trigger | **interaction** |
| Whether slow's result supersedes fast's, and what fills the gap | **interaction** |

So `interleave`'s current content splits. Its authority model and phase provenance
stay in cognition; its rollout policy — "fast once, slow unconditionally, slow
again after tool results" — becomes a named, swappable interaction policy.

That is not a cosmetic move. That rollout policy is exactly what factor F2 in §9
varies (fast-only · fast+slow · endpointed slow-only), and a policy that cannot be
swapped cannot be measured. It also explains something the earlier framing
obscured: **fast/slow is an interaction pattern over cognition providers**, which
is why it is one of the main things the runtime contributes to bindings whose
models have no interaction capability of their own.

### 2.4 Action

Currently spread across `realtimegateway/speech.go` (640 lines) and `media.go`.
Pulling it out is the largest single piece of core work in the plan.

It owns:

- **Speech planning and pacing** — text to audio, bounded prepared/queued
  horizons, fixed-interval frame emission.
- **The irreversibility ledger** — prepared → queued → played → cancelled, plus
  outstanding repair obligations.
- **Tool dispatch** — only from authoritative calls, idempotent by call ID.
- **Computer-use actions** — the same dispatch path.

**One commit boundary for every output.** Speech, assistant text, tool calls, and
clicks are all irreversible once emitted. A spoken sentence cannot be unsaid; a
repair is damage limitation, not reversal, and treating it as a safety margin
would be a mistake. So action does not classify outputs by how
recoverable they are — it holds a single line between *decided* and *emitted*,
identically for all of them: nothing crosses without authority, and everything
that has not yet crossed can still be cancelled.

This is simpler than a taxonomy of consequence and it is also stronger, because
the guarantee does not depend on having classified a new action type correctly.
What remains genuinely per-action is a **confirmation requirement**, and that is a
policy a developer declares (§3.5), not a property the runtime infers. It applies
uniformly: a payment click and reading an account balance aloud in a shared room
can both warrant confirmation.

### 2.5 Interaction

**Interaction is doing the right thing at the right time — and doing different
things at once.** Both halves matter, and the second is the one usually missed.

The timing half is what the project has already studied: trigger cadence,
speculative preparation, endpointing, barge-in. The parallelism half is that fast
and slow are not a latency trick. They are a **division of labour**: the fast
provider answers what the user actually asked, concisely and now; the slow
provider does the heavy work concurrently. Dead air is the symptom that reveals
this plane is missing, and so is its opposite — an agent that finishes all the
background work before saying anything, then delivers everything at once.

This is the component that produces interaction quality, and for two of the four
bindings it is the *only* source of it.

A full-duplex interaction model has turn-taking, overlap, and interruption trained
into its weights. An Omni model has none of it — it is a turn-based generator
handed a turn by an external VAD. A cascade has none of it either, and is
additionally assembled from components that each know nothing about the
conversation's timing. **Neither cascade nor Omni has any interaction capability
inside the model.** Whatever responsiveness, overlap
handling, or naturalness they exhibit is manufactured outside, here. That makes
this the highest-leverage subsystem in the system for those bindings, and it is
also where the project's existing research already lives — the M2 schedulers, the
M3 duplex and repair work, preparation pacing, and the cadence matrices are all
interaction results.

Each row is a named policy with an interface and a shipped default:

| Policy | Decides | Affects | Today |
| --- | --- | --- | --- |
| Trigger | when a decision opportunity opens (cadence, revision-triggered) | responsiveness | `microturn` |
| Preparation | whether to speculatively pre-start fast/slow before the endpoint | perceived latency | `preparation` |
| Fast/slow rollout | when each cognition provider fires; whether slow runs unconditionally; whether it supersedes | intelligence per unit latency | `interleave` (policy half) |
| Duplex state | the authoritative answer to *is the user speaking* and *is the agent speaking* | every other policy depends on it | VAD in `audio.go`, `activePhase` in `speech.go`, the state machine in `duplex/policy.go` — **three places, not yet one** |
| Endpoint / floor | when the user has finished; who holds the turn | latency *and* correctness | gateway VAD, `duplex` |
| Barge-in | detecting user speech over agent output and cancelling | naturalness | gateway interrupt path |
| Admission | which work gets scarce compute; what preempts | tail latency | `admission` |
| Commitment | how much to emit before certainty; when to cancel | wrong-start rate | scattered in the gateway |
| Repair | what to do when a commitment proves wrong | error recovery | trajectory repair lifecycle |
| Backchannel | whether to emit "mm-hm" while the user is still speaking | naturalness | **not implemented** |
| Turn projection | anticipating the end of a user turn before silence confirms it | latency | **not implemented** |
| Holding behaviour | what occupies the gap while slow work runs | perceived intelligence | the **fast provider**, by instruction — see below |

Backchannel and turn projection are the unbuilt ones. Holding behaviour is not a
gap and not a component: **it is the fast provider's job**, and the fast provider
is the only participant that can do it well.

That follows from the canonical trajectory rather than from convenience. A
separate filler generator would be blind — it would emit "one moment" on a timer
with no idea what is happening. The fast provider sees the same log as the slow
one, so it knows what is in flight, what has returned, and what the user last
asked. It keeps the air warm with a filler, a "let me work on that", or a
clarifying question when nothing has come back; and when a tool call completes it
reports the progress, because the result is in its context too.

That last part has a concrete consequence for the rollout policy. Today a
tool-result batch resumes slow directly (`interleave/processor.go`). For progress
reporting to work, a completed tool result must also be able to trigger a short
fast utterance — not a second answer, a status. Whether it does is a rollout
decision, and it is one of the levers §9's F2 should vary.

A system that scores well on tool-use benchmarks and leaves dead air while it
reasons has solved the measurable half of the problem.

**Two of these concerns are not policies at all, and should not be built as
machinery.**

*Division of labour and response granularity live in the instructions* given to
the fast and slow providers (§2.3), not in a runtime component. The fast provider
is told to answer quick questions directly, defer hard ones to the slow provider,
keep answers short and offer detail rather than delivering it unprompted, and
never leave dead air — a filler, "let me work on that", or a clarifying question,
whichever fits. That is a prompt, and building a policy interface around it would
be machinery for something a sentence already does.

*Triage is the event loop's existing job.* Deciding whether an arriving event
cancels current work, waits for the next safe point, or is handled without
disturbing what is running is the queuing property of an asynchronous agent, and
it belongs to `eventloop` alongside priority and safe-point admission. The one
genuine gap there is the parallel branch: the loop can cancel and it can queue,
but it cannot yet answer a quick question without waiting for the work in flight.

#### Duplex state, and what depends on it

Two facts must be known at every instant and owned in one place:

- **Is the user speaking?** From VAD, or from a model-native signal where the
  binding provides one.
- **Is the agent speaking?** Meaning *audio is reaching the user right now* — not
  that a continuation is generating, and not that speech was enqueued. Synthesised
  audio takes real time to play, and a design that conflates "we decided to speak"
  with "the user is hearing us" will get every overlap decision wrong.

The pieces exist and are scattered: the VAD detector in `realtimegateway/audio.go`,
`activePhase` in `speech.go`, and an unwired state machine in `duplex/policy.go`
that already models `USER_SPEAKING`, `SYSTEM_SPEAKING`, and the overlap states.
Consolidating them into one authoritative session state is M11 work, and it is a
precondition for most of this table rather than another row in it.

#### Pending events, and why deferral must be general

A policy that defers work must never be able to discard it. This needs one
invariant, not a rule per case:

> **Commit is unconditional; acting is conditional; every deferral has a
> wake-up.** An event that has been committed but not yet acted upon must
> eventually cause a run. Nothing committed is ever silently dropped.

Three parts:

1. **Commit is unconditional.** Every external event — a tool result, an
   observation from any observer, a timer, an external trigger — enters the
   trajectory the moment it arrives, whatever the duplex state.
2. **Acting is conditional.** Whether a continuation runs is a policy decision.
   Deferring leaves the event **pending**; it does not consume it.
3. **Every deferral condition has a matching wake-up.** When agent playback
   completes, when the user's turn ends, when an admission lease is granted — if
   pending events exist, a run starts.

Applied to a tool result completing:

| State when it lands | Commit | Run |
| --- | --- | --- |
| Both silent | immediately | now — a fast continuation reports progress |
| User speaking | immediately | deferred; the endpoint starts one |
| Agent speaking | immediately | deferred; **playback completion** starts one |

**The third row is why this has to be an invariant rather than a table.** The
second row is safe almost by accident: a user always stops speaking, and
endpointing already starts a run, so a deferred event gets picked up without
anyone designing for it. The third row has no such natural trigger. An agent that
finishes speaking, with the user silent and a tool result sitting unacted in the
trajectory, will sit there forever. A rule written case by case would have covered
the first two and lost the third — and would lose the fourth condition someone
adds next year.

**Provider backpressure is a candidate condition, left open by default.** If token
cost turns out to warrant it, a run can be deferred under provider pressure using
the same invariant — commit, mark pending, wake on relief. It is configurable and
unconstrained by default, because throttling a conversation to save tokens is a
deployment decision and not something the runtime should assume.

*Implementation note.* Today `eventloop.RunNext` commits a batch and invokes the
processor in one step, so "committed but not yet acted upon" is not a
representable state. Introducing deferral means representing it and tracking it,
plus wake-ups on the duplex-state transitions. Part of that path exists —
`signalCognition` is already called from media commitment transitions
(`media.go:262`) — but the bookkeeping is not there. This is M11 work and it is
the least optional item in it, because every policy that defers depends on it.

#### Policy models

Backchannel and turn projection are judgement calls about a live conversation, and
rule-based versions of them are exactly the brittle keyword-and-threshold
machinery this project has avoided everywhere else. Each is served instead by a
**policy model**: a small, fast model with a short prompt and a constrained
output.

The name deliberately avoids "interaction model", which in this document and in
the wider literature means a full-duplex model such as Moshi.

Three constraints keep this cheap and safe:

1. **Enumerated output, never free generation.** A policy model selects among
   declared options and returns a structured decision. It cannot become a third
   cognition provider, and its injection surface stays minimal.
2. **No tool authority, ever.** Policy models sit outside the proposal/execute
   split because they never emit calls at all.
3. **Decision-time information only.** A turn-projection model may see only what
   was available at the instant of the decision. Prompted or trained on
   hindsight, it yields a judgement that cannot be reproduced online — the
   standard trap for any learned endpointer.

**Sizing is measured, not assumed.** Start at a state-of-the-art ~3B model — the
Qwen size class — and check whether it can actually make these calls. If it
cannot, escalate to ~8B. The point is that the answer comes from F8 before the
implementation is fixed, rather than from picking a comfortable size and
discovering later that it was wrong in either direction.

They degrade cleanly. With no policy model configured, backchannel falls back to
off and turn projection to VAD-only endpointing. The system runs without them; it
is simply less alive.

Cost: each decision is tens of milliseconds on a small model, admitted at
interactive class under the existing governor — above speculative preparation,
below the foreground fast continuation. Whether policy models are co-located or
run as sidecars is settled in M16 (§7).

Because these are named policies rather than gateway internals, they are also the
axes the measurement program varies (§9) — which is the practical reason for
separating policy from mechanism rather than an aesthetic one.

### 2.6 Bindings declare ownership

A binding is not a pipeline. It is a declaration of which subsystems the model
owns.

| Binding | Perception | Cognition (fast) | Cognition (slow) | Action | Floor |
| --- | --- | --- | --- | --- | --- |
| `cascade` | engine | engine | engine | engine | engine |
| `omni` | model | model | engine | model | **engine** |
| `duplex` | model | model | engine | model | model |
| `upstream` | remote | remote | engine | remote | engine or remote |

| Binding | Models |
| --- | --- |
| `cascade` | Qwen3-ASR + vLLM Qwen + Fish S2-Pro |
| `omni` | Qwen3-Omni, MiniCPM-o 4.5 |
| `duplex` | Moshi |
| `upstream` | `gpt-realtime-2`, vLLM, SGLang, any Realtime-compatible endpoint |

Two things the table does not show and should be read alongside it.

**The floor column is an interaction policy, not a data-plane subsystem.** It
appears here because it is the one interaction policy a model can take over. Even
in `duplex`, where the model owns the floor, the engine still owns every other
interaction policy — trigger, fast/slow rollout, holding behaviour, admission —
so ownership is not all-or-nothing.

**The slow column is always the engine.** That is the whole differentiator: no
binding delegates slow cognition, because no foreground model provides it. What
varies is only who supplies the fast provider.

The engine keeps the floor in `omni` deliberately: Omni models assume turn-taking
and lean on VAD, which mis-endpoints on spelled identifiers and digit strings.
That is a measurable claim (F5 in §9) and one of the clearer places the runtime
adds value to a model that already does perception and generation itself.

## 3. The OpenRealtime Protocol, version 1

The OpenAI Realtime API is large and mature. Additions must be few, orthogonal,
and strictly compatible. This section is the design; the normative text goes in
`docs/protocol/openrealtime-1.md` and is proposed publicly **early**, before v1.0.

It has no abbreviation. The wire namespace is `openrealtime.*`, the project is
OpenRealtime, and the protocol is the OpenRealtime Protocol — one name to learn
rather than a name plus an acronym that means nothing to a first-time reader.
Written in full on first use, "the protocol" thereafter.

### 3.1 Principles

1. **Strict superset.** Every valid OpenAI Realtime GA session is a valid
   OpenRealtime session. A voice-only client written against the OpenAI API works
   against an OpenRealtime server with no changes, and with no awareness that the
   extension exists.
2. **Namespaced.** New events are prefixed `openrealtime.`; new fields live under
   an `openrealtime` key inside existing objects. No existing event changes shape,
   and no existing field changes meaning.
3. **Negotiated, never assumed.** Extensions activate only after the client
   declares support and the server confirms. Absent negotiation, behaviour is
   exactly the base protocol.
4. **Minimal.** The total addition is three events and two object extensions.
5. **Actions are not new protocol.** Computer use rides existing function calling.

### 3.2 Negotiation

```jsonc
// client → server
{ "type": "session.update", "session": {
    "openrealtime": {
      "version": 1,
      "supports": ["video.input", "observations", "computer_use"] }}}

// server → client
{ "type": "session.updated", "session": {
    "openrealtime": {
      "version": 1,
      "enabled": ["video.input", "observations"],
      "video": { "format": "jpeg", "fps_cap": 3, "max_dimension": 1280 } }}}
```

The version lives inside the `openrealtime` key, so it carries the number and
nothing else — repeating the name inside a field already named for it would be
noise. A base-protocol server ignores the unknown key and never echoes it; the
client sees no `enabled` list and stays voice-only. A base-protocol client sends nothing
and gets an ordinary session. Backward compatibility is a property of the
negotiation, not of client discipline.

### 3.3 Video input — two events

```jsonc
// declare or update a source. Required before frames; carries the geometry
// that computer-use coordinates are expressed in.
{ "type": "openrealtime.input_video_source.update",
  "source": "screen",              // "screen" | "camera" | opaque id
  "state": "active",               // active | paused | closed
  "width": 1920, "height": 1080 }

// one frame
{ "type": "openrealtime.input_video_frame.append",
  "source": "screen",
  "frame": "<base64 jpeg>",
  "timestamp_ms": 1724236800123 }
```

Design notes, each load-bearing:

- **`source` is mandatory** because screen and camera are simultaneously live and
  semantically different: you act on the screen, you observe the camera.
- **`input_video_source.update` exists mainly for geometry.** A click at `(x, y)`
  is meaningless without knowing the coordinate space the model saw. Declaring
  width and height once, and on every change, is what makes computer-use
  coordinates well-defined instead of a convention.
- **The gate runs server-side, always.** The client sends video; it makes no
  decisions about which frames matter. This is not a performance trade — it is
  the product boundary. Selective perception is what OpenRealtime is *for*; the
  critique it answers is that native realtime APIs "offer no selective
  perception." Push the gate into the client and OpenRealtime no longer has it
  either — every client reimplements it, differently or not at all, and the
  observation a session produces stops being a property of the server.

  A dumb client is also what makes the protocol adoptable: send frames, receive
  observations. A client that must implement pixel-change gating at a specific
  threshold is a client nobody writes.

  Bandwidth is a *transport* problem, not a client-intelligence problem. Base64
  JPEG at a capped rate is fine for v1 at the frame rates that matter; a static
  screen costs almost nothing once the reserved WebRTC transport lands, because
  inter-frame prediction already removes exactly the redundancy a pixel-change
  gate would have removed. Solving it in the client would be solving it in the
  wrong layer, twice.

- **This is how video enters, for every client.** A browser sending a WebRTC
  track does not bypass this event; the transport adapter decodes the track and
  emits these frames on its behalf (§3.6). One entrance means one thing to
  specify and one thing to test.

### 3.4 Observations — one event

```jsonc
{ "type": "openrealtime.observation.added",
  "observation_id": "obs_...",
  "observer": "video",             // "video" | "audio" | opaque id
  "source": "screen",
  "text": "A confirmation dialog appeared: 'Confirm payment of $40.'",
  "item_id": "item_...",
  "timestamp_ms": 1724236800456 }
```

Observations are already committed to the trajectory; this event exists so a
client can display and audit what the agent perceived. It is optional and
purely outbound.

### 3.5 Computer use — zero new events

Actions are ordinary function calls. `response.function_call_arguments.done`
already exists, and the client already returns
`conversation.item.create` with a `function_call_output`. Nothing is added.

The elegant part is the return path: **the result of an action is the next screen,
and the screen already arrives through the video stream.** So the tool output
stays text (`"clicked"`), and the visual consequence flows back through the
perception path exactly as it does for a human. No image is ever carried in a
function result, which is what keeps the addition small.

Two spec artifacts instead of protocol surface:

**A standard tool namespace**, proposed publicly as a specification in its own
right alongside the protocol, so that client and server agree on semantics without
negotiating them per deployment — and so that other realtime implementations can
adopt the same action vocabulary without adopting anything else here:

```text
computer.click · computer.double_click · computer.move · computer.drag
computer.type  · computer.key          · computer.scroll
computer.screenshot · computer.wait
```

Each with a fixed JSON Schema, published in the spec, and each taking `source` so
the action targets a declared video source.

This is deliberately the most portable piece of the design. It depends on nothing
in this document except function calling, which every Realtime implementation
already has, so it is the part most likely to be adopted elsewhere — which is the
reason to propose it as a standard rather than document it as ours.

**One additive field on tool definitions**, declaring whether an action requires
explicit authorization before dispatch:

```jsonc
{ "type": "function", "name": "computer.click",
  "parameters": { "...": "..." },
  "openrealtime": { "confirm": "never" } }     // never | policy | always
```

There is no reversibility class. Every output — speech, text, tool call, click —
is irreversible once emitted, so the runtime does not grade them; it applies one
commit boundary to all of them (§2.4). `confirm` is a developer's declaration
about consequence, not an inference the runtime makes, and it applies to any tool,
not only `computer.*`. Servers that do not understand the key ignore it, as JSON
Schema requires.

### 3.6 Transports layer above the protocol

The protocol is the narrow waist. Nothing goes around it.

```text
  browser (WebRTC)  ─┐
  mobile  (WebRTC)  ─┼─►  transport adapter  ─┐
  LiveKit room      ─┘                        │
                                              ├─►  OpenRealtime Protocol  ─►  session core
  plain WebSocket client  ────────────────────┘         (WebSocket)
```

Four rules, and the third is the one that matters:

1. **The protocol over WebSocket is the only entrance to a session.**
2. **A transport adapter is a protocol client.** It terminates media, handles ICE
   and jitter and echo cancellation, and then speaks exactly the events any other
   client speaks.
3. **No adapter has privileged access.** If an adapter can express something a
   plain WebSocket client cannot, that is a defect, not a feature.
4. **Adapters may be in-process** where that is cheaper, but must stay
   semantically identical to the wire form and are tested through the real
   protocol, never through a private path.

The reason is compatibility over time. A transport that reaches the session core
directly becomes a second place the protocol can drift, and every such place
multiplies the compatibility surface — a client works over WebRTC and fails over
WebSocket, or a feature exists on one path and not the other. Keeping one waist
means there is exactly one thing to specify, one thing to version, and one
conformance suite that covers every client regardless of how it connected.

| Adapter | Handles | For |
| --- | --- | --- |
| *(none — direct)* | nothing | server-to-server, local development, the simplest possible client |
| WebRTC (in-process, Pion) | ICE, jitter buffer, echo cancellation, loss concealment, adaptive bitrate | browsers and mobile |
| LiveKit or another RTC provider | the provider's room, tracks, and data channel | deployments that already run RTC infrastructure |

Two consequences worth stating.

**The LiveKit integration need not live in this repository at all.** It is an
agent that joins a room and proxies to a protocol endpoint. It can ship
separately, on its own release cycle, written by someone else — which is only
possible because it is a client rather than a component.

**Clean audio is the client's responsibility.** Echo cancellation and noise
suppression happen before audio reaches the protocol, and the server does not
attempt either. This is a stated requirement on clients, not an omission: a
browser's WebRTC stack already does it well, doing it again server-side would be
worse than doing it once, and a server that tried to compensate for unknown
client-side processing would be guessing. A LiveKit agent participant inherits the
same requirement from whatever publishes into the room.

**Video always enters as protocol events.** A WebRTC adapter decodes the track and
emits frames; the codec's inter-frame compression pays for itself on the
client-to-adapter hop, which is the hop that crosses the network. The adapter to
protocol hop is local and does not need it.

### 3.7 Total surface

| Addition | Count |
| --- | --- |
| New client → server events | 2 |
| New server → client events | 1 |
| Extended objects | 2 (`session`, tool definition) |
| Changed existing events | 0 |
| New transports | 0 |

Three events for vision and computer use, on a protocol with 66 wire names, is
the bar this design was written to.

## 4. Efficiency

A stated requirement, so it gets a section and appears in the acceptance gates.

**Perception.** The gate is sub-millisecond and runs before any I/O, so idle
content costs almost nothing — AOI measured roughly 60% of steps as fully idle.
Frames that pass are capped by rate and dimension. The saving that matters is
*compute and context*, not bandwidth: the gate exists so the narrator and the
model are never handed a frame that says nothing new.

Bandwidth is handled at the transport. Independent JPEG frames are wasteful on a
static screen — roughly 450 KB/s at 1080p and 3 Hz — which is why WebRTC carries
video on a media track (§3.6). It is never an argument for making the client
smart (§3.3).

**Context.** AOI's decomposition found that narration, not keyframe selection, is
where the value is: persistent text survives after images are pruned. So the
observer contract returns text, images are an optional attachment with a bounded
retention window, and the trajectory references media by handle rather than
inlining it — `trajectory.Snapshot` is copied for every continuation request and
must stay small.

**Compute.** The admission governor already exists and already separates
interactive, speculative, and background classes with cooperative preemption.
Video narration is a new class competing for the same GPU; it belongs under the
same governor rather than beside it.

**Acceptance gates.** Steady-state cost of an idle video source; added latency of
an enabled video observer on a voice-only task; bandwidth with and without
client-side gating; context growth per minute with narration retention. Each has
a published number at release.

## 5. Safety and authority

| Concern | Mechanism |
| --- | --- |
| Fast model taking irreversible action | Existing proposal-versus-execute authority. Fast calls are structurally non-executable, not discouraged. |
| Destructive actions | A declared `confirm` policy per tool (§3.5). Applies uniformly to speech, text, tool calls, and clicks, because all of them are irreversible once emitted. |
| Prompt injection via observed content | Observations carry `observer` provenance, never `user`. On-screen text is data; it is never promoted to instruction authority. |
| Blast radius | Computer-use tools target a declared video source bound to a declared context — a browser context or virtual display, never an ambient desktop by default. |
| Auditability | Every executed action is a trajectory item with causal parents, so any click traces to the observation and continuation that produced it. |

The injection row deserves emphasis. An agent that narrates screen text into its
own context is an obvious injection vector, and the typed provenance the
trajectory already carries is the correct defence — but it must be *enforced*,
with a test that a narrated instruction cannot reach execution authority.

## 6. What exists versus what is new

The schedule depends on this being honest, so it is stated separately rather than
implied by the milestones.

| Piece | State |
| --- | --- |
| Realtime protocol, conformance suite, session lifecycle | **exists**, complete against the pinned spec |
| WebSocket transport | **exists** |
| WebRTC adapter (Pion), above the protocol | new, medium |
| LiveKit agent participant | new, small; can ship as a separate repository |
| Unified duplex state | new, small; the pieces exist in three places (§2.5) |
| Slow-cannot-speak boundary | new; a change to `session.go:493` behaviour (§2.3) |
| Tool-call placeholders and batch-event markers | new, small; the async-trajectory gaps (§2.3) |
| Cascade pipeline — ASR, fast continuation, TTS, tools | **exists** as `realtimegateway` |
| Trajectory, safe-point loop, cancel and repair, admission | **exists** |
| Fast/slow over one trajectory | **exists** as `interleave`, to be split (§2.3) |
| Benchmark harness, provenance, fail-closed reporting | **exists** |
| Action extracted from the gateway | new, medium — the largest single piece of core work |
| Interaction policies extracted and named | new, medium |
| `Binding` seam — the gateway currently *is* the cascade | new, medium |
| Realtime client for `upstream` | new, medium; reuses generated definitions |
| `Observer` interface, video observer, narrator composition | new, medium |
| Protocol spec and its conformance suite | new, small |
| Sidecar protocol and Omni sidecars | new, medium |
| Duplex binding | new; the binding is routine, the slow-lane injection is research |
| Computer-use safety model | new, small in code and design-critical |
| Parallel event handling in `eventloop` | new, small; cancel and queue exist, the parallel branch does not (§2.5) |
| Backchannel, turn projection | new, unbuilt; small policy models with constrained output (§2.5) |
| Holding behaviour and fast progress reporting | new; fast-provider instructions plus a rollout change so tool results can trigger a fast status utterance (§2.5) |
| Policy-model serving and admission class | new, small |
| Packaging, documentation, demo, operations | new, medium |

**Contract impact is additive throughout.** `api/v1` is not broken: the new
subsystem interfaces are new packages, and `MediaRef`, new item kinds, and new
observation types are additive fields and values.

`docs/api-v2-proposal.md` is unaffected and remains post-study. Its subjects —
spoken forms, `Reassembled`, `Alternates`, typed repair — are a perception
fidelity problem, independent of this restructuring, and should land together with
any audio-native contract change in one `v2` rather than piecemeal.

## 7. Build phases

Internal milestones, continuing the existing numbering.

**M11 — Subsystem extraction, duplex state, and bindings.** Consolidate
user-speaking and agent-speaking into one authoritative session state (§2.5), and
make agent-speaking mean audio is reaching the user rather than speech being
generated. Enforce the two cognition boundaries at the commit point: fast cannot
call tools, slow cannot speak (§2.3). Extract action out of
`speech.go`/`media.go`; extract the interaction control plane into named policies;
introduce the `Binding` seam; reimplement `cascade` behind it with byte-identical
behaviour. Add `upstream` — the Realtime *client* binding — reusing the generated
conformance definitions. First non-cascade pipeline, no new model dependency.

**M12 — The protocol and perception.** Spec published and proposed publicly.
`Observer` interface; conformance suite for the extension; compatibility tests in
both directions (base client against extended server, extended client against
base server). Re-implement AOI's perception components in Go: pixel-change gate,
keyframe capture, narration prompting, and the audio energy gate. Whisper is not
ported — Qwen3-ASR already occupies that role.

**M13 — Transport adapters.** A WebRTC adapter (Pion) and a LiveKit agent
participant, both written as clients of the protocol rather than as paths into the
session core (§3.6). This follows M12 rather than preceding it: an adapter is
written *against* the protocol, so the protocol has to exist first — including the
video events, since decoding a track into frames is most of what the adapter does. The acceptance test is adversarial: anything an adapter can
express, a plain WebSocket client must be able to express too.

**M14 — Omni bindings.** A documented, versioned sidecar protocol over a process
boundary; reference sidecars for **Qwen3-Omni** and **MiniCPM-o 4.5**. Python
stays out of the Go build, test, and analysis path; the conformance suite is the
contract.

**M15 — Duplex binding.** Moshi, as a required v1.0 capability. The model itself
is mature and two years published; running it, committing its observations, and
holding a full-duplex conversation is well-trodden. The open part is narrower
than "duplex support": **where a slow continuation splices into a model that owns
its own floor.** Candidates in order — inner-monologue text conditioning,
user-side context injection, explicit hand-off. The third certainly works and is
the documented fallback, so the binding ships regardless of how the research
resolves.

**M16 — Interaction policy models.** Backchannel and turn projection, each served
by a small model with a constrained, enumerated output (§2.5). Includes the
serving path and its admission class, and the decision-time-information test for
turn projection. Holding behaviour is not here: it belongs to the fast provider
and lands with the rollout policy in M11. Falls back to rules when no
policy model is configured, so the capability is additive rather than required.

**M17 — Computer-use composition and safety.** §5 implemented and tested: the
`computer.*` namespace, the uniform commit boundary, confirmation dispatch, the
injection-authority test, declared targets, action auditing.

**M18 — Release engineering.** Packaging, documentation set, demo application,
operations surface, reproducibility scripts, support policy.

**M19 — Measurement program.** §9, executed continuously *after* launch.

### 7.1 Order and constraints

**Dependencies.** M11 precedes everything: nothing else is safe until the seam,
the duplex state, and the two cognition boundaries exist. M12 precedes M13
(adapters are written against the protocol) and M17 (computer use needs the video
events). M14, M15, and M16 are independent of each other. M18 follows M11–M17;
M19 follows launch. Everything not on that list may overlap, and should.

**Extension-point stability.** `Binding`, `Observer`, `Narrator`, and the sidecar
protocol are what third parties write against. They are explicitly unstable until
v1.0 and versioned from it, so a binding written against v1.0 keeps working.
`api/v1` is untouched throughout — these are new packages, not changes to it.

**Provenance.** Every new dependency declares origin and licence per
CONTRIBUTING: Pion, the LiveKit SDK, Moshi, Qwen3-Omni, MiniCPM-o 4.5, and any
weights used by a policy model or a dedicated narrator. Model weights carry their
own redistribution terms, and those terms decide whether a reference sidecar can
ship weights or only fetch them — worth settling in M14 rather than at release.

**Regression coverage.** Every binding runs the same session-level suite, not only
its own tests. The four differ in who owns what, and a change to the interaction
plane that breaks `omni` while leaving `cascade` green is precisely the failure
this exists to catch.

**Reference hardware.** The §4 efficiency gates are meaningless without one.
Declare a single reference machine, publish every gate against it, and state it
next to the number rather than in a footnote.

M11–M17 overlap substantially. M18 gates the release. M10 continues undisturbed,
rebuilding from pinned revision `3048160e…` against executable SHA-256
`2168fe5a…`.

## 8. Definition of done for v1.0

The release gate is **functional completeness plus verified correctness**.
Comparative claims are gated separately, on the measurement program, and are not
allowed to block the release or to appear before their evidence.

**Install and run**
- One documented command brings up a working `/v1/realtime` on localhost.
- The default binding is `cascade` — fully local, no third-party account, nothing
  to sign up for. That is the right first impression for an open implementation.
- Switching to `upstream` is one flag and one credential, documented on the
  quickstart page rather than buried in a binding guide. A user with no GPU must
  reach a working session in the same number of steps as a user with one.
- An official OpenAI Realtime client completes a tool-using session unmodified,
  over WebSocket and over WebRTC.
- A browser client reaches a working voice session without implementing jitter
  buffering or resampling itself; echo cancellation and noise suppression are
  documented as its responsibility, which its WebRTC stack already satisfies.
- Joining a LiveKit room as an agent participant is documented with an example.

**Configurations**
- All four bindings run, are documented, and have a working example.
- Fast/slow configurable on every binding, including `duplex` via its fallback.
- Observers configurable per session, with a documented default set per binding.

**Protocol**
- The OpenRealtime Protocol published, versioned, and proposed publicly well
  before release.
- Conformance suites for the OpenAI surface and the extension.
- Bidirectional degradation tested and documented.

**Correctness and efficiency**
- DynaCU-Bench passes as a **functional check** that video observation and action
  grounding work end to end. It remains in the AOI repository; OpenRealtime ships
  a runner, not a copy.
- The §4 efficiency gates each have a published number.
- The injection-authority test passes.

**Documentation and operations**
- Quickstart, architecture, one guide per binding, protocol spec, safety model,
  benchmark harness guide, demo application.
- Health and metrics endpoints, structured logs, resource guidance per binding,
  stated support policy for `api/v1` and the OpenRealtime Protocol.

**Not a release gate:** the comparative matrix in §9. The README at v1.0 says what
the system *supports* and what has been *verified*; it makes no claim that one
configuration beats another until §9 says so.

## 9. Measurement program (post-launch, continuous)

The claim is not "four pipelines are supported" — that is a release gate. It is
"here is what each pipeline is worth, measured the same way." That answer
publishes continuously after launch.

**Design.** A reference configuration with paired cells changing exactly one
factor each — the design the frozen τ matrices already use. Full cross-product is
infeasible and uninterpretable.

**Reference.** `cascade` · Qwen3-ASR 1.7B · local Qwen fast · Gemini 3.5 Flash
slow at high effort · Fish S2-Pro · 200 ms cadence · endpoint-only observation ·
audio observer only.

| | Factor | Levels |
| --- | --- | --- |
| F1 | Binding | cascade · omni-Qwen3 · omni-MiniCPM-o · duplex-Moshi · upstream-`gpt-realtime-2` |
| F2 | Cognition | fast-only · fast+slow · endpointed slow-only |
| F3 | Observers | audio · audio+video · video-only |
| F4 | Trigger cadence | 50 · 100 · 200 · 400 · 800 ms |
| F5 | Floor source | engine floor · model-native VAD |
| F6 | Slow model and effort | Gemini high · Gemini medium · local slow |
| F7 | Observer components | keyframe+narration · narration-only · keyframe-only |
| F8 | Policy models | none (rule fallback) · backchannel · turn projection · both |

F7 exists because AOI found the bundle is not uniformly good — Gemini 3 Flash
regresses on the keyframe stream through image-token dilution. Shipping a default
without measuring its components per model would repeat a mistake the evidence
has already identified.

| Suite | Scale | Answers |
| --- | --- | --- |
| τ-Voice | 278 tasks × control and regular speech | tool-use success under voice. F1, F2, F4, F5, F6 |
| FDB v1.5 | 498 overlap recordings | overlap, barge-in, turn-taking. F1, F5 |
| FDB v3 | 100 tool-use recordings | tool use with real function results. F1, F2 |
| FD-Bench | 6,147 conversations, 77.2 h | endpointing and timing at scale. F1, F4, F5 |
| DynaCU-Bench | 100 dynamic + 50 static | video observation, action grounding. F1, F3, F7 |

**Reporting rules**, unchanged from the existing apparatus: no incomplete cell is
reported; every cell declares source revision and executable hash; latency claims
require distributions; negative results publish; no cross-suite synthesis.

**What it cannot settle:** human preference or perceived naturalness. Those need
the prospective protocol in `docs/research/human-study-protocol-v0.1.md`, which
is a separate program.

**Scale and budget.** M19 is larger than the existing M10 queue, which is already
a multi-week GPU-bound program for one binding. All eight factors are funded; none
is dropped for cost. The constraint is not budget but coupling: the program runs
**asynchronously**, alongside and after launch, and is never permitted to become a
reason to delay a release. The project therefore ships before its most interesting
claims are provable, which is the deliberate trade made in §8, and the claims
appear as each cell completes rather than in one batch at the end.

## 10. Risks

| Risk | Mitigation |
| --- | --- |
| The protocol changes after being proposed | Propose early in M12, freeze at v1.0, version explicitly, keep the surface at three events |
| Duplex slow-continuation injection does not work well | The binding ships on explicit hand-off regardless; the research result publishes either way |
| Video observation is too slow or too expensive | §4 gates are release blockers, not aspirations |
| WebRTC brings ICE, TURN, and scaling problems into the project | In-process termination targets direct and localhost cases; production scale is what the LiveKit path is for, rather than reimplementing an RTC provider (§3.6) |
| Computer use unsafe by omission | M17 gates the capability; the injection-authority test is a release gate |
| No external feedback before v1.0 | Accepted deliberately; DynaCU-Bench functional checks substitute for early users as a correctness signal |
| Measurement never happens once launched | M19 is a milestone with an owner, and the README's claim set is explicitly bounded until it lands |
| Study contamination | Default cascade path stays byte-identical; M10 rebuilds from a pinned revision |

## 11. Decisions taken, and what remains open

### 11.1 Settled

| Decision | Resolution |
| --- | --- |
| Default binding | `cascade` — fully local, no third-party account. `upstream` is one flag and one credential, documented on the quickstart, not buried in a binding guide (§8). |
| `computer.*` namespace | Proposed publicly as a specification in its own right, alongside the protocol. It depends on nothing but function calling, so it is the most portable piece of the design and the most likely to be adopted elsewhere (§3.5). |
| Where the video gate runs | **Server, always.** The client sends video and decides nothing. Selective perception is the product; delegating it to clients means the server no longer has it. Bandwidth is a transport concern, addressed by WebRTC later (§3.3). |
| Backchannel, turn projection | Ship in v1.0 as small policy models with short prompts and enumerated outputs rather than rules, with rule fallbacks when unconfigured (§2.5, M16). |
| Holding behaviour | The fast provider's job, by instruction — not a component. It shares the trajectory, so it knows what is in flight (§2.5, M11). |
| Tool result while speaking | An instance of the general rule: commit is unconditional, acting is deferred by duplex state, and every deferral has a wake-up so nothing committed is dropped (§2.5). |
| Fast/slow boundaries | Fast cannot call tools; slow cannot speak. Slow's output appends and a fast continuation voices it — a change from today's behaviour (§2.3). |
| Transport layering | WebRTC and LiveKit are adapters above the protocol, never second entrances to the session core (§3.6). |
| Echo cancellation and noise suppression | The client's job, and a stated requirement on clients. The server does neither and does not compensate for their absence (§3.6). |
| Policy model size | Start at a state-of-the-art ~3B model, escalate to ~8B only if measurement shows 3B cannot do the job. Measured under F8 before the implementation is fixed (§2.5). |
| Provider backpressure as a deferral condition | Configurable, unconstrained by default. Throttling a conversation to save tokens is a deployment decision, not a runtime assumption (§2.5). |
| Duplex | Required v1.0 capability. Ships on explicit hand-off regardless of how slow-lane injection research resolves (M14). |
| DynaCU-Bench | Stays in the AOI repository. OpenRealtime ships a runner and uses it as a functional release gate, not a copied suite (§8). |
| Measurement timing | Post-launch, asynchronous, and explicitly non-blocking. Functional correctness gates the release; comparative claims gate only the claims (§8, §9). |
| Measurement budget | All eight factors are funded and none is cut. The program runs asynchronously alongside and after launch, and never becomes a reason to delay it (§9). |
| Public preview | None. One deliberate v1.0. |
| Transports | WebSocket, in-process WebRTC, and an agent participant for LiveKit or another RTC provider. A WebSocket-only endpoint is not a usable product for most clients (§1.2, §3.6, M13). |

### 11.2 Open to measurement, not to decision

Nothing in this plan is undecided. Three things are unresolved because they are
empirical, and each has a stated way of being resolved:

1. **Whether a ~3B policy model is adequate** for backchannel and turn projection,
   or whether ~8B is needed. Answered by F8, before the implementation is fixed.
2. **Whether slow-continuation injection into a duplex model works** beyond
   explicit hand-off. Answered in M15; a negative result is publishable and the
   binding ships either way.
3. **Whether provider backpressure needs to be a deferral condition at all.**
   Depends on observed token cost in real deployments. The mechanism exists and is
   configurable; the default stays unconstrained until evidence says otherwise.
