# The OpenRealtime sidecar protocol, version 1

**Status:** stable from OpenRealtime v1.0.
**Transport:** a duplex byte stream — a child process's stdin and stdout, a
Unix socket, or TCP.

A sidecar is a model that is not written in Go, running behind a process
boundary. Omni and full-duplex models live in Python; letting that into the
engine's build, test, and analysis path would make every one of them slower and
more fragile. This protocol is what keeps it out.

**The conformance suite is the contract.** A sidecar that passes it works with
the engine whatever it is written in; one that does not is broken before
anybody spends a GPU-hour finding out:

```sh
openrealtime conformance sidecar -- python3 sidecars/qwen3_omni_sidecar.py --mock
```

## 1. Framing

Each message is one JSON header line terminated by `\n`, optionally followed by
exactly `payload_bytes` of binary payload:

```
{"type":"audio","payload_bytes":960}\n<960 bytes of PCM16>
```

Audio is raw rather than base64. A third more bytes and an encode/decode pass on
every frame is a real cost on the hot path, and audio is the only thing large
enough to matter. Everything else is ordinary JSON, so a sidecar can be
debugged by reading the stream.

Both sides MUST flush after every frame. A sidecar that buffers is a sidecar
that appears to hang, and the point of streaming audio is that it arrives while
it is still useful.

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
| `full_duplex` | the model listens and speaks at once | stops asking it to take turns |

## 4. Engine to sidecar

| Type | Payload | Meaning |
| --- | --- | --- |
| `hello` | — | open the session (§2) |
| `audio` | PCM16 | input audio at the declared `sample_rate` |
| `text` | — | inject `text` with a `role`, without asking for a turn |
| `respond` | — | produce a turn now |
| `interrupt` | — | stop generating at the next safe point |
| `tool_result` | — | the outcome of a call the model requested |
| `bye` | — | end the session |

`audio` MUST be handled without waiting for a turn in flight. A sidecar that
queued audio behind generation would hear the past.

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
`text`, followed by `respond` where the model does not own its floor. Where it
does, the text is injected and the model decides for itself when to say it.

## 7. Failure

- A malformed frame is a fatal error: a payload that does not match its declared
  length desynchronises the stream permanently.
- A sidecar's stderr is forwarded by the engine. A traceback that reaches the
  operator is the difference between "the model failed" and knowing why.
- A sidecar that does not exit after `bye` is killed rather than left holding a
  GPU.

## 8. Reference sidecars

| Sidecar | Model | Binding |
| --- | --- | --- |
| `sidecars/qwen3_omni_sidecar.py` | Qwen3-Omni | `omni` |
| `sidecars/minicpm_o_sidecar.py` | MiniCPM-o 4.5 | `omni` |
| `sidecars/moshi_sidecar.py` | Moshi | `duplex` |

Each accepts `--mock`, which speaks the protocol without loading a model. That
is how a deployment verifies its plumbing, how the conformance suite runs in
CI, and how a new sidecar is developed before the GPU is involved.

## 9. Versioning

Version 1 is frozen at the OpenRealtime v1.0 release. Later versions are
additive under a higher number; the handshake is where they are negotiated, and
a sidecar that does not implement the engine's version says so and exits.
