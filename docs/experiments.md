# Experimental protocol and prospective hypotheses

This document freezes the initial direction of the M0/M1 research program
before engine optimization. Changes after benchmark inspection must be dated
and labeled exploratory. The original M0–M7 conditions and H1–H4 below remain
historical; the 2026-08-18 prospective extension adds live microturn and
canonical-trajectory experiments without rewriting the completed evidence.

## Conditions

- **B0 endpointed cascade:** all downstream work begins after final endpoint.
- **B1 streaming conventional:** perception streams, response initiation still
  waits for endpoint.
- **M1 fixed microturn:** the B0 component models receive fixed-cadence decision
  opportunities.
- **M2 adaptive microturn:** stability and turn projection can open a decision.
- **M3 fast/slow:** foreground coordination and deliberation are separate.
- **N0 online native/VAD:** public speech-in/speech-out services opened by
  endpoint/VAD behavior, where access permits.
- **N1 native interaction:** GPT-Live, TML Interaction Models, Moshi, or another
  continuous/short-block interaction model, where access permits. GPT-Realtime
  and LiveKit are not substitutes for an unavailable GPT-Live endpoint.
- **H1 hybrid:** acoustic or native interaction signals with modular cognition.

The same fixture, prompt, model version, voice, region, device path, network
condition, and random seed are paired wherever the provider permits.

## Primary prospective tests

### H1 cadence

For predictable controlled turns, fixed-cadence microturn scheduling with
incremental perception will reduce median end-of-user-speech to first semantic
audio and median absolute turn-gap error relative to B0 using identical
component models. Primary guardrails are task success, premature takeover rate,
and audible false-start duration. The claim fails if timing improves but a
guardrail’s confidence interval exceeds the preregistered non-inferiority
margin; margins will be set by power analysis before confirmatory collection.

### H2 stable-prefix planning

Planning on stable prefixes will improve first semantic audio latency on
predictable utterances relative to endpoint-only planning. Ambiguous minimal
pairs are expected to require uncertainty-aware deferral. Primary failure
measures are candidate supersession, committed wrong starts, and repairs.

### H3 speculation

Prepared-but-unplayed text and audio will reduce onset only when candidates
carry explicit validity and cancellation. The ungated ablation is expected to
increase audible false-start duration and reduce blinded trust ratings.

### H4 fast/slow separation

On difficult questions, an honest bounded acknowledgement plus cancellable
deliberation will improve the joint latency-quality-cost frontier relative to a
single blocking path. Acknowledgements that imply nonexistent tool progress are
scored untruthful.

## Prospective extension: 2026-08-18

### Target conditions

- **B0 live endpointed:** streaming-capable components are present, but LLM and
  TTS response work waits for a final endpoint.
- **R0 fixed microturn reflex:** the B0 ASR, fast LLM, and TTS receive
  50/100/200/400/800 ms opportunities. Opportunities with no new usable evidence
  need not invoke a provider.
- **R1 event/adaptive reflex:** the R0 components are triggered by perception
  revisions, semantic events, and turn/stability evidence.
- **I0 homogeneous interleaving:** Gemini 3.5 Flash minimal thinking emits the
  fast trajectory segment; the same family continues with medium/high thinking.
- **I1 heterogeneous interleaving:** local Qwen instruct emits the fast segment;
  Gemini 3.5 Flash medium/high thinking continues from the canonical trajectory.
  Both receive real tool schemas; Qwen calls are non-executable proposals and
  only Gemini calls execute.
- **D0 independent fast/slow control:** separate fast output and slow advice are
  reconciled afterward, preserving the completed M4 architecture as a
  split-brain comparison.
- **N0 online native/VAD:** applicable Qwen online, GPT-Realtime-2, and Gemini
  Live profiles are measured with their documented endpoint behavior where
  access and terms permit.
- **N1 native interaction:** GPT-Live, Thinking Machines Lab Interaction
  Models, Moshi, or another available persistent interaction model are measured
  at their actual cadence where access and terms permit.

### H8 trigger timing and co-location

Persistent incremental ASR, fast-model, and streaming-TTS sessions will reduce
first semantic audio relative to endpointed execution. A 200 ms tick is a safe
opportunity, not a mandatory stateless reinference of every stage. The primary
comparison reports actual component calls, queue wait, cache reuse, GPU
contention, premature takeover, repair, and final task quality.

### H9 canonical-trajectory continuation

On difficult and tool-using requests, I0 and I1 will retain the foreground
latency benefit while improving final quality over fast-only execution.
Compared with D0, they are expected to reduce contradiction, repeated work,
false capability denial, and fabricated completion because slow inference
inherits the exact reasoning/content/tool trajectory rather than a summary.

### H10 model-boundary compatibility

I0 is expected to preserve reasoning continuity better than I1 when the
provider supports native reasoning replay. I1 must compare reusable reasoning,
a normalized working trace, and content-only continuation. Foreign raw thinking
syntax is not assumed valid input for another provider.

### H11 streaming-ASR capacity

The official Qwen3-ASR 1.7B streaming model is compared with the frozen 0.6B
baseline under identical τ-Voice tasks, voices, seed, cadence, fast/slow models,
and tool policy. The hypothesis is improved grounded identifier/tool-argument
accuracy without a meaningful interaction-latency regression. The preserved
identifier failure motivates the ablation but cannot define its acceptance
subset, add recognition hints, or select the model at runtime.

