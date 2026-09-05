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

## Checked against OpenAI's own client

The schema check runs on every session, but a schema is a description of an
API rather than the API, and the two can disagree. So the suite also runs the
published `@openai/agents-realtime` package - unmodified, configured as its own
documentation says - through a tool-using turn on both transports. See
[examples/sdk-client](../examples/sdk-client/README.md); `go test
./examples/sdk-client/` runs it, and skips where npm's dependencies are absent.

It found two places where this server was stricter than the API it describes,
and both are now reconciled:

**`noise_reduction: null`.** The source specification gives the field
`"type": "object"`, `"default": null`, and a description stating it can be set
to null to turn the feature off. Converted to JSON Schema the type won and the
default became unrepresentable, so a validator built from it rejected the null
OpenAI's own API returns and OpenAI's own client sends. The generator now emits
`["object", "null"]` wherever the specification declares null as the default -
a mechanical rule reading the specification's own statement, visible in the
pinned artifact rather than hidden in the validator. It applies to five fields,
all of them noise reduction.

**An unsupported field is refused by name, not by discarding the event.** A
client may ask for a turn detector this deployment does not have; the SDK's
default is `semantic_vad`. Three things could happen and only one of them is
right.

Refusing the whole `session.update` discards the instructions, the tools, and
the audio formats that arrived in the same event, which left every unmodified
official client unable to configure a session at all. Quietly substituting the
detector this server does have is worse in a subtler way: the client asked for
particular endpointing behaviour, did not get it, and would only find out by
reading a field back and noticing it had changed.

So everything else in the event applies, the unsupported field does not, and an
`error` names it:

```jsonc
{ "type": "error",
  "error": {
    "type": "invalid_request_error",
    "code": "unsupported_value",
    "param": "session.audio.input.turn_detection.type",
    "event_id": "<the client's own event_id, when it sent one>",
    "message": "turn detection \"semantic_vad\" is not supported: ..." }}
```

The ordering is deliberate: `session.updated` is sent first, then the errors.
A client told about an error before it has been told the update applied has
every reason to read the first as the second failing. The session stays open —
which the base protocol's own description of the error event says is the normal
case, and which OpenAI's SDK is verified to handle: it completes a tool-using
turn after receiving one.

Turn detection parameters a client leaves unset resolve to the deployment's
rather than to zero.

The same treatment covers every session field this server parses and does not
act on, because a field dropped in silence is the substitution case wearing a
different hat — `session.updated` reports the value actually in force, so the
only way to discover the difference is to read the field back and notice it
changed, and most clients never do:

| Field | Why it is not applied |
| --- | --- |
| `tool_choice` | Which model may call tools is an authority boundary here, not a session setting: the fast model proposes and cannot execute, the reasoner executes. |
| `audio.output.speed` | Synthesis runs at the rate the speech provider produces. |
| `audio.input.transcription.model` | The recogniser is the deployment's, named in `session.updated`. |
| `reasoning` | Effort belongs to the provider the reasoner runs on, not to a session. |
| `audio.input.turn_detection: null` | Only on a binding whose model owns the floor. Taking the floor requires someone to hand it over, and `duplex` declares the model holds it. |
| `audio.output.voice` | Only on a binding that says it cannot honour one. A speech provider is built with its voice, and a speech plan carries text and nothing else, so `cascade` has no per-session voice to give; a binding forwarding to a model with several does, and takes it. |

`tool_choice` is the one with teeth. A client that asks for no tools and
receives tool calls is not looking at a cosmetic difference; it is looking at
behaviour it explicitly turned off. The rest are quieter, and `reasoning` is
quietest of all, because it is not echoed in `session.updated` at all — a
client setting it has no field to read back.

Two fields in the session object were stating a default rather than a fact,
which is the same failure without a client to blame for it:

- `audio.output.voice` was a name from a hosted catalogue on every deployment,
  including ones synthesising with something else entirely. It is now the voice
  the binding says is in force, and it is **omitted** where the binding cannot
  say — a session forwarding to a remote provider does not know the voice that
  provider will use, and an absent field says "not stated" rather than guessing.
- `max_output_tokens` was `"inf"` unconditionally. `"inf"` is a claim, not a
  placeholder: a turn cut short reports `max_output_tokens` as the reason it
  stopped, and a client told in one breath that no limit exists and in the next
  that a limit ended its turn has two facts that cannot both be true. It now
  reports the limit actually enforced, and `"inf"` only where none is.

Asking for what is already in force is not refused. A client that sends the
server's own defaults back has asked for nothing it will not get, and answering
that with errors would make the well-behaved clients the noisy ones.

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

**A response is one thing the agent did, not the whole turn.** One
`response.create` produces one response, and it carries every output item that
*it* produced — spoken or written content and function calls, indexed within
it. The response closes when what opened it has finished planning *and* every
utterance it started has finished playing, which are not the same moment:
speech is paced out over seconds after planning returns.

A turn can span several of them, and normally does. The voice answers in one
response; the background reasoner's tool calls arrive in another, and what it
found is spoken in a third, once the gate lets anything be heard. Nothing is
lost by that. Audio reaches the client on the audio channel rather than inside
a response envelope, and a client executing a tool reads `function_call` items
as they arrive — a response being done means that response has no more items,
not that the session has stopped producing them. An earlier version of this
document claimed the opposite, and the claim was wrong: it would require
holding a response open across an unbounded deliberation, so the first answer
could not complete until the last one did.

