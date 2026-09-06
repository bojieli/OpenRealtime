# Safety and action authority

OpenRealtime separates a model's request from permission to execute it. Use
this reference when adding tools, enabling computer use, or implementing a
client that executes effects. For event schemas, see
[Protocol v1](protocol/openrealtime-1.md).

An action follows this path: model output → committed call → authority and
confirmation checks → target validation → effect → recorded result. The
checks apply whether the server or the client performs the effect.

| Concern | Mechanism | Enforced in |
| --- | --- | --- |
| The fast model exceeding a bounded action lane | per-invocation tool filtering plus proposal versus execute authority | `cognition`, `continuation`, `action.Tools` |
| Destructive actions | a declared `confirm` policy per tool | `action.Tools.confirm` |
| Prompt injection through observed content | typed provenance, fenced rendering | `trajectory`, `continuation.ObservationContent` |
| Blast radius | actions target a declared source in a declared context | `computeruse.Dispatcher` |
| Auditability | every action is a trajectory item with causal parents and producer-phase provenance | `trajectory`, `action.Record`, `computeruse.Records` |

## 1. Fast is proposal-only unless a bounded lane is explicitly opened

This is structural rather than discouraged. A fast provider's descriptor
carries proposal-only tool authority, so a call it emits is committed as a
`tool_proposal`: a typed record of what capability it thought was needed, which
no dispatcher will execute.

The recommended `voice+vision` profile can add an independent silent visual
reflex for cue-sensitive action. It receives only the current task, newest
image per source, and the live filtered action schemas, and must return one
`act`, `wait`, or `abstain` decision. A 650 ms hard deadline and malformed
output both become safe abstentions into the ordinary slow lane. Audio-only
sessions do not instantiate the controller. The compatible
`-fast-computer-use` flag remains available when a deployment intentionally
uses its voice model for the same bounded job.

Neither route exposes the session's general tool catalogue. Each requires
execution authority and a live filter, attaches tools only at an eligible
committed-observation safe point, and admits only exact standard computer-use
actions. A server action
must be backed by an in-process dispatcher. A client action must declare both a
target and `confirm: never`; a client tool requiring policy or human
confirmation stays outside the reflex lane. Names like `computer.exfiltrate`
are not standard actions and stay proposals. `computer.screenshot` and
`computer.wait` are standard but remain slow-only: a reflex cannot replace the
current streaming frame or deliberately sleep through a transient cue.

The check is repeated at the point of effect. `action.Tools.Dispatch` reads the
canonical trajectory and refuses any call that was not committed as an
executable `tool_call`, so a proposal cannot become an effect however it is
routed — including by a caller that simply passes it to the dispatcher.

Client-executed implementations use the same boundary. Before a call is
emitted over the protocol, `action.Tools.EmitRemote` verifies trajectory
authority, answers its declared confirmation, and crosses the irreversible
ledger. Returning the client result completes that commitment. Tool ownership
never implies action authority.

The same rule holds across a process boundary. A sidecar model is the fast
provider, and a tool call from a sidecar is refused with a reason it can see.

## 2. Confirmation is declared, not inferred

There is no reversibility class. Every output — speech, text, tool call, click
— is irreversible once emitted, so the runtime does not grade them; it holds
one commit boundary and applies it to all of them.

What remains per-action is a **confirmation requirement**, and it is a
developer's declaration about consequence rather than something the runtime
infers from a tool's name or arguments:

```jsonc
{ "type": "function", "name": "transfer_funds",
  "openrealtime": { "confirm": "always" } }
```

| Value | Meaning |
| --- | --- |
| `never` | dispatch without asking. The default, and what an ordinary tool gets. |
| `policy` | ask the deployment's policy; treated as `always` when none is configured |
| `always` | ask a human every time |

An unattended deployment refuses everything above `never`. An action nobody can
authorise should not happen because nobody was asked.

### What `policy` means for computer use

Every action in the `computer.*` namespace that changes anything — click,
double-click, drag, type, key, scroll — declares `policy`. Move, screenshot,
and wait declare `never`.

The policy the server supplies is the **declared target**: an action is
admitted when it names a video source the target owns, and refused otherwise.
That is the same fence the dispatcher enforces on coordinates, stated once as
a confirmation answer, and it is deliberately narrower than "yes" — a tool that
declares `always` is a different question and this does not answer it.

Without a confirmation policy, `policy` falls back to `always`; without a human
confirmer, such actions are denied. Configure the target policy or a confirmer
before expecting clicks and other effects to execute.

`-computer-confirm always` is therefore refused at startup rather than at
dispatch: this server has no confirmer to offer, so every action would be
denied, and failing at the flag says so where it can be read.

## 3. Observed content is data, forever

An agent that narrates screen text into its own context is an obvious injection
vector. Provenance is the defence and it is enforced in three places:

**The log.** An observation produced by an observer carries observer authority,
and the trajectory refuses to commit it any other way: observer authority under
the user phase, user authority under the observer phase, and observer-phase
content with no provenance at all are all rejected by `Store.Append`. There is
no code path that writes screen text as user speech.

**The rendering.** Observed content reaches a provider fenced inside a
delimited block with a standing, runtime-authored instruction that it is data.
Delimiters appearing inside the observed text are neutralised, so the content
cannot close its own fence and continue as though it were the runtime talking.
The user's own speech is not fenced: the defence applies to what was observed,
not to what was said.

**The authority boundary.** Even a model that is entirely taken in cannot turn
an arbitrary call into an effect. In the default arrangement fast has no
execution authority. In constrained-fast mode only the attached bounded
computer actions can become calls; a dangerous arbitrary tool remains an
undeclared proposal. Declared confirmation stands between every authoritative
call and the world, regardless of which phase produced it or whether the
implementation runs in the server or client.

`computeruse/injection` is the release gate for all of this. It drives a
compromised screen through a live session with both models taken in and a
dangerous tool declared, and asserts that nothing happens. A second gate runs
the fast provider with execution authority and proves the exact filter still
downgrades that dangerous call to a proposal.

## 4. Blast radius is bounded by construction

A computer-use action names a video source; a source belongs to a declared
target; a target has a coordinate space. An action naming a source the target
does not own is refused, and so is a coordinate outside the space the model was
shown — refused rather than clamped, because an action that lands somewhere
nobody looked at is not a near-miss.

The browser target restricts effects to its declared context. Native host
effects use separately composed profiles and negotiated authority; see the
[macOS guide](../macos/README.md). Enabling a voice session alone grants neither.

## 5. Everything is auditable

Every executed action is a trajectory item with causal parents, so any click
traces back through the continuation that produced it to the observation that
prompted it. The computer-use dispatcher keeps its own record of every attempt,
performed or refused, which is what makes an injection visible after the fact
rather than only preventable before it.

## What the client must do

Two things are the client's responsibility and are stated rather than assumed:

- **Echo cancellation and noise suppression.** A browser's WebRTC stack already
  does both well; doing them again server-side would be worse than doing them
  once, and a server compensating for unknown client-side processing would be
  guessing.
- **Authorising confirmations.** A deployment that declares `confirm: always`
  tools and supplies no confirmer has configured a system that refuses them.
  That is the correct failure, and it is worth knowing about before it happens
  in front of a user.
