# Reference-engine architecture

OpenRealtime begins with a compiled Go engine. Go provides efficient streaming
I/O, explicit cancellation, a mature race detector, and deployable static
binaries while preserving a fast research loop. The media path is bounded by
frame size and uses absolute sample timing.

```text
24 kHz PCM WAV -> frame player -> OpenAI client-event JSONL
                        |                    |
                  VirtualClock       official JSON Schema
                        |
                        +--------> timed causal trace
```

## Time model

Components depend on an injected `Clock`. Live code uses the operating
system’s monotonic clock; replay uses `VirtualClock`. Latency arithmetic never
uses calendar time. Capture time is payload evidence and processing time is the
event envelope timestamp. They may differ in live traces.

The WAV player derives every frame timestamp from its absolute sample offset:

```text
timestamp_ns = start_ns + floor(sample_offset * 1_000_000_000 / sample_rate)
```

It does not repeatedly add a rounded frame duration, so rounding cannot
accumulate into scheduling drift.

## Trace boundary

OpenAI-compatible messages remain unchanged at the wire boundary. An append-only
OpenRealtime trace separately records the profile, direction, monotonic time,
causal parents, and complete message. This prevents research metadata from
breaking existing Realtime clients.

The generated OpenAI bundle checks every documented field and nested resource.
The trace state machine checks facts spanning records: one session per stream,
contiguous sequence, nondecreasing monotonic time, globally unique IDs, and
parents preceding children. Unknown future OpenAI event types remain decodable
for lossless proxying but cannot be falsely reported as conformant.

## Current and deferred boundaries

M1 adds provider-neutral, context-cancellable perception, cognition, and speech
contracts. The endpointed B0 control streams fixture frames into perception but
does not create a response until finalization after input commit. Its seeded
timing simulator assigns each component and queue duration explicitly and
reconciles their sum with an observed output-playback marker. A self-contained
timeline renders client and server events from the same validated trace.

The reference adapters are deterministic instrumentation doubles, not model
quality baselines. Live ASR, language, speech, WebSocket/WebRTC/SIP transport,
and device adapters remain deferred. Native and hosted adapters are optional;
the open reference condition cannot require an API key.

## Target realtime abstraction

The experimental architecture combines two independent mechanisms:

1. **Microturn execution for responsiveness.** Continuous media and incremental
   perception create fixed or event-driven opportunities to advance cognition
   and speech without waiting for a VAD endpoint.
2. **Canonical-trajectory continuation for intelligence.** A low-latency model
   and a higher-reasoning model append successive reasoning, assistant, and
   tool items to one trajectory. The slow phase continues the fast phase; it
   is not a second agent sending advice.

```text
audio frames → incremental ASR ─┐
tool/user/system events ────────┼→ scheduler and safe-point event loop
playback/interrupt events ──────┘                 │
                                                  ▼
                                      canonical trajectory
                                      ├─ observations
                                      ├─ reasoning
                                      ├─ assistant content
                                      ├─ non-executable tool proposals
                                      └─ executable tool calls/results
                                           │           │
                                     fast append   slow append
                                           └─────┬─────┘
                                                 ▼
                                      speech plan and commit
```

The canonical trajectory is the cognitive source of truth; the timed causal
trace is the experimental evidence record. A trajectory compiler supplies a
causally valid prefix to each provider and records which items were representable
in that provider's chat/reasoning format. The external OpenAI-compatible wire
protocol remains unchanged.

### Synchronization boundary

The event loop is the only owner that advances semantic state. Audio, ASR,
playback, and tool workers are concurrent producers; they enqueue typed events
and never mutate conversation history from callbacks. At each safe point the
loop drains an arrival-ordered batch, appends it atomically, and applies one
content-independent transition: observation runs fast then slow, tool results
resume slow, and playback state invokes neither model.

Every provider call captures trajectory version `v`. Its phase instruction and
completed output publish in one compare-and-append transaction only while `v`
is current. An interrupt is typed by a trusted acoustic/semantic source and
requests cooperative cancellation; no transcript keyword or model-authored
status controls scheduling. Tools execute only after a slow call commits and
return as a complete identity-matched event batch. Source occurrence time is
kept separately from commit time so safe-point waiting stays observable.

