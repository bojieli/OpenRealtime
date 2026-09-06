# OpenAI Realtime protocol compatibility

OpenRealtime implements a **schema-validated subset** of the OpenAI Realtime
API. The published `@openai/agents-realtime` SDK is tested through a tool-using
session over WebSocket and WebRTC. This does not establish full feature parity
with the hosted service or compatibility with every client configuration.

Use this page to check an application's required events and settings before
migrating it. Start with the [SDK walkthrough](../examples/sdk-client/README.md)
for a runnable integration and [Transports](transports.md) for media setup.

## Compatibility at a glance

| Surface | Current behavior |
| --- | --- |
| Connection | Persistent WebSocket; WebRTC through an adapter |
| Output | Audio with transcript, or text, selected by `output_modalities` |
| Input | Audio and supported conversation messages containing text/images |
| Tools | Standard function-call items and client function-call outputs |
| Turn detection | Server VAD, or explicit client control where the selected binding supports it |
| Item deletion/retrieval | Not served; see the supported event list below |
| Session settings | Some values are refused by field name while the rest of the update applies |
| Video and observations | Require negotiated OpenRealtime extensions and a capable deployment |

## Pinned schema

- Source revision: `openai/openai-openapi@2186421dca0cca7c1e67caa7739005e8b1ccc4dd`
- Source SHA-256: `542299d304cdeb78deff4172b3790d52c7e7e75fb2b517e9c2787c52f1424acc`
- Retrieved: 2026-08-17
- Official API reference: <https://platform.openai.com/docs/api-reference/realtime>
- Generated definitions: 133 across four profiles and two directions
- Unique wire event names: 66

The registry includes decoded definitions that the gateway does not implement.
Schema coverage and executable feature coverage are different: a recognized but
unsupported client event receives a standard error and the session continues.
Both directions are validated against the pinned schema by default
(`-validate-wire`).

The [OpenRealtime extension](protocol/openrealtime-1.md) activates through
negotiation. A base-only client is not sent extension fields or events.

## Checked against OpenAI's own client

The SDK checks exercise session configuration, function calls, returned tool
results, response completion, and audio/transcript output. The local test is:

```bash
go test -v ./examples/sdk-client/
```

Node and the installed npm dependencies are required; WebRTC also requires
Chromium. Local checks can skip unavailable integrations. The
[release matrix](release-validation.md) distinguishes those skips from passing
provisioned checks.

### Unsupported session settings

A partially supported `session.update` applies accepted fields, sends
`session.updated`, then sends errors naming refused fields. For example, an
unsupported `semantic_vad` request does not discard instructions and tools
included in the same update:

```json
{
  "type": "error",
  "error": {
    "type": "invalid_request_error",
    "code": "unsupported_value",
    "param": "session.audio.input.turn_detection.type",
    "message": "turn detection is not supported by this deployment"
  }
}
```

The server includes the triggering `event_id` when provided. Read the effective
settings from `session.updated`; a requested value may not have taken effect.

| Field | Limitation |
| --- | --- |
| `tool_choice` | Tool authority belongs to the runtime composition, not this session setting. |
| `audio.output.speed` | Synthesis uses the speech provider's rate. |
| `audio.input.transcription.model` | The deployment selects the recognizer. |
| `reasoning` | The deployment configures the reasoner's effort. |
| `audio.input.turn_detection: null` | Refused when the binding requires a model-owned floor. |
| `audio.output.voice` | Requires a binding that can honor per-session voice selection; the cascade speech provider has a configured voice. |

Requesting a setting already in force does not require a change and is not
refused. Voice is reported only when the binding can identify it;
`max_output_tokens` reports the enforced limit, or `"inf"` when no limit applies.

The pinned schema generator also accepts `null` where the source specification
explicitly gives it as a default, including noise-reduction fields. This
reconciles the schema with the values sent by the SDK.

## Internal synchronization does not change the wire

Trajectory revisions, background-reasoning state, preparation, causal metadata,
and repair obligations remain internal. Clients see ordinary transcription,
response, cancellation, truncation, audio, and function-call events.

An input transcription can complete independently of response events; correlate
items by identity rather than assuming arrival order. On interruption, report
the playback boundary through the applicable cancel/clear/truncate lifecycle.
Canceled, unheard content should not be treated as spoken history. See
[The spoken boundary](spoken-boundary.md) for the implementation model.

