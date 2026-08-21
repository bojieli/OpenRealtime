# The safety model

Five concerns, five mechanisms. Each is enforced somewhere specific, and each
has a test that fails if the enforcement is removed.

| Concern | Mechanism | Enforced in |
| --- | --- | --- |
| The fast model taking irreversible action | proposal versus execute authority | `continuation`, `action.Tools.Dispatch` |
| Destructive actions | a declared `confirm` policy per tool | `action.Tools.confirm` |
| Prompt injection through observed content | typed provenance, fenced rendering | `trajectory`, `continuation.ObservationContent` |
| Blast radius | actions target a declared source in a declared context | `computeruse.Dispatcher` |
| Auditability | every action is a trajectory item with causal parents | `trajectory`, `computeruse.Records` |

## 1. The fast provider cannot act

This is structural rather than discouraged. A fast provider's descriptor
carries proposal-only tool authority, so a call it emits is committed as a
`tool_proposal`: a typed record of what capability it thought was needed, which
no dispatcher will execute.

The check is repeated at the point of effect. `action.Tools.Dispatch` reads the
canonical trajectory and refuses any call that was not committed as an
executable `tool_call`, so a proposal cannot become an effect however it is
routed — including by a caller that simply passes it to the dispatcher.

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

Getting that wrong is not a safe failure. With no policy supplied, `policy`
reads as `always`, and `always` with no confirmer denies — so a deployment that
turned computer use on would get an agent that can move the pointer and take
screenshots and can never press anything. A capability that cannot be
exercised is not a safe capability, it is a broken one, and it looks like a
broken model rather than a configuration nobody could satisfy.

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

**The authority boundary.** Even a model that is entirely taken in cannot cause
the effect. The fast provider has no execution authority, and the declared
confirmation requirement stands between the slow provider and the world.

`computeruse/injection` is the release gate for all of this. It drives a
compromised screen through a live session with both models taken in and a
dangerous tool declared, and asserts that nothing happens.

## 4. Blast radius is bounded by construction

A computer-use action names a video source; a source belongs to a declared
target; a target has a coordinate space. An action naming a source the target
does not own is refused, and so is a coordinate outside the space the model was
shown — refused rather than clamped, because an action that lands somewhere
nobody looked at is not a near-miss.

The shipped target is a browser context. There is no ambient-desktop option,
and that is not an omission.

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
