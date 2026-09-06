# Examples

Start with the [official SDK tool-use walkthrough](sdk-client/README.md). It
runs a complete function-call round trip without provider keys, then explains
how to connect the same client to a live server.

| Goal | Example or guide |
| --- | --- |
| Understand client-side tool execution | [SDK weather example](sdk-client/README.md) |
| Talk through a browser | [Quickstart](../docs/quickstart.md) |
| Explore interruption, translation, and screen awareness | [Planned demo gallery](../docs/demos.md) |
| Connect a LiveKit room | [LiveKit agent](../integrations/livekit/README.md) |
| Build a local voice stack | [Local setup](../docs/guides/local-stack.md) |
| Reproduce the meeting evaluation deployment | [Meeting assistant](../docs/guides/meeting-assistant.md) |

The SDK example is the maintained standalone end-to-end client example. The
other entries are application guides or planned recordings, with their own
prerequisites.

## Extend the Go runtime

The stable provider contract is [API v1](../docs/api-v1.md). Deterministic
reference adapters live under [internal/testserver](../internal/testserver),
and binding tests show how to compose providers. There is currently no
standalone, key-free Go application example; the SDK test starts an in-process
reference server for that workflow.

For external model processes, use the [sidecar guide](../docs/sidecars.md).
