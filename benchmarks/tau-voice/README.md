# τ-Voice integration and evaluation plan

## Role in OpenRealtime

τ-Voice is the primary joint benchmark for the target claim. Its grounded
airline, retail, and telecom tasks exercise long policy context, multi-turn
dialogue, real environment tools and database outcomes while its full-duplex
simulation scores response, yield, interruption, and selectivity behavior.
That combination can falsify both halves of the project: a system can be fast
but fail the task, or intelligent but interact poorly.

The pinned upstream identity and current run status are in
[`tau-voice.manifest.json`](../external/tau-voice.manifest.json). No local score
has been produced. The adapter and required external voice credentials are
still pending, so published τ-Voice numbers remain external context only.

## Adapter boundary

Implement an upstream `DiscreteTimeAdapter` against a persistent OpenRealtime
session; do not port or fork the benchmark loop. The bridge must:

1. Accept the benchmark `system_prompt` as the shared `AgentInstruction` seen
   by both fast and slow profiles.
2. Convert the complete upstream tool list once into the common capability and
   schema catalog. Fast sees proposal authority; slow sees execute authority.
3. Feed every 8 kHz G.711 μ-law input tick into one persistent media/ASR
   stream. A tick is not an ASR or LLM restart.
4. Record the tick as a scheduler opportunity. Advance stateful perception only
   when provider buffering is ready and start preparation only for changed
   semantic revisions.
5. Submit only response-eligible stable observations, directed interruptions,
   playback transitions, and complete tool-result batches to the canonical
   event loop.
6. Return at most one tick of output audio, buffering excess audio according to
   the upstream contract. Transcript and speech flags must describe the audio
   actually exposed during that tick.
7. Surface only committed slow calls to τ-Voice. `send_tool_result` queues a
   structured completion event and resumes slow without rerunning fast.
8. Preserve upstream tick/event/usage records and an OpenRealtime causal trace;
   do not store credentials or raw private reasoning.

The bridge must pass the upstream audio-native provider test suite at revision
`c3398666e6559e3a063da3fc04b5acf7f941464e` before any scored task. Internal
continuous time and τ-Voice's default 200 ms evaluation ticks must remain
distinct in telemetry.

## Preregistered system matrix

Use the same benchmark tasks, user simulator, tool environment, speech
condition, output voice, and hardware/network region within each paired block.

| ID | Trigger/cascade | Cognitive policy | Purpose |
| --- | --- | --- | --- |
| B0 | VAD/endpointed ASR→LLM→TTS | one high-reasoning model | conventional modular control |
| R0-Q | fixed 200 ms opportunities, persistent components | local Qwen fast only | reflex/latency control |
| R1-Q | revision/event opportunities | local Qwen fast only | adaptive timing contribution |
| I0-G | revision/event opportunities | Gemini 3.5 Flash minimal → Gemini 3.5 Flash medium/high | same-family canonical continuation |
| I1-QG | revision/event opportunities | local Qwen instruct/no thinking → Gemini 3.5 Flash medium/high | heterogeneous target |
| D0 | same triggers as I1-QG | independently prompted fast plus slow advice | split-brain control |
| S0 | endpointed | Gemini 3.5 Flash medium/high only | blocking intelligence ceiling |
| N0/N1 | provider-native | available realtime/interaction model | external architectural baseline |

For R0/R1/I0/I1, sweep 50, 100, 200, 400, and 800 ms opportunity intervals
where compute permits. The provider's real ASR advance size is reported
separately. A 50 ms opportunity around a 200 ms stateful ASR buffer is a valid
condition; describing it as 50 ms ASR inference is not.

Run both `control` and `regular`. Use complete 278-task cells for confirmatory
claims. Small task subsets are smoke tests and must be labeled by IDs, domains,
selection procedure, and exploratory status.

## Intelligence and synchronization ablations

The primary comparison is I1-QG versus B0, R1-Q, D0, and S0. Additional
ablation axes are:

- fast profile: local Qwen instruct/no thinking versus Gemini 3.5 Flash
  minimal;
- slow effort: medium versus high;
- continuation projection: same-family native state, portable working trace,
  and content-only;
- trajectory architecture: canonical continuation versus independent advice;
- preparation: endpoint-only versus latest-wins exact preparation;
- placement: co-located ASR/Qwen/Fish Audio versus split process, with Gemini
  slow hosted in both;
- tool completion: canonical complete-batch resumption versus a fault-injection
  partial/stale result that must be rejected;
- interruption: cancellation-aware audible-history projection versus a
  fault-injection replay of unheard text that must be rejected by conformance.

Always-continuing slow is the reference semantic policy. A
content-independent launch pacer may be tested as a resource ablation and must
be bypassed at exact final commit. Content patterns, keyword routers, and
benchmark-task lookup are prohibited.

## Primary reporting panel

Report task and interaction behavior together:

- upstream `pass^1` overall and per domain;
- response latency/rate and yield latency/rate;
- agent interruption rate;
- backchannel, vocal-tic, and non-directed-speech selectivity;
- tool call/result identity and task outcome;
- first semantic audio and time to terminal grounded outcome;
- fast/slow repetition, contradiction, explicit repair, and capability denial;
- opportunity count, ASR/fast/slow/TTS provider advances, cancellations,
  discarded tokens/compute, queue time, GPU utilization, cost, and tail latency.

The main result is a Pareto frontier, not a latency-only rank or a private
composite score. Report per-domain and `control`/`regular` cells before any
aggregate. Use the upstream interaction metric implementation on uploaded
tick trajectories rather than locally redefining its windows.

## Optimization gate

An optimization is accepted only if paired evidence shows one of:

- lower response/yield latency without a meaningful loss in pass^1,
  selectivity, tool correctness, or repair burden;
- higher pass^1/tool correctness without a meaningful regression in the
  interaction panel or foreground latency;
- lower compute/cost at statistically compatible task and interaction quality.

Cache hits, fewer calls, or lower first-token latency are explanatory metrics,
not success by themselves. Preserve failed trials and all condition changes.

## Current blockers and next action

The upstream repository and interface are pinned, but this environment lacks
the required ElevenLabs key and externally configured control-persona voice
IDs. The next implementation action is the persistent `DiscreteTimeAdapter`
bridge and upstream provider-conformance tests. The first live action after
credentials are supplied is a one-task `control` smoke test followed by the
preregistered paired matrix; no mock or text-only run will be relabeled as
τ-Voice.
