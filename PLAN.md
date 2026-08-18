# OpenRealtime Project Plan

- **Subtitle:** Microturn Realtime Intelligence Engine
- **Status:** Living research specification, version 0.3
- **Date:** 2026-08-18

## 1. Executive summary

OpenRealtime is a research-first open-source project for building and evaluating realtime spoken intelligence. It asks whether a modular system can obtain both qualities that are usually traded against one another:

1. **High responsiveness**, by running ASR, language generation, and speech synthesis incrementally and opening decision opportunities while interaction is still unfolding.
2. **High intelligence**, by letting a low-latency model speak first and a higher-reasoning model continue the same canonical trajectory rather than starting a second, loosely coupled agent.

The central experiment is whether these two mechanisms can approach the responsiveness of native realtime and interaction models without giving up the tool use, long-context reasoning, observability, and replaceability of a modular system.

The project will build:

1. A precise model of **microturn execution**: periodic and event-driven safe opportunities to advance perception, cognition, tools, speech planning, and commitment.
2. A **canonical trajectory** containing ordered observations, reasoning segments, assistant content, tool calls, and tool results.
3. **Heterogeneous interleaved thinking**: fast and slow model invocations append successive segments to that one trajectory, with no separate-agent advice protocol.
4. A **single-owner safe-point event loop** that serializes asynchronous observations, model output, playback transitions, and tool results with versioned atomic commits.
5. A live, co-located ASR–LLM–TTS path whose queueing, GPU contention, first-token, first-audio, cancellation, and repair behavior can be measured.
6. Reproducible baselines covering endpointed cascades, microturn cascades, independent fast/slow systems, interleaved fast/slow systems, and native speech-to-speech systems.
7. Benchmarks for turn timing, interruption, backchannels, overlap, simultaneous translation, rapid audio games, tool use, and difficult questions requiring continued thought.
8. Public traces, metrics, ablations, and a research report that states both positive and negative results.

The project is successful if it produces credible evidence about the hypothesis. A result showing that modular microturn systems cannot match native systems in important dimensions is still a successful research outcome if the experiment is sound and the limitations are characterized.

The focused internal design is documented in
[docs/canonical-trajectory.md](docs/canonical-trajectory.md), with the
concurrency contract in
[docs/safe-point-event-loop.md](docs/safe-point-event-loop.md). The first live
implementation and its exploratory positive and negative evidence are in
[docs/live-cascade.md](docs/live-cascade.md).

## 2. Clean-slate provenance

This specification is newly authored from public research, public API documentation, and the project goals stated in this document. Implementation must be developed from this specification and public sources. Contributions must be original or carry a compatible, documented license.

Before accepting a substantial contribution, maintainers should require:

- A declaration of origin for imported code, data, prompts, and model weights.
- License and attribution metadata for every third-party asset.
- Rejection of code copied from non-public or license-incompatible systems.
- Reproducible evidence for performance claims.

## 3. Mission and scientific posture

### 3.1 Mission

Build an open experimental platform that determines how much realtime conversational performance can be obtained from **incrementality and scheduling**, and how much intelligence can be retained through **continuous, interleaved thinking over a shared trajectory**. The two questions are related but must be measured independently: trigger timing governs when work can begin, while trajectory-preserving model continuation governs how much reasoning and tool competence remains available after an immediate response.

### 3.2 Central thesis

The central thesis has two parts:

> Realtime interaction is partly a scheduling property, not solely a model category.

> Fast and slow computation can remain one coherent agent when both append to the same canonical trajectory instead of exchanging summaries as separate minds.

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
- Fast and slow model continuations over one canonical trajectory.
- Continuous thinking across assistant speech, asynchronous observations, and tool calls.
- Model-family and reasoning-budget substitution at safe trajectory boundaries.
- Co-located local ASR, LLM, and TTS inference with explicit GPU scheduling.
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

A nominal 200 ms interval is the initial experimental condition because human conversational timing and interaction-model systems make that scale interesting. It is not a required production setting. Experiments must compare intervals such as 50, 100, 200, 400, and 800 ms, plus event-driven and adaptive policies.

A microturn is **not** an instruction to restart ASR, LLM, and TTS from scratch every 200 ms. Audio ingestion is continuous; ASR maintains incremental state; language-model sessions should reuse a stable prefix or persistent sequence; and TTS consumes only new speakable spans. A tick merely creates a safe opportunity to inspect new evidence and decide whether any stage should advance.

### 5.2 Trigger hierarchy and safe points

The engine distinguishes four clocks that must not be conflated:

1. **Media frames** preserve capture timing, commonly in 10–20 ms units.
2. **Perception updates** are emitted when a streaming ASR or acoustic model has a new revision; their cadence is provider-specific.
3. **Microturn opportunities** are fixed, revision-triggered, endpoint-triggered, or adaptive scheduling events.
4. **Semantic events**—a tool result, user interruption, slow continuation, or speech invalidation—wake the event loop immediately rather than waiting for the next periodic tick.

Model and tool work is consumed at safe trajectory boundaries. A routine event may queue until the current reasoning segment or tool call reaches a boundary. An urgent interruption may cancel current decoding, retain the completed reasoning prefix, append the new observation, and continue from the extended trajectory. Independent work may execute concurrently, but its outputs rejoin the same ordered trajectory with explicit causality.

The event loop is the sole semantic transition owner. Each invocation captures
trajectory version `v`; its phase instruction and completed output append
atomically only if the store is still at `v`. Concurrent producers enqueue
events rather than mutating state. Event occurrence time is distinct from
canonical commit time, so waiting for a safe point is measurable rather than
hidden. Urgency is trusted typed metadata from acoustic/semantic policy, never
keyword matching over transcript content.

