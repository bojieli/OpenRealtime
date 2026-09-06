# Bindings and capability composition

A binding declares selected ownership and available capabilities. The runtime
supplies the missing pieces. `cascade`, `omni`, and `duplex` are useful presets
and benchmark labels; they are not mutually exclusive model species.

Bindings are the adapter layer beneath the versioned architecture catalog.
For a durable deployment or experiment, prefer an exact architecture reference:

```sh
openrealtime architectures list
openrealtime serve -architecture cascade.controlled@3
```

The catalog derives the structural binding, ownership, capability requirements,
interaction boundary, and sidecar protocol. Provider flags still select actual
models and endpoints. Direct `-binding` launches remain useful compatibility
and extension surfaces, but do not attest an immutable architecture revision.

| Binding | Models | Best for |
| --- | --- | --- |
| [`cascade`](cascade.md) | recogniser + language model + synthesiser | selecting each component; local voice services with a hosted reasoner by default |
| [`upstream`](upstream.md) | any Realtime-compatible endpoint | a hosted voice stack with a background reasoner behind it |
| [`omni`](omni.md) | Qwen3-Omni, MiniCPM-o 4.5 | turn generation with engine floor |
| `omni+text-policy` | any turn generator plus policy ASR | speech-to-speech foreground with engine interaction controller |
| [`duplex`](duplex.md) | Moshi | preset selecting native interaction and floor |

These binding presets keep background reasoning in the engine. Graph-native
compositions can arrange cognition roles differently; see
[Architecture](../architecture.md).

## Choosing

**Start with `upstream`** for a first conversation without local model servers.
The [quickstart](../quickstart.md) configures both the voice and background
reasoner with one Gemini credential.

**Use `cascade`** when you want to choose speech recognition, the foreground
model, and speech synthesis independently. It is the CLI default, but its
services must already be running. The background reasoner defaults to Gemini;
the [local guide](../guides/local-stack.md) explains how to make it local too.

**Use `omni`** when you want a single speech-to-speech generator and an engine
floor. Add the `omni+text-policy` composition when a separate interaction model
should choose typed acts from a policy-only streaming transcript. Raw audio
still goes straight to the speech model; the recognizer is control-plane
evidence, not a foreground ASR→LLM→TTS path.

**Use the `duplex` preset** when a model exposes concurrent I/O, native floor,
and native interaction and you want all three selected. A hybrid experiment can
retain those capabilities while selecting engine interaction or engine floor;
construct a `sidecarbinding.Spec` instead of adding another binding species.

The generic capability vector is:

| Capability | Independent question |
| --- | --- |
| `turn_generation` | can an explicit request produce one model turn? |
| `concurrent_io` | can input continue while output is generated? |
| `native_floor` | can the model establish speech/turn boundaries? |
| `native_interaction` | can the model choose listen/speak/interrupt acts itself? |
| `interaction_acts` | can it accept a typed external policy plan? |
| `transcription` | can the stack expose what was heard? |
| `text_injection` | can background state enter context without posing as user speech? |

Any combination is representable. Ownership selects which available provider
is active for a session.

The evidence selected into that owner is another independent vector:
transcript, acoustic activity, silence clock, conversation state, tool state,
speaker identity, addressing, visual description, direct visual input, and
native model state. The runtime reports the exact active set. An external
catalog can compose supported evidence channels without creating another Go
binding; a new immutable definition revision records the changed selection.

Selected interaction mechanisms form a third vector: predicates, external text
policy, native interaction, and remote interaction. One selector uses `single`
arbitration. A supported multi-selector composition must name its arbiter; the
current component topology implements `predicate-floor`, where predicates keep
endpoint and overlap decisions and the text policy owns the other acts. Merely
installing two policies never lets both race to speak.

For a combination outside the named presets, use the generic sidecar binding:

```sh
openrealtime serve \
  -binding sidecar \
  -sidecar "python3 my_model_sidecar.py" \
  -sidecar-capabilities audio-input,audio-output,turn-generation,concurrent-io,native-floor,native-interaction,interaction-acts,text-injection \
  -interaction-owner engine \
  -floor model \
  -sidecar-protocol 2
```

This example retains both native capabilities but selects an engine
interaction controller and the model's floor. An engine controller additionally
requires `-policy-models interaction`, `-policy-model`, and a configured ASR
provider for its live evidence.

## Observers, and the default set

Only `cascade` runs its own perception, so only `cascade` has observers a
session can choose between. Its set is a deployment decision rather than a
constant - `-observers` selects what the process configures, and a session that
names nothing gets all of it:

| Binding | Default set | Selectable per session |
| --- | --- | --- |
| `cascade` | `audio`, plus `video` when `-observers audio+video` configured it | yes, by name |
| `omni` | the model's own perception | no |
| `duplex` | the model's own perception | no |
| `upstream` | the remote provider's own perception | no |

A session selects with `openrealtime.observers` in `session.update`, and gets
back the set that was actually enabled. Naming a subset of what exists gets
that subset; naming something the deployment does not have drops it. Naming
observers against a binding that has none is **refused**, not quietly accepted
- the alternative is a client told it enabled video observation on a binding
that will never produce one, which is indistinguishable from the feature
working until someone notices nothing was ever observed.

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
