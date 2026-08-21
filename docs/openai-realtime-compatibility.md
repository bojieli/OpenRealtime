# OpenAI Realtime protocol compatibility

- Source revision: `openai/openai-openapi@2186421dca0cca7c1e67caa7739005e8b1ccc4dd`
- Source SHA-256: `542299d304cdeb78deff4172b3790d52c7e7e75fb2b517e9c2787c52f1424acc`
- Retrieved: 2026-08-17
- Official API reference: <https://platform.openai.com/docs/api-reference/realtime>
- Generated definitions: 133 across four profiles and two directions
- Unique wire event names: 66

This document describes schema and codec coverage. The server implements a
strict, schema-validated subset rather than every decoded Realtime feature;
an unsupported standard client event returns a standard error and the session
continues.

Every event in both directions is validated against this pinned schema on every
session by default (`-validate-wire`). A compatibility claim that is not
continuously checked is a compatibility claim that decays, so the check runs in
production rather than only in tests.

The OpenRealtime Protocol extends this surface additively and only after
negotiation; see [protocol/openrealtime-1.md](protocol/openrealtime-1.md). A
client that never mentions it gets an ordinary Realtime session, and the server
never volunteers the key.

## Internal synchronization does not change the wire

The canonical trajectory and the safe-point event loop add no client or server
event types. Asynchronous transcription, response cancellation,
output-buffer clearing, item truncation, ordinary function calls/results, and
audio/transcript deltas already provide the observable wire behavior. Fast and slow phase identity, reasoning
lifecycle, event priority, trajectory versions, observation supersession, and
audible-repair obligations remain internal.

In particular, an input transcription may complete independently of response
events, so the internal runtime correlates it by item/event identity instead of
assuming arrival order. When output is interrupted, OpenRealtime projects the
actual playback boundary through the existing cancel/clear/truncate lifecycle;
content cancelled before playback is excluded from later internal provider
context. If a later canonical ASR revision invalidates content the client reports
as played, the internal trajectory records a typed repair obligation and keeps
it active until a committed slow assistant item supplies the correction. No
repair event is added to the OpenAI Realtime wire. This preserves compatibility
while keeping acoustic and cognitive history synchronized.

## Implemented gateway subset

The `gateway` package, served by `openrealtime serve`, serves a persistent
WebSocket using only standard events. Output is **audio or text**, one of them,
selected by `output_modalities`. A text session is the same conversation with a
different output boundary: same observations, same two cognition providers,
same rollout, same commitment policy — nothing is synthesised and nothing is
paced, because a turn nobody hears takes no time and cannot be talked over. It
is what a computer-use client wants, and refusing it made this server unusable
for exactly the clients the extension exists for.

It accepts eight of the eleven GA Realtime client events: `session.update`, `input_audio_buffer.append`,
`input_audio_buffer.clear`, `output_audio_buffer.clear`,
`conversation.item.create` for both function outputs and typed messages,
`conversation.item.truncate`, `response.create`, and `response.cancel`.

The other three are refused, and the refusal says which reason applies rather
than reporting "unsupported event" — a client author can act on the first,
and cannot on the second.

| Event | Why |
| --- | --- |
| `input_audio_buffer.commit` | server VAD owns input commitment here, so the buffer commits at the endpoint and an explicit commit would have nothing to do |
| `conversation.item.delete` | the conversation is an append-only trajectory: content that reached the world cannot be un-reached, so it is superseded rather than removed |
| `conversation.item.retrieve` | not served; a client that negotiated `observations` receives what the agent perceived as it happens |

The session continues after any of them. A refusal is an answer, not a
disconnection.

The server emits the standard session update, error, speech start/stop,
conversation item/transcription, response, audio/transcript, function-call,
truncation, and buffer-cleared lifecycles needed by the pinned τ OpenAI
adapter. Every accepted client event and emitted server event is checked by the
pinned validator in the production command. Fast/slow authority, reasoning,
trajectory versions, preparation, and media epochs remain internal.

The `continuous` versus `endpoint-only` preparation setting is also internal.
It controls whether typed partial transcripts may start private work; standard
server VAD and transcription events still define the public lifecycle, and the
same response/function-call events expose the post-endpoint result.

Canonical observation policy is separate. Its compatibility default,
`endpoint-only`, admits only the final transcript. The opt-in post-freeze
`stable-partial` mode may admit changed non-empty provider-typed `StableText`
before endpoint, without promoting `UnstableText`. Later promoted revisions
carry typed supersession provenance and cancel older work at provider/media safe
points. The frozen benchmark executable uses `endpoint-only`; stable-partial
behavior is not part of its reported evidence.