This is the synchronization layer derived from Chapter 4's asynchronous event
model. The complete contract and state machine are in
[safe-point-event-loop.md](safe-point-event-loop.md) and ADR-0004.

## Microturn scheduling and planning state

M2 separates scheduling from candidate lifecycle. Fixed and revision-event
schedulers consume monotonic observations and emit named opportunities carrying
the newest revision actually available at that instant. The revision ledger
protects stable prefixes; the candidate ledger requires explicit replacement
or cancellation and refuses candidates tied to unknown evidence. Experiment
actions record openings, suppressions, requests, preparation, supersession,
cancellation, endpoints, and playback markers in one deterministic order.

A scheduler opening is not synonymous with a provider call. In the live path,
audio ingestion and ASR state are persistent; a 50 ms opportunity contributes
to a stateful 200 ms provider buffer, and an opportunity with no new usable
evidence does not start a language model. Recognition revisions, tool results,
slow continuation, and interruption events may wake the loop between fixed
ticks. Reports separate opportunities, provider advances, semantic revisions,
and model preparation attempts.

The live preparation manager generalizes M2's text candidate rule to one
private continuation chain. A changed revision runs fast and then slow over the
same private append-only prefix; newer revisions cancel/coalesce older chains.
Nothing in that branch is audible or executable. At endpoint, the root and
each consumed stage must match their complete provider-visible semantic
fingerprints before their captured events are replayed through the ordinary
canonical runner. Any mismatch falls back to the live provider. The manager
performs no fuzzy transcript match or semantic difficulty routing.
Speculative TTS is still outside this measured path. Audio commitment,
continuous input during output, interruption, and repair remain M3 boundaries.

## Speech commitment and duplex state

M3 streams 20 ms speech chunks into a concurrency-safe commit controller. The
controller distinguishes prepared, queued, and observed-played samples, bounds
both lookahead regions, and treats played samples as immutable history. A
directed interruption yields; listener backchannels and side speech do not
stop playback. Candidate invalidation after playback creates a mandatory repair
obligation that prevents clean closure until recorded.

OpenAI cancellation, output-buffer clearing, and conversation-item truncation
are emitted at the observed playback boundary. Input append events remain
legal while the turn state is `SYSTEM_SPEAKING`, keeping the media path duplex.

## Historical M4 fast/slow baseline

M4 gives each deliberation task a goal ID, revision ID, deadline, and cancellable
context. Foreground decisions are bounded structured actions with explicit
progress claims. Slow updates form a monotonic stream and reach coordinator
state only while their exact goal revision remains current. Completion,
failure, cancellation, deadline misses, and stale rejection are recorded rather
than inferred from logs.

Providers that honor context cancellation stop promptly. Providers that do not
are still safe: an old callback can finish its goroutine but cannot overwrite a
replacement goal. The open reference workload assigns symbolic quality and
compute units so orchestration accounting stays independent of hosted models.

This M4 abstraction is retained for reproducibility and as an independent
fast/slow control. It is not the target design for continuous thinking because
`FastDecision` and `DeliberationUpdate` describe two information channels.

## Canonical trajectory and interleaved thinking

The target experimental interface replaces foreground decisions and slow advice
with ordinary trajectory continuations:

```text
system → observation → fast reasoning → fast assistant content/tool proposal
       → continuation instruction → slow reasoning → executable tool call
       → tool result → slow reasoning → slow assistant content
```

Every completed item is appended before a later invocation consumes it. Fast
assistant content is marked prepared, queued, or played through the existing
speech commitment controller, so the slow continuation knows which statements
are still replaceable and which require an explicit correction.

The fast phase may use a local Qwen instruct model with thinking disabled or
Gemini 3.5 Flash with minimal thinking. The initial slow phase uses Gemini 3.5
Flash with medium or high thinking. Both receive the same capability manifest
and real tool definitions as well as the same agent/domain system policy.
Fast-native tool calls become `tool_proposal` items
and cannot execute; only slow-native calls become `tool_call` items. This gives
the fast model enough schema knowledge to avoid capability denial without
creating a second action authority.

