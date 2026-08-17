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

See [ADR-0001](adr/0001-go-production-engine.md) for the language decision
and [protocol.md](protocol.md) for event semantics.
