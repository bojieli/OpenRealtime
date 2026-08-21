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

A client POSTs an SDP offer to `/v1/realtime` with `Content-Type:
application/sdp` and receives an answer. Protocol events travel on a data
channel named `oai-events`, which is what Realtime clients already expect.

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

## Video

Video always enters as protocol events, for every client. A WebRTC adapter that
terminates a video track decodes it and emits those events on the client's
behalf; it does not bypass them. One entrance means one thing to specify and
one thing to test.