### H12 long-form duplex robustness

The complete released FD-Bench matrix tests whether the same frozen local
cascade retains response, interruption, and false-interruption behavior across
three synthesized caller families, three difficulty levels, background noise,
and noise inserted into gaps. The condition changes only the released input
audio; endpoint, 20 ms transport frames, internal cadence, ASR/fast/slow/TTS
models, voice, prompt, declared 600 ms server-VAD finalization silence, and
fixed 10-second collection tail remain constant.
Primary measures are upstream SRR, SIR, EIR, NIR, SRIR, FSED, ERT, EIT, and
IRD by condition. This is a robustness matrix, not an intelligence proxy, and
its 21 cells are reported separately before any aggregate.

### Joint success criterion

The architecture succeeds only if the responsiveness and intelligence gains
coexist in the same condition. A fast first utterance with poor final reasoning,
or a strong final answer that blocks realtime interaction, does not establish
the joint claim.

## Exploratory implementation checks: 2026-08-18

These checks were run after the prospective extension and are not confirmatory
evidence for H8/H9.

The first passed slice prepared only Qwen fast output. A 14.327 s seed-0 Fish
fixture produced 287 scheduler opportunities, 72 stateful ASR advances, and 45
semantic revisions. The exact final candidate removed endpoint-time fast delay;
Gemini then made one correct call and returned the grounded value. First answer
audio began 3,547.2 ms after endpoint. Its v0.3 scorer checked the required tool
name but not the exact argument multiset; post-hoc inspection shows this
particular run still had exactly one correct call and no tool error.

The stronger v0.6 check prepared Qwen fast and Gemini slow successively on each
private latest-revision trajectory. Its 16.080 s explicit-identifier fixture
produced 322 scheduler opportunities, 81 stateful ASR advances, 45 revisions,
zero normalized WER, and 35.2 ms endpoint finalization. The final chain:

- exactly matched the final semantic root and replayed both stages with no live
  stage fallback;
- made one `records.read({"key":"access_phrase"})` fast proposal and one
  independently authorized call with the same exact JSON arguments;
- produced one successful `cerulean-17` result and one spoken answer;
- had zero endpoint-time fast delay and 3,175.0 ms endpoint-to-first-answer
  audio, including a 1,288.2 ms prepared hosted slow call, a 1,059.9 ms
  post-result slow continuation, and 947.2 ms Fish first PCM;
- released all 127 local admission leases.

The run superseded 42 of 44 background chains and recorded one provider failure
as ASR evidence and local admission changed.
That discarded hosted work is a measured cost of the unconditional reference
policy, not a hidden success. Content-independent temporal pacing is now an
implemented cost-policy ablation; provider prefix reuse or learned admission
remain future conditions. None may depend on keyword patterns.

Strict scoring was added after a legacy background run made a wrong tool call,
received an error, corrected itself, and was still labeled passed by the coarse
name-only scorer. The v0.5 scorer added exact calls and errors; v0.6 names the
`exact-tool-trajectory-v2` contract, which also requires exactly one
identity-matched terminal result per call. Expected tool name plus canonical
JSON arguments form an exact multiset, extra/missing calls fail, and tool errors
fail by default. A six-call identifier-guessing loop therefore failed and hit
the existing invocation safety bound instead of being reported as success.

Same-fixture warm controls show the mechanism without resolving the latency
distribution: endpointed, fast-only preparation, and full preparation reached
first correct audio at 4,336.4, 3,365.2, and 3,175.0 ms respectively. Only the
full condition moved the accepted slow start before endpoint (−120.7 ms).
Fast-only accepted an exact but late candidate whose safe point was +200.2 ms,
showing that acceptance is not itself a latency win. Hosted Gemini reasoning
and TTS varied materially, so these sequential single samples support wiring
and attribution checks, not a magnitude claim.

A first content-independent cost-policy ablation imposed a one-second minimum
between speculative slow launches. On the same fixture it passed the exact
tool trajectory, replayed both stages, and reduced slow preparation launches
from 43 to 12; eight obsolete waits ended before provider invocation and exact
commit bypassed one remaining wait. Endpoint-to-audio was 3,537.7 ms, so this
single run establishes launch suppression and commit behavior only—not latency
improvement or a cost distribution. Interval sweeps and repeated randomized
trials remain required.

All positive and negative reports are interpreted in
[live-cascade.md](live-cascade.md). Required next comparisons remain the
endpointed control, non-tool reflex workload, all registered cadences, native
systems, tail distributions, cancellation/interruption load, cost-policy
ablations with repetition, and human judgment.

The current full-benchmark sequence freezes the I1 local condition, runs both
278-task τ speech cells, all 498 FDB v1.5 overlap recordings, and all 100 FDB v3
tool-use recordings serially on one GPU. A separate watcher then runs both
complete 1.7B-ASR τ cells and restores the 0.6B baseline. Partial progress is
operational evidence only, never an aggregate score.

## Trial and analysis rules

Use paired prerecorded trials; randomize run order; retain warm and cold starts
as separate strata; declare exclusions before final collection; record every
failed trial; and report effect sizes and confidence intervals. Provider outage
and malformed output are outcomes unless a prospective infrastructure rule
excludes them. Exploratory tuning and confirmatory evaluation use disjoint
fixtures.
