# Operations

## Running it

```sh
openrealtime serve -listen 0.0.0.0:8765 -token-env OPENREALTIME_TOKEN
```

For a versioned deployment, select an exact architecture revision as well:

```sh
openrealtime architectures show cascade.controlled@3
openrealtime serve \
  -architecture cascade.controlled@3 \
  -listen 0.0.0.0:8765 -token-env OPENREALTIME_TOKEN
```

The architecture definition owns structural choices. Model/provider flags own
deployment choices. If an explicit `-binding`, `-floor`,
`-interaction-owner`, `-sidecar-capabilities`, `-sidecar-protocol`, or
`-policy-models`, `-interaction-floor`, `-interaction-sees`, observer, or
speaker-evidence setting
contradicts the selected definition, startup refuses
instead of silently changing the architecture. External catalogs use
`-architecture-catalog`; unpinned names are never accepted.

Authentication is a bearer token when `-token-env` names a variable that is
set, and absent when it is not. There is no middle setting: a deployment that
is reachable from anywhere and has no token is one you want to notice.

Every flag has a default that works on localhost. The ones that change
behaviour rather than location are listed under
[bindings](bindings/README.md); the rest are addresses and credentials.

## Health

```sh
curl -s http://127.0.0.1:8765/healthz
```

Reports the binding, its ownership declaration, its capabilities, the protocol
versions, and session counters. It is the fastest way to answer "what is this
process actually running", which is a question that comes up more often than it
should.

The process-level health response names the adapter. The post-handshake session
status is stronger evidence because it also carries the selected architecture
ID, revision, definition fingerprint, live stack capability vector, interaction
evidence representation, exact selected evidence-capability vector,
selected-controller vector and arbitration, transport/handoff,
speaker-identity adapter, and tool authority. Capture and validate it with:

```sh
openrealtime bench architecture inspect \
  -endpoint ws://127.0.0.1:8765/v1/realtime \
  -out results/runtime-status.json
```

Inspection refuses a legacy binding-only launch and any live status that does
not satisfy its catalog definition.

```jsonc
{
  "status": "ok",
  "binding": "cascade",
  "ownership": { "perception": "engine", "slow_cognition": "engine", "...": "..." },
  "capabilities": { "video": false, "computer_use": true, "fast_slow": true },
  "protocol": { "openai_realtime": "pinned", "openrealtime": { "version": 1 } },
  "sessions": { "sessions_started": 12, "sessions_completed": 11, "...": "..." },
  "recogniser": {
    "utterances": 340, "in_flight": 1,
    "advance_invocations": 5100, "advance_failures": 0,
    "advance_mean_elapsed_ns": 41000000, "advance_max_elapsed_ns": 220000000,
    "finalize_invocations": 340, "finalize_mean_elapsed_ns": 88000000, "...": "..."
  }
}
```

`/healthz` always returns 200 while the process is serving. It does not probe
the model backends: a health check that fails because a hosted provider is slow
takes a working server out of rotation for something a retry would have fixed.

## Metrics

```sh
curl -s http://127.0.0.1:8765/metrics
```

Counters only, and never content. A metrics endpoint that leaked conversation
would be a worse problem than having no metrics endpoint.

| Metric | Meaning |
| --- | --- |
| `sessions_started` / `sessions_completed` / `sessions_failed` | session lifecycle |
| `audio_frames_in` / `audio_frames_out` | media volume in both directions |
| `video_frames_in` | frames accepted from clients, before gating |
| `video_frames_dropped` | frames refused by the negotiated rate cap. A number that climbs says a client is not conforming to the limits it was told at negotiation. |
| `tool_calls_out` | authoritative calls handed to clients |
| `sessions_in_flight` | sessions admitted and not yet finished |
| `sessions_rejected` | upgrade requests refused because `-max-sessions` was reached. A number that climbs says add instances, not that the process is unwell. |

## Capacity and liveness

```sh
openrealtime serve -max-sessions 256 -write-timeout 30s -keepalive-interval 20s
```

Past admission a session holds a binding runtime and its provider connections,
so an unbounded gateway does not degrade under load; it exhausts the process
and takes every established session with it. `-max-sessions` turns that into a
`503` with `Retry-After`, which is a thing a load balancer can act on. It
defaults to `0`, meaning unbounded, because capping an existing deployment at a
number chosen here would be a worse surprise than the exhaustion it prevents —
but a production deployment should set it. Watch `sessions_in_flight` for a
while and pick a number above its peak.

The two liveness bounds exist because a WebSocket has no timeout of its own
once it is upgraded — the HTTP server's `IdleTimeout` stops applying at the
handshake.

