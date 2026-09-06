# The OpenRealtime sidecar protocol, version 4

**Status:** current reference for graph-native external models.

**Base:** the framing of [version 1](sidecar-protocol-1.md): a newline-terminated
JSON header, followed by exactly `payload_bytes` of binary body when the header
declares one, over the sidecar's stdin and stdout or a socket. Nothing else from
versions 1 to 3 is inherited. Those versions describe an *audio binding* - a
process that hears PCM, speaks PCM, and is told when to respond. Version 4
describes an *element*: a process that declares typed input and output ports,
proves what it is, and exchanges envelopes on those ports. Audio, images, text,
tool calls, and interaction acts are all port types rather than message kinds.

This is the version the graph-native runtime speaks. The locked omni, duplex,
and upstream external-model references in the shipped graphs dial it, and it
is what `elements/model` mounts. The legacy `serve -binding omni|duplex|sidecar`
presets still negotiate versions 1 to 3 and do not select this one; a version 4
sidecar is reached through a graph launch profile.

## Why a fourth version rather than a fourth capability

Versions 1–3 describe audio-model messages. Version 4 describes typed element
ports, allowing audio, text, images, and tools to use the same envelope
transport. Port descriptors define payload meaning and validation.

The handshake also identifies the exact descriptor, applied configuration,
runtime artifact, and provider/adapter identities. A host can compare those
identities with its deployment plan before admitting the model.

