# Run a local voice and vision stack

This guide configures the default `cascade` binding: speech recognition, a
fast conversational model, and speech synthesis under your control. It also
shows how to keep background reasoning local and how to add the optional visual
reflex.

> [!NOTE]
> OpenRealtime orchestrates model endpoints; it does not bundle weights or
> silently download them. Start each model server yourself, then point the
> runtime at its explicit URL.

## Before you start

You need Go 1.25+, a browser for the companion, and independently running model
services. Hardware and GPU memory depend on the chosen models; the runtime
itself connects to their endpoints and does not require a GPU.

Decide whether only the voice services should be local or whether background
reasoning must also stay local. The default reasoner is hosted, so use
[Make the reasoner local too](#make-the-reasoner-local-too) for an all-local
configuration. This guide configures endpoints; follow each model server's
installation instructions for its dependencies and weights.

## The default local voice path

From the repository root, build the runtime:

```bash
go build -o openrealtime ./cmd/openrealtime
```

The default cascade expects these services:

| Role | Default | Main flags |
| --- | --- | --- |
| Streaming recogniser | Qwen ASR at `http://127.0.0.1:8001` | `-asr-provider`, `-asr-url`, `-asr-model` |
| Fast voice model | vLLM at `http://127.0.0.1:8000/v1` | `-fast-provider`, `-fast-url`, `-fast-model` |
| Speech synthesis | OpenAI-compatible speech route at `http://127.0.0.1:8081/v1/audio/speech` | `-tts-provider`, `-tts-url`, `-tts-model`, `-tts-voice` |
| Background reasoner | Gemini | `-slow-provider`, `-slow-url`, `-slow-model` |

Once the three local voice services are listening, start the browser stack:

```bash
export GEMINI_API_KEY="your-key"
./openrealtime companion
```

This keeps ASR, the foreground LLM, and speech synthesis local. Only the
background reasoner is hosted in the default configuration.

Every command below that changes a provider names `-binding cascade` first, and
the reason is worth knowing once rather than rediscovering at each one. With no
explicit server composition, `companion` assembles the twelve-scenario room
pipeline, and that strict launch profile supersedes provider selection - `serve`
refuses to start rather than accept a provider flag it would have to ignore.
Naming the binding is what says "compose the cascade this page describes, from
these flags". `-config` and `-launch-profile` say the same thing in their own
way; `-binding upstream` is refused, because the room requires an OpenRealtime
pipeline rather than a hosted endpoint.

The repository includes a reproducible SenseVoice preparation path:

```bash
./scripts/prepare-sensevoice.sh --serve
```

This script requires a working system PyTorch installation, downloads the
SenseVoice dependencies and weights, and serves on port 8002. In another
terminal, select it explicitly with
`./openrealtime companion -- -binding cascade -asr-provider sensevoice`. The
foreground and TTS
services must still be running. See
[the deployment notes](../../deploy/sensevoice/README.md) for model-path setup.
Other streaming and batch recognizers use the same runtime surface;
list them with:

```bash
./openrealtime providers -role asr
```

## Make the reasoner local too

Point the slow role at any local endpoint that speaks OpenAI-compatible Chat
Completions:

```bash
./openrealtime companion -- \
  -binding cascade \
  -slow-provider openai-compatible \
  -slow-url http://127.0.0.1:8010/v1 \
  -slow-model your-reasoner
```

No provider credential is needed for an unauthenticated loopback endpoint. If
your endpoint requires one, name the environment variable without putting its
value on the command line:

```bash
export LOCAL_REASONER_API_KEY="your-key"

./openrealtime companion -- \
  -binding cascade \
  -slow-provider openai-compatible \
  -slow-url http://127.0.0.1:8010/v1 \
  -slow-model your-reasoner \
  -slow-token-env LOCAL_REASONER_API_KEY
```

You may also use one local server for both fast and slow cognition by selecting
different model names or reasoning budgets on the same base URL.

## Replace any component

Provider roles are independent. For example, a local voice can use Anthropic
for background reasoning:

```bash
export ANTHROPIC_API_KEY="your-key"

./openrealtime companion -- \
  -binding cascade \
  -slow-provider anthropic
```

Or replace only the recogniser while leaving the rest of the stack unchanged:

```bash
./openrealtime companion -- \
  -binding cascade \
  -asr-provider whisper \
  -asr-url http://127.0.0.1:8003/v1 \
  -asr-model openai/whisper-large-v3-turbo
```

Use the live catalog instead of copying a model name from old notes:

```bash
./openrealtime providers
./openrealtime providers -probe vllm -url http://127.0.0.1:8000/v1
```

`providers -probe` asks an endpoint what it currently serves and fails when a
catalog default has gone stale. The [provider reference](../providers.md)
documents credential precedence, dialects, reasoning controls, voices, and
endpoint overrides.

### A note about reasoning models in the fast role

The default fast provider is `vllm` because the default port is conventionally
vLLM and the adapter can explicitly disable thinking. A reasoning model with
thinking left on may spend the conversational latency budget deliberating
instead of answering.

Point `-fast-provider` at `openai-compatible` for a generic Chat Completions
server. OpenRealtime strips reasoning content from speech, so a misconfigured
fast model will not read private deliberation aloud, but it may still answer
slowly or exhaust its output budget before reaching the response.

## Voice-only and voice + vision profiles

The default profile is voice-only:

```bash
./openrealtime serve -profile voice
```

`voice` is an identity profile: it preserves the direct server configuration's
observers, providers, prompts, policies, limits, tools, and rollout.

The `voice+vision` profile adds a separate silent visual reflex without
changing the voice path:

```bash
./openrealtime companion -- \
  -binding cascade \
  -profile voice+vision \
  -computer-use \
  -visual-reflex-provider vllm \
  -visual-reflex-url http://127.0.0.1:8004/v1 \
  -visual-reflex-model qwen-vl-fast-local \
  -slow-provider google
```

Naming `-visual-reflex-model` enables the reflex and implies `voice+vision`
unless `-profile` was set explicitly. The shipped limits are 96 output tokens
and a 650 ms deadline; explicit flags always win.

The reflex receives only:

- the latest user task;
- the newest retained image for each source;
- exact, direct, standard computer-action schemas admitted for that turn.

One typed result means `act`, `wait`, or `abstain`. Malformed output, timeout,
and abstention fall through to the ordinary rollout. The controller is silent
and cannot replace the voice. Audio-only sessions do not instantiate it.

With the reflex enabled, `keyframe` is the default video observer so pixels can
reach the reflex without waiting for a narration model. Select both durable
narration and direct keyframes when the added model call is worthwhile:

```bash
./openrealtime companion -- \
  -binding cascade \
  -profile voice+vision \
  -computer-use \
  -visual-reflex-provider vllm \
  -visual-reflex-url http://127.0.0.1:8004/v1 \
  -visual-reflex-model qwen-vl-fast-local \
  -observer-components keyframe+narration \
  -vision-provider openai-compatible \
  -vision-url http://127.0.0.1:8005/v1 \
  -vision-model your-vision-model \
  -slow-provider google
```

Read the [protocol](../protocol/openrealtime-1.md) and [safety
model](../safety.md) before giving a visual model effect authority. In
particular, screenshots remain untrusted observations, targets are explicit,
and confirmation is enforced at dispatch.

## Pin a production architecture

Direct binding flags are convenient during development. A durable deployment
should select an immutable architecture revision:

```bash
./openrealtime architectures list
./openrealtime architectures show cascade.controlled@3

./openrealtime serve \
  -architecture cascade.controlled@3
```

The architecture owns structural choices such as binding, floor, interaction
owner, evidence capabilities, controllers, and arbitration. Provider flags
still choose the concrete model deployments. Startup refuses an explicit flag
that contradicts the selected architecture.

## Diagnose the stack

Check the runtime first:

```bash
curl -s http://127.0.0.1:8765/healthz
curl -s http://127.0.0.1:8765/metrics
```

Then verify the model services independently. A server process can be healthy
while a model endpoint is missing; `/healthz` deliberately does not make
third-party provider calls.

Drive the complete audio path with real speech:

```bash
./openrealtime probe -audio ./path/to/pcm16-speech.wav
```

For startup failures, enable structured logs:

```bash
./openrealtime serve -log-format json -log-level debug
```

Continue with [operations](../operations.md) for resource sizing, authentication,
failure behavior, and deployment checks. For the exact multi-model system used
by the meeting evaluation, use the [meeting-assistant
guide](meeting-assistant.md).
