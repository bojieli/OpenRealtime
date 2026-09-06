# ADR-0001: Go production engine and protocol generator

- Status: accepted
- Date: 2026-08-17
- Amended: 2026-08-21 (see Amendment)

## Context

Realtime media framing, duplex transport, cancellation, bounded queues, and
fine-grained scheduling are long-lived production responsibilities. A Python
runtime would add interpreter overhead, less predictable tail latency, and a
second deployment environment to the timing-critical path.

Rust was considered, but no Rust toolchain is present in the target workspace.
Go 1.25 is already installed and provides compiled native binaries, efficient
networking, explicit contexts, atomics, a production profiler, and a race
detector. The project needs measured latency rather than language folklore.

## Decision

Implement the engine, protocol generator, replay, tests, and analysis utilities
in Go. Python is not a runtime or build dependency. Use integer nanosecond and
absolute-sample arithmetic, keep streaming buffers bounded, and include race
and release benchmarks in gates.

Generate the OpenAI Realtime event registry and complete nested JSON Schema
closure from a cryptographically pinned official OpenAPI revision. Preserve raw
messages at the boundary so schema support does not discard future fields.

## Consequences

The same binary can run replay, conformance, future live transport, and research
instrumentation. Garbage-collector and scheduler jitter remain measurable
variables; M2 must profile them under load. A Rust or native media subcomponent
requires evidence that Go is the limiting stage plus protocol conformance traces
showing unchanged observable behavior.

## Amendment (2026-08-21)

The decision stands. Two parts of the reasoning are corrected, and one cost that
was not stated is recorded.

### The toolchain-availability rationale is superseded

The original Context says Rust "was considered, but no Rust toolchain is present
in the target workspace." That is an accident of the workspace, not an
engineering argument, and it is not why Go is correct here. It is left in place
because an ADR is a record of what was decided and on what basis, and deleting
weak reasoning is worse than marking it superseded. The substantive reasons are:

**The core problem is concurrency orchestration, not compute.** Safe points,
cooperative cancellation, batching, backpressure, and several asynchronous
producers writing one log. Goroutines, channels, `select`, and `context` express
that more directly than the alternatives — `eventloop/coordinator.go` reads close
to its own specification as a result. The same logic contends with the borrow
checker in async Rust and with the GIL in Python.

**Heavy compute is out of process by design.** Model work runs behind a sidecar
boundary, so the core never needs an ML ecosystem. That removes Python's main
advantage entirely rather than trading against it.

**Contributor cost is part of the requirement.** Go is deliberately small; an
unfamiliar package is readable in an afternoon, and compilation is fast enough to
keep a contributor's loop tight. For a project seeking outside contributions,
that outweighs expressiveness.

**Longevity.** Static binaries with no runtime, and a compatibility promise that
has held since 2012.

### WebRTC strengthens the decision, and postdates the original

The v0.1.0 plan adds in-process WebRTC termination and a LiveKit agent participant
(`docs/openrealtime-v1-plan.md` §3.6, M13). Pion is the most mature non-C WebRTC
implementation and is pure Go, and LiveKit's own server is Go built on Pion. Both
transport adapters are therefore same-language work. This argument did not exist
when this ADR was written.

### The cost, stated plainly

**Go cannot express a sum type, and the central data structure is one.**
`trajectory.Item` is a `Kind` discriminant plus six nullable pointers, with
validity enforced at runtime by `validateKindLocked` rather than by the compiler.
A language with algebraic data types would make an entire class of invalid item
unrepresentable. This is a real and permanent cost of the decision, not an
oversight, and it is the strongest argument a future reviewer will have against
it. It does not justify a rewrite: the price would be a year of work, a working
conformance suite, a frozen `api/v1`, and a study pinned to an executable hash,
against a gain in type-system expressiveness.

Garbage-collector jitter remains the other measurable cost, unchanged from the
original Consequences: pauses are typically well under a millisecond against a
50–200 ms cadence budget, and the obligation to profile it under load stands.

### The decision is for the core, not for the repository

Go is the language of the engine. It is not the language of everything the
project ships:

- **Python** for model sidecars. Moshi, Qwen3-Omni, and MiniCPM-o are Python and
  will remain so. The sidecar protocol exists so that this is contained rather
  than excluded.
- **TypeScript** for the demo application and browser client.

Each language is used where it is genuinely best, and the process boundary keeps
any of them from constraining the others — the same argument as the protocol's
narrow waist, applied to implementation.

### Conditions for revisiting

Unchanged and reaffirmed: replacing Go for any component requires evidence that
Go is the limiting stage, plus protocol conformance traces showing unchanged
observable behaviour. Type-system preference is not such evidence.
