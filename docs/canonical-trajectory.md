# Canonical trajectory and interleaved thinking

## Purpose

OpenRealtime combines two independent mechanisms:

- **Responsiveness:** incremental perception, fixed/event-driven opportunities,
  and revision-safe preparation let useful work begin before a VAD endpoint.
- **Intelligence:** a low-latency continuation and a higher-reasoning
  continuation append successive parts of one agent rollout.

Trigger cadence answers **when can computation advance?** The canonical
trajectory answers **what exact state does the next continuation inherit?** A
faster clock does not create better reasoning, and a stronger slow model does
not remove endpoint or handoff latency. The joint system needs both.

This split follows the continuous-thinking and asynchronous-event treatment in
chapters 4 and 9 of the AI Agent book: the engine resumes ordinary model
generation at safe boundaries as new observations arrive; it does not expose a
model-authored workflow state machine.

The precise concurrency contract is specified in the
[safe-point asynchronous event loop](safe-point-event-loop.md). It is part of
the architecture, not an implementation detail: without versioned safe-point
transactions, fast and slow cannot be said to inherit one trajectory under
real concurrent input.

## The complete cognitive policy

Fast and slow are execution profiles of one agent, not two agents:

```text
system → observation
       → fast reasoning? → fast assistant text? → fast tool proposal?
       → continuation instruction
       → slow reasoning → executable tool call → tool result
       → slow reasoning → slow assistant text?
```

That is the whole model-facing policy. The fast model generates the next
ordinary assistant segment, possibly with a structured proposal. When its
continuation reaches a safe point, the runtime starts the slow continuation.
The slow model continues from the resulting prefix and may use tools. The
reference policy invokes slow unconditionally; there is no hand-written
difficulty classifier or delegation keyword.

Questions, acknowledgements, and answers are ordinary assistant text. Silence
is an empty visible segment. Playback stopping and floor yielding are media
policy. There are no model decisions named `answer`, `ask`, `yield`, `stop`,
`present_slow`, `goal_status`, or `slow_control`.

## Trajectory items

The internal append-only store has these semantic item kinds:

| Kind | Meaning | May cause an external action |
| --- | --- | --- |
| `instruction` | Runtime-supplied system/continuation instruction | No |
| `observation` | User, ASR, tool, or environment evidence | No |
| `reasoning` | Reusable fast or slow working state | No |
| `assistant` | Ordinary assistant content with commit state | Becomes audible only through speech policy |
| `assistant_state` | Prepared/queued/played/cancelled transition | No |
| `repair` | Required/resolved lifecycle for played content invalidated by later canonical evidence | No |
| `tool_proposal` | Structured fast-model working state | **No** |
| `tool_call` | Authorized slow-model request | Yes, through the tool runtime |
| `tool_result` | Terminal result of one `tool_call` | No |

Identity, time, causal parents, source revision, producer profile, and audible
commitment are runtime provenance. Models do not generate these workflow
fields. This is an internal representation, not an OpenAI Realtime event.

## Append invariants

1. Every continuation consumes an immutable, causally valid prefix.
2. Completed output is atomically appended before a later invocation depends
   on it.
3. A proposal, executable call, and result are distinct states. A result can
   reference exactly one preceding executable call and can never satisfy a
   proposal.
4. Proposal and executable-call identifiers cannot collide.
5. Prepared or queued assistant text may be cancelled. Played content is
   immutable and can only be corrected by a later assistant item.
6. A late asynchronous result keeps its causal position and cannot overwrite a
   newer observation.
7. Provider views may differ in native reasoning representation or context
   size, but must preserve capability, action, and audible-history facts.
8. Provider-specific reasoning loss or normalization is declared; cross-model
   latent/KV continuity is never claimed.
9. An invocation instruction and every output item from that invocation enter
   the trajectory in one compare-and-append transaction. A stale prefix
   publishes nothing and exposes no tool action.
10. Source occurrence time and canonical commit time are distinct. Waiting for
    a safe point never falsifies when an external event actually happened.
11. All results for the executable calls emitted by one slow invocation cross
    the asynchronous boundary as one complete identity-checked batch.
