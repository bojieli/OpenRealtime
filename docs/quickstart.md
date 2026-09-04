# Quickstart

This guide gets a first conversation running in a browser and shows you how to
verify the server. The recommended path uses a hosted Realtime endpoint, so it
does not require a GPU.

Want every model on your own machine? Build the binary here, then switch to the
[local stack guide](guides/local-stack.md).

## What you need

- Go 1.25 or newer
- Git
- A Chromium-based browser; this is the release-gated browser path
- For the shortest verified path, a Gemini API key

The hosted path makes provider calls that may incur usage charges. OpenRealtime
does not send telemetry of its own.

## 1. Build OpenRealtime

```bash
git clone https://github.com/bojieli/OpenRealtime.git
cd OpenRealtime
go build -o openrealtime ./cmd/openrealtime
./openrealtime version
```

The final command should print `openrealtime 1.0.0` plus the source revision
and Go toolchain used for the build.

## 2. Start a hosted voice

Use the same Gemini credential for Gemini Live in the foreground and Gemini
background reasoning. This is the provider catalog's live-turn-verified path:

```bash
export GEMINI_API_KEY="your-key"

./openrealtime companion -- \
  -binding upstream \
  -upstream-provider google \
  -slow-provider google
```

When startup succeeds, the command prints:

```text
OpenRealtime companion ready
  server       http://127.0.0.1:8765/v1/realtime
  WebRTC       http://127.0.0.1:8766/v1/realtime/calls
  browser      http://127.0.0.1:8767
```

Your browser opens automatically. Allow microphone access, connect, and talk.
Keep the terminal running; press <kbd>Ctrl</kbd>+<kbd>C</kbd> once to stop the
supervised processes cleanly.

### What `companion` starts

The command supervises three separate local processes:

1. the clean Realtime server on port 8765;
2. a WebRTC adapter on port 8766;
3. a descriptor-locked presentation host on port 8767.

The gateway itself does not serve a UI. Browser and native clients remain
replaceable consumers of the same public APIs.

Use `-client none` if you do not want a browser opened:

```bash
./openrealtime companion -client none -- \
  -binding upstream \
  -upstream-provider google \
  -slow-provider google
```

Flags after the literal `--` belong to `serve`; flags before it configure the
companion supervisor.

## 3. Verify the server

In another terminal, check health and metrics:

```bash
curl -s http://127.0.0.1:8765/healthz
curl -s http://127.0.0.1:8765/metrics
```

`/healthz` describes the running binding, capabilities, protocols, and session
counters. It intentionally does not call provider backends. A healthy process
can therefore still report a provider error when the first turn uses a missing
credential or unavailable model.

To drive a complete audio turn without the browser, pass a 16-bit PCM WAV file:

```bash
./openrealtime probe -audio ./path/to/speech.wav
```

Running `probe` without `-audio` sends a generated tone. That checks transport
and session setup, but a working recogniser should not turn a tone into speech.

## Use another hosted provider

See what this build supports and which credentials are present:

```bash
./openrealtime providers
./openrealtime providers -role upstream
```

For example, one OpenAI key can supply both the OpenAI Realtime endpoint and an
OpenAI background model:

```bash
export OPENAI_API_KEY="your-key"

./openrealtime companion -- \
  -binding upstream \
  -upstream-provider openai \
  -slow-provider openai
```

Foreground and background providers do not have to match. Provider selection,
credential precedence, endpoint overrides, and current model defaults are
documented in the [provider catalog](providers.md).

## Run the local cascade

The default binding is `cascade`: streaming recognition, a fast language
model, speech synthesis, and a background reasoner. If the three default local
voice services are already running, start it with:

```bash
export GEMINI_API_KEY="your-key" # default background reasoner
./openrealtime companion
```

| Role | Default endpoint |
| --- | --- |
| Streaming ASR | `http://127.0.0.1:8001` |
| Fast model (vLLM) | `http://127.0.0.1:8000/v1` |
| Speech synthesis | `http://127.0.0.1:8081/v1/audio/speech` |
| Background reasoner | Gemini |

OpenRealtime does not silently download model weights or launch those model
servers. The [local stack guide](guides/local-stack.md) explains every role,
how to replace it, how to make the reasoner local too, and how to enable the
voice + vision profile.

## Connect your own client

A Realtime client connects over WebSocket at:

```text
ws://127.0.0.1:8765/v1/realtime
```

If `OPENREALTIME_TOKEN` is set when the server starts, send that value as the
bearer credential. If it is unset, loopback development is unauthenticated.

The [official SDK example](../examples/sdk-client/README.md) runs a tool-using
turn through the published `@openai/agents-realtime` client. The
[transport guide](transports.md) covers WebSocket, WebRTC, LiveKit, codecs,
CORS, and production network boundaries.

To run only the server, skip the presentation stack:

```bash
./openrealtime serve \
  -binding upstream \
  -upstream-provider google \
  -slow-provider google
```

## Native macOS client

The SwiftUI developer app adds native microphone and playout, camera, selected
screen capture, browser observation, protocol diagnostics, graph inspection,
and millisecond traces. Build it on macOS 14+ with Xcode 16+:

```bash
cd macos
./build-app.sh
cd ..
./openrealtime companion -client macos -- \
  -binding upstream \
  -upstream-provider google \
  -slow-provider google
```

The shipped developer profile is observation-only. Filesystem, shell, desktop
effects, and artifact authority require an explicitly composed host profile.
See the [macOS app guide](../macos/README.md) for setup and permissions.

## Common first-run problems

| Symptom | Check |
| --- | --- |
| `go` rejects the module version | Put Go 1.25+ first on `PATH` or invoke it by absolute path. Project scripts also honor `OPENREALTIME_GO_BIN`. |
| The server starts but a turn fails | Run `./openrealtime providers`; verify the selected credential and model endpoint. |
| A local cascade produces no speech | Confirm ASR, fast-model, and TTS services are listening on the configured URLs. |
| The browser cannot use the microphone | Open the loopback URL directly and allow site microphone access. |
| A port is already in use | Change `-server-listen`, `-webrtc-listen`, or `-presentation-listen` before the `--`. |
| `probe` reports no transcript for its default input | Expected: the default is a tone. Pass a spoken PCM16 WAV with `-audio`. |

## Where to go next

| Goal | Document |
| --- | --- |
| Understand the runtime | [Architecture](architecture.md) |
| Choose a voice architecture | [Bindings](bindings/README.md) |
| Configure models | [Providers](providers.md) |
| Add video and computer use | [OpenRealtime Protocol v1](protocol/openrealtime-1.md) |
| Deploy securely | [Operations](operations.md) and [Safety](safety.md) |
| Reproduce the full meeting stack | [Meeting-assistant guide](guides/meeting-assistant.md) |
| Browse every document by purpose | [Documentation home](README.md) |
