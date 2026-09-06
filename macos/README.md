# OpenRealtime Developer for macOS

A native SwiftUI client for talking to OpenRealtime, sharing camera or screen
input, and inspecting session behavior. It connects to the same server as the
browser client. Use the [browser quickstart](../docs/quickstart.md) for the
simplest first run.

| Profile | Includes | Selection |
| --- | --- | --- |
| `observer-developer` | Microphone/playback, visual capture, diagnostics, graph inspection | Default |
| `effects-developer` | Observer features plus explicitly composed host effects and artifact access | `OPENREALTIME_NATIVE_PROFILE=effects-developer` |

Selecting an effects profile does not bypass server authorization. Tools and
resources must also be declared by the host and negotiated by the session.

## Build and run

Requirements are macOS 14+, Xcode 16+, [uv](https://docs.astral.sh/uv/), and a
built `openrealtime` binary in the repository root. The example below uses a
Gemini key for voice and reasoning. Chrome and the prepared browser bridge are
needed for marked browser capture; microphone and ordinary screen/camera use
do not require the Chrome debugging endpoint.

For browser capture, start Chrome with a separate debugging profile, then build
and launch the application from the repository root:

```sh
open -na "Google Chrome" --args \
  --remote-debugging-port=9222 \
  --user-data-dir="$TMPDIR/openrealtime-browser"

cd macos
./prepare-browser-use.sh
./build-app.sh
cd ..
export GEMINI_API_KEY="your-key"
./openrealtime companion -client macos -- \
  -binding upstream -upstream-provider google -slow-provider google
```

The normal application waits for an explicit **Connect** action. Automated
developer and hosted-CI launches may pass
`--openrealtime-connect-on-launch` to the assembled executable; the flag only
performs that initial action and does not replace the selected native provider
graph, endpoint directory, or reducer.

Hosted CI uses a separate nonce-bearing smoke argument. It is accepted only as
one lowercase SHA-256 value and emits a single connection proof after the
reducer has received the server session identity; the gate binds that proof to
the exact assembled executable, observer manifest, generated endpoint
directory, and unchanged companion process before shutting the app down.

`companion` preflights the exact `.app` before starting anything, then
supervises the clean server, separate WebRTC adapter, and descriptor-locked
presentation host. Use `-client both` to open the browser and native observer
against that same long-lived server, or `-client none` to leave client launch
to another process. The equivalent manual composition remains available:

```sh
./openrealtime serve \
  -webrtc-listen 127.0.0.1:8766 \
  -webrtc-allow-origin http://127.0.0.1:8767
./openrealtime present \
  -client-profile browser-developer-webrtc \
  -webrtc-endpoint http://127.0.0.1:8766/v1/realtime/calls \
  -native-websocket-relay \
  -management-endpoint http://127.0.0.1:8765/openrealtime/v1
```

The second process serves the composable browser client and the public native
relay routes on `127.0.0.1:8767`; it does not add assets or UI routes to the
Realtime server. Effects-enabled native deployments additionally mount their
own explicit effect/resource providers and authority rather than relying on a
hidden CLI default.

The bundled endpoint directories select the loopback presentation host at
`127.0.0.1:8767`, while the clean Realtime server remains independently
reachable at `127.0.0.1:8765`. This client and the browser client therefore use
the same unchanged presentation-host `/client/v1/*` API without adding UI
routes to the server. To select another deployment, set
`OPENREALTIME_NATIVE_ENDPOINT_DIRECTORY` to a regular JSON file of at most
1 MiB containing the complete exact directory. This replaces deployment
wiring as one immutable value; it is not a place for tokens or session data.
When companion uses a non-default presentation address it creates that exact
file with mode `0600`, supplies it only for the launched app's lifetime, and
removes its containing temporary directory during bounded shutdown.
The system prompt is the base session configuration; media, video, debug, and
host-effect providers contribute independently disposable fragments through
the shared session-configuration service.

The first use of a native medium prompts for its corresponding macOS privacy
permission. The app never reports a source active before frames are flowing.

## Provider boundaries

The following contracts matter when extending or replacing native providers.
For normal app use, the build and connection steps above are sufficient.

Two exact client distributions ship against the same server API:

- `observer-developer` is the normal app default. It omits the effects and
  artifact providers, their permissions, and the
  effects/artifact/download endpoint declarations. It retains native media,
  protocol diagnostics, and scoped graph inspection without inventing client
  authority.
- `effects-developer` is an explicit authority-bearing distribution that mounts
  the pinned host-effects and hosted-resource providers.

Set `OPENREALTIME_NATIVE_PROFILE=effects-developer` before launching to opt in
to the effects manifest. An absent or empty value selects the observer; an
unknown non-empty value is refused and shown in the launch window, never
silently mapped to another permission profile.

Each distribution also ships a separate version-1 endpoint directory. It pins
the exact credential-free absolute URL and protocol identity for every selected
transport: realtime WebSocket and management for the observer, plus effects,
artifacts, and downloads for the effects distribution. Its SHA-256 fingerprint
covers the complete canonical directory. Missing, extra, reordered, tampered,
credential-bearing, or protocol-substituted destinations are refused before a
native provider is constructed. No normal-path component derives management,
effects, or resource origins from the realtime URL.

The view does not execute tools or manufacture effect authority. The client
composition manifest pins the exact `openrealtime.client-effects.v1` protocol
and default host catalog digest. After the realtime session negotiates `client.effects`,
the client accepts only declarations from the matching `ready` message. Each
tool call must contain the exact declaration digest and an opaque, bounded
server authority; that authority is sent once to the same-origin
declared effects socket and is never stored, displayed, or logged.
Hosts with a replacement effect catalog can build the same provider graph via
`NewNativeEffectsDeveloperBundleWithEffectsCatalog`; the canonical SHA-256 is
pinned in the resulting manifest and used for every ready/declaration/call
check. Hosts replacing both deployment wiring and the catalog can bind them in
one construction step with
`NewNativeEffectsDeveloperBundleWithEffectsCatalogAndEndpointDirectory`.


The bundled app registry is an installed implementation catalog keyed by exact
implementation identity. It preflights every manifest row before constructing
anything, instantiates only selected providers, and exposes each factory only
to dependencies declared by that row. Missing factories, replacement
identities, dependency substitutions, and permission drift fail closed. The
SwiftUI boundary receives a typed service set assembled solely from its
declared dependencies, so the observer view never receives effects or artifact
services.

- The audio provider uses AVAudioEngine for PCM16 at 24 kHz and tracks actual
  playout for barge-in truncation. It has microphone/playout permission only.
- The video provider owns ScreenCaptureKit, AVFoundation, and optional
  browser-use marked capture. It enforces negotiated FPS, dimension, and frame
  byte ceilings and can be lost/remounted without quiescing audio or effects.
- The host-effects provider uses a credential-free ephemeral WebSocket at the
  endpoint directory's exact URL, cross-checked against the manifest protocol,
  path, and catalog. Ready, declaration, limit, confirmation,
  result, digest, catalog, call-count, and authority fields are strictly
  bounded. Provider loss fails pending calls and cancels reconnect work.
- The artifact provider stores metadata references only. Viewing/exporting
  fetches beneath the separately declared artifact or download base using only
  the immutable reference's bounded `?version=&digest=` query through a
  redirect-free, cookie-free, credential-free bounded transport; it checks
  status, media type, no-store/nosniff, version/digest/ETag headers, byte count,
  and SHA-256 before rendering or writing. HTML receives a restrictive CSP in
  a non-persistent WKWebView.
- Graph inspection keeps its short-lived management token private and sends it
  only in `OpenRealtime-Management-Token` to the exact declared management
  base, never in a URL or public snapshot.
- SwiftUI subscribes through the logical view boundary. Provider loss removes
  subscriptions, and remount creates fresh scoped projections.

The browser-use Python environment is not embedded in the signed app.
`prepare-browser-use.sh` creates the pinned `browser-use==0.12.6`
environment in Application Support. The bridge is used only as an explicit
video-capture source; tool authority remains on the host-effects path.

## Development verification

Portable reducer/composition tests run in Swift 5.10 on Linux. Apple framework
compilation and signed microphone/camera/screen/browser end-to-end checks run
on a real macOS runner:

```sh
swift build
./prepare-browser-use.sh
python3 -m py_compile BrowserUseBridge/bridge.py
bash -n build-app.sh
```

The signed gate intentionally fails closed away from macOS.

The provisioned `external.macos.signed-e2e` gate does not accept a runner's
exit status as evidence. The Go test builds the exact OpenRealtime server
executable from the checked source, hashes it and the supplied frozen launch
profile, captures the signed app's bundle/executable bytes plus code-signing
identifier, TeamIdentifier, CDHash, authority chain, and designated
requirement, and generates an unpredictable invocation nonce and server-run
identity. It writes that exact browser-then-native contract once in a private
gate-owned directory. The trusted runner is invoked as:

```text
runner --app APP --contract CONTRACT --receipt RECEIPT --nonce SHA256
```

`RECEIPT` must not exist before launch. The runner creates one canonical
private JSON receipt. The gate strictly reopens it and rejects unknown fields,
replay, path or identity substitution, duplicate/reordered sessions, and
credential retention. Browser WebRTC must complete before native WebSocket on
one unchanged server process/run. Each session separately retains microphone
input, assistant-audio output, camera frames, screen frames, permission-path,
interruption-to-tool-continuation, tool, and graph-inspection assertions. The
native permission record includes Accessibility when the selected distribution
actually mounts desktop effects. Exact population and bounded cleanup evidence
cover both sessions, the presentation/server processes, listeners, generated
endpoint file, and media. Only after verification are the original contract
and canonical receipt published create-only to the release artifact directory.

The gate additionally requires
`OPENREALTIME_MACOS_E2E_LAUNCH_PROFILE`,
`OPENREALTIME_MACOS_E2E_GRAPH_FINGERPRINT`, and
`OPENREALTIME_MACOS_E2E_SERVER_PROFILE_FINGERPRINT`. Portable Go tests verify
the receipt schema and mismatch/tamper/replay cases, but they are not signed
Darwin execution evidence.
