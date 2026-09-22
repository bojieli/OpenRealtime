# Streaming and full-duplex integration: results

**Executed:** 2026-09-22 on `rtx-pro` (one RTX PRO 6000 Blackwell, 96 GB,
driver 595.91.07). **Plan:** [streaming and full-duplex components](full-duplex-streaming-plan.md)
and the [open-model survey](open-duplex-models-survey.md). **Status:** first
execution in progress across stages P0–P9; every number below is a component or smoke
measurement on this host, not a release-grade campaign.

**Host conditions.** The machine is shared. Throughout the runs, other users'
EDA jobs (yosys/OpenROAD) kept the 32-core CPU at a load average of roughly
90–250, and up to seven model-integration jobs shared the GPU under a single
large-model lease. Latency numbers describe these shared-host runs; they do not establish an
upper bound or isolated performance for this hardware. Result metadata records
the load average where available.
ASR and end-to-end interaction probes use wall-clock replay; offline task
evaluations must be interpreted according to their own timing protocol.

Result files live under `.runtime/duplex-plan/results/` (not committed); the
tables here are generated from them by `tools/duplexmodels/asr_summary.py` and
`tools/duplexmodels/e2e_summary.py`.

## What was built

| Plan item | Where | What it establishes |
| --- | --- | --- |
| Streaming recognition contract (7.1) | `adapters/vllmrealtime`, `adapters/qwenasr` (`stable_text`), `providers/asr.go` (`vllm-realtime`, `streaming-asr`) | Committed-prefix revisions from any service; withdrawals are refused, provenance of commitment is named |
| Incremental synthesis contract (7.4) | `openrealtime-incremental-speech/1` in `tools/duplexmodels/common.py`; `adapters/speechsocket` (`speech-socket`) | One context: append while audio plays, nonterminal flush, end, cancel with no stale audio after it |
| Acoustic end-of-turn evidence (7.7) | `adapters/turnend`, `interaction/acoustic.go`, `binding/cascade/turnend.go`, `-turn-end-url/-mode/-threshold` | Evidence recorded at every pause (`turn_end.acoustic` on the timeline); promoted to control only by an explicit mode, bounded by the floor's hold |
| 500 ms micro-turn clock (7.2) and live LM session (7.3) | `sidecars/microturn_sidecar.py` | Ticks on silence, admits late words at the current tick with their spoken time, separates "sound with no words yet", traces every decision |
| Component benchmarks | `tools/asrbench`, `tools/ttsprobe`, `tools/duplexmodels/*` | Wall-clock replay through the real adapters |
| Deployment | `deploy/duplex/services/*.sh`, `deploy/duplex/profiles/*.yaml`, `deploy/duplex/run-e2e.sh`, `deploy/duplex/manifest.json` | Pinned revisions, one start/stop/status script per service, one serve profile per cell |

## Streaming recognition (P2)

Fixtures: 40 LibriSpeech test-clean (English) and 40 FLEURS cmn_hans_cn
(Mandarin) utterances, 3–15 s, each with 300 ms of leading silence, replayed
at wall-clock speed in 100 ms frames through the Go adapter the runtime uses.
Times are from the start of the audio. "Committed" is text the recogniser will
not revise; "rewrites" counts hypotheses that took back text a consumer had
already been shown.

| Recogniser | Route | Lang | Error rate | First hypothesis p50 | First committed p50 | Finalize p50 | Withdrawals | Rewrites |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Qwen3-ASR 0.6B | start/chunk/finish (baseline) | en | **2.0%** WER | **847 ms** | 6,054 ms (final only) | 59 ms | 0 | n/a |
| Nemotron streaming EN 0.6B, 160 ms chunk | start/chunk/finish | en | 2.4% WER | 1,313 ms | **1,313 ms** | 27 ms | 0 | 0 |
| Nemotron streaming EN 0.6B, 560 ms chunk | start/chunk/finish | en | 2.4% WER | 1,805 ms | 1,805 ms | 98 ms | 0 | 0 |
| Voxtral Mini 4B Realtime, 480 ms delay | vLLM /v1/realtime | en | 2.8% WER | 1,702 ms | 1,702 ms | 317 ms | 0 | 0 |
| Voxtral Mini 4B Realtime | vLLM /v1/realtime | zh | 9.5% CER | 2,701 ms | 2,701 ms | 451 ms | 0 | 0 |
| Deepgram Nova-3 (closed) | WebSocket | en | 4.5% WER | 1,101 ms | 4,439 ms | 41 ms | 0 | 31 |
| Deepgram Nova-3 (closed) | WebSocket | zh | 12.9% CER | 2,101 ms | 4,701 ms | 75 ms | 0 | 61 |
| Kyutai STT 1B | start/chunk/finish | en | 6.0% WER | 1,434 ms | 1,740 ms | 188 ms | 0 | 0 |
| Kyutai STT 1B | start/chunk/finish | fr | 22.2% WER | 2,574 ms | 3,036 ms | 157 ms | 0 | 0 |
| Nemotron 3.5 multilingual 0.6B, 320 ms | start/chunk/finish | en / zh | 4.7% / 18.0% | 1,813 / 2,534 ms | same | 177 / 65 ms | 0 | 0 |
| FunASR streaming Paraformer, 600 ms chunk | start/chunk/finish | zh | 11.8% CER | 2,679 ms | 2,679 ms | 184 ms | 0 | 0 |

Times are from the start of the audio, which carries 300 ms of pre-roll plus
each recording's own leading silence, so they compare recognisers rather than
state an absolute word lag. The full table, including the configurations not
shown here, is `python tools/duplexmodels/asr_summary.py`.

Readings:

