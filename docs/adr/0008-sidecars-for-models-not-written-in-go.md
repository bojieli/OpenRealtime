# ADR-0008: Models not written in Go live behind a process boundary

## Status

Accepted, v1.0.

## Context

Omni and full-duplex models are Python. Their inference stacks, their
tokenisers, and their audio front ends are Python, and there is no realistic
prospect of that changing.

There are two ways to use them from a Go engine. Embed a Python runtime, which
means cgo, a build that depends on the host's interpreter, and a test suite
that cannot run without a model. Or put them behind a process boundary.

The first is tempting because it avoids a protocol. It is also how a fast, race
detected, statically linked engine becomes a slow, fragile one: every test needs
an interpreter, every build needs the right wheels, and a segfault in a model's
audio front end takes the server with it.

## Decision

A model that is not written in Go runs as a **sidecar**: a separate process
speaking a documented, versioned protocol over a duplex byte stream.

The protocol is one JSON header line and an optional raw binary payload. Audio
is raw rather than base64, because a third more bytes and an encode pass on
every frame is a real cost on the hot path; everything else is readable in a
terminal.

**The conformance suite is the contract.** A sidecar that passes it works with
the engine whatever it is written in.

## Consequences

**Python stays out of the build, the test path, and the analysis path.** The
engine's tests run in seconds with no interpreter, and the reference sidecars
have a `--mock` mode so plumbing can be verified without a GPU.

**Capabilities are declared, so partial implementations are useful.** A sidecar
with no native voice detection gets an engine-owned floor; one that cannot
accept injected text gets an explicit hand-off instead. A model does not have
to implement everything to be worth running.

**A crash is contained.** A sidecar that dies ends its session and is reported;
one that does not exit after goodbye is killed rather than left holding a GPU.

**The cost is a copy and a hop.** Audio crosses a pipe. At the frame sizes and
rates involved this is not the bottleneck — the model is — and the alternative
was a build that nobody could reproduce.

**A sidecar can be remote.** The same protocol runs over a Unix socket or TCP,
which is what makes an expensive model shareable across sessions rather than
loaded per session.
