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
or timeout at an ordinary endpoint delegates to the unchanged fast/slow
rollout. Once the interaction controller has granted typed direct-screen
authority on a live partial, failed pixel grounding stays on that visual lane:
a later micro-turn or canonical endpoint retries it rather than sending a
coordinate problem to a visionless slow reasoner. The reflex is silent and
cannot replace the voice. Audio-only sessions do not instantiate it.

With the reflex enabled, the profile defaults the video observer to
`keyframe`: pixels reach the reflex without first waiting for a narration model.
Explicit observer, model, policy, token, and timeout flags always win, so use
`-observer-components keyframe+narration` when durable rich descriptions are
worth that additional call.

## Local live meeting assistant

The reproducible meeting deployment keeps the existing Gemini background
reasoner and compares two local foregrounds:

| Cell | Foreground path | Shared controls |
| --- | --- | --- |
| cascade | SenseVoice streaming ASR → Qwen3-VL-8B → Fish speech | direct keyframes, Qwen interaction policy, bounded computer actions, Gemini slow cognition |
| Omni | raw audio + direct images → Qwen3-Omni native speech | the same Qwen policy, SenseVoice control transcript, bounded actions, Gemini slow cognition |

Run the long-lived pieces in separate terminals:

```sh
export GEMINI_API_KEY=...
scripts/meeting-assistant.sh policy

# Cascade cell: policy + the already-running SenseVoice and Fish services.
scripts/meeting-assistant.sh cascade
scripts/meeting-assistant.sh bench-cascade

# Omni cell: policy + SenseVoice, plus one persistent model process.
scripts/meeting-assistant.sh omni-sidecar
scripts/meeting-assistant.sh conformance-omni
scripts/meeting-assistant.sh omni
scripts/meeting-assistant.sh bench-omni
```

Override paths and endpoints with the `MEETING_*` variables at the top of
`scripts/meeting-assistant.sh`. The script builds its own binary under
`.runtime/meeting-assistant`, writes result JSON there, keeps every server in
the foreground, and never kills an existing GPU process. The default Omni
checkpoint is about 35 GB; inspect available memory before starting it beside
the 8B policy service.

The recognizer is a controlled part of each measured cell. Select SenseVoice,
Whisper, or another compatible endpoint without changing the meeting runtime:

```sh
export MEETING_ASR_PROVIDER=whisper
export MEETING_ASR_URL=http://127.0.0.1:8003/v1
export MEETING_ASR_MODEL=openai/whisper-large-v3-turbo
```

`MEETING_SLOW_PROVIDER`, `MEETING_SLOW_URL`, and `MEETING_SLOW_MODEL` similarly
select the asynchronous reasoner. The benchmark launcher writes these actual
recognizer, policy, foreground, and slow-model identities into the result
cell; an override therefore cannot leave a result mislabeled as the default
SenseVoice/Gemini deployment.

Both servers expose WebSocket and direct WebRTC endpoints. The default WebRTC
listeners are `127.0.0.1:28786` for cascade and `127.0.0.1:28787` for Omni.
Microphone audio uses the media track; the repository console/surface sends
selected screen or camera frames as direct protocol video events over the
WebRTC data channel. The fast action path sees pixels, not an intermediate
narration. Adaptive observation can still retain keyframes and optional
narration for persistent context outside that reflex path.

In the cascade deployment, the conversational 8B model is proposal-only for
computer use. A separate silent visual actor is the sole owner of screen
effects, which prevents a spoken answer and a visual monitor from both acting
on the same obligation. It receives the newest retained screen frame even
when a spoken command and a frame do not arrive in the same 200 ms batch. A
visual `WAIT` completes only that visual branch: presentation speech and slow
work continue, and later visual evidence resumes monitoring. Autonomous
screen observations are admitted while agent audio is playing, so a silent
alert acknowledgement does not wait for a presentation to finish.

The actor can use `computer.click` for literal target pixels or
`computer.click_normalized` for the 0–1000 coordinate convention learned by
many vision-language models. Normalized coordinates are explicitly converted
to target pixels by the dispatcher; the runtime never guesses which
coordinate system a model intended. The actor also sees unresolved tool names
and the latest computer action/result, and identical unresolved calls are
suppressed. These state features make a 200 ms replan cadence safe without
turning it into five duplicate actions per second.

The shipped cascade meeting topology is intentionally asymmetric. The local
Qwen3-VL-8B service owns interaction policy and the conversational fast voice;
the local Qwen3-VL-30B service is a silent direct-pixel actor; Gemini 3.5 Flash
is an asynchronous slow reasoner and authoritative semantic-tool user. The
200 ms clock belongs to streaming ASR, interaction, and action opportunities.
The one-second audible endpoint is a separate commitment clock for ordinary
speech, so a clear UI command can act before the complete sentence ends while
conversation does not fragment at every ASR partial.

Visual control is receding-horizon action chunking: the actor may commit one
bounded effect, then adaptive observation must admit a fresh post-action frame
before another coordinate is grounded. While a VLM call is occupied, newer ASR
or frames replace the pending request instead of forming a stale queue. A
canonical endpoint cancels visual inference computed from the superseded live
prefix and takes over immediately. The controller carries completed action
chunks, the current reconstructed task in the user role, and the next
unfulfilled explicit UI clause; an incomplete tail such as `share your` cannot
authorize a guessed second click.

