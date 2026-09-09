# Room latency profile — September 9, 2026

The room previously used Gemini 3.5 Flash with a requested 512-token thinking
budget and 1,024-token total output ceiling. The new candidate uses Gemini 3.7
Flash with 128 thinking tokens and retains the 1,024-token output ceiling.
Fish Speech 1.5 remains the synthesizer. The model and budget choice follows the
measurements below; a budget is an allowance, not a measurement of tokens used.

## Where the original audio wait went

Four retained successful adaptive caller turns from September 8 have complete
client-clock boundaries. They averaged 2,816 ms from input end to audible output.

| Observed interval | Mean | Range |
| --- | ---: | ---: |
| Input end → final transcript | 387 ms | 233–561 ms |
| Final transcript → semantic admission | 198 ms | 148–229 ms |
| Admission → first usable model text | 1,689 ms | 1,594–1,880 ms |
| First text → synthesis start | 44 ms | 1–66 ms |
| Synthesis start → first audio packet | 408 ms | 265–519 ms |
| First audio packet → audible PCM | 90 ms | 60–112 ms |

About 60% was in the admission-to-text interval. This includes context assembly,
model HTTP/network time, thinking/generation, and text quarantine; it is not a
pure GPU-compute timer. The TTS interval similarly includes playback plumbing.
All boundaries use the same client monotonic clock and add to each turn's audio
latency. The input pump has 100 ms frame granularity; no physical speaker or
WebRTC-device latency is claimed. This is a small sample, not a production p90.

The older [latency record](latency.md) measured other configurations. Its claim
that endpointing dominated must not be transferred to this room configuration.

## Controlled model replay

The authenticated provider model catalog confirmed all five model IDs below.
Three exact captured contexts (France, Japan after a preceding turn, and a
substantive reply about restarting a router) were replayed three times for every
model/budget pair. All 135 requests used streaming SSE, temperature zero, the
same 1,024-token output ceiling, and a seeded randomized execution order. A
persistent HTTP session was used. Time is request start to the first nonempty,
non-thinking text, not response headers or the completed answer.

Each accepted cell below contains nine samples. Mean first-text latency:

| Model | Requested budget 0 | Requested budget 128 | Requested budget 512 |
| --- | ---: | ---: | ---: |
| gemini-3-flash-preview | 638 ms | 702 ms | 686 ms |
| gemini-3.5-flash | 873 ms | 1289 ms | 1433 ms |
| gemini-3.6-flash | Rejected (HTTP 400) | 943 ms | 1109 ms |
| gemini-3.7-flash | 576 ms | 570 ms | 591 ms |
| gemini-3.8-flash | 858 ms | 690 ms | 1305 ms |

Gemini 3.7/128 ranged from 467 to 640 ms; its median was 580 ms. Gemini 3.5/512
had median 1,216 ms and maximum 3,552 ms. Gemini 3.8/512 had median 645 ms but a
6,065 ms maximum. These outliers are retained. The test establishes observed
behavior on this endpoint, not a permanent ranking of model families.

Gemini 3.6 rejected budget zero with `INVALID_ARGUMENT` in all nine requests.
Other combinations completed all nine requests and passed nonempty/completion
and expected-fact checks. Accepted numeric budgets are reported as requested;
the provider did not return thought-token counts for every model, so acceptance
does not prove identical reasoning behavior across model families.

An independent Gemini review evaluated 30 unique answers without model names or
timing data. It flagged two correct single-clause capital answers for not putting
the fact in the first clause; inspection finds those flags invalid because each
answer has only one clause. It also flagged the original 3 Flash preview at
budget zero for moving to an Ethernet fallback before the caller completed the
router restart. The proposed 3.7/128 answers had no other flagged issues. This
narrow review does not certify complex reasoning, tool use, or all room scenarios.

## Fish Speech 1.5 measured directly

The running process and `/health` identify checkpoint
`275a984d33c33659e39eed41ff5bcd6e67517f4c` from `fishaudio/fish-speech-1.5`.
Fifteen local streaming requests used the existing default voice and 200-character
chunk setting: five requests for each text, randomized. The probe read past the
44-byte WAV header before timing the first PCM sample. HTTP headers alone are
not audio.

| Text | Mean first PCM | Range |
| --- | ---: | ---: |
| Yes, I can help. | 194 ms | 153–223 ms |
| The capital of France is Paris. | 234 ms | 183–284 ms |
| I would be happy to help you troubleshoot your slow Wi-Fi connection today. | 340 ms | 277–399 ms |

