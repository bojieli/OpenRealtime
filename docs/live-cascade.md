# Live microturn cascade and interleaved continuation

## Implemented slice

The repository now contains two compositions of the same real audio-to-audio
runtime: the controlled `realtimebench` harness and a persistent standard
OpenAI Realtime-compatible gateway. The controlled path is:

```text
24 kHz PCM input
  → 50 ms scheduler frames
  → 200 ms buffered, stateful Qwen3-ASR advances
  → changed transcript revisions
  → latest-wins private Qwen fast preparation (proposal authority)
  → optional content-independent slow-launch pacing
  → private Gemini 3.5 Flash high-reasoning continuation
  → exact final-root + exact per-stage replay, or live fallback
  → canonical slow-authorized tool call/result continuation
  → streaming Fish Audio S2-Pro PCM
```

This is an internal server architecture. It does not add events or fields to the
OpenAI Realtime wire protocol, and it does not alter stable `api/v1`.

The live gateway has two explicit preparation policies. `continuous` is the
reference path above. `endpoint-only` keeps stateful ASR and standard server
VAD but does not allocate a private continuation chain, so partial revisions
cannot invoke fast or slow. The final transcript still becomes the same typed
`asr.endpoint` observation and enters the same canonical fast → slow → tool
result loop. The policy is selected per deployment, never from transcript
content or a model decision.

The persistent gateway path is:

```text
8 kHz G.711 mu-law or 24 kHz PCM Realtime input
  → acoustic VAD with prefix/silence hysteresis
  → persistent Qwen3-ASR with 200 ms provider buffering
  → response-eligible canonical observation
  → Qwen fast spoken micro-turn + unconditional Gemini slow continuation
  → standard function-call events
  → exact complete tool-result batch → slow-only resumption
  → Fish S2-Pro PCM → fixed 100 ms paced Realtime audio frames
```

The relevant packages are:

| Package | Responsibility |
| --- | --- |
| `asrbuffer` | Decouple scheduler cadence from minimum useful provider chunks |
| `adapters/qwenasr` | Stateful Qwen3-ASR start/chunk/finish transport |
| `continuation` | Provider-neutral streamed reasoning/text/call contract and tool authority |
| `trajectory` | Atomic append-only semantic history and proposal/call/result invariants |
| `preparation` | One-active-chain, temporal launch pacing, exact-root and exact-stage pre-endpoint preparation |
| `admission` | Explicit class/cost/deadline GPU admission and cooperative preemption |
| `interleave` | Fast once, slow unconditionally, then slow again after tool results |
| `benchspec` | Exact semantic tool-call multiset and tool-error scoring |
| `adapters/openaicompat` | Local vLLM context and streaming compiler |
| `adapters/gemini` | Gemini reasoning/tool continuation compiler |
| `adapters/openaitts` | OpenAI-compatible streaming speech transport |
| `realtimegateway` | Persistent Realtime session, VAD/media, canonical cognition, tool resumption, and bounded speech |
| `cmd/realtimegateway` | Production composition of local Qwen3-ASR/Qwen/Fish with hosted Gemini slow reasoning |
| `realtimebench` | End-to-end timing, scoring, and secret-free evidence |

## Tool authority

Both models receive the actual tool schema. The authority attached to the
provider descriptor determines the meaning of a native model call:

```text
Qwen tool call event   → tool_proposal → never executable
Gemini tool call event → tool_call     → tool runtime → tool_result
```

No prompt or transcript pattern changes this mapping. A result cannot reference
a proposal, proposal and call IDs cannot collide, and the engine invokes
`ToolSet.Execute` only for slow `tool_call` items. This is why the fast model can
form the right request without becoming a second action authority.

## Revision-safe preparation

Each changed ASR revision may start one private continuation chain. The manager
keeps at most one chain active and coalesces intermediate revisions when
cancellation has not yet reached a provider safe point. Qwen fast output is
atomically appended to that private branch and Gemini slow consumes the exact
resulting prefix, including retained reasoning and the non-executable proposal.
Private output stays outside the canonical trajectory.

At the endpoint, the candidate is accepted only when a SHA-256 fingerprint of
all model-visible semantics exactly equals the final request. The fingerprint
includes model/profile, policy, capability/tool definitions, trajectory
content, and retained provider state. It excludes only operational values that
the model never sees. Acceptance uses no fuzzy text comparison or routing rule.

