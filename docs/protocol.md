# Protocol architecture

OpenRealtime has two deliberately separate representation layers.

1. The external wire protocol is the OpenAI Realtime event protocol. Existing
   client and server messages retain their documented names and JSON shapes.
2. The research trace protocol is an append-only timing envelope around one
   complete wire message. It adds evidence without changing what peers send.

No OpenAI Realtime protocol update is planned or required. The canonical
trajectory does not introduce another external protocol.
It is internal cognitive state compiled into each model provider's supported
message format. Fast/slow continuation, reasoning lifecycle, and tool provenance
may be represented by a few internal research records, but they do not add or
rename OpenAI Realtime client/server event types. Those records belong in a
parallel internal journal, not on the Realtime connection.

Raw reasoning content is not required for wire compatibility or ordinary trace
validation. When a research run retains it, retention must be explicit and
provider-compatible; the default observability surface should record only the
continuation phase, model identity, timing, interruption/resumption, token
counts when available, and trajectory item references or hashes.

`tool_proposal` is likewise internal. A fast provider may emit a native
structured tool-call event so it can express the correct capability and
arguments, but proposal authority maps that event to non-executable trajectory
state. Only a slow provider with execute authority can create the function-call
behavior projected onto existing Realtime events. The public protocol does not
need a separate proposal event or a fast/slow control event.

The current `trace-record-v0.2` schema continues to wrap wire messages only.
Internal continuation telemetry must not be disguised as an OpenAI message
inside that envelope. A future experiment may add fields or records to a
parallel internal journal or introduce a separately versioned trace schema, but
neither choice changes the OpenAI profile registry or wire validators.

The live `realtimebench` report is a separate research artifact, currently
`realtime-benchmark-v0.8`. Its ASR revisions, private fast→slow preparation
attempts, temporal launch-pacing telemetry, semantic fingerprints, exact stage
replay/fallback counts, admission classes, model timings, scoring contract, and
canonical digest are not
serialized inside `trace-record-v0.2` or sent to Realtime clients.

This boundary reuses existing public behavior. The official OpenAI Realtime API
already defines ordinary response lifecycle, output-audio, transcript, and
function-call events; internal model substitution does not need a peer-visible
event. See the official [Realtime API reference](https://platform.openai.com/docs/api-reference/realtime)
and [GPT-Realtime-2 model description](https://developers.openai.com/api/docs/models/gpt-realtime-2).

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

The trace schema is `schemas/trace-record-v0.2.schema.json`. That schema is a
released artifact, so the writer is checked against it rather than merely
alongside it: `trace/schema_test.go` validates freshly written records, the
schema's enumerations against the writer's own constants, and every trace
artifact tracked in this repository.

A record naming no causal parents emits `causal_parent_ids` as an empty array;
the writer normalises an absent list, because a nil slice would serialise to
`null` and the schema requires an array. Five M5 reference traces were generated
before that normalisation and opened with a null instead, so they were
regenerated and the release manifest re-cut. The conformance sweep carries no
exceptions: every trace record this repository ships validates against the
published schema, and any that does not fails the build.

## Audio compatibility

The GA OpenAI Realtime PCM format is mono, signed 16-bit little-endian PCM at
24 kHz. Replay rejects mismatched WAV audio instead of silently resampling it.
Each frame becomes an `input_audio_buffer.append` event with standard base64
audio, followed by `input_audio_buffer.commit`. Transport adapters may support
the other documented G.711 formats, but must declare and test that capability.
