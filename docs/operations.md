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
  "sessions": { "sessions_started": 12, "sessions_completed": 11, "...": "..." }
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

All model work competes under one admission governor with three classes:
interactive above speculative preparation above background. Policy models are
admitted at interactive class, because they compete for the same GPU rather
than existing beside it.

## Failure behaviour

- **A provider fails.** The session survives and the client sees an `error`
  event. A failed slow continuation does not end the conversation.
- **A policy model fails or times out.** The policy falls back to its rule:
  backchannel to silence, projection to silence-only endpointing.
- **A sidecar dies.** The session ends and the client is told. A sidecar that
  does not exit after goodbye is killed rather than left holding a GPU.
- **A client sends a malformed event.** It gets an `error` and the session
  continues. A bad event is a client mistake, not a connection failure.
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
