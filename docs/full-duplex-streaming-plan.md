# Streaming and full-duplex components: survey and implementation plan

**Research cutoff:** 2026-09-22. **Status:** execution in progress on 2026-09-22; what was built, measured, blocked, and deferred is recorded in [Streaming and full-duplex integration: results](full-duplex-results.md). Scope correction (2026-09-23): the user has never trained micro-turn LLM or TTS checkpoints. The original user-model assumption is withdrawn; C2 is not applicable and released-model integration remains the execution scope.

**Plan revision:** incorporates the expanded open-model survey. This document is the authoritative implementation roadmap; [Open models beyond ASR and TTS](open-duplex-models-survey.md) retains detailed candidate analysis and release evidence. Its proposed ordering is consolidated into the stages below.

**Repository inspected:** `c715c6701f376ad608e92fff632b628349a73377`. Sources are linked next to their claims. Public documentation and released artifacts establish candidate capabilities, not measured performance in OpenRealtime. This is a prioritized survey, not an exhaustive model directory.

## Recommendation

OpenRealtime should support three complementary paths through its existing typed graph:

1. **Composable streaming cascade:** streaming ASR → timed text/revisions → a micro-turn language model → incremental text-input/audio-output TTS. This is the first priority, using released models and their documented native protocols.
2. **Integrated speech models:** native audio-in/audio-out sessions, optionally exposing text and interaction acts. Preserve their coupled codecs and generation schedules inside a graph element unless the model explicitly supports decomposition.
3. **Closed live services:** support both independently selectable ASR/TTS APIs and integrated live agents, with explicit control ownership and actual provider semantics.

The main missing abstraction is **stateful, concurrent input and output with explicit revision, timing, cancellation, and playback semantics**. Adding more providers to the existing complete-text TTS and immutable-request LLM interfaces would leave that problem unresolved.