### 5.3 Incremental hypothesis

An ASR or perception result is represented as:

- A stable prefix that is unlikely to change.
- An unstable suffix that may be revised.
- Acoustic and wall-clock timestamps.
- Confidence or uncertainty metadata when available.
- A monotonically increasing revision identifier.

Downstream components must never confuse a partial hypothesis with committed truth.

### 5.4 Response candidate

A response candidate is a proposed semantic action or utterance derived from the current evidence. It has:

- A source perception revision.
- A confidence and risk classification.
- A validity condition.
- A replaceable uncommitted region.
- Optional tool actions.
- A speech plan that may not yet be audible.

### 5.5 Commit horizon

The **commit horizon** separates output that may still be replaced from output already heard by the user. Text generation can be revised cheaply; synthesized but unplayed audio can be discarded; played audio can only be repaired socially through clarification or correction.

### 5.6 Canonical trajectory

The **canonical trajectory** is the single ordered working memory of the agent. It contains typed items for:

- System and phase instructions.
- User and environmental observations, including partial recognition revisions.
- Assistant reasoning segments when the provider exposes a reusable representation.
- Assistant content, including whether and when it became audible.
- Non-executable fast tool proposals as structured working state.
- Tool calls, tool progress, tool results, and authorization decisions.
- Cancellation, supersession, and explicit repair events.

Every model invocation consumes a causally valid prefix of this trajectory. Its completed output is appended before a later invocation may depend on it. Provider adapters may render trajectory items differently, but they may not silently discard a spoken commitment, fabricate a completed tool result, or present two incompatible histories to the fast and slow models.

### 5.7 Heterogeneous interleaved thinking

Fast and slow are phases of one rollout, not separate agents:

- The **fast continuation** uses a strict latency budget and may be a local instruct model with thinking disabled or a hosted model with minimal thinking. It may emit ordinary assistant content, a non-executable structured tool proposal, or remain silent.
- The **slow continuation** inherits the exact trajectory prefix—including fast reasoning when portable, fast assistant content, and subsequent observations—and continues with a higher reasoning budget. It may reason, call tools, consume results, and emit further assistant content.
- Tool capability awareness and schemas are shared. Execution authority differs:
  a fast native call becomes `tool_proposal` working state and cannot execute;
  only a new slow `tool_call` reaches the authority/tool runtime.
- Slow output is not an advisory side channel. It is the next reasoning, tool-call, or assistant segment in the same trajectory.
- Both phases receive one common agent/domain system policy. Phase instructions
  alter compute/authority behavior but cannot create different agent identities
  or capability descriptions.

The default research policy schedules one slow continuation after every completed fast phase that was invoked for newly appended trajectory input, including a stable partial recognition before endpointing. New events may extend or supersede that work, but the runtime keeps at most one current slow continuation for the same trajectory branch. This removes a hand-written difficulty router from the reference condition and permits thinking while listening. Conditional continuation may be studied later as a learned cost/quality policy.

Different models cannot share latent state or KV cache. Same-family minimal-to-high-thinking continuation may reuse provider-native reasoning representations when supported. Cross-family continuation is symbolic: reasoning, content, calls, and results are translated through the canonical trajectory without pretending that one provider produced another provider's signed or private thinking block.

### 5.8 Perceived responsiveness

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

### RQ4: Interleaved fast/slow continuation

Can a low-latency model respond immediately while a stronger model continues the same reasoning trajectory, uses tools, and improves the eventual answer without behaving like a second agent?

**H4:** A fast continuation followed by a slow continuation over the same canonical trajectory will improve the latency-quality frontier relative to a single blocking model, while producing fewer contradictions, repeated work, and false capability denials than two independently prompted fast/slow models.

### RQ5: Full duplex

Can modular components support interruption, backchanneling, and simultaneous listening/speaking at a level users perceive as natural?

**H5:** Full-duplex behavior depends more on continuous input processing and explicit overlap policy than on a single end-to-end model, but text-mediated systems will remain weaker on some prosodic distinctions.

### RQ6: Translation and rapid interaction

Can the same scheduling machinery support simultaneous translation and low-latency audio games?

**H6:** An adaptive trigger policy using stable semantic increments will achieve a better translation quality-latency tradeoff than fixed endpointing, while game-like tasks will benefit from smaller commit units and domain constraints.

### RQ7: Native versus modular limits

Which capabilities remain systematically better in native speech-to-speech systems?

**H7:** Native systems will initially outperform text-bottleneck cascades on emotion, non-speech vocalization, prosodic intent, and graceful overlap. The gap should be reported and used to define hybrid interfaces rather than hidden.

### RQ8: Co-located modular inference

How much latency is removed—and how much queueing is introduced—when streaming ASR, a fast language model, and streaming TTS share one local accelerator?

**H8:** Persistent sessions and priority-aware co-location will reduce network and handoff delay relative to hosted or process-isolated cascades, but naive co-location will suffer GPU contention. The claim succeeds only if first semantic audio, tail latency, task quality, and discarded compute improve together.

### RQ9: Reasoning continuity across model boundaries

When does carrying earlier reasoning improve a later continuation, and when does a foreign reasoning format harm it?

**H9:** Same-family minimal-to-high-thinking continuation will preserve reasoning most faithfully. Cross-family continuation will outperform content-only handoff when it carries a model-neutral working trace, but raw foreign thinking syntax may be neutral or harmful and must be tested rather than assumed compatible.

