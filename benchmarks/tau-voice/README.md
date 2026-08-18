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
has been produced. A reproducible patch now keeps τ's standard OpenAI adapter,
adds an explicit local WebSocket endpoint, and adds Fish Audio as a separately
identified caller-synthesis provider. The persistent OpenRealtime gateway is
still pending, so published τ-Voice numbers remain external context only.

## Executable harness integration

Prepare the exact upstream revision, apply the local-endpoint/Fish patch, and
run its affected tests:

```sh
scripts/prepare-tau-voice.sh --verify
```

The upstream voice extra builds PyAudio. On Ubuntu, install
`portaudio19-dev` before `--verify` if `portaudio.h` is unavailable.

The patch does not add an `openrealtime` provider to τ and does not alter the
OpenAI Realtime wire protocol. It extends `DiscreteTimeOpenAIAdapter` with the
ordinary operational parameter `base_url`; all session, audio, tool, and usage
events remain on the standard OpenAI path. It also adds `fish_audio` beside
`elevenlabs` in the simulated caller's synthesis layer. The provider name,
model, endpoint, voice, sample rates, and seed are retained in `VoiceRunConfig`;
the optional Fish bearer token is read only from `FISH_AUDIO_API_KEY`.

After the local gateway and model services are running, the first control
smoke command has this shape:

```sh
TAU2_DIR=.runtime/tau2-bench scripts/prepare-tau-voice.sh
OPENAI_API_KEY=local-only-token uv --directory .runtime/tau2-bench run tau2 run \
  --domain airline \
  --audio-native \
  --audio-native-provider openai \
  --audio-native-model openrealtime-local \
  --audio-native-base-url ws://127.0.0.1:8765/v1/realtime \
  --voice-synthesis-provider fish_audio \
  --fish-audio-endpoint http://127.0.0.1:8081/v1/audio/speech \
  --fish-audio-model fishaudio/s2-pro \
  --fish-audio-voice default \
  --tick-duration 0.2 \
  --speech-complexity control \
  --num-tasks 1 \
  --num-trials 1 \
  --verbose-logs
```

Use a dedicated local-only token in `OPENAI_API_KEY`; do not send a hosted API
key to a development gateway. Fish needs no key on a trusted loopback server.
The hosted slow continuation still reads its own provider credential inside
OpenRealtime, not through the τ client.

The checked patch is
[`0001-local-openai-fish-audio.patch`](patches/0001-local-openai-fish-audio.patch).
At the pinned revision its focused and affected voice/streaming suites pass 85
tests. This verifies configuration, PCM synthesis, effects, and caller
streaming; it is not the upstream live provider suite and is not a benchmark
score.

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

Fish Audio serves two distinct roles in the local condition. Agent speech is
produced inside the OpenRealtime cascade and crosses the Realtime WebSocket as
audio events. Simulated caller speech is produced directly by τ's patched
`fish_audio` synthesis backend before its normal effects and telephony stages.
Those paths must be reported separately even when they share one Fish service.

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

Run both `control` and `regular`. A single named Fish preset is sufficient for
the clean paired control smoke test. The regular/accent condition requires a
preregistered Fish voice registry or reference-audio map with license and
speaker provenance; it must not reuse ElevenLabs IDs or be reported as the
unmodified upstream voice condition. Use complete 278-task cells for
confirmatory claims. Small task subsets are smoke tests and must be labeled by
IDs, domains, selection procedure, and exploratory status.

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

The ElevenLabs dependency is removed for the local condition. The remaining
critical path is the persistent OpenRealtime `/v1/realtime` gateway backed by
the implemented ASR, canonical fast/slow event loop, external tool-result
resumption, and Fish agent speech. Once that gateway passes τ's live provider
suite, run the one-task `control` smoke command above. Configure and disclose
Fish reference voices before any `regular` cell, then run the preregistered
paired matrix. No patch test, mock, text-only run, or incomplete smoke run will
be relabeled as a τ-Voice score.
