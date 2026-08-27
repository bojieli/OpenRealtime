# ADR-0005: Separate the data plane from an interaction control plane

## Status

Accepted, v1.0; model-taxonomy language amended by
[ADR-0011](0011-capabilities-not-model-species.md).

## Context

The session that served the first live cascade was 819 lines, and it was that
long for a specific reason: it interleaved data transformation with timing
policy. Deciding when to trigger a continuation, whether to speculatively
pre-start, who held the floor, and whether to cancel in-flight speech were all
expressed inline among the code that moved audio, called models, and emitted
frames.

The cost was concrete. Adding a second speech path meant forking that file.
Varying a timing policy for measurement meant editing it. And the policies
themselves could not be named, which meant they could not be reported, compared,
or replaced.

## Decision

Split the runtime into a **data plane** of three subsystems — perception,
cognition, action — and an **interaction control plane** that decides when each
of them acts.

Perception and action are duals: perception gates a continuous external stream
into discrete commitments, action paces discrete commitments back into a
continuous stream. Cognition is not a boundary at all — it reads the log and
appends to the log. What the three share is only that data flows through them,
which is the definition of a data plane.

Interaction carries no data. Trigger cadence, speculative pre-start, floor
ownership, barge-in, and commit-versus-cancel are decisions about *when*, made
over evidence from all three. The borrowing from networking is exact rather
than metaphorical.

Every interaction decision becomes a named policy with an interface and a
shipped default.

## Consequences

**A policy that cannot be swapped cannot be measured.** Every axis the
measurement program varies is now a constructor argument and a command-line
flag, so a measured cell is a command line rather than a build.

**"Control plane" is structural, not a ranking.** Interaction may be selected
from an engine policy or a native model capability. Concurrent I/O, native
floor, and native interaction often occur together but are independent. For a
composition with engine interaction, responsiveness is manufactured here;
selecting native interaction moves that policy into the foreground model
without moving slow cognition, authority, or audit with it.

**Two things stayed out of it, deliberately.** The division of labour between
fast and slow, and the granularity of a spoken answer, live in the phase
instructions rather than in components — building a policy interface around
something a sentence already does would be machinery for its own sake. And
triage of an arriving event is the event loop's existing job, not a new policy.

**The cost is indirection.** A turn now crosses more package boundaries than it
did, and following one requires reading four files rather than one. That is the
price of being able to replace any of them, and it is the trade this project is
for.
