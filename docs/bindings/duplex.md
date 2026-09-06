# The `duplex` binding

The `duplex` preset selects a model's concurrent I/O, native floor, and native
interaction capabilities. Those are three capabilities that happen to be
present in Moshi, not a single indivisible type.

```sh
openrealtime serve \
  -binding duplex \
  -sidecar "python3 sidecars/moshi_sidecar.py"
```

The reference sidecar is **Moshi**.

## What the engine still supplies

The engine supplies the background reasoner, trajectory, tool authorization,
audit, and session lifecycle independently of the foreground model.

Engine interaction and engine floor remain valid controlled selections over a
duplex-capable model. They do not require deleting its native capabilities or
adding a new binding package; use a generic `sidecarbinding.Spec` with the
desired ownership vector.

## Where a background answer splices in

The sidecar exposes two strategies for adding background results. Their
interaction quality depends on the model and should be evaluated for your task:

```sh
--injection inner-monologue   # write into the text stream the model is generating
--injection handoff           # inject and request a turn
```

**Inner-monologue conditioning** respects a floor the model owns: the answer
becomes context and the model decides for itself when to say it.
**Explicit hand-off** injects the result and requests a turn. It is the
documented fallback when implicit conditioning is unsuitable.

## Overlap

The engine's barge-in policy is off by default here. A model that owns
interaction handles overlap itself, and the engine cancelling its speech would
overrule the thing it delegated to. `-floor engine` changes only the selected
floor owner. To select engine interaction as well, use the generic `sidecar`
preset with `-interaction-owner engine` and declare `interaction-acts`; the
model retains its native capabilities in the reported vector either way.

With engine floor and model interaction, an endpoint is sent as `commit`, not
`respond`: the selected floor establishes that the utterance ended, while the
native interaction policy still decides whether ending it should produce an
answer.

## Resource guidance

A duplex session holds one full-duplex model in a sidecar process and one
background reasoner. The model is the expensive half and the one that must stay
warm: it is doing recognition, turn-taking, and synthesis at once, and it is
answering in real time, so it wants a GPU to itself. The reasoner is not on the
voice path and is the natural place for a hosted provider.

As with `omni`, the model and the engine can be separate machines - point
`-sidecar-address` at a running sidecar rather than spawning one, which is the
right shape when a model is expensive to load and worth sharing between
sessions.

There is no separate recogniser or synthesiser to budget for in this preset.
That economy does not logically force floor or interaction ownership; those
are explicit selections and can be changed independently for measurement.
