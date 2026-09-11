# End-to-end diagnostic findings — 2026-09-09

**Status: not validated for live conversation.** The audio-quality and isolated
latency results in README.md did not establish interruption behavior. Subsequent
pipeline diagnostics found competing-speech transcription leakage and a filter
deadline failure that terminated the graph session.

The first observations below describe the original synchronous client. The
[recovery follow-up](#recovery-follow-up) records the revised client and actual
playback diagnostics; historical failures are retained for comparison.

## What was exercised

The actual target-room graph, real target extraction service, Deepgram Nova-3,
Qwen-fast interaction model, Gemini 3.7 Flash client, and configured Fish TTS
path were used. The diagnostic profile changed Qwen's endpoint to the available
local bridge at `http://172.17.0.1:8000/v1`. No model responses were mocked.

The harness (`e2e/main.go`) uses the repository's public Realtime session client.
It sends a clean initial utterance in a stable Fish `user` voice, requesting a
five-part explanation. A different Fish `other` voice says:

> Stop talking. What is the capital of France?

The competitor is deliberately phrased as a direct request: acoustic extraction
must distinguish the voice, rather than relying on wording that a semantic
policy can easily dismiss as background conversation.

The playback-triggered test inserts that competing voice only after observing
at least 500 ms of agent activity in an 800 ms lookback, including recent
activity within 200 ms. A missed opportunity is not a pass. A separate
`competitor-idle` diagnostic inserts it at exactly 12 seconds regardless of
agent playback, to test transcription and downstream activation independently.

A local WebSocket relay (`e2e_trace.py`) requests ASR/policy/TTS debug events and
retains protocol events with timing. It omits audio payloads and credential
fields. These are diagnostic records, not an attested benchmark publication or
human listening assessment. Input and agent output PCM are retained separately
at 24 kHz by the harness.

## Observations

1. **The first attempt was blocked by unavailable Qwen.** The local model was
   restored from the repository's existing pinned deployment configuration.
   This was an infrastructure failure, not evidence for suppression.
2. **Gemini rejected requests with HTTP 400**, `FAILED_PRECONDITION`, reporting
   `User location is not supported for the API use.` No agent audio was produced.
   Both playback-triggered attempts therefore missed their injection opportunity.
   Neither background interruption resistance nor legitimate user-stop behavior
   was measured successfully. Fish synthesis of the test voices worked; agent
   TTS was never reached with a successful Gemini response.
3. **A filter timeout stopped the graph** in one playback-triggered attempt,
   and again in the first fixed-time attempt, which overlapped another test.
   These are session failures, not correctly suppressed speech. Repeated error
   events after a timeout are consequences of the failed session receiving more
   microphone packets; they are not separate independent timeout samples.
4. **The single-session fixed-time rerun exposed transcription leakage and
   another timeout.** Its event timeline follows.

| Input-clock time | Observed event |
|---|---|
| 0 s | Intended user begins the clean initial request |
| 12.000 s | Different voice begins “Stop talking. What is the capital of France?” |
| 14.631 s | Deepgram partial: “What is the cap” |
| 14.908 s | Deepgram final: “What is the capital of” |
| 15.165 s | Another Gemini request returns the location error |
| 22.062 s | Graph-session failure reported |
| 22.102 s | Error explicitly identifies the pre-ASR filter request exceeding its deadline |

The later Gemini error demonstrates an unwanted downstream request associated
with the competing-speaker transcript. It does not demonstrate a spoken wrong
answer: Gemini failed before producing agent speech. There were no target-user
utterances after the initial request in this diagnostic.

The 50 ms deadline was unchanged. With the original fail-closed design, one late
filter request can end the conversation. The single-session rerun establishes
that this failure is not limited to simultaneous harness sessions. The exact
cause of the latency spike has not been isolated; it must not be attributed to
Qwen, the GPU scheduler, or reference encoding without further measurements.

## Evidence retained in this workspace

- `.runtime/target-e2e-summary.json`: event summary, including timeout times and
  post-cue ASR observations.
- `.runtime/target-e2e-wire/session-004.jsonl`: single-session rerun wire trace.
- `.runtime/target-e2e-idle-single/trial-01.json`: harness result.
- `.runtime/target-e2e-idle-single/trial-01.input.pcm`: exact sent microphone PCM.
- `.runtime/target-e2e-idle-single/trial-01.agent.pcm`: empty, confirming there was
  no agent audio in this attempt.
- `.runtime/target-e2e-competitor/trial-01.json` and `trial-02.json`: missed
  playback opportunities and explicit infrastructure failures.
- `.runtime/target-e2e.launch.yaml`, `.graph.json`, `.values.json`: exact frozen
  diagnostic profile and graph.

## Reproduction

Start the target-room diagnostic gateway with the frozen profile and working
provider access, then:

```bash
python3 tools/targetvoice/e2e_trace.py \
  --upstream ws://127.0.0.1:18766/v1/realtime \
  --port 18767 --out .runtime/new-target-wire

go build -o .runtime/targetvoice-e2e ./tools/targetvoice/e2e
.runtime/targetvoice-e2e -url ws://127.0.0.1:18767/v1/realtime \
  -mode competitor -repeat 2 -out .runtime/new-target-competitor
.runtime/targetvoice-e2e -url ws://127.0.0.1:18767/v1/realtime \
  -mode user-stop -repeat 2 -out .runtime/new-target-user-stop
.runtime/targetvoice-e2e -url ws://127.0.0.1:18767/v1/realtime \
  -mode competitor-idle -out .runtime/new-target-idle
```

Use new output directories and run modes sequentially. A valid interruption
assessment requires observed agent audio and a fired cue, not merely zero
cancellation events. The harness stops subsequent repetitions after an
infrastructure error.

Before promoting this pipeline, resolve the provider access failure, address
filter latency spikes and their session-failure behavior, and repeat comparison
runs that measure competing-speaker transcript leakage, unwanted model calls,
false playback cancellation, successful target-user interruption, and response
latency. No end-to-end success rate or production-readiness claim is supported
by the current attempts.


## Recovery follow-up

The revised Go client replaces packets that miss the 50 ms delivery deadline
with silence, while a single ordered worker completes the sidecar state update.
It bounds outstanding audio and job lifetime at 500 ms, discards late output,
and latches a visible degraded/muted state on protocol failure, queue overflow,
or eight consecutive misses. It never falls back to raw PCM or re-enrolls after
activation. This change addresses session survival, not acoustic suppression.

Gemini still rejected the configured location. To exercise actual playback,
a separate diagnostic profile used the available Qwen model for **cognition as
well as interaction**, retaining Deepgram, the target filter, and Fish TTS.
This is not validation of the requested Gemini pipeline. Each mode ran once;
these results do not establish success rates.

| Check | Observation |
|---|---|
| Competitor during agent playback | Cue fired at 10.000 s with observed recent agent activity |
| Competitor transcript | “What is the” at 12.889 s |
| False interruption | Active response cancelled at 13.436 s, reason `turn_detected` |
| Session survival | No graph error; 18 late packets replaced by 1.8 s of silence; delivery recovered |
| Intended user stop during playback | Cue fired at 10.000 s; “Stop talking.” transcribed at 11.742 s |
| Intended interruption | Active response cancelled at 12.255 s, approximately 2.255 s after speech onset |

The target-absent competitor still caused a real false interruption. The user
stop worked in this single diagnostic, but with substantial delay. Muting late
packets can remove useful speech; keeping the graph alive is not sufficient for
conversation quality. The exact cause of the GPU latency spikes remains unisolated.

### Gate and alternative-model investigations

The following small diagnostics used the test voices, not a representative
speaker population. They reject proposed shortcuts on these fixtures; they do
not prove that target-presence detection is impossible.

- **Output/input energy retention:** the competitor's median RMS retention was
  1.009 versus 1.079 for held-out target speech. A volume threshold would pass
  substantial competing speech, while low-energy overlap could lose target speech.
- **Short-window reference comparison:** ECAPA cosine scores overlapped. At
  192 ms, target 10th/50th/90th percentiles were 0.070/0.216/0.332; competitor
  scores were 0.028/0.121/0.188. At 512 ms they were 0.207/0.413/0.498 versus
  0.090/0.150/0.321. Longer lookback also compromises the requested timing.
  These measurements do not support a reliable threshold gate.
- **Context-conditioned REAL-TSE checkpoint:** an offline strict-weight-load
  test retained competitor RMS at 0.829 versus 0.969 for the target. That
  candidate did not fix target-absent leakage in this test; it was not integrated.
- **CPU extraction under conversation load:** for 96 ms PCM packets, median/max
  processing were 200.8/321.4 ms with one thread, 115.6/143.3 ms with two, and
  68.1/75.2 ms with four (20 measured packets per setting). CPU relocation did
  not meet the 50 ms budget on this workload.

No energy or speaker-score gate was enabled. Reliable low-latency target-presence
inference remains an unresolved model requirement. The fixed first-three-second
reference and the pre-ASR placement remain unchanged. A qualified model must
be tested for target-absent suppression, quiet target speech, overlap, retained
intelligibility, and delivery latency before it can control interruption.

Follow-up artifacts in this workspace:

- `.runtime/target-recovery-competitor/trial-01.json` and associated input/agent PCM.
- `.runtime/target-recovery-user-stop/trial-01.json` and associated input/agent PCM.
- `.runtime/target-recovery-qwen-wire/session-001.jsonl` and `session-002.jsonl`.
- `.runtime/target-recovery-qwen.log`: delivery recovery and muted-duration logs.
- `.runtime/target-recovery-qwen.launch.yaml`, graph and values siblings: surrogate profile.
- `.runtime/retention-study.json`, `.runtime/short-presence-study.json`, and
  `.runtime/target-cpu-bench.json`: exploratory numerical results.

Regression coverage exercises ordered state continuation, discarded late audio,
fixed packet length, bounded backlog, repeated-deadline degradation, cancellation
on close, protocol-regression muting, and graph survival with preserved frame
metadata. These are transport tests; they do not certify acoustic quality.
