# Transports

Clients reach an OpenRealtime session through WebSocket directly or through a
media adapter. All adapters use the same public protocol; none bypasses session
validation or gains extra action authority.

| Connection | Use it for | Media path |
| --- | --- | --- |
| WebSocket | Server applications and custom local clients | Protocol events carrying audio and other content |
| WebRTC | Browsers and mobile clients | Audio tracks plus a protocol data channel |
| LiveKit | An existing RTC room | An agent participant bridges tracks and data packets |

The [quickstart](quickstart.md) launches the browser and adapters together.
For a custom client, use the [SDK example](../examples/sdk-client/README.md)
and check [API compatibility](openai-realtime-compatibility.md).

## WebSocket

The default endpoint is `ws://127.0.0.1:8765/v1/realtime`. Send protocol events
as JSON text messages. When a server token is configured, supply it as the
bearer credential. Production TLS terminates in front of the server; see
[Deployment](../deploy/README.md).

## WebRTC

```sh
openrealtime serve --webrtc-listen 127.0.0.1:8766 --webrtc-stun stun:stun.l.google.com:19302
```

A client POSTs an SDP offer to `/v1/realtime/calls` with `Content-Type:
application/sdp` and receives an answer. Protocol events travel on a data
channel named `oai-events`, which is what Realtime clients already expect.

### Reaching the adapter from a browser

The SDP exchange carries a bearer credential and an `application/sdp` body, so
a browser will not send it cross-origin without asking first. The adapter
answers that preflight only for origins an operator named:

```sh
openrealtime serve -webrtc-listen 127.0.0.1:8766 \
  -webrtc-allow-origin https://your-app.example.com
```

The default allowlist is empty. Direct cross-origin browser clients must have
their origin listed; `*` is available for local development. Configure the
actual application origin for deployment. The adapter answers allowed
preflight requests and echoes the requested headers.

The shipped browser uses a presentation-host relay instead of connecting
directly across origins:

| Browser profile | Browser destination | Credential handling |
| --- | --- | --- |
| WebSocket | same-origin `/client/v1/realtime` | Host relay holds the upstream credential |
| WebRTC | same-origin `/client/v1/realtime/calls` | Host relay holds the upstream credential |

The host has an explicit directory of upstream endpoints. The browser manifest
contains relative public routes and does not expose the upstream token. Custom
transport plugins must declare their endpoints and permissions explicitly.
See [Presentation design](composable-presentation.md) for the detailed contract.

### Large events: chunk framing

One data channel message carries one protocol event, as text. That is the whole
of the framing for a voice session, and a client that only speaks voice needs
nothing else on this page.

Video does not fit in it. SCTP negotiates a maximum message size and each peer
advertises its own; the smallest in the field is Safari's 64 KiB, well under a
single screen frame, which is base64 and therefore a third larger again than
the image it carries. A peer that sends past that maximum does not get a
truncated message — the write is refused and the frame never leaves, so the
failure looks like video silently not working.

So a message too large for one SCTP message is carried as a sequence of
**binary** chunk frames:

```text
byte  0..3   "ORTC"
byte  4      version, currently 1
byte  5      flags; bit 0 set on the final chunk
byte  6..9   message identifier, uint32 big-endian
byte 10..11  chunk index,  uint16 big-endian
byte 12..13  chunk count,  uint16 big-endian
byte 14..    payload: this chunk's slice of the UTF-8 event
```

Concatenating the payloads in index order gives back exactly the event a
WebSocket client would have sent or received. Three properties are worth
stating, because each of them is a decision:

- **Text means an event, binary means a chunk of one.** The two cannot be
  confused, and no content inspection decides which is which.
- **Chunking is below the protocol, not part of it.** The reassembled bytes are
  an ordinary event, so the adapter remains a plain protocol client and rule 3
  still holds.
- **A message that fits is still sent whole.** The threshold is the size SCTP
  actually negotiated rather than a constant, so nothing a client can already
  receive changes shape. A client that ignores binary messages behaves exactly
  as it did before this existed: it does not see events too large for one
  message, which is what happened anyway.

Reassembly is bounded — 8 MiB per message and four partial messages in flight —
because it is memory a peer controls.

### Frame size and the read limit

The video limits a session advertises at negotiation are a promise the
transport has to be able to keep. A WebSocket read limit below the largest
legal frame does not refuse an oversized frame: it is enforced beneath the
protocol, so it closes the connection, and the client gets a dropped session
where it should have got an error naming the limit it exceeded.