Commit accepts a completed or in-flight chain and never waits. As the canonical
runner reaches fast and slow, each captured stage is replayed only if its own
complete request fingerprint also matches; otherwise that stage falls back to
the live provider. The private chain has no tool runtime or speech sink, so a
prepared slow call cannot create a side effect. It becomes executable only when
the exact event is replayed and appended canonically. Tool results then resume
the ordinary live slow provider.

The benchmark records observed/distinct revisions, chain disposition, per-stage
timings, exact replay/fallback counts, and provider usage without storing raw
reasoning. This makes canceled hosted slow work visible.

The endpoint-only policy guarantees zero pre-endpoint continuation work by
construction; its aggregate counters therefore contain ordinary post-endpoint
provider work only. Comparing it with `continuous` changes temporal
availability alone; it does not replace the fast model with the slow model or
create a separate agent policy.

### Temporal launch pacing

The reference policy remains unconditional: every completed fast continuation
makes the slow continuation eligible. An optional per-stage minimum start
interval limits how frequently speculative slow requests are actually launched.
This is an operational compute policy, not a third model decision:

- it reads only the stage index, monotonic launch time, configured interval,
  cancellation, and exact-commit signal;
- it never reads the transcript, reasoning, assistant text, tool proposal, or
  model confidence;
- a new semantic revision cancels a paced wait before provider invocation;
- exact final-root commit bypasses any remaining wait immediately; and
- the interval is excluded from the semantic fingerprint because no model sees
  it, while its value and every wait/cancellation/bypass remain in telemetry.

`0s` preserves the immediate reference behavior. A positive interval is a
registered cost-policy ablation. It can reduce discarded hosted work but cannot
change tool authority, exact replay, or the canonical fast→slow trajectory.

## GPU admission

The governor uses abstract capacity 100 with 25 reserved for interactive work.
The original slice used:

- ASR: interactive, cost 25, non-preemptible while updating its stateful
  session.
- Final fast fallback: interactive, cost 75.
- Fast preparation: speculative, cost 75, cooperatively preemptible.
- Local slow model, when configured: background, cost 75, cooperatively
  preemptible.
- TTS: interactive, cost 50.

The strict background trial gave each ASR advance cost 100. This makes
perception exclusive on the local accelerator while it holds its lease;
speculative Qwen can run between advances but cannot contend with ASR. The
choice is runtime provenance, independent of transcript content.

This governor controls request admission; it is not a CUDA kernel scheduler.
Cancellation does not release capacity until the provider returns at a safe
point. Hosted Gemini is outside the local GPU resource domain. Strict priority
protects foreground latency but can starve background work under sustained
interactive saturation; that tradeoff must be measured and, if necessary,
addressed by an explicit fairness/reservation experiment. Fixed-memory
per-class telemetry separates enqueue-to-grant wait, grant-to-release service,
and preemption-to-release acknowledgement delay.

## Reproducing the 96 GB co-located condition

The measured host used one NVIDIA RTX PRO 6000 Blackwell Workstation Edition.
The services occupied approximately 15.9 GiB for S2-Pro, 38.4 GiB for Qwen
fast, and 13.3 GiB for Qwen3-ASR. Exact use depends on runtime versions and
cache settings. Start and health-check the services strictly in the order Fish
→ Qwen fast → ASR; concurrent cold starts can make vLLM profile against
transient free memory and reject its KV-cache allocation.

Start Fish Audio S2-Pro through SGLang-Omni:

```bash
OPENREALTIME_ROOT="$(pwd -P)"
CUDA_HOME="$OPENREALTIME_ROOT/.runtime/sglang-omni/lib/python3.12/site-packages/nvidia/cu13" \
LD_LIBRARY_PATH="$OPENREALTIME_ROOT/.runtime/sglang-omni/lib/python3.12/site-packages/nvidia/cu13/lib:$OPENREALTIME_ROOT/.runtime/sglang-omni/lib/python3.12/site-packages/torch/lib" \
FLASHINFER_WORKSPACE_BASE="$OPENREALTIME_ROOT/.runtime/flashinfer-absolute" \
.runtime/sglang-omni/bin/sgl-omni serve \
  --config deploy/sglang-omni/s2pro-colocated-96gb.yaml \
  --host 127.0.0.1 --port 8081 \
  --model-name fishaudio/s2-pro
```

