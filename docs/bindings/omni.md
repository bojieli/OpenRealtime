# The `omni` binding

One speech-to-speech model does perception, fast cognition, and speech. The
default preset selects an engine floor and engine interaction policies, plus
the background reasoner. These are selections over capabilities, not a claim
that every model called Omni lacks native timing.

```sh
openrealtime serve \
  -binding omni \
  -sidecar "python3 sidecars/qwen3_omni_sidecar.py"
```

Reference sidecars ship for **Qwen3-Omni** and **MiniCPM-o 4.5**. Both accept
`--mock`, which speaks the protocol without loading a model — the way to verify
plumbing before a GPU is involved.

## The engine keeps the floor

Deliberately. The reference configuration is a turn-based generator handed a
turn by an external detector; the Omni label does not constrain every
speech-to-speech model to that capability set. Voice activity detection
mis-endpoints on spelled
identifiers and digit strings — exactly the inputs a tool-using voice agent
depends on getting right. This is a measurable claim rather than an assertion,
and `-floor model` is the other level of it.

## Compose an external interaction policy

`omni.NewWithTextPolicy` adds a policy-only streaming recognizer and an
engine-owned `InteractionModel`. The controller chooses only `listen`,
`speak-through`, `answer`, `interrupt`, `act-silently`, `keep-speaking`, or
`stop-speaking`. It never generates words.

The command-line preset is:

```sh
openrealtime serve \
  -binding omni+text-policy \
  -sidecar "python3 sidecars/qwen3_omni_sidecar.py" \
  -policy-models interaction \
  -policy-model qwen3.6-35b-a3b \
  -sidecar-protocol 2
```

A typed plan carries the act, policy identity, evidence reference, floor
semantics, deadline, and confidence to a protocol-v2 sidecar. A v1 sidecar
receives the compatible legacy translation, but that translation loses typed
evidence and should not be used to measure the interaction seam.

Acts are constrained by the composed executor. In particular,
`speak-through` is not offered unless `concurrent_io` is available. Raw audio
still goes directly to the voice model; policy ASR is evidence for control and
does not turn the foreground into an ASR→LLM→TTS cascade.

## Verify a sidecar before running it

```sh
openrealtime conformance sidecar -- python3 sidecars/qwen3_omni_sidecar.py --mock
```

The suite is the contract. A sidecar that passes it works with the engine
whatever it is written in; one that does not is broken before anybody spends a
GPU-hour finding out. See the frozen [v1 sidecar
protocol](../sidecar-protocol-1.md) and the [typed-act v2
extension](../sidecar-protocol-2.md). Direct pixels and live tool catalogs use
the [v3 multimodal extension](../sidecar-protocol-3.md).

## Authority

A sidecar model is the **fast** provider, and the boundaries do not change
because it lives in another process. A tool call from a sidecar is refused with
a reason it can see, and the background reasoner performs tool calls.

## Resource guidance

The model and the engine's reasoner are separate processes and can be separate
machines: point `-sidecar-address` at a running sidecar rather than spawning
one, which is the right shape when the model is expensive to load and worth
sharing.
