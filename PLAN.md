# OpenRealtime Project Plan

- **Subtitle:** Microturn Realtime Intelligence Engine
- **Status:** Clean-slate planning specification, version 0.1
- **Date:** 2026-08-17

## 1. Executive summary

OpenRealtime is a research-first open-source project for building and evaluating realtime spoken intelligence. It asks whether the responsiveness commonly associated with native speech-to-speech interaction models can also emerge from a modular system when that system operates incrementally, plans before the user finishes speaking, and receives frequent opportunities to revise, speak, stop, and repair.

The project will build:

1. A precise model of **microturn execution**: periodic or event-driven decision opportunities over a continuously changing stream of perceptual evidence.
2. An instrumented reference engine that separates incremental perception, fast foreground cognition, slow background cognition, speech planning, and audio commitment.
3. Reproducible baselines covering conventional endpointed cascades, microturn cascades, hybrid systems, and native speech-to-speech systems.
4. Benchmarks for turn timing, interruption, backchannels, overlap, simultaneous translation, rapid audio games, tool use, and difficult questions requiring slow thought.
5. Public traces, metrics, ablations, and a research report that states both positive and negative results.

The project is successful if it produces credible evidence about the hypothesis. A result showing that modular microturn systems cannot match native systems in important dimensions is still a successful research outcome if the experiment is sound and the limitations are characterized.

## 2. Clean-slate provenance

This specification is newly authored from public research, public API documentation, and the project goals stated in this document. Implementation must be developed from this specification and public sources. Contributions must be original or carry a compatible, documented license.

Before accepting a substantial contribution, maintainers should require:

- A declaration of origin for imported code, data, prompts, and model weights.
- License and attribution metadata for every third-party asset.
- Rejection of code copied from non-public or license-incompatible systems.
- Reproducible evidence for performance claims.

## 3. Mission and scientific posture

### 3.1 Mission

Build an open experimental platform that determines how much realtime conversational performance can be obtained from **incrementality and scheduling**, independent of whether the underlying intelligence is implemented by separate ASR, language, and speech models or by a native multimodal model.

### 3.2 Central thesis

The central thesis is:

> Realtime interaction is partly a scheduling property, not solely a model category.

This thesis does **not** claim that scheduling erases all architectural differences. Text-mediated systems may lose prosody, emotion, non-speech sounds, speaker state, and other paralinguistic information. Native audio models may possess interaction capabilities that a text bottleneck cannot reproduce. OpenRealtime must measure those differences rather than assume them away.

### 3.3 Research posture

- State hypotheses before optimizing against the benchmark.
- Preserve negative and ambiguous findings.
- Distinguish measured latency from perceived responsiveness.
- Separate model quality from orchestration quality through ablations.
- Avoid claims based on a single provider, language, voice, network, or task.
- Publish raw timing events and analysis code wherever data rights permit.

## 4. Scope and non-goals

### 4.1 In scope

- Incremental audio ingestion and timestamped frame processing.
- Streaming ASR hypotheses with stable and unstable spans.
- Fixed, adaptive, and learned microturn trigger policies.
- Early response planning while user speech continues.
- Fast foreground and slow background cognitive paths.
- Speculative language generation and speech synthesis.
- Explicit commit, cancel, truncate, and repair semantics.
- Voice activity, turn projection, barge-in, overlap, and backchannels.
- Full-duplex experiments, including simultaneous translation.
- Tool-call initiation and cancellation during live conversation.
- Provider-neutral experimental adapters.
- Trace recording, deterministic replay, simulation, benchmarks, and human studies.

### 4.2 Non-goals

- Becoming a marketplace or billing aggregator for model providers.
- Hiding all provider-specific capabilities behind a lowest-common-denominator API.
- Building a telephony carrier, conferencing platform, or general media server.
- Shipping a complete persistent personal agent; that belongs to a higher-level runtime.
- Claiming universal parity with native speech models before evidence exists.
- Optimizing only time-to-first-audio while ignoring wrong starts, repairs, comprehension, or task success.

## 5. Core concepts and vocabulary

