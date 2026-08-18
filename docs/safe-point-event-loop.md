# Safe-point asynchronous event loop

## Why this exists

The environment is asynchronous while a model continuation is locally
synchronous: audio arrives, ASR hypotheses change, playback advances, users
interrupt, and tools finish while a provider is decoding one prefix. The
runtime therefore needs one synchronization rule, not a growing collection of
special cases.

This design applies the event-driven agent model from Chapter 4 of the AI Agent
book: external occurrences become structured events, routine events wait for a
safe point, urgent events request an earlier safe point, and a single event-loop
owner advances the agent over an ordered history. The book's demonstration
uses keyword classification only as a teaching shortcut. OpenRealtime does not
copy it: event priority comes from a trusted acoustic/semantic source and is
never inferred by matching transcript text.

## First-principles invariants

1. **One history.** There is one append-only canonical trajectory for semantic
   observations, model continuations, audible commitments, calls, and results.
2. **One transition owner.** Concurrent sources may enqueue events, but only
   the event loop advances the cognitive state machine. A continuation runner
   appends only as part of that transition.
3. **Prefix validity.** Work computed from trajectory version `v` can publish
   only if the trajectory is still at `v`. Publication is an atomic
   compare-and-append transaction.
4. **Safe-point delivery.** An event becomes model-visible between provider
   continuations, never halfway through an indivisible provider operation.
5. **Occurrence is not commitment.** Source time is retained separately from
   canonical commit time. An event that occurs during decoding may be committed
   after the completed/interrupted prefix at the next safe point.
6. **Typed control.** Priority, authority, and eligibility are structured
   runtime facts. Model text cannot grant authority, mark itself urgent, or
   choose a hidden workflow state.
7. **Side effects follow commitment.** A tool executes only after an
   authoritative slow `tool_call` has committed. Speculative and fast calls are
   `tool_proposal` items and have no result channel.
8. **Acoustic truth is semantic truth.** Played assistant content is immutable.
   Content cancelled before playback is absent from later provider context,
   including opaque native state that may contain it.

## Events, opportunities, and trajectory items

These concepts are deliberately separate:

| Concept | Examples | Model-visible? | Persistent location |
| --- | --- | --- | --- |
| Media input | 10–20 ms PCM/G.711 frame | Through perception | media/causal trace |
| Opportunity | 50/100/200 ms tick, revision wakeup | No | scheduler trace |
| Private revision | unstable ASR hypothesis used for preparation | Only in private branch | preparation trace |
| Canonical event | stable observation, directed interruption, complete tool result batch, playback transition | After safe-point commit | canonical trajectory plus event provenance |
| Continuation output | reasoning, assistant content, proposal, call | Yes to later continuations | canonical trajectory |

A tick is therefore not an event that must enter conversation history and not
a command to restart the pipeline. It lets persistent ASR consume available
audio and lets revision-safe preparation advance if there is new semantic
evidence. A stable partial or endpoint can be submitted as a canonical
observation according to the declared turn/stability policy. Tool results and
directed interruptions wake the event loop without waiting for a tick.

## The state machine

```text
concurrent producers
  audio/ASR ─────┐
  playback ──────┼── enqueue typed events ──► pending queue
  tool runtime ──┘                                  │
                                                    ▼
                                             IDLE / SAFE POINT
                                                    │ drain snapshot
                                                    ▼
                                      atomic event-batch commit (CAS)
                                                    │
                          observation? ─────────────┼──── tool result?
                              │                     │          │
                              ▼                     │          ▼
                     fast continuation              │   slow continuation
                    atomic commit (CAS)              │   atomic commit (CAS)
                              │                      │          │
                              └────► slow continuation ◄────────┘
                                     atomic commit (CAS)
                                              │
                                  call? ──────┴────── no call
                                    │                    │
                           dispatch committed call       ▼
                           result returns as event    SAFE POINT
```

There is no `answer/ask/yield/stop/present_slow` cognitive router. For a
response-eligible observation, the policy is simply fast then one slow
continuation. For a completed tool-result event, it is slow continuation only.
For a playback-state event, no model is invoked. Ordinary assistant text
expresses an answer, question, or acknowledgement; the media controller owns
floor yielding and stopping.

## One safe-point transition

Assume the canonical trajectory is `T_v` and the pending queue begins with
events `B`.

1. Snapshot and remove `B` in arrival order. Events arriving afterward remain
   pending for the next transition.
2. Compile `B` into typed items. Preserve each event's source, channel,
   occurrence time, correlation, and causal parents.
3. Atomically append the entire batch only if the store is still at version
   `v`. A conflict requeues `B`; invalid input is rejected without a partial
   append.
4. Run the cognitive policy from the committed prefix. Each provider request
   receives an immutable snapshot plus a virtual current phase instruction.
5. Buffer streamed model output until the provider safe point. Atomically
   commit the instruction and all valid output only if the source prefix is
   still current. A stale completion is discarded and cannot expose calls.
6. Publish committed assistant segments to speech and committed slow calls to
   the tool orchestrator. These outputs return through the event queue rather
   than mutating the trajectory from callback goroutines.

The compare-and-append operation is the linearization point. Cancellation
alone is not sufficient: a provider may ignore cancellation or return late,
so prefix validation is still required.

## Routine events and interruption

- A **routine** event waits while a continuation is active. The continuation
  commits at its natural terminal boundary; the queued event is next.