Start Qwen3-30B-A3B-FP8 with vLLM 0.19.0:

```bash
VLLM_WORKER_MULTIPROC_METHOD=spawn \
.runtime/qwen-asr/bin/python -m vllm.entrypoints.openai.api_server \
  --model Qwen/Qwen3-30B-A3B-FP8 \
  --revision d206ba732169f29bb77fbf80fc2c4b81d4d30782 \
  --served-model-name qwen-fast \
  --host 127.0.0.1 --port 8000 \
  --gpu-memory-utilization 0.38 \
  --max-model-len 40960 \
  --enable-auto-tool-choice \
  --tool-call-parser hermes
```

The 40,960-token limit is the model's native context window, not a prompt
truncation policy. The frozen host reported capacity for 44,896 KV-cache tokens
at the declared 0.38 GPU-memory fraction, so one full-window continuation fits
without increasing the co-located allocation. This preserves the canonical
trajectory and its tool-result dependencies while the hosted slow continuation
retains the larger-context role. Runtime evidence records both the exact model
revision in argv and the vLLM `/version` and `/v1/models` declarations.

The pinned Qwen tokenizer emits JSON inside `<tool_call>` tags, so the
Hermes-compatible vLLM parser matches this checkpoint. The Qwen3-Coder XML
parser expects a different `<function=...>` form. The adapter sends
`enable_thinking=false` for the minimal fast condition.

Start stateful Qwen3-ASR 0.6B:

```bash
VLLM_WORKER_MULTIPROC_METHOD=spawn \
.runtime/qwen-asr/bin/python -m qwen_asr.cli.demo_streaming \
  --asr-model-path Qwen/Qwen3-ASR-0.6B \
  --host 127.0.0.1 --port 8001 \
  --gpu-memory-utilization 0.14 \
  --chunk-size-sec 0.2
```

## Persistent standard Realtime gateway

After all three local services are healthy, start the gateway with a dedicated
loopback credential and the hosted slow-model credential:

```bash
export OPENREALTIME_GATEWAY_TOKEN=local-only-token
export GEMINI_API_KEY=...
go run ./cmd/realtimegateway
```

The default endpoint is `ws://127.0.0.1:8765/v1/realtime`; `/healthz` is the
readiness endpoint. The default production composition uses Qwen3-ASR 0.6B,
local `qwen-fast` with thinking disabled and proposal-only tool authority,
Gemini 3.5 Flash at high effort with execute authority, and local Fish S2-Pro.
Every client and server message is validated against the pinned standard
Realtime schema unless `--validate-wire=false` is explicitly selected for
diagnosis.

The local launcher passes the declared ASR identity, ASR provider chunk, and
slow effort into the gateway. `/healthz` reports those deployment parameters
alongside the fast, slow, and speech descriptors so a benchmark can reject a
stale process before starting. Fixed-cadence experiments set both
`OPENREALTIME_ASR_CHUNK_SECONDS` for the Qwen server and
`OPENREALTIME_ASR_PROVIDER_CHUNK` for the gateway buffer; changing only one is
not the registered treatment. `OPENREALTIME_SLOW_EFFORT` selects the explicit
medium or high slow profile. `OPENREALTIME_SLOW_CONTEXT_POLICY` defaults to
`canonical`; `content-only` and `independent` are registered benchmark
controls whose typed provider projections never fork the canonical store.
`OPENREALTIME_PREPARATION_POLICY` defaults to `continuous`; `endpoint-only`
disables all partial-revision continuation work while retaining the identical
canonical event loop after VAD finalization.
The same health document exposes cumulative session, input-frame, ASR provider
advance, fast/slow continuation, and Fish speech counters. Continuation and
speech aggregates separate completion, failure, and cooperative cancellation;
canonical fast/slow calls and private preparation calls have disjoint
aggregates selected by typed execution provenance;
ASR aggregates separately measure stateful advance and endpoint-finalization
attempts, failures, and elapsed time. The other aggregates record streamed
event/chunk counts, tokens or source samples, and cumulative/maximum provider
and first-event timing. Matrix runners preserve both the start and final health
snapshots, keeping provider work measurable without extending the standard
Realtime event vocabulary.

