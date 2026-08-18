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
has been produced for the complete benchmark. A reproducible patch keeps τ's
standard OpenAI adapter, adds an explicit local WebSocket endpoint, and adds
Fish Audio as a separately identified caller-synthesis provider. The persistent
OpenRealtime gateway now passes all 12 selected cases in τ's official
audio-native provider suite and has completed two exploratory airline tasks.
The checked result is explicitly a one-task smoke, not a 278-task τ-Voice score.

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

Start the local gateway after Qwen3-ASR, vLLM Qwen, and Fish S2-Pro are healthy:

```sh
export OPENREALTIME_GATEWAY_TOKEN=local-only-token
export GEMINI_API_KEY=...
go run ./cmd/realtimegateway
```

The gateway ingests persistent G.711/PCM audio, uses acoustic VAD plus 200 ms
stateful ASR advances, commits one canonical fast→slow trajectory, emits only
slow-authorized function calls, resumes slow from exact external result
batches, and streams locally generated Fish speech. It validates every wire
message against the pinned OpenAI Realtime schema by default.

Run the official upstream provider suite through its ordinary OpenAI adapter:

```sh
OPENAI_REALTIME_BASE_URL=ws://127.0.0.1:8765/v1/realtime \
OPENAI_REALTIME_API_KEY="$OPENREALTIME_GATEWAY_TOKEN" \
uv --directory .runtime/tau2-bench run pytest \
  tests/test_voice/test_audio_native/test_provider_suite.py \
  -q -s -k openai
```

`OPENAI_API_KEY` remains the hosted key used by τ's user simulator and by the
suite's upstream availability gate. The patched local Realtime transport reads
only `OPENAI_REALTIME_API_KEY`, so a hosted credential is never sent to the
loopback gateway.

The reproducible task-3 control smoke is:

```sh
TAU2_DIR=.runtime/tau2-bench scripts/prepare-tau-voice.sh
OPENAI_REALTIME_API_KEY="$OPENREALTIME_GATEWAY_TOKEN" \
uv --directory .runtime/tau2-bench run tau2 run \
  --domain airline \
  --task-ids 3 \
  --num-trials 1 \
  --max-concurrency 1 \
  --max-retries 0 \
  --hallucination-retries 0 \
  --timeout 480 \
  --max-steps-seconds 360 \
  --audio-native \
  --audio-native-provider openai \
  --audio-native-model gpt-realtime-1.5 \
  --audio-native-base-url ws://127.0.0.1:8765/v1/realtime \
  --voice-synthesis-provider fish_audio \
  --fish-audio-endpoint http://127.0.0.1:8081/v1/audio/speech \
  --fish-audio-model fishaudio/s2-pro \
  --fish-audio-voice default \
  --tick-duration 0.2 \
  --speech-complexity control \
  --save-to openrealtime-canonical-tools-optimized-2026-08-18 \
  --verbose-logs
```

The `gpt-realtime-1.5` model value is only the compatibility/pricing identity
expected by the pinned upstream ledger. The endpoint is the local composite,
not an OpenAI model. Consequently τ's reported agent cost is a consistent
pricing proxy, not the actual Qwen/Gemini/Fish spend. Fish needs no key on a
trusted loopback server. The hosted slow continuation reads `GEMINI_API_KEY`
inside OpenRealtime, not through the τ client.

The checked patch is
[`0001-local-openai-fish-audio.patch`](patches/0001-local-openai-fish-audio.patch).
At the pinned revision its focused and affected voice/streaming suites pass 85
tests. In addition, the live local composition passes 12/12 OpenAI-selected
cases in the official horizontal provider suite. That suite covers lifecycle,
200 ms timing, two speech lengths, multi-turn audio, tool-result resumption,
usage, and barge-in. It is a compatibility gate, not a task score.

The final post-hardening rerun occurred while unrelated host work held all 32
logical CPUs at 0% idle. Two ordinary-scheduler runs each passed 11/12 and
failed one wall-clock tick at 414 ms and 386 ms respectively; the failed case
passed alone at a 215 ms maximum. The unchanged full suite passed 12/12 when
only the benchmark client was pinned to one CPU with FIFO priority 10. The
checked result preserves both the saturated-host failures and that execution
condition: the isolated pass establishes adapter/function conformance, not an
uncontended latency distribution.

## Adapter boundary

The implemented bridge uses the upstream `DiscreteTimeOpenAIAdapter` against a
persistent OpenRealtime session; it does not port or fork the benchmark loop.
Its contract is:

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

## Exploratory paired result: 2026-08-18

The checked secret-free record is
[`tau-voice-openrealtime-smoke-2026-08-18.json`](../results/tau-voice-openrealtime-smoke-2026-08-18.json).
Both conditions used official airline task 3, seed 300, 200 ms ticks, control
speech, the standard OpenAI Realtime adapter, local Fish caller and agent
speech, Qwen3-ASR, Qwen fast, and Gemini 3.5 Flash high slow reasoning.

| Measure | Baseline | Bounded/superseding media |
| --- | ---: | ---: |
| τ task reward | 0.0 | 1.0 |
| Required read actions | 2/2 | 2/2 |
| Exact communication `4` | Fail | Pass |
| DB match | Pass | Pass |
| Unresponsive period | No | No |
| Duration | 317.20 s | 223.61 s |
| τ priced agent-cost proxy | $1.0191 | $0.5601 |

The optimization made the fast phase one short spoken micro-turn, reduced its
hard output bound from 96 to 32 tokens, coalesced arbitrary Fish fragments into
real-time-paced 100 ms wire frames, and made an authoritative slow assistant or
tool safe point supersede only unplayed fast media. It did not inspect task
text or add a difficulty/status router. In this sequential pair, duration fell
29.5%, the pricing proxy fell 45.0%, and reward rose from 0 to 1. This is useful
mechanism evidence, but one task and one sample per condition do not establish
a distribution or general benchmark improvement.

An earlier task-1 smoke correctly refused a disallowed cancellation and
received reward 1.0, but skipped two expected diagnostic read actions; it is
preserved as negative evidence rather than used as the primary tool test.

## Current blockers and next action

The gateway, official provider gate, external tool-result resumption, local
Fish caller/agent paths, and one-task control smoke are complete. Next, freeze
the paired matrix manifests, add repeated seeds and task subsets without tuning
against their contents, configure and disclose Fish reference voices for the
`regular` condition, and then run complete 278-task cells as resources permit.
Native baselines and cadence/effort ablations remain outstanding. No patch
test, conformance suite, mock, text-only run, or one-task smoke will be
relabeled as a full τ-Voice score.
