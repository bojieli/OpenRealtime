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

## Controlled model replay (legacy numeric budget settings)

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

## Official API configuration verification

Checked Google's documentation and the same direct `v1beta` GenerateContent
API used by the replay. Google recommends `thinkingLevel` for Gemini 3;
`thinkingBudget` is accepted for backward compatibility, and the budget guidance
allows under/overflow. Therefore the original 0/128/512 matrix measures legacy
request settings, not three verified native reasoning modes. HTTP acceptance
alone did not validate that interpretation. [Google thinking guide](https://ai.google.dev/gemini-api/docs/generate-content/thinking?hl=en).

Both models support `low`, `medium` (default), and `high`; `minimal` is rejected.
[Gemini 3.7 model reference](https://ai.google.dev/gemini-api/docs/models/gemini-3.7-flash),
[Gemini 3.8 model reference](https://ai.google.dev/gemini-api/docs/models/gemini-3.8-flash),
[thinking-level defaults](https://ai.google.dev/gemini-api/docs/generate-content/thinking?hl=en#thinking-levels).

A follow-up API check used one captured router-concern context, one request per
cell, the same 1,024 output limit and temperature zero, and randomized order.
These are configuration/usage checks, not repeated latency or quality benchmarks.
Each request supplied either `thinkingLevel` or `thinkingBudget`, never both.
Returned `thoughtsTokenCount` values:

| Requested configuration | Gemini 3.7 Flash | Gemini 3.8 Flash |
| --- | ---: | ---: |
| `thinkingBudget: 128` | Omitted | Omitted |
| `thinkingLevel: low` | Omitted | Omitted |
| `thinkingLevel: medium` | 343 | 132 |
| `thinkingLevel: high` | 279 | 715 |
| `thinkingLevel: minimal` | HTTP 400 `INVALID_ARGUMENT` | HTTP 400 `INVALID_ARGUMENT` |

All eight accepted requests completed with `STOP`. Every omitted count again
had zero residual in the returned token accounting. Both minimal requests
explicitly said that MINIMAL is unsupported. Positive counts at medium/high
confirm that both models can report thinking tokens; the original omission
was not evidence that those models lacked thinking support. One sample cannot
establish a monotonic token-count relationship between medium and high.

The legacy-to-level mapping is not established by these checks. In particular,
the data do not prove that 128 is ignored, that it maps exactly to low, or that
an omitted usage field means no internal reasoning. `includeThoughts: false`
controls returning thought summaries, not selecting a reasoning level.
[Thought summaries](https://ai.google.dev/gemini-api/docs/generate-content/thinking?hl=en#thought-summaries).

A native low-latency request should use:

```json
{"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","includeThoughts":true}}}
```

`includeThoughts` is set for observability. The measurements below show that it
costs no measurable latency, and also that at `low` it returned nothing to
observe. See [native thinking-level response time](#native-thinking-level-response-time).

The live deployment still uses the previously tested numeric 128 setting.
This documentation/API investigation does not deploy an unvalidated replacement.
A production switch should compare native levels using repeated usage, latency,
and end-to-end voice tests. Raw follow-up responses are retained privately as
`thinking-level-api-check.jsonl` alongside the original replay evidence.

## Native thinking-level response time

The checks above requested one sample per cell and measured usage, not time.
This section measures response time for the native levels. On September 10,
144 requests covered `gemini-3.7-flash` and `gemini-3.8-flash` across four
requested settings — `low`, `medium`, `high`, and the deployed legacy
`thinkingBudget: 128` as an in-run reference — with each setting requested twice
over, once with `includeThoughts: false` and once with `true`. Each cell used the
same three captured contexts and three repeats. Conditions match the September 9
replay: streaming SSE on the same `v1beta` endpoint and proxy path, temperature
zero, a 1,024-token output ceiling, a persistent HTTP session, and a seeded
randomized order. A request carried either a level or a budget, never both.

All 144 requests returned HTTP 200, finished `STOP`, and passed the
nonempty/expected-fact checks. Time is request start to the first nonempty,
non-thought text. Completion followed that first text by a mean of 55 ms, so for
these short room answers these are effectively whole-response times rather than a
streaming head start. Each cell below pools the 9 `includeThoughts: false` and 9
`includeThoughts: true` samples, which the next subsection shows are not
separable; the mean/range of reported thought tokens counts only responses that
returned the field.

| Model | Requested setting | Mean first text | Median | Range | Reported thought tokens |
| --- | --- | ---: | ---: | ---: | ---: |
| gemini-3.7-flash | `thinkingLevel: low` | 716 ms | 719 ms | 556–969 ms | none in 18 |
| gemini-3.7-flash | `thinkingLevel: medium` | 1087 ms | 896 ms | 612–2072 ms | 108.0 (24–433) |
| gemini-3.7-flash | `thinkingLevel: high` | 1429 ms | 1137 ms | 749–2583 ms | 212.6 (46–566) |
| gemini-3.7-flash | `thinkingBudget: 128` | 689 ms | 695 ms | 559–889 ms | 1 of 18 reported (44) |
| gemini-3.8-flash | `thinkingLevel: low` | 892 ms | 649 ms | 481–3991 ms | none in 18 |
| gemini-3.8-flash | `thinkingLevel: medium` | 1119 ms | 1001 ms | 682–2144 ms | 145.4 (50–425) |
| gemini-3.8-flash | `thinkingLevel: high` | 1687 ms | 1430 ms | 774–3537 ms | 326.8 (81–979) |
| gemini-3.8-flash | `thinkingBudget: 128` | 624 ms | 625 ms | 510–791 ms | none in 18 |

Both models order `low` < `medium` < `high`. Paired by model, context, repeat and
thought setting, `medium` costs a median **+239 ms** over `low` and was slower in
31 of 36 pairs; `high` costs a median **+585 ms** and was slower in 34 of 36.
Reported thought tokens and first-text latency correlate at r = 0.90 across the 73
responses that returned a count, so the gap tracks generated reasoning rather than
a fixed per-level penalty. One 3,991 ms `3.8/low` sample lifts that cell's mean far
above its median; it is retained. These are observed request times on this endpoint
and proxy path, not a model-family ranking or a production p90.

The cost is context-dependent, so a level cannot be scored on short factual turns.
Median first text for Gemini 3.7 rose from 744 ms at `low` to 2,262 ms at `high` on
the router-concern context, while the France question moved only 787 → 852 ms.
Answer lengths were similar across settings (mean 11.4 to 13.9 answer tokens), so
the differences are not explained by longer replies.

Against the deployed legacy setting, `low` was slower by a paired median of 51 ms
and lost 22 of 36 pairs — far smaller than any between-level difference, and not a
result these three short contexts can call decisive. The two also share a reporting
signature: 0 of 36 `low` responses and 1 of 36 budget-128 responses returned a
thought-token count. That is consistent with 128 being handled like `low`, but it
still does not prove how the provider maps the legacy field.

This 18-sample-per-cell data does support the mean/median ordering that the earlier
single-sample check could not: `high` reported more thought tokens than `medium` for
both models. Per-request ranges overlap, so individual responses are not ordered.

### What `includeThoughts: true` costs and returns

Measured against `includeThoughts: false` on the same 72 configurations, enabling
thought summaries has no measurable latency cost: the paired difference is a median
**−13 ms**, and `true` was the slower half of the pair in 35 of 72. Keeping it on
for observability is therefore free at this sample size.

What it returns is narrower than the flag suggests:

- `thoughtsTokenCount` came back at `medium` and `high` in 36 of 36 requests under
  **both** settings. Usage accounting does not require the flag.
- Thought summary text appeared in 10 of 72 `true` requests and 0 of 72 `false`
  requests. All 10 were at `medium` or `high`, and all 10 were the router-concern
  context; the two capital questions never produced one.
- At `low` — the latency-oriented recommendation above — 18 requests returned no
  summary text and no thought-token count. Enabling the flag there changed nothing
  observable in this sample.
- When a summary did arrive it preceded the first answer text by 40 to 1,153 ms, so
  it is a genuinely earlier signal where it exists.

The room already has both knobs. `-model-retain-reasoning` sets `RetainReasoning`,
which becomes `IncludeThoughts` for a Gemini request (`providers/llm.go`), and the
adapter renders a named effort as `thinkingLevel` and a numeric one as
`thinkingBudget`, never both (`adapters/gemini/continuation.go`). A native level
would be `selection.modelEffort` in `cmd/openrealtime/companion_pipeline.go`, which
is `"128"` today.

Four more requests checked the exact strings that adapter emits, since it
uppercases a named effort. `thinkingLevel: LOW` was accepted by both models
(HTTP 200, `STOP`), so setting the room effort to `low` sends a form this endpoint
takes. `thinkingLevel: MINIMAL` was rejected by both with HTTP 400: *Thinking level
MINIMAL is not supported for this model.* That is worth noting outside this room,
because `cmd/openrealtime/meeting_profile.go` configures the meeting background
model as `google`/`gemini-3.7-flash` with `Effort: continuation.EffortMinimal`,
which renders as exactly that rejected value. This room profile does not use that
path, and nothing about it is changed here. Nothing here changes the running room: this measures the model
endpoint alone, and a production switch still needs the end-to-end voice comparison
described above. Raw responses are retained privately as
`thinking-level-latency.jsonl` and `thinking-level-case-check.jsonl`.

[Per-request thinking-level CSV](room-thinking-level-latency-20260910.csv) has all
144 rows: model, requested level or budget, `includeThoughts`, context, repeat,
status, finish reason, token counts, summary length, first-text and complete times.
It excludes prompts, answer text, and credentials.

## Returned reasoning-token usage

This section audits the original 135 replay responses; no new model requests
were made. Requested `thinkingBudget` is separate from the returned
`usageMetadata.thoughtsTokenCount`. All requests used `includeThoughts: false`;
that same setting still returned positive usage counts for some configurations.
The absence of displayed thought text therefore does not answer the token-count
question.

**For the selected Gemini 3.7/128 configuration, all nine responses omitted
`thoughtsTokenCount`. In all nine, `totalTokenCount` equalled `promptTokenCount`
plus `candidatesTokenCount`. Thus zero additional reasoning tokens were separately
accounted for in the returned totals. This is not an explicit reported zero,
and does not prove that the model performed no internal reasoning.** The earlier
selection was supported by latency and the tested answer quality, not evidence
that this configuration generated 128 reasoning tokens or preserved a measured
amount of thinking.

Each configuration had nine attempts. The mean/range column uses only responses
that explicitly returned the thoughts field. The last column is the arithmetic
residual `totalTokenCount - promptTokenCount - candidatesTokenCount`, averaged
across all successful responses. It is an accounting cross-check, not a
replacement measurement of hidden reasoning. Missing fields remain missing in
the per-request data; they are not silently converted to measured zeros.

| Model | Requested budget | Explicit thoughts field / successes | Reported thoughts: mean (min–max) | Mean accounting residual |
| --- | ---: | ---: | ---: | ---: |
| gemini-3-flash-preview | 0 | 0/9 | Not reported | 0.00 |
| gemini-3-flash-preview | 128 | 0/9 | Not reported | 0.00 |
| gemini-3-flash-preview | 512 | 0/9 | Not reported | 0.00 |
| gemini-3.5-flash | 0 | 0/9 | Not reported | 0.00 |
| gemini-3.5-flash | 128 | 9/9 | 94.56 (25–137) | 94.56 |
| gemini-3.5-flash | 512 | 9/9 | 93.56 (28–137) | 93.56 |
| gemini-3.6-flash | 0 | 0/0 | HTTP 400; no usage | N/A |
| gemini-3.6-flash | 128 | 3/9 | 103.33 (69–169) | 34.44 |
| gemini-3.6-flash | 512 | 3/9 | 209.67 (99–407) | 69.89 |
| gemini-3.7-flash | 0 | 1/9 | 72.00 (72–72) | 8.00 |
| gemini-3.7-flash | 128 | 0/9 | Not reported | 0.00 |
| gemini-3.7-flash | 512 | 0/9 | Not reported | 0.00 |
| gemini-3.8-flash | 0 | 0/9 | Not reported | 0.00 |
| gemini-3.8-flash | 128 | 0/9 | Not reported | 0.00 |
| gemini-3.8-flash | 512 | 0/9 | Not reported | 0.00 |

No response in this matrix explicitly returned `thoughtsTokenCount: 0`.
Every response omitting that field had an accounting residual of zero. Every
response including it had a residual exactly equal to its reported thought count.

Notable returned counts:

- Gemini 3.5/128: 25, 25, 28, 123, 123, 123, 130, 137, 137.
- Gemini 3.5/512: 28, 28, 28, 118, 118, 118, 130, 137, 137.
- Gemini 3.6/128: 69, 72, 169 on the three router-concern cases; the six
  capital-question responses omitted the field.
- Gemini 3.6/512: 99, 123, 407 on the three router-concern cases; the six
  capital-question responses omitted the field.
- Gemini 3.7/0: one router-concern response reported 72; eight responses omitted
  the field. Both 3.7/128 and 3.7/512 omitted it in all nine responses each.

The 72 tokens at requested budget zero and the 130/137/169 counts at requested
budget 128 show that these returned results must not be described as a reliably
enforced numeric ceiling. The captured data cannot distinguish model-specific
budget interpretation, endpoint handling, or usage-reporting behavior. It also
cannot establish that changing 3.7 from 128 to 512 increases thinking: neither
configuration reported separately accounted thoughts in this sample. These
three short contexts do not establish behavior on harder reasoning tasks.

[Per-request token usage CSV](room-model-token-usage-20260909.csv) contains all
135 rows, including rejected requests, missing-field indicators, counts, model,
budget, case, repeat, and first-text latency. It excludes prompts, answer text,
and credentials. Source: retained `matrix.jsonl` response usage metadata.

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
`--models`, `--budgets`, `--levels`, `--include-thoughts`, `--repeats`, and
`--seed` configure the matrix. `--levels` requests native `thinkingLevel` values
instead of a numeric budget, and `--include-thoughts both` measures each cell with
thought summaries off and on. The September 10 level matrix was
`--models gemini-3.7-flash gemini-3.8-flash --budgets 128 --levels low medium high
--include-thoughts both --repeats 3 --seed 20260910`.
`scripts/test_room_model_latency.py` covers the request construction offline.

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