`OPENREALTIME_ASR_PROVIDER_MAX_CHUNK` enables a bounded revision-adaptive
provider cadence when it exceeds `OPENREALTIME_ASR_PROVIDER_CHUNK`. The next
threshold resets to the minimum after the provider emits a typed revision and
doubles after an advance with no revision, up to the maximum. This controller
does not read transcript content. A zero maximum preserves fixed cadence, and
endpoint finalization always flushes pending audio immediately.

The fast phase has two explicit provider profiles, not a content router. The
default `--fast-provider vllm` selects local Qwen instruct with thinking
disabled. `--fast-provider gemini` selects Gemini 3.5 Flash with minimal
thinking; slow remains Gemini 3.5 Flash at medium or high thinking. Both fast
profiles receive the same policies, capability manifest, and tool schemas with
proposal-only authority. When both phases use the exact same Gemini model, the
slow phase may inherit its authenticated native state. A different Gemini
model receives the portable assistant/proposal trajectory only; signed state
is never replayed across model identities.

The launcher exposes the hosted fast profile without starting the local Qwen
LLM:

```bash
OPENREALTIME_FAST_PROVIDER=gemini \
OPENREALTIME_FAST_MODEL=gemini-3.5-flash \
  scripts/local-cascade.sh start
```

The service launcher also supports `restart-asr` for local diagnosis. A scored
capacity experiment restarts both ASR and the gateway so the adapter descriptor
cannot retain the previous model identity. The first such experiment is frozen
in `benchmarks/tau-voice/asr-ablation-v1.json`:
Qwen3-ASR 0.6B versus the official 1.7B streaming model, with every benchmark
and cognitive variable held fixed. It is a post-baseline ablation, not an
automatic fallback or input-dependent router.

The gateway has no answer/ask/yield/status router. A final ASR observation runs
fast once and slow once. A complete external function-result batch resumes the
slow continuation directly from the exact extended canonical prefix; fast is
not rerun. No process-local agent or advice object owns the resumption. Both
phases see the same session instruction and tool definitions. Fast call-shaped
output is a non-executable proposal; only a newly committed slow call crosses
the WebSocket.

The speech scheduler separates semantic and acoustic commitment:

- fast assistant text is canonical but provisional until played;
- the fast model is instructed to emit one self-contained spoken micro-turn of
  at most twelve words and has a 32-token hard bound;
- arbitrary Fish HTTP fragments are coalesced into fixed 100 ms Realtime audio
  frames and paced at media time, preventing an entire long utterance from
  being deposited in the client buffer;
- a slow assistant or authoritative tool safe point cancels only unplayed fast
  media, while user VAD interrupts all unplayed speech; and
- played text remains visible to later continuations and can only be corrected,
  never erased.

This supersession rule depends only on phase authority and playback state. It
does not inspect transcript/model text or benchmark identity.

The pinned τ audio-native provider suite exercises this endpoint through τ's
ordinary OpenAI adapter. On 2026-08-18 all 12 selected OpenAI cases passed,
including 200 ms timing bounds, multi-turn speech, function-call result
resumption, usage, and barge-in. See the
[τ-Voice integration record](../benchmarks/tau-voice/README.md) for exact
commands and limitations.

Generate the checked input fixture through the same Fish S2-Pro server. The
seed is sent as the provider-specific `seed` request field and recorded in the
generation report:

```bash
go run ./cmd/audiobench \
  --tts-provider openai-speech \
  --tts-url http://127.0.0.1:8081/v1/audio/speech \
  --tts-model fishaudio/s2-pro \
  --tts-text 'Call records dot read with this JSON argument: key equals access underscore phrase. The exact key value has two words, access and phrase, separated by one underscore. Do not add any other word. Return the code exactly.' \
  --tts-deterministic --tts-seed 0 \
  --tts-wav artifacts/realtimebench/opaque-code-explicit-input.wav \
  --tts-case opaque-code-explicit-input-seed-0 \
  --output artifacts/realtimebench/opaque-code-explicit-input-generation.json
```

