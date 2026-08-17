# ADR-0001: Go production engine and protocol generator

- Status: accepted
- Date: 2026-08-17

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