The approximately 200 ms figure is achievable for short replies. Longer input,
voice generation, leading silence, and playback add cost; changing the model
name to Fish 1.5 cannot help because this room already runs that checkpoint.

## Repeatability and evidence

Use `scripts/room-latency-profile.py DIRECTORY --output profile.json` on a
`room-selftest.py` recording. Missing answers and interrupted/incomplete paths
are explicitly retained as exclusions rather than converted into zero latency.
Use `scripts/room-model-latency.py --cases cases.json --output new-matrix.jsonl`
with `GEMINI_API_KEY` and any required proxy environment. The case file is an
array of `{name, expected, body}` with exact captured Gemini request bodies.
`--models`, `--budgets`, and `--repeats` configure the matrix.

Private raw evidence is retained under `.runtime/room-latency-20260909/`, including
the catalog, request cases, all model responses and usage reports, blinded review
and mapping, Fish measurements, and the original timestamp breakdown. Do not
commit private context traces or credentials.

The first fresh five-turn run on September 9 failed because the local Qwen
policy service at `127.0.0.1:8000` was down. Recognition was correct, but model
admission returned connection-refused errors. That failed run is retained and
is not included in latency averages. The documented Qwen service command was
restored; health and the served model identity were verified before candidate
audio testing. Startup loaded roughly 30 GiB of weights and compiled CUDA graphs;
that startup interval is not interactive response latency.

## Complete audio-path validation

The Gemini 3.7/128 candidate answered all five adaptive caller turns. Their
input-end to audible-response times were **2,005, 2,124, 1,677, 1,394, and
1,758 ms**: mean **1,792 ms**, median **1,758 ms**. These are different adaptive
utterances from the retained baseline, so the observed reduction from 2,816 ms
is not a controlled whole-pipeline A/B estimate. The model replay above is the
controlled comparison.

Four of these five turns have nonoverlapping observable stage boundaries:

| Observed interval | Mean |
| --- | ---: |
| Input end → final transcript | 329 ms |
| Final transcript → admission | 199 ms |
| Admission → first usable text | 833 ms |
| Text → synthesis start | 15 ms |
| Synthesis → first audio packet | 360 ms |
| First packet → audible PCM | 155 ms |

The fourth turn had overlapping/reordered stage events and was excluded only
from the stage means; its 1,394 ms total remains in the five-turn result. The
stage subset averages 1,891 ms. Faster text alone therefore cannot make total
response latency 200 ms: recognition/finalization, several policy decisions,
synthesis, and audible onset still add roughly another second.

The first qualitative audio review mistook generated text from a cancelled
response for promised delivered speech. Recorded protocol events mark that
response `cancelled` and contain no audio for it. The diagnostic now annotates
response IDs, cancellation/completion status, and whether audio was received;
the simulated caller excludes cancelled text from its heard conversation.
Original review and unannotated evidence are retained alongside the annotated
review. This changes diagnostic attribution, not the underlying recording.

The candidate stop-and-follow-up test passed: acoustic cessation was **960 ms**
after interruption onset, and the next question received audible output **947 ms**
after input ended. The first acknowledgement run preserved the original response
and supplied the full explanation, but failed its 500 ms pause bound with a
**640 ms** gap. This acoustic-flow failure is retained; it is not reported as a
fully passing acknowledgement test merely because answer correctness passed.

A repeat acknowledgement run had a 540 ms pause and exposed a streamed-word
join (`return labelto`). The segmenter's batch splitter had trimmed whitespace
from the incomplete remainder before the next model delta arrived. The fix
preserves trailing whitespace (including newlines and Unicode spaces), with
regression coverage. The scenario's minimum clause length is also increased
from 12 to 80 characters to avoid separate TTS requests for short comma clauses;
short complete sentences still release immediately. The timing cost and voice
flow of this final speech configuration require live validation below.


The first run with the whitespace/clause change passed the narrow waveform
checks (340/280 ms acknowledgement gaps), but Gemini's audio review found
missing policy clauses. An isolated-agent-channel review confirmed the omitted
thirty-day eligibility condition and damage photograph instruction. That run
is therefore **not an end-to-end pass**, despite the deterministic score.

