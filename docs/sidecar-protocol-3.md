# The OpenRealtime sidecar protocol, version 3

**Status:** additive experimental contract.

**Base:** all framing and messages from [version 1](sidecar-protocol-1.md) and
typed interaction control from [version 2](sidecar-protocol-2.md).

Version 3 adds direct encoded images and live function-catalog replacement.
It does not add visual narration: a sidecar declaring `visual_input` receives
the pixels themselves. Versions 1 and 2 retain their original meanings and
must be selected explicitly when an older sidecar is used.

**Use this version when:** a binding-based sidecar needs image pixels or a
replacement tool catalog during a session. Implement v1 framing and the v2
interaction contract first. The image, tool-update, and authority rules below
are the additions. For graph-native models, use [v4](sidecar-protocol-4.md).

## Direct images

The engine sends an `image` frame with a JPEG or PNG binary payload:

```jsonc
{ "type": "image", "source": "screen", "mime_type": "image/jpeg",
  "width": 1280, "height": 720, "timestamp_ms": 1787821745123,
  "payload_bytes": 82491 }
<82491 encoded bytes>
```

`source`, positive `width` and `height`, and the binary payload are required.
`timestamp_ms` is capture time on the session clock. A sidecar keeps sources
separate: a camera frame must not silently replace the screen frame whose
coordinate system an action targets. Payload framing and the 16 MiB limit are
unchanged from v1.

The sidecar answers `ready` with `visual_input` only when it can pass these
pixels to the model. OCR text, captions, or a client-provided narration do not
satisfy the capability. A voice+vision sidecar deployment is rejected unless
it negotiates protocol 3 and declares `visual_input`.

## Live tools

Realtime clients commonly declare tools in `session.update`, after the
sidecar handshake. Version 3 therefore adds a JSON-only replacement frame:

```json
{
  "type": "tools_update",
  "tools": [
    {
      "name": "computer.click_element",
      "description": "Click one visible marked element",
      "parameters": {"type": "object", "properties": {"label": {"type": "integer"}}}
    }
  ]
}
```

The array replaces the prior catalog atomically; an empty array removes every
client tool. A sidecar must not retain a stale declaration after replacement.
The Python base class routes this frame to `on_tools_update`.

## Fast-action authority

Receiving a tool schema is not execution authority. By default a sidecar call
is refused or remains a proposal. A deployment must explicitly enable bounded
fast computer use. Even then, the engine accepts only a declared
`computer.*` action in its allowlist and applies target scoping, confirmation,
canonical trajectory recording, the action ledger, and the configured local
or client dispatcher. The result returns in `tool_result`; arbitrary tools and
ambient desktop actions remain unavailable.

This boundary matters for visual actors: a model may wake on every retained
frame, but it cannot convert a hallucinated function name or stale coordinate
into an unrecorded side effect.

## Compatibility and conformance

A v3 engine can still run a v1 or v2 sidecar by selecting that version, but it
cannot send direct images or live tool updates over the older contract. It
must not translate pixels to prose and call that equivalent.

```sh
openrealtime conformance sidecar -protocol-version 3 \
  -- python3 sidecars/qwen3_omni_sidecar.py --mock
```

In addition to the earlier checks, conformance verifies that a declared
visual-input sidecar accepts an encoded image and that a tool-capable sidecar
accepts a catalog update.
