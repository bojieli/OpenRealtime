# Initial literature review

Retrieved 2026-08-17; architecture synthesis updated 2026-08-18. This is a maintained map of evidence and design pressure,
not an assertion that prior results transfer unchanged to OpenRealtime.

## Incremental dialogue and response planning

Schlangen and Skantze’s abstract incremental-processing model represents module
outputs as incrementally added, revoked, and committed units. That motivates
explicit revision identities and append-only supersession rather than mutating
partial transcripts in place. Their framework is conceptual evidence for a
protocol shape, not evidence for OpenRealtime latency.

Skantze and Hjalmarsson’s incremental speech work shows why synthesis must be
planned in replaceable chunks and why revisions need downstream propagation.
Bögels reports neural evidence that response planning can begin before a turn
ends in free conversation. Together these support testing early planning while
leaving H2 falsifiable.

Stivers et al. find both broad turn-taking regularities and cross-language
variation. OpenRealtime must therefore report per-language timing and cannot
treat one fixed 200 ms cadence as universal.

TurnGPT frames turn completion as a predictive language-model task. It is an
appropriate linguistic baseline only after the acoustic VAD baseline; otherwise
model-category changes would be confounded with scheduling changes.

## Native and full-duplex systems

Moshi models user and system audio streams in parallel and reports theoretical
160 ms and practical 200 ms latency. It is a native open baseline and highlights
information lost by a text bottleneck, including emotion and non-speech sound.
OpenRealtime should compare observable behavior without assuming modular parity.

Thinking Machines Lab's Interaction Models use time-aligned 200 ms microturns
and persistent streaming sessions, and pair realtime foreground interaction
with asynchronous background reasoning. This supports 200 ms as an experimental
condition and reinforces two caveats: a tick is not a stateless restart, and
background reasoning must not block the foreground media loop. It does not show
that a modular cascade automatically matches a natively trained interaction
model.

GPT-Live is a distinct comparison target from GPT-Realtime and LiveKit. Its
official engineering description removes turn detection from the continuous
media path, makes interaction decisions several times per second, and delegates
deeper search/reasoning to an asynchronous frontier-model path. It also
describes provisional versus authoritative conversation state, prewarmed model
sessions, and warm context-compaction handoff. These are design pressures, not
local results: the official pages describe API access as upcoming, and the
account-scoped model inventory used for this study did not expose a GPT-Live
voice endpoint.

The OpenAI Realtime API is a concrete interoperability baseline rather than a
research result. Its current public reference separates client and server
events across Realtime, transcription, and translation sessions over low-
latency transports. OpenRealtime pins the official OpenAPI event schemas and
keeps its experimental timing envelope outside the wire message.

Full-Duplex-Bench v1 covers pause handling, backchanneling, turn-taking, and
interruption; v1.5 adds overlap such as listener backchannels, side speech, and
ambient speech. The repository has since added dynamic v2 and tool-use v3. Its
repository-wide CC BY-NC 4.0 license means data or code must not be copied into
the permissive OpenRealtime distribution. A separately installed adapter can be
evaluated where usage is compliant.

Qwen3-ASR provides official 0.6B and 1.7B models under one streaming/offline
inference interface. That makes capacity a cleaner recognition ablation than
changing ASR families after an observed identifier error. Model capacity,
latency, memory allocation, and grounded tool-argument accuracy still have to
be reported together; the larger model is not presumed better end to end.

## Asynchronous events and continuous thinking

The target OpenRealtime abstraction treats user speech, recognition revisions,
assistant reasoning/content, tool calls/results, and interruptions as one
ordered trajectory. Events are consumed at safe boundaries: routine events may
batch, urgent events may force an early boundary, and independent tools may run
concurrently while retaining causal provenance.

This produces a specific fast/slow hypothesis. A fast and a slow model should
not be prompted as two independent agents and reconciled through an advice
summary. Instead, the fast model appends the first reasoning/content segment
and any non-executable tool proposal, and the slow model continues from that
exact prefix with execute authority. The project calls this heterogeneous
interleaved thinking. One exploratory live tool trial now validates the
proposal/call/result wiring, but the comparative claim remains prospective
until tested against the independent M4 baseline and the registered controls.

## Simultaneous translation

SimulEval evaluates translation quality jointly with latency in a server-client
simulation and implements established latency measures. Its official repository
was archived on 2025-09-18 and declares CC BY-SA 4.0. OpenRealtime should first
implement protocol-level compatible metrics or an external adapter, not copy
share-alike code into the Apache-licensed package.

## Sources

1. Stivers et al. (2009), “Universals and cultural variation in turn-taking in
   conversation.” <https://doi.org/10.1073/pnas.0903616106>
2. Schlangen and Skantze (2011), “A General, Abstract Model of Incremental
   Dialogue Processing.” <https://aclanthology.org/2011.dnd-2.11/>
3. Skantze and Hjalmarsson (2013), “Towards incremental speech generation in
   conversational systems.” <https://doi.org/10.1016/j.csl.2012.05.004>
4. Ekstedt and Skantze (2020), “TurnGPT.”
   <https://aclanthology.org/2020.findings-emnlp.268/>
5. Bögels (2020), “Neural correlates of turn-taking in the wild.”
   <https://doi.org/10.1016/j.cognition.2020.104347>
6. Défossez et al. (2024), “Moshi.” <https://arxiv.org/abs/2410.00037>
7. Lin et al. (2025), “Full-Duplex-Bench.”
   <https://arxiv.org/abs/2503.04721>
8. Lin et al. (2025), “Full-Duplex-Bench v1.5.”
   <https://arxiv.org/abs/2507.23159>
9. Ma et al. (2020), “SimulEval.”
   <https://aclanthology.org/2020.emnlp-demos.19/>
10. OpenAI, “Realtime API reference.”
    <https://platform.openai.com/docs/api-reference/realtime>
11. Thinking Machines Lab (2026), “Interaction Models: A Scalable Approach to
    Human-AI Collaboration.”
    <https://thinkingmachines.ai/blog/interaction-models/>
12. OpenAI (2026), “Introducing GPT-Live.”
    <https://openai.com/index/introducing-gpt-live/>
13. OpenAI (2026), “How OpenAI built continuous voice interaction with
    GPT-Live.”
    <https://openai.com/index/continuous-voice-interaction-with-gpt-live/>
14. Qwen (2026), “Qwen3-ASR-1.7B model card.”
    <https://huggingface.co/Qwen/Qwen3-ASR-1.7B>
