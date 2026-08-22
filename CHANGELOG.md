# Changelog

## Unreleased

### Cognition

- **A question the voice can answer is answered once.** The rollout ran the
  background reasoner on every observation, so a turn needing no deliberation
  produced a second answer nobody asked for — and the step that read it back
  spoke it. Whether a turn needs deliberation is now the fast phase's
  judgement, handed on with a control marker that is stripped before any item
  is committed, so it reaches neither the trajectory nor the user.
- **Slow's result is background state, not an assistant turn.** Every item
  records whether its producer could be heard, and the projection into a
  provider discarded that, so a written result arrived as an ordinary assistant
  message — indistinguishable from what the agent had actually said. A voicing
  step told to "say the answer the reasoning continuation just produced" had no
  referent for it and recited the earlier spoken turn back instead, word for
  word, including the truncation from its own token limit. It is now projected
  as what it is, in every dialect.
- **The voicing step is gone.** Nothing is asked to recite what another
  provider wrote. The voice reads the same trajectory and answers in its own
  words, which is the only kind of spoken turn there is.
- **The fast provider is told what the agent can do and given nothing to
  execute.** It was handed the full tool definitions while its authority was to
  propose, so it spent a ninety-six token budget emitting JSON that could not
  run — and left the dead air its own instruction forbids. It gets the
  capability list and the escalation marker instead. A call it emits anyway is
  still recorded as a non-executable proposal rather than failing the turn.

### The event loop

- **Every completion re-enters the loop.** Locally dispatched tool results were
  appended straight to the trajectory, and a finished slow chain was acted on
  where it happened, so neither passed the gate that decides when the agent may
  be heard. Both now travel as events, exactly as a client-executed result
  already did. A signal event opens a safe point without appending anything,
  because what it refers to is already in the log.
- **Deliberation is never deferred for silence.** The gate holds what will be
  heard; it must not hold what will only be thought, or the background reasoner
  could not reason while the voice is talking, which is what it is for.
- **A superseded turn is a cancellation, not a failure.** Two of the three
  interrupt paths marked themselves as interruptions and the third did not, so
  a turn replaced by newer evidence was reported to the client as an error.

### Compatibility

- **A response is one thing the agent did, not the whole turn.** The
  compatibility document claimed a turn had to be a single response, on the
  grounds that a client stops reading a finished one. That is not what a client
  does — audio arrives on the audio channel and function calls are read as they
  arrive — and the claim would require holding a response open across an
  unbounded deliberation. Corrected, with the test that encoded it.

### Removed

- `AppendBatchAfter`, `AppendToolResults`, `ErrSlowMaySpeak`,
  `ErrFastMayExecute`, and the `ValidateArrangement` the last two named in
  their documentation: none had a caller, and the last three described a check
  that does not exist. The arrangement is enforced at engine construction.

### Realtime endpoints

- **`providers -role upstream -probe NAME` contacts an endpoint for real**,
  using the same dial path the binding uses, and prints every server event it
  sent back. A fake built from a vendor's documentation proves only that this
  code does what the documentation was read to say; the endpoint itself is the
  only thing that can prove the reading was right.
- **Each entry records how far it has been checked** — `live-turn`,
  `reachable`, or `documented` — and the listing prints it, so the table says
  what is evidence and what is a careful reading rather than leaving that in a
  paragraph.

- **Five remote realtime endpoints behind the background reasoner**, resolved
  through the catalogue: OpenAI, xAI, Azure OpenAI, Alibaba's
  Qwen-Omni-Realtime, and Google's Gemini Live. `-upstream-provider` selects
  one; `openrealtime providers -role upstream` lists them.
- **A rename table for endpoints on the pre-GA event names.** OpenAI renamed
  its audio events at general availability and several endpoints implement the
  earlier spelling. That is now a catalogue entry rather than an adapter.
- **The hand-off is a declared property of the endpoint.** Giving the reasoner's
  answer to the remote to say is what this binding is for, and the base
  protocol is not as portable as it looks: Qwen-Omni-Realtime reserves
  conversation items for tool results and takes no per-response instructions,
  so its answer travels in the session instruction and is taken back out
  afterwards.
