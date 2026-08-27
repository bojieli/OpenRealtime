# OpenRealtime Developer for macOS

A native SwiftUI developer client for the complete OpenRealtime session:
microphone and audio output, typed and written text, physical camera, screen
sharing, downloadable files, local filesystem/shell tools, timestamped server
debugging, and computer use.

Computer use has two deliberately separate targets:

- **Browser / set of mark** connects to Chrome or Chromium over CDP. The
  bundled bridge imports `browser-use==0.12.6` and uses its DOM serializer,
  selector map, Python highlight renderer, and hardened action implementation.
  Labels in the JPEG are the exact identifiers accepted by
  `computer.click_element`.
- **Desktop / pixels** shares one explicitly selected display and accepts only
  pixel actions inside that display's declared coordinate space. It requires
  Screen Recording and Accessibility permission. There is no ambient,
  unbounded desktop target.

## Build and run

Requirements are macOS 14+, Xcode 16+, and
[uv](https://docs.astral.sh/uv/). Chrome needs a debugging endpoint for browser
mode:

```sh
open -na "Google Chrome" --args \
  --remote-debugging-port=9222 \
  --user-data-dir="$TMPDIR/openrealtime-browser"

cd macos
./prepare-browser-use.sh
./build-app.sh
open ".build/OpenRealtime Developer.app"
```

Set the WebSocket endpoint, local workspace root, and system prompt before
connecting. The prompt is included in the first `session.update`; the app does
not silently invent a second session configuration.

The first use of each medium prompts for the corresponding macOS privacy
permission. The app shows the permission state and never reports a stream as
active until frames are flowing.

## What is native

- AVAudioEngine converts the hardware microphone to PCM16 at 24 kHz, streams
  silence while muted so endpointing remains correct, and schedules incoming
  PCM output. Barge-in clears queued output and reports the amount actually
  heard with `conversation.item.truncate`.
- ScreenCaptureKit captures one selected display; AVFoundation captures one
  physical camera. Both resize and recompress JPEGs to the negotiated FPS,
  dimension, and byte limits and attach absolute millisecond capture times.
- Filesystem reads, directory listings, regex search, confirmed writes, and
  confirmed zsh commands resolve after symlinks inside the chosen workspace.
  Shell execution has a 120-second limit and bounded output.
- `display_artifact` uses a non-persistent WKWebView. Artifact `postMessage`
  text returns as an ordinary user message. `publish_download` writes at most
  8 MiB to the app's Application Support directory and provides Open, Save As,
  and Show in Finder controls.
- The server timeline preserves its absolute millisecond timestamp,
  correlation identifier, phase, and duration and reports p50/p95. Raw media
  is elided from the separate protocol inspector.

The browser-use Python environment is not embedded in the app and Browser mode
runs `uv` offline. Prepare the locked environment explicitly with
`./prepare-browser-use.sh`; it lives in Application Support rather than inside
the signed bundle. The bridge is pinned to 0.12.6 and the complete transitive
resolution is in `uv.lock`, so an update cannot silently change the grounding
implementation.

## Development verification

`swift build` is the native compile gate. Linux CI also statically checks the
required capability boundaries, while the repository's macOS CI job compiles
the Swift package against the Apple frameworks. The browser bridge has a
separate Python syntax gate:

```sh
swift build
./prepare-browser-use.sh
python3 -m py_compile BrowserUseBridge/bridge.py
bash -n build-app.sh
```