**Implementation order:** [handshake](#handshake) → [ports and media](#ports-formats-and-media)
→ [frames](#element-frames) → [limits](#limits) → [conformance](#conformance).
The [Python example](#writing-a-version-4-sidecar) shows the adapter structure;
[Sidecars](sidecars.md) explains how this differs from binding-based launch.

## Handshake

The engine sends `hello`; the sidecar answers `ready` or `error`. Both carry
`"version": 4` and neither may carry a binary payload or any field from the
earlier versions. A `hello` with `sample_rate` in it, or a `ready` with
`capabilities`, is rejected before negotiation.

```jsonc
{ "type": "hello", "version": 4,
  "element_descriptor": { "format_version": 1, "name": "sidecar.ConformanceProbe",
    "revision": 1, "ports": [ /* see below */ ],
    "reaction": { "triggers": ["request"], "interrupts": ["cancel"], "outcomes": ["result"] } },
  "element_config": {"mode":"conformance"},
  "selected_ports": [
    { "name": "request", "direction": "input",
      "type": { "name": "Trigger", "arguments": [{ "name": "sidecar.ConformanceRequest" }] },
      "formats": [ { "payload_mode": "json", "max_json_bytes": 524288 } ] },
    { "name": "cancel",  "direction": "input",  "type": { "name": "Interrupt", "arguments": [{ "name": "flow.RunID" }] },
      "formats": [ { "payload_mode": "json", "max_json_bytes": 524288 } ] },
    { "name": "result",  "direction": "output", "type": { "name": "Event", "arguments": [{ "name": "sidecar.ConformanceResult" }] },
      "formats": [ { "payload_mode": "json", "max_json_bytes": 524288 } ] }
  ],
  "required_capabilities": [ { "name": "model.generation", "contract": "speech.v1" } ] }
```

- `element_descriptor` is the complete typed contract the engine expects: the
  ports, their directions and types, cardinality, and the reaction contract
  that says which ports trigger work, which interrupt it, and which carry
  outcomes. It is the same descriptor format every Go element publishes.
- `element_config` is the exact configuration the sidecar must apply, as
  compact JSON. Whitespace is significant: the digest in `ready` is over these
  bytes, so a value re-serialised with different spacing is a different value.
- `selected_ports` is the subset of descriptor ports the mounted graph actually
  connected, each with the wire formats the engine can offer for it. A port
  the descriptor declares but the graph did not connect is not listed and must
  not be used. At least one port is selected.
- `required_capabilities` names what the deployment must prove at readiness,
  by capability name and optional contract. The `port.` namespace is reserved.

```jsonc
{ "type": "ready", "version": 4,
  "element_descriptor": { /* identical identity to the hello */ },
  "applied_config_digest": "sha256:…",
  "runtime_artifact": { "id": "openrealtime/python-sidecar", "revision": "protocol-v4" },
  "resolved_capabilities": [
    { "name": "model.generation", "contract": "speech.v1",
      "provider": { "id": "vendor/model", "revision": "2026-08" },
      "adapter":  { "id": "openrealtime/python-element-wire", "revision": "4" } },
    { "name": "port.input.request", "contract": "Trigger[sidecar.ConformanceRequest]",
      "provider": { "id": "openrealtime/conformance-probe", "revision": "1" },
      "adapter":  { "id": "openrealtime/python-element-wire", "revision": "4" } }
    /* one entry per selected port, plus every required capability */
  ],
  "negotiated_ports": [
    { "name": "request", "direction": "input",
      "format": { "payload_mode": "json", "max_json_bytes": 524288 } },
    { "name": "cancel",  "direction": "input",
      "format": { "payload_mode": "json", "max_json_bytes": 524288 } },
    { "name": "result",  "direction": "output",
      "format": { "payload_mode": "json", "max_json_bytes": 524288 } }
  ] }
```

Readiness is verified, not trusted:

- the descriptor identity in `ready` must equal the one in `hello`, so a
  package declaration cannot drift from the process that started;
- `applied_config_digest` must equal the SHA-256 of the exact `element_config`
  bytes sent, so the engine knows the configuration it asked for is the one
  running;
- `runtime_artifact` and every provider and adapter identity must be immutable:
  a revision or a digest, never `latest`, `current`, a template marker, or an
  unresolved placeholder;
- every selected port must be negotiated, with a format the engine offered for
  it, and no unselected port may appear;
- every selected port must be resolved as a `port.<direction>.<name>` capability
  with an exact adapter identity, and every required capability must be
  resolved with the contract that was required.

Any failure is a refused mount, reported with the exact field that disagreed.

## Ports, formats, and media

Each selected port offers one to sixteen wire formats and the sidecar picks
one. A format is a payload mode with byte limits and, for media, an exact
profile:

| `payload_mode` | Header | Body |
| --- | --- | --- |
| `json` | envelope with `json` | none |
| `binary` | envelope with `media` | `payload_bytes` of body |
| `json_binary` | envelope with `json` and `media` | `payload_bytes` of body |

`max_json_bytes` is at most 512 KiB and `max_binary_bytes` at most 16 MiB. A
media format names a `kind` of `audio` or `video`, an `encoding`, and for
audio an exact `sample_format`, `sample_rate_hz`, and `channels` with a bounded
`max_frame_duration_ms`; for video, bounded `max_width`, `max_height`, and
`max_frame_rate_millihz`. Whether a port is media at all follows from its type:
a carrier type under `audio.`, `speech.audio`, `video.`, `vision.video`,
`image.`, or `vision.image` whose leaf is a frame, batch, chunk, or samples is
media; a `…Reference` type is not, and neither is a file, which may negotiate
`binary` without pretending to be live media. A type that mixes audio and
video contracts is rejected.

Every frame in both directions is checked against the negotiated format: the
payload lanes it uses, its byte counts, and its media metadata against the
exact profile. A frame on an unselected port, in the wrong direction, or with
audio metadata on a video port is refused.

## Element frames

```jsonc
{ "type": "element_frame", "port": "request", "payload_bytes": 0,
  "envelope": {
    "type": { "name": "Trigger", "arguments": [{ "name": "sidecar.ConformanceRequest" }] },
    "item_id": "request-1", "session_id": "s-1", "run_id": "run-1", "sequence": 1,
    "trace_id": "t-1", "cancellation_scope": "run-1",
    "causal_parents": ["turn-7"],
    "json": { "challenge": "…" } } }
```

An `element_frame` carries exactly a port name, an envelope, and an optional
body. The engine sends frames only to input ports; the sidecar sends frames
only from output ports. The envelope is the language-neutral part of the Go
`element.Envelope`: the value type, item, session, source, opportunity, and
run identities, a sequence number, capture and receive clocks, a trace and a
cancellation scope, up to 256 causal parents, and the payload in `json`,
`media`, or both. Identifiers are bounded at 1,024 bytes and may not contain
control characters. `json` must be strict JSON; the header's own encoding is
canonical.

Triggering, interruption, timing, and terminal semantics live in the port's
type and the descriptor's reaction contract, not in message kinds. An
interrupt port is one the descriptor lists under `reaction.interrupts`; the
reference implementation dispatches frames on those ports from the reader
thread, ahead of the bounded work queue, so a cancellation is never stuck
behind the work it cancels. A result that answers a request carries the
request's `run_id` and names it among its `causal_parents`; a result that
answers a cancellation names both the request and the cancellation.

## Limits

| | |
| --- | --- |
| Ports per descriptor | 256 |
| Wire formats per port | 16 |
| Resolved capabilities | 512 |
| Required capabilities | 256 |
| JSON in one envelope or config | 512 KiB |
| Binary body | 16 MiB |
| Identifier | 1,024 bytes |
| Causal parents | 256 |

## Writing a version 4 sidecar

The Python package under `sidecars/openrealtime_sidecar` carries the whole
transport. A sidecar subclasses `ElementSidecar`, publishes its descriptor and
identities as class attributes, and implements `on_element_frame`:

```python
from openrealtime_sidecar import ElementSidecar, run_element

class Echo(ElementSidecar):
    element_descriptor = {...}                                    # the typed contract
    runtime_artifact = {"id": "example/echo", "revision": "1"}
    provider_artifact = {"id": "example/model", "revision": "2026-08"}
    adapter_artifact = {"id": "example/echo-wire", "revision": "1"}

    def configure_element(self, config):                          # exact hello config
        ...

    def on_element_frame(self, port, envelope, payload):
        self.send_element_frame("result", {...})

if __name__ == "__main__":
    run_element(Echo)
```

The base class negotiates, validates every frame it sends and receives against
the negotiated formats, dispatches interrupt ports on the reader thread and
everything else on one bounded worker queue, and reports a fatal `error`
rather than a half-negotiated session on any disagreement. A provider adapter
that supports only some codecs overrides `supports_wire_format`.

`sidecars/v4_conformance_sidecar.py` is the smallest complete example: the
`ConformanceElementSidecar` it runs is the bundled probe that the conformance
suite below drives, and it uses no model at all.

## Mounting one

A graph mounts a version 4 sidecar through the external-model element. Its
configuration names a registered deployment, the settings that become
`element_config`, the capabilities readiness must prove, and the wire formats
to offer per port:

```json
{ "deployment": "omni.reference",
  "settings": { "voice": "default" },
  "required_capabilities": [ { "name": "model.generation", "contract": "speech.v1" } ],
  "port_formats": { "audio_in": [ { "payload_mode": "binary", "max_binary_bytes": 1048576,
    "media": { "kind": "audio", "encoding": "pcm", "sample_format": "s16le",
               "sample_rate_hz": 24000, "channels": 1, "max_frame_duration_ms": 200 } } ] } }
```

The deployment registry supplies the dialer: a command line for a subprocess,
an address for a socket, or a Go dialer for a remote Realtime adapter that
speaks the same contract in-process. The graph's own locked identities and the
sidecar's attested identities are both retained as evidence, and a benchmark
cell reports the pair.

## Conformance

```sh
openrealtime conformance sidecar -protocol-version 4 \
  -- python3 sidecars/v4_conformance_sidecar.py
```

The suite offers the bundled probe descriptor and checks fifteen things: that
the probe is valid; that the handshake completes; that the sidecar declares
version 4; that it attests the exact probe descriptor, an immutable runtime
artifact, and the exact applied configuration; that it negotiates every
selected port; that it accepts a typed request and produces a typed result
carrying the challenge and a success status; that the result preserves run and
causal correlation; that it accepts a pending request and a typed cancellation
on the interrupt port; that it acknowledges the cancellation with a canceled
result; and that the acknowledgement preserves the cancellation scope and both
causes. `TestRunConformanceV4AgainstBundledPythonSDK` runs the same suite
against the Python fixture as a real subprocess in the ordinary test run, and
it is the one sidecar version exercised that way.

## Compatibility

A version 4 engine still runs version 1, 2, and 3 sidecars through the legacy
bindings, which select those versions explicitly. There is no translation in
either direction: an audio-binding sidecar is not wrapped as an element, and an
element is not exposed as an audio binding. A version 4 `hello` that reaches a
version 1 sidecar is answered with an `error` naming the unsupported version,
as version 1 requires.
