# Providers

A self-hostable Realtime API is only as useful as the set of models it can be
pointed at. This page is the catalogue: which providers this build reaches for
each of the three roles a voice stack needs, what each one is named, and what
it needs in the environment.

The catalogue is data, not documentation. It lives in
[`providers/`](../providers), the server resolves every provider through it,
and it is checked by tests: every entry has to resolve by name and by alias,
carry an endpoint that parses, name a credential, and build an adapter for both
the fast and the slow phase. A provider that would fail on the first session
fails in CI instead.

```sh
openrealtime providers                 # the whole catalogue
openrealtime providers -role upstream  # one role
openrealtime providers -probe openai
```

## Two kinds of fact

Every entry separates two things that age at different rates.

**The endpoint, the credential, and the wire dialect** are durable. They change
on the timescale of API versions, and they are what makes a provider reachable
at all.

**The default model is a hint.** A vendor ships something new and the constant
in this repository is wrong the same week. So a default model is never
required — every provider takes `-fast-model` and `-slow-model` — the listing
prints when the defaults were last reviewed, and `-probe` asks the provider
itself what it serves rather than trusting this file:

```console
$ openrealtime providers -probe openai
openai serves 124 models at https://api.openai.com/v1/models

  gpt-5.6-luna
  gpt-5.6-sol
  gpt-5.6-terra
  ...
```

One consequence is deliberate: **a marketplace has no default model.** Groq,
OpenRouter, Together, Fireworks, SiliconFlow, Cerebras, and NVIDIA serve other
people's models, and there is no default that is right for anyone. Selecting
one without naming a model is refused, with the probe command in the error.

## Language models

`-fast-provider` and `-slow-provider` select from the same catalogue, and the
two phases differ in what is asked of the provider rather than in which
providers are available:

| | Fast | Slow |
| --- | --- | --- |
| Reasoning | off | on, at `-slow-effort` |
| Tools | proposal-only | executable |
| Speech | voiced | silent |
| Default model | the provider's small model | the provider's large model |

Those are properties of the arrangement, not of the vendor. The catalogue knows
how to reach a provider; the arrangement knows what the provider is for. That
separation is what stops a fast provider acquiring tool authority because its
vendor happens to support tools.

Three wire dialects are implemented:

- **`openai-chat`** — `/v1/chat/completions` with SSE. Most of the catalogue.
- **`anthropic-messages`** — the Messages API, which is not a dialect of Chat
  Completions and gets [its own adapter](../adapters/anthropic).
- **`gemini-generate-content`** — `:streamGenerateContent`, which preserves
  thought signatures that the OpenAI compatibility layer does not.

### Telling a provider to reason

Every provider that speaks Chat Completions invented its own spelling for "how
hard should you think", and there is no way to detect which one an endpoint
wants except by sending the wrong one and reading the 400. So it is declared
per provider, and the listing prints it:

| Spelling | Providers |
| --- | --- |
| `reasoning_effort` | OpenAI, Azure OpenAI, DeepSeek, OpenRouter, Gemini's compatibility layer |
| `thinking: {"type": ...}` | Zhipu GLM, Z.ai |
| `enable_thinking` | DashScope (Qwen), SiliconFlow |
| `chat_template_kwargs` | vLLM, SGLang |
| nothing | xAI, Mistral, MiniMax, and every endpoint whose model decides for itself |

An effort level the endpoint has no word for is **refused at construction**
rather than replaced with a neighbouring one. A run that quietly answered a
request for high effort with medium would make every measurement naming an
effort a measurement of something else.

### The catalogue

Frontier labs: `openai`, `anthropic`, `google` (and `google-openai` for its
compatibility layer), `xai`.

Open-weight labs: `deepseek` (and `deepseek-anthropic` for its Messages-API
endpoint), `zhipu` / `z-ai`, `minimax`, `moonshot`, `dashscope`, `mistral`,
`perplexity`, `ark`, `qianfan`.

Marketplaces: `openrouter`, `siliconflow`, `groq`, `cerebras`, `together`,
`fireworks`, `nvidia`.

Your own machine: `vllm`, `sglang`, `ollama`, `llama-cpp`, `lm-studio`, and
`openai-compatible` as the escape hatch for anything else.

`azure-openai` is in the list but takes no default endpoint: set
`-slow-url https://RESOURCE.openai.azure.com/openai/v1` and name your
deployment as the model.

## Recognisers

`-asr-provider`. The property that matters most here is not accuracy, it is
whether the provider **streams**, because the `stable-partial` observation
policy has nothing to observe without it:

| Provider | Streams | Notes |
| --- | --- | --- |
| `qwen-asr` | yes | the local default; no account, no audio leaves the machine |
| `deepgram` | yes | a WebSocket with interim results, built for voice agents |
| `openai` | no | `/v1/audio/transcriptions` |
| `groq` | no | Whisper, fast enough that the round trip is close to a streaming one |
| `elevenlabs` | no | Scribe, over its own batch route |
| `fireworks`, `siliconflow`, `mistral` | no | the same OpenAI route |
| `sensevoice` | no | local, non-autoregressive; the batch recogniser a long conversation can afford — see [deploy/sensevoice](../deploy/sensevoice/README.md) |
| `whisper-server` | no | whisper.cpp, faster-whisper-server, anything local |

