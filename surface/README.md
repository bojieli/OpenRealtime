# The test surface

Every channel this system has, in both directions, on one page.

```sh
openrealtime surface
```

Open `http://127.0.0.1:8768` and press Connect.

```sh
openrealtime surface \
  -endpoint ws://gpu-box:8765/v1/realtime \
  -webrtc  http://gpu-box:8766/v1/realtime \
  -root ~/project \
  -browser-devtools-url http://127.0.0.1:9222 \
  -browser-start-url https://example.com
```

## Why it is not the console

The [developer console](../console/README.md) is the minimal complete client:
one voice session, an inspector, and nothing that is not on the wire. It is
what you read while implementing a client.

This is the bench. A voice agent that can also see three sources and click on
one of them fails in ways that are obvious when you can watch every channel
together and nearly impossible to reconstruct from an event log afterwards — a
camera that stopped sending, an observation that never came back, a tool the
model called and the client silently ignored. So the page is arranged as the
two spaces rather than as a conversation:

| Observation — world → agent | Action — agent → world |
| --- | --- |
| **Audio** — speech, continuously | **Speech** — audio out, and what it was saying |
| **Text** — typed, or sent back by an artifact | **Text** — written output, the half nobody hears |
| **Screen** — a display you share | **Computer use** — clicks, keys, scrolls |
| **Camera** — the physical world | **Tool calls** — what it asked this machine to do |
| **Browser** — a browser this surface drives | **Artifacts** — HTML it wrote for you |
| **Tool results** — what this machine answered | |

Each channel shows whether it is carrying anything, how much it has carried,
and the last few things it carried. The count matters more than it looks: a
channel at zero when you expected traffic is the most common finding in an
end-to-end run, and it is invisible in a transcript, which only shows what did
arrive. Disconnecting reports which channels stayed silent.

## Nothing here is new protocol

That is the point, and it is worth checking rather than believing.

**The browser channel** is a context this process both captures frames from and
performs clicks against. The frames are ordinary `video.input` frames under a
source name; the clicks are the ordinary `computer.*` vocabulary naming that
source. Because it is one page on both ends, what the agent looks at and what
it acts on are the same coordinate space with nothing in between to be wrong
about — and the surface draws a mark where each action landed, which is the
difference between "clicked at (740, 313)" and "hit the button".

**Artifacts** are generative UI, and they are an ordinary function tool. Not an
event, not a namespace, nothing the protocol knows about: the agent calls
`display_artifact`, this process stores the HTML, and the page renders it. Any
Realtime server that can call a function can drive it, including one that has
never heard of OpenRealtime.

So the wire is exactly what [the protocol](../docs/protocol/openrealtime-1.md)
says it is. Remove the `openrealtime` key from the session editor and the three
video channels go dark and everything else keeps working, which is the
compatibility claim made visible.

## The browser it drives

Point `-browser-devtools-url` at a browser that is already listening:

```sh
chromium --remote-debugging-port=9222 --user-data-dir=/tmp/agent-browser
openrealtime surface -browser-devtools-url http://127.0.0.1:9222
```

The surface declares that browser as a computer-use target owning exactly one
video source. An action naming the shared screen or the camera is refused
before it reaches the browser — you observe a camera, you do not act on it —
and a coordinate outside the space the model was shown is refused too, because
an action outside the screen is an action on something nobody looked at.

Without the flag the channel is unattached and the page says so. It does not
offer a control that cannot work, and the command refuses to start if a browser
was named and could not be reached: a channel silent because nobody attached it
and a channel silent because it is broken look identical, and that confusion is
what this page exists to prevent.

## Artifacts

The agent writes HTML, and the page frames it from `/artifacts/{id}` — a route
rather than a `srcdoc`, and that is a security decision. A frame loaded from
`srcdoc`, `blob:`, or `data:` inherits the embedding page's content security
policy, so letting model-authored inline script run would mean relaxing this
application's own policy far enough that it did. A real URL carries its own:

- `default-src 'none'` with no `connect-src`, so the artifact has no network at
  all — no fetch, no WebSocket, no remote image. HTML a model wrote cannot send
  what it was shown to anybody.
- `script-src 'unsafe-inline'`, so its own script runs.
- The frame is sandboxed **without** `allow-same-origin`, so it runs in an
  opaque origin and can read neither this page's DOM nor its storage.

Calling `display_artifact` again with the same `artifact_id` revises what the
person is already looking at rather than stacking a second copy underneath it.

The one way out is `postMessage`, which is deliberate: a person who clicks a
button inside an artifact is a person saying something, and what they said goes
back to the session as an ordinary user message, with the provenance a user
message already has.

```js
window.parent.postMessage({ text: "I acknowledged the deadline" }, "*");
```

A fragment is wrapped. Models write fragments — asked for a table, a good one
returns a style block and a table and stops, because that is what the answer
is — so the surface supplies the charset, the viewport, and a readable default
font rather than holding out for boilerplate that adds nothing.

## Why it runs on your machine

Same three reasons as the console, plus one more.

**The microphone works without a certificate.** A browser will not give a page a
microphone, a screen, or a camera outside a secure context, and `127.0.0.1` is
one.

**The credential never enters the browser.** The protocol connection is made
from this process, which holds the token and relays the session byte for byte.

**Tools run where the files are.** A browser cannot open a file.

**And the browser is here.** A DevTools endpoint is a remote-control interface
for a browser; the only sane place to hold one is the machine it is on.

