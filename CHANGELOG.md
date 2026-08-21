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

### Fixed before release

An audit against the plan found one recurring defect: policies that were
constructed, validated, named in the health report, and exposed as flags — and
never consulted at runtime. A measured factor that is a silent no-op cannot
measure anything, so each of these is now wired to the live path with a test
that fails when it is unwired.

- **The repair lifecycle.** Nothing created a repair obligation, so
  `RepairInstruction` never fired and audible repair never happened. Content
  the user heard and a later observation invalidated is now recorded in the
  ledger, raised into the trajectory at the next safe point, put to the slow
  provider as an instruction, and resolved against the correction.
- **Turn projection.** The floor policy was never consulted, so
  `-policy-models=turn-projection` changed nothing. A projected endpoint now
  closes the turn before silence confirms it, and only a projected one does —
  an ordinary endpoint stays the acoustic gate's.
- **Backchannel.** The policy was never consulted, so
  `-policy-models=backchannel` changed nothing. A chosen continuer is now
  spoken, off the audio path, carrying no assistant item, and does not trigger
  barge-in against itself.
- **Preparation.** `Prepare()` was called and its decision discarded. A
  continuation is now genuinely generated before the endpoint against an
  uncommitted observation, committed to nothing, and adopted only if the
  endpoint says the same thing. It is opt-in (`-preparation continuous`),
  because it is the one policy that spends tokens rather than reorders work.
- **Computer use could not press anything.** Every action in the namespace that
  changes something declares `confirm: policy`; with no policy supplied that
  reads as `always`, and `always` with no confirmer denies. The declared target
  is now the policy, `-computer-confirm always` is refused at startup rather
  than denying silently at dispatch, and a `Confirmer` can be supplied.
- **Observers were per deployment, not per session.** They are now selected in
  the session extension (`observers`, answered with `available_observers`), and
  factor F3's video-only level genuinely runs no recogniser instead of being
  audio+video under another name.
- **The declared video rate cap was not enforced**, and the video observer's
  sampling interval keyed on client-supplied capture times — so a client that
  sent no timestamps had no rate limit at all. Both now hold server-side.
- **The admission governor was never constructed.** Policy models, narration,
  and speculative preparation now compete under one budget when
  `-compute-capacity` is set.
- **Deferred routine work could ride out on a parallel batch** and bypass the
  deferral gate. A merged batch is parallel only when every part of it is.
- **Two policies that cancel out are refused rather than silently reconciled**:
  `stable-partial` observation with a deferral that waits for the endpoint held
  every partial until the endpoint and bought nothing.

Two defects in the measurement harness itself, which are the more serious kind
because they would have produced published numbers that were wrong:

- **FDB v3 scored an unreassembled identifier as correct.** Value comparison
  stripped whitespace, so an agent that heard the caller spell it out and sent
  `order_id="B O B 1 2"` matched the expected `BOB12`. Reassembling a spelled
  identifier is the distinct failure this suite exists to separate from not
  knowing which tool to call, and the scorer was reporting it as absent.
  Punctuation is still normalised away; a word boundary is now a word boundary.
- **FD-Bench computed the premature/overrun distinction and did not report
  it.** An answer begun mid-turn and an answer that ran into the next turn have
  different causes and different fixes; both are now in the metrics, with the
  overlap duration.

### What is not claimed

The measurement program in `docs/measurement.md` runs continuously after
launch, and its claims appear as each cell completes. Until a cell is complete
there is no score for it, which is a deliberate trade: the project ships before
its most interesting claims are provable. Human preference and perceived
naturalness are not measurable by any of this and need a separate study.