Two independent executions of that command produced byte-identical WAV files
with SHA-256
`15cb8ab88bd03141d23ae2f8597d315a3963c722576e1e2037c90c5167255162`.
The benchmark report binds its input to that digest; generated audio remains an
ignored local artifact rather than redistributed model output.

Run the integrated tool-grounding trial:

```bash
GEMINI_API_KEY=... go run ./cmd/realtimebench \
  --task-id opaque-code-retrieval \
  --audio artifacts/realtimebench/opaque-code-explicit-input.wav \
  --reference 'Call records dot read with this JSON argument: key equals access underscore phrase. The exact key value has two words, access and phrase, separated by one underscore. Do not add any other word. Return the code exactly.' \
  --asr-url http://127.0.0.1:8001 \
  --asr-frame 50ms --asr-provider-chunk 200ms \
  --prepare-fast=true --prepare-slow=true --gpu-admission=true \
  --slow-preparation-min-interval=1s \
  --gpu-asr-cost=100 --gpu-fast-cost=75 \
  --gpu-capacity=100 --gpu-interactive-reserve=25 \
  --fast-provider vllm --fast-model qwen-fast \
  --fast-base-url http://127.0.0.1:8000/v1 --fast-effort minimal \
  --slow-provider gemini --slow-model gemini-3.5-flash --slow-effort high \
  --tts-url http://127.0.0.1:8081/v1/audio/speech \
  --tts-model fishaudio/s2-pro \
  --runtime 'input_generation=Fish S2-Pro seed 0; explicit JSON argument' \
  --runtime 'local_priority=ASR exclusive interactive; Qwen speculative' \
  --output artifacts/realtimebench/background-current.json
```

The shown `1s` value reproduces the temporal-pacing condition. Set
`--slow-preparation-min-interval=0s` to reproduce the immediate reference
policy; zero is also the default.

Model weights are not redistributed by this repository. Fish S2-Pro's local
model card identifies a Fish Audio Research License and requires a separate
commercial license. Operators must verify the terms of every selected model and
service; see [the artifact policy](../LICENSES.md).

## Exact-scored background result: 2026-08-18

The primary checked-in full-background report is
`realtime-benchmark-v0.6` with scorer `exact-tool-trajectory-v2`:
[`realtime-opaque-code-background-strict-explicit-v06-2026-08-18.json`](../benchmarks/results/realtime-opaque-code-background-strict-explicit-v06-2026-08-18.json).
The current v0.8 schema adds temporal-pacing telemetry; it does not change the
canonical chain or scorer used by this earlier report.

| Measure | Observed |
| --- | ---: |
| Input duration | 16,079.8 ms |
| Scheduler opportunities | 322 at 50 ms |
| Actual stateful ASR advances | 81 including finalization |
| ASR revisions / distinct preparation inputs | 45 / 44 |
| First non-empty ASR partial | 432.0 ms wall time |
| End of input to final ASR | 35.2 ms |
| Normalized WER | 0.0000 |
| ASR provider-boundary service RTF | 0.189 |
| Background chains started/completed/superseded/failed | 44 / 1 / 42 / 1 |
| Coalesced duplicate revisions | 1 |
| Final prepared fast first event / duration | 121.2 / 121.3 ms |
| Final prepared slow first event / duration | 1,282.8 / 1,288.2 ms |
| Final slow start relative to endpoint | −120.7 ms |
| Exact prepared stages replayed / live fallback | 2 / 0 |
| Endpoint to committed fast first event / safe point | 0.0 / 0.0 ms |
| Fast tool proposals | 1 exact `records.read({"key":"access_phrase"})` |
| Executed tool calls | 1 independently authorized exact call |
| Tool results / tool errors | 1 / 0 (`cerulean-17`) |
| Post-result slow continuation | 1,059.9 ms; 1,037.4 ms to first event |
| Endpoint to first answer audio | 3,175.0 ms |
| TTS first PCM / RTF | 947.2 ms / 0.511 |
| Admission leases admitted/released | 127 / 127 |
| Final exact task score | Pass |

“Provider service” is elapsed time at the provider boundary and includes local
admission, transport, server queueing, and model work. It is deliberately not
labeled GPU kernel time. Zero endpoint-to-fast means the matching candidate was
already complete; Qwen still took 121.3 ms from invocation to safe point. The
prepared Gemini stage began 120.7 ms before the audio endpoint, so only that
portion of its 1.288 s request was hidden on this utterance.

