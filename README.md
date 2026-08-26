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

**On any provider.** Thirty language models, ten recognisers, and nine
synthesisers resolve through one catalogue — OpenAI, Anthropic, Google, xAI,
DeepSeek, Qwen, GLM, MiniMax, Kimi, Mistral, the marketplaces, and whatever is
running on your own machine. `openrealtime providers` lists them, and
`-probe` asks a provider what it serves rather than trusting a constant that
went stale. See [providers](docs/providers.md).

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

Two boundaries define the division of labour between the models:

> **Fast is proposal-only by default and may execute only an explicit bounded
> computer-action lane. Slow cannot speak.**

Fast answers the question the user actually asked, immediately. Slow reasons
and acts, concurrently, and the voice says what it found. The default remains
proposal-only. `-fast-computer-use` is a two-key exception for realtime
reaction: the descriptor must grant execution and the current invocation sees
only exact direct standard `computer.*` definitions admitted by a live filter;
screenshot and wait remain slow-only observation control. Every other call is
committed as a non-executable proposal. Confirmation, the target fence, the
irreversible-action ledger, and audit still run at the point of effect.

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

- Fast calls are structurally non-executable by default. The opt-in reflex lane
  exposes only exact bounded computer actions on committed observation turns;
  arbitrary and confirmation-requiring client tools stay out of it.
- Confirmation is a developer's declaration per tool, never an inference, and
  an unattended deployment refuses what it cannot get authorised.
- Screen text is data forever: the log refuses to record it as user speech, and
  providers see it fenced with a standing instruction that it is a quotation.
- Computer-use actions are bounded by a declared target and a declared
  coordinate space. There is no ambient-desktop option.

`computeruse/injection` is the release gate: a compromised screen, both models
taken in, a dangerous arbitrary tool declared, and nothing happens in both
default and constrained-fast modes. See
[safety](docs/safety.md).

## Audiovisual computer-use evaluation

The release and capability suite is repository-owned: eight deterministic
audio/video/browser task families, each run with pixel and set-of-mark
grounding. It covers static control, transient UI, live dashboards, a moving
game, spoken visual choice, a separate physical-camera feed, authorization,
and multi-step form entry. Correctness and deadline success are reported
separately, alongside cue-to-action, observation, and action-execution latency.

```sh
openrealtime bench realtime-cu -out results/realtime-cu.json
```

`bench dynacu` remains available as optional independent validation against the
published AOI environment. OpenRealtime does not depend on it and never uses it
as its release gate. See [the benchmark harness](docs/benchmarks.md).

## Documentation

| | |
| --- | --- |
| [Quickstart](docs/quickstart.md) | run it, check it, connect to it |
| [Providers](docs/providers.md) | every model provider it runs on, and how to add one |
| [OpenAI's own client](examples/sdk-client/README.md) | the compatibility claim, checked by the published SDK |
| [The developer console](console/README.md) | both transports, video, and tools on your own machine |
| [The test surface](surface/README.md) | every channel, both directions, on one page |
| [Architecture](docs/architecture.md) | the four subsystems and why they are separate |
| [Bindings](docs/bindings/README.md) | which voice stack, and what each one owns |
| [The OpenRealtime Protocol](docs/protocol/openrealtime-1.md) | normative spec for video, observations, and computer use |
| [The sidecar protocol](docs/sidecar-protocol-1.md) | the process boundary for models not written in Go |
| [Transports](docs/transports.md) | WebSocket, WebRTC, LiveKit |
| [Safety](docs/safety.md) | authority, confirmation, injection, blast radius |
| [Operations](docs/operations.md) | health, metrics, logs, failure behaviour, support policy |
| [Deployment](deploy/README.md) | the container, the release binaries, colocated model serving |
| [Measurement](docs/measurement.md) | what is measured, and what is claimed |
| [The benchmark harness](docs/benchmarks.md) | running a suite, reading a cell, adding one |
| [Two agents, talking](docs/simulation.md) | conversations between two agents, and what they check |
| [Efficiency](docs/efficiency.md) | what each part of the loop costs |

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