### 5.1 Microturn

A **microturn** is a bounded decision opportunity created while interaction is still unfolding. At a microturn, the engine may:

- Update its belief about what the user means.
- Keep listening without acting.
- Prepare or revise a response candidate.
- Emit a reversible backchannel.
- Commit a portion of semantic or audio output.
- Cancel uncommitted work.
- Interrupt or yield ongoing speech.
- Start, pause, or cancel a tool action.

A nominal 200 ms interval is the initial experimental condition because human conversational timing and prior incremental systems make that scale interesting. It is not a required production setting. Experiments must compare intervals such as 50, 100, 200, 400, and 800 ms, plus event-driven and adaptive policies.

### 5.2 Incremental hypothesis

An ASR or perception result is represented as:

- A stable prefix that is unlikely to change.
- An unstable suffix that may be revised.
- Acoustic and wall-clock timestamps.
- Confidence or uncertainty metadata when available.
- A monotonically increasing revision identifier.

Downstream components must never confuse a partial hypothesis with committed truth.

### 5.3 Response candidate

A response candidate is a proposed semantic action or utterance derived from the current evidence. It has:

- A source perception revision.
- A confidence and risk classification.
- A validity condition.
- A replaceable uncommitted region.
- Optional tool actions.
- A speech plan that may not yet be audible.

### 5.4 Commit horizon

The **commit horizon** separates output that may still be replaced from output already heard by the user. Text generation can be revised cheaply; synthesized but unplayed audio can be discarded; played audio can only be repaired socially through clarification or correction.

### 5.5 Fast and slow cognition

- The **fast path** handles frequent, low-cost decisions: backchannels, turn prediction, short answers, interruption, and routing.
- The **slow path** handles difficult reasoning, planning, retrieval, and tool work.
- The slow path may update future speech or request a correction, but it may not silently rewrite audio already committed.

### 5.6 Perceived responsiveness

Perceived responsiveness is not identical to first audio latency. A meaningless filler sound can arrive quickly while the system remains unhelpful. Evaluation therefore combines timing, semantic progress, appropriateness, interruption behavior, repair burden, and user judgment.

## 6. Research questions and hypotheses

### RQ1: Cadence

How does decision cadence affect response timing, semantic accuracy, false starts, computational cost, and user preference?

**H1:** A microturn cascade with incremental perception will reduce median semantic response onset and turn-gap error relative to the same models behind endpointed turn detection.

### RQ2: Early planning

Can a modular system prepare useful responses before a user reaches the end of a turn without damaging comprehension?

**H2:** Planning from stable prefixes will improve response timing on predictable turns, while uncertainty-aware deferral will be necessary on ambiguous turns.

### RQ3: Speculation and repair

What amount of speculative work is beneficial before revision and repair costs dominate?

**H3:** Speculative text generation and synthesis will improve latency only when coupled to explicit validity, cancellation, and commit policies. Ungated speculation will increase audible corrections and reduce trust.

### RQ4: Fast/slow separation

Can a small fast model maintain conversational flow while a stronger slow model improves difficult answers?

**H4:** Separating foreground coordination from background deliberation will improve the latency-quality frontier, especially for queries where an immediate acknowledgement can be followed by a considered answer.

### RQ5: Full duplex

Can modular components support interruption, backchanneling, and simultaneous listening/speaking at a level users perceive as natural?

**H5:** Full-duplex behavior depends more on continuous input processing and explicit overlap policy than on a single end-to-end model, but text-mediated systems will remain weaker on some prosodic distinctions.

### RQ6: Translation and rapid interaction

Can the same scheduling machinery support simultaneous translation and low-latency audio games?

**H6:** An adaptive trigger policy using stable semantic increments will achieve a better translation quality-latency tradeoff than fixed endpointing, while game-like tasks will benefit from smaller commit units and domain constraints.

### RQ7: Native versus modular limits

Which capabilities remain systematically better in native speech-to-speech systems?

**H7:** Native systems will initially outperform text-bottleneck cascades on emotion, non-speech vocalization, prosodic intent, and graceful overlap. The gap should be reported and used to define hybrid interfaces rather than hidden.

