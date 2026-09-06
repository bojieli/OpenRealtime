# The `cascade` binding

The engine owns everything: perception, both cognition providers, action, and
the floor.

```sh
openrealtime serve   # cascade is the default
```

## Why it is the default

Cascade exposes recognition, language generation, speech synthesis, and
interaction policies as separate components. This makes it useful for model
integration and controlled experiments.

The default voice services are local, but background reasoning uses Gemini.
Start those services and configure its credential before running the command
above. For an all-local setup, follow the
[local stack guide](../guides/local-stack.md). For a first conversation without
model servers, use the hosted [quickstart](../quickstart.md).

## Components

| Role | Default | Flags |
| --- | --- | --- |
| Recogniser | Qwen3-ASR | `-asr-url`, `-asr-model`, `-asr-cadence` |
| Voice/fast provider | a local OpenAI-compatible model | `-fast-url`, `-fast-model`, `-fast-max-tokens` |
| Visual reflex | disabled | `-visual-reflex-provider`, `-visual-reflex-model`, `-visual-reflex-timeout` |
| Slow provider | Gemini | `-slow-provider`, `-slow-model`, `-slow-effort`, `-slow-max-tokens` |
| Speech | an OpenAI-compatible endpoint | `-tts-url`, `-tts-model`, `-tts-voice` |

A new recogniser is created per utterance, so recogniser state cannot leak
between turns.

## The turn

1. Audio arrives. The acoustic gate answers "is the user speaking" — for the
   duplex state as well as for perception, which is why it sits upstream of the
   observer rather than inside it.
2. The recogniser produces typed revisions. The trigger and preparation
   policies see every one of them.
3. The floor decides the turn ended. A canonical observation commits.
4. The deferral gate decides whether to act now. If the agent is still audible,
   the observation waits and playback completion wakes it.
5. The default rollout schedules a foreground answer and background reasoning
   for the observation. Background results re-enter as events; the foreground
   presents them when the interaction policy permits.
6. Authoritative calls go to whoever executes them — in-process when a
   dispatcher is declared, to the client otherwise.

## Policies worth varying

```sh
-trigger-cadence 200ms        # 50, 100, 200, 400, 800 are the measured levels
-observation-policy stable-partial   # answer a stable prefix before the endpoint
-rollout endpointed-slow-only # what a conventional agent does
-tool-progress                # a spoken status when a tool result lands
-preparation continuous       # speculate before the endpoint; costs tokens
-barge-in sustained           # hold the floor briefly instead of yielding at once
```

`-observation-policy stable-partial` also changes the deferral policy, because
it has to: answering a partial means acting while the user is still speaking,
and the shipped deferral waits for them to stop. The server composes the pair;
the binding refuses the incoherent one rather than picking a side.

`-preparation continuous` starts a fast and a slow continuation on every
changed revision. Nothing it produces is committed, spoken, or dispatched — it
is adopted at the endpoint only if the canonical observation says the same
thing the speculation answered, and discarded otherwise. That makes it safe to
be wrong about and expensive to be wrong about, which is why it is off by
default. `-preparation-slow-pace` bounds how often the slow provider may be
speculatively started.

## Policy models

```sh
openrealtime serve \
  -policy-models backchannel,turn-projection,overlap \
  -policy-model qwen-3b -policy-url http://127.0.0.1:8002/v1
```

Each is a small model with a short prompt and an enumerated output, and each
degrades to a rule when unconfigured: backchannel to off, turn projection to
silence-only endpointing, overlap classification to unclassified. They are
admitted at interactive class when `-compute-capacity` is set.

Turn projection reaches the conversation through the floor: a projection ends
the turn before silence does, and the endpoint is recorded as projected so that
cutting someone off and answering late are never averaged together.

Backchannel decides off the audio path — a model call that made the recogniser
wait would trade what makes a system feel alive for what makes it feel slow —
and one decision is in flight at a time.

### Composing interaction controllers

An interaction policy is not automatically the only interaction controller.
The selected controller vector and its arbitration rule make that distinction
explicit:

| Selection | Endpoint and overlap | Other typed interaction acts | Arbitration |
| --- | --- | --- | --- |
| predicates (P) | shipped predicates | shipped predicates | `single` |
| text policy (T) | text policy | text policy | `single` |
| predicates + text policy (C) | shipped predicates | text policy | `predicate-floor` |

Pure T routes both floor and barge-in through the same typed act model. At bare
acoustic onset it has no transcript evidence yet, so it keeps speaking until a
transcript revision permits a semantic `keep-speaking` or `stop-speaking`
decision. C deliberately retains the immediate barge-in and endpoint predicates
while the text policy owns durable semantic, visual, quiet, and silent-tool
acts. Runtime liveness bounds, stale-decision refusal, duplicate suppression,
and authorization remain safety fences; they can reject an act but are not
additional conversational controllers.

Use immutable definitions such as `cascade.text-policy@3` and
`cascade.composed-policy@1` to select these arrangements. Their controller and
arbitration fields drive generic component-topology construction and are
attested in live status. A contradictory `-interaction-floor` override is
refused instead of silently changing the treatment. Adding another supported
combination therefore means catalog data plus a named generic arbitration rule,
not a cascade/Omni/duplex species or an architecture-ID code branch.

## Adding video

```sh
# A session narrator: the session's own model narrates as a side-output, which
# is the configuration the measured result used and the one that avoids putting
# a second model in the loop. It takes the fast provider's endpoint and model,
# so there is nothing else to name - but that model has to be able to see.
openrealtime serve \
  -observers audio+video -fast-sees \
  -observer-components keyframe+narration

# A dedicated narrator: a separate vision-language model. Required for a
# binding whose own model cannot see, which `duplex` cannot by construction.
openrealtime serve \
  -observers audio+video \
  -narrator dedicated -vision-model your-vlm \
  -observer-components keyframe+narration
```

`-fast-sees` and `-slow-sees` declare that a model accepts images. They default
to off, and the asymmetry is why: handing an image to a text-only model fails
the turn outright, while withholding one from a model that could have used it
costs only what the narration does not carry.

A client must negotiate `video.input` before sending frames, and must declare a
source's geometry before sending any. See the
[protocol](../protocol/openrealtime-1.md).

For cue-sensitive browser control, the cascade can open a bounded reflex lane:

```sh
openrealtime serve \
  -profile voice+vision -computer-use \
  -visual-reflex-provider vllm \
  -visual-reflex-url http://127.0.0.1:8004/v1 \
  -visual-reflex-model qwen-vl-fast-local
```

This is a separate model role; it does not change the fast voice provider or
the slow reasoner. The reflex sees only the latest user task, newest image per
source, and exact standard direct `computer.*` actions admitted by the live
session filter. It produces exactly one of three typed outcomes:

- `act`: exactly one offered tool call;
- `wait`: no effect, awaiting newer visual evidence;
- `abstain`: delegate to the ordinary fast/slow rollout.

The default bound is 48 output tokens and a 650 ms hard deadline. Malformed
output and timeout are safe abstentions. `computer.screenshot` and
`computer.wait` remain slow-only observation control. Server-owned actions run
through the declared browser target; client-owned computer environments must
declare a target and `confirm: never` to enter the reflex lane. Every call
still crosses trajectory authority, confirmation, the action ledger, and
audit. Slow remains active for planning and consumes fast action results from
the shared trajectory.

`keyframe` is the direct visual route for the reflex and is selected by the
profile unless the operator explicitly chooses another component set.
Narration is useful persistent context, but a narration-only reflex waits for
the narrator and often lacks exact pixel coordinates; set-of-mark grounding is
the robust browser alternative when coordinate-producing vision is weak.

`-fast-computer-use` remains compatible for deployments that intentionally use
the voice model for bounded actions. The independent visual role is preferred:
model choice and timeout can change without touching voice-only behavior.

## Resource guidance

A voice cascade session holds one recogniser stream, one fast continuation,
one slow continuation, and one synthesis stream at a time. A voice+vision
session may additionally hold the optional reflex model. On a single GPU, the
recogniser, voice, and reflex are the components that must stay warm; the slow
model is where a hosted provider makes the most sense.

Everything competes for capacity under one admission governor, with three
classes: interactive above speculative preparation above background.
