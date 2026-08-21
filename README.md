# OpenRealtime

> An open, self-hostable implementation of the OpenAI Realtime API that runs
> any voice stack, adds a background reasoner, and extends the protocol —
> minimally and compatibly — to realtime video and computer use.

```sh
go build -o openrealtime ./cmd/openrealtime
./openrealtime serve
./openrealtime probe
```

That is a working `/v1/realtime` on localhost, running fully locally, with
nothing to sign up for.

## What it is

**It is the Realtime API.** An official client connects unchanged. Every event
in both directions is validated against the pinned OpenAI schema on every
session, because a compatibility claim that is not continuously checked is a
compatibility claim that decays.

**It runs any voice stack.** A cascade of recogniser, language model, and
synthesiser; a speech-to-speech Omni model; a full-duplex interaction model; or
a remote Realtime endpoint. Four bindings, one runtime.

**It makes any of them smarter.** A background reasoner shares one trajectory
with the foreground model — reasoning and calling tools while the voice keeps
talking. A single-model server cannot do this by construction: there is no
second model, and no shared log to put one on.

**It sees and acts.** The OpenRealtime Protocol adds realtime video and
computer use over the same session, as a strict backward-compatible superset:
three events and two object extensions, on a base protocol with 66 wire names.

## The idea

Most of what makes a voice agent feel good is not in any of its models. A
recogniser knows nothing about turn-taking, a language model knows nothing
about the conversation's timing, and a synthesiser knows nothing at all. So
this system separates **what happens** from **when it happens**, and makes the
second one a set of named, swappable policies:

```text
Perception  →  gate the world into sparse, persistent text
Cognition   →  read the log, append to the log
Action      →  pace commitments back out into the world
Interaction →  decide when each of those happens
```

Two rules define the division of labour between the models:

> **The fast provider cannot call tools. The slow provider cannot speak.**

Fast answers the question the user actually asked, immediately. Slow reasons
and acts, concurrently, and a fast continuation voices what it produced. Both
are properties of the provider descriptor rather than routing decisions,
enforced where output commits — so a fast provider's tool call is recorded as a
non-executable proposal and can never become an effect.

## Bindings

| Binding | Perception | Fast | Slow | Action | Floor |
| --- | --- | --- | --- | --- | --- |
| `cascade` | engine | engine | engine | engine | engine |
| `omni` | model | model | **engine** | model | engine |
| `duplex` | model | model | **engine** | model | model |
| `upstream` | remote | remote | **engine** | remote | remote or engine |

The slow column never varies — that is the whole differentiator. See
[bindings](docs/bindings/README.md).

## Transports

WebSocket is the only entrance to a session. A WebRTC adapter and a LiveKit
agent participant sit above it and speak it like any other client, so a browser
gets echo cancellation, jitter buffering, and loss concealment without the
server growing a second way in. If an adapter could express something a plain
WebSocket client cannot, that would be a defect — and there is a test that says
so. See [transports](docs/transports.md).

## Safety

- A fast provider's tool call is structurally non-executable, checked again at
  the point of effect.
- Confirmation is a developer's declaration per tool, never an inference, and
  an unattended deployment refuses what it cannot get authorised.
- Screen text is data forever: the log refuses to record it as user speech, and
  providers see it fenced with a standing instruction that it is a quotation.
- Computer-use actions are bounded by a declared target and a declared
  coordinate space. There is no ambient-desktop option.

`computeruse/injection` is the release gate: a compromised screen, both models
taken in, a dangerous tool declared, and nothing happens. See
[safety](docs/safety.md).

## Documentation

| | |
| --- | --- |
| [Quickstart](docs/quickstart.md) | run it, check it, connect to it |
| [Architecture](docs/architecture.md) | the four subsystems and why they are separate |
| [Bindings](docs/bindings/README.md) | which voice stack, and what each one owns |
| [The OpenRealtime Protocol](docs/protocol/openrealtime-1.md) | normative spec for video, observations, and computer use |
| [The sidecar protocol](docs/sidecar-protocol-1.md) | the process boundary for models not written in Go |
| [Transports](docs/transports.md) | WebSocket, WebRTC, LiveKit |
| [Safety](docs/safety.md) | authority, confirmation, injection, blast radius |
| [Operations](docs/operations.md) | health, metrics, logs, failure behaviour, support policy |
| [Measurement](docs/measurement.md) | what is measured, and what is claimed |

## What this claims, and what it does not

v1.0 ships on **functional completeness plus verified correctness**. The
README says what the system *supports* and what has been *verified*. It makes
no claim that one configuration beats another.

Comparative claims are gated separately, on the [measurement
program](docs/measurement.md), which runs continuously after launch and
publishes each cell as it completes. Shipping before the most interesting
claims are provable is a deliberate trade: a system nobody can run is not
evidence of anything.

## Licence

Apache 2.0. See [LICENSE](LICENSE), [LICENSES.md](LICENSES.md) for dependency
provenance, and [CONTRIBUTING.md](CONTRIBUTING.md).
