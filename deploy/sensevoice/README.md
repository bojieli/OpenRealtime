# SenseVoice on the OpenAI transcription route

`server.py` serves [SenseVoiceSmall](https://github.com/FunAudioLLM/SenseVoice)
at `POST /v1/audio/transcriptions`, which is the shape the `sensevoice`
recogniser already speaks. Nothing in the engine knows what is behind that
route.

```sh
./scripts/prepare-sensevoice.sh          # venv, FunASR, weights
./scripts/prepare-sensevoice.sh --serve  # and run it on 127.0.0.1:8002

openrealtime serve -asr-provider sensevoice
```

## Why this rather than a streaming recogniser

SenseVoice is non-autoregressive: it reads the utterance and emits the whole
transcript in one forward pass. Cost therefore tracks the *audio*, not the
transcript, and re-recognising a growing utterance stays cheap — which is what
a batch recogniser on a realtime cadence does all day. Measured here, one
utterance costs about 26 ms at a real-time factor near 0.008.

An autoregressive recogniser decodes token by token, so re-reading a growing
utterance costs more every time it is asked. On long-form conversation that
turns into a recogniser that cannot keep up with the audio arriving, and the
advance bound then fails the session — correctly, but for a reason that looks
like a bug in the engine.

## What is deliberate in the server

- **`/health` runs audio through the model.** A recogniser that has stopped
  answering still accepts connections and still answers a session-open request
  instantly. A health check that dials the port reports that as healthy, so
  this one round-trips a tone and reports the timings it has been serving.
- **Inference holds a lock and runs off the event loop.** The model is one
  graph on one device; serialising is what stops a burst of sessions from
  interleaving into it.
- **Every request has a deadline.** A recogniser that queues without limit
  turns one slow call into a stall that outlives the request that caused it.

## Language

SenseVoice detects the language itself and the server passes `language="auto"`,
so a session that switches languages mid-conversation is transcribed without
being told to expect it. Pass `-language` to `openrealtime serve` to pin one.