## Implemented gateway subset

The gateway accepts these nine GA Realtime client events:

| Event | Use |
| --- | --- |
| `session.update` | Configure supported session fields |
| `input_audio_buffer.append` | Append audio |
| `input_audio_buffer.clear` | Discard buffered input |
| `input_audio_buffer.commit` | Commit buffered input |
| `output_audio_buffer.clear` | Clear active output playback |
| `conversation.item.create` | Submit function outputs or supported text/image messages |
| `conversation.item.truncate` | Report the heard boundary of assistant content |
| `response.create` | Request a response |
| `response.cancel` | Cancel a response |

`conversation.item.delete` is refused because the trajectory is append-only;
`conversation.item.retrieve` is not served. Negotiated observations can expose
perception as it happens, but are not an item-retrieval API.

### Responses and turns

A response groups the output of one operation. It closes after that operation
finishes planning and its started utterances finish playing. A user turn can
span several responses: an initial answer, background tool calls, and a spoken
follow-up. `response.done` means that response has no more items; it does not
mean the session cannot produce further work.

Text output uses the same configured cognition and commitment behavior without
speech synthesis or paced audio playback. An `input_image` in a message is
one-time turn content, not a video stream subject to frame gating. Vision-capable
providers resolve its retained media; other providers can use accompanying text.

### Client turn and playback control

Where the binding permits client-owned turns, setting `turn_detection: null`
disables silence-based endpoints and automatic responses. Send
`input_audio_buffer.commit` to end the input, then `response.create` to ask for
output.

`output_audio_buffer.clear` requires active playback and is refused when there
is nothing to clear. A retained item ID alone does not establish that audio is
still playing. Track actual playback and response state in the client.

### Tool results

Function arguments are a JSON-encoded string, for example:

```json
{"arguments": "{\"path\":\"notes.txt\"}"}
```

Return one `function_call_output` per call, then send `response.create`. The
gateway validates the complete call-ID/name batch, commits it atomically, and
resumes background work without rerunning the foreground. Calls and results
use ordinary response and conversation events.

### Preparation and early observations

`continuous` preparation can start private work on partial transcripts. It does
not add public event types. The default `endpoint-only` observation policy
commits final transcripts. The opt-in `stable-partial` policy can admit changed,
nonempty provider-declared stable prefixes and cancel work superseded by later
revisions. These settings are deployment choices, not extra Realtime fields.

## Schema registry reference

The tables below describe pinned decoding and schema coverage. They do not add
to the implemented gateway event list above. Historical selected-case results
validate only the tested subset and configuration.

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

The pinned source revision and its SHA-256 are recorded inside the committed
schema bundle (`x-openrealtime-source` in
`protocol/openai/openai-realtime-events.schema.json`) and asserted by
`TestGeneratedDefinitionsMatchSchemaConstants`, so a regenerated bundle from a
different revision fails the ordinary test run rather than drifting quietly.
There is no refetch script in this tree; moving the pin is a deliberate
regeneration that goes through the ageing audit described above.

Run `openrealtime conformance protocol` (or `./scripts/check.sh`) to
audit all registry/schema links, compile the entire nested closure, confirm the
profile/direction counts, and exercise unknown-type, wrong-direction,
wrong-profile, and missing-required-field rejection.

## What conformance cannot see

Valid wire events do not prove a client handles or displays them correctly.
When adding a feature, verify the complete path through transport, reducer,
and presentation. Test new values of existing fields as well as new event
names; for example, a client can receive `response.done` but ignore an
unfamiliar status.

A useful review checks:

1. Which events and field values can this configuration now produce?
2. Which component consumes each one?
3. What state change or visible result should the user receive?
4. Does a test assert that result at the final consumer?

Forwarding layers do not need to interpret every event. The browser transport
can forward frames to a reducer while a view renders the result. Likewise,
intentionally ignored lifecycle events need no artificial handler. Tests
should establish the behavior required by the selected client capabilities.

The shared reducer retains every response status and applies warning treatment
to `incomplete` and `failed`; cancellation remains distinguishable as an
interruption. See [Presentation design](composable-presentation.md) for the
client state contract.
