# ADR-0003: Canonical trajectory with heterogeneous continuations

- Status: Accepted for the experimental continuation packages; stable `api/v1`
  and the OpenAI Realtime wire protocol remain unchanged
- Date: 2026-08-18

## Context

OpenRealtime currently demonstrates two useful but separate mechanisms:

- M2/M3 open decision opportunities before endpointing and protect speculative
  speech with cancellation, commitment, and repair.
- M4 separates a bounded foreground decision from an asynchronous deliberation
  update stream.

The M4 contract is safe and reproducible, but it models fast and slow cognition
as separate information channels. A foreground model can misunderstand slow
advice, repeat its work, deny capabilities that only the slow path can execute,
or contradict content already spoken. It also does not express the continuous
thinking sequence used by ordinary tool-capable language models: reasoning,
assistant content, further reasoning, a tool call, a tool result, and continued
reasoning.

Realtime triggering and cognitive depth solve different problems. Frequent or
event-driven microturns reduce the time before useful work can begin. A stronger
continuation raises the eventual reasoning and tool-use ceiling. The target
architecture needs both without creating two agents.

## Decision

Adopt a canonical, append-only trajectory as the experimental cognitive source
of truth. Fast and slow inference are continuation profiles that consume a
prefix of that trajectory and append ordinary reasoning, assistant-content,
tool-proposal, tool-call, and tool-result items.

- The fast profile uses a strict deadline and either a local instruct model or
  Gemini 3.5 Flash with minimal thinking.
- The slow profile initially uses Gemini 3.5 Flash with medium/high thinking,
  a larger context projection, and executable tools.
- Both profiles receive the same agent identity, capability manifest, actual
  tool definitions, audible commitments, and actual tool state.
- Fast has proposal-only authority. Its structured calls are working state and
  can neither execute nor receive a result. Slow has execute authority and must
  generate the authoritative call independently.
- The default experiment schedules one slow continuation after every completed
  fast phase invoked for newly appended trajectory input, including a stable
  partial recognition before endpointing. It does not use keyword or pattern-
  based difficulty routing.
- Tool execution remains subject to an authority layer independent of model
  phase.
- Fast/slow model substitution occurs at safe trajectory boundaries. Cross-
  model transfer is symbolic and does not claim shared latent state or KV cache.

Microturn schedulers continue to open opportunities rather than mandatory
provider calls. Continuous ASR, model sessions, and TTS retain their own state.
Recognition revisions, tool results, interruptions, and completed continuations
may wake the event loop between periodic ticks. Pre-endpoint work remains on a
private trajectory: fast appends first and slow consumes that exact private
prefix. The branch has no tool runtime or speech sink. It may commit only when
its complete provider-visible root fingerprint matches final input, and each
stage is replayed only when its exact canonical stage input also matches. The
implementation uses no fuzzy matching, transcript normalization, or difficulty
router.

Co-located resources use runtime-declared scheduling classes and capacity costs.
Interactive capacity is reserved from speculative/background work. Preemption
is cooperative and releases capacity only after a provider safe point; it never
depends on prompt content.

The OpenAI Realtime wire protocol and its generated schemas remain unchanged.
Canonical trajectory items are internal. Optional research telemetry records
only minimal reasoning lifecycle metadata by default; raw reasoning is opt-in.

Stable `api/v1` is unchanged. M4 remains a historical result and the independent
fast/slow control. A public continuation interface requires `api/v2` after the
experimental contract is validated.

## Consequences

### Positive

- Slow reasoning directly inherits fast reasoning and exactly what the user
  heard instead of receiving a lossy summary.
- Capability knowledge is consistent even when execution authority differs.
- Fast models can express a precise tool need without acquiring action
  authority or falsely denying the capability.
- Tool calls, asynchronous results, interruption, and resumption share one
  causal history.
- Exact-match preparation can move both fast and slow inference before the
  endpoint without appending stale partial-hypothesis output or executing a
  speculative tool call.
- Trigger timing and reasoning depth can be ablated independently and then
  evaluated jointly.
- The external Realtime protocol remains interoperable.

### Costs and risks

- Provider chat templates and reasoning representations differ; adapters must
  declare loss or normalization.
- Preserving reasoning can increase context size and may hurt cross-family
  inference.
- Always scheduling slow continuation consumes additional compute. The first
  background live trial superseded 44 of 45 private chains, making this cost
  visible rather than treating canceled requests as free.
- Revision-triggered fast→slow preparation can consume substantial local and
  hosted inference even when most chains are discarded; its benefit must be
  reported with per-stage invocation, token, and cancellation counts.
- Content-independent temporal pacing can reduce speculative launch frequency
  without adding semantic routing. It is an explicit deployment ablation,
  defaults to zero, and exact commit must bypass any remaining wait.
- Co-located foreground and background models may contend for accelerator
  capacity.
- Strict priority can starve background work during sustained foreground load;
  deployments that require progress guarantees need an explicit fairness
  policy.
- The internal trajectory schema and an eventual `api/v2` migration add
  implementation work.

## Alternatives considered

### Independent foreground and slow advice

Retain M4 as the target. This preserves a simple typed coordinator but leaves a
semantic handoff and split-brain failure mode. It remains the control condition.

### Fast-model delegation tool

Expose a `delegate_to_slow` call and invoke slow reasoning only when selected by
the fast model. This is useful as a later cost ablation, but makes a latency-
optimized model responsible for recognizing its own reasoning limits and can
reintroduce brittle routing.

### One blocking high-reasoning model

This maximizes continuity but sacrifices the foreground latency objective and
cannot test whether microturn responsiveness composes with deeper reasoning.

### Modify the OpenAI Realtime protocol

Rejected. Model-internal continuation and research telemetry do not require new
wire events. Changing the public protocol would reduce interoperability without
improving the internal trajectory semantics.

## Implementation evidence

The accepted design is implemented by the experimental `trajectory`,
`continuation`, `preparation`, `admission`, and `interleave` packages. An
exact-scored co-located Qwen3-ASR/Qwen/Fish S2-Pro run with hosted Gemini 3.5
Flash committed and replayed both prepared stages, then preserved one
non-executable fast proposal, one executable slow call, and one result. Failed
runs remain published, including a wrong-call recovery that exposed the old
coarse scorer and a six-call identifier loop stopped by the invocation bound.
This is exploratory implementation evidence, not the comparative-study
conclusion. See
[live-cascade.md](../live-cascade.md).