There are now relevant **text-based full-duplex LLMs**: DuplexCascade releases a fine-tuned Qwen2-7B-Instruct component for text micro-turn interaction. Most other well-known full-duplex models are speech language models, not interchangeable text-only components. Ordinary token-streaming chat APIs are still useful as an explicitly orchestrated baseline. They do not by themselves provide new-input admission during the same generation. See the [DuplexCascade code](https://github.com/sbintuitions/DuplexCascade) and [gated model card](https://huggingface.co/sbintuitions/DuplexCascade).

Across these three paths, support independently selected **interaction prediction, speaker/overlap perception, acoustic preprocessing, and background audio understanding**. Translation and speech-plus-action belong to separate task profiles. They must not silently alter the evidence or authority of an otherwise identical cascade.

**First deliverable:** a measured 500 ms micro-turn pipeline using released LLM/TTS assets, plus a native speech/tool reference and one controlled endpoint-evidence comparison. Preserve Qwen3-ASR and Fish S2 Pro as baselines. Add Voxtral or Nemotron streaming ASR first, then the other as a substitution; use Kyutai for faithful DuplexCascade reproduction and CosyVoice for multilingual incremental synthesis. This is an integration priority, not a quality ranking.

### Integration priorities

| Priority | Candidates / capability | Reason and boundary |
| --- | --- | --- |
| **Core contracts and released models** | Timed revisions, 500 ms clock, live LLM, incremental TTS, playback reconciliation | Enable actual composition; preserve each released model's native protocol |
| **First supporting comparison** | Smart Turn plus a transcript endpoint baseline | Test acoustic versus textual completion evidence without replacing the language model |
| **First new native reference** | NemotronLabs VoiceChat 11B | Exercise continuous speech and tool-result admission in the existing agent runtime |
| **Reference reproductions** | DuplexCascade; existing Moshi/PersonaPlex, MiniCPM-o, and closed live paths | Establish faithful model/runtime behavior before substitutions |
| **Next interaction/native candidates** | DualTurn with VAP baseline; Lychee-FD | Anticipatory overlap evidence and separate dialogue-control computation |
| **Perception extensions** | Streaming Sortformer + multitalker Parakeet; one audio-understanding observer; acoustic preprocessing | Improve speaker attribution and non-text evidence with explicit delay and compute budgets |
| **Later task profiles** | ELLSA, Freeze-Omni, Hibiki/SeamlessStreaming | Speech/action, frozen-backbone, and translation-specific research |
| **Deferred** | DuplexOmni deployment; SALMONN-omni and OmniFlatten deployment | Hardware scale or matching release assets remain unresolved; retain architectural references |

Candidate mechanisms, public assets, and limitations are sourced in the [expanded survey](open-duplex-models-survey.md). Select a small number of representatives per stage; comprehensive compatibility does not require implementing every candidate before the first release.

## 1. Scope and verified inventory

The task authorized repository inspection, current research, a plan, and read-only server discovery. Implementation, installations, model downloads, paid API requests, and GPU inference are deferred.

| Item | Observed on 2026-09-22 |
| --- | --- |
| Local repository | `/Users/boj/OpenRealtime`; clean before document changes |
| Full inspected commit | `c715c6701f376ad608e92fff632b628349a73377` |
| GPU host | SSH alias `rtx-pro`; hostname `ubuntu`; `x86_64` |
| GPU | NVIDIA RTX PRO 6000 Blackwell Workstation Edition |
| Reported GPU memory | 97,887 MiB; approximately 96 GB class |
| Driver | `595.91.07` |
| Remote repository | `/home/ubuntu/OpenRealtime`; same HEAD as local; clean |
| Work actually executed remotely | Host, GPU/driver, and Git inventory only |
| Not established | Available free VRAM, concurrent workload budget, runtime compatibility, model quality/latency, API entitlements |

All future model execution in this plan belongs on **`rtx-pro`**, including Python inference, serving engine validation, co-location, and performance measurements. The Mac can edit documentation or act as a remote client; its inference performance must not enter the result table.

## 2. Define capabilities before comparing models

“Streaming” and “full duplex” describe several independent properties:

| Capability | What must actually happen | What does not establish it |
| --- | --- | --- |
| Streaming ASR input | Process audio before the utterance ends | Uploading a finished recording |
| Incremental transcript output | Emit hypotheses during incoming speech | Splitting a completed transcript into tokens |
| Stable transcript prefix | Explicitly commit a prefix, or declare a stability policy | A partial that happened not to change yet |
| Streaming LLM output | Emit tokens before response completion | Concurrent input admission |
| Micro-turn LLM interaction | Consume timed incoming updates and choose waiting, speaking, backchanneling, or yielding | A chat prompt containing timestamps alone |
| Native live model session | Admit input during ongoing output under the model's supported schedule | Canceling one ordinary chat request and starting another |
| Streaming TTS output | Produce playable audio before synthesis completes | Incremental text input |
| Incremental TTS input | Accept further text while an active synthesis context produces audio | Repeated sentence-sized complete-text requests |
| Full-duplex interaction | Continue listening, reason about overlap, and change speaking behavior while output is playing | Merely using a bidirectional WebSocket |

An ASR can serve a full-duplex system without itself being a conversation model. Deepgram streaming ASR belongs here; Flux adds learned turn signals, which remain distinct from an LLM consuming new input while speaking. Similarly, standalone “full-duplex TTS” usually means incremental synthesis plus interruption handling; audio-conditioned listening and speaking generally belongs to an integrated speech model.

Keep these properties orthogonal to model size, language, transport, and interaction ownership. This follows [ADR-0011](adr/0011-capabilities-not-model-species.md), the [evidence vector decision](adr/0013-interaction-evidence-is-a-capability-vector.md), and [controller arbitration](adr/0014-controller-composition-requires-arbitration.md).

## 3. Repository audit: retain the foundations, extend the interfaces

| Existing surface | Finding | Consequence for this plan |
| --- | --- | --- |
| [Graph architecture](architecture.md), [graph assembly](graph-native-assembly.md), [catalog](../architecture/catalog.json) | Versioned topology, descriptors, deployment profiles, and exact selected evidence already exist | Add graph elements and catalog revisions; avoid a new hardcoded binding for every combination |
| [Sidecar protocol v4](sidecar-protocol-4.md) | Typed ports, identity, timing envelopes, causal relationships, cancellation, and bounded communication | Extend typed payload contracts; reuse the envelope and conformance machinery |
| [Component API](../api/v1/api.go), `PerceptionRevision` | Stable/unstable text, revision ID, source sample, delta, final flag | Useful starting point; needs explicit patch semantics, alignment quality, and stability provenance for timed model input |
| [Qwen ASR adapter](../adapters/qwenasr/qwenasr.go) | Stateful chunk API; nonfinal transcripts are reported as unstable, stable only at finalization | Do not assume this adapter currently supplies an early committed prefix |
| [Deepgram adapter](../adapters/deepgram/) | Nova and Flux streaming already exist | Audit semantic fidelity and benchmark them; do not describe Flux ASR as a new adapter to build from scratch |
| [Cascade audio path](../binding/cascade/audio.go) | Revision policies, stable-partial admission, speculative preparation, and superseding cancellation already exist | Reuse these as a fallback; measure their behavior separately from native micro-turn execution |
| [Audio observation](../perception/audio.go) | Cadenced observation, energy admission, and utterance lifecycle | Native clocked models need a path preserving silence and continuous audio; absence of transcript is not proof of silence |
| [Continuation provider](../continuation/continuation.go) | `Continue` receives a request with an immutable trajectory snapshot and emits deltas | The ordinary interface does not admit new input into that invocation; add a separate live-session capability |
| [Speech API](../api/v1/api.go), `StreamingSpeechProvider` | `Stream` receives a complete `SpeechPlan.Text` and streams audio | Output streaming exists; same-context incremental text input needs a new contract |
| [Sentence TTS adapter](../adapters/bysentence/bysentence.go) | Splits supplied text into smaller requests | Retain as a compatibility path, with buffering and prosody costs recorded |
| [Moshi sidecar](../sidecars/moshi_sidecar.py) | Existing reference integration and mock path | Verify real checkpoint/API behavior, transcript speaker attribution, and input injection; advertised flags are not hardware evidence |
| [MiniCPM-o sidecar](../sidecars/minicpm_o_sidecar.py) | Current path uses audio prefill and explicit response triggering | Audit official duplex execution separately; the current path does not establish native full duplex |
| [Qwen3-Omni sidecar](../sidecars/qwen3_omni_sidecar.py) | Accumulates input and calls generation under engine floor control | Keep as an integrated, turn-controlled comparison; streaming output does not justify relabeling it native duplex |
| [GPT-Live integration](gptlive-integration.md), [provider inventory](providers.md) | Several closed live providers already have adapters | Extend and validate existing integrations before adding duplicate abstractions |
| [Spoken tracking](../spoken/), [spoken boundary](spoken-boundary.md) | Playback reconciliation distinguishes heard from generated speech | Make this the authority for cancellation and future conversational history |
| [Benchmark guide](benchmarks.md), [architecture experiments](architecture-experiments.md) | Existing scenarios, external suites, traces, and controlled-comparison rules | Extend these rather than build an unrelated benchmark runner |

The repository is already a suitable orchestration and experiment platform. The highest-value work is making stream semantics precise and testing actual implementations against them.

## 4. Streaming ASR survey

The architectural distinction matters: cache-aware transducers, chunked non-autoregressive recognizers, causal audio/text decoders, and prefix-revising encoder–LLM systems can all expose a “stream,” with different compute, delay, and revision behavior.

| Candidate | Architecture / stream behavior | Availability and OpenRealtime action |
| --- | --- | --- |
| **Qwen3-ASR 0.6B / 1.7B** | Audio encoder plus language decoder; official streaming implementation maintains accumulated audio and rolls back a text prefix before decoding updates. The paper's chunk/rollback settings are not an append-only guarantee | Existing adapter baseline. Measure revisions and duration-dependent cost. [Paper](https://arxiv.org/html/2601.21337v1), [inference source](https://github.com/QwenLM/Qwen3-ASR/blob/main/qwen_asr/inference/qwen3_asr.py) |
| **Voxtral Mini 4B Realtime** | Causal audio encoder with autoregressive text stream and configurable transcription delay; 80 ms frame structure, recommended 480 ms delay | Public Apache-2.0 checkpoint. High-priority native streaming contrast. Frame size is not measured word latency. [Paper](https://arxiv.org/html/2602.11298v1), [model](https://huggingface.co/mistralai/Voxtral-Mini-4B-Realtime-2602) |
| **Kyutai STT / delayed streams modeling** | Aligns delayed text with audio-codec frames, including padding/word markers; released variants have different delay/language tradeoffs | Reproduce the 1B English/French, roughly 500 ms-delay path for DuplexCascade; the 2.6B English path has a different delay budget. [Code](https://github.com/kyutai-labs/delayed-streams-modeling), [paper](https://arxiv.org/html/2509.08753v1) |
| **Nemotron Speech Streaming / Nemotron 3.5 ASR 0.6B** | Cache-aware FastConformer RNN-T; configurable chunks from 80 to 1120 ms in the multilingual card | Strong efficiency-oriented candidate. The card separates 32 out-of-box locales from 8 requiring adaptation; Mandarin is in broad coverage. Do not transfer H100 throughput to this server. [Multilingual model and streaming examples](https://huggingface.co/nvidia/nemotron-3.5-asr-streaming-0.6b), [English model](https://huggingface.co/nvidia/nemotron-speech-streaming-en-0.6b) |
| **FunASR streaming Paraformer** | Chunked encoder, CIF token emission, non-autoregressive decoding with caches and lookahead | Useful mature Chinese baseline. For example, chunk and lookahead configuration contribute separately to latency. [Examples](https://github.com/modelscope/FunASR/blob/main/examples/industrial_data_pretraining/paraformer/README.md), [model source](https://github.com/modelscope/FunASR/blob/main/funasr/models/paraformer_streaming/model.py) |
| **VibeVoice-ASR-Streaming** | Structured streaming recognition with speaker attribution and hotwords; released September 2026 | Add to the speaker-aware evaluation queue, behind simpler first integrations. Validate chunk delay and overlap attribution independently. [Official repository and release links](https://github.com/microsoft/VibeVoice) |
| **Deepgram Nova / Flux** | Persistent streaming service; provisional/final text and, for Flux, semantic turn lifecycle events | Already integrated. Test text stability separately from end-of-turn and resumed-turn behavior. [Interim results](https://developers.deepgram.com/docs/interim-results), [Flux state](https://developers.deepgram.com/docs/flux/state) |
| **ElevenLabs Scribe realtime** | Partial versus committed transcripts; manual or VAD commitment | Candidate streaming adapter; an existing batch provider is not this protocol. [Commit strategies](https://elevenlabs.io/docs/eleven-api/guides/how-to/speech-to-text/realtime/transcripts-and-commit-strategies) |
| **AssemblyAI streaming** | Word finality, turn completion, and formatting are distinct events | Useful stability-contract comparison. Pin API/model generation rather than reuse assumptions across Universal Streaming releases. [Message sequence](https://www.assemblyai.com/docs/streaming/message-sequence) |
| **Gemini live transcription** | Dedicated live transcription service exposed separately from conversational audio generation | Secondary closed API candidate; validate availability and transcript semantics before catalog inclusion. [Official guide](https://ai.google.dev/gemini-api/docs/live-api/live-transcribe) |

Also evaluate learned interaction evidence as an independent component:

- **X2-Turn** combines streaming ASR with fine-grained turn-state prediction. Its shared backbone makes it an ASR-plus-interaction candidate, not merely an interchangeable endpoint detector. [Code](https://github.com/X-Square-Robot/X2-Turn), [August 2026 paper](https://arxiv.org/abs/2608.10878).
- **SoulX-Duplug** is a text-guided streaming semantic VAD/interaction module using external transcription. Keep its observations and controller authority separate from the ASR's. [Code](https://github.com/Soul-AILab/SoulX-Duplug), [paper](https://arxiv.org/abs/2603.14877).

For every ASR, report both **recognition quality** and **when useful evidence became available**. A delayed stable word can be preferable for one task and worse for interruption. Low final WER alone cannot select the best source for a 500 ms micro-turn model.

## 5. Full-duplex LLM and integrated speech-model survey

### Independently composable language model

**DuplexCascade is the clearest first reference for the user's design.** Its text-trained LLM uses conversational control tokens in a cascaded, VAD-free micro-turn system. The public inference repository launches ASR, LLM, and TTS independently, prescribing Kyutai STT/TTS. The model card identifies the fine-tuned Qwen2-7B-Instruct backbone and MIT license, but checkpoint access requires accepting a gate. Reproduce its own schedule and token grammar before changing components; do not assume its micro-turn duration is the user's 500 ms. [Reference implementation](https://github.com/sbintuitions/DuplexCascade), [checkpoint](https://huggingface.co/sbintuitions/DuplexCascade), [paper](https://arxiv.org/abs/2603.09180).

For each released micro-turn LLM, record: tokenizer/control-token IDs; micro-turn duration and alignment origin; silence versus no-new-text convention; revision handling seen during training; input admission schedule; output act grammar; state reset; tool-result injection; and whether input/output are logically concurrent or serialized within clock steps. A general chat-completion wrapper may erase the training-time protocol.

An ordinary text LLM remains a valuable baseline with cancellation, prefix caching, and repeated continuation. Name that execution mode **orchestrated micro-turns**. Native micro-turn support means preserving the model's actual state and schedule; it does not require simultaneous GPU kernels, but does require correct admission while the session is producing output.

### Integrated models and research

| Candidate | Role and release evidence | Recommendation |
| --- | --- | --- |
| **Moshi** | Public speech model with parallel audio streams and text generation. [Code](https://github.com/kyutai-labs/moshi) | Validate existing sidecar, then use as a native audio baseline. Confirm whether exposed text describes assistant speech or user speech |
| **PersonaPlex** | Moshi-derived 7B full-duplex speech model with role/voice conditioning; code and weights have different licenses. [Official repository](https://github.com/NVIDIA/personaplex) | High-priority native comparison after Moshi conformance; retain its coupled speech stack |
| **MiniCPM-o 4.5** | Official duplex audio/video demo path and research release. [Paper](https://arxiv.org/abs/2604.27393), [demo repository](https://github.com/OpenBMB/MiniCPM-o-Demo) | Audit the actual duplex API, not only the existing response-triggered adapter; useful for later visual/full-duplex comparison |
| **BayLing-Duplex** | Interleaves incoming audio, outgoing text/audio, and control tokens; public repository and checkpoint listing. [Code](https://github.com/BayLing-Models/BayLing-Duplex), [model files](https://huggingface.co/BayLing-Models/BayLing-Duplex/tree/main), [paper](https://arxiv.org/abs/2606.14528) | Second wave. Verify downloadable weights, inherited licenses, and continuous live inference; an offline generation example is insufficient |
| **SyncLLM** | Time-synchronized speech-unit dialogue modeling. [Project](https://syncllm.cs.washington.edu/), [paper](https://aclanthology.org/2024.emnlp-main.1192.pdf) | Architectural reference, not a verified drop-in text LLM or deployment artifact in this survey |
| **LSLM** | Listening-while-speaking speech-model research. [Paper](https://arxiv.org/abs/2408.02622) | Inform concurrent conditioning experiments; release/runtime readiness requires a separate audit |
| **DuplexSLA** | Synchronized speech, language, and action modeling. Repository still marks inference/checkpoints as forthcoming at inspection. [Code](https://github.com/hyzhang24/DuplexSLA), [paper](https://arxiv.org/abs/2605.20755) | Research watchlist; do not schedule as immediately runnable |
| **Parallel transcription head research** | September 2026 proposal to add transcription to a duplex speech model. [Paper](https://arxiv.org/abs/2609.15759) | Track for observability/user-transcript improvements; an intended release is not a released adapter |

### Closed integrated services

Keep existing `openai`, `openai-live`, `google`, `xai`, Azure, and Qwen provider paths visible in the inventory, with per-model capability evidence. Their presence does not establish identical live semantics or current account availability; consult the [repository provider reference](providers.md).

OpenAI's **Live** contract is particularly relevant: continuous audio and transcript streams, with a different lifecycle from turn-oriented Realtime and delegated Responses tasks. OpenRealtime already has a corresponding integration. Test its continuous stream behavior directly instead of forcing artificial response boundaries onto it. [Official migration guide](https://developers.openai.com/api/docs/guides/live-migration).

Google's **Live API** exposes bidirectional realtime input and interruption signaling. Treat its native session as one element and verify playback reconciliation and text attribution through the adapter. [Official API](https://ai.google.dev/api/live).

Managed agent products can be represented as an **opaque external agent**, if wanted, but must own their orchestration explicitly. They are not arbitrary ASR/LLM/TTS replacements while OpenRealtime also independently controls the same conversation. Preserve the project's existing single-owner/arbitration rules.

## 6. Incremental TTS survey

The first acceptance test is simple: submit an unfinished text prefix, observe generated audio, then append more text **to that same active context**. A `stream=true` option or HTTP chunked response does not establish this property.

| Candidate | Relevant capability | Recommendation / limitation |
| --- | --- | --- |
| **CosyVoice 3 / 2** | Officially supports text-in and audio-out streaming; public inference and model releases | First multilingual local candidate; validate the chosen serving engine preserves generator input. Published minimum latency is not a server measurement. [Official repository](https://github.com/QwenAudio/CosyVoice) |
| **Kyutai TTS** | Delayed-stream modeling designed for incremental text conditioning | First English/French research reproduction with DuplexCascade. Preserve its lookahead/action protocol. [Code](https://github.com/kyutai-labs/delayed-streams-modeling), [paper](https://arxiv.org/html/2509.08753v1) |
| **VibeVoice-Realtime 0.5B** | Released model explicitly supporting streaming text input | Lightweight additional candidate; use the realtime release, not the disabled older long-form TTS implementation. [Official repository](https://github.com/microsoft/VibeVoice) |
| **Qwen3-TTS** | Streaming-oriented text/audio architecture and released inference stack | Test exact input-stream support of the selected runtime; audio chunks alone do not satisfy the requirement. [Code](https://github.com/QwenLM/Qwen3-TTS), [paper](https://arxiv.org/abs/2601.15621) |
| **Fish S2 Pro** | Existing local synthesis baseline with serving infrastructure in this repository | Retain for controlled comparisons; classify the adapter's actual text-input granularity. Pin engine and checkpoint license separately. [Code](https://github.com/fishaudio/fish-speech), [model](https://huggingface.co/fishaudio/s2-pro) |
| **Voxtral TTS** | Open-weight synthesis candidate with a noncommercial weight license | Optional research comparison; not an unrestricted default deployment. Validate exact stream interface. [Release](https://mistral.ai/news/voxtral-tts/) |
| **Deepgram Aura WebSocket** | Incremental `Speak`, `Flush`, and `Clear` | A distinct integration from the repository's HTTP speech output path. [Protocol](https://developers.deepgram.com/reference/text-to-speech/speak-streaming) |
| **Deepgram Flux TTS** | `/v2/speak`: incremental text, turn completion, interruption with playback position, cross-turn state | High-priority closed comparison. Preserve provider lifecycle instead of mapping it blindly to Aura semantics. [Client messages](https://developers.deepgram.com/docs/flux-tts/client-messages) |
| **Cartesia WebSocket** | Continued synthesis contexts and flush boundaries | Candidate same-context streaming adapter; record context lifetime and cancellation limits. [Contexts](https://docs.cartesia.ai/use-the-api/tts-websocket/contexts), [flush semantics](https://docs.cartesia.ai/api-reference/tts/working-with-web-sockets/context-flushing-and-flush-i-ds) |
| **ElevenLabs WebSocket** | Incremental text and multiple contexts; separate text-to-dialogue API family | Support exact endpoint/model contracts; do not assume all voices/models share one WebSocket protocol. [Multi-context TTS](https://elevenlabs.io/docs/eleven-api/guides/how-to/websockets/multi-context-web-socket), [TTS versus TTD](https://elevenlabs.io/docs/eleven-api/guides/how-to/websockets/tts-vs-ttd-websockets) |

Serving engines are another compatibility dimension. A checkpoint may support incremental synthesis while an engine endpoint accepts only complete text. Record engine commit and endpoint behavior; use [vLLM-Omni speech API documentation](https://github.com/vllm-project/vllm-omni/blob/main/docs/serving/speech_api.md) and [SGLang-Omni TTS documentation](https://github.com/sgl-project/sglang-omni/blob/main/docs/basic_usage/tts.md) as starting points, not capability guarantees for every supported model.

## 7. Proposed contracts and composition rules

These are proposed semantics, not new public Go API names. Specify them through versioned graph port types first, then decide which deserve a stable component API extension. Preserve existing v1 providers through explicit compatibility elements; do not silently change their meaning.

### 7.1 Transcript revisions and time

Extend the current perception representation with:

- A stream ID, revision ancestry, and an unambiguous replace/append range. Define offsets as Unicode code points or another explicit unit; provider token IDs are not portable text offsets.
- Committed and provisional regions. Record whether commitment is provider-guaranteed, decoder-policy-derived, or locally heuristic. Surface unexpected committed-prefix corrections as protocol violations or explicit repair events.
- Source-audio intervals and alignment quality per span, when available. An audio-consumption frontier is not an exact word timestamp.
- Capture time, producer emission time when available, receiver time, and clock-domain metadata. Reuse the sidecar envelope rather than duplicating clocks inconsistently.
- Separate lexical finality, utterance boundary, formatting revision, and interaction intent. None implies all the others.

Do not concatenate partial transcripts as independent new text. For example, a provisional recognition later repaired from “背景” to “北京” must replace or explicitly correct the earlier observation, rather than appear as two sequential statements. If the LLM already consumed it, choose an attested policy: stable-prefix admission, explicit correction tokens, or replay/recompute. Arbitrary past KV-cache mutation is not a generic capability.

### 7.2 A real 500 ms micro-turn clock

Use 500 ms as the user's initial profile, not as a universal port constant. Maintain a monotonic session clock and emit input opportunities even when no new words arrive. Preserve at least these distinct states: observed silence, speech with no decoded text yet, no new transcript, and unavailable/stalled input.

Each tick carries new observations, revisions, source intervals, and readiness metadata; it can contain zero, one, or several words. If a recognizer delivers old audio's transcript late, retain its source time but admit it at the current tick. Do not backdate model knowledge.

A 500 ms clock adds up to almost 500 ms of scheduling wait on top of ASR evidence delay, depending on phase. Measure that wait separately. The output text duration is also not fixed by the number of characters in a micro-turn: synthesis and playback have their own clocks.

### 7.3 Live language-model sessions

Add an opt-in stateful session with input events and asynchronous output events. The contract must expose input admission/consumption watermarks, output epochs, control acts, bounded queues, cancellation, reset, and supported injection points for tool results or instructions.

Expose execution mode explicitly: native clocked state, supported interleaved inference, or cancel/restart orchestration. A transport accepting a message does not prove the model conditioned on it. Where consumption cannot be observed, record it as unknown and use behavioral tests.

Tool execution remains behind existing authority boundaries. A provisional hypothesis may justify speculative reasoning, but must not independently commit an irreversible external action. Record whether a later correction canceled reasoning, pending speech, or an unexecuted tool proposal.

### 7.4 Incremental synthesis sessions

Represent opening a synthesis context, appending text, optional nonterminal flushing, ending input, canceling an epoch, and closing a session as separate operations. A provider that only supports terminal flushing must advertise that restriction. Maintain accepted-text, generated-audio, queued-audio, and played-audio positions separately.

For example, Flux TTS `Flush` ends a turn; later text starts another turn. Its interrupt offset is session-relative, and in-flight frames must be discarded around the acknowledgment boundary. Map that through OpenRealtime's playback tracker, stopping local playback immediately rather than waiting for a network round trip. [Provider semantics](https://developers.deepgram.com/docs/flux-tts/client-messages).

Cancellation must invalidate queued and late audio for the old generation epoch at the output boundary. Resumption starts from the actual heard state. A provider's generated-text alignment is evidence for reconciliation, not proof that the listener heard those words.

### 7.5 Capability negotiation and honest compatibility

Descriptors should declare supported input/output stream modes, text revision handling, time resolution, alignment reliability, sample formats, language/voice coverage, cancellation granularity, state persistence, and model/engine identities. Keep availability, selected evidence, and control ownership separate.

The planner can insert resampling, revision stabilization, clocking, sentence buffering, or cancel/restart adapters **only when their semantic loss is exposed**. Reject combinations that require unavailable behavior. A compatibility report should explain, for example, “this TTS waits for a complete segment” or “this LLM cannot consume an ASR repair without restart.”

Latent/codebook connections need encoder and decoder checkpoint identities, tokenizer/codebook definitions, tensor shape/dtype, frame rate, normalization, and training compatibility. Equal dimensions do not make two independently trained representations interchangeable. Keep unsupported fused models intact.

### 7.6 Interaction ownership

Select who controls endpointing, taking/yielding the floor, overlap, and response veto. Flux events, X2-Turn, semantic VAD, the micro-turn LLM, and native speech models must not all issue competing actions implicitly. Extend the existing arbitration vocabulary only with explicit jurisdiction and fallback rules; attest those choices in the graph/catalog fingerprint.

Keep continuous microphone capture, echo handling, speaker evidence, model input, and playback distinct. An assistant hearing its own loudspeaker is a different problem from a model handling a real user interruption.

### 7.7 Interaction forecasts and speaker evidence

Add typed observations for endpoint probability, future speech activity, backchannel likelihood, and hold/yield proposals. Include subject/speaker, prediction horizon, calibration/model identity, source-audio interval, arrival time, and expiration. Smart Turn, transcript classifiers, VAP, and DualTurn predict different quantities; adapters must not relabel them all as a universal end-of-turn score.

Speaker observations need simultaneous activity tracks, session-local IDs, capacity limits, uncertainty, and attribution revisions. Accept delayed corrections without rewriting what the LLM knew at an earlier tick. Sortformer's documented low-latency setting buffers about 1.04 seconds before computation, so speaker attribution must not automatically block the 500 ms interaction path. See the [speaker-perception survey](open-duplex-models-survey.md#3-speaker-identity-overlapping-speech-and-the-audio-front-end).

Introduce predictors in observation-only mode. Promote them to control only through a versioned arbitration policy with measured premature endpoints, missed interruptions, and false stops. Keep prediction horizons distinct from any future context used to derive offline training labels; inference must remain causal.

### 7.8 Playback-reference audio and asynchronous perception

Expose a clock-aligned playback-reference stream, including discontinuities and uncertainty about device delay. Distinguish generated PCM, transmitted PCM, and rendered PCM. Dual-channel interaction models and echo cancellation need the appropriate reference; feeding future synthesized speech as if already heard leaks unavailable information into decisions.

Acoustic preprocessing must declare sample formats, algorithmic delay, state/reset behavior, and raw-versus-processed provenance. Preserve a bypass for controlled comparisons. Noise suppression, echo cancellation, speaker diarization, and source separation are different capabilities.

Background audio-understanding observations carry a bounded source window, task/schema, completion time, uncertainty, and expiration. Start with one observer from the expanded shortlist. Schedule it outside the 500 ms critical path, discard stale results, and measure whether it provides useful evidence without starving ASR/LLM/TTS. Inferred emotion is a hypothesis, not a verified fact.

### 7.9 Native tools, actions, and translation

Map a native model's tool channel into existing proposal, authorization, execution, result, and cancellation boundaries. Record the input revision and output epoch that caused the proposal. Test delayed results, duplicate proposals, and user correction while a tool is pending. A native tool-call format must not bypass exactly-once execution or imply that an already completed action can be undone by canceling speech.

Translation profiles require source/target languages, a source-consumption frontier, translated-text revisions, and output alignment/lag. Speech/action profiles require a task-specific action schema and execution receipts. ELLSA robot tokens are not automatically desktop tool calls; begin with simulation. Keep these task profiles separate from conversational endpoint evaluation.

### 7.10 Serving-runtime compatibility gate

Before writing a model-specific scheduler, audit whether a released serving engine already exposes the needed live contract. Pin the engine revision, model revision, deployment mode, and endpoint together. Require handshake capabilities plus behavioral evidence of new input affecting active output; successful connection alone is insufficient.

The expanded survey found that vLLM-Omni's unified duplex endpoint documentation currently names MiniCPM-o, while a separate VoiceChat recipe describes an experimental older path. Resolve this per pinned runtime instead of treating support as a model-wide boolean. See the [runtime findings and primary sources](open-duplex-models-survey.md#6-serving-runtime-findings-that-change-the-plan). Keep engine scheduling inside the serving process and OpenRealtime's authority/playback/provenance in the graph runtime.

## 8. Text versus latent: preserve both as testable options

There is no universal theorem that a finite trained latent-based system must outperform a text cascade. With raw audio `X`, transcript `T=f(X)`, target `Y`, and the same conditioning context, data processing gives `I(Y;T) <= I(Y;X)`. An unconstrained predictor with access to `X` can emulate the transcript pipeline, so its ideal minimum risk cannot be worse. Strict improvement requires task-relevant information lost by `T`. A learned bottleneck `Z=g(X)` can itself lose information, and finite compute, data, optimization, and latency change the practical comparison.

Timed text may be enough for many semantic tasks. It cannot generally distinguish two acoustically different utterances with the same words when prosody changes the correct action. Homophone tolerance also does not imply robustness to names, numbers, negation, or requests whose meaning changes after a revision.

Therefore retain three experimental representations:

1. Transcript only, with explicit streaming revisions.
2. Timed text plus selected acoustic evidence: silence, speech activity, speaker, prosody or other declared features.
3. Audio/latent input to a compatible speech model or a specifically trained adapter.

Compare them with matched backbones/training wherever possible. Native omni versus a different text LLM is a **system comparison**, not proof that latent representation caused the result. For perception-rich visual tasks, ordinary captions may discard more spatial/detail information; that does not make every trained VLM superior on every downstream task either.

## 9. Implementation sequence and acceptance gates

All stages below are future work. Preserve existing working paths while adding independent capability-driven elements. The stages define dependency and acceptance gates, not a requirement to implement every surveyed model serially.

| Stage | Concrete deliverable / likely repository surfaces | Exit evidence |
| --- | --- | --- |
| **P0: freeze baselines and feasibility** | Capability ledger in `docs/providers.md`; pinned manifests; RTX Pro availability/runtime audit; released-model protocol inventory | Retained ordinary cascade and entitled live-provider baseline traces; exact assets and execution route identified for each first-wave candidate |
| **P1: streaming and evidence contracts** | Versioned graph ports and sidecar v4 conformance; live component extensions; interaction forecasts, playback-reference and speaker/observer schemas; new catalog definitions | Fixtures for revisions, late input, empty ticks, queue pressure, stale observations/audio, tool duplication, and causal channel alignment; existing API behavior preserved |
| **P2: ASR evidence** | Audit Qwen/Deepgram semantics; first Voxtral or Nemotron streaming element, then the other; timed-transcript/clock element | Realtime-paced multilingual audio, revision/stability traces, and evidence-lag distributions; no unmarked partial concatenation |
| **P3: incremental TTS** | Same-context synthesis interface; Kyutai/CosyVoice reference path; one closed WebSocket TTS path; existing Fish compatibility path | Audio begins before input ends, continuation stays in context, and cancel/restart reconciles actual playback; no stale-epoch audio after local cutoff |
| **P4: micro-turn LLM** | User model adapter and DuplexCascade reference adapter; explicit ordinary-LLM fallback; tool-result admission | Reproduce reference protocol first; input arrives during output and causes measured changes without an artificial user endpoint; 500 ms profile exercised |
| **P5: native speech, tools, and live APIs** | VoiceChat first new native/tool reference; validate Moshi/PersonaPlex and MiniCPM-o; refresh existing closed integrations; Lychee-FD after runtime feasibility | Input affects ongoing output; delayed tool results and user corrections reconcile correctly; playback state, control delay, and long-session memory measured |
| **P6: interaction prediction** | Smart Turn plus transcript endpoint baseline; VAP/DualTurn next; compare existing X2-Turn/SoulX-Duplug candidates where feasible | Observation-only traces first; then one explicit controller policy; matched endpoint/overlap metrics and causal played-audio evidence |
| **P7: richer perception** | Sortformer/multitalker ASR; one bounded audio-understanding observer; optional noise/AEC profiles | Speaker-attributed overlap quality, observation lag, preprocessing effects, and interference with critical-path deadlines measured |
| **P8: task extensions** | ELLSA simulation/speech, Freeze-Omni comparison, Hibiki/Seamless translation; offline data/evaluation adapters | Task-specific contracts and metrics; no implicit promotion to generic duplex support |
| **P9: comparative release** | Supported compatibility matrix, complete benchmark artifacts, reproducible deployment recipes, research limitations | Publish only tested profiles with clear semantic losses and Pareto tradeoffs; unsupported/deferred candidates remain explicit |

**Dependencies:** P2 and P3 can proceed independently after the relevant P1 contracts. P4's DuplexCascade reproduction needs the prescribed Kyutai stack; the user's checkpoint does not depend on access to DuplexCascade. P5 needs P0 feasibility and P1 live/tool contracts, not completion of every cascade adapter. P6's initial endpoint comparison can run on the existing cascade after P1; dual-channel predictors additionally require a verified playback-reference stream. P7 and P8 are optional extensions, not blockers for the first P9 release.

**First release scope:** one working micro-turn cascade, one native speech/tool profile, and one matched endpoint-evidence comparison, each with complete traces and playback/cancellation validation. Use released LLM/TTS assets for the cascade and identify each reference profile and its protocol faithfully. C2 is not applicable; no user checkpoints are expected. A closed API profile is conditional on account access, not a mandatory dependency. Later releases expand the tested matrix rather than weakening the gates to claim broad support.

**Defer explicitly:** DuplexOmni's published multi-GPU recommendation is outside the first single-server profile; SALMONN-omni/OmniFlatten need matching runnable assets before deployment work. Lychee-FD's patched older runtime requires Blackwell validation. Gate failures become documented unavailable profiles with a reason, while independent stages continue.

### Initial experiment cells

| Cell | Composition | Question |
| --- | --- | --- |
| C0 | Existing Qwen ASR → ordinary continuation LLM → existing Fish path | What does the current system achieve under the new instrumentation? |
| C1 | Kyutai STT → DuplexCascade → Kyutai TTS | Can the released reference be reproduced faithfully? |
| C2 | Not applicable: no user-trained checkpoints exist | Original assumption withdrawn after user clarification on 2026-09-23; not a pending dependency |
| C3 | Hold the released-reference LLM/TTS fixed; substitute Voxtral, Nemotron, or Deepgram individually | Which recognition/revision tradeoff helps this same LLM? |
| C4 | Hold ASR/LLM fixed; substitute CosyVoice, Fish compatibility, or closed incremental TTS | What is gained by same-context incremental synthesis? |
| N0 | Moshi / PersonaPlex, each as its native integrated system | Native interaction reference and instruction-following tradeoff |
| N1 | MiniCPM-o official duplex path | Audio first; visual evidence as a separately labeled treatment |
| N2 | VoiceChat native session with the same tool schemas as a modular baseline | Do tool requests/results remain correct during speech, correction, and interruption? |
| N3 | Lychee-FD native deployment | Does the separate control path remain responsive under synthesis load? |
| I0 | Fixed cascade; Smart Turn versus transcript endpoint classifier versus calibrated fusion | What do acoustic and semantic completion evidence each contribute? |
| I1 | Fixed cascade; current overlap policy versus VAP/DualTurn-informed policy | Can we distinguish acknowledgments from requests to take the floor? |
| S0 | Mixed-audio recognition versus Sortformer plus multitalker recognition | Does speaker-attributed quality justify added delay and per-speaker compute? |
| A0 | Fixed cascade with/without one bounded audio observer or preprocessing stage, one change at a time | Does extra acoustic information help without missed micro-turn deadlines? |
| X0 | ELLSA speech/action simulation or Hibiki/Seamless translation, separately scored | Can task-specific concurrent input/output be represented faithfully? |
| R0 | Existing OpenAI Live / Gemini Live and other entitled live APIs | Closed system reference with network/cost recorded |

Run feasible cells, not an indiscriminate Cartesian product. Keep language and voice coverage visible. An English-only reference is not a Chinese quality baseline. If access to a checkpoint/API is unavailable, retain an explicit blocked cell and proceed with the other cells.

## 10. RTX Pro validation protocol

### Environment and execution

1. On `rtx-pro`, create an isolated worktree at the selected implementation commit. Read current GPU utilization/processes and free memory before allocating; the inventory above established capacity, not availability. Do not replace a running service as part of a benchmark.
2. Pin model revisions, tokenizer/codec assets, engine commits, dependency locks or image digests, precision, CUDA/PyTorch versions, attention backend, chunk sizes, lookahead, and decoding settings. Check code, weight, voice-asset, and dataset licenses separately. Record gates and account access without embedding secrets in artifacts.
3. Validate Blackwell/SM120 kernel support for each engine in isolation. The existing [96 GB SGLang/Fish profile](../deploy/sglang-omni/s2pro-colocated-96gb.yaml) documents useful prior choices and CUDA/FlashInfer concerns; it is not a universal memory or compatibility guarantee.
4. Start with one stream and one model, then test complete co-located pipelines. Measure weights, KV cache, encoder/codec buffers, and peak allocated/reserved VRAM. Set admission limits from measured peaks with headroom, not parameter counts alone.
5. Test cold start and warmed steady state separately. Increase concurrency through 1, 2, 4, and higher only while memory and deadlines permit. Capture contention between ASR, LLM, and TTS; isolated real-time factors do not prove a co-located pipeline meets deadlines.
6. Replay fixtures at wall-clock audio speed, including silence and overlapping speech. Keep throughput-only, faster-than-real-time decoding as a separately labeled experiment.
7. Store configuration, stderr, machine inventory, raw events, source/playback waveforms, exact evaluated population, and aggregate results with checksums. Keep remote test artifacts under a dedicated run directory; reference them from the repository's existing measurement/benchmark records.

Use server-local loopback for compute comparisons and an actual client for network/playout behavior. Do not subtract unsynchronized host wall clocks; establish clock mappings or use same-clock intervals, and record synchronization uncertainty. Closed API inference runs remotely by definition; drive and instrument those clients on `rtx-pro`, reporting network effects separately.

Run CPU-oriented interaction baselines on `rtx-pro` as well, recording CPU load and thread settings. Test large native models in isolation before any co-location. For multi-stream models, inventory every codec/vocoder process and GPU allocation. Small parameter count does not excuse missing end-to-end measurements; auxiliary observers can still compete for CPU, memory bandwidth, or GPU scheduling.

### Measurements

| Layer | Required observations |
| --- | --- |
| ASR | WER/CER, names/numbers/negation accuracy, first provisional lag, committed-word lag, revision count/depth, alignment error, reset cost, long-session drift |
| Micro-turn admission | Capture-to-evidence lag, evidence-to-tick wait, input acceptance versus consumption, missed 500 ms deadlines, silence/stall distinction, correction recovery |
| LLM | Time to useful response, instruction/task quality, wait/backchannel/yield decisions, response to new input during output, tool correctness, state growth |
| TTS | First playable audio from a fixed usable text prefix, sustained real-time factor, underruns, input buffering, prosody seams, intelligibility, voice consistency |
| Interruption | User onset to decision, decision to local playback stop, acknowledgment lag, stale frames discarded, actually heard text, false interruptions |
| Interaction prediction | Premature endpoint rate, unnecessary waiting, hold/yield/backchannel errors, calibration, forecast horizon, input causality, and late-observation rate |
| Speaker/overlap perception | Speaker-attributed WER or cpWER, diarization error with declared collar/overlap policy, ID consistency, attribution lag, active-speaker capacity and cost |
| Acoustic processing / observers | Algorithmic delay, quiet-speech and backchannel preservation, echo/double-talk behavior, observation usefulness/expiry, and downstream deadline impact |
| Native tools/actions | Duplicate execution, wrong or stale arguments, result-admission delay, correction recovery, and consistency between spoken claims and execution receipts |
| Translation profiles | Translation quality versus source-to-target lag, alignment, omission/revision behavior, and sustained simultaneous operation |
| Whole system | Task success, inappropriate overlap, missed backchannels, recovery from corrections, peak VRAM, utilization, cost per audio minute, long-session stability |

Model frame size, network packet size, word emission interval, and end-to-end latency are different measurements. Also distinguish “barge-in detected” from “assistant stopped audibly.” For TTS, use a held-back text suffix to prove incremental input rather than accidentally testing a complete prompt.

### Fixtures and comparison discipline

Extend the existing twelve scenarios and the repository's full-duplex suites using the [benchmark guide](benchmarks.md). Add targeted fixtures for Chinese homophone repair, corrections involving negation/numbers, an unfinished clause, backchannels that should not interrupt, genuine interruption that should, long silence, speech without a decoded word, network jitter, disconnect/reconnect, rapid cancel-and-resume, and a tool result arriving during speech.

Use clean loopback and real speaker/microphone conditions as separate populations. Include same-word/different-prosody pairs and competing speakers to test information beyond transcripts. Evaluate TTS with an independent recognizer and listening judgments, not the same model used to produce the input transcript.

Add two- and four-speaker overlap, quiet acknowledgments during assistant playback, echo with simultaneous near-end speech, delayed speaker reassignment, expired background observations, and delayed tool results after a user correction. Playback-reference tests must include queued-but-unplayed speech and device discontinuities. Keep translation and simulated-action fixtures in separate task families. Offline data tools such as Sommelier can prepare fixtures, but preserve original timing and processing provenance and check the rights of each source dataset.

For controlled comparisons, hold prompts, input evidence, voice where possible, model versions, policy, hardware, and fixture timing fixed. Repeat stochastic runs and report denominators and uncertainty. Tiny smoke suites can establish operation, not a reliable p95 or quality ranking. Native-model versus cascade results remain system comparisons unless the changed causal factor is isolated.

### Initial engineering targets, not claimed results

- Zero accepted audio from an invalidated generation epoch after the local output cutoff; separately measure audio already irrevocably submitted to the device.
- No unbounded input/output queues or silently dropped transcript repairs in stress fixtures.
- Report the fraction of micro-turn deadlines met at 500 ms; establish an SLA only after baseline measurements and the user's quality/latency priorities are known.
- Sustain TTS faster than playback under co-location; an initial engineering target is RTF below 0.7 for headroom, with underruns measured directly.
- Investigate a local decision-to-playback-stop budget of 200 ms or less, explicitly excluding the model's time to decide whether overlap is an interruption. Calibrate against actual client audio buffers.
- Publish latency–quality–cost–concurrency frontiers instead of claiming one universal best combination.

## 11. Release evidence and remaining decisions

Keep the existing provider proof levels (`documented`, `reachable`, `live-turn`) and add orthogonal evidence for streaming contracts: conformance fixtures, incremental input, concurrent input/output, cancellation/playback, and measured benchmark runs. A model card, a sidecar flag, and a mock test establish different things. None should silently promote a provider to “verified full duplex.”

Implementation uses released checkpoints and their native inference/training protocols. No user-trained checkpoints exist or are required. Further choices include primary languages, deployment/commercial constraints, preferred voices, quality/latency priorities, and allowed concurrency on the shared GPU. These do not block this survey or the protocol design, but they determine the concrete deployment profiles.

Track work at the granularity of **model + checkpoint revision + serving runtime + graph profile**, with fields for public assets, license/dependency checks, hardware feasibility, contract conformance, live behavior, benchmark evidence, and next blocking condition. Inference release, training release, and data release are separate statuses. Native speech/tool models and small observers use the same promotion discipline. A failed or unavailable candidate must not be presented as implemented merely because its adapter or deployment recipe exists.

External links in this document were consulted for the September 2026 survey. Before implementation, pin the exact upstream revisions and archive a capability manifest for each selected provider; moving documentation and model cards are not reproducibility records. No best-performing combination is asserted until the planned RTX Pro experiments produce evidence.
