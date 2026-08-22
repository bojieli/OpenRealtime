# The `duplex` binding

A full-duplex interaction model listens and speaks at once. Turn-taking,
overlap, and interruption are in its weights, so it owns its floor and the
engine does not second-guess it.

```sh
openrealtime serve \
  -binding duplex \
  -sidecar "python3 sidecars/moshi_sidecar.py"
```

The reference sidecar is **Moshi**.

## What the engine still supplies

Everything except the floor. Trigger, rollout, holding behaviour, admission,
and — the reason this binding exists — the background reasoner. A single model
has no second model and no shared log to put one on.

## Where a background answer splices in

This is the one genuinely open question about the binding, and the sidecar
carries both paths as a flag rather than a rewrite:

```sh
--injection inner-monologue   # write into the text stream the model is generating
--injection handoff           # inject and request a turn
```

**Inner-monologue conditioning** respects a floor the model owns: the answer
becomes context and the model decides for itself when to say it.
**Explicit hand-off** certainly works and is the documented fallback, which is
why this binding ships regardless of how the research resolves.

## Overlap

The engine's barge-in policy is off by default here. A model that owns its floor
handles overlap itself, and the engine cancelling its speech would be the engine
overruling the thing it delegated to. `-floor engine` flips both, which is the
honest way to ask whether a duplex model's own floor beats a measured one.

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

There is no recogniser and no synthesiser to budget for. That is the whole
economy of this binding, and the reason its floor belongs to the model.