## 7. Reference architecture

```text
Audio input ─→ frame clock ─→ incremental ASR/acoustic perception
                                      │ revisions and events
                                      ▼
                         microturn + asynchronous event loop
                                      │ safe opportunities
                                      ▼
                ┌────────── canonical trajectory ──────────┐
                │ observations                             │
                │ reasoning segments                       │
                │ assistant content and audible commits    │
                │ tool calls, progress, and results        │
                └───────────────┬───────────────────────────┘
                                │ shared prefix
                   ┌────────────┴────────────┐
                   │                         │
           fast continuation         slow continuation
        low latency/minimal thought  medium/high thought
           content or silence          reasoning/tools/content
                   │                         │
                   └──────── append in order ┘
                                │
                 assistant content → streaming TTS
                                │
                     commit/cancel/repair controller
                                │
                         audible audio output
                                │ playback/interrupt events
                                └──────────→ event loop
```

### 7.1 Frame clock

- Assign monotonic timestamps at media ingress.
- Preserve capture time separately from processing time.
- Detect dropped, delayed, duplicated, and reordered frames.
- Support prerecorded deterministic replay and live devices.
- Avoid using wall-clock time for latency arithmetic.

### 7.2 Incremental perception

The first live implementation uses stateful Qwen3-ASR 0.6B and exposes
revisions rather than only a final transcript, with extension points for
SenseVoice and other public ASR models, speaker identity, acoustic events,
prosody, vision, and environment state.

An adapter must declare whether it is causal streaming, chunked streaming, or repeated inference over an increasing window. Increasing-prefix simulation is a valid experimental condition but cannot be reported as true streaming. Qwen3-ASR is recorded as a stateful chunked-streaming service in the first run. Future SenseVoice or other deployments must make the same declaration and cannot label repeated full-prefix inference as causal streaming.

ASR is not invoked as a stateless batch job on every microturn. It owns a persistent stream and publishes a revision only when its evidence changes. Voice activity and semantic endpointing are evidence sources, not mandatory gates that downstream cognition must always wait for.

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

Fixed ticks and event triggers share one interface. A tick with no new usable evidence may be recorded and suppressed without invoking a language model. A perception revision, tool result, user interruption, slow-model completion, or output invalidation may open an immediate opportunity between fixed ticks. Experiments must report both opportunity count and actual provider invocation count.

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

### 7.5 Canonical trajectory store

The trajectory store is the cognitive source of truth. It appends typed items atomically, preserves causal parents and monotonic time, and can compile a valid provider-specific context from any prefix. The store distinguishes private reasoning from user-visible content and records when content crosses the audio commit horizon.

The store, rather than either model, owns item identity, revision provenance, invocation identity, tool lifecycle, and cancellation facts. Models do not generate workflow status fields. A fast or slow invocation produces ordinary reasoning and assistant content. A native fast call is appended as a non-executable `tool_proposal`; a native slow call is appended as an executable `tool_call`. The runtime supplies provenance and rejects a result that references a proposal.

The store exposes versioned compare-and-append. An external event batch or
model continuation derived from prefix `v` either appends completely while
`v` remains current or appends nothing. Phase instructions are provider-visible
during generation but enter the audit trajectory in the same transaction as
their output, so an in-flight invocation cannot leave a misleading standalone
instruction. Assistant playback state is append-only: cancelled-before-playback
content and native state that embeds it are excluded from subsequent provider
views, while played content remains immutable.

### 7.6 Fast continuation

The fast continuation receives the latest valid trajectory prefix, the common
agent/domain instruction, a shared capability manifest, the real tool schemas,
and a strict deadline. It produces the smallest truthful ordinary assistant
segment that makes progress, an optional structured proposal, or no content.
Asking a question is ordinary assistant text; keeping quiet is an empty visible
segment. Turn yielding and playback stopping remain media-policy decisions.

Two primary fast conditions are planned:

- A local Qwen instruct model with thinking disabled or tightly bounded, optimized for reflexive FD-Bench-style interaction.
- Gemini 3.5 Flash with minimal thinking, accepting higher network latency in exchange for stronger semantic judgment.

Both must receive the same description and schemas for what the overall agent can do. The fast model has proposal-only authority. Its call-shaped output is useful working state for slow, but the trajectory type and runner make it impossible to execute or attach a result to it. This structural separation avoids false capability denial without trusting prompt compliance as an action boundary.

### 7.7 Slow continuation and tools

After every canonical fast phase invoked for newly appended trajectory input, a continuation instruction asks the slow model to continue the latest request. This may begin from a stable partial recognition before endpointing. Gemini 3.5 Flash with medium or high thinking is the initial condition. The invocation receives the same agent/domain policy and trajectory plus the fast reasoning representation when compatible, the exact assistant content and proposals already emitted, a larger context view, and execute authority for the same tool definitions. It appends only missing work, an action, or an explicit correction rather than repeating an adequate fast segment.

The slow model continues normally: reasoning may lead to assistant content, a newly generated authoritative tool call, a tool result, more reasoning, and further content. A fast proposal is evidence about prior working intent, not an already-pending call. Slow outputs append directly to the canonical trajectory; they are not summarized into a separate `SlowUpdate` advice channel. An authority policy remains responsible for approving irreversible external effects.

The slow runner is asynchronous and cancellable. A new urgent event forces a safe point; completed reasoning is retained, the observation is appended, and continuation resumes from the extended prefix. A late result is never rewritten into an earlier position. Its provenance remains visible so a later model can accept, qualify, or repair it.

