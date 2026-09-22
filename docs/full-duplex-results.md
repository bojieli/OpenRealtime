# Streaming and full-duplex integration: results

**Executed:** 2026-09-22 on `rtx-pro` (one RTX PRO 6000 Blackwell, 96 GB,
driver 595.91.07). **Plan:** [streaming and full-duplex components](full-duplex-streaming-plan.md)
and the [open-model survey](open-duplex-models-survey.md). **Status:** first
execution pass of stages P0–P9; every number below is a component or smoke
measurement on this host, not a release-grade campaign.

**Host conditions.** The machine is shared. Throughout the runs, other users'
EDA jobs (yosys/OpenROAD) kept the 32-core CPU at a load average of roughly
90–250, and up to seven model-integration jobs shared the GPU under a single
large-model lease. Latency numbers are therefore upper bounds for this
hardware; every result file records the load average it was measured under.
Faster-than-real-time throughput was never substituted for wall-clock replay.

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

RESULTS_TTS

## Micro-turn language model (P4)

The micro-turn cascade runs as one duplex model
(`sidecars/microturn_sidecar.py`): streaming ASR, a controller asked every
500 ms, a streamed answer, and incremental synthesis, with its own floor.
DuplexCascade's released checkpoint is gated, so the controller is an ordinary
instruct model (Qwen3-8B) driven through the same protocol - the plan's
"orchestrated micro-turns" fallback, and the sidecar refuses to pretend
otherwise.

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
moment the user goes quiet rather than at the next tick. Before them the
controller answered half-sentences and then stopped itself - FD-Bench answered
turns went from 31/55 to 30/36 with no premature starts when they were added.

## Native speech models (P5)

RESULTS_NATIVE

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

RESULTS_PERCEPTION

## End-to-end voice agent performance (P9)

Each cell is one profile driven through the public Realtime protocol: the same
number of FDB v1.5 recordings from each of its four categories, then an
FD-Bench slice. "n/a" recordings are those where the agent was not speaking
when the event arrived, which is a latency failure of a different kind and is
counted separately rather than scored.

<!-- generated: e2e -->
| profile | interrupt yield | backchannel hold | background hold | other-talk hold | yield p50 ms | FD-Bench answered/turns | premature | resp p50 ms | load |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| microturn-clock-only | 0/8 (+0 n/a) | - | - | - | 1854 | - | - | - | 222.01 200.98 160.93 |
| microturn-clock-only | **INCOMPLETE (no finished.json)** | | | | | | | | |
| microturn-voxtral-qwen3-kyutai | 8/8 (+0 n/a) | 7/8 (+0 n/a) | 7/7 (+1 n/a) | 7/7 (+1 n/a) | 55 | 31/55 | 0 | 1556 | 145.43 146.51 120.52 |
| microturn-voxtral-qwen3-kyutai | **MIXED (results newer than the run that finished)** | | | | | | | | |
<!-- end generated -->

The clock's own evidence for every traced run:

<!-- generated: microturn -->
| trace | sessions | ticks | evidence wait p50/p90 ms | decision p50/p90 ms | over tick | decisions | triggers |
| --- | --- | --- | --- | --- | --- | --- | --- |
| microturn-clock-only.jsonl | 14 | 787 | 229/449 | 116/182 | 0 | {'continue': 570, 'idle': 110, 'wait': 61, 'respond': 32, 'stop': 14} | {'clock': 787} |
| microturn-smoke.jsonl | 4 | 216 | 238/413 | 108/188 | 0 | {'continue': 148, 'wait': 25, 'idle': 22, 'respond': 12, 'stop': 8, 'backchannel': 1} | {'clock': 216} |
| microturn-smoke2.jsonl | 6 | 301 | 266/448 | 96/136 | 0 | {'continue': 139, 'wait': 85, 'idle': 45, 'respond': 19, 'stop': 13} | {'clock': 301} |
| microturn-smoke3.jsonl | 6 | 266 | 249/466 | 97/149 | 0 | {'continue': 129, 'idle': 82, 'wait': 27, 'respond': 18, 'stop': 9, 'backchannel': 1} | {'clock': 266} |
| microturn-smoke4.jsonl | 6 | 304 | 186/447 | 131/212 | 0 | {'continue': 196, 'idle': 42, 'wait': 26, 'respond': 21, 'stop': 17, 'backchannel': 2} | {'clock': 273, 'word': 31} |
| microturn-smoke5.jsonl | 12 | 623 | 161/380 | 143/203 | 0 | {'continue': 335, 'idle': 154, 'wait': 50, 'respond': 48, 'stop': 36} | {'clock': 576, 'word': 47} |
| microturn-voxtral-qwen3-kyutai.jsonl | 84 | 5017 | 175/422 | 123/198 | 0 | {'continue': 2616, 'idle': 1121, 'wait': 602, 'respond': 384, 'stop': 294} | {'clock': 4540, 'word': 416, 'pause': 61} |
| mt-fdbench2.jsonl | 8 | 1004 | 244/459 | 160/239 | 0 | {'continue': 572, 'wait': 206, 'idle': 130, 'respond': 55, 'stop': 41} | {'clock': 921, 'word': 83} |
| mt-fdbench3.jsonl | 8 | 1039 | 180/458 | 93/163 | 0 | {'continue': 557, 'wait': 210, 'idle': 137, 'respond': 73, 'stop': 62} | {'clock': 866, 'word': 114, 'pause': 59} |
<!-- end generated -->

## Blocked and deferred

| Item | Status | Reason |
| --- | --- | --- |
| DuplexCascade checkpoint (cell C1 as released) | blocked | `sbintuitions/DuplexCascade` is gated on the Hub and this account is not authorised; accepting its terms is the account owner's decision. The micro-turn sidecar refuses `--llm duplexcascade` with that reason and runs the orchestrated fallback |
| PersonaPlex 7B (cell N0 second half) | blocked | `nvidia/personaplex-7b-v1` gated the same way |
| User's own micro-turn LLM and TTS (cell C2) | pending | No checkpoint locations were provided |
| DuplexOmni | deferred | Upstream recommends eight H20 GPUs for low-latency serving |
| SALMONN-omni, OmniFlatten | deferred | No matching runnable release assets |

## Reproduce

```bash
deploy/duplex/services/llm-qwen3-8b.sh start      # and the other services a profile names
deploy/duplex/run-e2e.sh microturn-voxtral-qwen3-kyutai 10 12
python tools/duplexmodels/e2e_summary.py
```

`deploy/duplex/manifest.json` pins every model revision used;
`python tools/duplexmodels/manifest.py --out deploy/duplex/manifest.json`
regenerates it from the local cache.
