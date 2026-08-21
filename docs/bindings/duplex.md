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