Typed authority is enforced on every entry path. A conservative deterministic
compiler recognizes only high-confidence UI imperatives; learned interaction
policy handles ambiguous and future-monitoring language. The pixel actor must
return a private grounded target label (Qwen's decoder-friendly `label` alias
is accepted), which is checked against the next requested control and stripped
before execution. Rejected model calls are closed as controller placeholders
so a corrected retry is not mistaken for unresolved work. Nearby repeats of a
completed normalized coordinate are no-ops, and within-word ASR completion
such as `over` → `overview` updates the canonical task.

Composite requests remain concurrent. Typed decomposition can resume an
immediate semantic clause—present now, or report the tool-grounded value—while
a visual condition stays armed. A slow tool result wakes the fast voice even
when Gemini correctly emits no additional prose after consuming that result;
the result already in the canonical trajectory remains the source of truth.

The current Qwen3-Omni checkpoint does not independently emit a trustworthy
user transcript. Consequently this production cell is deliberately
`omni+text-policy`: raw audio still goes straight to Qwen3-Omni, while
SenseVoice supplies control evidence and the canonical user text needed by
Gemini. It is not a transcript-free Omni experiment. See the
[meeting benchmark](benchmarks.md#openrealtime-meeting-assistant-v1) before
interpreting the two result files as a ranking.

The validated meeting runs described by this repository exercise the cascade
cell above. They do not measure the Omni cell unless the separate persistent
Omni sidecar is started and the same complete four-case suite is run. Do not
infer Cascade-versus-Omni, interaction-model-versus-policy, or general
architecture superiority from a Cascade-only result.

## Check it works

```sh
./openrealtime probe
```

The probe drives one turn over the protocol and prints what happened: the
transcript, what the agent said, how much audio came back, and how long after
the endpoint the first frame arrived.

## Talk to it

```sh
./openrealtime present
```

Open `http://127.0.0.1:8767`. This starts a separate, loopback-only
presentation host pointed at the public realtime endpoint. Its browser client
is assembled from a locked manifest of content-addressed plugins; the realtime
gateway serves no HTML or client assets. The initial minimal profile supports
typed text over WebSocket. Select `-client-profile browser-developer` together
with an explicit management API base such as
`-management-endpoint http://127.0.0.1:8765/openrealtime/v1` for the
independently authorized inspection and graph-authoring plugins. The
`browser-developer-webrtc` composition additionally requires an explicit
`-webrtc-endpoint`. Realtime, WebRTC, and management endpoints are independent
entries in the profile's fingerprinted endpoint directory; none is inferred by
rewriting another endpoint's origin or path. Static catalog and authoring calls are available only
when the upstream server composition selects the UI-independent management
`StaticCatalog` and `Authoring` services and the operator supplies their narrow
capability; an absent service stays visibly unavailable rather than falling
back to a browser implementation.

Server profiles opt into that independent plane through the public
`management/server.MountOperatorAPI` composition API. They provide an existing
base handler, their own `management.Authorizer`, and only the UI-independent
`StaticCatalog`, `Authoring`, or `Reconciliation` services they intend to
expose. The overlay owns no credential issuer and has no session service:
operator route families use the supplied operator authority, while session and
unrelated routes fall through to the unchanged base handler. Closing the
overlay withdraws only those selected operator routes.

The shipped `present` presets are observer-only: they do not advertise a local
effects socket, artifact authority, or confirmation UI. An effects-enabled
host is a separate plugin composition built with the presentation packages and
an explicit receipt issuer/verifier. Selecting a richer view never creates
effect authority.

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
openrealtime console -webrtc http://127.0.0.1:8766/v1/realtime/calls
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

## The complete browser surface

For all twelve input/output channels, downloadable files, and the searchable
millisecond debug timeline, run the richer loopback surface:

```sh
openrealtime surface \
  -root "$PWD" \
  -browser-devtools-url http://127.0.0.1:9222
```

Open `http://127.0.0.1:8768`. It can send the microphone, typed text, selected
screen, physical camera, and a CDP browser; receive speech and text; execute
confirmed local tools and bounded browser actions; render interactive HTML;
and offer generated files without putting their bytes in the transcript. The
Timeline tab negotiates redacted server traces and reports absolute
millisecond timestamps plus phase latency. See [the test surface](../surface/README.md).

## Native macOS developer app

The SwiftUI app adds real local filesystem/shell tools and an explicitly
selected desktop target to the same session. It requires macOS 14+, Xcode 16+,
and `uv` for the pinned browser-use bridge:

```sh
open -na "Google Chrome" --args \
  --remote-debugging-port=9222 \
  --user-data-dir="$TMPDIR/openrealtime-browser"

cd macos
./prepare-browser-use.sh
./build-app.sh
open ".build/OpenRealtime Developer.app"
```

Choose the endpoint, workspace root, initial system prompt, and either Browser
set-of-mark or selected-display pixel mode before connecting. Browser mode uses
browser-use's DOM selector map and visible marks. Desktop mode requires Screen
Recording and Accessibility permission and maps coordinates only into the
chosen display; it never exposes an ambient, unbounded desktop. Writes, shell
commands, and consequential actions wait for a native confirmation sheet. See
[the macOS app](../macos/README.md).

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