The server log identified `provider chunk offset is 0, want ...`. The
`bysentence` adapter forwarded the second inner synthesis request's zero-based
sample offsets unchanged. The TTS element correctly rejected the discontinuity,
losing the rest of the sentence. The adapter now rebases offsets across requests
and assigns unique chunk IDs, while preserving the final marker only on the last
piece. A regression test uses an inner provider that restarts offsets and IDs
for each request. Fish itself was not resetting offsets within one request.
Original failures, stereo review, isolated-channel review, and logs are retained.


With the offset fix, the new-question-during-speech test passed deterministic
checks and Gemini audio review: first response 1,657 ms, new-question response
1,431 ms. The stop/silence/follow-up test also passed both checks: cessation
1,260 ms after interruption onset, follow-up response 1,580 ms. This is functional
barge-in, but is not a sub-200 ms interruption claim.

The first acknowledgement run after the offset fix delivered every policy
clause and held across acknowledgements (0/340 ms gaps), but failed the initial
5,000 ms response bound with **5,839 ms**. The model request began about 5.4 s
into the recording and the first delivered text appeared at 10.5 s. This
long-context tail was not represented by the three short replay contexts; the
small matrix's 640 ms maximum for 3.7/128 is not a universal upper bound.
The failed timing run and Gemini's failure verdict are retained.


The repeated acknowledgement run answered in **1,524 ms** and passed the
waveform and content checks; acknowledgement gaps were **160/340 ms**.
The earlier 5,839 ms tail remains a reported failure, not a discarded warm-up.
Gemini's independent audio review also passed the repeat, including answer
completeness, clarification handling, acknowledgements, and voice flow.

Validation includes the complete `cmd/openrealtime` package, interaction and
scenario graph tests, speech/Fish/sentence-adapter tests, targeted race checks,
and Python profiler unit tests and lint. These are live diagnostic scenarios,
not a tau-bench certification or a statistical production reliability claim.

## Deployment verification and fragment recovery

Build `03855603` was pushed and deployed with the previous binary, environment,
and user-service definition backed up privately. Public API health and the
authenticated browser returned HTTP 200. A fresh adaptive conversation then
exposed a further failure: the caller's trailing "if that makes a difference"
cancelled a reply after "Yes", and the policy treated the question as answered.
This deployment check is retained as a failure, not an end-to-end success.

The unanswered-request boundary now requires a complete spoken utterance when
an exact playback mark is available. A clipped reply leaves the same-speaker
question and its trailing clarification together for the existing narrow
unanswered-request guard. Explicit stop/silence instructions still require wait.
A regression covers the exact question/partial-answer/tail sequence.

The diagnostic also now waits for completed responses and measures their audio,
excluding cancelled response IDs. Its earlier 3 ms onset on the first deployed
turn was leftover cancelled output, not a three-millisecond response, and must
not be used in a latency comparison.

The recovery candidate passed the live trailing-clarification test and Gemini
review, with a 2,105 ms input-end-to-audible response. The stop/silence/follow-up
regression also passed both checks: 1,180 ms cessation after interruption onset,
1,341 ms follow-up latency. The complete policy package and targeted race tests
passed. Exact timings here come from the recorded metrics, not Gemini's listening
estimates.

## Final deployed result

Executable `38884e8c` was pushed and redeployed. A fresh three-turn adaptive
caller conversation passed all answer/acoustic checks and Gemini's qualitative
review: **1,964, 1,521, and 1,822 ms**, mean **1,769 ms**. All three have complete
nonoverlapping stage boundaries, and cancelled audio is excluded:

| Observed interval | Mean |
| --- | ---: |
| Input end → final transcript | 287 ms |
| Final transcript → admission | 169 ms |
| Admission → first usable text | 739 ms |
| Text → synthesis start | 59 ms |
| Synthesis → first audio packet | 365 ms |
| First packet → audible PCM | 151 ms |

Gemini confirmed relevant answers, successful handling of the caller's concerns,
clear speech, and no clipping or internal-note reading. Public API health and
the authenticated browser both returned HTTP 200; Qwen policy and Fish health
checks also passed. The deployed binary records this clean Git revision.

The original rollback executable, `room.env`, service definition, and checksum
are retained in `.runtime/room-latency-20260909/backup-before-deploy/` with private
permissions. The intermediate `03855603` executable is also retained. The
isolated candidate server was stopped after verification. No private traces,
recordings, credentials, or backup environments are committed.