`-write-timeout` bounds one send. A client that stops reading closes its
receive window, and without a bound the send blocks on the session context,
which ends only when the session does. Nothing ends it: the writer stops
draining, the send buffer fills, the handler blocks, the read loop blocks
handing it the next event, and the session is wedged for the life of the
process while every health check reports a healthy server. Thirty seconds is
past any stall a congested link produces and short of forever.

`-keepalive-interval` is the other direction. The server answers a client's
pings, so a client that sends them knows the server is alive; nothing tells the
server the reverse. A peer that disappears without a FIN — a NAT rebind, a
closed laptop, a dropped mobile handover — leaves a connection that is open on
this side only, and in a session where neither side is speaking there is no
write to discover it with. The ping is sent only after an idle interval, so a
session carrying audio never pays for it.

Both default to a working value and both take `0` to remove the bound. Sessions
that end this way are counted in `sessions_failed` and named in the log:
`client stopped reading` and `peer did not answer a keepalive ping` are
different problems with different fixes.

A `-launch-profile` composition takes the shipped write and keepalive defaults;
capacity is owned by the flag on `serve`.

## Logs

```sh
openrealtime serve -log-format json -log-level info
```

Structured, to stderr, and content-free for the same reason as the metrics.
Session start and completion carry the session identifier and duration; errors
carry a code and a message. `debug` adds nothing about what was said.

## What to watch

**First-audio latency after an endpoint** is the number a person experiences.
`openrealtime probe` prints it, and the browser demo shows it live.

**Deferrals without wake-ups.** The event loop records why committed work is
waiting and how long it has waited. Work that is deferred and never released is
the failure the invariant exists to prevent, so a deferral that persists is
worth alerting on.

**Policy-model refusals.** A policy model that keeps answering off its
enumerated list is a model too small for the job. The count is what tells you.

**Repair obligations.** A rising rate means commitments are being made too
early — the commitment policy is emitting before certainty more often than the
conversation supports.

## Resources

| Binding | Holds | Notes |
| --- | --- | --- |
| `cascade` | recogniser stream, fast model, slow model, synthesiser | the recogniser and fast model must stay warm; the slow model is where a hosted provider fits |
| `omni` / `duplex` | one model process per session, or one shared | use `-sidecar-address` to share an expensive model rather than loading it per session |
| `upstream` | one outbound WebSocket, plus the reasoner | the lightest to run; the remote does the work |

All local model work can compete under one admission governor with three
classes: interactive above speculative preparation above background. Policy
models are admitted at interactive class, video narration at background, and
speculative preparation in between — they compete for the same GPU rather than
existing beside it.

```sh
openrealtime serve -compute-capacity 8
```

It is **off by default**, and that is a deliberate refusal rather than an
oversight: the unit is abstract, the right number depends on the machine and
the models, and a governor with a made-up capacity would throttle a deployment
that was perfectly healthy. A deployment that is contending states its own
number; one whose providers are hosted competes for nothing local and should
leave it off.

With it on, watch the class timings. A speculative class whose wait time climbs
is preparation that will not be ready by the endpoint, which costs tokens and
saves no latency — turn it off with `-preparation endpoint-only` or raise the
capacity.

## The silence thresholds, and how they compose

Five numbers decide what silence means, they live in three packages, and each
is individually documented in a place that does not mention the others. They
are listed together here because a deployment that changes one has changed a
relationship, and because one of them silently overrode another until it was
measured.

| Setting | Default | Question it answers |
| --- | --- | --- |
| gate `SpeechDurationMS` | 120 ms | has a person started speaking, or was that a door |
| gate `PrefixPaddingMS` | 300 ms | how much of the onset to keep once the answer is yes |
| gate `SilenceDurationMS` | 500 ms | is this person still audible |
| `-projection-hold` | 1 s | how much longer to wait when a model says they are mid-thought |
| `-asr-cadence` | 200 ms | how often to ask the recogniser what it has |

They are ordered, and the order is the point. The gate decides audibility and
nothing else: below `SpeechDurationMS` there is no turn, and after
`SilenceDurationMS` the person is no longer audible. Whether a turn has *ended*
is a separate question, asked of the floor, and the floor may keep it open for
up to `-projection-hold` beyond the gate's answer. So the longest a turn can
stay open on silence alone is `SilenceDurationMS + projection-hold` — 1.5
seconds by default — and past that it ends whatever any model thinks, because a
floor that can be argued out of closing is not a floor.