Before an endpoint, every changed ASR revision may prepare one private
fast-to-slow chain. The reference manager keeps at most one chain active and
coalesces revisions while cancellation reaches a provider safe point. Fast
output is appended to a private trajectory and slow consumes that exact prefix;
the private branch has neither a tool runtime nor a speech sink. A root is
accepted only if a SHA-256 fingerprint of every provider-visible semantic input
matches the final request, and each captured stage is replayed only if its own
canonical request fingerprint also matches. Operational IDs/timestamps are
excluded because the model cannot see them. This is cache validation, not
semantic routing: there is no fuzzy transcript normalization, keyword rule, or
difficulty classifier. A slow call becomes executable only when its exact
captured event is replayed into the canonical runner after final evidence.

### 7.8 Event loop and continuation boundaries

The event loop is one actor over a unified ingress queue. Audio/perception,
playback, client, and tool workers are concurrent event producers; they never
append semantic items from callback goroutines. It snapshots pending events in
arrival order, compiles their causal metadata, and atomically commits the whole
batch before invoking cognition. Events arriving during processing remain for
the next safe point.

The reference cognitive transition table is deliberately small:

| Committed batch contains | Continuation transition |
| --- | --- |
| Response-eligible observation | fast, then one slow continuation |
| Complete authoritative tool-result batch | one slow continuation |
| Playback-state transitions only | none |

If observations and results share a batch, fast runs once for the new evidence
and slow consumes the combined prefix. There is no `answer`, `ask`, `yield`,
`stop`, `present_slow`, difficulty, or keyword router. Eligibility of an ASR
revision and interruption priority are typed decisions made by declared
perception/turn policies, not inferred by the event loop from text.

Routine observations wait for the current provider boundary. A trusted urgent
event requests cooperative cancellation; completed partial reasoning or
assistant content may commit, incomplete calls are suppressed, and the event
commits next. Compare-and-append remains mandatory because a provider may
ignore cancellation. Source occurrence and canonical commit timestamps are
both retained.

Tool initiation and completion are split. Only a committed slow call batch is
dispatched. Results may finish concurrently, but every outstanding call from
that invocation must return in one exact identity-checked event; the batch is
ordered by original call order and committed atomically. The event then resumes
slow directly without rerunning fast.

Model substitution occurs only at trajectory boundaries. A runtime cannot move a KV cache from Qwen to Gemini; it can carry the completed symbolic prefix. If a provider exposes only signed thinking blocks or summaries, the adapter must preserve what is legally and technically reusable and declare what was omitted or normalized.

The normative runtime details are in
[docs/safe-point-event-loop.md](docs/safe-point-event-loop.md) and ADR-0004.

### 7.9 Response and speech planning

The speech planner turns semantic candidates into incrementally synthesizable spans. It should support:

- Clause-level and phrase-level chunking.
- Optional neutral backchannels separated from substantive claims.
- Prosody and speaking-style controls when supported.
- Pre-synthesis of likely continuations.
- Audio chunk identifiers linked to source text and candidate revision.
- Truncation boundaries and repair annotations.

The persistent gateway's initial policy makes fast output one bounded spoken
micro-turn, coalesces Fish output into paced 100 ms wire frames, and lets a
committed slow assistant or tool call supersede only unplayed fast media. This
is a phase-authority/playback rule, not a transcript classifier. It prevents
the stronger continuation from waiting behind a large provisional audio buffer
while preserving every already-heard prefix.

### 7.10 Commit controller

The controller is the safety boundary between speculation and user-visible output. Its policy considers:

- Perception stability.
- Semantic risk and reversibility.
- Whether the output is a backchannel or factual claim.
- Tool side effects.
- Time until playback.
- Current interruption evidence.

Irreversible tool calls require a separate authority policy and are never justified merely by low latency.

### 7.11 Local inference and resource governor

The live modular path now supports a co-located Qwen3-ASR, Qwen fast LLM, and
streaming Fish Audio S2-Pro on the same accelerator, while slow Gemini may be
hosted. Co-location remains an experimental variable, not an assumed latency
win. The first governor controls request admission with explicit runtime class,
deadline, abstract capacity cost, and an interactive reservation; it never
classifies prompt text. It records:

- Reserved memory and model residency.
- Prefill, decode, and synthesis priority.
- Queue arrival, service, and wait time per stage.
- Persistent-session and prefix-cache reuse.
- Preemption and cancellation delay.
- GPU utilization, memory bandwidth pressure, and discarded speculative work.

ASR, final fast fallback, and TTS receive interactive priority. Pre-endpoint
fast work is speculative and cooperatively preemptible; a local slow
continuation is background and cooperatively preemptible. Capacity is released
only after the provider reaches a safe point. This protects perception and
audible-output deadlines, but strict priority can starve background work under
sustained load; fairness or a background reservation must be an explicit
follow-up policy. Separate-process, shared-runtime, and partially hosted
placements should be compared.

### 7.12 Trace and replay system

Every experiment emits an append-only trace with:

- Monotonic timestamps.
- Audio frame ranges, stored or hashed according to consent.
- Perception revisions.
- Microturn triggers and suppressions.
- Canonical trajectory item identities and the prefix boundary visible to each invocation.
- Fast/slow phase, model identity, reasoning budget, and request/response timing spans.
- Candidate creation, replacement, commitment, playback, and cancellation.
- Tool lifecycle.
- Network, provider, and GPU queue delays.
- Resource use and estimated cost.

