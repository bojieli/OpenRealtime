# Operations

## Running it

```sh
openrealtime serve -listen 0.0.0.0:8765 -token-env OPENREALTIME_TOKEN
```

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

  It is absent rather than zero on `omni`, `duplex`, and `upstream`. Those
  models hear the user directly and there is no recogniser to time; a zero
  would read as one answering instantly.

- **A policy model fails or times out.** The policy falls back to its rule:
  backchannel to silence, projection to silence-only endpointing.
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