`-asr-cadence` is orthogonal and easy to mistake for one of these. It sets how
often the recogniser is asked, which bounds how *stale* the words behind every
decision above can be; it does not decide anything by itself. Watch it against
`advance_mean_elapsed_ns`: a mean approaching the cadence is a recogniser that
cannot keep up, and every threshold above is then being applied to a transcript
older than it looks.

## Failure behaviour

- **A provider fails.** The session survives and the client sees an `error`
  event. A failed slow continuation does not end the conversation.
- **A recogniser stops answering.** Perception is the one provider whose
  silence produces no output at all: no transcript is finalised, so no
  observation is committed, so nothing downstream runs and the client waits on
  a turn that never arrives. The advance is bounded by the recogniser's own
  cadence rather than by `-request-timeout`, so the session fails in seconds
  with `asr_provider_error` naming the provider, rather than at the shared
  two-minute deadline with nothing to read.

  **Reachability is not liveness here.** A recogniser can accept connections
  and answer a session-open request instantly while never answering a chunk
  again — which is what a health check that only dials the port reports as
  healthy. Check that a chunk round-trips, not that the port is open.

  **A recogniser getting slower is visible before it stops.** `/healthz`
  carries a `recogniser` object on any deployment whose binding owns
  perception, which is `cascade`. The number to watch is
  `advance_mean_elapsed_ns`: a maximum jumps once on a single bad call and
  stays there for the life of the process, while a mean climbing over an hour
  is a recogniser degrading. Compare it against the cadence the advance is
  bounded by — a mean approaching `-asr-cadence` is a recogniser about to miss
  it, which is the failure above with a warning attached.

  The counters live on one buffer per utterance, and a buffer folds itself into
  a process-level accumulator when it closes. Utterances still open are read in
  place and counted in `in_flight`, because the utterance most likely to be the
  slow one is the one that has not finished — a total that only moved at the
  endpoint would go quiet during exactly the stall it exists to report.

  It is absent rather than zero on ordinary `omni`, `duplex`, and `upstream`:
  those models hear the user directly and there is no recogniser to time; a
  zero would read as one answering instantly. `omni+text-policy` reports its
  policy-only recognizer here.

- **A policy model fails or times out.** The policy falls back to its rule:
  backchannel to silence, projection to silence-only endpointing.
- **The agent goes quiet while it thinks.** The reasoning phase never speaks,
  so a turn that needs it produces a gap with nothing in it, and how long the
  gap lasts is a property of the question rather than of anything going wrong.
  What distinguishes it from a finished conversation is that the turn is still
  open: deliberation runs inside the response, so a client sees
  `response.created` without `response.done` and knows work is owed.

  Anything waiting on this system should wait on that rather than on a silence
  timer. A driver that ends a conversation after a few quiet seconds is
  measuring how fast the agent thinks, and reporting it as how well the agent
  works.
- **A model names a tool that does not exist.** It is recorded as a
  non-executable proposal and the conversation continues. It cannot execute —
  the dispatcher checks the name again at the point of effect — and it is not
  worth a session: a wrong name is a model mistake of the same kind as a wrong
  argument, and ending the call over one costs the user everything to prevent
  nothing.
- **A sidecar dies.** The session ends and the client is told. A sidecar that
  does not exit after goodbye is killed rather than left holding a GPU.
- **A client sends a malformed event.** It gets an `error` and the session
  continues. A bad event is a client mistake, not a connection failure.
- **A client never returns a tool result.** The Realtime protocol puts tool
  execution on the client, so between emitting a call and receiving its result
  the session is waiting on something it does not control. After
  `-client-tool-timeout` (two minutes by default, matching the provider request
  timeout) the unanswered calls are failed, the batch commits, and the client
  gets a `tool_result_timeout` error. Without the deadline that branch of the
  conversation never advances again and nothing says so.

  A client whose work legitimately runs longer returns a result immediately
  saying the work started, and reports the outcome as a message when it
  finishes. That keeps the model's view accurate at every moment, which holding
  the batch open does not.
- **Backpressure.** Ingress is bounded, with capacity reserved for interrupts
  so a routine burst cannot stop a barge-in from being heard.

## Support policy

| Surface | Stability |
| --- | --- |
| `api/v1` | stable. Breaking changes require a new import path. |
| OpenAI Realtime compatibility | pinned to a specific revision, validated in both directions on every session |
| The OpenRealtime Protocol | version 1, frozen at v1.0. Later versions are additive and negotiated. |
| The sidecar protocol | version 1, frozen at v1.0. Handshake refuses a mismatch rather than negotiating down. |
| `Binding`, `Observer`, `Narrator`, `Vision`, `Decider`, `Surface` | versioned from v1.0. A binding written against v1.0 keeps working. |
| Everything else | internal, and may change |