Replay can replace models with recorded outputs, inject network jitter, and evaluate alternative policies against the same input.

## 8. Protocol and extension boundaries

The external OpenAI Realtime wire protocol remains unchanged. Canonical
trajectory items are internal engine state, not new client/server messages.
No OpenAI protocol revision is needed to implement microturn scheduling,
fast/slow continuation, tool execution, or provider-state inheritance; adapters
project their results onto existing text, audio, cancellation, and function-call
events.
Reproducibility does not require a second public event vocabulary. The current
implementation uses typed trajectory items plus separate benchmark reports. If
a streaming internal journal becomes necessary, it should stay compact and
separately versioned around observation, continuation lifecycle, preparation
disposition, tool action/result, speech commitment, and metric records. It must
not mirror every model-internal transition or masquerade as a Realtime event.
Internal records carry monotonic time and causal identity; provider-specific
data belongs in a namespaced extension.

Reasoning observability is deliberately minimal. The default trace records
phase, model, timing, token counts when exposed, interruption/resumption, and a
hash or item reference. Raw reasoning text is opt-in research data subject to
provider support, privacy, consent, and retention policy. It is never required
on the OpenAI-compatible wire boundary.

### 8.1 Component interfaces

- `PerceptionProvider`: audio frames to perception revisions.
- `TrajectoryStore`: append typed items and compile a causally valid prefix.
- `ContinuationProvider`: a trajectory prefix and phase configuration to streamed reasoning, assistant-content, and native call events; descriptor authority determines whether a call becomes a proposal or an executable call.
- `ContinuationPolicy`: safe opportunities to fast or slow model invocations.
- `SpeechProvider`: speech spans to timestamped audio chunks.
- `TurnPolicy`: evidence to turn/overlap state transition.
- `CommitPolicy`: candidate and evidence to prepare/commit/cancel decision.
- `ToolProvider`: typed calls with authority and cancellation semantics.

Adapters must declare capabilities rather than silently ignore unsupported operations.
The existing stable `api/v1` fast-decision and deliberation roles remain supported
as the reproducible M4 boundary. Canonical-trajectory continuation is an
experimental interface and requires `api/v2` before it can replace those roles
for downstream Go consumers. Here `api/v2` means a future version of this
repository's component interfaces, not a version or fork of the OpenAI Realtime
wire protocol.

## 9. Experimental program

### 9.1 Baseline conditions

Each major experiment should compare as many of these conditions as applicable:

- **B0: Endpointed cascade.** Wait for end-of-turn, then ASR finalization, language generation, and speech.
- **B1: Streaming without microturn policy.** Stream components but retain conventional response initiation.
- **R0: Fixed-cadence reflex cascade.** Incremental ASR, one fast model, and streaming TTS, with the same component versions as B0 where possible.
- **R1: Adaptive reflex cascade.** The R0 models with revision-, stability-, and turn-projection triggers.
- **I0: Homogeneous interleaved thinking.** Gemini 3.5 Flash minimal-thinking fast continuation followed by the same family at medium/high thinking over one trajectory.
- **I1: Heterogeneous interleaved thinking.** Local Qwen instruct fast continuation followed by Gemini 3.5 Flash medium/high thinking over one trajectory.
- **D0: Independent dual-model control.** Fast output and slow advice are generated independently and reconciled afterward. This intentionally retains the split-brain baseline.
- **N0: Online native speech model.** Speech-in/speech-out service whose
  response is conventionally opened by VAD/endpointing, for example applicable
  Qwen online, GPT-Realtime-2, or Gemini Live profiles available during the
  study.
- **N1: Native interaction model.** Continuous or persistent audio-in/audio-out
  model trained to make subturn interaction decisions, such as GPT-Live,
  Thinking Machines Lab Interaction Models, or Moshi. Record the system's
  actual decision/block scale where disclosed rather than assuming all native
  systems use the same clock. Do not substitute GPT-Realtime or LiveKit when a
  GPT-Live endpoint is unavailable.
- **H1: Hybrid.** Native or acoustic model for interaction signals with modular higher-level reasoning.

Comparisons must control component model versions, region, network path, audio device, prompt, voice, and workload where possible.

τ-Voice is the primary joint falsification harness for B0/R0/R1/I0/I1/D0 and
available N0/N1 conditions because it combines grounded environment tools,
long multi-turn policy context, task success, and full-duplex interaction. Its
200 ms default simulation tick is an evaluation clock, not permission to tie
the implementation to a 200 ms internal scheduler. Each submission records the
actual internal cadence and maps continuous output to the benchmark tick
contract.

### 9.2 Ablations

- Trigger interval.
- Fixed versus perception-event versus semantic-event triggering.
- Opportunity count versus actual ASR/LLM/TTS invocation count.
- Stable-prefix gating.
- Turn projection model.
- Speculative text generation.
- Speculative TTS.
- Commit horizon size.
- Backchannel policy.
- Fast-model choice: local Qwen instruct versus Gemini 3.5 Flash minimal thinking.
- Slow continuation availability and reasoning effort.
- Always-continue versus learned continuation policy.
- Same-family versus cross-family continuation.
- Full reasoning inheritance, normalized working trace, and content-only handoff.
- Independent advice versus canonical-trajectory continuation.
- Cancellation support.
- Prefix/session reuse.
- Co-located versus split or hosted component placement.
- Acoustic/prosodic side channel.
- Network latency and jitter.
- Small versus strong fast-decision model.
- Streaming ASR capacity: Qwen3-ASR 0.6B versus 1.7B with identical downstream
  components, tasks, voices, cadence, and seed.