## 7. Reference architecture

```text
Audio input
    │
    ▼
Frame clock ────────────────┐
    │                       │
    ▼                       │
Incremental perception     │
    │ revisions            │
    ▼                       │
Microturn ledger ◄─────────┤ timing and trace bus
    │                       │
    ├── Turn/overlap policy │
    ├── Fast cognition      │
    └── Slow cognition      │
             │              │
             ▼              │
      Response candidates   │
             │              │
             ▼              │
      Commit controller     │
       │       │       │    │
       │       │       └────┤ cancel/repair
       │       ▼            │
       │   Tool actions     │
       ▼                    │
Streaming speech plan       │
       │                    │
       ▼                    │
Audio output ───────────────┘
```

### 7.1 Frame clock

- Assign monotonic timestamps at media ingress.
- Preserve capture time separately from processing time.
- Detect dropped, delayed, duplicated, and reordered frames.
- Support prerecorded deterministic replay and live devices.
- Avoid using wall-clock time for latency arithmetic.

### 7.2 Incremental perception

The first implementation targets streaming ASR, with extension points for speaker identity, acoustic events, prosody, vision, and environment state. The contract must expose revisions rather than only final transcripts.

### 7.3 Microturn scheduler

The scheduler creates decision opportunities using interchangeable policies:

1. Fixed interval.
2. Perception revision event.
3. Voice-activity transition.
4. Predicted turn-completion probability.
5. Semantic stability threshold.
6. Deadline-aware adaptive policy.
7. Learned policy, introduced only after trace data and safe offline evaluation exist.

The scheduler records why every microturn fired or was suppressed.

### 7.4 Turn and overlap manager

The manager maintains explicit state such as:

```text
LISTENING
USER_SPEAKING
SYSTEM_PREPARING
SYSTEM_SPEAKING
OVERLAP_USER_INTERRUPTION
OVERLAP_BACKCHANNEL
REPAIRING
```

Transitions must use acoustic evidence, semantic evidence, current system commitments, and policy. Side speech and listener backchannels must not automatically be interpreted as commands to stop.

### 7.5 Fast cognition

Responsibilities:

- Decide whether to listen, acknowledge, answer, defer, or yield.
- Generate short, low-risk response candidates.
- Classify interruption and backchannel evidence.
- Route difficult work to the slow path.
- Produce structured decisions under a strict deadline.

Fast cognition is replaceable. Rules and small classifiers should be benchmarked alongside language models.

### 7.6 Slow cognition

Responsibilities:

- Produce higher-quality answers and plans.
- Use retrieval and tools.
- Re-evaluate assumptions as the utterance evolves.
- Return structured updates with a validity scope.
- Declare when the fast path should avoid premature content.

The slow path is asynchronous and must be cancellable. Stale results are rejected by revision or goal identity rather than allowed to overwrite newer state.

### 7.7 Response and speech planning

The speech planner turns semantic candidates into incrementally synthesizable spans. It should support:

- Clause-level and phrase-level chunking.
- Optional neutral backchannels separated from substantive claims.
- Prosody and speaking-style controls when supported.
- Pre-synthesis of likely continuations.
- Audio chunk identifiers linked to source text and candidate revision.
- Truncation boundaries and repair annotations.

### 7.8 Commit controller

The controller is the safety boundary between speculation and user-visible output. Its policy considers:

- Perception stability.
- Semantic risk and reversibility.
- Whether the output is a backchannel or factual claim.
- Tool side effects.
- Time until playback.
- Current interruption evidence.

Irreversible tool calls require a separate authority policy and are never justified merely by low latency.

### 7.9 Trace and replay system

Every experiment emits an append-only trace with:

- Monotonic timestamps.
- Audio frame ranges, stored or hashed according to consent.
- Perception revisions.
- Microturn triggers and suppressions.
- Model request and response spans.
- Candidate creation, replacement, commitment, playback, and cancellation.
- Tool lifecycle.
- Network and queue delays.
- Resource use and estimated cost.

