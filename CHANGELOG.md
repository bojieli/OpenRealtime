# Changelog

## v1.0.0

The first release of the current architecture. The engine was rebuilt around
four subsystems over a session core — perception, cognition, action, and an
interaction control plane — and everything below is what that made possible.

### The engine

- **Four bindings, one runtime.** A cascade of recogniser, language model, and
  synthesiser; a speech-to-speech Omni model; a full-duplex interaction model;
  and a remote Realtime endpoint. The binding seam is what varies; the
  trajectory, the event loop, and the interaction policies do not.
- **A background reasoner that shares one trajectory** with the foreground
  model, reasoning and calling tools while the voice keeps talking. Two
  boundaries hold it in place: fast cognition cannot call tools, and slow
  cognition cannot speak.
- **An interaction control plane** with named, swappable policies for
  triggering, preparation, rollout, the floor, and commitment — the parts of
  feeling good in a conversation that no model in the stack knows anything
  about.
- **Duplex state derived from playout**, not from generation. Agent-speaking
  means audio reaching a person, which is the only definition barge-in can be
  built on.
- **An event loop with one invariant**: commit is unconditional, acting is
  conditional, and every deferral has a wake-up.

### The protocol

- **OpenRealtime Protocol v1**, a strict superset of OpenAI Realtime:
  three events and two object extensions, zero changes to the base surface. A
  client that never mentions it gets an ordinary Realtime session, and the
  server never volunteers the key.
- **Realtime video and computer use** over the same session, through the
  extension.
- **Wire validation on every session by default**, in production rather than
  only in tests.
- **A sidecar protocol** for model implementations that are not Go, with a
  conformance suite and reference sidecars for Qwen3-Omni, MiniCPM-o, and
  Moshi.

### Transports and integrations

- **WebRTC termination in-process** via Pion, with Opus inbound and PCMU
  outbound.
- **LiveKit** as a separate module, so a server build does not inherit its
  dependency tree.
- **A browser example** that connects to a local server with no build step.

### Safety

- **Observer authority and fenced observed content.** What a screen or a
  document says is evidence, never instruction, and the injection-authority
  test is a release gate rather than a test that happens to exist.
- **Declared targets and confirmation requirements** on the `computer.*`
  namespace, with a uniform commit boundary and an action audit.

### Measurement

- **A harness that drives a running server over the protocol**, not an
  in-process session, with fail-closed reporting: no incomplete cell is
  reportable, every cell declares its revision and executable hash, latency
  claims carry distributions, and negative results publish.
- **Five suites**: FDB v1.5, FDB v3, and FD-Bench run here; τ-Voice and
  DynaCU-Bench stay in their own environments and OpenRealtime ships a runner
  for each.
- **Dataset preparation** that verifies every archive against a pinned digest
  and every partition against a pinned population.

### Release engineering

- **One verification gate**, `scripts/check.sh`, which is what CI runs.
- **Reproducible cross-platform builds** with published checksums, verified by
  building twice and comparing rather than asserted in prose.
- **A distroless container** that runs as a non-root user and contains the
  server binary and the certificate roots and nothing else.

### What is not claimed

The measurement program in `docs/measurement.md` runs continuously after
launch, and its claims appear as each cell completes. Until a cell is complete
there is no score for it, which is a deliberate trade: the project ships before
its most interesting claims are provable. Human preference and perceived
naturalness are not measurable by any of this and need a separate study.
