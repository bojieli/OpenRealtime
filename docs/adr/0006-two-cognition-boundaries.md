# ADR-0006: The fast provider cannot call tools; the slow provider cannot speak

## Status

Accepted, v1.0. Extends ADR-0003.

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

Add a second boundary, symmetric with the first:

> The fast provider cannot call tools. The slow provider cannot speak.

Slow's output appends to the trajectory, and a fast continuation voices it.
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
