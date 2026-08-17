# Live provider benchmark — 2026-08-17–18

## Outcome

The completed live result has two complementary Full-Duplex-Bench v1.5 cells
for `gemini-3.1-flash-live-preview`:

- the entire official overlap population, 498/498 successful trials; and
- an 80/80 paired sensitivity matrix: 10 fixed examples per scenario streamed
  once with overlap and once with the matching clean input.

All 578 calls produced distinct output hashes. The full-population result is
the primary benchmark; the smaller paired cell isolates the effect of adding
the overlapping speaker to otherwise identical audio.

An equivalent live GPT-4o or Groq result could not be obtained from the
credentials available on this machine. Those cells remain unavailable rather
than being filled with another model:

| Requested system | Live result | Evidence |
| --- | --- | --- |
| Gemini 3.1 Flash Live Preview | 498/498 full overlap plus 80/80 paired sensitivity trials | Local aligned WAVs, hashes, secret-free traces, manifests, and summaries |
| GPT-4o Realtime `2025-06-03` | Not measured | Realtime server returned `model_not_found` |
| GPT-4o Realtime alias | Not measured | Realtime server returned `model_not_found` |
| GPT-Realtime 1.5 availability check | Not measured | Realtime server returned `billing_not_active`; it was never relabeled as GPT-4o |
| Groq voice cascade | Not measured | `GROQ_API_KEY` was unavailable; Groq's documented voice design is a cascade, not a native “Groq Live” audio protocol |

The three OpenAI cells were re-probed between 2026-08-17T17:15:15Z and
17:15:18Z (August 18 in Singapore), one bounded attempt each. OpenAI's model
page still lists the GPT-4o alias and snapshots in its deprecated section;
`model_not_found` is therefore recorded as an account-specific observed error,
not generalized into a claim that no account can access them.

To preserve a genuine GPT-4o comparison, the repository pins the historical
GPT-4o Realtime row from Full-Duplex-Bench v1.5 Table 2 separately from local
measurements. The paper used `gpt-4o-realtime-preview-2024-12-17` with the
`alloy` voice.

## Protocol and sampling

The external dataset is pinned at Full-Duplex-Bench revision
`3e799c45a045256f47d5f1c9cda90157e2d2ec9e`. Its four official archives contain
498 complete pairs: 100 background-speech, 100 side-conversation, 98
backchannel, and 200 interruption pairs. The upstream documentation says 99
backchannel pairs; the downloaded valid archive contains 98.

The primary cell uses every complete overlap input in the archive, one
replicate each and no concurrency. The paired sensitivity cell was fixed before
inspecting outcomes: ten examples per scenario selected at evenly spaced
indices after stable lexical ID ordering. Every input was sent as real-time
paced PCM16 mono in 1,024-sample, 16 kHz frames. Provider output was treated as
immediate playback at 24 kHz, queued audio was flushed on an `interrupted`
message, and the stored evaluator WAV was padded or cropped to the input's exact
timeline.

The Gemini setup matches the pinned upstream 3.1 runner's minimum policy:
native audio output, explicit `minimal` thinking, no extra system instruction,
and no transcription service. A provider interruption or turn-completion
rotates to a fresh Live session and continues with the next unsent input frame.
The run used raw JSON WebSockets rather than a Python SDK.

The full population ran from 2026-08-17T14:23:03Z through
2026-08-17T17:13:55Z, or 22:23:03 on August 17 through 01:13:55 on August 18
in Asia/Singapore, on macOS 26.3 arm64. The earlier paired cell ran from
13:33:26Z through 14:02:00Z. All four full manifests contain zero terminal
failures and 498 unique trial/output hashes.

The retry ledger covers 497 of 498 full trials; one successful background
canary predates the ledger and was hash-verified on resume. The ledger records
504 attempts: 497 successes and seven transient EOF failures across six trials,
all recovered within the three-attempt bound. Successful sessions additionally
record 11 internal connection retries. In total, the 498 trials used 981 Live
sessions because the pinned upstream policy rotates after provider
turn-completion or interruption. The 200-trial interruption population needed
neither outer nor internal retries.

## Full-population Gemini observations

