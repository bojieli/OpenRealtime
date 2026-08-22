# ADR-0004: One safe-point event loop for asynchronous interaction

- Status: Accepted for the experimental runtime
- Date: 2026-08-18
- Supersedes: ad hoc direct callback mutation in the target M8/M9 architecture

## Context

Realtime audio, ASR revisions, playback, user interruptions, and tool results
arrive concurrently. A model invocation consumes a fixed prefix and cannot in
general absorb an event in the middle of provider decoding. Letting callbacks
append independently creates races, stale output, reordered tool results, and
two incompatible histories. Content-based routing adds another failure mode:
words in an ASR transcript are not a reliable authority or priority signal.

Chapter 4 of the AI Agent book resolves the synchronous-model/asynchronous-world
tension with structured events, a unified queue, safe-point batching,
interruption through cancellation, and one event-loop owner. The target
architecture also has the canonical-continuation decision in ADR-0003, so the
event loop must preserve one fast→slow trajectory rather than resurrect the M4
foreground/slow-advice split.

## Decision

Adopt one provider-neutral event-loop state machine:

1. External producers enqueue typed events. They never append semantic state
   directly.
2. The event loop drains routine events in arrival order and commits the batch
   atomically at a safe point.
3. Trusted sources label an event `routine` or `interrupt`; the loop never
   derives priority from event text.
4. An interrupt cancels active processing but remains queued until that
   processing reaches a provider-supported safe point.
5. Every model continuation computes from an immutable version and atomically
   publishes only if that version remains current.
6. A batch containing a new observation runs fast. A tool-result-only batch
   resumes slow directly. Media-state-only batches run neither.
7. Tool calls execute only after a slow call commits. All results for one call
   batch cross back as one exact, identity-checked transaction.
8. Event occurrence time and canonical commit time are retained separately.
9. Playback transitions determine the assistant history visible to later
   models; content cancelled before playback and its native provider state are
   excluded.
10. Scheduler ticks and unstable private ASR revisions are opportunities, not
    automatically model-visible observations.
11. Ingress has an explicit hard bound and may reserve capacity for interrupts.
    Exhaustion returns backpressure; the coordinator never silently drops,
    merges, or overwrites a semantic event.
12. Acoustic output has a bounded commit horizon. Provider fragments are
    normalized into paced wire frames, and a slow assistant/tool safe point
    may invalidate only unplayed fast media. This media transition re-enters
    the queue as typed assistant state and does not cancel the slow transition
    that caused it.

The OpenAI Realtime protocol and stable `api/v1` remain unchanged.

## Consequences

There is a single linearization point for every semantic transition. Routine
events can be delayed to preserve provider continuity; source timestamps expose
that delay. Urgent events gain bounded cooperative interruption but still need
provider cancellation support for low stop latency. A late or non-cancellable
provider is safe because compare-and-append rejects stale output.

The design removes semantic routers and model-authored workflow states. It does
not remove policy: acoustic/semantic turn classification, stable-prefix
eligibility, authority, resource admission, and media commitment remain typed,
versioned, measurable policies outside model text.

The primary cost is that all semantic mutation must pass through the owner.
Independent tools and media operations may still run concurrently, but their
results rejoin through events. The implementation requires an explicit queue
bound and supports reserved interrupt capacity; deployments must additionally
specify how upstream producers react to returned backpressure.

## Rejected alternatives

- **Callback-owned mutation:** permits stale output and nondeterministic order.
- **Immediate mid-token event injection:** unsupported across providers and
  falsely implies that an indivisible invocation changed its source prefix.
- **Keyword urgency or tool routing:** brittle, language-dependent, and grants
  control semantics to untrusted content.
- **Independent fast agent plus slow adviser:** preserves split-brain state and
  requires a reconciliation protocol.
- **New public Realtime events for every internal state:** leaks
  provider/runtime details and unnecessarily forks an interoperable wire
  protocol.

The complete operational contract is in
[architecture.md](../architecture.md).

Amended after v1.0: slow no longer runs on every observation. Whether a turn
needs deliberation is the fast phase's judgement, expressed as a control marker
that never reaches the trajectory or the user; a turn the voice can answer
outright is answered once, which is what stops a simple question being
processed - and heard - twice. Every completion travels back as an event
rather than being acted on where it happened, so the gate decides when anything
is heard, and one turn may therefore span several responses.

Amended at v1.0: the loop gained the state this decision described but could
not represent. Committing an event and acting on it are now separate steps, a
deferral records what it is waiting for, and every deferral condition declares
the transition that releases it. It also gained a parallel branch, so a quick
question can be answered without waiting for work in flight.