12. A superseding observation names an older known source revision and causally
    references that observation. Tool, repair, and media events cannot claim
    observation supersession.
13. A repair may be required only for played assistant content and must name a
    later known canonical observation. Resolution names a distinct committed
    slow assistant item from that repair revision or later. Required and
    resolved transitions are append-only and identity matched.

## Fast continuation and tool awareness

The fast provider receives the common agent/domain policy plus its phase
instruction, the current trajectory projection, the complete capability
manifest, and the real tool definitions. It therefore knows that the agent can
use a capability and knows the schema needed to form a correct request.

Its descriptor grants `propose`, never `execute`, authority. A native model
tool call is validated and stored as `tool_proposal`. No code path sends it to
the tool runtime. Proposal-bearing provider-native state is not retained for
same-provider replay, because replaying it as a pending assistant tool call
could accidentally convert working state into action. Cross-provider adapters
compile a proposal into an explicitly non-executable working-state object.

This avoids the split-brain failure where the fast model falsely says the
agent lacks a capability, while preserving one hard authority boundary. Prompt
instructions encourage the fast model not to invent unknown results; structural
types—not text pattern matching—ensure that its proposals cannot execute.

Supported fast profiles are:

- Local Qwen instruct with thinking disabled or tightly bounded.
- Gemini 3.5 Flash with minimal thinking.

## Slow continuation

The slow provider receives the same trajectory prefix, including fast
assistant content and tool proposals, plus a larger context projection,
medium/high reasoning effort, and `execute` authority. Gemini 3.5 Flash is the
initial slow profile.

Fast output and proposals are provisional working state, not proof. The slow
model independently decides whether to call a tool, and its newly generated
`tool_call` receives a distinct identity. The runtime executes only that call,
appends exactly one result, and resumes the same slow continuation policy from
the extended prefix. No slow advice object is passed back to a fast agent.

The runner uses only two control rules: invoke slow after fast reaches a safe
point, and invoke slow again after an authoritative tool result while a call is
pending. A finite invocation bound is a resource-safety guard, not a semantic
router.

## Registered context controls

Production uses the exact canonical prefix. Two negative controls test which
part of continuity matters without creating another memory owner:

- `content-only` retains portable fast assistant content but removes fast
  reasoning, tool proposals, and opaque native state from the slow provider's
  view.
- `independent` removes all fast-produced items from the slow provider's view,
  while retaining user observations and prior authoritative slow calls,
  results, and assistant history. Fast speech is still published, making this
  the intentionally split-brain experimental control.

Both are pure projections over typed item kind, producer phase, invocation
identity, and assistant-state identity. They never inspect transcript text,
task IDs, or model claims. The canonical store is unchanged. The continuation
runner captures its full version before inference, gives the provider the
declared view, and compare-and-appends output only to that same store version.
Prepared pre-endpoint slow work uses the identical projection, and complete
tool-result batches resume under it. Thus the controls measure information
inheritance; they do not introduce an advice channel or a second agent loop.

## Revision-safe work before the endpoint

Incremental ASR revisions can start an entire fast-to-slow continuation while
speech continues, but an unstable hypothesis must not pollute the canonical
trajectory. The preparation manager therefore applies latest-wins semantics to
one private branch:

1. Hash the complete provider-visible root input: provider profiles,
   instructions, capability/tool schemas, generation policy, portable
   trajectory content, and retained native state.
2. Exclude operational values the models cannot see, such as timestamps, item
   IDs, and revision counters.
3. Keep at most one chain active. A distinct revision cancels the active chain;
   revisions arriving before cancellation reaches a safe point are coalesced.
4. Run fast normally on the private root. Atomically append its reasoning,
   assistant content, and proposal to that private trajectory. That completion
   always makes slow eligible on the exact resulting prefix.
5. A deployment may impose a per-stage minimum start interval before launching
   speculative slow inference. This pacer uses time and cancellation only,
   never model content; a new revision cancels the wait and exact final commit
   bypasses it.
6. Do not attach a tool runtime or speech sink to the private branch. A prepared
   slow call is captured output, not an external action.
7. At endpoint, accept only the completed or in-flight chain whose root hash
   exactly equals the final semantic input hash. Commit itself never waits.