- **When evidence becomes usable differs more than accuracy does.** Qwen3-ASR
  has the lowest English WER and the earliest first hypothesis, but commits
  nothing until the final; a consumer that must not act on words it will see
  revised waits about six seconds into an utterance. Nemotron (cache-aware
  RNN-T) and Voxtral (causal decoder) commit as they go.
- **Deepgram Nova-3** shows the provisional-then-final pattern: early
  interim text (1.1 s) with 31 rewrites over 30 utterances, and committed
  segments only at its finals.
- **Voxtral's first word after a long silence** arrives about 1.4 s after the
  speech began on FDB recordings (measured directly against vLLM), which
  alone rules out word-only barge-in inside a 1 s yield window.
- Replay concurrency 4 against Voxtral on this CPU-starved host failed 16 of
  40 utterances (the realtime route's per-step Python scheduling fell behind);
  concurrency 2 failed none. Capacity is a property of the host, not only the
  GPU.

## Incremental synthesis (P3)

Six synthesisers serve `openrealtime-incremental-speech/1`. The acceptance
test is the plan's: open a context, append the first half of a sentence, wait
1.5 s, and see whether audio for that unfinished prefix arrives before the
rest is sent - run through the runtime's own client (`tools/ttsprobe`), not
the service's self-report.

| Synthesiser | Declares | Audio for the unfinished prefix | First audio | Audio after cancel |
| --- | --- | --- | --- | --- |
| VibeVoice-Realtime 0.5B | token | **4 of 4** | **70-234 ms** | 0 ms |
| Kyutai TTS 1.6B | token | 2 of 4 | 587-1,793 ms | 0 ms |
| Deepgram Aura (closed) | flush | 1 of 4 | 323-1,612 ms | 0 ms |
| CosyVoice 3 0.5B | token | 0 of 4 | 5,984-15,727 ms | 0 ms |
| Qwen3-TTS 0.6B | token | 0 of 4 | 4,404-16,610 ms | 0 ms |
| Fish S2 Pro, no compile (resumed pass) | sentence | 0 of 4, as declared | 2,186–2,830 ms | 0 ms |

Readings:

- **A 1.5-second held-prefix probe measures responsiveness under its run
  conditions.** Failure to produce audio inside that window does not disprove
  incremental input. The later component report (`results/tts/SUMMARY.md`)
  records Qwen3 producing early audio on 6/6 English prefixes and CosyVoice
  on 1/6; those are separate runs, not a matched latency comparison.
  Deepgram requires an explicit flush rather than token-by-token admission.
- **Cancellation is clean everywhere**: no service delivered any audio after
  the cancel was acknowledged, which is the property the runtime needs to stop
  speaking without leaving stale speech in flight.
- VibeVoice's first audio is an order of magnitude earlier than the rest, and
  since the synthesiser dominates the end-to-end reply latency of this
  cascade, that is the substitution most likely to move it.

The resumed Fish S2 Pro service loads on this GPU after replacing its incompatible
TorchCodec reference-file decoder with SoundFile (the same mono conversion,
resampling and codec encoding are retained). Four English probes through the
real Go adapter produced 4.46–5.90 s of audio each; cancellation delivered no
further audio after the client cancelled. First-audio times include the probe's
1.5 s held suffix. Uncompiled wall times were 6.92–14.04 s, so these runs do not
establish real-time synthesis. The subsequent 70-case component run completed: 40 complete-text, 12 held-prefix,
12 word-by-word, and six cancellation cases. Qwen3-ASR transcription of the
complete-text outputs measured 0% English WER and 0.67% Mandarin CER (20 cases
each); these are intelligibility proxies, not human quality ratings. Median
first audio was 707/769 ms (English/Mandarin), and mean RTF was 1.42/1.74.
All 12 held-prefix cases waited for the suffix, as required by the declared
sentence-input contract. All six cancellations were acknowledged in 30–106 ms,
with no audio frames after the cancel request. The new
`cascade-nemotron-qwen3-fish` public smoke run completed without task errors:
7/8 FD-Bench turns answered, one missed, zero premature starts, and 1,125 ms
median of the two per-conversation response-latency values. FDB passed 1/1
applicable interruption (245 ms yield), 1/1 background-speech case and 2/2
talking-to-other cases. Four of eight recordings were not applicable, including
both backchannels. This is a small NOT REPORTABLE smoke run, not full acceptance.
The exact profile, results and timeline are retained under
`deploy/duplex/evidence/cascade-nemotron-qwen3-fish/20260922T164918Z-uc548hir/`. Evidence is
retained in `deploy/duplex/evidence/fish-s2-pro/20260922-resume/`.
The weights are revision `1de9996b6be38b745688de084d87a5633f714e4e`,
with Fish source `e5e292632cb11e7a27b2b7487f58f612bc101e13`; retained
checksums cover the weight shards, codec, config, tokenizer and license.
The Fish Audio Research License permits research/non-commercial use;
commercial use requires a separate Fish Audio license.

All of these were measured while the machine was loaded; the per-service
comparison in `.runtime/duplex-plan/results/tts/` has the quieter re-runs and
available raw outputs. Its latest summary marks intelligibility as unscored;
latency and cancellation alone do not establish speech quality.

Kyutai's model-side PCM queue now pauses at 25 frames (two seconds) per row.
A live GPU backpressure probe exhausted a one-second transport credit window,
observed generation remain at step 58 with exactly 25 queued frames, then
returned credit and observed generation resume to step 71. Cancellation was
acknowledged in 94 ms and released the row. This bounds model PCM and credited
transport audio; it does not establish bounded text/history or rendered playback.
Evidence: `deploy/duplex/evidence/kyutai/20260922-resume/bounded-queue.json`.

## Micro-turn language model (P4)

The micro-turn cascade runs as one duplex model
(`sidecars/microturn_sidecar.py`): streaming ASR, a controller asked every
500 ms, a streamed answer, and incremental synthesis, with its own floor.
The measurements below use an ordinary instruct model (Qwen3-8B), the plan's
"orchestrated micro-turns" fallback; they do not establish released DuplexCascade
behavior. Access to DuplexCascade and PersonaPlex was verified on 2026-09-22
using the saved Hugging Face login after removing an environment credential
override. Their pinned checkpoints are now fully downloaded; integrations are in progress.

The first composed Kyutai ASR → released DuplexCascade → Kyutai TTS probe
failed to answer: 36 ticks, no text answer and no audio. The admitted transcript
stopped at “What is the capital of”; the recognizer's completed-word commitment
policy can retain its final word until a following boundary or session finish.
This is a concrete integration failure requiring investigation, not a passing
native cascade. Its trace is retained as `audio-probe-failed.json` beside the
component evidence below.

The follow-up capture confirmed that Kyutai had recognized the full question
but withheld its final word from `stable_text`. The native path now admits raw
fragments only when the service explicitly declares `decoder-append-only`,
rejects prefix revisions, and preserves fragment whitespace. On the same paced
recording the composed pipeline answered “The capital of France is Paris” and
returned 34 synthesis audio packets over 36 ticks, with no reported errors or
500 ms tick overruns after explicit warm-up. This is one component probe,
not public Realtime or rendered-playback acceptance. The successful trace and
raw recognizer comparison are retained as `audio-fragments.json` and
`asr-finalword.json` alongside the failed attempt.

A follow-up 25 s component probe injected a second recorded question 800 ms
after the first synthesis packet. It answered Paris and then George Washington,
with one model-driven synthesis cancellation at 6.753 s after second-input onset
at 5.7 s (about 1.05 s). The trigger was `<|user is talking|>`, not a dedicated
interruption token. All 46 ticks stayed within 500 ms; no errors were reported.
The retained `audio-interrupt.json` includes the concatenated output waveform's
hash (6.08 s at 24 kHz). Packet arrival and concatenated output do not establish
rendered overlap or audible yield latency.

The first public native-cascade smoke attempt failed: persistent synthesis
outran paced delivery and exceeded the bounded 30 s output queue. Its failed
results are retained under `deploy/duplex/evidence/native-duplexcascade/`.
Backpressure at the synthesis reader preserved the buffer bound but exposed a
second failure: long answers filled the WebSocket receive queue and delayed
keepalive handling. Trace evidence from `20260922T162147Z-5q1y_xfd` shows PCM
playout progressing normally, with a successful initial model interruption,
while the follow-up answer filled the 30 s queue. The transport now negotiates
byte credits, so synthesis sends only the audio the consumer has capacity to
receive while control messages remain readable. Backend generation queues are
not bounded by this transport change. The follow-up public attempt
`20260922T162928Z-tbyao6ly` delivered 164.24 s of PCM without the keepalive
failure, but its roughly 436-word response exceeded the benchmark's three-minute
conversation deadline. Later connections encountered the still-occupied session.
EOF shutdown now signals native workers before joining the response worker,
and synthesis shutdown cancels a reader blocked on playback capacity. A real
GPU reconnect probe then passed two consecutive audio sessions: disconnecting
after the first output packet allowed a second handshake in 219 ms and fresh
audio without protocol errors. This verifies early-output cleanup, not
saturated-buffer or long-session shutdown. The public smoke rerun remains
pending; prior failed measurements are retained.

The cleanup rerun `20260922T165731Z-7crrl2jb` completed all five sessions
without connection refusals. It still failed overall: interruption and
backchannel recordings each hit the three-minute conversation timeout.
Background speech and talking-to-other each passed one applicable recording.
The single FD-Bench conversation answered 3/4 turns, missed one, recorded zero
premature starts, 4,960 ms overlap and 1,547 ms response latency. These are
partial smoke observations within a failed run, not a supported profile.
The complete attempt is retained under `deploy/duplex/evidence/native-duplexcascade/`.

The new native sidecar also passed a real protocol question probe: “Paris,”
2.0 s of paced output audio, one output turn boundary, and no protocol errors.
First audible packets arrived 1.445 s after the supplied question ended.
Cold model loading plus warm-up made the handshake 61.697 s; the deployment
now loads once behind a persistent TCP listener for public benchmark runs. The output boundary
uses the documented thinking/drain/quiet adapter policy, not a native EOS or
playback receipt. Evidence: `sidecar-question.json`.

The released DuplexCascade checkpoint now passes a text-only GPU micro-turn
probe with strict tensor loading after 112 shape-checked PEFT base-layer key
renames. It emits `<|user is talking|>` for two incoming question chunks, then
`<|user finish talking|>` and “The capital of France is Paris” on the first
silence chunk. Peak allocation was 16.81 GiB. The cold first generation took
727 ms; subsequent generations took 28–148 ms. This unpaced probe does not
establish the 500 ms audio pipeline. The slow tokenizer matched the released
fast tokenizer IDs on five prompt/control/multilingual examples; its use and
the complete key mapping are retained in
[`duplexcascade/20260922-resume`](../deploy/duplex/evidence/duplexcascade/20260922-resume/).

What the clock costs and what it delivers, from the traces of the end-to-end
runs (`python tools/duplexmodels/microturn_trace.py`):

| Measurement | Value |
| --- | --- |
| Committed word to the tick that admitted it | p50 175 ms, p90 422 ms |
| Control decision (Qwen3-8B, enumerated answer) | p50 123 ms, p90 198 ms |
| Decisions that outlasted their 500 ms tick | 0 of 5,017 |
| Decision triggers | 4,540 clock, 416 a word during speech, 61 the user going quiet |

Three rules are enforced without asking the controller, because they are about
who holds the floor rather than about what was meant: while the user is
audibly speaking the assistant waits; words spoken before the assistant took
the floor cannot be an interruption of it; and a decision is taken at the
moment the user goes quiet rather than at the next tick. Two exploratory
FD-Bench runs recorded 31/55 and 30/36 answered turns with no premature starts.
Their different denominators and run conditions prevent attributing the
difference to these rules; a matched replay is still required.

## Native speech models (P5)

PersonaPlex now runs with the authorized pinned weights and upstream code
`3428dfd95309a7f3c84fd93259ded0f810d1ff91`. Its 40 s assistant example
produced 40 s of finite, non-silent 24 kHz audio with voice prompt NATF2 and
the upstream teacher role prompt. This was offline frame inference, not
wall-clock replay or a public Realtime session. The summary, prompt, seed,
and audio hashes are retained in
[`personaplex/20260922-resume`](../deploy/duplex/evidence/personaplex/20260922-resume/).

A subsequent 25 s wall-clock-paced sidecar question probe answered “Paris”
and emitted an answer turn boundary without protocol errors. First audible
answer packets arrived 158 ms after the supplied question ended; this is
packet-arrival timing, not rendered playback latency. Across 312 model frames,
p50/p95 compute was 28.61/31.93 ms per 80 ms frame, with one frame over budget
and no dropped or starved input frames. The sidecar uses an explicit
RMS/hangover policy for output boundaries; these are not native EOS signals.
The public Realtime smoke run `20260922T153606Z-0uz6_y88` completed without
task errors: interruption 2/2, backchannel 2/2, and FD-Bench 8/8 turns answered
with zero premature starts and 212 ms median response latency. Both background
and both other-talk recordings were not applicable because the assistant was
not speaking at the event; neither category is validated. These small samples
are NOT REPORTABLE as a full campaign. Result JSON, profile, and integrity
metadata are retained under `deploy/duplex/evidence/native-personaplex/`.

Native validation remains incomplete. The retained component artifacts under
`results/native/` establish narrower findings:

- **Freeze-Omni:** the recorded eight-clip interruption smoke run had seven
  applicable clips and zero yield passes, with 3,392 ms median yield latency.
  The model generally waited until the interrupting utterance ended. Its
  ten-minute probe grew from 17,342 to 22,508 MiB and experienced shared-GPU
  out-of-memory errors; it is not a clean endurance pass. See
  `freeze-summary.json` and the referenced raw runs.
- **Lychee-FD:** `lychee-control-summary.json` records 29 model interrupts
  and 26 audio-stall endings. Its 64 first-audio observations have a 1,099 ms
  median, but the control-delay sample is empty. This does not establish the
  planned control-latency acceptance criterion.
- **Moshi:** the pinned public-Realtime profile completed a two-recording-per-category
  smoke run (`20260922T171034Z-giltdhg4`) without task errors. It answered
  8/8 FD-Bench turns but started 4 prematurely. Interruption yield passed 1/2
  (213 ms and 2,943 ms); backchannel hold passed the one applicable recording,
  with the other not applicable. Both background and both other-talk recordings
  were not applicable, so those behaviors remain unvalidated. The median of
  the two conversation response-latency values was 210 ms. This is NOT REPORTABLE
  as a full campaign and does not establish acceptable floor control.
  [Retained results, profile and integrity metadata](../deploy/duplex/evidence/native-moshi/20260922T171034Z-giltdhg4/)
  preserve the complete run.
- **MiniCPM-o:** the official audio-only profile completed public-Realtime smoke
  run `20260922T172034Z-7b3gefwh` without task errors. Both interruption
  recordings failed (20,457 ms and 2,537 ms yield latency). Backchannel hold
  passed 1/1 applicable recording, with one not applicable; background speech
  and other-talk each passed 2/2. FD-Bench answered 6/8 turns, missed two,
  and had zero premature starts, but recorded 23,040 ms aggregate overlap
  and six overrun turns. The median of the two per-conversation response
  latency values was 891 ms. All result digests were verified before retaining
  [the profile, results and runtime logs](../deploy/duplex/evidence/native-minicpm-o/20260922T172034Z-7b3gefwh/).
  This is NOT REPORTABLE as a full campaign. The working tree was modified,
  and the service remained on its original loaded code while cancellation
  fixes were committed; this run does not validate those fixes. The model's
  floor control, rendered playback and endurance remain unaccepted.
  After reloading the cancellation fixes, a 40-second GPU probe sent an
  explicit interrupt at 8.960 s. No audio packets arrived between that control
  write and the turn boundary 441 ms later; previously received audio extended
  an estimated 65 ms beyond onset. This is receive-time evidence, not rendered
  playback. The model resumed audible output at 11.111 s while the interrupting
  recording continued until 11.915 s, so successful packet suppression does
  not establish correct floor behavior. The continuation text was fragmented
  ("start ly saving right now."). No protocol errors occurred.
  [Cancellation trace and summary](../deploy/duplex/evidence/minicpm-o/20260922-cancellation/)
  retain this limitation alongside the transport result. A subsequent real-model
  disconnect/reconnect probe also passed: both sessions produced fresh PCM,
  with handshakes of 345 ms and 358 ms and no protocol errors. It disconnects
  at the first audio packet; full-buffer cancellation and endurance remain
  unverified.
- **VoiceChat:** the 8,192-position talker failed startup with a 1 GiB KV
  cache; 2 GiB allowed loading. The originally pinned vLLM-Omni `9ebef4b`
  then rejected duplex WebSockets because its new plugin framework disables
  the legacy VoiceChat integration. On the earlier `9005d789` revision, a
  30-second real question session answered with 1.2 s measured first audible
  response latency after the question; a separate 45-second session called
  `generate_random_number`, accepted a result delayed four seconds, and said
  “The random number is thirty seven.” Both probes recorded no protocol
  errors. [Retained summaries](../deploy/duplex/evidence/voicechat/20260922-resume/)
  are sidecar-level evidence. The subsequent public-Realtime FDB probe
  reached its three-minute timeout on the first recording: continuous
  silence-frame output kept a response active after input ended. The run was
  stopped and retained as failed evidence under `deploy/duplex/evidence/native-voicechat`.
  Turn completion is an open integration defect; public tool handling,
  correction, duplicate calls, rendered playback and endurance remain unverified.
  A fresh 30-second diagnostic replay reproduced the missing final boundary:
  the answer's audible segment ended at 8.905 s, but no terminal event followed
  before session close at 30 s. The runtime logged end tokens at text frames
  10 and 38 (earlier output), then padding at frames 100, 200 and 300 with
  an empty pending-end list and audio coverage one frame behind. Thus this
  reproduction is not an end marker stuck waiting for the decoder; the final
  answer's end marker did not reach the data-plane projection. First audible
  answer latency was 1.235 s and no protocol errors occurred.
  [Retained upstream events and model-boundary trace](../deploy/duplex/evidence/voicechat/20260922-boundary/)
  narrow the investigation to token generation/projection before turn completion.
  A second recorded question reproduced the same pattern: 1.222 s first-audio
  latency, audible answer ending at 9.341 s, and no final boundary by 30 s.
  Both cases used the same warmed runtime and different context recordings;
  the missing boundary is not confined to the first question. Instrumenting
  the thinker's greedy sampler before scheduler transport then localized the
  first case: the answer's final punctuation token (1046) transitioned directly
  to padding (12), with no sampled end token (2). The sampler did emit earlier
  end tokens for the greeting. This implicates generation in this configured
  runtime rather than a final end token discarded by the downstream projector;
  it does not yet distinguish model behavior from runtime numerical differences.
  Delaying the question to 5 s (after the greeting's audible end at 3.888 s)
  did not restore the final answer boundary. The greeting boundary instead
  shifted to 5.849 s, after the new user onset, while the answer's audible
  output ended at 14.126 s without a boundary by 25 s. This suggests the
  marker may track a later dialogue transition rather than audible completion.
  A controlled next-input check confirmed that behavior in this runtime: after
  the first answer's audible end at 8.817 s, a second recorded utterance began
  at 11.520 s with no explicit interrupt command. An end token was then sampled
  and the first answer's boundary arrived at 12.505 s (985 ms after new input
  onset). The second answer ended audibly at 20.922 s but had no final boundary
  before 30 s. Upstream EOS therefore cannot be the sole playback-completion
  signal for this profile; an explicitly declared output-segmentation policy
  is needed without relabelling its boundaries as model decisions.
  The opt-in `native-voicechat-output-segmented` profile now uses 800 ms of
  decoded quiet PCM after audible output to close an adapter segment. It
  suppresses idle codec silence and leaves the default upstream-EOS profile
  available. A real GPU question probe closed the final answer at 8.907 s
  (audible segment ended at 8.175 s), with no protocol errors or duplicate
  greeting boundary. An earlier implementation's duplicate boundary and the
  corrected result are both retained in
  [the segmentation evidence](../deploy/duplex/evidence/voicechat/20260922-output-segmentation/).
  This one-question check does not establish interruption or playback acceptance;
  the new profile's public-Realtime evaluation is pending.

## Interaction prediction (P6)

Six predictors run as evidence producers on one service
(`tools/duplexmodels/turn_server.py`), each answering its own question and
never relabelled as a universal end-of-turn score: Smart Turn v3.2 (acoustic),
LiveKit's transcript detector, VAP and DualTurn (activity forecasts),
X2-Turn and SoulX-Duplug (their own turn states). The survey recorded X2-Turn
and SoulX-Duplug as having no released weights; that was a wrong repository
id - `x-square-robot/X2-Turn-4B-0812` and `Soul-AILab/SoulX-Duplug-0.6B` are
Apache-2.0 and were downloaded and integrated.

**Cell I0 - endpointing.** 2,407 labelled pauses over 602 files (FD-Bench,
FDB v1.5 and FDB v3), split by file into calibration and test; at each silence
of 200 ms or more every predictor is asked, and re-asked as the silence grows.
ROC-AUC on the test split at the 200 ms checkpoint:

| Source | Smart Turn (acoustic) | LiveKit (transcript) | Fusion |
| --- | --- | --- | --- |
| all | 0.681 | 0.878 | **0.882** |
| FD-Bench synthetic speech | 0.448-0.582 | 0.801-0.929 | 0.787-0.926 |
| FDB v3 disfluent human speech | **0.813** | 0.772 | 0.803 |

At a matched 10% premature-endpoint budget the fusion ends a turn after a mean
558 ms of silence, LiveKit 578 ms, Smart Turn 1,136 ms, and a plain 0.95 s
silence timer 750 ms; the shipped default (500 ms silence, no classifier) is
45.8% premature on the same points. **Transcript lag decides the ranking**:
with the transcript truncated as a streaming recogniser would leave it,
LiveKit falls from 0.878 to 0.706 at 300 ms of lag and 0.567 at 600 ms, while
Smart Turn is unchanged and the fusion degrades gracefully. Which is to say
the text model wins on paper and loses part of that advantage in a live
pipeline - and the acoustic model is the one that survives disfluent human
speech.

**Cell I1 - overlap.** On 297 FDB v1.5 events (backchannel versus
interruption, with a constructed assistant channel), the forecasts carry
signal - AUC at 0.8 s of overlap: DualTurn 0.818, VAP 0.713 - but at a matched
10% false-stop budget a sustained-800 ms voice-activity rule beats both
(balanced accuracy 0.886 against 0.729 and 0.601). On this fixture
backchannels last 0.38-0.80 s and interruptions 1.8-3.8 s, so duration is
nearly a sufficient statistic: that is a property of FDB v1.5 as much as of
the models, and it is why the plan asks for real overlap fixtures before
promoting a predictor to control.

Per-call latency on the loaded host (p50): Smart Turn 14.6 ms, LiveKit
26.6 ms, VAP 25.4 ms, fusion 27.8 ms, DualTurn 136.9 ms at a 3 s window and
220 ms at 8 s - which is why the DualTurn cell raises the classifier bound to
500 ms. Two integration findings worth keeping: LiveKit's raw probability
decides English at 0.011, so a 0.5 threshold would never end a turn, and
DualTurn's published `modeling_dualturn.py` silently drops the per-task layer
attention its own config enables, so the service implements the research
path and says which one it ran.

## Perception and task extensions (P7, P8)

**Speaker attribution under overlap (cell S0).** Controlled mixtures from
LibriSpeech test-clean at 25% overlap, scored by concatenated
minimum-permutation WER:

| Condition | Ordinary mixed-audio recognition | Streaming Sortformer + multitalker Parakeet |
| --- | --- | --- |
| 2 speakers | cpWER 0.74 | **cpWER 0.18** |
| 4 speakers | cpWER 1.24 | **cpWER 0.17** |

Mixed-audio recognition does not degrade gracefully under overlap - at four
speakers it is worse than useless - while the diarizer-plus-multitalker path
holds. That gain is paid for in delay and per-speaker compute, and Sortformer's
documented low-latency setting buffers about a second before computing, so
speaker attribution must not sit on the 500 ms interaction path; it belongs
where a late, revisable answer is acceptable.

**Acoustic preprocessing (cell A0).** DeepFilterNet3 behind the existing
pre-ASR filter contract has a measured 40 ms waveform delay. In the separate
offline preprocessing comparison on FD-Bench's
0 dB background-noise condition it lowers WER from 14.4% to 12.2%, and at
10 dB it changes nothing (2.42% to 2.49%). It also cuts what the recogniser
hallucinates in the gaps between turns (488 to 450 words, gap level -30.7 dB
to -56.4 dB). These offline results do not validate the runtime resampler:
its identity round trip measured only 17.73 dB SNR at 16 kHz. An indicative
29-turn comparison recorded 38.0% streaming WER versus 28.1% with offline
polyphase resampling. Quiet backchannels mixed with noise also lost word
recall (0.82 to 0.44 in the recorded probe). Runtime resampling and quiet-speech
preservation remain open acceptance items; this is an experimental branch.

**Background audio understanding (cell A0).** Audio Flamingo 3 serves bounded
windows off the critical path, every answer carrying when it became available
and when it expires, shedding load rather than queueing. As a four-way
classifier of what an overlapping sound means it fails (42% accuracy; it never
recognises background speech or side conversation as such), but as a detector
it is informative: AUROC 0.88 for "is there background speech", 0.95 for
"side conversation versus interruption". That is the shape the plan predicted -
a useful uncertain observer, not a controller - and it is why its answers are
typed as expiring hypotheses.

**Task extensions (P8).** ELLSA's speech-only research loop now runs on the
GPU after bridging its joint speech/vision attention to SDPA. The bridge was
checked against explicit attention calculations for prefill, cached decoding,
grouped-query heads, and padding masks. Eight available Llama Questions clips
completed: six answers contain the dataset reference string (a limited scoring
rule, not a semantic accuracy assessment), with 36.04 GiB peak allocation and
approximately 684 ms mean compute per 1 s block. Whole-clip fbank preparation
and history recomputation follow the research driver; spoken output, actions,
causal live input, and public agent acceptance remain unverified.

BayLing's retained GPU run covers six question/interruption cases. Its offline
path tokenizes the complete clip before generation. A separate live-prefix
simulation took 903 ms p50 / 1,016 ms p90 per 800 ms block and fed 99/230 tokens
different from the offline sequence. The prefix stability probe found 176
mismatches in 1,900 comparisons, including nine within completed 4 s blocks;
its older claim of perfectly stable completed blocks is not supported by this
GPU result. These are component feasibility measurements. The JSON and their
provenance are retained in
[`task-extensions/20260922-resume`](../deploy/duplex/evidence/task-extensions/20260922-resume/).

## End-to-end voice agent performance (P9)

The resumed Nemotron/Qwen3/Kyutai smoke run retains its hashed result JSON
and profile in [`deploy/duplex/evidence`](../deploy/duplex/evidence/). It covers
two recordings per FDB category and two FD-Bench conversations. The working
tree was modified, so this is a diagnostic result, not release evidence.

Each cell is one profile driven through the public Realtime protocol: the same
number of FDB v1.5 recordings from each of its four categories, then an
FD-Bench slice. "n/a" recordings are those where the agent was not speaking
when the event arrived, which is a latency failure of a different kind and is
counted separately rather than scored.

<!-- generated: e2e -->
Smoke measurements; NOT REPORTABLE as a full campaign. Invalid or unverified runs are excluded.

| profile | interrupt yield | backchannel hold | background hold | other-talk hold | yield p50 ms | FD-Bench answered/turns | premature | resp p50 ms | load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| cascade-nemotron-qwen3-fish | 1/1 (+1 n/a) | 0/0 (+2 n/a) | 1/1 (+1 n/a) | 2/2 (+0 n/a) | 245 | 7/8 | 0 | 1125 | ['33.31', '35.87', '34.92'] |
| closed-gemini-live | - | - | - | - | - | - | - | - | 218.92 247.93 236.45 |
| closed-gemini-live | **UNVERIFIED legacy run (no result digests or command statuses)** | | | | | | | | |
| closed-openai-live | - | - | - | - | - | - | - | - | 265.13 254.39 239.89 |
| closed-openai-live | **UNVERIFIED legacy run (no result digests or command statuses)** | | | | | | | | |
| microturn-clock-only | - | - | - | - | - | - | - | - | 222.01 200.98 160.93 |
| microturn-clock-only | **INCOMPLETE (no finished.json)** | | | | | | | | |
| microturn-nemotron-qwen3-kyutai | 2/2 (+0 n/a) | 2/2 (+0 n/a) | 1/2 (+0 n/a) | 2/2 (+0 n/a) | 44 | 6/8 | 0 | 872 | ['65.88', '39.67', '26.48'] |
| microturn-voxtral-qwen3-kyutai | - | - | - | - | - | - | - | - | 145.43 146.51 120.52 |
| microturn-voxtral-qwen3-kyutai | **UNVERIFIED legacy run (no result digests or command statuses)** | | | | | | | | |
| native-duplexcascade | - | - | - | - | - | - | - | - | ['38.03', '36.42', '35.55'] |
| native-duplexcascade | **FAILED (see finished.json)** | | | | | | | | |
| native-minicpm-o | 0/2 (+0 n/a) | 1/1 (+1 n/a) | 2/2 (+0 n/a) | 2/2 (+0 n/a) | 11497 | 6/8 | 0 | 891 | ['71.73', '68.43', '51.84'] |
| native-moshi | 1/2 (+0 n/a) | 1/1 (+1 n/a) | 0/0 (+2 n/a) | 0/0 (+2 n/a) | 1578 | 8/8 | 4 | 210 | ['36.58', '34.54', '34.90'] |
| native-personaplex | 2/2 (+0 n/a) | 2/2 (+0 n/a) | 0/0 (+2 n/a) | 0/0 (+2 n/a) | 335 | 8/8 | 0 | 212 | ['9.08', '8.79', '10.39'] |
| native-voicechat | - | - | - | - | - | - | - | - | ['9.49', '18.16', '23.06'] |
| native-voicechat | **FAILED (see finished.json)** | | | | | | | | |
<!-- end generated -->

The clock's own evidence for every traced run:

<!-- generated: microturn -->
| trace | sessions | ticks | evidence wait p50/p90 ms | decision p50/p90 ms | over tick | decisions | triggers |
| --- | --- | --- | --- | --- | --- | --- | --- |
| microturn-clock-only.jsonl | 39 | 2256 | 259/457 | 80/143 | 0 | {'continue': 1524, 'idle': 311, 'wait': 250, 'respond': 116, 'stop': 55} | {'clock': 2256} |
| microturn-nemotron-qwen3-kyutai.jsonl | 10 | 615 | 234/442 | 66/287 | 0 | {'continue': 346, 'wait': 119, 'idle': 94, 'respond': 34, 'stop': 22} | {'clock': 549, 'word': 34, 'pause': 32} |
| microturn-smoke.jsonl | 4 | 216 | 238/413 | 108/188 | 0 | {'continue': 148, 'wait': 25, 'idle': 22, 'respond': 12, 'stop': 8, 'backchannel': 1} | {'clock': 216} |
| microturn-smoke2.jsonl | 6 | 301 | 266/448 | 96/136 | 0 | {'continue': 139, 'wait': 85, 'idle': 45, 'respond': 19, 'stop': 13} | {'clock': 301} |
| microturn-smoke3.jsonl | 6 | 266 | 249/466 | 97/149 | 0 | {'continue': 129, 'idle': 82, 'wait': 27, 'respond': 18, 'stop': 9, 'backchannel': 1} | {'clock': 266} |
| microturn-smoke4.jsonl | 6 | 304 | 186/447 | 131/212 | 0 | {'continue': 196, 'idle': 42, 'wait': 26, 'respond': 21, 'stop': 17, 'backchannel': 2} | {'clock': 273, 'word': 31} |
| microturn-smoke5.jsonl | 12 | 623 | 161/380 | 143/203 | 0 | {'continue': 335, 'idle': 154, 'wait': 50, 'respond': 48, 'stop': 36} | {'clock': 576, 'word': 47} |
| microturn-voxtral-qwen3-kyutai.jsonl | 84 | 5017 | 175/422 | 123/198 | 0 | {'continue': 2616, 'idle': 1121, 'wait': 602, 'respond': 384, 'stop': 294} | {'clock': 4540, 'word': 416, 'pause': 61} |
| mt-fdbench2.jsonl | 8 | 1004 | 244/459 | 160/239 | 0 | {'continue': 572, 'wait': 206, 'idle': 130, 'respond': 55, 'stop': 41} | {'clock': 921, 'word': 83} |
| mt-fdbench3.jsonl | 8 | 1039 | 180/458 | 93/163 | 0 | {'continue': 557, 'wait': 210, 'idle': 137, 'respond': 73, 'stop': 62} | {'clock': 866, 'word': 114, 'pause': 59} |
<!-- end generated -->

## Compatibility matrix

What each model is integrated as, and how far it was taken. "Measured" means
a real model produced real output under wall-clock replay on this GPU;
"protocol only" means the integration passes conformance with `--mock` but the
model itself was not measured here.

| Model | Role | Integration | Status |
| --- | --- | --- | --- |
| Qwen3-ASR 0.6B | streaming ASR | `qwen-asr` start/chunk/finish | measured (baseline) |
| Voxtral Mini 4B Realtime | streaming ASR | `vllm-realtime` on vLLM `/v1/realtime` | measured, used in the headline profile |
| Nemotron streaming EN 0.6B / 3.5 multilingual | streaming ASR | `streaming-asr` service | measured, both chunk settings |
| Kyutai STT 1B | streaming ASR | `streaming-asr` service | measured (en, fr) |
| FunASR streaming Paraformer | streaming ASR | `streaming-asr` service | measured (zh), append-only verified |
| Deepgram Nova-3 / Flux | streaming ASR (closed) | existing adapter | measured (Nova-3) |
| Kyutai TTS 1.6B | incremental TTS | `speech-socket` service | measured, used in the headline profile |
| CosyVoice 3 0.5B | incremental TTS | `speech-socket` service | measured |
| VibeVoice-Realtime 0.5B, Qwen3-TTS | synthesis | `speech-socket` services | see the synthesis section for actual input semantics |
| Fish S2 Pro | sentence-input TTS, streaming audio output | `speech-socket` service | component evaluation and public cascade smoke measured; not incremental text input |
| Deepgram Aura | incremental TTS (closed) | `speech-socket` bridge | measured |
| Qwen3-8B as micro-turn controller | micro-turn LLM | `microturn` sidecar, orchestrated mode | measured end to end |
| DuplexCascade | micro-turn LLM | `native-duplexcascade` profile and native sidecar | real ASR/model/TTS probes passed; public transport validation ongoing |
| Moshi, VoiceChat 11B, MiniCPM-o 4.5, Lychee-FD, Freeze-Omni | native duplex | sidecar protocol v1 | see the native section; all pass mock conformance |
| PersonaPlex 7B | native duplex | `native-personaplex` profile, sidecar protocol v1 | public smoke measured; full campaign pending |
| Smart Turn v3.2, LiveKit, VAP, DualTurn, X2-Turn, SoulX-Duplug | interaction prediction | turn service + `-turn-end-url` hook | measured (I0, I1) |
| Sortformer + multitalker Parakeet | speaker attribution | offline pipeline | see the perception section |
| Audio Flamingo 3 | audio observer | bounded observer service | see the perception section |
| DeepFilterNet | acoustic preprocessing | existing pre-ASR filter contract | measured (raw versus processed WER) |
| Hibiki, SeamlessStreaming | translation | sidecar / offline harness | see the task section |
| ELLSA, BayLing-Duplex | research extensions | feasibility only | see the task section |
| OpenAI Live, Gemini Live | closed live agents | existing `upstream` binding | profiles written; runs pending |

## Blocked and deferred

| Item | Status | Reason |
| --- | --- | --- |
| DuplexCascade checkpoint (cell C1 as released) | in progress | Gated-file access verified with saved HF credentials, revision `31c038ece2f006a28722dd60d1df3868fbb2cc42`. Checkpoint downloaded and all 10 file sizes verified; strict GPU loading and live ASR/TTS integration passed; public end-to-end transport failures under investigation |
| PersonaPlex 7B (cell N0 second half) | in progress | Gated-file access verified with saved HF credentials, revision `fdaf4090a61cb315c138a1faee287ffd6c716309`. Checkpoint downloaded and all 16 file sizes verified; upstream GPU audio inference and public live-profile smoke passed; full acceptance pending |
| User's own micro-turn LLM and TTS (cell C2) | pending | No checkpoint locations were provided |
| DuplexOmni | deferred | Upstream recommends eight H20 GPUs for low-latency serving |
| SALMONN-omni, OmniFlatten | deferred | No matching runnable release assets |

## What these numbers are not

- **Subsets, not campaigns.** Each end-to-end cell drives the same number of
  recordings from each FDB v1.5 category and a slice of FD-Bench. The bench
  itself reports NOT REPORTABLE for anything short of the full 498-task
  population, and nothing here should be quoted as a complete-cell result.
  One full campaign on the headline profile is the natural next step.
- **Measured on a shared, loaded machine.** Other users' jobs held the CPU at
  a load average of 90-250 throughout, and up to seven model integrations
  shared the GPU. Latencies and comparisons remain conditional on each run
  environment. Similar load averages do not establish equal scheduling
  pressure or remove this confound.
- **System comparisons, not causal ones.** A native speech model against this
  cascade differs in backbone, training and serving stack at once. Only the
  cells that change exactly one factor - the recogniser, the synthesiser, the
  endpoint predictor, the clock's triggers - are candidates for controlled
  comparisons, provided fixtures, repetitions and runtime conditions also match.
- **No quality judgement of what was said.** The suites score interaction
  timing and tool behaviour, and the component benchmarks score recognition
  and intelligibility. Whether an answer was a good answer is not measured
  here.
- **One voice, mostly English.** Mandarin appears in the recognition and
  synthesis components; the end-to-end suites are English. A recogniser's
  Mandarin error rate here does not describe a Mandarin conversation.

## Reproduce

```bash
deploy/duplex/services/llm-qwen3-8b.sh start      # and the other services a profile names
deploy/duplex/run-e2e.sh microturn-voxtral-qwen3-kyutai 10 12
python tools/duplexmodels/e2e_summary.py
```

`deploy/duplex/manifest.json` pins every model revision used;
`python tools/duplexmodels/manifest.py --out deploy/duplex/manifest.json`
regenerates it from the local cache.
