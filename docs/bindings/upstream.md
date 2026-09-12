# The `upstream` binding

Use a hosted Realtime endpoint for voice while OpenRealtime manages a separate
background reasoner and tool execution. This is the recommended first-run path
when you do not have local model services.

```bash
export GEMINI_API_KEY="your-key"
openrealtime serve \
  -binding upstream \
  -upstream-provider google \
  -slow-provider google
```

For OpenAI, set `OPENAI_API_KEY` and select `openai` for both provider flags.
The [provider catalog](../providers.md#realtime-endpoints-the-upstream-binding)
lists alternatives and current connection status.

## What it adds

The binding mirrors the remote conversation into the shared trajectory. The
engine's background model reads that context, requests authorized tools, and
returns information for the remote voice to present. Voice and background
providers can be configured independently.

## Two distinctions that carry the design

**What the remote said is evidence, not authorship.** It is committed with
observer authority rather than as an assistant item, because the reasoner did
not produce that text and must not mistake it for its own prior reasoning. It
also does not open a turn: a mirrored voice that did would have the reasoner
answering the voice model instead of the person.

**The remote is told about the tools but never given authority over them.** The
same proposal-versus-execute boundary every other binding holds. Calls from the
engine's reasoner go to your client or to a server-side dispatcher; the remote
is never asked to execute one.

## The hand-off

The reasoner's answer reaches the remote as a conversation item followed by a
response request. That uses nothing but the base protocol, which is what makes
this binding portable across any Realtime-compatible endpoint rather than tied
to one vendor's internals.

## Floor

The remote's own endpointing decides turns by default. `-floor engine` is the
comparison, and it is worth running if your tasks involve spelled identifiers
or digit strings.

## What it cannot do

Computer use is reported as unsupported, and video is supported only when a
video observer is configured here (`-observers video`): the base protocol
gives no way to ask a remote endpoint whether it accepts a frame, so a frame
never goes to the remote. It goes to this side's observer, and what the
observer narrates reaches the remote as context. Without an observer, video
is reported as unsupported at negotiation, as it always was.

## Behind GPT-Live

`-upstream-provider openai-live` puts this binding behind an endpoint that was
built for it. GPT-Live does not reason or call tools; it *delegates* and keeps
talking while it waits, and with client delegation the endpoint asks this
process for help. The binding's reasoner is therefore the backend the vendor's
design expects rather than a second model bolted to the side of one. Four
things change, all verified against the real endpoint.

**The reasoner runs when the voice asks.** A delegation is what the interaction
layer already calls an escalation - the fast turn handing the work on - and on
this dialect the reasoner runs on it and not on every turn. Every turn is still
mirrored, so the reasoner has the whole conversation when it is asked. The
switch is `-upstream-delegation-gating auto|on|off`; `auto` gates on GPT-Live
and nowhere else, because nowhere else ever asks. Typed input runs the reasoner
regardless: the voice never saw it, so it will never delegate it.

**Evidence reaches the voice silently.** An observation that is not the user -
a client's system message today, a screen change once a video observer runs
here - goes to the voice as *thinking*: something to know, not something to
say, with no model call in between. The remote's own mirrored words are never
pushed back to it. On a Realtime endpoint the same evidence becomes a
conversation item with no response requested, which is the portable
equivalent. Pushes are coalesced, split at the vendor's 500-token cap, and
suppressed once the endpoint reports its context window over 90% full; the
trajectory keeps everything regardless.

**Cancel is an instruction.** Live has no response to cancel. `Cancel` becomes
`session.instructions.append` telling the voice to stop, which interrupts
speech in progress - measured on the real endpoint at "Here is the full", cut
off there.

**The meter is visible and bounded.** `Status().Remote` carries the vendor's
session id, its expiry, billed seconds, the context-window ratio, and the open
delegation; each also reaches the debug stream. A full-duplex session is billed
by the second and kept alive by this binding's own silence frames, so
`-upstream-idle-timeout` (default 15 minutes on GPT-Live, none elsewhere)
closes a session that has heard no user speech, and says so.

Two things it still cannot do here, and says so: the floor cannot be given to
the engine (`ManualTurns` is false - there is no commit to drive), and a
session-instruction change after start is appended only when it fits the cap,
otherwise refused with an error the client sees rather than dropped.

### Eyes, memory, and the phone

**Seeing is this side's.** `-observers video` runs the cascade's screen
narrator behind this binding. A frame goes to the observer, never to the
remote; the narration is committed as observer-authority evidence and pushed
to the voice as thinking, with no reasoner call. The `video.input` and
`observations` capabilities are negotiated from what is actually configured.

**A voice that speaks first.** `-upstream-greeting` sends one instruction once
the session has started - on GPT-Live down the instruction channel with input
audio already running, which is the vendor's own recipe.

**Standing instructions said out loud.** When the interaction extraction pass
is configured, "keep it short from now on" pins a rule and steers the voice
with it, and "never mind" lifts it and says so. The pass runs off the mirror,
because it is a model call and the mirror is the remote's read side.

**The vendor's clock.** GPT-Live reports every transcript on its own session
timeline. Those milliseconds are translated onto this side's clock as the
observation's source time, so latency evidence on this binding measures the
vendor's timing rather than arrival.

**Bounded sessions.** `-upstream-max-sessions` refuses the *n*+1th session
here, with `ErrAtCapacity`, rather than letting the vendor refuse it
mid-conversation at its tier limit.

**A telephone leg.** `-upstream-audio-format pcmu|pcma` opens the session in
G.711 at 8 kHz: the caller's 24 kHz is resampled and companded on the way in
and expanded and resampled on the way out, and the keepalive is the codec's own
silence byte. Verified against the real endpoint: a µ-law session spoke a
hand-off and its audio expanded to audible PCM.

**Storage, forking, and recording.** `-upstream-store` asks the vendor to keep
a resumable recording. With one, a dropped connection is *forked* rather than
started cold - the vendor's own recovery guidance - transparently to the
caller, up to three times; and the recording is downloadable afterwards
(`gptlive.Recording`). On a project that does not permit data persistence the
vendor refuses the start over the store flag, and the session restarts
unstored and reports `storage_refused` rather than failing. That is the
project this was built on, so forking and recording are verified against a
fake built from the specification and not against the vendor; the refusal path
is verified against both.

**A sideband.** `gptlive.Attach` opens a second socket onto a running session:
it receives the session's events and reflected audio with timing, may steer
and push context, and refuses audio. The vendor offers it for sessions whose
primary connection is WebRTC or SIP - the topology in which this process is
not on the audio path - and answers 404 for a WebSocket primary, which is what
this environment has. It is verified against the fake.

## What the remote is told

The remote speaks this protocol, so a client's declarations are forwarded to it
rather than interpreted here. Three travel on every `session.update` the
binding sends, not only the first — a hand-off re-sends the session to carry an
answer, and one sent without them would undo the client's choices as a side
effect of speaking:

| Declaration | Why it is forwarded |
| --- | --- |
| `instructions` | Composed with this binding's own, which tell the remote a reasoner will hand it answers to say. |
| `turn_detection: null` | The remote holds the floor here, so it is the side that has to stop ending turns on silence. |
| `output_modalities` | A remote told nothing synthesises a full audio response for a client that asked for text — billed as audio output, sent over the network, and dropped on arrival. |

The modality is the one where the cost is the provider's meter rather than a
local GPU, which is why it is forwarded here and not on the bindings that
synthesise locally.

## Resource guidance

Nothing local runs the voice. An upstream session holds one WebSocket to the
provider and one background reasoner, so a deployment with no GPU at all can
serve it - which is the point of the binding, and why the quickstart reaches a
working session in the same number of steps either way.

What it costs instead is a credential and a round trip. The provider's latency
is the floor on every turn, and it is not something the engine can page over:
budget for the network rather than for capacity. The reasoner is the only
component you choose the placement of, and hosting it near the provider rather
than near the client is usually the wrong instinct - it is not on the voice
path.
