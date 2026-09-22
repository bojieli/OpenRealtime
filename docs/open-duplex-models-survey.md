# Open models beyond streaming ASR and incremental TTS

**Survey date:** 2026-09-22. **Status:** research and integration recommendations; no new inference runs, installations, or implementation. Companion to the [streaming/full-duplex plan](full-duplex-streaming-plan.md).

## Recommendation

Expand OpenRealtime's candidate registry beyond ASR, LLM, and TTS into five independently selectable roles: native live speech agents, interaction prediction, speaker/overlap perception, acoustic preprocessing, and asynchronous audio understanding. Add translation and action-producing models as distinct experimental tasks.

The most valuable new candidates are **NemotronLabs VoiceChat 11B**, **Lychee-FD**, **Smart Turn**, **DualTurn/VAP**, and **Streaming Sortformer with multitalker recognition**. They add capabilities missing from a plain recognition/synthesis catalog. **ELLSA** is particularly relevant to later multimodal/action experiments. Preserve the previous priorities for DuplexCascade, the user's own micro-turn LLM/TTS, PersonaPlex, and MiniCPM-o.

These are integration priorities based on architectural value and release evidence, not a benchmark ranking. “Public” below means an official repository or checkpoint listing was inspected; it does not mean weights were downloaded, licenses fully audited, dependencies resolved, or the model run on our RTX Pro.

## 1. Native dialogue and multimodal models

| Candidate | Architectural contribution | Release evidence | Integration decision |
| --- | --- | --- | --- |
| **NemotronLabs VoiceChat 11B** | Full-duplex audio understanding/generation with a separate tool-call channel | Public NVIDIA checkpoint, NeMo inference code, interactive deployment documentation | **First wave:** strongest new fit for an agent runtime with tools |
| **Lychee-FD** | Shared lower layers; specialized upper semantic, acoustic, and dialogue-control streams | Public weights, online serving stack, training code | **First wave:** native multi-stream architectural contrast |
| **Freeze-Omni** | Streaming speech interfaces around a frozen text LLM; learned dialogue state | Public inference, demo, and weights | **Second wave:** useful intelligence-retention and state-control baseline |
| **ELLSA** | Joint streaming vision, speech, text, and action; modality-specific experts with shared attention | Public checkpoint and evaluation code; training still marked forthcoming | **Second wave:** speech first, simulated action later |
| **DuplexOmni** | Fast audiovisual interaction plus pluggable slower reasoning/tool processing | Public code, Thinker/Talker weights, dataset metadata | **Deferred deployment:** interesting architecture, substantially larger published hardware recommendation |
| **SALMONN-omni** | Codec-free interface to the LLM token space, dynamic listening/speaking transitions | Papers and demos verified; a complete matching deployable release was not established | **Research reference:** do not label the whole SALMONN family a released omni implementation |
| **OmniFlatten** | Flattens interleaved speech/text streams into a common autoregressive sequence | Paper and official demo verified; matching runnable checkpoint not established | **Research reference:** relevant to training and serialization |

### NemotronLabs VoiceChat: prioritize tools during continuous speech

