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
| Streaming recogniser | `http://127.0.0.1:8001` | `-asr-url` |
| Fast model (OpenAI-compatible) | `http://127.0.0.1:8000/v1` | `-fast-url`, `-fast-model` |
| Speech synthesis (OpenAI-compatible) | `http://127.0.0.1:8081/v1/audio/speech` | `-tts-url`, `-tts-model` |
| Background reasoner | Gemini | `-slow-provider`, `-slow-model` |

The background reasoner needs one credential:

```sh
export GEMINI_API_KEY=...
```

Point it at a local model instead if you would rather run everything yourself:

```sh
./openrealtime serve -slow-provider openai-compatible -slow-model your-model
```

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
