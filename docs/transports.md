# Transports

The protocol over WebSocket is the only entrance to a session. Everything else
sits strictly above it and speaks it like any other client.

```text
  browser (WebRTC)  ─┐
  mobile  (WebRTC)  ─┼─►  transport adapter  ─┐
  LiveKit room      ─┘                        │
                                              ├─►  OpenRealtime Protocol  ─►  session core
  plain WebSocket client  ────────────────────┘         (WebSocket)
```

Four rules, and the third is the one that matters:

1. The protocol over WebSocket is the only entrance to a session.
2. A transport adapter is a protocol client. It terminates media, handles ICE
   and jitter and loss concealment, and then speaks exactly the events any
   other client speaks.
3. **No adapter has privileged access.** If an adapter can express something a
   plain WebSocket client cannot, that is a defect, not a feature.
4. Adapters may run in-process where that is cheaper, but stay semantically
   identical to the wire form and are tested through the real protocol.

The reason is compatibility over time. A transport that reached the session
core directly would become a second place the protocol can drift, and every
such place multiplies the compatibility surface until a feature exists on one
path and not the other.

Rule 3 has a test rather than a promise: the adapter suite checks every event
the adapter sends against the set a plain client can send.

| Adapter | Handles | For |
| --- | --- | --- |
| *(none — direct)* | nothing | server-to-server, local development, the simplest client |
| WebRTC (in-process, Pion) | ICE, jitter, loss concealment, RTP pacing | browsers and mobile |
| LiveKit agent participant | a provider's room, tracks, and data channel | deployments already running RTC infrastructure |

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

Empty is the default and answers no browser, which is right for a
server-to-server deployment. It is a list rather than a switch because this
endpoint has no credential of its own and starting a session is all it does: a
wildcard would let any page a user visits open a session against any adapter
their browser can route to. `*` is accepted for local development.

This is not an optional convenience. A browser is always on a different origin
from the adapter - the adapter is a port on a server and the application is a
site - and OpenAI's own SDK only speaks WebRTC from a browser. Without it, an
unmodified official client cannot reach this endpoint at all.

The requested headers are echoed rather than enumerated, because a client sends
its own alongside the two the exchange needs and a server cannot know in
advance what every client will identify itself with.

That is the adapter's half. The page has a half of its own, and it is the
mirror image: a `Content-Security-Policy` with `connect-src 'self'` forbids the
browser from making the very request the adapter has just agreed to answer. The
demo shipped that way and could not reach an adapter in any documented
configuration — served intact, script parsing, every event name present, and
dead, because the one request it exists to make was blocked before it left the
page.

**The two pages in this repository need opposite values, and the difference is
decided by where the credential lives.** They are worth reading together before
changing either, because they look like the same header set wrong in one place:

| | `examples/browser` | `console` |
| --- | --- | --- |
| Where the SDP goes | direct to the adapter, at an address the page takes as a `?adapter=` parameter | to `/api/webrtc` on its own origin, which the server proxies onward |
| Where the credential lives | nowhere — the adapter is reached unauthenticated or through an operator's own edge | in the server, attached to the proxied request, never in the browser |
| Correct `connect-src` | `'self' http: https: ws: wss:` — the destination is a parameter and cannot be enumerated | `'self' ws: wss:` — every connection is same-origin by construction |

So the console's stricter policy is not extra diligence to be copied, and the
demo's broader one is not laxity to be tightened. Each follows from the shape of
its page. Harmonising them breaks whichever one gets changed, and it breaks it
in the way that is hardest to see: the page still loads, still parses, and still
does nothing.

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

The two directions do not share a codec, because they do not have the same
constraint.

**Inbound is Opus**, decoded to 24 kHz PCM by a pure-Go decoder. That direction
carries the user's voice into speech recognition, where bandwidth is accuracy.

**Outbound is G.711 mu-law** — 8 kHz, telephone quality. There is no pure-Go
Opus encoder. The alternatives are writing one, which is a codec nobody here
can verify against a reference, or taking a cgo dependency on libopus, which
trades a static binary and a reproducible build for wideband synthesis. Neither
is obviously right, and the cost of the choice made is stated here rather than
hidden: synthesised speech reaches a browser at telephone bandwidth.

A deployment that needs wideband output should use an RTC provider, which is
what the LiveKit path is for.

### Clean audio is the client's responsibility

Echo cancellation and noise suppression happen before audio reaches the
protocol. This is a stated requirement on clients rather than an omission: a
browser's WebRTC stack already does both well, doing them again server-side
would be worse than doing them once, and a server compensating for unknown
client-side processing would be guessing.

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

The current participant subscribes to room audio and publishes agent audio.
It also forwards protocol events carried as LiveKit data packets, so a custom
room client can send `openrealtime.input_video_frame.append` exactly as the
repository browser surface does. It does **not** yet decode an ordinary
LiveKit screen-share or camera video track. A stock meeting client that only
publishes a video track is therefore audio-only to this agent; use a protocol
frame publisher or add a codec-to-JPEG bridge before calling that deployment
voice+vision.

## Video

Video always enters the engine as protocol events. The repository console and
surface capture a selected screen/camera, encode retained JPEG/PNG frames, and
send those events over the WebRTC data channel; the audio track remains RTP.
The in-process adapter currently ignores inbound video tracks rather than
pretending encoded RTP is a model image. Likewise, the LiveKit integration
forwards video events in data packets but does not decode room video tracks.

| Client path | Audio | Direct screen pixels |
| --- | --- | --- |
| repository console/surface over WebRTC | RTP media track | protocol frames on the data channel |
| custom LiveKit client publishing protocol frames | room audio track | protocol frames in data packets |
| stock WebRTC or LiveKit client publishing only a video track | RTP/room audio | not yet bridged |

This still preserves one engine entrance and the same server-side adaptive
observation gate. Transport adapters may decode a track into those events in a
future implementation, but the current production boundary must be described
by what it actually carries.
