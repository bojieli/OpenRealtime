# Browser demo

A voice session in a browser, in one HTML file with no build step and no
dependencies.

```sh
# One terminal: the server, with the WebRTC adapter enabled.
openrealtime serve --webrtc-listen 127.0.0.1:8766

# Another: serve this directory over HTTP.
python3 -m http.server 8080 --directory examples/browser
```

Open `http://127.0.0.1:8080/` and press Connect. Point it somewhere else with
`?adapter=http://host:port/v1/realtime`.

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

- **First-audio latency** after each endpoint, measured in the browser, which
  is the number a person actually experiences.
- **Observed content**, rendered differently from speech on purpose. A person
  looking at narrated screen text is looking at something the agent read, not
  something anyone said.