- **Gemini Live is translated rather than forked.** BidiGenerateContent is not
  a Realtime dialect - no event type field, no conversation items, no session
  updates after the handshake, and 16 kHz audio in. `adapters/geminilive`
  speaks it on one side and the Realtime protocol on the other, so the binding
  keeps one runtime and one mirror. Verified against Google's live API, not
  only against a fake.
- Deepgram Voice Agent and ElevenLabs Agents are deliberately **not** here:
  they are agent platforms that run their own loop, and this binding's reasoner
  behind one would be two orchestrators on one conversation.

### Providers

- **One catalogue for every model provider.** The new `providers` package is
  the single place that knows how to reach a language model, a recogniser, or
  a synthesiser, and the server resolves all four roles through it. Thirty
  language providers, ten recognisers, and nine synthesisers; adding one is a
  table entry plus, when the wire format is genuinely its own, an adapter.
  See [providers](docs/providers.md).
- **`openrealtime providers`** lists the catalogue for any role and shows
  which credentials are present. `-probe NAME` asks a provider what it
  actually serves, because a default model compiled into this repository is
  the one part of an entry that goes stale.
- **An Anthropic adapter** over the native Messages API. It is not a dialect
  of Chat Completions, and three of its constraints are load-bearing: a turn
  must end with a user message, every tool call must be answered in the very
  next one, and thinking blocks must be replayed with their signatures.
- **Provider dialects for OpenAI-compatible endpoints.** The reasoning switch
  is spelled five different ways across vendors and the output-token limit two;
  both are now declared per provider rather than assumed. An effort level an
  endpoint has no word for is refused at construction instead of being answered
  at a neighbouring one.
- **Streaming recognition from Deepgram**, dialled at the caller's own sample
  rate so nothing is resampled before it is recognised, and **batch
  recognition** from OpenAI, Groq, ElevenLabs, Fireworks, SiliconFlow,
  Mistral, and any local whisper server through one adapter. A batch endpoint
  recognises once, at the endpoint of the utterance, and says so rather than
  pretending to stream.
- **Speech from Deepgram, ElevenLabs, and Cartesia**, sharing one adapter and
  one streaming reader with the endpoints that speak OpenAI's speech route.

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
  for each. Both runners point the published environment at a running server
  and patch nothing in it: those benchmarks speak the OpenAI Realtime protocol
  over a configurable base URL, and this is a strict superset of it, so the
  endpoint is an argument. What they measure is therefore the protocol claim
  rather than an adapter written to agree with us.
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

### What running the benchmarks unmodified found

DynaCU-Bench drives 150 real browser tasks with an unmodified official-API
client, which exercises paths no test written here had thought to. Five
compatibility defects came out of pointing it at the server, and each is now a
capability rather than a workaround:

- **A response was not a turn.** Every output kind was rendered as its own
  response, so an agent that spoke and then called a tool produced two of them
  and a client that stops reading at the first `response.done` — which the
  protocol says it may — never saw the calls. One `response.create` now
  produces one response carrying every output item, closing when the rollout
  has finished *and* every utterance it started has finished playing.
- **Text output was refused.** `output_modalities: ["text"]` failed the session
  update, so the whole class of clients the video and computer-use extension
  exists for could not open a session at all.
- **Turn detection could only be the server's.** A client sending
  `turn_detection: null` had it ignored and `input_audio_buffer.commit`
  refused, so a client driving its own turns could not work here.
- **An image attached to a turn was dropped**, and the cognition engine had no
  media resolver, so even retained media was invisible to every provider — an
  agent handed a screen acted on one it had never seen.
- **Vision was not a declared property.** A text-only fast model was handed the
  screenshots and returned "is not a multimodal model" on every step.

### What is not claimed

The measurement program in `docs/measurement.md` runs continuously after
launch, and its claims appear as each cell completes. Until a cell is complete
there is no score for it, which is a deliberate trade: the project ships before
its most interesting claims are provable. Human preference and perceived
naturalness are not measurable by any of this and need a separate study.
