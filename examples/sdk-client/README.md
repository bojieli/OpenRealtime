# Tool use with the official Realtime SDK

This example connects the published `@openai/agents-realtime` SDK to
OpenRealtime, asks about the weather, executes a client-side function, and
receives an audio response with a transcript. The `get_weather` tool returns
fixed example data: 14 degrees and rain. It does not contact a weather service.

Use it to understand the tool-call round trip or check a client integration.
For a microphone conversation, start with the [browser quickstart](../../docs/quickstart.md).

## Prerequisites

- Go 1.25+ and the cloned repository.
- Node.js 22+ and npm.
- Chromium for the WebRTC check; WebSocket does not need a browser.

## Run a complete example without provider keys

From the repository root:

```bash
cd examples/sdk-client
npm ci
cd ../..
go test -v -count=1 ./examples/sdk-client/
```

The Go test starts a real local server with deterministic in-process providers,
then runs the SDK through WebSocket and, when Chromium is available, WebRTC.
No model weights, GPU, microphone, or provider key are needed. Installing npm
and Go dependencies may require network access.

Look for the SDK's `PASS` lines for the handshake, tool execution, completed
response, and audio/transcript output, followed by passing Go tests. A `SKIP`
means a dependency was unavailable; it is not a successful transport check.
Set `CHROMIUM` to the browser executable's path if it is not discovered.

## Follow the round trip

1. `RealtimeAgent` declares `get_weather` and its argument schema.
2. `RealtimeSession` connects and sends the agent's session settings.
3. The client sends “What is the weather in Cambridge? Use your tool.”
4. The server emits a function call; the SDK runs the local `execute` callback.
5. The SDK returns the tool result and the server produces a response.
6. The script records transcript text and counts received audio bytes.

The Node script does not play the returned audio through your speakers. Its
output verifies receipt of audio and prints the transcript.

The connection uses the SDK's standard URL option:

```js
const transport = new OpenAIRealtimeWebSocket({ url, useInsecureApiKey: true });
const session = new RealtimeSession(agent, { transport });
```

See [websocket.mjs](websocket.mjs) for the complete configuration, event
handlers, tool, and checks. The SDK package itself is unmodified.

## Connect to your own server

Start a configured OpenRealtime server using the
[quickstart](../../docs/quickstart.md), then run from this directory:

```bash
node websocket.mjs ws://127.0.0.1:8765/v1/realtime
```

If the server uses authentication, set `OPENREALTIME_TOKEN` in the client
terminal to the same token. You can override the model label with
`OPENREALTIME_MODEL` and pass a different tool-using prompt as the second
argument:

```bash
node websocket.mjs ws://127.0.0.1:8765/v1/realtime \
  "What is the weather in London? Use your tool."
```

Live results depend on the chosen models. The script includes a regression
assertion that `semantic_vad` is refused by name. A deployment that supports
that setting can complete the conversation and still fail this particular
assertion. The deterministic test above is the reference configuration for
all assertions; this script is not a universal health probe.

## WebRTC

The Go test builds and runs the browser example automatically. For a manual
run against an existing adapter, build the browser bundle from this directory:

```bash
./node_modules/.bin/esbuild webrtc.mjs \
  --bundle --format=esm --outfile=dist/webrtc.js
node browser.mjs http://127.0.0.1:8766/v1/realtime/calls
```

The driver uses headless Chromium with simulated microphone input and serves
its page on a temporary loopback origin. For this local test, the adapter must
allow that origin; a local-only adapter can use `-webrtc-allow-origin '*'`.
Production deployments should name their actual application origins. See
[Transports](../../docs/transports.md#reaching-the-adapter-from-a-browser).

## Troubleshooting

| Symptom | Check |
| --- | --- |
| The Go test skips the SDK | Run `npm ci` in this directory and ensure Node is on `PATH`. |
| WebRTC is skipped or cannot start | Install Chromium or set `CHROMIUM` to its executable. |
| WebSocket connects but no tool result arrives | Check the selected model, provider credentials, and server errors. |
| Browser SDP exchange fails | Check the adapter URL, token, and allowed origin. |
| Audio check passes but nothing is audible locally | Expected for the Node script; it counts audio bytes without playing them. |
| Only the unsupported-setting assertion fails on a live server | Check whether that deployment accepts `semantic_vad`. |

## What this establishes

These checks validate a tool-using SDK session on two transports. They do not
establish full hosted API parity, live-provider reliability, or model quality.
See the [compatibility reference](../../docs/openai-realtime-compatibility.md)
for supported events and limitations, and the
[release matrix](../../docs/release-validation.md) for provisioned checks.
