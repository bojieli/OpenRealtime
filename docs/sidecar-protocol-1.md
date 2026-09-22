# The OpenRealtime sidecar protocol, version 1

**Status:** stable from OpenRealtime v0.1.0.
**Transport:** a duplex byte stream — a child process's stdin and stdout, a
Unix socket, or TCP.

This protocol connects the runtime to an external audio model process. For
version selection and a first integration, read the [sidecar guide](sidecars.md).
Versions 2 and 3 extend this message set; graph-native v4 inherits only its byte
framing.

Validate the reference process contract without loading weights:

```sh
openrealtime conformance sidecar -- python3 sidecars/qwen3_omni_sidecar.py --mock
```

## 1. Framing

Each message is one JSON header line terminated by `\n`, optionally followed by
exactly `payload_bytes` of binary payload:

```
{"type":"audio","payload_bytes":960}\n<960 bytes of PCM16>
```

Binary audio avoids base64 expansion and conversion on every frame. Control
fields remain JSON. Keep diagnostics on stderr so they cannot corrupt framing.

Both sides MUST flush after every frame so the peer can process streaming
input without waiting for another message.

Audio payloads are little-endian signed 16-bit mono PCM.

## 2. Handshake

The engine sends `hello` first. The sidecar answers `ready` or `error`.

```jsonc
// engine → sidecar
{ "type": "hello", "version": 1, "sample_rate": 24000,
  "instructions": "You are a helpful assistant.",
  "voice": "default",
  "tools": [ { "name": "get_balance", "description": "...", "parameters": {} } ] }

// sidecar → engine
{ "type": "ready", "version": 1, "model": "Qwen/Qwen3-Omni-30B-A3B-Instruct",
  "output_rate": 24000,
  "capabilities": ["transcript", "text_injection"] }
```

- A version mismatch MUST be refused with a fatal `error`. A model that
  half-understands the protocol is worse than one that does not start.
- `model` is REQUIRED, so evidence can name what produced a result.
- `output_rate` is REQUIRED and may differ from the input rate.

## 3. Capabilities

The engine reads these to decide what it must supply itself. This is what makes
a partial implementation useful rather than broken.

| Capability | Meaning | What the engine does without it |
| --- | --- | --- |
| `native_vad` | the model detects voice activity | keeps the acoustic floor itself |
| `transcript` | the model reports what it heard | commits nothing as user speech |
| `text_injection` | text can be added to context between turns | falls back to explicit hand-off |
| `tools` | the model can request function calls | ignores calls it never expects |
| `barge_in` | the model handles overlap itself | applies its own barge-in policy |
| `full_duplex` | the model listens and speaks at once | treats input/output concurrency as unavailable |

## 4. Engine to sidecar

| Type | Payload | Meaning |
| --- | --- | --- |
| `hello` | — | open the session (§2) |
| `audio` | PCM16 | input audio at the declared `sample_rate` |
| `text` | — | inject `text` with a `role`, without asking for a turn |
| `commit` | — | the client declared its turn over rather than waiting for silence |
| `respond` | — | produce a turn now |
| `interrupt` | — | stop generating at the next safe point |
| `tool_result` | — | the outcome of a call the model requested |
| `bye` | — | end the session |

`audio` MUST be handled without waiting for a turn in flight. A sidecar that
queued audio behind generation would hear the past.

`commit` is only meaningful to a sidecar whose model owns its own floor. A
client that turned server voice-activity detection off is saying where its turn
ended; a model that decides that for itself may act on the declaration, and one
whose floor the engine keeps can ignore it, because the engine has already
ended the turn before the message was sent. A sidecar that does not implement
it MUST ignore it rather than failing the session.

`interrupt` is cooperative. Nothing can safely kill a model mid-forward-pass, so
a generation loop is expected to check between chunks.

## 5. Sidecar to engine

| Type | Payload | Meaning |
| --- | --- | --- |
| `ready` | — | the handshake answer (§2) |
| `speech_started` / `speech_stopped` | — | model-native voice activity |
| `transcript` | — | what the model heard; `final` marks the terminal one |
| `text_delta` / `text_done` | — | what the model is saying, as text |
| `output_audio` | PCM16 | output audio at the declared `output_rate` |
| `turn_done` | — | the end of one turn |
| `tool_call` | — | a call the model wants made |
| `error` | — | a failure; `fatal` ends the session |
| `log` | — | a diagnostic line the engine forwards |

`text_delta` carries a nonempty fragment of the output text. A fragment may
consist entirely of whitespace (for example, a word separator or newline token);
consumers must preserve it when concatenating the stream. Empty fragments are
invalid. This does not relax the nonblank content requirement for `transcript`
or injected `text` messages.

An interrupted turn MUST still end with `turn_done`. The engine is waiting for a
boundary, not for completion.

## 6. Authority

A sidecar model is the **fast** provider. The two cognition boundaries apply to
it exactly as they apply to a provider inside the engine:

- **It cannot call tools.** A `tool_call` is answered with a `tool_result`
  carrying an error explaining that the background reasoner performs tool
  calls. The refusal is visible to the model rather than silent, so it can
  proceed rather than wait.
- **It owns the voice.** The engine never runs a second fast continuation for a
  sidecar-backed binding; doing so would produce two voices answering the same
  question.

The background reasoner's completed answer reaches the model as injected
`text`, followed by `respond` where the engine selected interaction ownership.
Where model-native interaction is selected, the text is injected and the model
decides for itself when to say it. Floor ownership is a separate selection.

## 7. Failure

- A malformed frame is a fatal error: a payload that does not match its declared
  length desynchronises the stream permanently.
- A sidecar's stderr is forwarded by the engine. A traceback that reaches the
  operator is the difference between "the model failed" and knowing why.
- A sidecar that does not exit after `bye` is killed rather than left holding a
  GPU.

## 8. Reference sidecars

| Sidecar | Model | Usual preset |
| --- | --- | --- |
| `sidecars/qwen3_omni_sidecar.py` | Qwen3-Omni | `omni` |
| `sidecars/minicpm_o_sidecar.py` | MiniCPM-o 4.5 | `omni` |
| `sidecars/moshi_sidecar.py` | Moshi | `duplex` |

Each accepts `--mock`, which speaks the protocol without loading a model. That
is how a deployment verifies its plumbing, how the conformance suite runs in
CI, and how a new sidecar is developed before the GPU is involved.

## 9. Versioning

Version 1 is frozen at the OpenRealtime v0.1.0 release. Later versions are
additive under a higher number; the handshake is where they are negotiated, and
a sidecar that does not implement the engine's version says so and exits.

| Version | Adds | Document |
| --- | --- | --- |
| 1 | the audio binding: PCM in, PCM out, respond and interrupt | this document |
| 2 | typed interaction acts and selected ownership | [version 2](sidecar-protocol-2.md) |
| 3 | direct encoded images and live tool catalogs | [version 3](sidecar-protocol-3.md) |
| 4 | descriptor-declared element ports and attested readiness; the version graph-native external models speak | [version 4](sidecar-protocol-4.md) |
