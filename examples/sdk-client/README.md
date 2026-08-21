# OpenAI's own client, against this server

The project claims an official OpenAI Realtime client connects unchanged. This
is the thing that checks it.

```sh
npm install
node websocket.mjs ws://127.0.0.1:8765/v1/realtime
```

```text
PASS  the official SDK connects to an OpenRealtime server
PASS  the server completes the SDK's session handshake
PASS  the server accepted the session the SDK configured
PASS  the SDK's tool was called and executed by the SDK
PASS  a response completed
PASS  the SDK received the tool call on the event it expects
PASS  speech came back as audio and as a transcript
PASS  no protocol error was reported at any point
```

Nothing here is written against OpenRealtime. It is
[`@openai/agents-realtime`](https://www.npmjs.com/package/@openai/agents-realtime)
as published, configured the way its own documentation says to configure it,
with one URL pointed somewhere else:

```js
const transport = new OpenAIRealtimeWebSocket({ url, useInsecureApiKey: true });
const session = new RealtimeSession(agent, { transport });
```

It runs a **tool-using** turn, because tool use is where an implementation is
most likely to diverge: the call goes out on one event, the result comes back
as a conversation item, and the SDK is particular about the shape of both.

## Both transports

The SDK's WebRTC transport only runs in a browser — it needs
`RTCPeerConnection` and `getUserMedia`, and there is no Node equivalent. So
that half is bundled with esbuild and driven in Chromium:

```sh
node browser.mjs http://127.0.0.1:8766/v1/realtime
```

The page is served from **a different origin than the adapter**, because that
is what every real deployment looks like: the adapter is a port on a server and
the application is a site. A same-origin test would pass while proving nothing
about whether anyone can actually do this — which is exactly the trap the first
version of this fell into, and how the missing CORS support was found.

That means the adapter has to be told which origins may reach it:

```sh
openrealtime serve -webrtc-listen 127.0.0.1:8766 \
  -webrtc-allow-origin https://your-app.example.com
```

Empty is the default and answers no browser at all, which is right for a
server-to-server deployment. The endpoint has no credential of its own and
starting a session is all it does, so a wildcard would let any page a user
visits open a session against any adapter their browser can route to. `*` is
accepted for local development and says what it is.

## Run it in the gate

```sh
go test ./examples/sdk-client/
```

Skipped when Node, Chromium, or `node_modules` is missing, so the gate still
runs offline. `npm install` here is what turns it on.

## What it found

Four defects, all of which had gone unnoticed because nothing had ever run an
official client against this server:

**A null the specification asks for and the schema forbids.** The SDK sends
`noise_reduction: null` on every connection. OpenAI's own specification gives
that field `"type": "object"`, `"default": null`, and a description saying it
can be set to null to turn the feature off — three statements that do not
agree. Converted to JSON Schema the type won, so a validator built from it was
stricter than the API it describes, and rejected OpenAI's own client while
claiming compatibility with it. The generator now reconciles the two where the
specification declares null as the default.

**An unsupported option that took the whole session with it.** The SDK defaults
to `semantic_vad`. This server has server VAD, and refused the entire
`session.update` — discarding the instructions, the tools, and the audio
formats that arrived in the same event, so an unmodified client could not
configure a session at all. It now runs on the detector it has and reports that
in `session.updated`, where a client can see it did not get what it asked for.

**Turn detection parameters read as zero.** A client that names a detector
without naming its numbers is asking for the deployment's. Reading absent as
zero produced a gate with no silence duration, which the gate itself refuses —
so a session failed on a field the client never set.

**A WebRTC adapter no browser could reach.** The SDP exchange carries a bearer
credential and an `application/sdp` body, so a browser preflights it; the
adapter answered 405 and had no notion of an allowed origin. Since the SDK's
WebRTC transport is browser-only, the WebRTC half of the compatibility claim
was unreachable by construction.