8. When the canonical runner reaches each stage, replay it only if that stage's
   complete request fingerprint—including the actual preceding stage output—
   also matches. Otherwise call the live provider for that stage.
9. Execute a replayed slow call only after it has been appended canonically;
   append its authoritative result and continue slow through the normal live
   provider path.

There is no transcript normalization, keyword routing, fuzzy matching, or
difficulty prediction in acceptance. A one-character model-visible change is a
cache miss. Raw prepared output and reasoning are absent from default telemetry.
The telemetry exposes revision/attempt/stage timing, launch-pacing waits and
bypasses, cancellation disposition, exact replay versus fallback, tokens when
available, and no model content.

This is continuous thinking while listening without speculative side effects.
It is independent of canonical observation admission. The gateway now exposes a
second closed policy: compatibility/default `endpoint-only` admits only the
terminal ASR observation, while opt-in `stable-partial` also admits a changed,
non-empty provider-typed `StableText`. `UnstableText`, transcript keywords, and
model confidence never determine eligibility. A later promoted revision names
the exact prior source revision it supersedes, preserves that causal edge,
requests a provider safe point, and invalidates older unplayed media. An exact
final duplicate does not rerun cognition. Executable effects remain restricted
to tool calls that reach a committed slow-model safe point.

`stable-partial` was implemented after the frozen M8–M10 executable. It is
opt-in and unmeasured; every frozen study population retains canonical
`endpoint-only` observations.

## Cadence and asynchronous events

The timing hierarchy is:

```text
10–20 ms media frames
        ↓
50 ms scheduler opportunities (example condition)
        ↓ coalescing
200 ms stateful ASR provider advances (example condition)
        ↓ changed semantic revisions
private latest-wins fast → optional temporal pacing → slow preparation
        ↓ exact final-root and per-stage replay
canonical fast → canonical slow call → tool/result continuation → TTS
```

A tick is an opportunity, not mandatory stateless reinference. Stateful ASR
advances only when its provider-sized buffer is ready. A fast request starts
only for a changed, non-empty semantic revision. TTS consumes only canonical,
speakable assistant segments. Tool results, interruptions, and other semantic
events may wake the loop without waiting for a periodic tick.

Ticks and unstable hypotheses stay outside the canonical conversation unless a
declared stability/turn policy promotes a response-eligible observation. They
may still drive private latest-wins preparation. Once promoted, concurrent
event sources only enqueue structured events; one event-loop owner drains them
at safe points. Routine events wait. A trusted interrupt classification
requests cooperative cancellation and is committed immediately after the
active continuation reaches its boundary. Priority is never inferred from
words in a transcript.

Pre-endpoint preparation is itself a closed temporal policy, not a routing
decision. Under preparation `continuous`, changed typed ASR revisions may drive
the private latest-wins chain. Under preparation `endpoint-only`, they cannot
invoke either continuation provider privately. This setting does not decide
which observations are canonical; the independent observation policy above does.
Every admitted observation selects the same `fast → slow` canonical transition,
and complete tool-result batches select the same slow-only resumption transition.

For a canonical observation the cognitive transition is `fast → slow`; for a
complete tool-result batch it is `slow`; for a newly required audible repair it
is `slow`; for playback state or repair resolution alone it is empty. Repair
never reruns fast. Those are the only reference routing rules. Event occurrence
metadata is retained even when canonical ordering places the event after the
completed/interrupted model prefix that was active when it occurred.

## Co-located resource admission

Co-location removes network handoffs but introduces contention. Admission is
based on explicit runtime provenance, never prompt content:

| Class | Typical work | Preemptible |
| --- | --- | --- |
| `urgent` | Hard interruption/recovery work | Deployment-specific |
| `interactive` | ASR, final fast fallback, first-audio TTS | Normally no |
| `speculative` | Pre-endpoint fast preparation | Yes |
| `background` | Local higher-reasoning continuation | Yes |

Each request declares an abstract capacity cost. A reserved interactive slice
cannot be consumed by speculative/background work. Queues order by class,
deadline, then FIFO. Preemption is cooperative: cancellation never releases
capacity until the holder acknowledges a provider safe point. Strict priority
can starve background work under sustained interactive saturation, so production
deployments must measure that condition and may add a separately specified
fairness or background reservation policy; semantic routing is not the remedy.