Replay can replace models with recorded outputs, inject network jitter, and evaluate alternative policies against the same input.

## 8. Protocol and extension boundaries

The project requires a small semantic event protocol for reproducible experiments. It is not a universal model gateway.

Initial event families:

```text
media.input.frame
media.output.played
perception.revision
perception.finalized
microturn.opened
microturn.decision
turn.state.changed
candidate.created
candidate.superseded
speech.prepared
speech.committed
speech.cancelled
speech.repair.requested
tool.requested
tool.started
tool.completed
tool.cancelled
metric.sampled
session.ended
```

All events contain a schema version, session ID, monotonic timestamp, causal parent IDs, and provider-neutral payload. Provider-specific data may be retained in a namespaced extension field.

### 8.1 Component interfaces

- `PerceptionProvider`: audio frames to perception revisions.
- `FastDecisionProvider`: current state to bounded structured decision.
- `DeliberationProvider`: goal snapshot to asynchronous result stream.
- `SpeechProvider`: speech spans to timestamped audio chunks.
- `TurnPolicy`: evidence to turn/overlap state transition.
- `CommitPolicy`: candidate and evidence to prepare/commit/cancel decision.
- `ToolProvider`: typed calls with authority and cancellation semantics.

Adapters must declare capabilities rather than silently ignore unsupported operations.

## 9. Experimental program

### 9.1 Baseline conditions

Each major experiment should compare as many of these conditions as applicable:

- **B0: Endpointed cascade.** Wait for end-of-turn, then ASR finalization, language generation, and speech.
- **B1: Streaming without microturn policy.** Stream components but retain conventional response initiation.
- **M1: Fixed-cadence microturn cascade.** Same component models as B0.
- **M2: Adaptive microturn cascade.** Trigger from stability and turn prediction.
- **M3: Fast/slow microturn cascade.** Separate foreground coordination and deliberation.
- **N1: Native realtime speech model.** Use public API behavior as available.
- **H1: Hybrid.** Native or acoustic model for interaction signals with modular higher-level reasoning.

Comparisons must control component model versions, region, network path, audio device, prompt, voice, and workload where possible.

### 9.2 Ablations

- Trigger interval.
- Stable-prefix gating.
- Turn projection model.
- Speculative text generation.
- Speculative TTS.
- Commit horizon size.
- Backchannel policy.
- Slow-path availability.
- Cancellation support.
- Acoustic/prosodic side channel.
- Network latency and jitter.
- Small versus strong fast-decision model.

### 9.3 Workload families

#### A. Turn timing and backchannels

Controlled utterances with pauses, continuations, hesitations, and clear turn endings. Measure premature takeover, excessive silence, appropriate acknowledgement, and response onset.

#### B. Interruption and overlap

User interruption, listener backchannel, side conversation, and ambient speech, aligned with the public Full-Duplex-Bench taxonomy. Measure stop latency, false interruption, recovery, and semantic continuity.

#### C. Incremental dictation and correction

Numbers, identifiers, addresses, and constrained commands where rapid feedback is valuable and revisions are objectively scorable.

#### D. Rapid audio games

Word association, quizzes, cooperative timing games, and reaction tasks. These expose jitter, rule adherence, premature speech, and synchronization failures.

#### E. Simultaneous translation

Streaming speech translation where quality and latency are both measured. Begin with public, redistributable corpora and adapt SimulEval metrics.

#### F. Difficult questions

Questions that need tools or deliberation. Compare silence, fillers, immediate partial answers, explicit deferral, and fast acknowledgement followed by slow completion.

#### G. Paralinguistic challenge set

Sarcasm, uncertainty, laughter, sighs, emotional prosody, and non-speech events. This set is expected to expose limitations of text bottlenecks.

## 10. Metrics

### 10.1 Timing

- Capture-to-perception revision latency.
- Stable-prefix latency.
- End-of-user-speech to first audible output.
- First **semantic** audio latency, excluding non-substantive filler.
- Predicted versus actual turn-end error.
- User interruption to audible system stop.
- Tool request and tool completion latency.
- P50, P90, P95, P99, distribution shape, and deadline miss rate.

