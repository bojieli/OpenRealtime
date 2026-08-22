# The developer console

A browser client that exercises the whole system at once — both transports, the
protocol extension, video, and tools that run on your own machine.

```sh
openrealtime console
```

Open `http://127.0.0.1:8767` and press Connect.

```sh
openrealtime console \
  -endpoint ws://gpu-box:8765/v1/realtime \
  -webrtc  http://gpu-box:8766/v1/realtime \
  -root ~/project \
  -tools read_file,list_directory,search_files,write_file,run_command
```

## Why it runs on your machine

The console is a local process that serves a page and connects out. Three
things follow from that, and each of them is the reason it is built this way
rather than served by the server:

**The microphone works without a certificate.** A browser will not give a page
a microphone, a screen, or a camera outside a secure context. `127.0.0.1` is
one. So the page can capture all three against a server running anywhere, with
no TLS to arrange for a machine you are only testing.

**The credential never enters the browser.** The protocol connection is made
from this process, which holds the token and relays the session byte for byte.
A browser cannot set an `Authorization` header on a WebSocket at all, so the
alternative would be putting the credential in a query string or in the page.

**Tools run where the files are.** A browser cannot open a file. The
interesting workload for a voice agent that can also act is your own working
directory, and this is the only process in the picture that can reach it.

## What it shows

| | |
| --- | --- |
| **Transport** | WebSocket or WebRTC, switchable, against the same server |
| **Conversation** | what you said, what the agent said, and what it *observed* — rendered differently, because narrated screen text is something the agent read rather than something anyone said |
| **Events** | every event in both directions, raw, with audio and frame payloads elided; Save writes the whole session to a file, which is what a useful bug report contains |
| **Session** | the `session.update` this page will send, editable — remove the `openrealtime` key to watch the session degrade to the base protocol |
| **Stats** | first audio after the endpoint, tool round trip, the negotiated data channel limit, and whether each frame crossed whole or in chunks |
| **Tools** | what this console will run, and the confirmation each one requires |

## The two transports are not a fallback and a primary

They differ in what the browser has to do for itself, and the difference is
audible.

Over **WebSocket** the page captures at 24 kHz, frames PCM16 itself, schedules
playout itself, and gets full-rate audio in both directions for the trouble.
Over **WebRTC** the browser's own stack does jitter buffering, loss
concealment, and echo cancellation, and the synthesised voice arrives as 8 kHz
mu-law, because there is no pure-Go Opus encoder
([transports](../docs/transports.md)).

So the WebSocket path is the higher-fidelity one and the WebRTC path is the one
that survives a real network. Hearing them back to back is the point of having
both in one page.

## Tools

Every tool is declared by this process, not by the page, so what the session is
told a tool is and what will actually run are one statement rather than two
that can disagree.

| Tool | Confirmation |
| --- | --- |
| `read_file`, `list_directory`, `search_files` | none |
| `write_file`, `run_command` | always |

The default set is read-only. `-tools` selects others by name, or `all`.

**Confirmation is enforced here, not in the page.** When a tool needs it, the
call is held in this process, a request goes to the browser, and nothing
happens until you answer. The page renders the dialog because that is where you
are looking; this process holds the veto because that is what touches the disk.
A session that talked the page into skipping the dialog still could not make
anything happen.

**Everything resolves inside `-root`**, after symlinks. A path that looks
contained and resolves elsewhere is exactly what a prefix check misses, so the
containment test runs on the resolved path — including for a file that does not
exist yet, which is where creating one would otherwise be unbounded in a way
opening one is not.

**The console listens on loopback only** and refuses to start otherwise. It runs
what a session asks it to, so reachability is the whole of its security model.

### Long-running work

A tool has a deadline — the server fails a call the client has not answered
within `-client-tool-timeout`, because an invocation nobody answers leaves the
model waiting forever on something that is never coming
([operations](../docs/operations.md)). A command that legitimately runs longer
than that should return immediately saying the work started, and report the
outcome as a message when it finishes. That keeps the model's view accurate at
every moment; holding the batch open does not.

## Video

Screen and camera are separate sources and the protocol treats them
differently: you act on the screen, you observe the camera. The page declares
geometry before any frame and again whenever it changes, because a computer-use
coordinate is meaningless without the space the model saw.

It sends frames and makes no decision about which ones matter. Selective
perception is the server's job — a gate pushed into the client would mean every
client reimplemented it differently or not at all.

What the page *does* respect is the negotiated limits: it scales to the
advertised long edge and steps JPEG quality down until a frame fits the
advertised byte limit, reporting when it had to.

## Testing it

```sh
go test ./console/
```

The suite includes a real browser run — the real gateway, the real cascade
binding, the real WebRTC adapter, and this console, with scripted providers
where the models would be, driven by Chromium over the DevTools protocol. It
covers what cannot be checked from Go: capture, playout, the client half of the
chunk framing, and whether a browser's WebRTC stack talks to the adapter at
all.

It skips when Chromium or Node is missing, so the gate still runs offline on a
machine with neither.

The fake microphone plays a tone, which is enough to prove audio reaches the
protocol and is all the committed test needs. A run against a real recogniser
needs real speech, because a tone transcribes to nothing:

```sh
CONSOLE_FAKE_AUDIO=/path/to/speech.wav node console/testdata/browser.mjs \
  http://127.0.0.1:8767/ websocket
```

## Building on it

The page is plain ES modules with no build step, one file per concern:

| | |
| --- | --- |
| `transport.js` | both transports, and the chunk framing |
| `audio.js` | capture, playout, and the accounting of what was actually heard |
| `video.js` | screen and camera as protocol events |
| `tools.js` | the bridge to this process |
| `app.js` | which event means what |

That is deliberate. This page is the most complete worked example of the
protocol anyone will read, and a bundled artefact is not something you can
read.
