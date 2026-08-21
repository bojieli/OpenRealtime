# The `cascade` binding

The engine owns everything: perception, both cognition providers, action, and
the floor.

```sh
openrealtime serve   # cascade is the default
```

## Why it is the default

It is fully local, needs no third-party account, and is the right first
impression for an open implementation. It is also the honest test of the
interaction plane: a recogniser knows nothing about turn-taking, a language
model knows nothing about the conversation's timing, and a synthesiser knows
nothing at all. Every bit of responsiveness a cascade session exhibits was
manufactured by the runtime.

## Components

| Role | Default | Flags |
| --- | --- | --- |
| Recogniser | Qwen3-ASR | `-asr-url`, `-asr-model`, `-asr-cadence` |
| Fast provider | a local OpenAI-compatible model | `-fast-url`, `-fast-model`, `-fast-max-tokens` |
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
5. The rollout plans: fast answers immediately, slow reasons and acts, and a
   fast step voices what slow produced.
6. Authoritative calls go to whoever executes them — in-process when a
   dispatcher is declared, to the client otherwise.

## Policies worth varying

```sh
-trigger-cadence 200ms        # 50, 100, 200, 400, 800 are the measured levels
-observation-policy stable-partial   # commit a stable prefix before the endpoint
-rollout endpointed-slow-only # what a conventional agent does
-tool-progress                # a spoken status when a tool result lands
```

## Adding video

```sh
openrealtime serve \
  -observers audio+video \
  -vision-model your-vlm \
  -observer-components keyframe+narration
```

A client must negotiate `video.input` before sending frames, and must declare a
source's geometry before sending any. See the
[protocol](../protocol/openrealtime-1.md).

## Resource guidance

A cascade session holds one recogniser stream, one fast continuation, one slow
continuation, and one synthesis stream at a time. On a single GPU, the
recogniser and the fast model are the components that must stay warm; the slow
model is where a hosted provider makes the most sense.

Everything competes for capacity under one admission governor, with three
classes: interactive above speculative preparation above background.