- Slow launch immediately after every committed fast safe point versus a
  separately declared content-independent resource pacer.
- Serial callback mutation versus the canonical safe-point event loop is not a
  benchmark condition: the former violates the architecture and belongs only
  in fault-injection tests.

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

Questions that need tools or continued reasoning. Compare blocking slow inference, fast-only answers, independent fast/slow advice, and interleaved fast/slow continuation. Score whether the later model inherits prior reasoning and spoken commitments, whether the agent repeats itself or contradicts audible content, and whether tools are used without falsely denying or claiming capabilities.

τ-Voice is the main external workload here. Run its airline, retail, and
telecom task policies under both clean/control and realistic/regular speech.
Preserve the benchmark's real environment tools and database outcome checks;
do not replace them with text-only or mocked call-shape scoring. Report tool and
long-context failure subsets separately so an aggregate pass rate cannot hide
split-brain, forgotten-policy, or synchronization failures.

Full-Duplex-Bench v3 complements τ-Voice with 100 released human recordings,
79 unique scenarios, 12 mock APIs, disfluency, rollback, and exact tool-call
evaluation. Run it through the standard OpenAI Realtime adapter with real
`function_call_output` resumption. Its response-quality judge and transcript
alignment method must be declared separately from tool-name/argument metrics.

Peng et al.'s FD-Bench is a distinct long-form interaction suite, not an
abbreviation for Full-Duplex-Bench. Run every released audio condition through
the same standard Realtime boundary at wall-clock speed. Preserve the
release's 16 kHz timestamp clock, Silero-VAD threshold 0.5 and 1,500 ms minimum
silence, and 10-second post-input collection window. Because many released
files end during speech, stream a declared 600 ms zero-PCM VAD finalizer inside
that fixed window; otherwise the standard server-VAD boundary may never emit
the final turn. The 13 source archives expand to 21 evaluation cells and 6,147
conversations (77.2184 hours). The paper and ground truth describe 293
conversations per cell. Only the three released ChatTTS cells contain 291:
IDs 60 and 120 are absent; the other 18 cells contain all 293. Report the exact
released population and this discrepancy instead of imputing cases. Keep its
interaction timing metrics separate from τ-Voice task/tool success and FDB v3
tool correctness.

#### G. Paralinguistic challenge set

Sarcasm, uncertainty, laughter, sighs, emotional prosody, and non-speech events. This set is expected to expose limitations of text bottlenecks.

## 10. Metrics

### 10.1 Timing

- Capture-to-perception revision latency.
- Stable-prefix latency.
- Trigger-to-provider-start and trigger-to-provider-first-token latency.
- End-of-user-speech to first audible output.
- First **semantic** audio latency, excluding non-substantive filler.
- Time from first semantic audio to the first slow continuation or tool call.
- Time to final correct answer or completed tool outcome.
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
- Fast/slow commitment contradiction and explicit-repair rate.
- False capability denial and false action-completion rate.
- Resumption success after user, ASR, or tool interruption.
- Conversation task success.
- τ-Voice response/yield latency and rate, agent interruption rate, and
  selectivity for backchannels, vocal tics, and non-directed speech using the
  upstream tick-level metric implementation.

### 10.3 Content quality

- ASR word and semantic error rates.
- Exact recognition and repair burden for spoken identifiers, numbers, and
  addresses; aggregate WER alone cannot hide grounded argument corruption.
- Factual and reasoning task scores.
- Tool correctness.
- Reasoning continuity: retained assumptions, avoided duplicate work, and correct use of earlier tool state.
- Translation quality paired with latency metrics.
- Human ratings of relevance, naturalness, prosody, trust, and cognitive burden.

### 10.4 Efficiency

- Audio, text, and reasoning tokens where exposed.
- Microturn opportunities, suppressed opportunities, and actual provider invocations.
- Compute time, CPU/GPU utilization, memory, and network bandwidth.
- Queue wait, cache reuse, model preemption, and residency time by component.
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
│   ├── canonical-trajectory.md
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

### M4 — Fast/slow cognition lifecycle baseline

Status: complete historical milestone. Its independent foreground decision and
slow-update abstraction remains reproducible but is not the target cognitive
architecture introduced in plan version 0.2.

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

### M8 — Live local microturn cascade

Status: in progress. The live 200 ms input / 200 ms stateful provider condition
is implemented with Qwen3-ASR 0.6B, Qwen3-30B-A3B-FP8, Fish S2-Pro, explicit GPU
admission, exact fast→slow background preparation, and real audio-to-audio
reports. A same-fixture endpointed/fast-only/full-preparation exploratory check
is published. A closed endpoint-only gateway policy and complete paired
control population are now implemented, frozen, and queued; it suppresses
partial-revision continuation calls but retains the identical post-endpoint
fast→slow→tool-result trajectory. A content-independent one-second
slow-launch pacer reduced
speculative slow provider launches from 43 to 12 in one exact-scored trial and
the final commit bypassed its remaining wait. A persistent standard Realtime
gateway now composes all three local GPU services with hosted slow reasoning,
passes the official τ OpenAI provider suite 12/12, and exposes paced Fish agent
speech. Complete 50/100/400/800 ms candidate matrices are frozen and queued;
each changes the τ input frame, gateway provider chunk, and Qwen server chunk
together and verifies the live profile before execution. A separate
revision-event control and bounded 100–400 ms revision-adaptive condition are
also frozen and queued. Both use 50 ms input opportunities, and the adaptive
rule observes typed revision presence only; matrix artifacts preserve start
and final ASR, fast, slow, and Fish provider-work/timing counters alongside GPU
telemetry. Repeated randomized trials, tail distributions, and analysis of the
complete results remain.