`-language` gives every recogniser and synthesiser a hint; each decides for
itself when it is empty.

A batch endpoint cannot produce a partial hypothesis: it can only be asked what
a recording said. So by default it recognises **once**, at the endpoint of the
utterance, and emits one final revision. `-asr-partial-interval` will buy
earlier text by re-transcribing everything heard so far on a fixed schedule,
and it costs exactly what that sounds like. A streaming provider ignores the
flag because it produces partials for free.

**What "it costs exactly what that sounds like" costs depends on the model.**
An autoregressive recogniser decodes token by token, so re-reading a growing
utterance gets dearer every time it is asked, and on long-form conversation it
stops keeping up with the audio arriving — at which point the advance bound
fails the session, correctly, for a reason that looks like an engine defect.
SenseVoice is non-autoregressive and emits the whole transcript in one forward
pass, so its cost tracks the audio rather than the transcript. That is why it
is the recogniser to reach for when utterances are long or partials are wanted,
and it is what `-asr-cadence` is really trading against.

Watch `advance_mean_elapsed_ns` and `finalize_mean_elapsed_ns` on `/healthz` to
see which side of that you are on; see [operations](operations.md).

This is also what decides whether `-policy-models` can do anything. Backchannel
and turn projection answer a question about the *partial* transcript, so a
batch recogniser left at `-asr-partial-interval 0` gives them nothing to judge
and they measure as having no effect. Enabling them against a batch recogniser
means enabling partials too.

Deepgram is dialled with the **caller's own sample rate**, declared from the
first frame, so nothing is resampled on the way in. A recogniser that resamples
before recognising has thrown away information no later stage can recover.

## Synthesisers

`-tts-provider`:

| Provider | Route | Voice |
| --- | --- | --- |
| `openai-compatible` | `/v1/audio/speech` | the local default: SGLang-Omni, Kokoro, anything |
| `openai` | `/v1/audio/speech` | `alloy` and the rest |
| `fish-audio` | the native Fish server | selected when the server starts |
| `fish-audio-cloud`, `groq`, `siliconflow` | `/v1/audio/speech` | vendor voices |
| `deepgram` | `/v1/speak` | named **inside the model**; setting a voice is refused |
| `elevenlabs` | `/v1/text-to-speech/VOICE/stream` | required, and part of the path |
| `cartesia` | `/tts/bytes` | required, in a nested object |

The last three differ only in how the request is shaped — the voice is in the
path for one, in the body for another, and the sample rate is a query
parameter, a format string, and a nested object respectively. The response is
the same thing in all three cases, and it is the response that is hard, so they
share [one adapter](../adapters/pcmtts) and one
[streaming reader](../internal/speechstream): resampling, read boundaries that
split a sample, and the rule that exactly one chunk carries the terminal flag.

A vendor that answers `200` with a JSON error body is caught rather than
synthesised: playing an error message to the caller as noise is worse than
failing.

## Realtime endpoints (the `upstream` binding)

`-binding upstream -upstream-provider`. These are remote realtime **model**
APIs — an endpoint that already owns perception, a voice, and action, and lacks
only a second model reasoning alongside it. That is what the binding adds.

Agent platforms are deliberately absent. Deepgram's Voice Agent and ElevenLabs
Agents already run their own agent loop, so putting this binding's reasoner
behind one would be two orchestrators arguing over one conversation, not a
seam. Use those products directly, or build the same stack with `cascade`.

| Endpoint | Protocol | Hand-off | Verified | Notes |
| --- | --- | --- | --- | --- |
| `openai` | Realtime | conversation item | reachable | The reference implementation |
| `xai` | Realtime | conversation item | reachable | Documented as Realtime-compatible; reports user transcripts as `.updated` |
| `azure-openai` | Realtime | conversation item | documented | Same spec; credential in `api-key`, model is a deployment in the URL |
| `qwen-omni` | Realtime (pre-GA names) | session instruction | reachable | Emits `response.audio.*`; conversation items are tool-results only |
| `google` | **BidiGenerateContent** | conversation item | live-turn | Not a Realtime dialect; translated into one |

### How far each one has actually been checked

A fake server built from a vendor's documentation proves that this code does
what the documentation was read to say. That is worth having, and it is not the
same as working: it cannot catch a document that is wrong, a field a vendor
quietly requires, or a name that changed last month. So the catalogue records
the difference rather than leaving it in prose, and the listing prints it.

- **`live-turn`** — a real session completed a turn against the vendor's own
  endpoint, and its events arrived under the names this catalogue expects.
- **`reachable`** — the real endpoint answered on this URL and evaluated a
  credential sent this way, so the address and the authentication scheme are
  confirmed against the vendor. No turn has been run, for want of a working
  credential, so the event names and the hand-off remain as good as the
  documentation and the fake.
- **`documented`** — built from the specification and run against a fake, and
  nothing else.

Raising a level takes a credential and one command, which contacts the endpoint
for real and prints every event name it sent back:

```console
$ openrealtime providers -role upstream -probe google
google  (gemini-live)
  endpoint  wss://generativelanguage.googleapis.com/ws/…BidiGenerateContent
  model     gemini-2.5-flash-native-audio-latest
  connected true
  events received:
    response.done                                        1
    response.output_audio.delta                          6
    response.output_audio_transcript.delta               1
    response.output_audio_transcript.done                1
  said      "probe ok."
```

If an endpoint sends something this catalogue does not expect, it appears in
that list under its own name — which is exactly how a stale entry gets found.

Two vendor differences turned out to matter enough to be modelled rather than
assumed.

**Event names.** OpenAI renamed `response.audio.delta` to
`response.output_audio.delta` at general availability. An endpoint built
against the earlier specification sends the same fields under the earlier
names, so the catalogue carries a rename table and the mirror is untouched. A
vendor whose only deviation is a spelling should not cost an adapter.

**The hand-off.** Giving the reasoner's completed answer to the remote to say
is the entire point of the binding, and the base protocol turned out not to be
as portable as it looks. Injecting a conversation item is the natural form and
every endpoint modelled on OpenAI's accepts it — but Qwen-Omni-Realtime accepts
conversation items **only** for tool results, and its `response.create` takes no
per-response instructions. There the session instruction is the only writable
channel, so the answer goes in it and is taken back out when the response
completes. An endpoint that supported neither could not host this binding at
all, and declaring the channel is what makes that checkable rather than
discovered live.

### Gemini Live

Gemini is not a dialect. BidiGenerateContent has no event `type` field —
messages are discriminated by which top-level key is present — no conversation
items, no session updates after the handshake, and it wants 16 kHz audio in
while the Realtime wire carries 24 kHz. So
[`adapters/geminilive`](../adapters/geminilive) is a translator: it speaks
Gemini on one side and the Realtime protocol on the other, which keeps one
runtime, one mirror, and one set of interaction policies.

Three of its constraints shape the translation:

- The **system instruction is settable only in the opening handshake**, so
  setup is deferred until the caller's first `session.update` arrives, and
  anything sent before that is held rather than dropped.
- **Transcription streams with no completion event.** Text is accumulated and
  reported when the turn completes, which is the point the Realtime protocol
  reports the same thing.
- **`interrupted` is not `turnComplete`.** An interruption means the model
  stopped because the user carried on; what it managed to say is real, but the
  user has not finished, so the accumulated user transcript keeps growing
  rather than being committed as a turn. Conflating the two hands the reasoner
  half a question.

`MiniMax` is absent: it announced a realtime API but publishes no wire
specification, and guessing at a protocol is not the same as supporting one.

## Narration

`-vision-provider` selects the model a video observer narrates through. It
resolves against the language-model catalogue, because a narrator is a
vision-capable chat model and every provider that serves one already has an
entry. What it adds is a refusal: a provider that does not speak Chat
Completions, or whose entry does not declare vision, cannot narrate, and being
told that at start-up is better than a decode error on the first frame.

## Credentials

Each entry names the environment variable its vendor conventionally uses —
`ANTHROPIC_API_KEY`, `DEEPSEEK_API_KEY`, `DEEPGRAM_API_KEY`, and so on — so
selecting a provider is usually the only configuration needed. The listing
shows which are actually set:

```console
$ openrealtime providers -role llm
  NAME       DIALECT             FAST MODEL        SLOW MODEL     REASONING         CREDENTIAL
  openai     openai-chat         gpt-5.6-luna      gpt-5.6-sol    reasoning_effort  OPENAI_API_KEY set
  anthropic  anthropic-messages  claude-haiku-4-5  claude-opus-5  thinking          ANTHROPIC_API_KEY
```

Resolution order, for a model credential:

1. the variable named by `-fast-token-env` / `-slow-token-env`, if you set it;
2. `OPENREALTIME_FAST_API_KEY` / `OPENREALTIME_SLOW_API_KEY`, for running two
   profiles of one vendor on separate keys;
3. the provider's conventional variable.

A local provider needs none of them.

## Examples

Everything local, nothing to sign up for — the default:

```sh
openrealtime serve
```

A hosted reasoner behind a local voice, which is the arrangement the fast/slow
split was designed for:

```sh
export ANTHROPIC_API_KEY=...
openrealtime serve -slow-provider anthropic
```

Hosted end to end:

```sh
export OPENAI_API_KEY=... DEEPGRAM_API_KEY=...
openrealtime serve \
  -fast-provider openai -slow-provider anthropic \
  -asr-provider deepgram -tts-provider deepgram
```

A marketplace, where the model has to be named:

```sh
export OPENROUTER_API_KEY=...
openrealtime serve \
  -fast-provider openrouter -fast-model openai/gpt-5.6-luna \
  -slow-provider openrouter -slow-model anthropic/claude-opus-5
```

A remote realtime endpoint behind the background reasoner:

```sh
export GEMINI_API_KEY=...
openrealtime serve -binding upstream -upstream-provider gemini
```

Adding a provider is a table entry in [`providers/`](../providers), plus an
adapter only when the wire format is genuinely its own.