- An **interrupt** event is assigned by a trusted source such as directed-speech
  classification or an explicit client action. Submission cancels the active
  continuation context. The provider retains any completed reasoning/assistant
  prefix it can return, suppresses incomplete tool calls, and reaches a safe
  point. The interrupt observation then commits and cognition resumes from the
  extended trajectory.

Priority is intentionally absent from model-visible trajectory content. It is
recorded in the scheduler/causal trace for reproducibility. Event text is never
searched for words such as “stop,” and a model cannot promote its own output.

## Fast and slow are one continuation

Both profiles receive one common agent/domain instruction, one capability
manifest, and one tool schema set. Their only structural difference is compute
profile and authority:

| Profile | Initial candidates | Reasoning | Context | Tool authority |
| --- | --- | --- | --- | --- |
| Local reflex | Qwen instruct, thinking disabled/bounded | minimal | declared provider projection | propose |
| Hosted reflex | Gemini 3.5 Flash | minimal | declared provider projection | propose |
| Slow | Gemini 3.5 Flash | medium/high | larger declared projection | execute |

The fast model emits the smallest truthful segment that makes immediate
progress. Slow consumes that exact committed prefix. It appends only missing
content, an explicit correction, or an authoritative call; it does not send
advice to a different agent. Tool completion resumes slow directly, so fast is
not rerun and completed work is not summarized or reconstructed.

Different providers cannot share KV caches or hidden activations. Same-provider
opaque state is replayed only when its provider/model identity matches and its
assistant output was not cancelled. Cross-provider continuity is the portable
trajectory and must be described as symbolic continuity.

## Asynchronous tools

Initiation and completion are separate events:

1. Slow atomically commits all calls from one invocation.
2. The tool sink dispatches those committed calls.
3. Tools may execute concurrently outside the event-loop owner.
4. Results for one invocation may arrive in any order, but cross the semantic
   boundary as one complete batch.
5. The runtime verifies an exact call-ID/name match, orders results by original
   call order, and commits the batch atomically.
6. The next transition invokes slow from the call-plus-result prefix.

Partial batches, duplicate IDs, result/name substitutions, results for fast
proposals, and stale commits are rejected. Streaming tool progress belongs in
an operational trace until a separately specified semantic progress contract
exists.

Dispatch is idempotent by canonical call ID. A transport failure after call
commit may be retried, but it may not mint a replacement call or duplicate an
irreversible effect.

## Speech synchronization

Model commitment and playback commitment are different boundaries. A completed
assistant item begins `prepared`; TTS may move it through `queued` to `played`.
Prepared or queued content can be cancelled. Played content cannot be removed
from history and later reasoning must correct it explicitly.

Provider compilers resolve the append-only playback transitions before each
request. Cancelled-before-playback assistant text and provider-native state from
that invocation are omitted. Queue/played bookkeeping does not change a
preparation fingerprint because both remain model-visible audible history.

The gateway additionally bounds how far media can get ahead of this state.
Fish fragments are coalesced into 100 ms wire frames and paced against their
encoded duration. A committed slow assistant or tool call invalidates queued
and active fast-only media without cancelling slow media. The already emitted
prefix is handled by normal Realtime playback/truncation events and is never
rewritten. This prevents an authoritative tool result from waiting behind a
large provisional client-side audio buffer without introducing a semantic
router.

## Latency path

The principled latency optimizations are changes in when safe work begins and
how much state is reused:

- persistent ASR with provider-sized buffering rather than full-prefix restart;
- fixed opportunities coalesced to changed semantic revisions;
- latest-wins private fast→slow preparation before endpoint;
- exact fingerprint replay at commit, never fuzzy acceptance;
- one stable system/tool prefix per invocation rather than replaying every
  historical phase instruction;
- immediate TTS after a committed fast segment while slow continues;
- direct slow resumption after tool results;
- co-location with explicit interactive capacity reservation and measured
  contention.

Shorter ticks alone can increase discarded work and contention. Every cadence
condition must report opportunities, actual provider advances, cancellations,
queue time, and task/interaction quality.

## Public protocol boundary

This is an internal runtime abstraction. The OpenAI Realtime client/server event
names and schemas are unchanged. Existing audio, transcript, response,
function-call, cancellation, buffer-clear, and item-truncation events expose the
observable behavior. Canonical items and raw reasoning are not invented as new
wire messages. A compact internal lifecycle journal may be versioned later,
but it remains separate from the OpenAI-compatible connection.

## Implementation map

- `trajectory`: immutable items, visibility projection, exact tool-result
  matching, and versioned atomic append.
- `eventloop`: structured ingress queue, routine/interrupt handling, one active
  transition, an explicit queue bound with reserved interrupt capacity, event
  provenance, and atomic batch commit. A full class allocation returns
  backpressure; it never silently drops or overwrites an event.
- `continuation`: provider-safe buffering and stale-prefix rejection.
- `interleave`: common policy/tool view, fast proposal authority, slow execute
  authority, and the observation/tool-result cognitive transition.
- `preparation`: private latest-wins work and exact provider-visible
  fingerprints.
- `speech`: prepared/queued/played/cancelled media commitment.
- `realtimegateway`: standard Realtime transport, persistent acoustic/media
  state, exact external result batching, phase-authority speech supersession,
  and 100 ms paced Fish audio projection.

The `cognition` M4 goal/update abstraction remains a benchmark control and is
not part of this target state machine.
