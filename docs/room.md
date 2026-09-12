# Conversation room

Build with Go 1.25 or later, then start the shared browser/macOS room:

```sh
go build -o openrealtime ./cmd/openrealtime
./openrealtime companion
```

Open `http://127.0.0.1:8767`. Use `-client none` to start without opening a
browser, or `-client both` on a Mac with the application installed.

The room itself requires Linux. It freezes its selection into a launch profile,
and reading a profile file back uses a hardened open implemented for Linux only,
so on macOS `serve` stops with `secure launch-profile file opening is
unsupported on this platform` before it listens. That bound covers every
launch-profile path below, including an `-launch-profile` you author. The macOS
application still connects to a room served from Linux, and on a Mac alone it
runs against an explicitly composed cascade instead - see
[Use local models](guides/local-stack.md).

The default room uses OpenRealtime's graph-native twelve-scenario pipeline:
Deepgram Nova-3 streaming recognition, local Qwen interaction decisions,
Gemini 3.5 Flash cognition, local Fish Speech synthesis, speaker embeddings,
and word timing. Set `DEEPGRAM_API_KEY` and `GEMINI_API_KEY` in the launching
environment. The local services must already be running:

| Component | Default endpoint |
| --- | --- |
| Qwen policy, with vision | `http://127.0.0.1:8000/v1` |
| Fish Speech | `http://127.0.0.1:8123/v1/tts` |
| Speaker identification | `http://127.0.0.1:8124/embed` |
| Word timing | `http://127.0.0.1:8003/v1/audio/transcriptions` |

The room freezes the selected graph into a temporary launch profile, verifies
it using the same registry as the evaluation, and removes it on shutdown.
Missing credentials or services produce errors; there is no fallback to a
hosted Realtime endpoint. `companion -- -binding upstream` is rejected.
A separately authored `-launch-profile` can select another OpenRealtime graph.
For example, `profile scenario` authors the all-local SenseVoice/Qwen/Fish
variant; pass its output with `companion -- -launch-profile /absolute/path.yaml`.
The model and credential environment then come from that frozen profile.

## Media and recording

Join the room, then use the controls independently:

- **Microphone:** mute or unmute the outgoing audio track.
- **Agent audio:** mute playback without muting the microphone or cancelling the agent.
- **Camera:** start or stop physical-camera capture and its preview.
- **Share screen:** choose a screen/window/tab. Stopping it leaves the camera running.
- **Agent tile:** hide or show the agent's visual representation. The present
  pipeline produces audio, so the tile does not pretend to be a video feed.
- **Add image:** send a JPEG, PNG, or WebP image through the same image-message
  path used by the visual benchmark case. Images are resized before sending.
- **Record room:** start/stop a local recording and download it. Browser
  recordings compose the participant tiles and shared screen, mixing unmuted
  microphone and agent audio. They stop on leaving the room and at 256 MB.

Browser microphone, camera, and screen capture require localhost or HTTPS.
Screen selection always uses the browser's permission picker. Support varies
by browser/device, especially on mobile; capture errors appear in the room.

The native room uses SwiftUI with the same catalog, reducer semantics, protocol,
layout, and independent controls. Camera and screen previews come from the
existing native capture providers. Native recording requires macOS 15+ and
uses ScreenCaptureKit to record the room window, application audio, and the
enabled microphone to MP4. Other room features retain the macOS 14 minimum.
On Linux, Go validates native composition and resources; build/install and
permission testing must run on macOS with Xcode 16 or later.

## Practice the twelve scenarios

Expand **Practice an interaction scenario**, select a case, and apply it while
connected. Both clients use instructions, scripts, and tool declarations from
`bench/scenario.Suite()`. Read the script aloud at your own pace. Use a fresh
session for each recorded case so preceding instructions do not affect it.
The recorded-menu case handles `press_key` as an explicitly simulated local
keypad; it does not place a telephone call.

These practice sessions are not scored benchmark runs. Use the existing
[scenario evaluation workflow](../bench/scenario/graphnative/README.md) for
behavioral measurements and retained evidence. A single diagnostic pass does
not establish the suite's repeated-run acceptance criteria.

## Verification and generated assets

```sh
go test ./presentation/browser ./cmd/openrealtime ./macos \
  ./graphs ./graph/binding/scenarioconversation ./bench/scenario/graphnative
node presentation/browser/testdata/room.mjs http://127.0.0.1:8767
```

The browser probe uses headless Chromium with simulated microphone/camera/screen
sources and tests a real model answer, media independence, image submission,
recording playback, and leave/disposal. Set `ROOM_SCREENSHOT` to save a PNG.
`CHROMIUM` and `CDP_PORT` override the browser executable and debugging port.

After native source or scenario-catalog changes, regenerate the source-bound
native manifests and shared scenario resource:

```sh
go run ./tools/room-assets
```

For a single live diagnostic attempt of every canonical scenario against a
running room (including recorded input synthesis and output transcription):

```sh
OPENREALTIME_ROOM_TEST_ENDPOINT=ws://127.0.0.1:8765/v1/realtime \
  go test ./cmd/openrealtime -run '^TestLiveRoomTwelveScenarios$' \
  -count=1 -v -timeout=45m
```

This opt-in test verifies the reported runtime binding and fails on any
behavioral check. It uses the local Fish speech and word-timing endpoints,
retains structured results in the test log, and does not certify the repeated
benchmark. Provider routing is inherited from the process environment.
