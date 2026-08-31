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

**It runs composable, evolving architectures.** A cascade of recogniser,
language model, and synthesiser; a speech-to-speech foreground under external
predicates or a policy model; a native-interaction foreground; or a remote
Realtime endpoint. The repository-owned architecture catalog versions those
compositions over shared binding machinery instead of treating them as closed
model species.

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

Two boundaries define the division of labour between the conversational models:

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

For voice+vision deployments, the recommended profile keeps that voice path
unchanged and adds a separate silent visual reflex model. It sees only the
current task, the newest image per source, and the exact bounded action schemas;
it returns one typed `act`, `wait`, or `abstain` decision under a hard deadline.
Abstention, malformed output, and timeout fall through to the existing slow
reasoner. The default `voice` profile does not construct this controller.

## Architectures and bindings

| Example architecture | Perception | Fast | Slow | Action | Interaction | Floor |
| --- | --- | --- | --- | --- | --- | --- |
| `cascade.controlled@3` | engine | engine | engine | engine | one predicate controller | engine |
| `cascade.composed-policy@1` | engine | engine | engine | engine | predicates + policy, arbitrated | engine |
| `omni.external-policy@4` | model | model | **engine** | model | one engine policy | engine |
| `omni.native-policy@3` | model | model | **engine** | model | one native controller | engine |
| `omni.native-full@3` | model | model | **engine** | model | one native controller | model |
| `realtime.remote@3` | remote | remote | **engine** | remote | one remote controller | remote |

The slow column never varies — that is the whole differentiator. List the
immutable catalog with `openrealtime architectures list`, inspect a revision
with `openrealtime architectures show omni.external-policy@4`, and launch one
with `openrealtime serve -architecture omni.external-policy@4 ...`. Each current
revision selects both an exact interaction-evidence capability vector and an
exact selected-controller vector plus arbitration rule. Direct vision, speaker
identity, a silence clock, or a predicate/text composition cannot hide behind
one coarse `transcript` label. Named
bindings remain concrete adapters and compatibility presets. See
[architecture](docs/architecture.md), [architecture experiments](docs/architecture-experiments.md),
and [bindings](docs/bindings/README.md).

## Transports

WebSocket is the only entrance to a session. A WebRTC adapter and a LiveKit
agent participant sit above it and speak it like any other client, so a browser
gets echo cancellation, jitter buffering, and loss concealment without the
server growing a second way in. If an adapter could express something a plain
WebSocket client cannot, that would be a defect — and there is a test that says
so. See [transports](docs/transports.md).

## Safety

- Fast calls are structurally non-executable by default. The opt-in visual
  reflex role exposes only exact bounded computer actions on current visual turns;
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
| [Composable presentation](docs/composable-presentation.md) | descriptor-locked browser and native clients over the same public server APIs |
| [The native macOS developer app](macos/README.md) | microphone, camera, screen, browser-use, desktop control, local tools, files, and millisecond traces |
| [Architecture](docs/architecture.md) | the four subsystems and why they are separate |
| [Composable agent graph proposal](docs/composable-agent-graph.md) | the typed element graph and refactoring plan for general multimodal realtime agents |
| [Evolving architectures](docs/architecture-experiments.md) | catalog revisions, live attestation, and controlled P/T/C/N experiments |
| [Bindings](docs/bindings/README.md) | which voice stack, and what each one owns |
| [The OpenRealtime Protocol](docs/protocol/openrealtime-1.md) | normative spec for video, observations, and computer use |
| [The sidecar protocol](docs/sidecar-protocol-1.md) | the process boundary for models not written in Go |
| [Transports](docs/transports.md) | WebSocket, WebRTC, LiveKit |
| [Safety](docs/safety.md) | authority, confirmation, injection, blast radius |
| [Operations](docs/operations.md) | health, metrics, logs, failure behaviour, support policy |
| [Deployment](deploy/README.md) | the container, the release binaries, colocated model serving |
| [Measurement](docs/measurement.md) | what is measured, and what is claimed |
| [The benchmark harness](docs/benchmarks.md) | running a suite, reading a cell, adding one |
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
