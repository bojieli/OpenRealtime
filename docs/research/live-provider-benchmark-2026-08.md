# Live provider benchmark — 2026-08-17

## Outcome

The completed live result is an 80-trial Full-Duplex-Bench v1.5 matrix for
`gemini-3.1-flash-live-preview`: 10 deterministically selected paired examples
from each of four overlap scenarios, each streamed once with overlap and once
with the paired clean input. All 80 final trials succeeded and produced unique
output hashes.

An equivalent live GPT-4o or Groq result could not be obtained from the
credentials available on this machine. Those cells remain unavailable rather
than being filled with another model:

| Requested system | Live result | Evidence |
| --- | --- | --- |
| Gemini 3.1 Flash Live Preview | 80/80 complete | Local aligned WAVs, hashes, secret-free traces, manifests, and summary |
| GPT-4o Realtime `2025-06-03` | Not measured | Realtime server returned `model_not_found` |
| GPT-4o Realtime alias | Not measured | Realtime server returned `model_not_found` |
| GPT-Realtime 1.5 availability check | Not measured | Realtime server returned `billing_not_active`; it was never relabeled as GPT-4o |
| Groq voice cascade | Not measured | `GROQ_API_KEY` was unavailable; Groq's documented voice design is a cascade, not a native “Groq Live” audio protocol |

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

The sample was fixed before inspecting outcomes. Ten examples per scenario were
selected at evenly spaced indices after stable lexical ID ordering. Every input
was sent as real-time paced PCM16 mono in 1,024-sample, 16 kHz frames. Provider
output was treated as immediate playback at 24 kHz, queued audio was flushed on
an `interrupted` message, and the stored evaluator WAV was padded or cropped to
the input's exact timeline.

The Gemini setup matches the pinned upstream 3.1 runner's minimum policy:
native audio output, explicit `minimal` thinking, no extra system instruction,
and no transcription service. A provider interruption or turn-completion
rotates to a fresh Live session and continues with the next unsent input frame.
The run used raw JSON WebSockets rather than a Python SDK.

The final sample comprises 40 pairs and 80 trials between
2026-08-17T13:33:26Z and 2026-08-17T14:02:00Z on macOS 26.3 arm64 from the
Asia/Singapore timezone. The four final manifests contain zero terminal
failures. Transient TLS establishment failures occurred during collection and
were replayed by exact profile/hash; three successful interruption trials also
record an internal bounded connection retry.

## Local Gemini observations

The Go scorer follows the upstream `get_timing.py` interval and millisecond
de-duplication rules, but uses the explicitly named
`openrealtime-energy-vad-v2-fdb-timing` instead of claiming Silero equivalence.
Values below are milliseconds; intervals are deterministic 10,000-resample
percentile-bootstrap 95% confidence intervals for the mean.

| Scenario | Speech present during annotated overlap | Mean stop latency (95% CI) | Mean response latency (95% CI) |
| --- | ---: | ---: | ---: |
| User interruption | 9/10 | 711 (582–842), n=9 | 2,239 (1,892–2,633), n=10 |
| User backchannel | 10/10 | 580 (500–674), n=10 | 1,974 (1,830–2,128), n=10 |
| Talking to other | 10/10 | 968 (628–1,358), n=10 | 2,120 (1,894–2,360), n=10 |
| Background speech | 10/10 | 930 (772–1,098), n=10 | 2,234 (1,867–2,626), n=10 |

The paired clean control clarifies what changed at the same point in the
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
values use the smaller deterministic sample and energy VAD described above.
The paper's Gemini column is the older `gemini-2.0-flash-live-001` system.

| Scenario | Local Gemini 3.1 stop / response (s) | Published GPT-4o stop / response (s) | Published Gemini 2.0 stop / response (s) |
| --- | ---: | ---: | ---: |
| User interruption | 0.71 / 2.24 | 0.23 / 1.50 | 2.20 / 2.62 |
| User backchannel | 0.58 / 1.97 | 0.21 / 1.32 | 0.66 / 2.45 |
| Talking to other | 0.97 / 2.12 | 0.18 / 1.16 | 1.69 / 1.78 |
| Background speech | 0.93 / 2.23 | 0.18 / 1.26 | 0.95 / 2.38 |

The paper found historical GPT-4o highly responsive to real interruptions
(Respond 0.78), but also likely to respond when it should hold the floor:
Respond 0.91 for side conversation and 0.93 for background speech. That is a
useful warning against optimizing stop latency alone. The local Gemini sample
appears less immediately yielding than that historical GPT-4o row, but no claim
about better addressee detection is justified without the missing local
categorical judge.

## What was not claimed

- Token/cost results are omitted. The upstream session-rotation policy often
  closes after `turnComplete` before Gemini's usage event; complete usage was
  captured in only 2 of 80 trials.
- The GPT-4o transcript judge, Parakeet-TDT ASR alignment, prosodic adaptation,
  and UTMOSv2 were not run. Their absence is preferable to substituting an
  undisclosed evaluator.
- TOBench was assessed for fit but not run. It measures general closed-loop
  omni-modal MCP workflows, not realtime duplex voice behavior, and its
  official harness requires Python 3.12 plus a large mixed MCP/Node setup.
- Full-Duplex-Bench v3 tool-use definitions are pinned, but v3 is not reported
  as run. A valid result additionally requires its separate audio bundle,
  LiveKit-equivalent orchestration, mock-tool execution, ASR, and a declared
  judge policy.
- Groq is not xAI Grok. The production Groq adapter is explicitly a current
  STT→LLM→TTS cascade (`whisper-large-v3-turbo`, `openai/gpt-oss-120b`, and
  `canopylabs/orpheus-v1-english`), and no live score exists without a key.

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

The concise checked-in result is
`benchmarks/results/fdb15-live-provider-2026-08-17.json`. Full local manifests,
WAVs, and the complete summary remain under the ignored
`artifacts/livebench-gemini-fdb15-aligned/` tree because third-party inputs and
large generated media are not redistributed.

## Sources

- [Full-Duplex-Bench repository](https://github.com/DanielLin94144/Full-Duplex-Bench)
- [Full-Duplex-Bench v1.5 paper and Table 2](https://arxiv.org/html/2507.23159)
- [OpenAI GPT-4o Realtime model and deprecated snapshots](https://developers.openai.com/api/docs/models/gpt-4o-realtime-preview)
- [OpenAI GPT-Realtime 1.5 model](https://developers.openai.com/api/docs/models/gpt-realtime-1.5)
- [Gemini Live API technical specifications](https://ai.google.dev/gemini-api/docs/live-api)
- [Gemini raw WebSocket protocol guide](https://ai.google.dev/gemini-api/docs/live-api/get-started-websocket)
- [Groq supported production models](https://console.groq.com/docs/models)
- [Groq Orpheus TTS and voice IDs](https://console.groq.com/docs/text-to-speech/orpheus)
- [TOBench official overview](https://williamiiliu.github.io/tobench_online/)
