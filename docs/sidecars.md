# Integrate an external model

A sidecar runs a model in a separate process and exchanges messages with
OpenRealtime. Use it for Python model libraries, GPU runtimes, or independently
deployed services. For an existing hosted provider, first check the
[provider catalog](providers.md); a built-in adapter may already be available.

## Choose the contract for your launch path

| Launch path | Contract | What it carries |
| --- | --- | --- |
| Binding-based audio model | [v1](sidecar-protocol-1.md) | Audio, transcripts, response requests, interruption, and context injection |
| Binding-based model with an external interaction policy | [v2](sidecar-protocol-2.md) | v1 plus typed conversational acts |
| Binding-based model with direct vision and changing tools | [v3](sidecar-protocol-3.md) | v2 plus image payloads and tool-catalog updates |
| Graph-native external model | [v4](sidecar-protocol-4.md) | Typed element ports, envelopes, and exact implementation identity |

Versions 2 and 3 extend the audio contract. Version 4 reuses v1's byte framing
but defines a different element contract; it does not inherit the v1–v3 message
set. A v4 process needs a graph launch profile. The older `-binding` presets do
not select it automatically.

## Verify the connection before loading a model

From the repository root, after building the CLI:

```bash
./openrealtime conformance sidecar -- python3 sidecars/qwen3_omni_sidecar.py --mock
```

This exercises the reference sidecar without downloading weights or using a
GPU. It checks the process protocol; it does not establish model quality,
latency, or a working GPU deployment.

For a real model, follow the relevant [omni](bindings/omni.md) or
[duplex](bindings/duplex.md) setup. Graph integrations should start with
[assembly](graph-native-assembly.md) and
[writing a v4 sidecar](sidecar-protocol-4.md#writing-a-version-4-sidecar).

## Implementation checklist

1. Implement the selected handshake and refuse incompatible versions.
2. Declare only capabilities the running model can provide.
3. Follow the specified payload lengths, sample rates, and flush behavior.
4. Handle cancellation and shutdown without publishing stale output.
5. Treat tool requests as requests; execution authority belongs to the host.
6. Run the version-specific conformance checks and a real model turn.

Keep diagnostics off a stdout stream used for protocol frames. Document model
dependencies, hardware requirements, and startup commands with your adapter.