### 10.2 Interaction quality

- Premature takeover rate.
- Missed-turn rate.
- Appropriate backchannel precision and recall.
- Interruption classification accuracy.
- False stop and failure-to-stop rates.
- Audible false-start duration.
- Repair count and repair duration.
- Repetition and contradiction rate.
- Conversation task success.

### 10.3 Content quality

- ASR word and semantic error rates.
- Factual and reasoning task scores.
- Tool correctness.
- Translation quality paired with latency metrics.
- Human ratings of relevance, naturalness, prosody, trust, and cognitive burden.

### 10.4 Efficiency

- Audio, text, and reasoning tokens where exposed.
- Compute time, CPU/GPU utilization, memory, and network bandwidth.
- Discarded speculative work.
- Cost per successful interaction and cost per conversation minute.

### 10.5 Composite reporting

Do not collapse all dimensions into a single leaderboard score by default. Publish Pareto frontiers for latency versus content quality, interaction quality, and cost. A composite score may be added only with transparent weights and sensitivity analysis.

## 11. Statistical and human-study methodology

- Register hypotheses, primary metrics, exclusion rules, and analysis scripts before the final benchmark run.
- Use paired trials when systems receive identical prerecorded input.
- Randomize system ordering in human studies.
- Blind participants to system identity where practical.
- Conduct power analysis rather than selecting sample counts by convenience.
- Report confidence intervals and effect sizes, not only significance tests.
- Separate exploratory from confirmatory results.
- Preserve per-language results instead of averaging away cultural timing differences.
- Obtain informed consent for voice recording and publish only data with appropriate rights.

## 12. Proposed repository structure

```text
OpenRealtime/
├── README.md
├── PLAN.md
├── LICENSE
├── CONTRIBUTING.md
├── CODE_OF_CONDUCT.md
├── SECURITY.md
├── docs/
│   ├── architecture.md
│   ├── protocol.md
│   ├── experiments.md
│   ├── metrics.md
│   └── adr/
├── schemas/
├── engine/
├── adapters/
│   ├── perception/
│   ├── cognition/
│   └── speech/
├── benchmarks/
├── replay/
├── examples/
├── analysis/
└── tests/
```

The first implementation-language decision is an architectural decision record. A reasonable starting strategy is a high-level research harness for rapid experimentation and a separately profiled media/timing core. A lower-level rewrite is justified only by measured scheduling jitter or resource limits.

## 13. Engineering requirements

### 13.1 Determinism and time

- Inject clock and randomness dependencies.
- Use monotonic timestamps.
- Make offline replay deterministic within documented provider limits.
- Version prompts, model identifiers, schemas, and configuration.

### 13.2 Backpressure and cancellation

- Every stream has bounded queues and an overflow policy.
- Every asynchronous operation is cancellable or explicitly marked non-cancellable.
- Stale outputs cannot mutate current session state.
- Shutdown drains or records abandoned work deterministically.

### 13.3 Observability

- Structured logs are derived from the trace rather than maintained separately where possible.
- Spans preserve causal links across media, model, tool, and playback stages.
- Debug visualization displays audio, transcript revisions, candidates, commits, and playback on one timeline.

### 13.4 Testing

- Unit tests for state machines and policies.
- Property tests for ordering, revision, and cancellation invariants.
- Golden traces for deterministic replay.
- Fault injection for packet loss, provider timeout, malformed events, and clock skew.
- End-to-end audio tests with controlled loopback.
- Performance regression tests with explicit hardware metadata.

## 14. Milestones and exit criteria

### M0 — Research specification and reproducibility scaffold

Deliverables:

- Ratified glossary, hypotheses, trace schema, and baseline protocol.
- Literature review and benchmark inventory.
- Minimal audio replay harness.
- License and governance decision.

Exit criteria:

- A contributor can reproduce a timing trace from a public audio fixture.
- Primary hypotheses and metrics are reviewable before engine optimization.

### M1 — Endpointed reference baseline

Deliverables:

- One streaming perception adapter, one language adapter, and one speech adapter.
- Conventional endpointed pipeline.
- End-to-end trace visualization.

Exit criteria:

- Repeated prerecorded trials produce explainable timing distributions.
- Component and queue latency sum to the observed end-to-end latency within tolerance.

### M2 — Microturn engine

Deliverables:

- Fixed and event-driven scheduler.
- Revision-aware state and response candidates.
- Text-level prepare, supersede, and cancel.
- Initial cadence ablation.

Exit criteria:

- The same component models can run in endpointed and microturn conditions.
- Every latency improvement can be attributed to a measured stage.

### M3 — Incremental speech commitment and duplex behavior

Deliverables:

- Streaming synthesis, commit horizon, truncation, and repair.
- Continuous input during system speech.
- Interruption and backchannel policies.
- Full-Duplex-Bench-compatible adapter where licensing permits.

Exit criteria:

- Stop latency, false stop, and repair metrics are automatically reproducible.
- No played audio is silently removed from the semantic history.

### M4 — Fast/slow cognition

Deliverables:

- Asynchronous deliberation contract.
- Stale-result rejection and slow-to-fast updates.
- Difficult-question workload.
- Cost and quality comparison.

Exit criteria:

- Background reasoning can finish, fail, or be cancelled without blocking realtime interaction.
- Users receive truthful state rather than fabricated progress.

### M5 — Translation and rapid-interaction demonstrations

Status: complete.

Deliverables:

- Simultaneous translation policy and evaluation adapter.
- At least one rapid audio game.
- Public demo scripts and prerecorded fixtures.

Exit criteria:

- Each demonstration reports latency, task quality, failures, and compute cost.
- Results are repeatable without a proprietary client.

### M6 — Comparative study and research release

Status: complete for the open reference condition. Native-provider and human
participant comparisons are prospectively specified and explicitly not run
because neither provider access nor participant-study authority was in scope.

Deliverables:

- Native, endpointed, microturn, and hybrid comparisons where access permits.
- Predefined ablations and human study.
- Technical report or paper draft.
- Versioned benchmark release.

Exit criteria:

- Claims are supported by published traces and analysis.
- Known counterexamples and limitations are prominent.
- A third party can reproduce at least the open-model/reference condition.

### M7 — Stable research engine

Status: complete.

Deliverables:

- Versioned component API.
- Compatibility and conformance suite.
- Contributor documentation and maintained examples.

Exit criteria:

- Experimental changes no longer routinely break stable adapters.
- Downstream applications can depend on a documented release while research continues behind experimental flags.

## 15. Initial issue backlog

1. Write the glossary and event causality rules.
2. Select redistributable audio fixtures and document consent/licensing.
3. Implement a monotonic virtual clock and deterministic frame player.
4. Define trace JSON Schema and validation tests.
5. Build the endpointed baseline before any optimization.
6. Add a timeline viewer for transcript revisions and audio playback.
7. Implement fixed-cadence microturn scheduling.
8. Add stable-prefix gating.
9. Define response candidate and commit state machines.
10. Add cancellation and stale-result property tests.
11. Implement first streaming synthesis adapter.
12. Measure scheduling jitter under load.
13. Add turn projection baseline using acoustic VAD only.
14. Add a linguistic turn-prediction baseline.
15. Port the public overlap benchmark taxonomy.
16. Implement interruption stop-latency measurement.
17. Define fast/slow deliberation contract.
18. Add simultaneous translation fixture and SimulEval adapter.
19. Prepare human-study protocol and ethics checklist.
20. Publish M0/M1 reproducibility instructions.

## 16. Risks and mitigations

### Benchmark gaming

- **Risk:** Policies overfit fixed utterances or metric quirks.
- **Mitigation:** Hidden test subsets, multiple task families, prospective hypotheses, and human validation.

### Meaningless low-latency output

- **Risk:** The engine optimizes fillers rather than useful response.
- **Mitigation:** Measure first semantic audio separately and penalize inappropriate backchannels.

### Cascading revision failures