External tools use the ordinary protocol sequence: the slow continuation emits
standard function-call response items; the client sends one
`function_call_output` item per call followed by `response.create`; the gateway
validates the complete call-ID/name batch, commits it atomically, and resumes
slow without rerunning fast. Agent speech uses ordinary output-audio and
transcript deltas. Local Fish fragments are emitted as real-time-paced 100 ms
wire frames so server cancellation remains meaningful at the acoustic commit
horizon.

The pinned official τ horizontal provider suite passes 12/12 selected OpenAI
cases against this live endpoint. This validates the implemented subset; it
does not imply MCP, DTMF, translation, manual input commits, every item CRUD
operation, or full parity with the hosted service.

## GA Realtime client events (11)

`session.update`, `input_audio_buffer.append`, `input_audio_buffer.commit`,
`input_audio_buffer.clear`, `conversation.item.create`,
`conversation.item.retrieve`, `conversation.item.truncate`,
`conversation.item.delete`, `response.create`, `response.cancel`, and
`output_audio_buffer.clear`.

## GA Realtime server events (46)

- Session and errors: `error`, `session.created`, `session.updated`,
  `conversation.created`, and `rate_limits.updated`.
- Conversation items: `conversation.item.added`, `conversation.item.created`,
  `conversation.item.done`, `conversation.item.retrieved`,
  `conversation.item.truncated`, and `conversation.item.deleted`.
- Input audio: `input_audio_buffer.committed`, `input_audio_buffer.cleared`,
  `input_audio_buffer.speech_started`, `input_audio_buffer.speech_stopped`,
  `input_audio_buffer.timeout_triggered`, and
  `input_audio_buffer.dtmf_event_received`.
- Input transcription: `conversation.item.input_audio_transcription.delta`,
  `.segment`, `.completed`, and `.failed`.
- Output buffer: `output_audio_buffer.started`, `.stopped`, and `.cleared`.
- Response lifecycle: `response.created`, `response.done`,
  `response.output_item.added`, `response.output_item.done`,
  `response.content_part.added`, and `response.content_part.done`.
- Streaming output: `response.output_text.delta`,
  `response.output_text.done`, `response.output_audio.delta`,
  `response.output_audio.done`, `response.output_audio_transcript.delta`, and
  `response.output_audio_transcript.done`.
- Function calls: `response.function_call_arguments.delta` and `.done`.
- MCP discovery and execution: `mcp_list_tools.in_progress`, `.completed`,
  `.failed`, `response.mcp_call_arguments.delta`, `.done`,
  `response.mcp_call.in_progress`, `.completed`, and `.failed`.

## GA transcription events

The client profile covers `input_audio_buffer.append`, `.commit`, `.clear`, and
`transcription_session.update`. The server profile covers `error`, input-buffer
commit/clear and speech start/stop, all four input-transcription result events,
and `transcription_session.updated`.

## GA translation events

Client events are `session.update`, `session.input_audio_buffer.append`, and
`session.close`. Server events are `error`, `session.created`,
`session.updated`, `session.closed`, `session.input_transcript.delta`,
`session.output_transcript.delta`, and `session.output_audio.delta`.

M5 exercises this entire non-error translation lifecycle with 200 ms PCM16
frames in 90 validated traces. Error coverage remains schema-conformance based:
the deterministic reference provider does not fabricate server failures.

## Legacy beta

The beta profile covers 12 client and 40 server definitions from the same
pinned official specification, including `transcription_session.created` and
the beta-specific nested resource shapes. Beta validation is opt-in; a GA event
is never silently checked against a beta schema.

## Conformance rules

- JSON field names and event `type` strings are unchanged.
- Optional client `event_id` values are retained. Required server `event_id`
  values are enforced by their event schemas.
- Required and optional status, usage, audio format, turn detection, tool, MCP,
  conversation item, content part, and response fields come directly from the
  full generated schema closure rather than a hand-maintained subset.
- Unknown JSON fields survive decoding and proxying. Unknown event names fail
  strict conformance until the pinned specification is updated.
- Direction and profile are part of validation, preventing a client event from
  being accepted on a server path merely because it is valid JSON.

Run `./scripts/check_openai_realtime_spec.sh` to refetch the pinned source,
verify its cryptographic hash, regenerate into a temporary directory, and
compare both committed generated outputs byte-for-byte.

Run `openrealtime conformance protocol` (or `./scripts/check.sh`) to
audit all registry/schema links, compile the entire nested closure, confirm the
profile/direction counts, and exercise unknown-type, wrong-direction,
wrong-profile, and missing-required-field rejection.