Deliverables:

- Live incremental ASR adapter with an explicit causal/chunked/increasing-prefix declaration.
- Local fast-model adapter with a persistent session or measured prefix-cache behavior.
- Streaming Fish Audio speech adapter with measured first-chunk latency.
- GPU resource governor for co-located ASR, language, and speech inference.
- Endpointed, 50/100/200/400/800 ms, revision-event, and adaptive conditions using the same models.

Exit criteria:

- A real audio-to-audio run publishes stage, queue, cache, GPU, and playback timing rather than symbolic delays.
- Every fixed tick can be distinguished from an actual provider invocation.
- Co-location claims report tail latency, task quality, and contention, not only median first audio.

### M9 — Canonical trajectory and heterogeneous interleaved thinking

Status: in progress. The append-only store, provider compilers, fast/slow
runners, proposal-versus-execute authority, exact-match pre-endpoint
fast→slow preparation, exact stage replay/live fallback, tool-result
continuation, content-independent per-stage temporal pacing with exact-commit
bypass, exact tool-trajectory scoring, and passing heterogeneous live trials
are implemented. The single-owner structured event loop, versioned
compare-and-append model transactions, trusted typed interruption, complete
asynchronous tool-result batches, common agent policy across phases, and
cancellation-aware audible-history projection are also implemented and wired
into the persistent Realtime server. Standard external function results resume
slow directly from the exact extended canonical prefix without rerunning fast;
the bounded continuation count is itself derived from that prefix rather than
hidden process state. Phase-authority media
supersession and 100 ms real-time output pacing bound the unplayed fast prefix.
The scorer
requires an exact call multiset and exactly one identity-matched terminal
result per call. An official τ airline task executed both required reads and,
after the bounded-media change, passed its exact numeric communication check at
reward 1.0. The trial also records superseded/failed background work and
preserved failure cases. The same-family Gemini-fast matrix and typed
content-only/independent slow-context projections are implemented and queued
as complete controls; every condition retains one canonical store and differs
only in the declared provider view. Canonical stable-partial effects, broader
audible repair integration, and an `api/v2` proposal remain.

Deliverables:

- Experimental canonical trajectory schema and append-only store.
- Provider-specific context compilers for local Qwen and Gemini 3.5 Flash.
- Fast and slow continuation runners over the same trajectory.
- Tool-capability manifest and schemas shared by both phases, with fast calls mapped to non-executable proposals and execution restricted to slow authority.
- Safe-point interruption, reasoning preservation, tool-result injection, and audible-commit repair.
- One structured event-loop owner with atomic event/model/result transactions,
  occurrence-versus-commit provenance, and no content-pattern routing.
- Migration proposal from the M4 roles to an eventual `api/v2` continuation interface.

Exit criteria:

- Slow inference consumes the exact fast trajectory prefix and never relies on a lossy task summary.
- Proposals, executable calls, and results remain distinct; executable calls
  and results appear once in causal order and are visible to every later
  continuation.
- New user or ASR evidence can interrupt, extend, and resume thinking without erasing completed reasoning or silently rewriting speech.
- Concurrent event, provider, and tool completion race tests prove that stale
  work cannot append or expose a side effect.
- Same-family, cross-family, content-only, and independent-advice controls are reproducible.

### M10 — Responsiveness–intelligence comparative study

Status: full execution in progress. The 12-case official τ provider gate,
exploratory paired task, strict seven-persona Fish registry, frozen baseline
matrix, exact FDB v1.5/FDB v3 releases, and resumable runners are complete. The
278-task control cell is running; regular, 498-recording FDB v1.5,
100-recording FDB v3, complete Qwen3-ASR 1.7B paired cells, the 21-cell,
6,147-conversation released FD-Bench matrix, complete paired Gemini-fast cells,
four complete cadence matrices, a complete medium-slow matrix, and complete
content-only/independent context controls are queued. Native GPT-Live and TML
Interaction Model controls remain unavailable through a public executable
endpoint. A complete endpoint-only preparation control is queued behind the
event/adaptive populations and changes only whether partial ASR revisions may
start private continuation work. A fresh complete continuous population runs
immediately before it because the historical baseline predates provider-work
health counters. A strict paired reporter requires matching source revisions,
ASR/model/Fish/tool profiles, complete populations, and provider deltas before
it emits raw differences; it creates no private composite score. No incomplete
cell is a benchmark score.

Deliverables:

- Paired local-Qwen-fast and Gemini-fast conditions with Gemini 3.5 Flash medium/high slow continuation.
- A pinned τ-Voice adapter covering its full 278-task airline/retail/telecom
  suite in both control and regular speech conditions. Preserve its standard
  OpenAI adapter with an explicit local endpoint; use the separately named
  Fish Audio caller backend and the disclosed strict Fish voice registry.
- Complete FDB v1.5 overlap and FDB v3 disfluent tool-use populations through
  the same standard Realtime boundary, with immutable artifacts, actual tool
  results, terminal result resumption, and official evaluation.
- Complete all 21 released FD-Bench cells from the 13 source archives through
  that boundary, retain the upstream timestamp/VAD contract, and evaluate the
  pinned upstream timing decision core without inventing its missing non-Moshi
  WER or CPPL files.
- A complete paired Qwen3-ASR 0.6B/1.7B capacity matrix whose motivating
  identifier failure cannot become a task-specific hint or routing rule.
