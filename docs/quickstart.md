# Quickstart

This guide gets a first conversation running in a browser and shows you how to
verify the server. The recommended room uses the project’s benchmark pipeline,
with local model services and separate recognition/cognition providers.

Want every model on your own machine? Build the binary here, then switch to the
[local stack guide](guides/local-stack.md).

## What you need

- Go 1.25 or newer
- Git
- A Chromium-based browser; this is the release-gated browser path
- For the shortest verified path, a Gemini API key
- Linux, for the room pipeline specifically. The room freezes its selection into
  a launch profile, and a profile file is read back through a hardened open -
  refusing symlinks, hard links, special files, and any identity change between
  lookup and read - that is implemented for Linux only. Everywhere else `serve`
  stops with `secure launch-profile file opening is unsupported on this
  platform`, which applies to the default room, to `-client macos`, and to any
  `-launch-profile` you author yourself. macOS composes the cascade instead, by
  naming an explicit composition; see [Use local models](guides/local-stack.md).

The hosted path makes provider calls that may incur usage charges. OpenRealtime
does not send telemetry of its own.

## 1. Build OpenRealtime

```bash
git clone https://github.com/bojieli/OpenRealtime.git
cd OpenRealtime
go build -o openrealtime ./cmd/openrealtime
./openrealtime version
```

The final command should print `openrealtime 0.1.0` plus the source revision
and Go toolchain used for the build.

## 2. Start the conversation room

The room is the project's default pipeline, the one the twelve interaction
scenarios accept: Deepgram streaming recognition, a local Qwen interaction
policy, Gemini 3.8 Flash as the voice, Fish speech, speaker identification,
and word timing. Prepare the services and
credentials listed in [Conversation room](room.md), then run:

```bash
./openrealtime companion
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

### Try an interruption

Ask: “Explain how a rainbow forms in a few sentences.” While the assistant is
speaking, say: “Actually, give me the one-sentence version.” Listen for whether
it stops and answers the revised request. You should see the conversation
update and hear the shorter answer.

The runtime decides the turn-taking here, not the voice provider: the
interaction policy is asked on every partial and final whether the person is
cutting in. The [scenario gallery](demos.md) describes the twelve scripted
situations, and [the default pipeline](room.md#the-default-pipeline) plays
them against the room you just started.

To see a client execute a tool, use the
[SDK walkthrough](../examples/sdk-client/README.md). Its weather tool returns
fixed example data, so no weather-service account is needed.

### What `companion` starts

The command supervises three separate local processes:

1. the clean Realtime server on port 8765;
2. a WebRTC adapter on port 8766;
3. a presentation host serving the browser client on port 8767.

The presentation host connects the browser to the server through public APIs.
For the component and credential boundaries, see
[Transports](transports.md#reaching-the-adapter-from-a-browser).

Use `-client none` if you do not want a browser opened:

```bash
./openrealtime companion -client none
```

Flags after the literal `--` belong to `serve`; flags before it configure the
companion supervisor.

## 3. Verify the server

In another terminal, check health and metrics:

```bash
curl -s http://127.0.0.1:8765/healthz
curl -s http://127.0.0.1:8765/metrics
```

Without a configured token, `/healthz` describes the running binding,
capabilities, protocols, and session counters. With a token, detailed health
and metrics requests need the bearer header; see [Operations](operations.md#health). It intentionally does not call provider backends. A healthy process
can therefore still report a provider error when the first turn uses a missing
credential or unavailable model.

This loopback server has no bearer token, so both endpoints answer directly. A
deployment that sets one serves the health detail and the metrics only to a
caller presenting it — `curl -H "Authorization: Bearer $OPENREALTIME_TOKEN"` —
while the health status word stays public so an external check can read it. See
[Operations](operations.md#authentication-and-where-the-endpoint-is-reachable-from).

To drive a complete audio turn without the browser, pass a 16-bit PCM WAV file:

```bash
./openrealtime probe -audio ./path/to/speech.wav
```

Running `probe` without `-audio` sends a generated tone. That checks transport
and session setup, but a working recogniser should not turn a tone into speech.

## Select another pipeline

See what this build supports and which credentials are present:

```bash
./openrealtime providers
./openrealtime providers -role upstream
```

The companion rejects hosted Realtime bindings. To select another pipeline,
author a strict graph profile and pass it explicitly:

```bash
./openrealtime companion -- -launch-profile /absolute/path/to/profile.yaml
```

See [Conversation room](room.md) for media controls, recording, and the shared
browser/macOS scenario selector.

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
./openrealtime serve -launch-profile /absolute/path/to/profile.yaml
```

## Native macOS client

The SwiftUI developer app adds native microphone and playout, camera, selected
screen capture, browser observation, protocol diagnostics, graph inspection,
and millisecond traces. Build it on macOS 14+ with Xcode 16+:

```bash
cd macos
./build-app.sh
cd ..
./openrealtime companion -client macos -- -binding cascade
```

`-binding cascade` is what makes this runnable on the Mac you just built the
app on: without an explicit composition the companion freezes a launch profile,
and reading one back is Linux-only, as **What you need** describes. Name the
rest of the cascade the same way - see [Use local models](guides/local-stack.md).

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
| `serve` refuses a routable listen address | It has no bearer token, and an open Realtime endpoint spends your model credentials. Export the variable named by `-token-env` (`OPENREALTIME_TOKEN` by default), or bind loopback and publish it through something that authenticates. |
| `probe` reports no transcript for its default input | Expected: the default is a tone. Pass a spoken PCM16 WAV with `-audio`. |

## Where to go next

| Goal | Document |
| --- | --- |
| Understand the runtime | [Architecture](architecture.md) |
| Choose a voice architecture | [Bindings](bindings/README.md) |
| Configure models | [Providers](providers.md) |
| Add video and computer use | [OpenRealtime Protocol v1](protocol/openrealtime-1.md) |
| Deploy securely | [Operations](operations.md) · [Deployment](../deploy/README.md) · [Safety](safety.md) |
| Reproduce the full meeting stack | [Meeting-assistant guide](guides/meeting-assistant.md) |
| Browse every document by purpose | [Documentation home](README.md) |