## Speech and commitment

```text
canonical assistant text
      ↓
phrase/clause stability policy
      ↓
incremental TTS
      ↓
prepared → queued → played
```

The slow continuation inherits what has already been heard. It may replace
unplayed content. When a later promoted ASR revision invalidates a branch whose
audio may have crossed the wire, the client playback boundary determines the
disposition: zero played duration remains cancellable, while a positive duration
appends `assistant_state: played` and `repair: required` atomically. A pending
repair injects a fixed runtime correction instruction and invokes slow only from
the latest canonical observation. Its later committed assistant correction is
named by `repair: resolved`. If the correction already exists when playback is
reported, required and resolved may cross one atomic event batch without an
extra continuation. The current live benchmark starts TTS only after a canonical
model safe point; speculative TTS remains a separate experiment.

The persistent gateway makes that replacement rule operational. Fast output is
a bounded spoken micro-turn, not an unconstrained answer queued ahead of slow
reasoning. Fish provider fragments are normalized into fixed 100 ms wire frames
and released at media time. When a slow assistant segment or authoritative
tool call commits, it advances a fast-media epoch: queued fast speech from an
older epoch is discarded and an active fast stream is cooperatively cancelled.
Slow speech is not invalidated. At most the bounded already-sent media prefix
remains, and ordinary playback/truncation events decide what was heard.

This is a consequence of phase authority plus the acoustic commit horizon. It
is not a learned or hand-written semantic decision, and it never searches model
text. The canonical fast assistant item remains in the audit trajectory; its
append-only visibility transitions determine whether later provider projections
include or omit it.

Cancelled-before-playback assistant text is removed from every later provider
projection, including opaque native state from the same invocation that may
embed the cancelled text. Prepared, queued, and played content remains visible;
the operational queued/played transitions themselves do not inflate semantic
fingerprints. Repair transitions are likewise operational; only whether an
obligation remains pending changes the provider-visible request and preparation
fingerprint.

## Provider-boundary compatibility

Different model families cannot share hidden activations or KV caches. The
portable continuity layer is the symbolic trajectory.

- Same provider/family may retain opaque authenticated state only when the API
  explicitly supports safe replay.
- Cross-family continuation carries ordinary reasoning representations,
  assistant text, proposals, calls, and results.
- Foreign text is never labeled as another provider's signed/private thinking.
- Provider-native state is reusable only by the exact same provider and model
  identity; another model in the same family receives the portable trajectory.

Reasoning text is optional. The architecture remains valid when an adapter can
retain only assistant/tool history and timing/token metadata.

## Protocol and observability boundary

The public OpenAI Realtime client/server protocol is unchanged. Canonical
items, preparation attempts, scheduling classes, observation supersession,
repair obligations, and continuation lifecycle are internal. Existing text,
audio, cancellation, truncation, and function-call events carry observable
behavior.

`realtimegateway` implements this projection at `/v1/realtime`: session/audio
events enter the canonical runtime, slow calls leave as standard function-call
events, standard `conversation.item.create` function outputs become one exact
result batch, and the resulting slow continuation leaves as ordinary response
audio/transcript events. Fast/slow phase names, preparation fingerprints, media
epochs, and canonical item IDs do not become public protocol extensions.

Default research telemetry contains model/profile identity, reasoning effort,
source revision, prefix boundary, start/first-event/completion/cancellation
times, token counts when exposed, proposal/call/result causality, audible state,
and hashes. Raw reasoning is opt-in and subject to provider, privacy, consent,
and retention constraints. Internal telemetry must never be serialized as a
made-up OpenAI event.

Stable `api/v1` is also unchanged. The continuation packages remain
experimental; any future downstream public replacement requires `api/v2`.
That version denotes OpenRealtime's Go API, never a version of the OpenAI
Realtime protocol.

See [ADR-0003](adr/0003-canonical-trajectory-continuations.md) for the decision
and [live-cascade.md](live-cascade.md) for the implemented path and measured
evidence.