The task's scoring contract is embedded in the report. It requires exactly one
`records.read` call with canonical JSON `{"key":"access_phrase"}`, rejects
extra or missing calls, and fails any tool error. Proposal, call, and result IDs
remain distinct and causal. The private prepared call had no tool runtime; the
external lookup happened only after exact replay into the canonical runner.

The 42 superseded and one failed chain are important negative cost evidence.
Immediate fast→slow continuation follows the simple reference semantics, but a
hosted slow request frequently begins just before the next ASR revision cancels
it. Temporal pacing is now an implemented content-independent ablation.
Provider prefix reuse or learned compute admission remain future conditions;
any such condition must be registered as a cost policy and cannot use
hand-written transcript patterns.

### One-second slow-launch pacing ablation

The v0.8 report
[`realtime-opaque-code-background-paced-1s-strict-explicit-v08-2026-08-18.json`](../benchmarks/results/realtime-opaque-code-background-paced-1s-strict-explicit-v08-2026-08-18.json)
uses the same deterministic audio hash, models, 50/200 ms cadence, exclusive
ASR admission, high slow reasoning, tool data, and exact v2 scorer as the
unpaced full-background result. It changes only the slow preparation minimum
start interval from `0s` to `1s`:

| Measure | Unpaced reference | 1 s paced |
| --- | ---: | ---: |
| ASR revisions / distinct inputs | 45 / 44 | 44 / 43 |
| Slow preparation provider launches | 43 | 12 |
| Paced waits / cancelled before start / commit bypasses | 0 / 0 / 0 | 15 / 8 / 1 |
| Exact stages replayed / fallback | 2 / 0 | 2 / 0 |
| Exact proposals / calls / results | 1 / 1 / 1 | 1 / 1 / 1 |
| Endpoint→fast safe point | 0.0 ms | 0.0 ms |
| Endpoint→first correct audio | 3,175.0 ms | 3,537.7 ms |
| Exact task score | Pass | Pass |

The paced condition launched 31 fewer speculative Gemini requests, a 72%
reduction from 43 to 12 in this trial. The matching final slow stage became
ready at 15,998.9 ms, was nominally eligible at 16,196.4 ms, and exact commit
bypassed the remaining interval at 16,144.7 ms. Thus pacing did not hold the
canonical response behind its cost budget. Eight obsolete slow stages were
cancelled before provider invocation.

This is one sequential sample, not a cost or latency distribution. Its 362.8 ms
higher endpoint-to-audio value cannot be attributed to pacing because hosted
Gemini and TTS latency were not controlled or repeated. The defensible result
is the mechanism-level launch reduction with unchanged exact action/result
quality and an observed commit bypass. Confirmatory work requires randomized,
repeated intervals with provider request/token accounting.

### Same-fixture exploratory controls

Two warm local-service controls used the same audio hash, models, ASR cadence,
exclusive ASR admission, high slow reasoning, tool data, and v2 exact scorer.
They are one sequential sample per condition—not a randomized latency study:

| Condition | First slow start vs endpoint | First slow event vs endpoint | Endpoint→fast safe point | Endpoint→first correct audio | Score |
| --- | ---: | ---: | ---: | ---: | --- |
| Endpointed v0.7 | +184.8 ms | +2,056.1 ms | 184.8 ms | 4,336.4 ms | Pass |
| Fast-only preparation v0.7 | +200.3 ms | +1,464.7 ms | 200.2 ms | 3,365.2 ms | Pass |
| Full fast→slow preparation v0.6 | −120.7 ms | +1,162.2 ms | 0.0 ms | 3,175.0 ms | Pass |

The source reports are
[`endpointed`](../benchmarks/results/realtime-opaque-code-endpointed-warm-strict-explicit-v07-2026-08-18.json),
[`fast-only`](../benchmarks/results/realtime-opaque-code-fast-prepared-warm-strict-explicit-v07-2026-08-18.json),
and [`full background`](../benchmarks/results/realtime-opaque-code-background-strict-explicit-v06-2026-08-18.json).

