# Legacy one-file WebRTC example

A voice session in a browser, in one HTML file with no build step and no
dependencies. This package is retained as a compatibility oracle while its
media behavior moves into the composable browser client.

```sh
openrealtime serve -webrtc-listen 127.0.0.1:8766 -webrtc-allow-origin http://127.0.0.1:8080
python3 -m http.server 8080 --directory examples/browser
```

Open `http://127.0.0.1:8080` and press Connect. Point it at another adapter
with `?adapter=http://host:port/v1/realtime/calls`.

The realtime gateway deliberately does not serve this page. New client work
uses `openrealtime present`, whose standalone host serves a descriptor-locked
manifest and independently replaceable client modules.

Browsers require a secure context for microphone access. `127.0.0.1` counts as
one; any other host needs HTTPS.

## What it demonstrates

The client implements **no media plumbing**. There is no jitter buffer, no
packet loss concealment, no resampling, and no echo cancellation in this file —
`getUserMedia` and `RTCPeerConnection` do all of it. That is the whole argument
for shipping a WebRTC adapter rather than telling every client to reinvent this
over a raw socket.

Echo cancellation and noise suppression are requested from the browser and are
the client's responsibility. The server does neither and does not compensate
for their absence.

Everything else is the protocol. The data channel carries the same events a
WebSocket client would send and receive, which is what makes the adapter a
client rather than a second way in.

It also shows two things worth seeing:

- **First received audio-delta latency** after each endpoint, measured when
  the protocol event reaches the page. It is not an audio playout metric.
- **Observed content**, rendered differently from speech on purpose. A person
  looking at narrated screen text is looking at something the agent read, not
  something anyone said.