- A complete paired local-Qwen/Gemini-3.5-Flash fast-provider matrix with the
  same high-thinking Gemini slow continuation and no content-dependent router.
- Difficult reasoning and active-tool workloads combined with overlap, interruption, selectivity, long-context, and cadence workloads.
- Native realtime and interaction-model comparisons where access and redistribution permit.
- Pareto analysis over first semantic audio, final quality, trajectory consistency, tool correctness, compute, and cost.

Exit criteria:

- The project can separately attribute gains to trigger timing and to continued reasoning.
- Results identify when the two mechanisms compose, interfere through resource contention, or fail to match native systems.
- τ-Voice results report pass^1 jointly with response/yield behavior,
  interruption rate, and selectivity; a latency-only or text-only result does
  not satisfy the milestone.
- Claims of high responsiveness and high intelligence are supported by joint measurements rather than separate demonstrations.

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
21. Maintain the implemented experimental canonical trajectory item schema and provider context compiler contract.
22. Extend the implemented fast/slow continuation phases without a separate advice channel.
23. Compare the implemented Qwen3-ASR/local-Qwen/Fish Audio adapters with SenseVoice and other declared ASR conditions.
24. Extend implemented priority/capacity admission with aligned GPU residency, utilization, and queue sampling.
25. Expand the implemented fixed-tick versus provider-invocation accounting across every cadence condition.
26. Add same-family and cross-family reasoning-continuity ablations.
27. Add capability-consistency and split-brain evaluations.
28. Maintain the integrated safe-point event loop across live ASR, playback,
    asynchronous tool ingress, bounded queues, and overload tests.
29. Maintain the completed persistent local Realtime gateway and its 12-case
    official τ OpenAI-provider conformance gate as the wire/runtime changes.
30. Complete the running preregistered τ-Voice control/regular matrix with the
    fixed provenance-recorded Fish voice set; publish failures and incomplete
    cells without relabeling them.
31. Complete the queued FDB v1.5/FDB v3 populations and official FDB v3
    evaluation through the standard Realtime adapter.
32. Complete the frozen 0.6B/1.7B ASR capacity comparison after the primary
    queue, restoring the baseline service afterward.
33. Complete the queued 21-cell released FD-Bench population and its pinned
    Silero/upstream-timing evaluation after the ASR capacity matrix.
34. Complete the queued Gemini-minimal fast-provider matrix after FD-Bench,
    restoring the local Qwen fast profile afterward.
35. Draft the `api/v2` migration only after the experimental trajectory contract stabilizes.

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
- **Mitigation:** Treat ticks as opportunities rather than mandatory calls; use persistent ASR/model sessions, prefix caching, state reuse, explicit budgets, and discarded-work metrics. The reference semantic policy still makes slow eligible after a completed fast continuation. The implemented temporal pacer can delay only speculative provider launch, is bypassed by exact commit, and never reads content; any learned admission rule is likewise a declared cost-policy ablation, never content-pattern routing.

### Split-brain cognition

- **Risk:** Independently prompted fast and slow models disagree about capabilities, repeat work, or contradict content already spoken.
- **Mitigation:** Use one canonical trajectory, one capability manifest, ordinary interleaved reasoning/tool/content items, and an independent-advice condition only as a control.

### Cross-model reasoning mismatch

- **Risk:** A receiving model cannot consume another provider's private, signed, or template-specific reasoning representation.
- **Mitigation:** Declare provider capabilities, preserve native reasoning only where supported, compare a model-neutral working trace with content-only handoff, and never label symbolic transfer as latent-state continuation.

### Accelerator contention

- **Risk:** Co-located ASR, LLM, TTS, and background reasoning remove network delay but create GPU queueing and missed audio deadlines.
- **Mitigation:** Reserve foreground capacity, prioritize perception and first audio, run slow continuation opportunistically, and publish component queue and utilization distributions.

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
2. Which improvements required speculation, early planning, or canonical-trajectory fast/slow continuation?
3. What were the costs in wrong starts, repairs, compute, and complexity?
4. Which behaviors remained better in native speech systems?
5. Which results generalized across languages, tasks, providers, and networks?
6. How can another researcher reproduce or challenge the findings?
7. Did trigger timing improve responsiveness independently of model intelligence?
8. Did interleaved thinking improve final reasoning and tool use without sacrificing the foreground latency gain?

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
10. Thinking Machines Lab, “Interaction Models: A Scalable Approach to Human-AI Collaboration,” 2026. <https://thinkingmachines.ai/blog/interaction-models/>
11. Ray et al., “τ-Voice: Benchmarking Full-Duplex Voice Agents on Real-World Domains,” 2026. <https://arxiv.org/abs/2603.13686>
12. OpenAI, Realtime API documentation. <https://developers.openai.com/api/docs/guides/realtime>
13. OpenAI, “Introducing GPT-Live,” 2026. <https://openai.com/index/introducing-gpt-live/>
14. OpenAI, “How OpenAI built continuous voice interaction with GPT-Live,” 2026. <https://openai.com/index/continuous-voice-interaction-with-gpt-live/>
15. Qwen, “Qwen3-ASR-1.7B model card,” 2026. <https://huggingface.co/Qwen/Qwen3-ASR-1.7B>
16. Peng et al., “FD-Bench: A Full-Duplex Benchmarking Pipeline Designed for Full Duplex Spoken Dialogue Systems,” 2025. <https://arxiv.org/abs/2507.19040>

Public APIs and papers evolve. Any experiment must cite the exact version and retrieval date used.