The Go scorer follows the upstream `get_timing.py` interval and millisecond
de-duplication rules, but uses the explicitly named
`openrealtime-energy-vad-v2-fdb-timing` instead of claiming Silero equivalence.
Values below are milliseconds; intervals are deterministic 10,000-resample
percentile-bootstrap 95% confidence intervals for the mean.

| Scenario | Speech during annotated overlap | Mean overlap speech (95% CI) | Mean stop latency (95% CI) | Mean response latency (95% CI) |
| --- | ---: | ---: | ---: | ---: |
| User interruption | 199/200 | 1,227 (1,093–1,374) | 811 (703–925), n=198 | 2,051 (2,004–2,099), n=200 |
| User backchannel | 97/98 | 620 (590–649) | 542 (514–569), n=97 | 1,775 (1,736–1,814), n=97 |
| Talking to other | 98/100 | 1,135 (1,004–1,265) | 978 (854–1,109), n=98 | 2,095 (2,014–2,179), n=99 |
| Background speech | 99/100 | 657 (570–745) | 433 (361–512), n=96 | 1,820 (1,710–1,932), n=100 |

Speech was detected somewhere in 493/498 annotated overlap windows. This is
not a success rate: holding the floor through a backchannel can be correct,
while continuing through a genuine interruption can be wrong. The timing
metrics describe behavior but do not identify the addressee or meaning.

The full inputs contain 112.699 minutes of audio and the provider emitted
117.550 minutes. At Google's 2026-08-18 paid list rates of $0.005 input and
$0.018 output per audio minute, that is a nominal $2.68 equivalent before
failed-attempt overhead. It is not an observed bill: free-tier audio is listed
as free and usage metadata was complete in 0/498 trials because session
rotation closed before the usage event.

## Paired clean sensitivity cell

The 40-pair, 80-trial cell clarifies what changed at the same point in the
conversation:

| Scenario | First-audio change, overlap − clean (95% CI) | Speech in annotated window, overlap − clean (95% CI) | Response-latency change (95% CI) |
| --- | ---: | ---: | ---: |
| User interruption | +121 ms (−22 to +294) | −1,721 ms (−2,299 to −1,083) | −361 ms (−1,933 to +556) |
| User backchannel | −103 ms (−257 to +38) | −132 ms (−269 to −34) | −8 ms (−168 to +142) |
| Talking to other | −44 ms (−173 to +73) | −863 ms (−1,269 to −465) | +312 ms (+97 to +560) |
| Background speech | +8 ms (−229 to +256) | −249 ms (−563 to 0) | +304 ms (−190 to +795) |

All first-audio confidence intervals cross zero, so this sample does not show a
reliable change in initial model-speech onset between paired conditions. The
model continued speaking somewhere inside 39 of 40 annotated overlap windows,
but produced materially less speech in the interruption, side-conversation,
and backchannel windows than in their clean controls. Side-conversation was the
only scenario whose paired response-gap increase excluded zero.

This timing behavior is not itself a semantic success score. Continuing through
a backchannel, side conversation, or background speaker can be desirable; doing
so through a genuine user interruption is usually undesirable. ASR-aligned
categorical labels are required to distinguish Respond, Resume, Uncertain, and
Unknown behavior.

## Historical GPT-4o context

The following is a contextual comparison, not a same-date leaderboard. GPT-4o
values are the paper's full benchmark result using Silero-VAD; local Gemini
values use the full overlap population and energy VAD described above.
The paper's Gemini column is the older `gemini-2.0-flash-live-001` system.

| Scenario | Local Gemini 3.1 stop / response (s) | Published GPT-4o stop / response (s) | Published Gemini 2.0 stop / response (s) |
| --- | ---: | ---: | ---: |
| User interruption | 0.81 / 2.05 | 0.23 / 1.50 | 2.20 / 2.62 |
| User backchannel | 0.54 / 1.78 | 0.21 / 1.32 | 0.66 / 2.45 |
| Talking to other | 0.98 / 2.10 | 0.18 / 1.16 | 1.69 / 1.78 |
| Background speech | 0.43 / 1.82 | 0.18 / 1.26 | 0.95 / 2.38 |

The paper found historical GPT-4o highly responsive to real interruptions
(Respond 0.78), but also likely to respond when it should hold the floor:
Respond 0.91 for side conversation and 0.93 for background speech. That is a
useful warning against optimizing stop latency alone. The local Gemini result
appears less immediately yielding than that historical GPT-4o row, but no claim
about better addressee detection is justified without the missing local
categorical judge.

