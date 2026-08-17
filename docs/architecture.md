# M0 architecture

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

M0 contains the complete schema-level event boundary but no ASR, language
model, speech synthesis, live WebSocket/WebRTC/SIP transport, or device adapter.
M1 adds the endpointed component pipeline and live transport state machines.
Native and hosted provider adapters remain optional; the open reference
condition cannot require an API key.

See [ADR-0001](adr/0001-go-production-engine.md) for the language decision
and [protocol.md](protocol.md) for event semantics.
