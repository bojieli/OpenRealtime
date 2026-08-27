# ADR-0009: One model decides what the agent does in an instant

## Status

Proposed, and measured. Shipped behind `-policy-models interaction`, off by
default. Extends ADR-0005.

End to end it takes a scripted-conversation suite from 33% to 60%, and two
capabilities from never working to working: counting out loud while somebody
keeps talking, and cutting into a sentence to correct something. One capability
it was built for - holding silence through a pause somebody asked for - still
does not work reliably, and the cause is extraction rather than the decision.
See `measurement.md` F13-F18 and `interaction-findings.md`.

## Context

ADR-0005 separated the data plane from the interaction control plane and put
five predicates in the second one: the floor, deferral, backchannel, turn
projection, and overlap classification. Each answers one narrow question from
one narrow input.

Two problems with that shape only became visible with the whole set built.

**They cannot disagree coherently.** Each sees a slice — silence duration,
duplex state, a partial transcript — and none sees the question the others are
answering. When they conflict, nothing adjudicates, because no component holds
the whole decision.

**None of them can read the conversation.** This is the serious one. The entire
input to every interaction policy was:

```go
type Context struct {
	NowNS    uint64
	Duplex   session.Snapshot   // who is speaking
	Revision Revision           // the newest partial transcript
	Phase    trajectory.Phase
}
```

Acoustics and a partial transcript. So when somebody says *wait, let me
finish*, or *tell me the moment the build lands*, or *count them out loud as I
mention them*, that instruction was not merely ignored — there was no path by
which it could arrive. An interaction policy that lives only in configuration
cannot be changed by the people it governs, and people set these policies out
loud constantly. That is not a policy. It is a setting.

## Decision

Replace the five predicates with one model, asked one question on the arrival
of evidence: **what does the agent do right now?**

A third authority rule joins the two in ADR-0006:

> Fast is proposal-only by default and may execute only an explicitly filtered
> bounded lane. The slow provider cannot speak. (ADR-0010)
> **The interaction model cannot produce content.**

It selects an act; cognition fills it. Interaction still decides only *when*.

### The acts select existing machinery

```
stay-silent    engage nothing
speak-through  fast phase, floor stays with the other party
answer         fast (+slow per rollout), floor transfers
interrupt      fast phase, floor taken from someone still holding it
call-tool      slow phase only  — already "the phase that may act and may not speak"
keep-speaking  no new engagement, existing output continues
stop-speaking  no new engagement, existing output cancelled
```

Nothing here is new machinery. `call-tool` is ADR-0006's second boundary
under another name.

### Two rules carry more weight than the model

**Inertia is the default in every state.** An agent mid-sentence keeps talking,
a silent one stays silent, one deliberating keeps deliberating. Nothing about
the agent's own situation makes acting necessary; only evidence does. So a
policy model that fails, times out, or cannot be reached falls back to silence
rather than to interrupting somebody.

**A model is never asked a question it cannot answer.** When somebody starts
making noise the recogniser has produced nothing yet, and acoustic onset alone
cannot distinguish a correction from a cough from the next item on a phone
menu. There is no answer to give, so none is asked for. This also retires a bug
class: barge-in triggered by voice activity has to guess, and a door slam
cancels the agent. Content-triggered barge-in does not, because noise produces
no content.

An earlier draft handled that gap by *ducking* — dropping the agent's volume on
acoustic onset and restoring or stopping once words arrived. It was rejected.
The premise was that 250ms of overlap is late, and it is not: people routinely
talk over each other for 200–500ms before one yields, and finishing the word
you are on is the human behaviour. Worse, with echo cancellation in the loop,
residual echo of the agent's own voice reads as onset, and the volume would
breathe for reasons nobody could diagnose.

### Standing instructions are extracted, not remembered

A policy set out loud has to survive the conversation moving on. It cannot live
in the rolling window, because truncating that window would then repeal a
policy somebody set — silently, at a moment determined by how much unrelated
conversation had happened since.

So a separate pass reads each completed turn and pins what it finds. It runs
off the critical path: a policy governs what happens next rather than what
happens now, so it has to land before the following utterance rather than
before this reply.

Three properties hold it honest. It is a separate invocation, never another
paragraph in the reasoning prompt — instruction capacity is real and measured,
and one added paragraph moved unrelated decisions from 8/15 to 3/15. It records
and never adjudicates: when *don't cut me off* and *tell me the moment it
lands* collide, that conflict belongs to the model deciding the instant it
bites. And it is a state machine rather than a list, because people lift these
as readily as they set them, and because *wait, I have more to say* and *don't
cut me off* are the same request at two lifetimes — giving the first the
lifetime of the second leaves an agent permanently mute for a reason nobody
would connect to the sentence that caused it.

### The window grows and truncates rather than rolling

The conversation an interaction model reads is a cached prefix. Dropping the
oldest turn each time it gains one changes where the prefix starts, which
invalidates every token after it — so a window that shifts every turn
re-prefills the whole prompt every turn and the cache never pays. It grows
instead, and only on passing an upper bound truncates in one step to a lower
one. The same hysteresis, and the same reason, as the onset and release
thresholds on the speech gate.

## Consequences

**The interaction model is a separate model with its own cache.** Sharing one
with the fast phase buys nothing across two different models, and a separate
one is free to read a projection shaped for deciding rather than for answering
— different content, and a clock in milliseconds where cognition's is rounded
to seconds.

**It must decide without reasoning.** Enabled on an 8B, reasoning scored 9/23
against 13/23 with it off, and took 5.5 seconds. That is a constraint on the
design, not a preference.

**Some standing policies govern what is said, not only when.** The split
between the two subsystems is not as clean as the names suggest: told to count
out loud, a runtime that chose the right acts at the right instants still
produced nonsense until the voice was told what it had been called for. So
pinned policies reach cognition as well, and a turn taken by interrupting is
marked as one, because what belongs in a turn taken from someone still speaking
is not what belongs in one they offered.

**It is off by default and earns the swap by measurement.** The predicates have
measured behaviour on four benchmark suites. Shadow mode runs the model beside
them on every revision, records its answer against theirs, and acts on nothing.

## Alternatives considered

**Add the missing axis only** — split the endpoint decision into floor-transfer
and act-now, and leave the predicates in place. Smaller and lands sooner, but
keeps four components that each see a slice, which is the disease rather than
the symptom.

**Infer interaction ownership from a model-native floor.** Rejected by
ADR-0011. A floor boundary and an interaction act are separate decisions, and
a native-capable model may be run under an external controller as a controlled
selection. The ownership vector must say which one is active; capability must
continue to report that both are available.
