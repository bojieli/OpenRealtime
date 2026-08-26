# ADR-0006: Proposal-only fast cognition and silent slow cognition

## Status

Accepted, v1.0. Extends ADR-0003. The first boundary is narrowed for an
explicit cascade computer-use mode by ADR-0010; it remains unchanged by
default and for every other tool and binding.

## Context

Fast and slow share one trajectory. ADR-0003 established the first boundary:
a fast provider's emitted call is recorded as a non-executable proposal, so a
low-latency model with minimal reasoning cannot cause a side effect.

The second boundary did not exist. Both phases could produce speech, and slow
output superseded fast output when it arrived. That created a race with three
distinct costs: slow could contradict something fast had already said and been
heard saying; a long written answer reached the synthesiser unedited, because
nothing condensed it; and the division of labour was invisible from outside —
which model owned what the user heard depended on timing.

## Decision

Add a second boundary, symmetric with the original first boundary:

> The fast provider cannot call tools. The slow provider cannot speak.

ADR-0010 later narrows the first clause for an explicitly filtered cascade
computer-action lane. Proposal-only remains the default; the silent-slow clause
is unchanged.

Slow's output appends to the trajectory as background state, and the next fast
turn speaks from it in its own words.
Both boundaries are properties of the provider descriptor, validated once at
construction and recorded on every item the provider produces, so the action
plane decides what may be voiced from the committed log rather than from a
configuration it would have to be handed separately.

## Consequences

**It costs a hop.** Voicing a slow answer requires a second fast continuation,
which is latency the previous arrangement did not spend.

**It buys three things.** Fast is always the last writer before audio, so slow
can no longer contradict something already heard. Fast condenses a written
answer into something worth listening to, which is a different job from
producing it. And the division of labour is legible: one model owns what the
user hears, one owns what the system does.

**It generalises across a process boundary.** A sidecar model is the fast
provider, and a tool call from a sidecar is refused with a reason it can see.
The boundary does not weaken because the provider is in Python.

**A single-provider arrangement is still legal.** The silent-slow requirement
is enforced only when the two are paired, because an `upstream` binding whose
remote model both speaks and answers is a legitimate configuration.

**Enforcement is at the commit point, twice.** The descriptor decides, and the
tool dispatcher independently re-reads the trajectory before any effect. A
proposal cannot become an action however it is routed, including by a caller
that simply hands it to the dispatcher.