The [NVIDIA model card](https://huggingface.co/nvidia/NVIDIA-NemotronLabs-VoiceChat-11B) describes an 11B English model using a FastConformer input path, Nemotron Nano V2 language backbone, speech decoder, and a distinct function channel. It explicitly supports speech during tool execution. The card currently labels weights `openmdw-1.1`; pin the actual license file and revision.

Expose it initially as one native live-session element, with audio, attributed text, tool proposals, and lifecycle events. Its internal module split does not establish that an arbitrary external ASR or TTS can be substituted. Test a delayed tool result, a correction while waiting, and an interruption during the response; require exactly-once tool execution and correct recovery of conversational state.

The [NeMo model implementation](https://github.com/NVIDIA-NeMo/Speech/blob/main/nemo/collections/speechlm2/models/nemotron_voicechat.py) is an evaluation/inference wrapper joining duplex speech-to-text and speech-generation components. It is not itself proof of a production live server. The [NeMo release notes](https://github.com/NVIDIA-NeMo/Speech/releases) distinguish training the underlying components from the joint wrapper.

Candidate serving routes include the [vLLM-Omni recipe](https://github.com/vllm-project/vllm-omni/blob/main/recipes/NVIDIA/NemotronLabs-VoiceChat.md) and [NVIDIA NeMo-Speech.cpp server](https://github.com/NVIDIA/NeMo-Speech.cpp/blob/main/docs/server.md). First reproduce one pinned route; avoid changing model, precision, runtime, and interruption policy simultaneously. Their audio formats differ, so derive sample rate and framing from the selected route rather than the model name.

### Lychee-FD: separate the control deadline from speech generation

[Lychee-FD](https://github.com/HITsz-TMG/Lychee-FD) separates upper-layer semantic, acoustic, and control computation after shared lower layers. Its control path can exit early. This is valuable for testing whether interruption decisions can remain responsive when speech generation is busy. [Weights](https://huggingface.co/HIT-TMG/Lychee-FD) and a Step-Audio-2-mini Token2Wav dependency are identified by the authors.

The release includes training and online serving code, but uses patched vLLM and a pinned older PyTorch stack. Its default demo places the backend and vocoder on different GPUs, with a documented single-GPU configuration. Therefore single-RTX-Pro deployment is a feasibility experiment, especially for Blackwell kernels and combined KV/vocoder memory, not a guaranteed turnkey install. Pin Apache-2.0 project code and dependency licenses independently.

Wrap the model and its required vocoder as one deployment initially. Measure control-event delay under load separately from first audio, and preserve model control events as proposals to OpenRealtime's selected authority. Do not infer live correctness from an offline waveform.

### Freeze-Omni: a useful bridge to the user's approach

[Freeze-Omni](https://github.com/VITA-MLLM/Freeze-Omni) publishes inference, demo, and model assets for streaming speech interfaces around a frozen text backbone. Its [paper](https://arxiv.org/abs/2411.00774) describes learned dialogue states and duplex behavior. This makes it a useful comparison to micro-turn text fine-tuning: how much interaction behavior can be learned while preserving the language backbone?

Audit the actual listening/speaking process topology, cache ownership, and state transitions. Start with its prescribed components; a frozen backbone does not make its speech interfaces universally compatible with any LLM. It is a second-wave comparison because the newer native candidates add more immediately distinctive coverage.

### ELLSA: relevant to acting while speaking

[ELLSA's official branch](https://github.com/bytedance/SALMONN/tree/ELLSA) and [model card](https://huggingface.co/tsinghua-ee/ELLSA) provide speech-interaction and LIBERO evaluation paths, with dependencies including Llama, vision assets, and CosyVoice. Training remains marked forthcoming. The card describes shared attention with modality-specific experts and concurrent perception/action/speech.

This could exercise OpenRealtime's action boundaries: continue explaining while acting, accept a corrective instruction, and interrupt a pending action. Begin with speech-only evaluation and simulated actions. Robot action tokens are not interchangeable with desktop tools; mapping them requires an explicit task adapter. Check complete asset availability and licensing rather than treating the top-level Apache label as covering every dependency.

### DuplexOmni: preserve the idea, defer the expensive reproduction

[DuplexOmni](https://github.com/MuyeHuang/DuplexOmni) separates fast audiovisual interaction from a slower reasoning/tool layer and releases training, data preparation, and serving code. Its [checkpoint card](https://huggingface.co/MuyeHuang/DuplexOmni/blob/main/README.md) recommends at least **eight H20 GPUs** for low-latency deployment at its current optimization level, and lists unexpected silence and speech-quality issues.

That is not a proof that a smaller configuration is impossible, but it is enough to exclude it from the initial single-RTX-Pro promise. Review its asynchronous evidence/result interfaces now; defer full reproduction until a supported smaller deployment or additional hardware is available. Its training-data release is metadata plus a sample shard, not a complete downloaded training corpus.

### Research with valuable ideas but unverified deployment assets

**SALMONN-omni** studies embedding-mediated speech interfaces without audio-codec tokens in the LLM vocabulary and learned transitions between listening and speaking. “Codec-free” here does not mean that all audio representations are lossless or that no audio decoder is needed. The [2025 paper](https://arxiv.org/abs/2505.17060) is the architectural source. The [current SALMONN repository](https://github.com/bytedance/SALMONN) directs users to several distinct branches, including ELLSA; those releases do not by themselves verify a matching SALMONN-omni checkpoint. Keep it research-only until exact assets are located.

**OmniFlatten** demonstrates a serialization/training approach for interleaved text and audio using progressive training. Its [paper](https://arxiv.org/abs/2410.17799) and [official project](https://omniflatten.github.io/) are useful for the user's micro-turn training protocol. This survey did not establish a complete public serving release. Do not substitute an unrelated generic GPT repository as its implementation.

## 2. Interaction models: higher value than another ASR wrapper

Endpoint detection, future speech prediction, and interruption intent are different tasks. Represent their outputs as typed evidence before enabling them as controllers.

| Model | Input and output | Why integrate | Main limitation |
| --- | --- | --- | --- |
| **Smart Turn v3.2** | Recent PCM audio → probability a turn is complete | Small, practical acoustic endpoint baseline; tests the value of prosody beyond transcripts | Designed around VAD-triggered evaluation, not a complete overlap policy |
| **LiveKit Turn Detector** | Transcript/history → semantic completion probability | Text-only comparator for the same endpoint task | Cannot recover missing prosody; model-specific license |
| **VAP** | Conversational audio → future voice-activity probabilities | Anticipatory floor-taking and backchannel evidence | Activity prediction is not proof of semantic intent |
| **DualTurn** | Dual-channel audio → turn/activity/backchannel signals | More explicit interaction vocabulary for modular agents | Needs correct channel alignment and causal inference validation |
| **X2-Turn / SoulX-Duplug** | Streaming speech and/or text → interaction states | Already in the first survey; include in a shared interaction comparison | Different evidence and ownership must remain visible |

### Smart Turn and LiveKit: a clean text-versus-acoustics experiment

[Smart Turn v3.2](https://github.com/pipecat-ai/smart-turn) publishes code, training data, and weights under a BSD-2-Clause project license. Its Whisper-Tiny-based endpoint classifier consumes 16 kHz audio, with up to eight seconds of recent context, normally after VAD detects silence. Public CPU/GPU variants make it an inexpensive first integration. An eight-second maximum context is not an eight-second mandatory delay.

The [LiveKit model card](https://huggingface.co/livekit/turn-detector) describes a transcript-based endpoint classifier, with its own model license and upstream-STT dependence. Use it as a semantic comparison, checking exact model version and text preprocessing.

Hold VAD timing, fixtures, downstream LLM/TTS, and action policy fixed. Compare acoustic-only, text-only, and explicitly calibrated fusion. Report premature endpoints and excessive waiting separately. Do not combine two scores by an arbitrary threshold and label the result a universally better controller.

### VAP and DualTurn: listen to both sides of the conversation

[Voice Activity Projection](https://github.com/ErikEkstedt/VoiceActivityProjection) supplies an established research basis for predicting future conversational activity from audio. Use it as a probability-producing observer first, then evaluate an explicit policy for holding/yielding the floor or backchanneling. Pin the selected implementation/checkpoint rather than assuming all VAP variants have the same input contract.

[DualTurn](https://github.com/anyreachai/dualturn) uses a Qwen2.5-0.5B backbone with Mimi features and per-channel heads for turn completion, holding, beginning, backchannels, current activity, and future activity. Public [checkpoints](https://huggingface.co/anyreach-ai/dualturn-qwen2.5-mimi-0.5B) are linked. The project is MIT, while its training/evaluation datasets include gated or separately licensed material.

Provide separate aligned user and assistant audio channels. For online decisions, assistant audio should reflect what was played, not speculative future synthesis. Training labels can use future events; inference inputs cannot. Audit feature windows and caching for causality, then test frame-by-frame behavior under delayed playout. Published offline action-prediction scores are not measured live interruption performance.

## 3. Speaker identity, overlapping speech, and the audio front end

These components address a failure that better language modeling alone cannot fix: recognizing the wrong speaker, treating loudspeaker echo as an interruption, or losing the quieter of two simultaneous speakers.

| Component | Role | Integration and evaluation |
| --- | --- | --- |
| **Streaming Sortformer 4-speaker v2.1** | Online speaker activity and diarization | Speaker evidence producer; preserve uncertainty, speaker-ID changes, and observation lag |
| **Multitalker Parakeet Streaming 0.6B** | Speaker-conditioned recognition from a mixture plus diarization | Compose with a diarizer; compare speaker-attributed transcription under overlap |
| **DeepFilterNet** | Low-latency noise suppression | Optional preprocessing branch with bypass; test quiet speech and backchannel preservation |
| **AEC Challenge resources** | Echo-cancellation research, data, and evaluation | Reproduce acoustic double-talk tests; not a single drop-in dialogue model |

[Sortformer's model card](https://huggingface.co/nvidia/diar_streaming_sortformer_4spk-v2.1) lists a low-latency configuration with **1.04 seconds of input buffering**, excluding computation. This is useful for speaker tracking, but already exceeds two 500 ms micro-turns. Treat attribution as potentially late and revisable; it must not silently delay the entire fast interruption path. Its four-speaker capacity must also be explicit.

[Multitalker Parakeet](https://huggingface.co/nvidia/multitalker-parakeet-streaming-0.6b-v1) uses external diarization activity and per-speaker recognition instances. It is not a diarizer or a waveform-separation model. Its resource use grows with active speakers; benchmark the combined pipeline's latency and speaker-attributed error, not just isolated ASR WER.

[DeepFilterNet](https://github.com/Rikorose/DeepFilterNet) is a noise-suppression candidate, not an acoustic echo canceller. For echo, use the [Microsoft AEC Challenge](https://github.com/microsoft/AEC-Challenge) resources to design reproducible near-end/far-end/double-talk tests and compare an explicit AEC implementation. Preserve access to raw and processed audio, report algorithmic delay, and use the actual playback-reference signal. A native model's echo-handling paper result does not establish robustness in the deployed room.

## 4. Audio understanding and expressive conversation

Some useful open models do not establish native full duplex. They can still serve as background observers, semantic-quality baselines, or training references.

| Candidate | Useful role | Release/streaming qualification |
| --- | --- | --- |
| [**Kimi-Audio**](https://github.com/MoonshotAI/Kimi-Audio) | Audio understanding, speech conversation, quality comparison | Public model/code; treat as a bounded audio request until concurrent-input behavior is verified |
| [**Audio Flamingo 3**](https://github.com/NVIDIA/audio-flamingo) | Sound/music/speech reasoning; asynchronous scene understanding | Public model family; a chat TTS module does not establish native duplex input |
| [**Fun-Audio-Chat**](https://github.com/QwenAudio/Fun-Audio-Chat) | Low-latency speech-language interaction comparison | Public code/models; exact live-input semantics require runtime inspection |
| [**MiMo-Audio**](https://github.com/XiaomiMiMo/MiMo-Audio) | Audio-language and few-shot transfer experiments | Public code/models; do not infer full duplex from audio generation |
| [**OpenS2S**](https://github.com/CASIA-LM/OpenS2S) | Empathetic speech and hidden-state-conditioned synthesis | Public training/inference paths and checkpoint link; streaming speech decoder alone is not continuous listening |

Initially select **one** of Kimi-Audio or Audio Flamingo as an observer on bounded windows. Emit timestamped, expiring hypotheses such as a sound event or a prosodic cue. Do not place expensive windowed inference on every 500 ms critical path, and do not treat inferred emotion as ground truth. The graph should record which raw audio the observation describes and when the controller received it.

OpenS2S is useful for testing hidden-state-conditioned synthesis versus plain text. Its audio encoder, language backbone, and decoder are trained dependencies: connecting a new encoder to the same tensor-shaped port does not preserve meaning. Fun-Audio-Chat and MiMo-Audio broaden semantic/speech-quality baselines, but add them after the native and interaction candidates above unless their language/voice coverage is required.

## 5. Simultaneous translation, data, and evaluation

### Translation is a separate concurrency task

[Hibiki](https://github.com/kyutai-labs/hibiki) reuses Moshi's multi-stream approach for simultaneous speech translation, emitting text and audio while source speech continues. The official repository currently describes French-to-English support. It is a valuable test of continuous input/output and alignment, but translation overlap is not a conversation's barge-in decision.

[SeamlessStreaming](https://github.com/facebookresearch/seamless_communication/blob/main/docs/streaming/README.md) offers a multilingual translation comparison with released streaming components and evaluation support. Check checkpoint licenses and expressive-vocoder access separately. Add translation quality versus lag measurements; ordinary endpoint latency does not characterize this task.

### Training/evaluation assets worth integrating

[Sommelier](https://github.com/naver-ai/sommelier) provides a conversational-audio preparation pipeline using diarization, multiple ASR systems, alignment, and music processing. Its [paper](https://arxiv.org/abs/2603.25750) is directly relevant to preserving overlaps and backchannels for duplex training. Integrate as an offline dataset tool, not a live dialogue model. Retain original audio/timing and processing provenance; alignment and ensemble recognition can still be wrong.

[Kimi-Audio-Evalkit](https://github.com/MoonshotAI/Kimi-Audio-Evalkit) can broaden semantic/audio evaluation beyond turn-taking. Its documented evaluation includes optional model-based judges, so freeze judge identity and account for external dependencies before calling a run fully local.

Keep OpenRealtime's existing full-duplex benchmark integrations as the primary interaction harness. Add the ELLSA speech/action fixtures, VAP/DualTurn event tasks, overlap attribution tests, and translation lag tests as separate task families. Aggregate only comparable metrics; do not collapse conversational timing, action success, and sound understanding into an unexplained single score.

## 6. Serving-runtime findings that change the plan

The current [vLLM-Omni full-duplex endpoint documentation](https://docs.vllm.ai/projects/vllm-omni/en/latest/serving/full_duplex_api/) provides capability negotiation, playback acknowledgment, overlap events, and persistent sessions. **It currently names MiniCPM-o 4.5 as the only model served through the unified duplex-plugin endpoint**, while PersonaPlex and Nemotron still need migration from older paths.

This differs from the [VoiceChat recipe's experimental native-duplex instructions](https://github.com/vllm-project/vllm-omni/blob/main/recipes/NVIDIA/NemotronLabs-VoiceChat.md). Treat this as a version/path compatibility issue, not evidence that either all recipes work on current main or that VoiceChat lacks duplex capability. Pin the runtime revision and verify the exact route.

The endpoint documentation also warns that requesting the duplex query on an ordinary deployment can reach the turn-based handler. Check the returned capability contract and actual input-during-output behavior. Do not certify a deployment from its URL or a successful handshake.

Use an OpenRealtime adapter over the engine's actual contract, retaining local authority, provenance, and playback tracking. Avoid reimplementing an engine's token scheduler in Go. The vLLM runtime class called `DuplexOmni` is distinct from the research model named DuplexOmni above.

## 7. Additional graph contracts needed

The [first plan's live-session and revision contracts](full-duplex-streaming-plan.md#7-proposed-contracts-and-composition-rules) remain the foundation. This expanded survey adds:

1. **Interaction forecasts:** event type, subject/speaker, prediction horizon, confidence/calibration identity, source interval, evidence arrival, and expiration. An observation becomes an act only through the selected controller.
2. **Speaker activity:** simultaneous probability tracks, session-local speaker identifiers, attribution revisions, capacity limits, and optional enrollment provenance. Diarization IDs are not verified real-world identities.
3. **Playback-reference audio:** a clock-aligned tap representing rendered audio, with discontinuities and output delay. Needed for dual-channel prediction and AEC; distinguish generated, sent, and played signals.
4. **Background audio observations:** bounded source window, completion time, task/schema, uncertainty, and stale-result rejection. A late analysis cannot be presented as knowledge available to an earlier micro-turn.
5. **Native tool/action proposals:** separate model proposal, authorized execution, result admission, and cancellation. Preserve the existing action authority when a native model supplies its own tool stream.
6. **Translation streams:** source/target language, source-consumption frontier, translated-text revisions, audio alignment, and lag. Do not force translation into a turn-completion contract.

Use independently declared evidence and controller ownership. Running Smart Turn, DualTurn, Flux, and a native model together does not justify letting all of them stop playback. Start new predictors in observation-only mode; enable one explicit arbitration policy at a time.

## 8. Revised integration order on the RTX Pro

The table below records candidate-level research priorities. The [main plan's implementation sequence](full-duplex-streaming-plan.md#9-implementation-sequence-and-acceptance-gates) consolidates them with the user's micro-turn cascade, defines dependencies and release scope, and is authoritative for execution order.

The already verified server is a single RTX PRO 6000 Blackwell with approximately 96 GB class memory. Capacity is not availability or a performance result. All future inference remains on `rtx-pro`; lightweight CPU baselines also run on that server for controlled comparisons.

| Order | Work package | Acceptance evidence | Relative effort / risk |
| --- | --- | --- | --- |
| 1 | Smart Turn plus a text endpoint baseline | Matched premature-endpoint/waiting comparison; no new controller ambiguity | Small interface surface; language calibration needed |
| 2 | VoiceChat reference deployment and tool channel | Real input during output; delayed tool result; correction and cancellation; exact runtime manifest | Medium/high; evolving runtime routes |
| 3 | DualTurn with VAP baseline | Causal aligned-channel predictions; played-audio reference; live event metrics | Medium; timing and dataset access matter |
| 4 | Lychee-FD isolated deployment | Compatible Blackwell kernels; control latency under synthesis load; long-session memory | High; patched older serving stack |
| 5 | Sortformer plus multitalker ASR | Speaker-attributed overlap quality, lag, ID consistency, speaker-scaled compute | Medium/high; not automatically on the fast path |
| 6 | One audio-understanding observer; optional denoiser/AEC experiment | Useful incremental evidence without breaking micro-turn deadlines or quiet speech | Medium; schema and audio effects |
| 7 | ELLSA, Freeze-Omni, Hibiki/Seamless task profiles | Speech/action or translation-specific conformance and quality | Research extensions after core comparisons |
| Deferred | DuplexOmni full deployment; unverified-release papers | Complete asset/runtime/hardware feasibility established first | Large hardware requirement or unavailable assets |

This ordering extends, rather than displaces, the user's own model integration and the previously planned DuplexCascade/PersonaPlex/MiniCPM-o work. Run large native models separately first. Do not load every shortlisted model concurrently on the same GPU.

### Concrete comparison cells

- **Endpoint evidence:** same cascade, acoustic Smart Turn versus transcript classifier versus calibrated combination.
- **Overlap interpretation:** same cascade, current policy versus VAP/DualTurn-informed policy; separate real interruptions from acknowledgments.
- **Native tools:** VoiceChat versus an existing native provider and the modular tool path, with identical tool schemas and delayed-result fixtures. This is a system comparison unless backbones/evidence are matched.
- **Native architecture:** Lychee-FD versus PersonaPlex and the user's cascade; report language/task quality separately from interaction timing.
- **Speaker attribution:** ordinary mixed-audio ASR versus diarizer plus multitalker ASR, with two and four speakers and controlled overlap.
- **Acoustic robustness:** raw versus processed audio under noise, loudspeaker echo, and double talk. Include soft backchannels and user silence.
- **Information representation:** transcript only versus timed text plus one acoustic observer versus a native speech model. Preserve the original plan's distinction between controlled representation ablations and whole-system comparisons.

For every promoted candidate, archive exact code/checkpoint/runtime revisions, licenses and asset dependencies, selected graph definition, hardware inventory, raw event traces, playback receipts, and complete benchmark population. No model in this expanded survey has yet earned an OpenRealtime live or RTX Pro performance claim.
