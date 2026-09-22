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

RESULTS_ASR

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

RESULTS_MICROTURN

## Native speech models (P5)

RESULTS_NATIVE

## Interaction prediction (P6)

RESULTS_INTERACTION

## Perception and task extensions (P7, P8)

RESULTS_PERCEPTION

## End-to-end voice agent performance (P9)

RESULTS_E2E

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