Function call arguments arrive as a JSON-encoded **string**, not an object —
`{"arguments": "{\"path\":\"notes.txt\"}"}` — which is what the official API
does and what every client executing a tool has to unwrap.

Turn detection is **server VAD or the client's own**. Setting
`turn_detection` to `null` in `session.update` is a client taking the floor:
silence stops ending turns, `input_audio_buffer.commit` says where a turn
ended, and the server stops creating responses until `response.create` asks
for one. Both halves have to hold or the client gets a session that
half-listens to it.

It accepts nine of the eleven GA Realtime client events: `session.update`, `input_audio_buffer.append`,
`input_audio_buffer.clear`, `output_audio_buffer.clear`,
`input_audio_buffer.commit`, `conversation.item.create` for function outputs
and for messages carrying text, images, or both, `conversation.item.truncate`,
`response.create`, and `response.cancel`.

`output_audio_buffer.clear` is refused when nothing is playing, and the
refusal is worth explaining because it exposes a client-side trap rather than
creating one. The acknowledgement carries the response it cleared, so with no
response in progress there is nothing to name, and a client that asked to stop
hearing something it was not hearing has a bug it wants to see.

The bug it usually has is measuring "the agent is speaking" by item identity.
An item identifier outlives the sound: it is cleared on
`response.output_audio.done`, which arrives after the last sample has played
and never arrives at all if the response was cancelled. A client that unmutes a
microphone and clears the output buffer whenever it holds an utterance
identifier will therefore clear an empty buffer every time the person speaks
after the agent has already finished. The honest measure is the playhead - the
end of the last scheduled buffer, compared against the clock - because that is
what "sound is still coming" actually means.

An `input_image` attached to a message is not a video source. A source is a
stream the server gates, which is what the extension's video events are for; an
image in a message is content of the turn, shown once because the client chose
to show it, and gating it could discard the only thing the turn was about. It
is retained outside the trajectory and referenced by handle from the
observation, so a provider that can see resolves it and one that cannot reads
the text and never pays for the bytes.

The other two are refused, and the refusal says why rather than reporting
"unsupported event" — one of them is structural and no client can work around
it, which is worth saying out loud.

| Event | Why |
| --- | --- |
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

Every check above inspects the wire, and there is a class of defect where the
wire is entirely correct. **A client is only written against the parts of a
protocol that were reachable when it was written**, so making a capability
reachable ages every consumer that predates it — each keeps handling the events
that existed before and ignoring the ones that did not. Nothing fails, because
nothing is wrong: the events are valid, correctly named, and correctly
directed. They are simply not read, and the symptom is an absence.

Making text-only sessions reachable on `upstream` did this three times in one
day — to the former mirror, to the former standalone client, and to the pre-GA
rename table, which had no name to rename onto while nothing downstream
handled text.

It is not a property of that capability. Letting a client take the floor did
the same thing to the former standalone client independently: it could declare
`turn_detection: null` from its session editor and had no way to end a turn, so
audio flowed, the server correctly waited to be told, and nothing happened —
with no error anywhere, because nothing was wrong. Two unrelated capabilities,
the same absence. Expect this of any capability that makes new events
reachable, and run the audit when adding one rather than when something is
reported.

Not every ageing is a new event, and the subtraction below will not find the
ones that are not. An incomplete turn added no event at all — it added a new
*value* to `status` on `response.done`, which every consumer already handled.
Switching on a status nobody has ever sent looks exactly like switching on one
that cannot happen, so when a capability widens an existing field, grep for the
field rather than the event name.

The defence is cheaper than the audit and worth building in: **render the value,
enumerate only the treatment**. A consumer that records whatever `status`
arrives and enumerates only which statuses deserve a warning cannot be blinded
by a new one — it shows something unfamiliar rather than nothing, which is a
question somebody asks rather than an absence nobody notices. A consumer that
enumerates the rendering instead reintroduces the same silence one layer down.
The descriptor-locked shared reducer does this now: every status reaches client
state, and only `incomplete` and `failed` require warning treatment, because
`cancelled` is the user interrupting on purpose.

The audit is cheap. Enumerate the events the server can emit, subtract the ones
a consumer handles, and go through the remainder:

```sh
# the emitted set comes from the pinned registry; the handled set from the client
grep -o 'case "[^"]*"' binding/upstream/mirror.go | sed 's/case "//;s/"//' | sort -u
```

The whole audit is in the second step, and the rule is **reachability, not
coverage**. Most of the remainder is correctly unhandled — lifecycle events,
acknowledgements of what this side sent, the `.done` twin of a delta already
accumulated, capabilities this consumer never declares. Handling everything
would bury the one case that matters under a dozen pointless ones, which is the
same failure as a test that cannot fail. Ask of each: *can this consumer now
receive this, and what happens if it does?*

Two worked answers. The descriptor-composed browser WebSocket transport handles
no text events and needs none: it forwards validated protocol frames to the
selected reducer service, while a separately replaceable view decides what to
render. The `upstream` mirror ignores every function call event and should:
this binding declares no tools to the remote, because the remote is the fast
voice and has no execution authority.

Assert on what the consumer rendered, never on what crossed the wire. A
wire-only test agrees there is nothing wrong.
