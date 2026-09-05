# Maintained examples

## The official OpenAI client, unchanged

[`sdk-client`](sdk-client/README.md) is the published `@openai/agents-realtime`
SDK, configured the way its own documentation says to configure it, pointed at
an OpenRealtime server. Nothing in it is written against OpenRealtime. It is
the executable form of the compatibility claim: the SDK completes a tool-using
session over WebSocket and over WebRTC, and the release gate runs it
(`scripts/check.sh` and `local.sdk.official` in the release matrix).

```sh
cd examples/sdk-client
npm install
node websocket.mjs ws://127.0.0.1:8765/v1/realtime
```

The Go test beside it, `go test ./examples/sdk-client/`, starts a server with
in-process providers, so the gate needs Node and a network-free npm cache but
no model, credential, or microphone.

## Writing against the component API

The stable Go contract lives at [`api/v1`](../api/v1) and is documented in
[Component API v1](../docs/api-v1.md). The key-free reference adapters used
by the gates are the fakes under `internal/testserver`, and every binding's
own tests show how a provider is composed behind the `Binding` seam. A
standalone key-free example program was retired with the legacy experiment
scaffolding and has not been replaced; the SDK example above is the one
maintained end-to-end example.
