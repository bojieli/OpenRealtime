# The `omni` binding

One speech-to-speech model does perception, the voice, and speech. The engine
supplies everything about *when*, and the background reasoner.

```sh
openrealtime serve \
  -binding omni \
  -sidecar "python3 sidecars/qwen3_omni_sidecar.py"
```

Reference sidecars ship for **Qwen3-Omni** and **MiniCPM-o 4.5**. Both accept
`--mock`, which speaks the protocol without loading a model — the way to verify
plumbing before a GPU is involved.

## The engine keeps the floor

Deliberately. An Omni model is a turn-based generator handed a turn by an
external detector, and voice activity detection mis-endpoints on spelled
identifiers and digit strings — exactly the inputs a tool-using voice agent
depends on getting right. This is a measurable claim rather than an assertion,
and `-floor model` is the other level of it.

## Verify a sidecar before running it

```sh
openrealtime conformance sidecar -- python3 sidecars/qwen3_omni_sidecar.py --mock
```

The suite is the contract. A sidecar that passes it works with the engine
whatever it is written in; one that does not is broken before anybody spends a
GPU-hour finding out. See the [sidecar protocol](../sidecar-protocol-1.md).

## Authority

A sidecar model is the **fast** provider, and the boundaries do not change
because it lives in another process. A tool call from a sidecar is refused with
a reason it can see, and the background reasoner performs tool calls.

## Resource guidance

The model and the engine's reasoner are separate processes and can be separate
machines: point `-sidecar-address` at a running sidecar rather than spawning
one, which is the right shape when the model is expensive to load and worth
sharing.
