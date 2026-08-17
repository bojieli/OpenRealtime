# Protocol architecture

OpenRealtime has two deliberately separate protocols.

1. The external wire protocol is the OpenAI Realtime event protocol. Existing
   client and server messages retain their documented names and JSON shapes.
2. The research trace protocol is an append-only timing envelope around one
   complete wire message. It adds evidence without changing what peers send.

## Wire events

`protocol/openai/openai-realtime-events.schema.json` is mechanically extracted
from a pinned revision of OpenAI's official OpenAPI 3.1 specification. The Go
registry and JSON Schema bundle are generated in the same operation, so an
event constant cannot silently diverge from its schema.

The decoder always preserves the complete JSON object, including fields unknown
to this revision. Strict conformance then checks all required fields, constant
event names, nested unions, enums, limits, and profile/direction membership.
This gives forward-compatible proxy behavior without claiming that an unknown
event has passed a schema it was never tested against.

Profiles are explicit because identical event names can have different nested
session shapes:

- `realtime`: current GA conversation events.
- `transcription`: current GA transcription-only events.
- `translation`: continuous Realtime translation events.
- `beta`: legacy beta event shapes retained for compatibility testing.

The full human-readable inventory is in
[openai-realtime-compatibility.md](openai-realtime-compatibility.md). The
machine-readable inventory is produced by `openrealtime protocol inventory`.

## Timed trace envelope

Every `trace-record-v0.2` record contains:

| Field | Meaning |
| --- | --- |
| `schema_version` | Trace schema version, currently `0.2.0` |
| `trace_id` | Unique record identity within the trace |
| `session_id` | Identity shared by every record in one trace stream |
| `sequence` | Contiguous zero-based serialization order |
| `monotonic_ns` | Time from the injected monotonic clock |
| `direction` | `client` or `server` |
| `profile` | Schema profile used for wire validation |
| `causal_parent_ids` | Zero or more earlier direct causes |
| `message` | Complete OpenAI Realtime JSON event |

Rules spanning records are enforced by Go rather than per-record JSON Schema:

1. A trace contains one session and is append-only.
2. Sequence starts at zero and has no gap or duplicate.
3. Monotonic time never decreases; equal timestamps are permitted.
4. Every causal parent appears earlier in the same trace.
5. Trace IDs are unique. Wire `event_id` remains independent.
6. Capture or media time is derived from absolute sample offset. It is not wall
   time and cannot be inferred from serialization order alone.

The trace schema is `schemas/trace-record-v0.2.schema.json`.

## Audio compatibility

The GA OpenAI Realtime PCM format is mono, signed 16-bit little-endian PCM at
24 kHz. Replay rejects mismatched WAV audio instead of silently resampling it.
Each frame becomes an `input_audio_buffer.append` event with standard base64
audio, followed by `input_audio_buffer.commit`. Transport adapters may support
the other documented G.711 formats, but must declare and test that capability.