The defensible mechanism result is that full preparation moved the final fast
safe point before endpoint and started the accepted slow invocation 120.7 ms
before endpoint. The whole-run audio differences also contain substantial
Gemini sampling and service variance. Fast-only provides a useful
counterexample: its exact candidate was accepted, but the matching revision
arrived late enough that its safe point occurred 200.2 ms after endpoint—worse
than the endpointed fast safe point in this sample. Acceptance is correctness,
not a promise of latency savings.

The earlier fast-only `realtime-benchmark-v0.3` report remains available:
[`realtime-opaque-code-qwen3-asr-qwen3-30b-gemini-3.5-flash-s2pro-prepared-2026-08-18.json`](../benchmarks/results/realtime-opaque-code-qwen3-asr-qwen3-30b-gemini-3.5-flash-s2pro-prepared-2026-08-18.json).
It had 3,547.2 ms endpoint-to-answer audio and exactly one correct call on
inspection, but its legacy scorer required only the tool name. It is not used
as exact-scored evidence and is not directly comparable because the fixture and
preparation condition differ.

This exploratory trial validates wiring, continuity, authority, and exact
scoring—not a latency superiority claim. Non-tool reflex tasks, endpointed
controls, other cadences, native systems, repeated distributions,
interruption, cost-policy ablations, and human judgments remain required.

## Preserved negative evidence

The earlier report
[`realtime-active-seat-qwen3-asr-qwen3-30b-gemini-3.5-flash-s2pro-2026-08-18.json`](../benchmarks/results/realtime-active-seat-qwen3-asr-qwen3-30b-gemini-3.5-flash-s2pro-2026-08-18.json)
failed. TTS/ASR changed `route_north` and `route_south` into `root_north` and
`root_south`; the old fast configuration invented values, and slow repeatedly
guessed identifier variants after authoritative tool errors. That failure
motivated the proposal/execute split and an unambiguous spoken identifier in
the integration check. It remains evidence that ASR fidelity, literal
identifier handling, and grounded tool authority are joint correctness
requirements—not details that a latency result can ignore.

The first full-background report,
[`realtime-opaque-code-qwen3-asr-qwen3-30b-gemini-3.5-flash-s2pro-background-2026-08-18.json`](../benchmarks/results/realtime-opaque-code-qwen3-asr-qwen3-30b-gemini-3.5-flash-s2pro-background-2026-08-18.json),
committed and replayed both stages, but Gemini first called the pluralized
`access_phrases`, received an error, then corrected to `access_phrase`. Its v0.4
name-only scorer labeled the run passed. That is a preserved scorer false
positive: successful recovery does not erase an extra external action or tool
failure.

The first strict v0.5 rerun,
[`realtime-opaque-code-qwen3-asr-qwen3-30b-gemini-3.5-flash-s2pro-background-strict-2026-08-18.json`](../benchmarks/results/realtime-opaque-code-qwen3-asr-qwen3-30b-gemini-3.5-flash-s2pro-background-strict-2026-08-18.json),
correctly failed. Under co-located load, ASR collapsed parts of the spoken
identifier; Qwen proposed `KeyAccess`, and Gemini tried six wrong key variants.
All six tool errors were scored, and the slow invocation safety bound stopped
the loop. This shows why exact arguments, error-free action trajectories, and a
finite resource guard are necessary.

## Current limitations

- Pre-endpoint preparation is text-only; speculative TTS is not yet part of
  this measured path.
- Slow reasoning now runs privately before endpoint, but tool effects and
  speech remain endpoint-gated. The runner does not yet canonicalize a declared
  stable partial and permit its effects while the utterance continues.
- Immediate slow preparation discarded 42 superseded chains and recorded one
  failed chain in the primary passing run. A one-second temporal-pacing
  ablation reduced slow launches from 43 to 12 once, but interval sweeps,
  repetitions, token/currency cost, and prefix reuse remain unmeasured.
- The Qwen-ASR demo uses a development Flask server; a production deployment
  needs an equivalent hardened service without changing the adapter contract.
- The governor records logical admission, not device utilization or memory
  bandwidth. GPU telemetry still needs aligned sampling.
- One successful tool trial cannot establish P50/P95 behavior or parity with
  native realtime/interaction models.