Same-family continuation may preserve provider-native reasoning where supported.
Cross-family continuation cannot transfer hidden state or KV cache; it carries
a symbolic reasoning representation and exact assistant/tool history through
the trajectory compiler. Adapters must not pass foreign text as provider-signed
thinking or silently claim latent continuity.

New observations are consumed at safe points. Routine events queue until a
reasoning or tool boundary; urgent interruption cancels current decoding,
retains its completed prefix, appends the observation, and resumes from the
extended trajectory. Actual tool calls remain subject to authority and
idempotency policies independent of model latency. A tool result can satisfy
only a distinct, preceding executable call and never a fast proposal.

Assistant playback state is also append-only. Provider projections omit
assistant content cancelled before playback and invalidate opaque state from
that invocation, preventing unheard speech from reappearing as shared memory.
Played content remains immutable and can only be corrected by a later segment.

This continuation contract is experimental. Stable `api/v1` continues to expose
the historical five provider roles; replacing them requires a versioned
`api/v2` design and migration guide.

## Live co-located model path

The implemented experimental path places stateful Qwen3-ASR 0.6B, a
Qwen3-30B-A3B-FP8 fast model, and streaming Fish Audio S2-Pro on one 96 GB GPU;
Gemini 3.5 Flash provides hosted high-reasoning slow continuation. Changed ASR
revisions can prepare the heterogeneous Qwen→Gemini chain before endpoint, and
an exact final match replays both stages before the canonical tool/result
continuation. An explicit admission governor reserves interactive capacity for
perception, final fast fallback, and TTS, while fast preparation is speculative
and a local slow model would be background work. Scheduling class and cost come
from deployment provenance, never transcript content.

A generic per-stage temporal pacer can limit speculative slow launch frequency.
It does not decide whether slow reasoning is semantically needed: every fast
completion still makes slow eligible. The pacer observes only monotonic time,
stage identity, cancellation, and exact commit. Superseded waits end before a
provider call, while exact commit bypasses the remaining delay so the final
canonical continuation is not held behind a speculative cost budget. The
default interval is zero and preserves immediate continuation.

Preemption is cooperative. Cancelling a lease requests a provider safe point
but does not free capacity until the holder returns. Queue ordering is class,
then deadline, then FIFO. Strict priority protects interaction but may starve
background work under sustained load; a fairness policy is a separate,
measurable deployment choice.

Co-location is a hypothesis, not a guarantee: removing network handoffs can
reduce latency while shared memory bandwidth and compute can increase tail
latency. Endpointed, microturn, co-located, split-process, and partially hosted
conditions must therefore use the same workload and report quality alongside
timing.

The first passed integration artifact and its limitations are documented in
[live-cascade.md](live-cascade.md). It establishes wiring and authority
invariants, not parity with native realtime or interaction models.

## Translation, release, and stable boundary

M5 adds the distinct OpenAI Translation lifecycle with 200 ms input/output
frames and append-only transcript deltas, plus a GA Realtime rapid-game path.
M6 aggregates milestone reports only within named comparability groups and
cryptographically inventories the benchmark release.

M7 places the downstream contract in `api/v1`. The original `engine` and
experiment packages remain free to evolve; versioned adapters clone data across
that boundary. The stable conformance runner audits all protocol definitions
and actively probes provider cancellation, ordering, continuity, finality, and
terminal-state invariants. A breaking stable change moves to `api/v2`.

See [ADR-0001](adr/0001-go-production-engine.md) for the language decision
and [protocol.md](protocol.md) for event semantics. The experimental cognitive
boundary is specified in
[canonical-trajectory.md](canonical-trajectory.md) and accepted in
[ADR-0003](adr/0003-canonical-trajectory-continuations.md). Its asynchronous
single-owner synchronization is accepted in
[ADR-0004](adr/0004-safe-point-asynchronous-event-loop.md).