- **Risk:** ASR changes invalidate generated text and prepared audio.
- **Mitigation:** Stable/unstable spans, causal revision IDs, bounded commit horizon, and repair metrics.

### Cost explosion

- **Risk:** Frequent triggers duplicate inference.
- **Mitigation:** Prefix caching, debouncing, state reuse, explicit budgets, and discarded-work metrics.

### Native-model feature mismatch

- **Risk:** Provider APIs expose different control and timing events.
- **Mitigation:** Capability declarations, careful caveats, and comparisons limited to observable behavior.

### Text bottleneck

- **Risk:** Prosody and emotion are irretrievably lost.
- **Mitigation:** Measure the gap, add acoustic side channels, and support hybrid/native components.

### Privacy

- **Risk:** Voice traces contain identity and sensitive content.
- **Mitigation:** Consent, local-first fixtures, minimization, encryption, retention controls, and redaction tooling.

### Research/production tension

- **Risk:** Stable API pressure prevents experiments, or experiments destabilize users.
- **Mitigation:** Separate stable and experimental namespaces and define release maturity clearly.

## 17. Governance and release policy

- Use public design discussions and architectural decision records.
- Require benchmark evidence for performance-related pull requests.
- Record model/provider versions and deprecation behavior.
- Publish a code of conduct, security policy, and contribution origin policy before accepting contributions.
- Prefer a permissive, patent-aware code license; make dataset and documentation licenses explicit and separate.
- Label releases `experimental`, `research`, or `stable` according to documented criteria.
- Never describe experimental measurements as production guarantees.

## 18. Definition of project success

At the end of the initial research program, a reader should be able to answer:

1. Which parts of realtime conversational quality improved because of cadence and incrementality?
2. Which improvements required speculation, early planning, or fast/slow separation?
3. What were the costs in wrong starts, repairs, compute, and complexity?
4. Which behaviors remained better in native speech systems?
5. Which results generalized across languages, tasks, providers, and networks?
6. How can another researcher reproduce or challenge the findings?

If these questions are answered with evidence, OpenRealtime has achieved its mission regardless of whether the original thesis is fully confirmed.

## 19. Public references

The implementation and study should begin from these public sources and add a maintained literature review:

1. Stivers et al., “Universals and cultural variation in turn-taking in conversation,” *PNAS*, 2009. <https://doi.org/10.1073/pnas.0903616106>
2. Schlangen and Skantze, “A General, Abstract Model of Incremental Dialogue Processing,” *Dialogue & Discourse*, 2011. <https://doi.org/10.5087/dad.2011.105>
3. Skantze and Hjalmarsson, “Towards incremental speech generation in conversational systems,” *Computer Speech & Language*, 2013. <https://doi.org/10.1016/j.csl.2012.05.004>
4. Ekstedt and Skantze, “TurnGPT: a Transformer-based Language Model for Predicting Turn-taking in Spoken Dialog,” EMNLP Findings, 2020. <https://aclanthology.org/2020.findings-emnlp.268/>
5. Bögels, “Neural correlates of turn-taking in the wild: Response planning starts early in free interviews,” *Cognition*, 2020. <https://doi.org/10.1016/j.cognition.2020.104347>
6. Défossez et al., “Moshi: a speech-text foundation model for real-time dialogue,” 2024. <https://arxiv.org/abs/2410.00037>
7. Lin et al., “Full-Duplex-Bench: A Benchmark to Evaluate Full-duplex Spoken Dialogue Models on Turn-taking Capabilities,” 2025. <https://arxiv.org/abs/2503.04721>
8. Lin et al., “Full-Duplex-Bench v1.5: Evaluating Overlap Handling for Full-Duplex Speech Models,” 2025. <https://arxiv.org/abs/2507.23159>
9. Ma et al., “SimulEval: An Evaluation Toolkit for Simultaneous Translation,” EMNLP 2020. <https://aclanthology.org/2020.emnlp-demos.19/>
10. OpenAI, Realtime API documentation. <https://developers.openai.com/api/docs/guides/realtime>

Public APIs and papers evolve. Any experiment must cite the exact version and retrieval date used.