The surface listens on loopback only and refuses to start otherwise. It runs
what a session asks it to and drives a browser, so reachability is the whole of
its security model.

## Confirmation

Declared per tool, enforced in this process, answered in the page. Because the
surface is the client that owns the implementation and the confirmation UI, it
declares `confirm: never` to the remote session and retains the real requirement
locally. This is explicit delegation, not a bypass: the server applies its
declared requirement before emitting the call, then the loopback host applies
the local requirement before touching the world. Computer actions also declare
the host's bounded browser target to the session.

| Requirement | What happens |
| --- | --- |
| `never` | runs |
| `policy` | runs if the action lands inside the declared target; asks otherwise |
| `always` | asks |

`policy` resolving against the target rather than asking is what makes a run
watchable. Every clicking and typing action declares `policy`; if that meant
"ask", you would answer a dialog for every keystroke the agent typed and stop
watching. Approving an action does not widen the target: the dispatcher checks
the fence again at the point of effect, which is where it has to be.

## Testing it

```sh
go test ./surface/
```

The suite includes a real browser run — the real gateway, the real cascade
binding, the real WebRTC adapter, this surface, and a second real browser as
the thing being acted on, with scripted providers where the models would be.
Nine of the eleven channels carry traffic in one session, on both transports.
It skips when Chromium or Node is missing.

Three of its checks could not be made any other way. An artifact is a sandboxed
cross-origin frame, so proving one rendered means dispatching a real input
event at the coordinates it occupies and watching a click on model-authored
HTML arrive as the person speaking. A computer-use action is only real if the
page it named changed, so the assertion is the target browser's own location
moving. And a coordinate mark exists only in a browser that laid the page out.

### Against real models

The scripted gate cannot prove the harder thing: that a model *chooses* to use
these channels, and that what it puts in them is usable. Start a server and
point the live run at it:

```sh
openrealtime serve -listen 127.0.0.1:18765 \
  -fast-provider vllm -fast-url http://127.0.0.1:8000/v1 -fast-model qwen-fast \
  -slow-provider vllm -slow-url http://127.0.0.1:8000/v1 -slow-model qwen-fast \
  -asr-provider sensevoice \
  -tts-provider openai-compatible -tts-url http://127.0.0.1:8081/v1/audio/speech

OPENREALTIME_LIVE_ENDPOINT=ws://127.0.0.1:18765/v1/realtime \
  go test ./surface/ -run TestLive -v
```

`OPENREALTIME_LIVE_AUDIO` points at a 16-bit PCM WAV to speak into the fake
microphone; without it the audio channel is left out of the run rather than
asserted on evidence that cannot exist, because a tone transcribes to nothing.

The slow provider is the one that has to be able to call tools — the fast one
structurally cannot, which is the whole differentiator — so an endpoint whose
slow model refuses tool calls fails at the first assertion, and should.

**Its endpoint has to return tool calls as tool calls.** A self-hosted server
started without tool-call parsing answers with the call written out as text in
the content field — `<tool_call><function=read_file>…` — which is a model doing
exactly the right thing and a client that can never see it. Nothing fires, no
error is raised, and the tool and artifact channels sit at zero looking like a
model that chose not to act. For vLLM that means
`--enable-auto-tool-choice --tool-call-parser <parser for your model>`; the
symptom is worth recognising because every layer above it behaves correctly.

The model names above are one machine's; the flags that matter are which
provider, which endpoint, and whether it parses tool calls.

The three video channels need the server to have negotiated `video.input`,
which needs a video observer, which needs a model that can see. A deployment
with no vision model connects fine and reports no video input, and the live run
says so rather than reporting three channels as silent. To run them, add a
narrator:

```sh
vllm serve Qwen/Qwen2.5-VL-7B-Instruct --served-model-name qwen-vl --port 8003 \
  --gpu-memory-utilization 0.28 --max-model-len 16384

openrealtime serve ... \
  -observers audio+video -narrator dedicated \
  -vision-provider openai-compatible -vision-url http://127.0.0.1:8003/v1 -vision-model qwen-vl \
  -narration actionable
```

`-narration actionable` is what makes computer use possible at all: it asks the
narrator for control positions as well as a description, so the agent is told
where the button is rather than left to infer a coordinate from pixels it never
saw. A 7B narrator locates a large control well enough to hit it; asking it for
five-pixel precision is a different and much harder question.

The run does the video half and the conversational half on **two sessions**.
Continuous narration is not free — two sources at three frames a second put
hundreds of observations into one trajectory — and the first version of this
asked its last question with forty thousand tokens of synthetic test pattern
behind it, on a model whose context is forty thousand and change. Both halves
failed, and what the run measured was context endurance: a real thing to
measure, and not this thing.

Against a local stack (Qwen3-30B reasoning, Qwen2.5-VL-7B narrating, SenseVoice
listening, FishAudio speaking) it passes thirty-one checks and nine of the
eleven channels carry. The two that do not are named: a headless browser has no
display to share, and a model that speaks its answer never writes one.

## Building on it

Plain ES modules, no build step, one file per concern:

| | |
| --- | --- |
| `transport.js` | both transports, and the chunk framing |
| `audio.js` | capture, playout, and the accounting of what was actually heard |
| `video.js` | screen and camera, and the browser as a third source |
| `tools.js` | the bridge to this process |
| `artifacts.js` | the sandbox, and what comes back out of it |
| `channels.js` | the two spaces, and what each one is carrying |
| `app.js` | which event belongs to which channel |
