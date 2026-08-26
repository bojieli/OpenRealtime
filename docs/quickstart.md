# Quickstart

One command brings up a working `/v1/realtime` on localhost.

```sh
go build -o openrealtime ./cmd/openrealtime
./openrealtime serve
```

That is the `cascade` binding: fully local, no third-party account, nothing to
sign up for. It expects three services on the machine, and the flags say where
they are:

| Component | Default | Flag |
| --- | --- | --- |
| Streaming recogniser | `http://127.0.0.1:8001` | `-asr-provider`, `-asr-url` |
| Fast model (vLLM) | `http://127.0.0.1:8000/v1` | `-fast-provider`, `-fast-model` |
| Speech synthesis (OpenAI-compatible) | `http://127.0.0.1:8081/v1/audio/speech` | `-tts-provider`, `-tts-model` |
| Background reasoner | Gemini | `-slow-provider`, `-slow-model` |

`-fast-provider` defaults to `vllm` because that is what the port above
usually is, and because vLLM can be asked to turn thinking off. A reasoning
model with it left on writes its deliberation into the reply, and a fast turn
has ninety-six tokens to spend — which it will spend thinking rather than
answering. Point it at `openai-compatible` for any other endpoint that speaks
Chat Completions; nothing vendor-specific is sent there, so a model that thinks
will think.

Reasoning that arrives inside a reply is never spoken whatever the provider —
it is routed to the reasoning channel where it belongs — so the worst a
misconfigured fast model costs is latency and a short answer, not an agent
reading its own thoughts aloud.

The background reasoner needs one credential:

```sh
export GEMINI_API_KEY=...
```

Point it at a local model instead if you would rather run everything yourself:

```sh
./openrealtime serve -slow-provider openai-compatible -slow-model your-model
```

Or at any of the others. Each provider knows its own endpoint, credential
variable, and models, so selecting one is usually the whole configuration:

```sh
export ANTHROPIC_API_KEY=...
./openrealtime serve -slow-provider anthropic
```

`./openrealtime providers` lists every provider for all three roles and shows
which credentials are set. See [providers](providers.md).

## Runtime profiles

The default is the existing voice-only construction:

```sh
./openrealtime serve -profile voice
```

`voice` is an identity profile: it selects the same observers, voice and slow
providers, prompts, policies, token limits, tools, and rollout as the legacy
flags. A voice+vision deployment uses the same runtime and adds roles through
configuration rather than a second implementation:

```sh
./openrealtime serve \
  -profile voice+vision \
  -computer-use \
  -visual-reflex-provider vllm \
  -visual-reflex-url http://127.0.0.1:8004/v1 \
  -visual-reflex-model qwen-vl-fast-local \
  -slow-provider google
```

Naming `-visual-reflex-model` enables the reflex and implies the voice+vision
profile unless `-profile` was explicitly set. Its shipped bounds are 48 output
tokens and 650 ms. It receives the latest user task, only the newest retained
image per source, and only direct standard computer-action schemas. One call
means `act`; `WAIT` waits for new visual evidence; `ABSTAIN`, malformed output,
or timeout delegates to the unchanged fast/slow rollout. The reflex is silent
and cannot replace the voice. Audio-only sessions do not instantiate it.

With the reflex enabled, the profile defaults the video observer to
`keyframe`: pixels reach the reflex without first waiting for a narration model.
Explicit observer, model, policy, token, and timeout flags always win, so use
`-observer-components keyframe+narration` when durable rich descriptions are
worth that additional call.

## Check it works

```sh
./openrealtime probe
```

The probe drives one turn over the protocol and prints what happened: the
transcript, what the agent said, how much audio came back, and how long after
the endpoint the first frame arrived.

## Talk to it

```sh
./openrealtime serve -demo -webrtc-listen 127.0.0.1:8766
```

Open `http://127.0.0.1:8765/demo`. The page is embedded in the binary, so
there is nothing else to install, and `-demo` is off by default because a
realtime server's job is one protocol on one port. See
[../examples/browser/README.md](../examples/browser/README.md). Give it a recording with `-audio file.wav`
to say something real.

Health and metrics are HTTP:

```sh
curl -s http://127.0.0.1:8765/healthz | jq
curl -s http://127.0.0.1:8765/metrics | jq
```

## Use a hosted voice stack instead

One flag and one credential. No GPU required, and the same number of steps:

```sh
export OPENAI_API_KEY=...
./openrealtime serve \
  -binding upstream \
  -upstream-url wss://api.openai.com/v1/realtime \
  -upstream-model gpt-realtime
```

The remote model does perception, the voice, and speech. OpenRealtime adds the
background reasoner over the same conversation and hands its answers back for
the remote to say — which is the thing a single-model server cannot do, because
it has no second model and no shared log to put one on.

## Talk to it from a browser

```sh
./openrealtime serve --webrtc-listen 127.0.0.1:8766
python3 -m http.server 8080 --directory examples/browser
```

Open `http://127.0.0.1:8080/` and press Connect. The demo implements no media
plumbing of its own: `getUserMedia` and `RTCPeerConnection` handle echo
cancellation, jitter, and loss concealment, which is the whole point of the
WebRTC adapter.

## The developer console

One command on your own machine, pointed at the server:

```sh
openrealtime console -webrtc http://127.0.0.1:8766/v1/realtime
```

Open `http://127.0.0.1:8767`. It speaks both transports against the same
server, shares your screen or camera, shows every event in both directions, and
gives the session tools that run in your own working directory — with
confirmation for anything that changes a file or runs a command.

It runs locally rather than on the server for three reasons that all follow from
where it sits: a browser only grants a microphone in a secure context and
`127.0.0.1` is one, so nothing needs a certificate; the credential stays in
that process instead of the page; and tools can reach your files, which a
browser cannot. See [the console](../console/README.md).

## Connect an existing Realtime client

An official client connects unchanged. Point it at
`ws://127.0.0.1:8765/v1/realtime` and it will not be able to tell the
difference — the server speaks the OpenAI Realtime protocol, and every event in
both directions is validated against the pinned schema on every session.

## What to read next

| Question | Document |
| --- | --- |
| How is this put together? | [architecture.md](architecture.md) |
| Which voice stack should I run? | [bindings/](bindings/) |
| How do video and computer use work? | [protocol/openrealtime-1.md](protocol/openrealtime-1.md) |
| Is it safe to give it tools? | [safety.md](safety.md) |
| How do I run it in production? | [operations.md](operations.md) |
| What is measured, and what is claimed? | [measurement.md](measurement.md) |
