# Bindings

A binding declares which subsystems the model owns. The runtime supplies the
rest.

| Binding | Models | Best for |
| --- | --- | --- |
| [`cascade`](cascade.md) | recogniser + language model + synthesiser | the default: fully local, every policy in your hands |
| [`upstream`](upstream.md) | any Realtime-compatible endpoint | a hosted voice stack with a background reasoner behind it |
| [`omni`](omni.md) | Qwen3-Omni, MiniCPM-o 4.5 | one speech-to-speech model, engine-owned timing |
| [`duplex`](duplex.md) | Moshi | genuine full duplex, model-owned floor |

One column never varies. **Slow cognition is always the engine's**, because no
foreground model provides it, and supplying it over a shared trajectory is what
this project adds to whatever stack it is given.

## Choosing

**Start with `cascade`.** It is the default because it is fully local, needs no
third-party account, and gives you every interaction policy to vary. It is also
the honest test of the control plane: none of its models knows anything about
the conversation's timing, so whatever responsiveness you observe was
manufactured by the runtime.

**Use `upstream`** when you already have a voice stack you like, or no GPU. It
is one flag and one credential.

**Use `omni`** when you want a single speech-to-speech model but still want the
engine deciding *when*. An Omni model leans on voice activity detection, which
mis-endpoints on spelled identifiers and digit strings — exactly the inputs a
tool-using voice agent depends on getting right — so the engine keeps the floor
by default. `-floor model` is the comparison.

**Use `duplex`** when overlap and interruption matter more than anything else.
A full-duplex model has them in its weights, and the engine stops asking it to
take turns.

## What every binding gets

- The background reasoner over a shared trajectory, with tool execution.
- Every interaction policy the binding does not delegate to its model.
- The same session-level test suite. A change to the interaction plane that
  breaks `omni` while leaving `cascade` green is precisely the failure that
  suite exists to catch.

## Fast and slow on every binding

Fast/slow is configurable everywhere, including `duplex` through its hand-off
fallback. What varies is only who supplies the fast provider:

```sh
-rollout fast-only            # no background reasoner; the control condition
-rollout fast+slow            # the default
-rollout endpointed-slow-only # only the reasoner, voiced afterwards
-tool-progress                # let a completed tool result trigger a spoken status
```
