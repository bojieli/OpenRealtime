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

## Microturn scheduling and planning state

M2 separates scheduling from candidate lifecycle. Fixed and revision-event
schedulers consume monotonic observations and emit named opportunities carrying
the newest revision actually available at that instant. The revision ledger
protects stable prefixes; the candidate ledger requires explicit replacement
or cancellation and refuses candidates tied to unknown evidence. Experiment
actions record openings, suppressions, requests, preparation, supersession,
cancellation, endpoints, and playback markers in one deterministic order.

Only text candidates are prepared before the endpoint in M2. Audio commitment,
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

## Fast and slow cognition

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
and [protocol.md](protocol.md) for event semantics.