## What was not claimed

- No provider-billed cost is claimed. The $2.68 figure is a duration-based paid
  list-price equivalent, not an invoice; complete Gemini usage was captured in
  0/498 full trials and 2/80 paired trials.
- The GPT-4o transcript judge, Parakeet-TDT ASR alignment, prosodic adaptation,
  and UTMOSv2 were not run. Their absence is preferable to substituting an
  undisclosed evaluator.
- TOBench was assessed for fit but not run. It measures general closed-loop
  omni-modal MCP workflows, not realtime duplex voice behavior, and its
  official harness requires Python 3.12 plus a large mixed MCP/Node setup.
- TREX FT-Bench was also assessed: it measures autonomous LLM fine-tuning over
  ten training tasks, not live voice. The similarly named FD-Bench and LiveKit
  eot-bench are relevant voice references, but their official Python harnesses
  respectively measure a separate full-duplex pipeline and an endpoint-detector
  component. Neither is relabeled as an end-to-end result from this runner.
- Full-Duplex-Bench v3 tool-use definitions are pinned, but v3 is not reported
  as run. A valid result additionally requires its separate audio bundle,
  LiveKit-equivalent orchestration, mock-tool execution, ASR, and a declared
  judge policy.
- Groq is not xAI Grok. The Groq adapter is explicitly a current
  STT→LLM→TTS cascade (`whisper-large-v3-turbo`, `openai/gpt-oss-120b`, and
  the preview `canopylabs/orpheus-v1-english`), and no live score exists without
  a key.

## Reproduction and protocol compatibility

`cmd/livebench` is production Go with no Python runtime or build dependency. It
supports corpus inspection, resumable live runs, offline rescoring, deterministic
bootstrap summaries, secret-free event traces, exact provider/model profiles,
and atomic manifests. Local WebSocket tests cover OpenAI GA Realtime message
shape/auth/audio events and Gemini binary/text frames, header credential
isolation, interruption flush, and multi-session continuation. The repository's
existing generated OpenAI protocol layer remains the authoritative full-event
compatibility boundary: all 133 pinned profile/direction definitions and their
schema closure are exercised by the conformance suite.

The pinned `openai/openai-openapi` revision
`2186421dca0cca7c1e67caa7739005e8b1ccc4dd` was still upstream HEAD on
2026-08-18. Refetching it, verifying its SHA-256, and regenerating both the Go
registry and schema produced byte-for-byte matches.

The generated full-population summary is checked in as
`benchmarks/results/fdb15-gemini-full-overlap-2026-08-17-18.json`; the paired
cell and provider-availability record remains
`benchmarks/results/fdb15-live-provider-2026-08-17.json`. Full local manifests,
WAVs, and traces remain under the ignored
`artifacts/livebench-gemini-fdb15-full-overlap/` and
`artifacts/livebench-gemini-fdb15-aligned/` trees because third-party inputs
and large generated media are not redistributed.

## Sources

- [Full-Duplex-Bench repository](https://github.com/DanielLin94144/Full-Duplex-Bench)
- [Full-Duplex-Bench v1.5 paper and Table 2](https://arxiv.org/html/2507.23159)
- [OpenAI GPT-4o Realtime model and deprecated snapshots](https://developers.openai.com/api/docs/models/gpt-4o-realtime-preview)
- [OpenAI GPT-Realtime 1.5 model](https://developers.openai.com/api/docs/models/gpt-realtime-1.5)
- [Gemini Live API technical specifications](https://ai.google.dev/gemini-api/docs/live-api)
- [Gemini raw WebSocket protocol guide](https://ai.google.dev/gemini-api/docs/live-api/get-started-websocket)
- [Gemini API pricing](https://ai.google.dev/gemini-api/docs/pricing)
- [Groq supported production models](https://console.groq.com/docs/models)
- [Groq Orpheus TTS and voice IDs](https://console.groq.com/docs/text-to-speech/orpheus)
- [TOBench official overview](https://williamiiliu.github.io/tobench_online/)
- [TREX FT-Bench paper](https://arxiv.org/abs/2604.14116)
- [FD-Bench repository](https://github.com/pengyizhou/FD-Bench)
- [LiveKit eot-bench repository](https://github.com/livekit/eot-bench)
