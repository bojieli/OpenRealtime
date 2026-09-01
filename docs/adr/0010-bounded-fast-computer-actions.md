# ADR-0010: Bounded fast computer actions

## Status

Superseded as a binding/CLI topology mode by
[ADR-0015](0015-agent-topology-is-a-versioned-graph.md). Retained as a measured
reference composition and for its bounded authority, confirmation, target,
ledger, audit, and result-feedback requirements. It historically narrowed the
first boundary in ADR-0006 for an explicit realtime computer-use deployment.

## Context

ADR-0006 made fast proposal-only and slow silent. That is the safe general
arrangement for business tools, but it makes visual reaction inherit the slow
provider's latency even when the task is an unambiguous click whose cue will
disappear in a second. Measurement separated the delay: browser execution was
milliseconds, observation arrived in hundreds of milliseconds, and the
slow-only action authority added seconds.

Granting the fast provider the whole tool catalogue would collapse a useful
boundary. A small low-latency model should not acquire transfers, messages, or
arbitrary client functions because one deployment also needs to click a moving
target. Nor may client execution bypass server confirmation merely because the
effect occurs across the protocol.

## Decision

Keep proposal-only fast cognition as the default. Add an opt-in cascade mode,
`-fast-computer-use`, with two structural keys:

1. the fast provider descriptor declares execution authority;
2. cognition has a non-nil live tool filter.

The filter attaches tools only on a committed-observation fast turn, or on a
speculative turn that can be adopted only when the endpoint commits the same
observation. Holding lines, interjections, and turns voicing background results
receive no fast tools.

Only exact names in the repository-defined computer-use vocabulary qualify.
A qualifying reflex action operates directly on the current frame;
`computer.screenshot` and `computer.wait` remain slow-only observation-control
tools, so the reflex lane cannot discard fresh streaming evidence or sleep
through a transient cue.
A server declaration must have an in-process dispatcher. A client declaration
must have a non-empty target and `confirm: never`; policy- and always-confirmed
client actions remain outside the reflex lane. An undeclared or filtered call
is committed as a proposal even though the descriptor can execute declared
calls.

Both local and remote implementations cross `action.Tools`. The boundary
re-reads trajectory authority, answers declared confirmation, transitions the
irreversibility ledger, and emits an audit record. The client's result closes
the commitment and enters the shared trajectory before further reasoning.

## Consequences

Simple time-sensitive computer actions no longer wait for the high-intelligence
lane. Observation control remains with slow. Slow still runs in the reference
fast+slow rollout, sees the fast action and its result, and handles ambiguity,
authorization, planning, arbitrary tools, and dependent work.

The fast descriptor alone is insufficient and is rejected without a filter;
the filter alone is rejected without execution authority. This makes an
accidental half-configuration fail at startup rather than silently changing
the security model or silently doing nothing.

Client-defined benchmark environments can exercise the lane without moving
their evaluator into the server, but only after declaring the bounded target
and no-confirm consequence policy explicitly. This keeps the repository-owned
protocol benchmark representative of external clients while retaining the
same action boundary as production.

ADR-0006 remains the rule for every other binding and for cascade when the
option is absent.