So the gateway sizes its read limit from `max_frame_bytes` rather than from a
constant, with headroom above it. The headroom is what makes an oversized frame
answerable — a client that forgot to downscale gets

```text
video frame of 5242880 bytes exceeds the 4194304 byte limit
```

on a session that stays up, instead of a disconnection it has to guess at.

### Codecs, and an honest limitation

| Direction | Default codec | Consequence |
| --- | --- | --- |
| Inbound audio | Opus, decoded to 24 kHz PCM | Browser microphone audio reaches recognition as PCM. |
| Outbound audio | G.711 mu-law at 8 kHz | The default pure-Go build has telephone-bandwidth output. |

Wideband output requires an appropriate RTC integration or the optional
`opus` build, which uses libopus through cgo. The `local.go.webrtc.opus` release
check requires libopus and libopusfile development dependencies. Use the
[release matrix](release-validation.md) to inspect that prerequisite.

### Clean audio is the client's responsibility

Apply echo cancellation and noise suppression in the capture client. The
server expects already processed input; duplicating unknown client processing
can damage recognition. Browser clients can use the WebRTC capture controls.

## LiveKit

An agent that joins a room and proxies to a protocol endpoint. It ships as a
[separate module](../integrations/livekit/) on its own release cycle, because
it is a client rather than a component — it can be replaced by an equivalent
for another provider, or rewritten by somebody else, without touching the
server.

```sh
cd integrations/livekit
go run ./cmd/openrealtime-livekit \
  -livekit-url wss://your-project.livekit.cloud \
  -room demo \
  -endpoint ws://127.0.0.1:8765/v1/realtime
```

The participant subscribes to room audio, publishes agent audio, and forwards
protocol events in data packets. With `-video`, it also bridges VP8 video key
frames after negotiating the extension. Without that flag, ordinary video
tracks do not become observations; a custom client can still publish protocol
video events in data packets. See the [integration guide](../integrations/livekit/README.md).

## Video

Video always enters the engine as protocol events. The descriptor-locked WebRTC
browser profile captures a selected screen/camera, encodes retained JPEG/PNG
frames, and sends those events over the WebRTC data channel; the audio track
remains RTP.

The in-process adapter also bridges an inbound **VP8 video track**, and it
does so as a protocol client rather than a second entrance: once the client
has negotiated `video.input` in `session.update`, the adapter decodes the
track's key frames, scales them to the negotiated `max_dimension`, encodes
JPEG under the negotiated `max_frame_bytes`, and sends the same
`openrealtime.input_video_source.update` and
`openrealtime.input_video_frame.append` events a client would, at no more than
the negotiated `fps_cap`. Nothing is sent before video is negotiated, and a
session that never negotiates it is voice-only with the track drained.

It bridges key frames only. There is no pure-Go decoder for VP8 inter frames,
and a cgo dependency on libvpx would cost the static binary the same way
libopus does; the key-frame decoder in `golang.org/x/image` builds
everywhere. So the adapter asks the sender for a key frame at a fixed cadence
with an RTCP picture-loss indication (`VideoKeyframeInterval`, one second by
default) and discards the inter frames between them. The engine's video
observer samples at a few hertz and gates on pixel change, so a key frame a
second is the observation it wanted; the inter frames it cannot decode are
the ones it would have discarded. H.264, VP9, and AV1 tracks are logged as
unbridged and drained.

The LiveKit agent does the same for a room's video tracks, behind its
`-video` flag: it subscribes to room video, declares the extension in its own
`session.update`, and bridges VP8 key frames as the same two events, asking
the publisher for one each second with a picture-loss indication. The flag is
opt-in because it changes what the agent negotiates; without it the wire is
unchanged. It writes the bridge itself rather than importing the server's,
for the reason it writes its own protocol client: an integration that ships
separately should not depend on the server's internals.

| Client path | Audio | Direct screen pixels |
| --- | --- | --- |
| composable browser WebRTC profile | RTP media track | protocol frames on the data channel |
| custom LiveKit client publishing protocol frames | room audio track | protocol frames in data packets |
| stock WebRTC client publishing a VP8 video track | RTP audio | key frames bridged after `video.input` is negotiated |
| stock LiveKit client publishing a VP8 video track, agent run with `-video` | room audio | key frames bridged after `video.input` is negotiated |
| any client publishing H.264, VP9, or AV1 video | RTP/room audio | not bridged; the track is drained and logged |

All supported paths produce the same protocol video events and use the same
server-side observation gate. The table describes the codecs and capture paths
currently implemented; other track formats need a separate conversion step.
