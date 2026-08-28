# Measurement

The claim this project wants to make is not "four voice stacks are supported" —
that is a release gate, and it is met. It is **"here is what each one is worth,
measured the same way."**

That answer publishes continuously after launch, and it does not gate the
release. Shipping before the most interesting claims are provable is a
deliberate trade: a system nobody can run is not evidence of anything.

## What v1.0 claims

The README says what the system *supports* and what has been *verified*. It
makes no claim that one configuration beats another, and no such claim appears
anywhere until this program says so.

## Design

A reference configuration with paired cells changing exactly one factor each.
The full cross-product is infeasible and uninterpretable; a paired design is
what makes a difference attributable.

**Reference:** `cascade` · Qwen3-ASR · a local fast model · a hosted slow model
at high effort · Fish S2-Pro · 200 ms cadence · endpoint-only observation ·
audio observer only.

| | Factor | Levels | How it is set |
| --- | --- | --- | --- |
| F1 | Preset/model | cascade · omni-Qwen3 · omni-MiniCPM-o · duplex-Moshi · upstream | `-binding`, `-sidecar` |
| F2 | Cognition | fast-only · fast+slow · endpointed slow-only | `-rollout`, `-tool-progress` |
| F3 | Observers | audio · audio+video · video-only | `-observers` |
| F4 | Trigger cadence | 50 · 100 · 200 · 400 · 800 ms | `-trigger-cadence` |
| F5 | Floor source | engine floor · model-native | `-floor` |
| F6 | Slow model and effort | high · medium · local | `-slow-model`, `-slow-effort` |
| F7 | Observer components | keyframe+narration · narration-only · keyframe-only | `-observer-components` |
| F8 | Policy models | none · backchannel · turn projection · both | `-policy-models` |
| F9 | Fast model | local text · hosted vision · local VLM | `-fast-provider`, `-fast-model`, `-fast-sees` |
| F10 | Fast action lane | slow-only · bounded fast computer use | `-fast-computer-use` |
| F11 | Video frame rate | 1 · 3 · 5 · 10 fps | client/evaluator `-fps` |
| F12 | Recogniser | Qwen3-ASR · SenseVoiceSmall · hosted transcription · local Whisper | `-asr-provider`, `-asr-model` |
| F52 | Interaction composition | engine predicates · engine text policy · model-native interaction | ownership + stack capabilities + sidecar protocol |

Every factor is a command-line flag, because a policy that cannot be swapped
cannot be measured. A cell is a command line rather than a build.

F7 exists because the bundle is not uniformly good: a strong model can regress
when handed a keyframe stream, through image-token dilution. Shipping a default
without measuring its components per model would repeat a mistake the evidence
has already identified.

F8 answers the sizing question before the implementation is fixed: whether a
~3B policy model can actually make these calls, or whether ~8B is needed. The
policy-model client counts refusals — answers outside the enumerated list —
which is the signal that a model is too small for the job.

F1 is a deployable preset/model comparison and must not be read as an
architecture-only ranking. F52 is the architectural factor. Its evidence must
record both selected ownership and the full available capability vector, so a
native-capable model run under an engine policy remains identifiable as that
control condition.

## Suites

| Suite | Scale | Answers |
| --- | --- | --- |
| τ-Voice | 278 tasks × control and regular speech | tool-use success under voice. F1, F2, F4, F5, F6 |
| FDB v1.5 | 498 overlap recordings | overlap, barge-in, turn-taking. F1, F5 |
| FDB v3 | 100 tool-use recordings | tool use with real function results. F1, F2 |
| FD-Bench | 6,147 conversations, 77.2 h | endpointing and timing at scale. F1, F4, F5 |
| OpenRealtime Realtime-CU v1 | 8 task families × pixel and set-of-mark | owned audio/video/camera action correctness and reaction. F2, F3, F7, F9, F10, F11, F12 |
| DynaCU-Bench | 100 dynamic + 50 static | optional independent video/action validation. F1, F3, F7 |

τ-Voice and DynaCU-Bench stay in their own repositories, and OpenRealtime
ships a runner for each rather than a copy (`scripts/prepare-tau-voice.sh` and
`scripts/prepare-dynacu.sh` pin and verify the checkouts). The environments own the domains,
the databases, the user simulator, and the reward function; a reimplementation
would produce a benchmark that agreed with this project rather than with the
published one.

τ-Voice needs no bridge to run here. tau2's audio-native path speaks the OpenAI
Realtime protocol over a configurable base URL, and OpenRealtime is a strict
superset of it, so pointing the benchmark at the server is a flag and the wire
is unmodified in both directions:

```sh
scripts/prepare-tau-voice.sh
openrealtime bench tau-voice -verify
openrealtime bench tau-voice -condition regular -out regular.json
```

The runner refuses before it spends hours rather than after: a checkout at the
wrong revision, an unapplied patch, an interpreter without tau2's audio
dependencies, or a missing credential each fail in seconds. What it cannot
refuse in advance it records honestly — a restricted run is reported incomplete
however well it scores, and a simulation that never reached evaluation is
incomplete rather than failed, because scoring a dead endpoint zero is how
infrastructure trouble becomes a published capability claim.

DynaCU-Bench needs no bridge either, and for the same reason: its own GA
Realtime baseline takes the websocket base as an argument, so a strict superset
of that protocol is a value for it.

```sh
scripts/prepare-dynacu.sh
openrealtime bench dynacu -verify
openrealtime bench dynacu -out results/dynacu.json
```

DynaCU is optional independent validation. It is not a dependency, release
gate, or prerequisite for an OpenRealtime capability claim.

The repository-owned gate is `openrealtime bench realtime-cu`. It streams live
screen frames (and a separate physical-camera source where applicable) while
the model acts, runs every task under both grounding modes, and scores hidden
browser state deterministically. A category, grounding, or task limit is a
diagnostic subset and is always reported incomplete; only all sixteen cases
constitute the suite.

Realtime-CU keeps four clocks conceptually separate:

1. cue to observation: perception/transport latency;
2. cue to first tool and first effectful action: model reaction and grounding;
3. tool receipt to completion: browser/action execution;
4. cue to task completion: dependent multi-step outcome.

Functional correctness is not collapsed into the deadline. A correct late
action records `correct_action_rate=1` and `deadline_miss_count=1`; this exposes
whether a change improved reasoning, reaction, or both. Screenshot, wait, and
pointer movement do not count as the first effectful action. All latency claims
retain distributions and sample counts.

The first controlled experiment is slow-only action versus bounded fast action
with every other factor fixed. Subsequent paired cells vary direct keyframes
versus narration, pixel versus set-of-mark readings within each task, hosted
Gemini versus a local VLM, and the video rate. A run from a modified worktree or
a restricted selection remains non-reportable even when it passes.

### Realtime-CU F10 result, 2026-08-25

One complete paired trial changed only F10. Both cells used the same clean snapshot
`b23b5b4f19fdb644b626bf0de74ed73147895c97`, executable SHA-256
`5417b9c0c093ba30861cd1497d620c56ec3b240cda9258444155d99aae8e0bcd`,
three video frames per second, Gemini 3.5 Flash for both cognition phases,
SenseVoiceSmall ASR, the local Qwen-VL observer, and Fish S2-Pro speech. Every
cell completed all sixteen cases with no infrastructure failure.

| Measure | slow-only | bounded fast |
| --- | ---: | ---: |
| Pixel correct | 2/8 | 0/8 |
| Pixel correct within deadline | 2/8 | 0/8 |
| Set-of-mark correct | 4/8 | 5/8 |
| Set-of-mark correct within deadline | 3/8 | 4/8 |
| Overall correct | 6/16 | 5/16 |
| Overall correct within deadline | 5/16 | 4/16 |
| Cue to first effectful action, P50 | 4,645 ms (`n=10`) | 1,533 ms (`n=13`) |
| Cue to first effectful action, P95 | 13,579 ms | 7,858 ms |
| Cue to first effectful action, max | 15,543 ms | 8,591 ms |
| Frame to observation, P50 | 6.0 ms (`n=16`) | 4.5 ms (`n=16`) |
| Cue to observation, P50 | 312 ms (`n=4`) | 146 ms (`n=4`) |

The bounded lane reduced median reaction by 67% and P95 by 42%. It did not
raise overall correctness or deadline success. Set-of-mark gained one outcome
while pixel lost two, which is useful diagnostic evidence and not a general
intelligence claim at this sample size. An earlier clean pair on a prior
snapshot independently moved the median in the same direction, 7,653 to 1,090
ms, but code changed between snapshots and the observations are not pooled.

The audit log explains the latency change rather than merely correlating with
it. Across the first pair, twelve direct clicks ran in the fast phase; all fifty
screenshots, waits, typing calls, scrolling calls, and deliberative repairs ran
in slow. The runtime coordinate fence also remained necessary. Slow-only made
no out-of-bounds calls, while bounded fast attempted three; all three were
refused even though the target-specific tool schema and task
instruction both stated the inclusive bounds. A provider accepting a schema is
not evidence that it will obey it.

The retained recognized turns expose another bottleneck. On every incident-code
case, the configured recognizer heard variants of “Ko now for N” and the agent
typed `KONOW4N`; the authored audio independently transcribes as “incident code
alpha-7”. That failure belongs to the measured end-to-end system, but it is ASR
evidence rather than a reason to relabel the downstream model as unintelligent.
The benchmark retains both recognized text and action trace so future ASR and
cognition cells can separate the two.

A counter-ordered repeat is retained only as diagnostic evidence. One bounded
fast task reported a trajectory-version conflict and one slow-only task hit a
Gemini HTTP 503. An earlier runner retained those errors but still marked the
cases complete; the shared driver now returns a typed session failure and makes
either cell incomplete. They are not pooled into the table above.

This is a measured speed result and a negative quality result. One clean pair
and one earlier-snapshot replication are not an estimate of a stable population
effect; broader model, video-rate, and repeated-seed cells remain the work
needed for comparative capability claims.

### Realtime-CU F12 result, 2026-08-25

One complete pair changed only the recognizer: local SenseVoiceSmall versus
streaming Deepgram Nova-3. Both cells used clean snapshot
`a2d726763713b06ecb89f155b7b4358c1e6cdc52`, executable SHA-256
`e9a52625bcb0e07b08b8508707ea1422e37ec53966555bb9dca15df0c3ee7ada`,
and the same slow-only action authority and cognition, vision, TTS, browser,
audio, and video configuration. Both completed all sixteen cases with no
infrastructure failure.

| Measure | SenseVoiceSmall | Deepgram Nova-3 |
| --- | ---: | ---: |
| Pixel correct | 2/8 | 2/8 |
| Pixel correct within deadline | 1/8 | 1/8 |
| Set-of-mark correct | 4/8 | 5/8 |
| Set-of-mark correct within deadline | 2/8 | 2/8 |
| Overall correct | 6/16 | 7/16 |
| Overall correct within deadline | 3/16 | 3/16 |
| Cue to first effectful action, P50 | 7,264 ms (`n=10`) | 4,481 ms (`n=11`) |
| Cue to first effectful action, P95 | 10,894 ms | 13,026 ms |
| Cue to first effectful action, max | 11,721 ms | 15,258 ms |
| Frame to observation, P50 | 5.6 ms (`n=16`) | 5.7 ms (`n=16`) |
| Cue to observation, P50 | 480 ms (`n=4`) | 156 ms (`n=4`) |

Deadline pass rate was identical, so the formal paired comparison reports a
zero difference. Functional correctness gained one case and typical reaction
was lower, while tail reaction was worse. The result is diagnostic, not a
general recognizer ranking.

The literal task shows the causal improvement clearly. SenseVoice heard “Ko
now for N”, the model submitted `K094N`, and the case failed. Deepgram heard
“AlphaDash7”, the same cognition path typed `Alpha-7`, and the deterministic
evaluator accepted it, 129 ms beyond the deadline in the smoke run and late
again in the full cell. Deepgram also preserved “smoke” and “emergency stop” in
the camera task, turning the pixel case functionally correct but late. Cleaner
language did not solve transient UI, dashboard, or game timing, and one
authorization pixel grounding regressed despite an accurate transcript. ASR
was one bottleneck, not the only bottleneck.

An OpenAI ASR smoke attempt returned HTTP 429 for exhausted quota. It is kept
as incomplete diagnostic evidence and is excluded from every result above.

## Reporting rules

- **No incomplete cell is reported.** A partially executed cell is not a
  smaller result, it is a different one.
- **No legacy session failure is reported.** The reader refuses older JSON in
  which a protocol error survived as a `session_failure` note on a supposedly
  completed task, even if that file's cached summary says it is complete.
- **Every cell declares its source revision and executable hash.** A number
  that cannot be traced to a build is a number that will eventually be wrong.
- **A pair uses one build and one machine description.** The comparison
  refuses different revisions, executable hashes, or hardware; otherwise code
  or hardware would be an unrecorded factor.
- **Latency claims require distributions.** A mean first-audio latency without
  its tail describes a system nobody is using.
- **Negative results publish.** A configuration that does not help is
  information, and suppressing it is how a measured claim becomes a marketing
  claim.
- **No cross-suite synthesis.** τ-Voice and FD-Bench measure different things
  and do not average.

## What it cannot settle

Human preference and perceived naturalness. Those need a prospective protocol
with human raters, which is a separate program and not a re-analysis of these
numbers.

## Efficiency gates

These are release gates rather than comparative claims, and each has a
published number against a declared reference machine. See
[efficiency.md](efficiency.md).

## Overlap: what the harness found while it was being built

The first FDB v1.5 recording that ran exposed a gap, and it is worth recording
because it is what a measurement program is supposed to do.

The shipped barge-in policy yields on any user speech over agent output. That
is the safe failure — stopping when somebody says "mm-hm" is annoying, talking
over a real interruption is worse — but the backchannel category measures
exactly how often it happens, and it fails all of them.

`interaction.BargeIn` already accepted typed overlap evidence. Nothing produced
it. So an overlap classifier was added: a policy model answering one enumerated
question about the partial transcript, directed against backchannel against
side speech. Two further problems surfaced immediately:

1. **The sustained policy ignored evidence during its hold.** The hold exists
   to give a classifier time to answer, so once something says "this is
   directed at you", waiting out the rest of it is talking over somebody who is
   already interrupting. The hold is now a maximum, not a minimum.
2. **The hold was never actually driven.** It was re-evaluated only when a
   recogniser revision arrived, and a first partial can be half a second away.
   A timeout that only fires when words happen to arrive is not a timeout. It
   is now checked on the audio path, where frames arrive every 100 ms whatever
   the recogniser is doing.

### The trade, measured

Two recordings per condition against the live stack — a smoke measurement, not
a published cell, and the harness refuses to report it as one:

| Configuration | Interruption: yield latency | Backchannel: held the floor |
| --- | --- | --- |
| `immediate` | ~195 ms | 0 of 2 |
| `sustained 400ms` + overlap classifier | ~665 ms | 2 of 2 |

Roughly 470 ms of extra talking-over, in exchange for not stopping when the
user was only signalling that they were still listening.

The floor on the classified path is not the hold and not the model — it is how
long the recogniser takes to produce its first partial. Classifying overlap
needs words, and words arrive when they arrive.

### Where the concurrency actually was, and was not

An agent that goes quiet while it reasons ought to say so. The turn that fills
that gap was written twice and withdrawn twice before it worked, and what the
two failures found is worth more than the feature.

The first instinct is that such a turn must run *beside* the deliberation, as a
parallel branch. Chasing that found three layers, each of which would have had
to change:

The **deferred set** was merged flat before it ran, and a merged batch carries
one triage, so a parallel branch inherited the deferral of whatever routine
traffic sat beside it. That was a real defect and is fixed.

The **loop has one driver**. While a batch is processed the driver is inside
that call, so a branch is planned correctly and runs once the work it was
reporting on has finished - the one moment it has nothing to say.

The **gateway has one turn**. `planning` is a boolean and `response` a single
pointer, and that is right rather than a limitation: the base protocol has one
active response, so a second concurrent turn would not be expressible on the
wire even if the loop could produce it.

The third layer is the answer to the other two. A turn that fills a silence is
not a second turn; the turn that started the deliberation is still open, and the
voice adding a sentence to a turn it is already in is what "the voice keeps
talking while the reasoner reasons" has always meant. No second driver, no
second response, no change to the loop.

What made it possible was unrelated and worse: a continuation committed against
the trajectory version it started from, so an assistant turn appended while the
reasoner was working discarded everything the reasoner had produced, tool calls
included. Every earlier attempt at this feature would have silently destroyed
the reasoning it was reporting on. Staleness is about evidence now, not about
version numbers, and speaking during deliberation costs the reasoner nothing.

### What listening to the recordings found

The suites score outcomes. Playing the audio back — the caller on one channel,
the agent on the other, on one clock — shows what the outcome sounded like, and
three defects were only visible that way.

On one FDB v3 recording the agent produced eleven turns, cancelled eight of
them mid-word, asked seven times for an order number the caller had given once,
and closed with a loop of farewells. The score said "no matching call". The
audio said the answer had been produced at sixteen seconds and cut off before
anyone heard it.

The cause was in perception, and none of it was the recogniser's fault. These
recordings are forty seconds long: eleven seconds of speech, then room tone
whose peaks reach full scale. The acoustic gate opened on those transients, the
recogniser was asked what was in them and answered ".", and an observation said
the user had spoken. Probing the recogniser directly on every post-speech window
returns "." or "" for all of them — it was right throughout.

The second cause was the same silence at a different scale. A disfluent request
pauses longer than the endpoint threshold, so one request arrived as five turns,
each answered separately, each answer cancelling the last.

The third was that turn projection had never run. A reasoning model asked for
one word spends its budget reasoning, so the policy model answered "<think>"
and its decision was scored as unknown and discarded. Every measurement of F8
taken before that fix is a measurement of a policy that never fired.

With those closed, the same recording is one observation, one turn and no
cancellations, and an independent listener moves from "broken" to "awkward":
the request is answered, and what remains is that the answer arrives in three
redundant pieces rather than one. That is F2 again — the arrangement speaking
more than once — and it is what the cells are for.

The listener is not exempt from this. Asked to transcribe a post-speech window,
Gemini produced a fluent account-number dialogue, complete with digits and a
self-correction, from audio whose peak was under twenty percent and which the
recogniser correctly called ".". Audio judgements here are trustworthy for
content that exists and worthless for near-silence, and the difference is
settled by measuring the signal rather than by asking a second model.

### What the arrangement costs a tool call

Sixteen FDB v3 tasks per condition, on the local open stack. A smoke
observation like the two below it: sixteen stochastic tasks do not separate
conditions that differ by two or three, and the harness refuses to report any
of this as a cell.

| Condition | Right tool and arguments | Call attempted | No call |
| --- | --- | --- | --- |
| `fast+slow` | 2 | 4 | 12 |
| `fast+slow`, clarified marker | 3 | 6 | 10 |
| `fast+slow`, clarified marker, policy models and hold | 3 | 7 | 9 |
| `endpointed-slow-only` | 8 | 15 | 1 |

Only the last row is outside the spread, and it is the interesting one. The
first three differ by prompt and policy; the fourth differs by who is asked at
all. Under `fast+slow` the reasoner runs only when the voice hands the turn on,
so every tool call in the arrangement depends on a small model correctly
judging that it cannot finish the request itself — and that judgement, not the
reasoner's ability to choose a tool, is what most of the missing calls are.

This is F2, and it is worth stating before the cells run because it changes
what F2 is measuring. The contrast between fast+slow and slow-only is usually
read as latency against quality: the voice answers immediately, and deliberation
costs time. These sixteen tasks say it is also a question of whether the
deliberation happens, which a paired design will attribute to the wrong factor
if the escalation rate is not reported beside the pass rate.

### What a recogniser's shape costs the policies

A second smoke observation, on one τ-Voice retail simulation per condition —
again not a published cell, and again the harness refuses to report it as one.

| Condition | Answered its turns | Agent interruptions | Rate |
| --- | --- | --- | --- |
| autoregressive recogniser, endpoint-only | none | — | — |
| SenseVoice, endpoint-only, no policy models | every turn | 107 | 7.1/min |
| SenseVoice, policy models, 300 ms partials | every turn | 36 | 2.1/min |

The first row is not a turn-taking result. The recogniser could not keep up
with the audio arriving, so the session failed before any policy ran; it is
here because it is what the other two rows were being compared against.

The third row is the one with a lesson in it. Backchannel and turn projection
answer a question *about the partial transcript*, so a batch recogniser asked
for nothing before the endpoint gives them nothing to judge — the policy models
were configured and had no evidence, which measures as no effect. What made
them work was not enabling them, it was making partials affordable enough to
ask for: 20 ms an advance, where the same flag against an autoregressive
recogniser had been the thing that pushed it past its own bound.

So `-policy-models` and `-asr-partial-interval` are one decision wearing two
flags, and F8 cannot be read without knowing which recogniser F1 selected.
The cost is about 200 ms of response latency, and neither run resolved its
task, which is a separate question from turn-taking and not one a single
simulation can answer.

**`immediate` remains the default.** It is the safer failure, and the
alternative is a documented configuration with a number attached rather than a
recommendation. Which one is right depends on whether a deployment's users
backchannel, which is what the full cells will say.

## The trajectory's clock (F13)

Every trajectory item has carried `MonotonicNS` since the log existed, and no
projection had ever put it in front of a model. The transcript a model read was
an ordered list with no clock: a reply after two hundred milliseconds and one
after half a minute were indistinguishable to it, though nobody sharing the
room could have missed the difference.

Measured against `qwen-fast` (Qwen3-30B-A3B-FP8), same prompt and same three
prior messages, differing only in whether the projection carried the gap:

| the user says | the agent answers |
| --- | --- |
| `how long was i gone` | "I don't have a way to track time, but I'm here whenever you're ready!" |
| `[2m14s later] how long was i gone` | "You were gone for 2 minutes and 14 seconds." |

Two things worth separating. The model *uses* the note rather than merely
receiving it — its reasoning refers to the elapsed time unprompted — and it
does not read the bracket aloud, which was the risk worth checking before
putting runtime-authored text into a user message.

The second, less flattering half: on a prompt where timing was available but
not load-bearing ("sorry, i'm back"), the answers with and without the clock
were the same sentence. The clock adds a fact the model can reach for. It does
not by itself change what the model says, and no measurement here claims it
improves turn-taking or task completion. Those are separate questions.

The threshold is a second. Below that a gap is the ordinary seam between one
item and the next — the pause between a question ending and an answer
beginning — and reporting it would be noise wearing the costume of information.

## The interaction model (F14)

Three boundaries, measured before any of it was wired into the runtime. The
step-by-step layer exists so that a design can be judged in milliseconds
against one provider rather than in minutes against a whole stack, and this is
the first change large enough to need it.

### Which model

Qwen3-30B-A3B-FP8 against Qwen3-8B, same prompts, same cases:

| | 30B-A3B | 8B |
| --- | --- | --- |
| interaction, balanced | **0.77** | 0.68 |
| timelines | **7/7** | 5/7 |
| standing instructions | 36/42 | **38/42** |
| latency, one decision | 25–45ms | 33–60ms |

The MoE wins where it matters and is the faster of the two, because only three
billion parameters are active. 8B is marginally better at extraction, which
runs off the critical path where 20ms is irrelevant. So: **30B-A3B decides,
and the choice is not close on the boundary that runs five times a second.**

Reasoning was tried and is not an option here. Enabled on the 8B it scored
9/23 against 13/23 with it off, and took 5.5 seconds. This decision has to be
made without deliberation, which is a constraint on the design and not a
preference.

### What actually moved the numbers

None of the four things that mattered was a better-written rule.

**Format consistency, 36/69 to 54/69.** The worked examples had been written as
compressed one-liners while the live input was a labelled block. A model was
spending its capacity translating between two shapes of the same thing before
it could decide. Examples now render through the same function as a real
decision.

**A missing field.** Who is speaking and what has been heard were one field, so
every completed utterance vanished the instant its speaker stopped: the model
was told a turn had ended and never told what the turn said. It answered
"listen" because nothing had been put in front of it to answer.

**An act with no way to perform it.** The phone-menu case failed while
call-tool was offered and no tool was ever named.

**The name of the do-nothing act, which is a threshold and not a quality
lever.** Asked to explain a wrong answer, a model said it had chosen "listen"
to keep track of what the speaker was saying - it had understood the task
perfectly and read the act as an instruction to pay attention rather than as a
decision to say nothing. Renaming it slides the model along a trade and leaves
discrimination unchanged:

| do-nothing act named | acts when it should | refrains when it should |
| --- | --- | --- |
| `listen` | 40% | 85% |
| `wait` | 60% | 73% |
| `stay-silent` | 75% | 54% |

Balanced accuracy is reported for exactly this reason: the plain total moves
with the case mix and would have called all three of these equally good.

### What the suite refuses to measure with one number

Acting and restraint are reported separately because a model that always acts
and one that never acts post identical totals while being opposite bugs. The
first two runs of this suite were precisely that pair.

## The interaction model end to end (F15)

The step-by-step layer says the decision is sound: balanced accuracy 0.76 on
the interaction boundary, 7/7 on the timelines, 38/42 on extraction, 34ms a
decision. The end-to-end layer says it does not yet make the system better.

Five scripted conversations, three runs each, same audio and same recogniser:

| | predicates | interaction model |
| --- | --- | --- |
| an ordinary question | 3/3 | 3/3 |
| a recorded menu | 2/3 | 1/3 |
| asked not to be interrupted | 0/3 | 0/3 |
| count as they go | 0/3 | 0/3 |
| cutting in on something wrong | 0/3 | 0/3 |
| **total** | **5/15** | **4/15** |

Parity, inside the run-to-run spread. The three capabilities this was built for
do not work yet, and neither configuration has ever passed them.

### The result that was not real

An intermediate run scored 3/3 on "asked not to be interrupted" and it was a
bug, not a capability. The floor cached its verdict by revision, and the last
call before a pause is made while the speaker is still audible - where
answering is refused. So the refusal was served back for the whole pause and
the turn never ended at all. It looked exactly like honouring "don't interrupt
me", including on the transcript.

Fixing the cache restored ordinary turn-taking and took that scenario back to
0/3. Both numbers came from the same code; only one of them was a measurement.

### What the harness was measuring before that

Nothing. The speech backend returns 44.1 kHz whatever it is asked for, and
those samples in a 24 kHz pipeline stretch by 1.84 and drop an octave. "A heron
landed on the far bank" reached the recogniser as "the horn mounted on the far
bank", which reads like a weak recogniser rather than like arithmetic. Every
scenario number before the resampler is void.

### What is actually blocking the three

Reading the recorded decisions rather than the scores: the interaction model
chooses sensibly. It answers "listen" at a pause where somebody has asked not
to be interrupted, with the request visible in the transcript. What fails is
downstream - the voice does not reliably do the thing the policy asked for,
and speaking through somebody without ending their turn has no path to
producing speech at all. The act selects machinery that in two cases does not
exist yet.

So the honest position is that the decision layer is measured and the
behaviour it should produce is not, and the flag stays off.

### Where the counting case actually fails (F16)

Probing the voice directly, outside the whole stack, separates the two layers
cleanly. Same trajectory, same instruction, one turn at a time:

| the turn | 30B-A3B | 8B |
| --- | --- | --- |
| somebody sets the policy | "One. Two. Three… Ten." | "Okay, I'll count them as you mention them." |
| a line with no animal in it | "Nothing." | "I saw a fox." |
| a line with an animal in it | **"One."** | **"One."** |

Both models get the case the demo is about. Both get the turn that *sets up*
the arrangement wrong, and they get it wrong in opposite directions: the MoE
performs the arrangement on being told about it, and the dense model
acknowledges correctly and then invents an animal that was never mentioned.

That first spurious count is the whole failure. Once it has counted to ten, the
next real animal is eleven, and every check for "one" and "two" fails for the
rest of the conversation - which reads in the scenario report as counting being
broken, when the only broken turn is the one before any animal exists.

The interaction model is right in all three cases. It answers the policy-setting
turn, stays silent through the line with no animal, and speaks through the line
with one. Four rewordings of the instruction moved this around without fixing
it, and moving the rule to the end of the prompt - where recency should have
helped - made it worse, because the example inside it primes the behaviour it
forbids.

So the limit here is the voice at this scale, not the design, and it is a
deployment's choice to spend a larger model on the phase that speaks. The
interaction model is 3B active for a reason; the voice need not be.

### What it takes to make the counting demo work (F17)

Three cells, same suite, same recogniser, same scripted audio:

| scenario | predicates + Qwen voice | predicates + Gemini voice | interaction model + Gemini voice |
| --- | --- | --- | --- |
| an ordinary question | 3/3 | 2/2 | 6/6 |
| a recorded menu | 2/3 | 2/2 | 3/6 |
| asked not to be interrupted | 0/3 | 0/2 | 1/6 |
| **count as they go** | **0/3** | **0/2** | **5/6** |
| cutting in on something wrong | 0/3 | 0/2 | 0/6 |
| **total** | **5/15** | **4/10** | **15/30** |

The middle column is the one worth having. A better voice on its own does not
produce the behaviour: counting stays at zero. The interaction model on its own
did not either. Together it is five in six, and the transcript is the demo:

```
 8190ms  "I will count each animal as you mention them. Go ahead."
21036ms  "That's one."
29881ms  "Two."
```

The acknowledgement with no count in it, then one number per animal while the
speaker keeps talking. That is what the whole design is for, and it needed both
halves - the acts arriving at the right instants, and a voice that does what
the act asked rather than what the words primed.

Gemini 3.5 Flash at minimal thinking answers in 700-900ms against the MoE's
30-40ms. That is far too slow for the interaction decision, which is asked
several times a second, and perfectly affordable for the phase that speaks,
which is asked once a turn. The two phases want different models for reasons
that have nothing to do with quality.

Two scenarios remain at zero or near it. Interrupting to correct has never
passed in any cell, and holding silence through a pause somebody asked for
passes one time in six. Neither is a measurement artefact now: the recogniser
is clean, the harness is honest, and the failures are visible as specific
decisions in the recorded log.

### The agent's own contract, and the final progression (F18)

Correcting somebody mid-sentence had never passed in any configuration, and the
reason was that the fact being corrected against - *the deadline is the 3rd* -
lives in the deployment's own instruction, which the decision could not see.
The situation was designed to carry it and did not. With it, the step-by-step
case goes from never passing to 3/3.

It costs something. The paired case, where the person says a date that agrees
with the contract, now fails 3/3: the model fires on "a date was mentioned"
rather than on the contradiction. Balanced accuracy is unchanged at 0.73, which
is the trade showing up honestly rather than the change being free.

Four cells, same suite, same recogniser, same scripted audio:

| scenario | predicates + Qwen | predicates + Gemini | + interaction model | + contract |
| --- | --- | --- | --- | --- |
| an ordinary question | 3/3 | 4/4 | 6/6 | 3/3 |
| a recorded menu | 2/3 | 4/4 | 3/6 | 3/3 |
| asked not to be interrupted | 0/3 | 0/4 | 1/6 | 0/3 |
| count as they go | 0/3 | 0/4 | 5/6 | 2/3 |
| cutting in on something wrong | 0/3 | 1/4 | 0/6 | 1/3 |
| **total** | **33%** | **45%** | **50%** | **60%** |

Two capabilities that had never worked in any configuration now work: counting
out loud while somebody keeps talking, and cutting into a sentence to correct
something. Both needed several things at once, and no single column shows
either of them arriving.

One has not been made to work. Holding silence through a pause somebody
explicitly asked for passes at most one time in six. The decision is right when
the policy is in front of it - measured directly, the model answers "listen" at
exactly the pause that fails - so what is left is the policy not reliably being
there, which is extraction and not the decision.

### The full suite, including the production cases (F19)

The suite had five scenarios and covered part of the demos and the phone menu.
It had nothing for ordering from a waiter, which came from somebody actually
using this, and nothing for simultaneous speech, which is the interpreting
demo. With those and a backchannel case added, eight scenarios and twenty-one
checks:

| scenario | passes | source |
| --- | --- | --- |
| a recorded menu | 2/2 | production: IVR navigation |
| an acknowledgement is not an interruption | 2/2 | overlap |
| an ordinary question | 2/2 | control |
| translating as they speak | 2/2 | the interpreting demo |
| asked not to be interrupted | 1/2 | a policy set out loud |
| count as they go | 1/2 | the counting demo |
| ordering from a waiter | 1/2 | production: the moment passes if you wait |
| cutting in on something wrong | 0/2 | correcting mid-sentence |
| **total** | **11/16 (68%)** | |

Against 33% for the shipped predicates on the five it shares. Seven of the
eight now pass at least sometimes; every one of them passed zero times when
this started.

Two bugs surfaced only because the new scenarios existed.

**The pause clock was not measuring a pause.** It deliberately survives a hold,
because reopening the gate zeroes the gate's own counter and a bound measured
from there would restart on every hold and never arrive. But it also survived
the speaker *speaking*, so it reported twenty seconds of silence across a
stretch containing three spoken sentences - and the liveness bound, measured
against it, fired in the middle of a monologue and interrupted the one thing it
had been asked not to interrupt. That scenario had never passed; it passes now.

**The recogniser endpoint accepted a language and dropped it**, always passing
"auto" to the model. Harmless until somebody is interpreting: auto-detection on
a short second-language utterance guesses the first language and returns the
right sounds spelled wrong. Asked for German - which this recogniser does not
cover at all - "Guten Tag" came back as "G talk", and the agent faithfully
interpreted the nonsense. Live interpreting is bounded by what the recogniser
can hear, and that bound belongs in the scenario rather than in its result.

### What is still not covered

The scenario harness plays audio, so the visual demos - announcing a posture
change, watching a screen and a camera at once - have no end-to-end case. The
perception layer supports them: `perception/video.go` takes named sources,
samples on change, and narrates to text. What is missing is a way to script a
visual event on a timeline beside the speech, and until that exists those
capabilities are covered only at the step-by-step layer.

### The visual case, and the route that skipped the decision (F20)

The suite played audio, so anything that fires with nobody talking had no
end-to-end case. The timeline now carries protocol events beside the audio, and
a scenario puts a real picture in front of the deployment's own narrator - a
terminal showing a build running, then the same terminal showing it finished.

It works. Told "tell me the moment the build finishes, and don't say anything
else", the agent acknowledges, stays quiet through the frame showing it still
running, and reports when the finished frame arrives, with nobody having spoken
since the first sentence.

Two things it found.

**A route to speech the interaction model never saw.** It governs endpointing,
which is how a person's turn reaches the microphone. An observer commits what
it saw and the rollout plans a turn on any observation at all, so a screen that
changed in a way nobody had asked about produced a turn exactly as one that
mattered did. A decision layer that governs some routes and not others governs
nothing, because the ungoverned route is always available. Same shape as the
pause decision; found the same way.

**Latency, and a wrong reading of it.** The first measurement said five to
fifteen seconds and it was wrong: what it had timed was a *second*
acknowledgement of the user's opening instruction, not the report. Measured
against the frame itself the report lands **1.8 seconds** after it - a vision
model narrating a picture, then a turn - which is fast enough to be worth
calling "the moment". The scenario's window is four seconds because that is a
real bound; fifteen was measuring the wrong event.

The remaining failure in that scenario is that duplicate acknowledgement. The
agent answers the opening instruction, and then answers it again several
seconds later, and the second one lands inside the window where it was asked to
be silent. That is a defect in its own right and nothing to do with seeing.

### The suite as it stands

| scenario | source |
| --- | --- |
| an ordinary question | control |
| a recorded menu | production: IVR navigation |
| ordering from a waiter | production: the moment passes if you wait |
| translating as they speak | production: simultaneous speech |
| an acknowledgement is not an interruption | overlap |
| count as they go | demo: counting while somebody talks |
| cutting in on something wrong | demo: correcting mid-sentence |
| asked not to be interrupted | a policy set out loud |
| telling them what it saw | demo: a visual trigger, nobody speaking |

Nine scenarios, twenty-three checks, against 33% for the shipped predicates on
the five they share.

### How long a person waits (F21)

Every check in the scenario suite asked what the agent did and none asked how
long it took. A system can pass all of them and be unbearable.

Latency is measured in waveform time: from the last sample of the thing that
triggered a reply to the first sample the agent produced. Not from the
decision - a decision taken in thirty milliseconds is still a long silence once
the recogniser, the voice and the synthesiser have each taken their share - and
not from the start of the trigger, because what a person waits through is the
silence after somebody finishes talking. The arrival of an audio event stands
in for the moment it is heard, which is exact while playback is realtime.

Measured with Gemini 3.5 Flash as the voice:

| what triggered it | median wait |
| --- | --- |
| interrupting a waiter mid-list | **324ms** |
| cutting in on a wrong date | **845ms** |
| a frame showing the build finished | **~900ms** |
| translating a sentence as it lands | 1895ms |
| an ordinary finished question | 1625ms |
| a phone menu naming the right option | 3322ms |

The ordering is the interesting part, and it inverts the usual assumption.
**Interrupting is faster than answering.** Answering waits out a silence
threshold to establish that the turn has ended; interrupting fires on content
and skips that wait entirely. The acts that felt like the risky, advanced ones
are the low-latency ones, and the ordinary reply is the slow one - because
endpointing, not thinking, is where the time goes.

That also says where to spend effort. Cutting a hundred milliseconds off the
voice improves every row a little; cutting the silence threshold improves only
the rows that wait for it, and those are the slowest.

Two bounds now fail and are worth failing: a phone menu answered at 4478ms
against a 4000ms bound, because a menu moves on and a key pressed late is
pressed into the next option, and the visual case at 11785ms against 3000ms.
The frame itself is answered in under a second; the eleven seconds is a
different trigger in the same scenario being answered late.

Writing the test found the measurement inverted. FirstAudioAfter returns the
wait rather than the moment, and subtracting the offset again turned every late
reply into a large negative number - a bound nothing could fail.

### Where it stands, and the bug class that dominated (F22)

Nine scenarios, three runs each, Gemini 3.5 Flash as the voice and
Qwen3-30B-A3B deciding: **15–17 of 27** across recent runs, against 33% for the
shipped predicates on the five scenarios they share. Run-to-run spread is two
scenarios wide, which is worth stating before any single number is quoted.

| scenario | typical |
| --- | --- |
| an ordinary question | 3/3 |
| asked not to be interrupted | 3/3 |
| an acknowledgement is not an interruption | 3/3 |
| ordering from a waiter | 2–3/3 |
| a recorded menu | 1–2/3 |
| count as they go | 1–2/3 |
| translating as they speak | 1–2/3 |
| cutting in on something wrong | 0/3 |
| telling them what it saw | 0–1/3 |

### The pattern in the defects

Of everything found end to end, the largest group is one shape: **an act that
was decided correctly and never carried out**. The model chose it, the eval
agreed it was right, and some route between the decision and the microphone
dropped it.

- speak-through was handled where partials are processed and not where pauses
  are, so most of them - a pause is when the floor is asked - reached nothing
- call-tool fell through the floor's default branch entirely and did nothing at
  all, and the phone menu passed anyway until the model got better at choosing
  the act built for it
- the adapters refused a turn whose log was empty, which is every turn that
  begins on a partial: fifty of seventy-seven silent acts died there
- a proposal nobody dispatched is a key nobody presses - calling the engine
  rather than the runtime's own slow step skipped the layer that executes what
  the engine produced

Each looked from outside like the interaction model failing to decide, and in
each case the decision was already right. That is the argument for recording
every act *and* every refusal: the two are indistinguishable in a transcript.

### What is left

**The voice, not the decision.** Told to correct a wrong date, the agent cuts
in at 630-843ms - the fastest response in the suite - and then says "got it" or
"are you sure?" rather than the right date. The decision, the timing and the
floor are all correct; what is said into the turn is not.

**Visual latency.** The frame is answered in under a second when it is
answered; the scenario's other triggers are answered at 13-18 seconds, and
that is what fails the bound.

**Repetition.** Two or three key presses at a phone menu where one is right,
now that the in-flight guard stops six.

### The shape of what is left (F23)

Two scenarios fail the same way, and the symmetry is the most useful thing
known about them. Counting says "One" and not "Two". Interpreting carries the
first sentence over and not the second. Both are repeated speak-through, both
answer the first occurrence promptly - 1.7 to 2.1 seconds - and both go quiet
for the second.

That is one cause rather than two, and it is not the decision: traced with the
policy pinned, the agent's own first answer in the conversation above it and a
two-second gap marking the new utterance, the model chose speak-through for the
second occurrence. Between that choice and the microphone something declines.

The candidates are enumerable, which is the value of having recorded every
refusal: one interjection per revision, one in flight at a time with a
three-second staleness bound, the floor's own cache, and the event loop's
single driver serialising the commit. Each is individually defensible and one
of them is wrong here.

Everything else in the suite is either passing consistently - an ordinary
question, a policy honoured through pauses, an acknowledgement that must not
stop the agent, ordering from a waiter - or failing for a reason already named:
the voice acknowledging where it should correct, and the vision pipeline's
latency on triggers other than the frame itself.

### The slot, and the instrument that was not wired (F24)

Counting said "One" and not "Two"; interpreting carried the first sentence and
not the second. One cause, and it was not the decision: an interjection commits
through a loop with a single driver, so it can sit behind other work while
every later moment worth speaking at is refused as "already in flight".

Bounding it fixed interpreting - 4/5 over five runs at a 2.1 second median,
the best that case has measured and the one I was least confident a cascade
could do at all.

The bound itself took two attempts, and the first was wrong in an instructive
direction. Six seconds cancels turns that were about to finish, which wastes
the work *and* the slot: counting's median wait went from 2.1 seconds to 13.9.
The deadline is for a turn that will never finish, not one that is late, and
those differ by an order of magnitude. Twelve.

**And the diagnosis before it was wrong for a worse reason.** I concluded that
no interjection ever completed, from the absence of completion records - and
those records were never being written, because the edit that added them had
silently failed to apply. An instrument that is not wired reads exactly like
the thing it would have measured being absent. The fix was right by luck of
the second look; the reasoning that produced it was not.

Counting is still 2/5 at a 6.7 second median against interpreting's 1.8, so
whatever is slow there is slow for its own reason. Sending only the new part of
the utterance rather than the whole monologue did not move it, which rules out
prompt size and leaves it open.

### Final state of the scenario suite (F25)

Nine scenarios, three runs each. 15-17 of 27 across runs, and the spread is two
scenarios wide, so the number is a range rather than a figure.

What has been established, in the order the failures were peeled back:

**Ordering from a waiter passes consistently.** So does an acknowledgement that
must not stop the agent, and a policy honoured through the speaker's pauses.

**Interpreting live is 4/5 over five runs at a 1.9 second median.** Both
sentences carried over into English while the other party keeps talking. This
is the case I was least confident a cascade could do at all.

**Counting is right in shape and unreliable in the tail.** The off-by-one is
gone - "One" now lands on the first animal rather than four seconds earlier on
the sentence that asked for counting - and the median wait fell from 6.4
seconds to 2.0 when premature interjections stopped spending the slot. The
second animal is still missed about three runs in five, and the trace says why
without saying what to do: the model chooses speak-through for it, the
interjection runs and reports no error, and the voice produces nothing. Told to
count and say nothing else, having already said "One", it stays silent.

**The visual case now reports what it saw** and fails only on restraint: it
also speaks after the frame showing the build still running, which is not the
moment anybody asked about.

**Cutting in on something wrong is the one that has never passed.** It
interrupts in 630-843 milliseconds, the fastest response in the suite, and says
"got it" or "are you sure?" instead of the date the contract gave it.

The pattern across the last three is one thing: the decision layer is choosing
correctly and the voice is not saying what the choice was for. That is a
different problem from the one this work started on, and a smaller one.

### The act was carried out and nobody could hear it (F26)

Two bugs, found by reading the recorded audio durations rather than the
transcripts.

**The voice never received the deployment's own instruction.** The phase
prompts are composed once, when the engine is built. A session's instruction
does not arrive then - every client sends it in `session.update`, the official
SDK included - and `Update` stored the new settings without recomposing
anything. The interaction model was unaffected, because its situation reads the
current settings on every decision.

That is the whole of the split under the cutting-in case. Told the deadline was
the third, the decision layer correctly recognised a wrong date mid-sentence
and cut in; the voice, which had never been told what the right date was,
invented one. Probed in isolation with the instruction present it said
"Actually, the deadline is the third" three times out of three. Wired up, the
scenario went from 0/5 to 2/5 in one step, and every remaining failure was a
timing failure rather than a content one.

**Every interruption was cancelled by the sentence it was interrupting.** The
shipped barge-in policy yields the floor the moment the user speaks. Speech the
agent began on purpose over somebody who already had the floor is the one case
where that is wrong, and the policy could not tell the difference. The
recording says it without ambiguity - turns produced in silence emit four to
six seconds of audio, and turns produced over somebody emit exactly one
hundred-millisecond frame and then stop:

```
8781 TEXT (user speaking) 'Actually, the deadline is the third of the month.'
9781 DONE  audio=100ms
```

The act chose right, the voice said the right words, and nothing audible
reached the person it was for. The guard for this already existed and was drawn
too narrowly: a continuer had it, on the argument that "cancelling it because
the user kept talking would be the agent interrupting itself for having said it
was listening" - which was never an argument about backchannels.

The first attempt read the marker from the duplex state when the audio was
queued, and failed on the case it was written for. The recogniser cuts one
continuous sentence into several stretches, so by the time a correction is
queued the stretch it answers has already been closed and reopened. It comes
down from the request now, where the decision to speak into somebody else's
turn was actually made.

**Cutting in on something wrong is 3/3, having never once passed.** Asked not
to be interrupted went to 3/3 and the control question to 3/3.

### What the suite total hides

15 of 27, which is what it was before both fixes. That is the least interesting
fact about them.

| | before | after |
| --- | --- | --- |
| cutting in on something wrong | 0/3 | **3/3** |
| asked not to be interrupted | 2/3 | **3/3** |
| an ordinary question | 2/3 | **3/3** |
| a recorded menu | 2/3 | 0/3 |
| translating as they speak | 2/3 | 1/3 |
| ordering from a waiter | 3/3 | 2/3 |
| count-as-they-go | 1/3 | 0/3 |

Every scenario in the lower half asks for silence somewhere, and every one of
them was passing that check with speech that had been cut to a hundred
milliseconds. The agent was talking over a recorded menu, over a waiter, and
over a build that had not finished the entire time; it was simply inaudible.
Fixing the audio did not cause those failures, it uncovered them, and a
measurement that reported them as passing was measuring the wrong thing.

So the remaining work has a shape now, and it is two shapes rather than one:

- **speaks when the act should be `listen`** - the recorded menu, the frame
  showing the build still running. This is restraint, and it is the interaction
  model's decision.
- **silent when an act said to speak** - the second animal, the second
  sentence, the dish that fits. The act is chosen, the interjection runs, no
  error is reported, and the voice produces nothing.

### The sentence the turn existed for (F27)

Both remaining "silent when an act said to speak" failures were one bug, and it
was not in the interaction model, the prompt, or the voice.

Every act that speaks into somebody else's turn runs on a sentence the speaker
has not finished, so it is not in the trajectory. That sentence went into the
instruction and nowhere else, which left the conversation handed to the
provider ending with whatever the agent last said - and a provider asked to
continue from its own last turn, with nothing new addressed to it, says
nothing.

Probed directly against Gemini 3.5 Flash, with the policy "count them as I
mention them", the agent having already said "1", and the next animal in the
instruction:

| what the conversation ends with | three calls |
| --- | --- |
| the agent's own "1" | `''` `''` `''` |
| the same call, the animal as a user turn | `'2'` `'2'` `'2'` |

Five instruction variants were tried first, down to a six-hundred-character one
stripped to the policy and the act. Every one returned empty. It was never a
wording problem, and the ablation that proved it was cheap - which is an
argument for reaching for it earlier than I did.

The runner already had exactly the mechanism: a provisional observation shown
to the provider and appended to nothing, written for preparation. Only
preparation used it.

### The suite at 21 of 27

| | before | after |
| --- | --- | --- |
| count-as-they-go | 0/3 | **3/3** |
| a recorded menu | 0/3 | **3/3** |
| translating as they speak | 1/3 | **3/3** |
| cutting in on something wrong | 3/3 | 3/3 |
| an acknowledgement is not an interruption | 3/3 | 3/3 |
| an ordinary question | 3/3 | 3/3 |
| asked not to be interrupted | 3/3 | 2/3 |
| ordering from a waiter | 2/3 | 1/3 |
| telling them what it saw | 0/3 | 0/3 |
| **total** | **15/27** | **21/27** |

Counting is the demo this work started from and it now passes all three runs at
a 433ms median, having been the case I could not make work at all. Interpreting
live passes all three. The phone menu passes all three, and it does it by
pressing the key and saying nothing, which is the whole point of the act.

What is left is restraint and one hallucination:

- **telling them what it saw, 0/3.** It speaks at the frame showing the build
  still running, and then answers the frame that matters 39 seconds late.
- **ordering from a waiter, 1/3.** It spoke at the right moment and said
  "that's it, we want a table for three" - nothing in the conversation.
- **asked not to be interrupted, 2/3.** One run spoke during a long pause.

### The suite at 37 of 45 (F29)

Nine scenarios, five runs each. Six of the nine pass every run.

| | 15/27 | 21/27 | 37/45 |
| --- | --- | --- | --- |
| count-as-they-go | 0/3 | 3/3 | **5/5** |
| asked not to be interrupted | 3/3 | 2/3 | **5/5** |
| a recorded menu | 0/3 | 3/3 | 3/5 |
| cutting in on something wrong | 3/3 | 3/3 | **5/5** |
| ordering from a waiter | 2/3 | 1/3 | **5/5** |
| translating as they speak | 1/3 | 3/3 | 4/5 |
| an acknowledgement is not an interruption | 3/3 | 3/3 | **5/5** |
| telling them what it saw | 0/3 | 0/3 | 0/5 |
| an ordinary question | 3/3 | 3/3 | **5/5** |

The number moved because six defects between a decision and its execution were
found and fixed, not because the interaction model got better at deciding. It
was never the thing in the way. What was:

1. the voice never received the session's own instruction
2. every interruption was cancelled by the sentence it was interrupting
3. the sentence an interjection exists for was never in the conversation
4. a repair required twice was listed twice and killed the session
5. a picture reached only the model that can see
6. the back half of a cut sentence revoked what the front half asked for

and two more that only became visible once the acts started being carried out:
a key pressed nine times in one call, and a safe point's refusal reported as a
session failure from four different places.

### What is left

**telling them what it saw, 0/5, and it is now one check.** It reports the
build finishing, correctly, in every run. It says nothing at the frame that
does not matter. What it misses is the latency bound: first audio 4.87s after
the frame against 3s.

That is close to the floor of this pipeline rather than a defect in it.
Narrating one frame costs 1.45s at the median (measured directly, five calls),
and the ordinary path from a finished observation to first audio is 1.6-1.8s in
every other row of this suite. 1.45 + 1.7 is 3.15s before anything goes wrong.
The bound beside it - the phrase within 4s - now passes, and audio cannot
precede the text it is synthesised from, so the two bounds as written cannot
both be met. The check is left failing rather than adjusted: the system does
not meet it, and that is the honest report.

**a recorded menu, 3/5.** Two runs still say something out loud to a recording.

**translating as they speak, 4/5.** One run left a greeting in Mandarin
instead of carrying it into English.

### The variance is part of the result

Across five-run passes today the total read 30, 35 and 37 of 45 on builds that
differed by one or two fixes, and single scenarios moved by two runs between
passes on identical code. Five runs is the floor for saying anything about one
scenario, and the total is a range rather than a figure. Both of today's
regressions - one measured, one reverted - were only visible because the pass
was five runs wide.

### Where each scenario comes from

Recorded because it was got wrong once. Interpreting live is one of the demos,
not a case that came from deployment, and the suite's own table said otherwise
for a while.

| scenario | source |
| --- | --- |
| count-as-they-go | demo: counting to a spoken policy |
| cutting in on something wrong | demo: correcting mid-sentence |
| translating as they speak | demo: interpreting live |
| telling them what it saw | demo: acting on a screen |
| waiting out a silence they asked for | demo: time awareness |
| an acknowledgement is not an interruption | overlap, from the demos' four streams |
| a recorded menu | production: IVR navigation |
| ordering from a waiter | production: a third party whose moment passes |
| somebody else's conversation | production: a room with other people in it |
| asked not to be interrupted | a policy set out loud, either source |
| an ordinary question | the control |

The last two matter for reading the rest. **An ordinary question** is there
because most of this suite asks for silence, and a system that had simply
stopped talking would pass almost all of it. **Somebody else's conversation**
is the complement of the waiter: one demands acting on a third party within
seconds, the other demands ignoring one completely, and a system that always
does either passes one and fails the other. Neither number means anything
without its pair.

### One five-run pass cannot rank two models (F30)

Counting scored 2/5 in a suite pass and 3/3 forty minutes later on the same
code and the same model. Nothing behavioural changed between them.

Collecting every pass taken tonight, per scenario, across builds that differ by
one or two fixes:

| scenario | passes seen |
| --- | --- |
| count-as-they-go | 0/3, 2/5, 3/3, 4/5, 5/5 |
| a recorded menu | 0/5, 1/5, 2/5, 3/3, 3/5 |
| translating as they speak | 1/5, 2/5, 3/5, 4/5, 5/5 |
| ordering from a waiter | 0/5, 1/3, 3/5, 4/5, 5/5 |

Four scenarios whose per-pass rate spans nearly the whole range. Some of that
is real - fixes landed between passes - and some of it plainly is not, because
two passes on identical code sit at opposite ends.

The consequence is uncomfortable and worth stating plainly: **several of
tonight's model comparisons were drawn from one five-run pass each, and on
these four scenarios that is not enough to rank anything.** What survives is
only what has a mechanism behind it as well as a number:

- **The visual case, 0/5 to 5/5.** Not noise: the frame reaching the decision
  directly is a different input, the narration it replaced was measured at 1.45
  seconds, and every run improved.
- **Qwen3.6's collapse.** Not noise either: counting, the waiter and the clock
  all went to zero at once, and the eval predicted it - the model buys nine
  points of balanced accuracy entirely with restraint and pays three cases of
  acting for them.
- **Everything else is unranked.** The 8B against the 30B end to end, the VL
  against the text 30B on speech, the menu against anything - one pass each,
  and the spread above says one pass is a coin.

What the suite is good for is finding defects, which it has done all night:
every fix in this document came from reading a failing run rather than from a
score. What it is not yet good for is comparing two systems that are close,
and no amount of care in reading a single number fixes that. That needs more
passes per model than a night has room for, or scenarios whose outcome does not
turn on a recogniser hearing "build" rather than "bill".

### Eleven scenarios, everything in (F32)

39 of 55. Six of the eleven pass every run: asked not to be interrupted,
cutting in on something wrong, ordering from a waiter, waiting out a silence
they asked for, an acknowledgement is not an interruption, an ordinary
question.

What the remaining five say, read from their traces rather than their scores.

**telling them what it saw, 1/5.** It was 5/5 an hour earlier, and the drop is
a check I added rather than a regression. The trace is why:

```
 4301  I will let you know as soon as the build finishes.
 7935  The build has finished.
19087  Actually, correction: the build is still running.
```

It announces the finish at 7.9 seconds, looking at a frame that says 41%, and
corrects itself eleven seconds later. Every other check here asks whether it
said the right thing near the right moment; none of them asked whether it knew.
A model that can see is not thereby a model that looks.

**count-as-they-go, 3/5.** Still speaking where nothing was asked: "I'm
listening. Tell me about your afternoon." on a line with no animal in it. The
wait token did not fire once in the whole pass, and the instruction carrying it
is only attached when a policy is already in force - so the next question is
whether the policy is pinned by then, not whether the voice ignored a rule it
had.

**a recorded menu, 1/5.** The state machine works: the call reaches order
status, so the key is right. It takes 5.6 seconds to press it, because a silent
act runs through the reasoner - the only phase with executable authority - and
a reasoner deliberating over a full trajectory is not a reflex. The act is
correct and the path to it is the wrong shape.

**somebody else's conversation, 0/5.** Structural, and measured to be so: the
cascade discards who spoke before any decision is taken. The same twelve
seconds handed to an audio-native model chose listen five times out of five,
and answer five out of five when it was really the user. Not a defect in the
decision layer.

**translating as they speak, 4/5.** One run in five.

The shape of what is left is worth stating: one scenario is blocked by the
architecture rather than the implementation, one by a synthesiser and a
reasoner being on paths that want a reflex, and two by a model acting on
conditions that have not been met - which is the same failure the step-by-step
eval predicts for this model, and the one thing here that a better decision
model would actually fix.

### The synthesiser was not the model's fault (F33)

Nine hundred milliseconds to synthesise one sentence is too slow for speech
somebody is waiting on, and almost none of it was the model. Measured against
the Fish Speech 1.5 checkpoint on this GPU:

```
generating the semantic tokens for a one-second phrase    ~90ms
firefly-gan vocoder                                        ~40ms
the upstream API server, same request                      375ms
```

The upstream server routes every request through `TTSInferenceEngine`, which
calls `torch.cuda.empty_cache()` and `gc.collect()` after each synthesis, so
every request pays to rebuild the allocator state the last one tore down. Going
straight from the semantic token queue to the decoder, with CUDA graphs
compiled and warmed at startup, a comma-length phrase returns first audio in
111-230ms. Inside a real turn it is 161ms.

Serving it at all needed one fix in the vendored tree, and it is the kind worth
recording because nothing about it looks like a TTS problem: `decode_one_token`
is captured as a CUDA graph, and `decode_n_tokens` kept a view of its output in
`cur_token` and fed that view to the next call, which overwrites the buffer.
Torch detects this and refuses. The release predates the version that checks,
so upstream never hit it.

### Where the wait actually was (F34)

With synthesis at 161ms the control scenario still took 933ms, and the
instrumentation that answers why is one number: `voice-first-token` was 673 to
871ms against a `voice` stage of 713 to 907ms. The first token and the last
arrived together. Everything downstream of the word-model - streaming text into
the synthesiser, cutting at commas, any pipelining at all - was chasing about
40ms.

The word-model was Gemini 3.5 Flash and the wait was its time to first token.
Qwen3-VL-30B-A3B-FP8, already served locally for the interaction policy,
answers the same prompt in 15 to 45ms with prompt size barely mattering. The
control scenario went to 238ms:

```
endpoint-silence gate                    120ms
word-model (local), whole stage           58ms   of which 20-48ms to first token
synthesis, first audio                   161ms
```

Interrupting a wrong statement measures 17ms p50 in the same pass, and ordering
from a waiter 69ms.

A turn also read `turn=3619ms voice=840ms`, and the missing 2.8 seconds was the
reasoner, which recorded no stage at all. It is 2.6 to 3.2s and sits off the
first-response path.

### A speaker who was a different person every sentence (F35)

The suite's third-party scenarios rely on a voice name to tell the user from
somebody else in the room. The name changed nothing that mattered. Fish is
zero-shot, and asked for speech with no reference it invents a speaker - a
different one on every call. Measured with ECAPA-TDNN embeddings over the
benchmark's own cached audio:

```
two lines from the same scripted speaker      0.27
a user line against a third-party line        0.43
```

Two lines from one speaker scored what two strangers score, and the pair that
was supposed to differ scored higher. So the scenario about a third party
talking near the microphone was not posing that case, and no amount of work on
the decision layer could have fixed it.

Enrolling each named voice once from a fixed reference and keeping it: the same
speaker now scores 0.60 and 0.77, different speakers 0.15 to 0.17.

### The model was answering a false premise (F36)

An earlier note called this scenario structural - "the cascade discards who
spoke before any decision is taken" - and left it there. That was half right.
The cascade does discard it, but the situation did not report the loss: it
named whoever was heard as the user. The trace is unarguable.

```
Now:
agent: not speaking
user: speaking right now
heard from user so far: "Did you get the milk."
```

Every partial decided listen and every endpoint decided answer. The model was
answering correctly; the premise was wrong.

With an ECAPA embedding of the first second of each utterance compared against
the voice the session was opened with, the same instant reads:

```
someone else in the room: speaking right now
heard from someone else in the room so far: "Do you get the milk."
```

The verdict settles within about 300ms of an utterance starting and is asked
for off the decision path. No evidence leaves the prior in place, which is that
whoever is talking is the person whose session it is - under a second of audio,
or no endpoint configured, and nothing changes.

That alone did not fix the scenario. The model had the evidence and no rule for
it, and the worked examples contained third-party speech exactly once, in the
phone menu, where acting was right - the same one-sided teaching the file's own
comment warns about for visual evidence. With both, 0/5 to 2/3.

### Two ways to get a standing instruction wrong (F37)

Counting an afternoon with two animals in it, the agent counted to sixteen: one
before any animal was mentioned, two three and four through a sentence about a
river, and five through eleven during the sentence with the capybara - one
number per revision of the same utterance.

Two instructions were telling it to. The interaction instruction said a
standing policy "applies to every piece of a broken-up sentence exactly as it
would to a whole one", which is true of the obligation and false of the
trigger. The voice was told "if they asked for a count, say the next number",
which counts turns rather than animals.

Correcting both moved it from counting sixteen to counting nothing at all, and
the reason is worth more than either fix. The new rule said to judge the
condition against what was new since the agent last spoke. The renderer omitted
that line when there was nothing new **and** when everything was new, so the
two opposite situations rendered identically. Told to judge against what was
new, and shown nothing about it, the model read every partial as already
answered.

The ambiguity had been harmless for as long as no rule depended on the line. It
became a scenario-wide failure the moment one did.

### The failure was in a different process (F38)

The control scenario - a finished question with nothing standing in the way -
scored 0/5, and its transcript held two events: the environment became ready,
and the user started speaking. Nothing was ever recognised, the gate never
closed, and eighteen seconds of playback went by with nobody answering a
question about the capital of France. Read as a decision it says the model
chose silence. It was not a decision.

The synthesiser had grown from 3.7GB to 38GB over a few hundred utterances and
filled a 98GB GPU, and the recogniser had no room left. Removing the upstream
server's per-request `empty_cache()` bought 245ms and, over a few hours of
utterances of every length, cost the whole machine. The allocator was
fragmenting rather than leaking, which is what expandable segments exist for;
they cost nothing per request. The trim stays as a backstop and runs when the
process is holding more than 8GB rather than after every request.

Worth stating plainly because it cost most of a suite run to find: a benchmark
that scores behaviour cannot tell a model that decided to stay quiet from a
pipeline that stopped feeding it. Both look like silence.

### A line that runs into the next one (F39)

Three of the failures this session were one bug in the harness. A script says
when a line starts, against durations the author heard from whatever
synthesiser was in use that day, and a slower one runs the lines together.

Within one speaker it destroys the endpoint: the counting story is three
sentences eight seconds apart, and joined into one utterance the gate opened at
13.2 seconds and did not close for the remaining twenty-seven, so the agent
heard the first sentence and nothing after it.

Across speakers it is worse, because the utterance is still well formed. The
caller asking for the call and the recording answering it arrived as one
sentence - "and find out where my order has got to Thank you for calling Press
one for billing" - and the agent pressed a key at the person who had asked for
the call to be made.

Every script in this suite is a conversation between people taking turns. The
scenarios about simultaneous speech are about the agent talking over somebody
or somebody talking over the agent, and neither is two scripted lines at once.

### A tool being available is not a moment to use it (F40)

The phone menu chose call-tool 283 times in one call. The first was at 2.5
seconds, on the partial "Ca." - it was acting on the user asking for the call
to be made, and there was no menu, no option and nothing to press. Four got
through the guards and spent the silent-act budget before the recording had
said a word; the guards then refused the 279 that followed, including the one
that mattered.

call-tool appeared in exactly one worked example, where a recording had just
named an option and acting was right. What that taught was that press_key plus
somebody mentioning a call is enough. This is the third time in this file the
same shape has appeared - visual evidence, third-party speech, and now an
attached tool - and each time the fix was an example on the other side.

The pattern is worth naming, because it is cheap to check and expensive to
miss: **any kind of evidence that appears in the examples only where it
justifies acting teaches that it is a reason to act.**

### Saying nothing has to be sayable (F41)

The voice is told to agree to a request once, to say a holding line once, and
not to answer the tail of a sentence it has already answered. All three ask for
silence, and the paragraph explaining how to produce silence was attached only
when a standing policy was already in force.

On every other turn the rules asked for something the model had no way to do,
and a model whose only channel is speech says something instead. Measured on
one instruction the recogniser split into three: "I'm ready. Please tell me
about your afternoon", "I'm listening. Please start telling me", "I'm ready.
Please begin describing your afternoon."

The comment beside that paragraph already recorded this exact failure from an
earlier measurement. The fix had been applied to one prompt path and not the
other.

### The instrument was one session behind (F42)

Two suite runs in a row scored worse than the one before them, and the control
scenario - a finished question with nothing standing in the way - scored 0/5 at
the end of both while passing 3/3 when run on its own. Anything that passes
alone and fails in company is a state that outlives what it belongs to.

The recogniser learns whose session it is from the first voice it hears, and it
was built once for the process. So it enrolled the first speaker of the first
scenario in a run, and every user in every scenario after that was a stranger.
The shadow log shows a single sentence changing hands halfway through:

```
listen  who=user                     heard "Everything you know."
listen  who=someone else in the room heard "Everything you know about theef."
```

The agent then correctly declined to answer a stranger, having just been taught
what a stranger means. Both halves of this session's work - the evidence and
the rule for reading it - were behaving exactly as designed on a premise that
was wrong for every session but the first.

Worth stating with the other one: a benchmark that scores behaviour cannot tell
a model that decided to stay quiet from a pipeline that stopped feeding it, and
it cannot tell either of those from a fact about the world that leaked between
two runs. All three look like silence.

### The suite at 40 of 55, on local models throughout (F43)

```
count-as-they-go                          2/5    3156ms p50
asked not to be interrupted               5/5
a recorded menu                           0/5    8179ms p50
cutting in on something wrong             5/5    2028ms p50
ordering from a waiter                    5/5      91ms p50
translating as they speak                 4/5      67ms p50
waiting out a silence they asked for      1/5    3778ms p50
somebody else's conversation              5/5    2837ms p50
an acknowledgement is not an interruption 5/5     307ms p50
telling them what it saw                  3/5      65ms p50 spoken, 2862ms seen
an ordinary question                      5/5      60ms p50
```

Against the cloud voice at 43/55 the total is lower and the composition is
different: the third-party case was structurally impossible before and is now
5/5, the waiter went 4/5 to 5/5, and the control answers in 60ms rather than
1522ms. What is left divides cleanly. The menu is an act carried out too late
by a path that wants a reflex; the silence and the counting are a policy read
off a fragment of the sentence that set it; the visual case is a model claiming
to have seen something before any frame arrived.

### Nothing at zero (F44)

```
                                          suite17   suite20
count-as-they-go                            2/5       3/5
asked not to be interrupted                 5/5       5/5
a recorded menu                             0/5       2/5
cutting in on something wrong               5/5       4/5
ordering from a waiter                      5/5       5/5
translating as they speak                   4/5       4/5
waiting out a silence they asked for        1/5       1/5
somebody else's conversation                5/5       3/5
an acknowledgement is not an interruption   5/5       4/5
telling them what it saw                    3/5       3/5
an ordinary question                        5/5       4/5
                                           40/55     38/55
```

The totals are the same number twice, and the movement inside them is mostly
the variance this file has already measured: four of these scenarios span
nearly their whole range across passes on identical code. What did change is
that nothing sits at zero any more, which had not been true of the menu at any
point before.

The menu took five separate fixes to get off zero, and only the first was about
deciding:

- the interaction model chose call-tool on the request for the call rather than
  on the menu, and spent the silent-act budget before the recording spoke;
- the guard against acting twice refused every revision that extended what it
  had acted on, so one press at a greeting disabled the rest of the call;
- the check measured the wait from the moment the menu line ended, so a key
  pressed while the options were still being read counted as never answering;
- the holding line fired during the silent act and read "I'm calling now,
  please hold" to a recording that could not hear it;
- and the voice wrote "Pressing the key for order status. <wait>", which the
  publish path spoke in full because it matched the token only when it stood
  alone.

Four of the five are the same bug class as everything else in this file: the
act was decided correctly and something between the decision and the world
undid it.

### The voice was blind (F45)

The visual case scored 0/5 and the agent, looking at a screen that reads BUILD
FINISHED SUCCESSFULLY, said "the build finish condition has not been met yet. I
am monitoring for it." Handed the same PNG directly, the same model answers:
"Yes, the build has finished successfully, as indicated by the message BUILD
FINISHED SUCCESSFULLY and the confirmation that all 214 tests passed in 41
seconds."

`-fast-sees` defaults to false, which is correct - a text-only voice handed an
image fails in a worse way - and the deployment never turned it on. The Gemini
adapter declares vision unconditionally, so the cloud voice had been seeing
frames all along and the local one never had. Every comparison of the two on
the visual case was a comparison of a model that could see against a model that
had been blindfolded.

With it on, 3/3 and 391ms from the frame to speech.

Two things are worth taking from this beyond the flag. The scenario had a check
for exactly this failure - "nothing has been seen yet, so there is nothing it
can know has finished" - and it did not fire, because a blind model that
declines to claim anything passes a check about not claiming things. And the
situation handed to the decider said `just seen: The user attached an image.`
in both cases, which is what the runtime says when it has decided the picture
itself is going to the model. It reads identically whether that happened.

### The prompt is evidence, and it was the last thing anybody looked at (F46)

Four rounds of reasoning went into why a counting policy answered "3 4" from a
model that answers the same question correctly five times out of five when the
conversation is assembled by hand. Each round proposed a difference, fixed it,
and measured no improvement. The interaction shadow could not settle it: it
records what the deciding model saw, and this was the other model.

Recording what the voice is given took twenty lines and answered it
immediately. Three things were wrong at once and none of them was arithmetic:

```
agent: I do not have an active task or any previous details on record...
agent: No animals have been mentioned yet. Please tell me about your afternoon...
agent: 0
```

The first two are answers to "I'm going to tell you." - a recogniser hands over
an unfinished sentence as though it were a turn, and the voice, having nothing
to answer, asks what the task is. Spoken aloud, into a policy that said to say
nothing else, and then part of the context every later turn had to count
through. The third is the voice writing a zero it had been told to replace with
the wait token, which is then read out and becomes the number every later count
was measured from.

The lesson is not about counting. A system with two models in it needs both
their inputs recorded, and this one had recorded one of them all along.

### One instruction, four wordings, and the shape of the fix (F47)

The running-commentary instruction has been rewritten five times, each time to
fix a measured failure, and the sequence is worth keeping because every step
looked right when it was made:

```
"say the next number"                   counted turns: one capybara became 5,6,7,8,9
"count them from the transcript"        gave "1 3 3 1": each partial a fresh question
+ "wait if the new part has none"       contradicted the line above it
"count from the beginning, say if changed"  waited through the first animal 5/5
worked through, both ends stated        5/5 on all four counting cases
```

And then the waiter scenario started counting - "4 4 4" for a dish - because
three paragraphs of counting arithmetic sat in front of one conditional
sentence, and the shape of an instruction teaches as much as its content.

Removing the arithmetic fixes the waiter and the interpreter and breaks the
count: measured five samples each, the general rule alone gets the sea bass 5/5
and the first animal 1/5, and with the arithmetic attached the first animal is
5/5. Neither rule is right for both, and which one a turn needs is a fact about
what the person asked for - which the composer already has, in their own words.

### The GPU fell over (F48)

Two services stopped answering in the middle of a suite - the recogniser first,
then the policy model - and both looked exactly like bugs in the work that had
just been done. They were not:

```
Fan  Temp   Perf   Pwr:Usage/Cap
ERR!  ERR!  ERR!          N/A / N/A     58222MiB / 97887MiB
No running processes found
temperature.gpu -> [GPU requires reset]
```

`nvidia-smi -r` answers "Not Supported" on this card, and the driver modules
hold 44 and 183 references with no live process behind them, so the modules
cannot be unloaded either. A reboot is the remaining path.

The suite reported it correctly - "ERR ... context deadline exceeded" rather
than five quiet zeroes - which is the third instrument fix in this file earning
its place. A benchmark that scores behaviour cannot tell a model that chose
silence from a pipeline that stopped, and this file now contains three
different ways that has happened: a session stream that ended early, a
synthesiser that ate the GPU, and the GPU itself.

### The recogniser was deleting the thing being counted (F49)

The counting scenario oscillated for a dozen builds - "1 3 3 1", then "4 5",
then silence, then "four five six" - and every fix traded one failure mode for
another. Asked the four cases directly, the model answered all four correctly
five times out of five. The prompt was not the problem.

Round-tripped through this project's own synthesiser:

```
said:  "A capybara wandered over and sat down next to me."
SenseVoice:  "A ki bara wandered over and SAT down next to me."     271ms
turbo:       "A capybara wandered over and sat down next to me."    103ms
```

In the live pipeline SenseVoice rendered the same line as "A cap borroworer"
and "A capy borroworough1". Every prompt fix that oscillated was asking a model
to count animals in a sentence with no animal left in it.

Whisper large-v3-turbo is both more accurate and faster - it decodes with a
fraction of large-v3's layers - and this is not an accommodation for one
scenario. A recogniser that loses proper nouns loses names, places, order
numbers and dish names, and three of the eleven scenarios here turn on hearing
one correctly.

Two things follow beyond the model choice. The first is that a benchmark cannot
measure a decision layer through a perception layer that destroys the input,
and nothing in the scoring said so: the scenario reported "none of [one 1]" as
though the agent had declined to count. The second is the discipline that found
it - recording the inputs rather than reasoning about them. Four rounds went to
the scenario preamble, the holding paragraph, the sampling temperature and the
position of the instruction, all wrong, all disproved in one command each once
the actual prompt and the actual transcript were written down.

### What "stable" meant, and what nothing computed (F50)

The guard meant to stop the agent answering one occurrence twenty times never
fired. It asked the revision what the recogniser had committed to, and every
partial said nothing: the adapter set StableText only on the final revision, so
until then the whole transcript was reported as unstable and the guard fell
back to comparing whole revisions - which is exactly the guard it replaced.

A partial is a whole re-transcription of the buffer. The recogniser revises its
own tail constantly while the front of the sentence stops moving almost at
once, and that settled front is what stable means. Both fields have been in the
perception contract from the beginning and nothing had ever computed them.

### A count is not something a model recounts (F51)

The counting scenario has taken more attention than the other ten together and
the last of it is worth writing down, because the answer is not a fix.

Give any model a conversation in which it has already said "1", "2" and "3",
then one more sentence, and ask it to count the things in what the person said:

```
Qwen3-VL-30B-A3B, current instruction            "4"  8 times out of 8
Qwen3-VL-30B-A3B, told its own numbers are not
  a sequence and the count is of their words     "4"  8 times out of 8
Gemini 3.5 Flash, same prompt                    "4"
```

Its own prior output is a run, and a run gets continued. This is not a Qwen
limitation and not a wording problem: the same instruction that produces "4"
here produces the right answer five times out of five when the conversation
carries no numbers, which is where every hand-test of it passed.

What follows for the design is that the number must never be asked for twice
about one occurrence, because the first answer is reliable and the second is
determined by the first. The runtime side of that is real and now built: act on
what the recogniser has settled rather than on every revision of it, and, when
the agent has already spoken into a stretch of speech, wait for the person to
finish saying something before answering it again.

What is left is not reachable from there. A recogniser that punctuates well
splits "A capybara wandered over and sat down next to me" into two settled
sentences, which is two occasions by any rule the runtime can apply, and only
the model can tell that there is one animal across them - which is the judgement
it does not make once its own numbers are in front of it.

So the honest position is that this scenario asks for something current models
do not do, the runtime has been made to ask as rarely as it correctly can, and
the remaining gap is a capability rather than a defect. Contorting the system
further to close it would be tuning for one benchmark, which is the one thing
this work is not allowed to do.

## F52 - a wait rendered as a result manufactures a reason to speak

The reasoner is asked on most turns and mostly has nothing to add, so it answers
`<wait>`. Every adapter rendered that as background state, behind the hint that
says a tool-call chain has finished and left a written result below, and to
answer from it in your own words. Four consecutive waits reached the voice as
four instructions to say something.

Under a counting policy, what it said was a number, at a sentence about a river
with no animal in it. Asked with the same conversation and no waits in it, the
voice answers `<wait>` eight times out of eight.

The window that renders a conversation for the interaction model already
dropped a wait, in almost these words: a decision to say nothing is not a line
of conversation and not a piece of background state either, it is the absence of
both. The rule existed in one of the two places a trajectory is rendered.

Same shape one layer over: retained reasoning with empty content rendered the
preamble and nothing else, so the voice saw five assistant turns in which the
agent had apparently said nothing. Removing both took hearing from 3157ms p50
to 701ms, because the prompts stopped carrying them.

## F53 - the speaker recogniser was never enrolling, and the prior took over

A speaker telling one uninterrupted story was reported as somebody else in the
room 122 times in one suite run.

The threshold was not the problem. One voice against itself scores 0.47 to 0.77
on whole utterances, against another voice 0.04 to 0.27, so 0.40 sits in a wide
gap. Two duration bounds were wrong.

How much speech a verdict needs, measured as same voice / another voice:

	 300ms   0.219 / 0.145
	 500ms   0.321 / 0.177
	 750ms   0.301 / 0.243
	1000ms   0.430 / 0.219
	1500ms   0.584 / 0.202
	3000ms   0.722 / 0.246

At the old one-second bound the same speaker scores 0.430 against a threshold of
0.40, which is a coin toss, and every toss that lands wrong relabels the person
mid-sentence.

How much speech a reference needs, measured the way one is actually used - built
from one utterance, compared against different utterances later:

	1000ms   0.415..0.429 / 0.093..0.114
	1500ms   0.509..0.563 / 0.060..0.097
	2000ms   0.571..0.616 / 0.114..0.168
	3000ms   0.576..0.667 / 0.091..0.147

Three seconds buys nothing over one and a half. The earlier three-second figure
came from segments of one recording, which is the easy case: same speaker, same
breath, same conditions, words that follow on.

What three seconds cost was the whole mechanism, because the buffer is per
utterance and nobody speaks in three-second sentences on purpose. Somebody who
opened with "I'm just going to get on with this for a bit" - 2.09 seconds -
never enrolled, and the failure is not degraded identification. With no
reference the prior takes over: whoever is talking is the person whose session
this is. The stranger asking somebody else about the milk was read as the user
in every run.

Failing to enrol is worse than enrolling on less. Third-party scenario 0/5 to
3/5, waiter to 5/5.

## F54 - a guard whose escape needs the guarded thing to succeed cannot escape

The guard that stops the agent counting the sentence which asked it to count
compared what it heard against the text extraction last read. Those were the
same thing until extraction began re-reading a whole turn on every word, at
which point "the text extraction last read" became "whatever they are saying
now" and the guard refused every occurrence there was - eighty times in one run.

The bound added to release it was that a policy cannot have been carried out
before it was set, so once the agent has spoken under one the moment is over.
That is true and it could never fire, because the guard was refusing the first
carry-out. A bound that needs the thing it guards to succeed once cannot be the
only bound.

The runtime now records the speech that set a policy at the moment the pinboard
gains one it did not have. Counting 0/5 to 3/5.

## F55 - the suite has run-to-run noise that a single pass cannot see through

Three consecutive full runs on nearly identical code returned 38/55, 38/55 and
36/55, and the per-scenario distributions moved much more than the totals: the
silence scenario went 4/5 to 1/5, the third-party scenario 3/5 to 1/5, and the
control - an ordinary question, which should never fail - returned 4/5.

Synthesised audio is already cached between runs, so this is not synthesis
variance. What is left is the system: sampling, and the timing of a pipeline
whose parts race.

The consequence for method is that a single pass cannot rank two versions that
differ by a scenario or two, and reporting one as an improvement over the other
is reading noise. Scenario-level probes at five repeats are what moved this
work; full-suite totals are for direction.

## F56 - Qwen3-VL-30B-A3B beats Qwen3-VL-8B on this GPU, on both axes

Run one at a time on the same GPU, same port, same served-model-name, so
nothing else in the system changed. Co-residency was rejected deliberately: only
one model serves the fast phase in deployment, and two would contend for memory
and scheduling and make the latency meaningless.

End to end, the full suite at five repeats:

	Qwen3-VL-30B-A3B-Instruct-FP8   35, 38, 38, 36 / 55
	Qwen3-VL-8B-Instruct            24 / 55

The gap is far outside the run-to-run spread of F55, so one pass is enough to
separate them. Per scenario the 8B loses everywhere it matters and holds only
the easy cases:

	                              30B      8B
	an ordinary question (control) 4-5/5   5/5
	asked not to be interrupted    5/5     5/5
	a recorded menu                4-5/5   5/5
	an acknowledgement             5/5     4/5
	cutting in on something wrong  4-5/5   3/5
	ordering from a waiter         3-5/5   2/5
	count-as-they-go               0-3/5   0/5
	translating as they speak      2-3/5   0/5
	waiting out a silence          1-4/5   0/5
	somebody else's conversation   0-3/5   0/5
	telling them what it saw       4-5/5   0/5

Two of those are worth naming. The visual case goes to zero: both are VL models
and the 8B does not hold up on it. And the silence case goes to zero, which was
predicted before the suite ran, from the judgement tests below.

The judgements the system actually depends on, measured directly:

	                        30B      8B
	extraction              5/5      5/5
	restriction classifier  12/14    11/14
	scope classifier        10/11    7/11
	voice: nothing to count 10/10    10/10

Scope is the one that predicts the end-to-end result. F51 and the silence
scenario showed what a scope error costs: a policy read as passing expires with
the turn, nothing is in force, and the agent falls back on an ordinary reply. A
model wrong four times in eleven produces that failure constantly, and the 8B's
0/5 on the silence scenario is exactly it.

Latency goes the same way, which is the part worth remembering:

	30B-A3B-FP8   17ms p50 on a short decision
	8B-Instruct   33ms p50

The bigger model is twice as fast. Thirty billion parameters with three billion
active, quantised to FP8, moves less weight per token than eight billion dense
at bf16 - so the parameter count in the name is the wrong thing to compare, and
on this GPU there is no quality-for-speed trade to make here at all.

The 30B-A3B stays.

## Capability-composed interaction architectures (F52)

The architectural question is no longer encoded as mutually exclusive
binding names. A reportable cell records the selected ownership vector and the
available stack capabilities. The controlled matrix has four levels:

| Cell | Foreground | Interaction evidence/owner | What it isolates |
| --- | --- | --- | --- |
| P | cascade or speech model with shipped predicates | engine acoustics and narrow predicates | established baseline |
| T | component or speech-to-speech foreground + policy ASR | one engine `InteractionModel`, including endpoint/overlap acts | replaceable full text-policy controller |
| C | the same T foreground and evidence plus predicates | explicit predicate-floor arbitration | whether grounded endpoint/overlap control complements learned semantic acts |
| N | the same capable foreground with native interaction selected | model multimodal state | information and integration gain from native interaction |

P→T and T→C may be run on the current cascade as controller ablations. T→N is
an architecture comparison only when the same foreground model can expose both
selections. Comparing Qwen in T to Moshi in N is useful system evidence but does
not identify the cause: model family, training data, audio codec, model size,
and interaction architecture all changed.

The minimum retained identity for every cell is:

- foreground model and revision;
- ownership for interaction and floor;
- full stack capability vector;
- interaction policy model, prompt/instruction revision, and decision timeout;
- broad policy evidence source, exact selected evidence-capability vector, and
  recognizer, speaker-identity, and visual-narrator revisions (`text-policy` is
  never relabeled audio-native);
- exact selected predicate/text/native/remote controller vector and its
  single-writer arbitration rule;
- sidecar protocol version and whether typed acts were accepted or translated;
- slow provider, tool authority, hardware, and audio fixture identity.

Primary outcomes are act correctness by act and speaker state, false
interruption rate, missed-intervention rate, decision-to-act latency, endpoint
latency, tool success, and task success. ASR destruction rate is reported
separately because a controller cannot decide from evidence its recognizer
removed. Confidence and abstention are retained rather than coerced into the
chosen act.

The repository now supports the T cell as `omni+text-policy`. Raw audio still
goes to the speech-to-speech foreground; a second recognizer exists only on the
control plane. Protocol v2 carries the selected act, policy identity, evidence
reference, floor semantics, deadline, and confidence. All seven acts have
binding and protocol behavior tests. No ranking is claimed until paired,
complete P/T/C/N cells exist under the identity rules above.

### F52 experiment gate and local conformance diagnostic (2026-08-27)

F52 is now executable as a versioned architecture experiment rather than an
informal binding label. `bench/architecture` validates the full identity above,
the scenario driver captures post-handshake `binding.Status()` for every task,
and `bench architecture` distinguishes an adjacent, controlled
`architecture-only` comparison from a confounded but still useful `system`
comparison. Desired cells can be retained as `unavailable` with a reason, so a
missing same-foreground N cell cannot silently disappear from the matrix.

A local P/T conformance diagnostic ran `count-as-they-go` once on the same
cascade foreground, SenseVoice recognizer, Fish Speech voice, slow provider,
hardware, and scenario fixture. P passed; T failed two count-order checks after
saying later numbers instead of one and two. P's per-run reaction-latency P50
was 4,660 ms and T's was 2,518 ms. This is deliberately **not an architecture
result**: one of eleven scenarios ran, each latency distribution has one
conversation sample, and the worktree was modified. The comparison artifact
refused publication for all three reasons. Its value is narrower: both live
sessions matched their declared ownership, capability, policy, evidence,
component-adapter, deadline, and tool-authority identities, proving the P/T
experimental path is mechanically controlled.

No N score was manufactured. The locally available foregrounds still do not
offer one honest model under both external typed-act selection and native
interaction selection. Qwen-T versus Moshi-N remains a system comparison until
that compatibility gap closes.

### Architecture-catalog conformance and requested-silence diagnostic (2026-08-27)

The structural source of truth is now the versioned project catalog rather
than the F52 manifest itself. `cascade.controlled@1` and
`cascade.text-policy@1` were launched through `serve -architecture`, inspected
through normal sessions, authored into cells from live status plus separate
immutable deployment pins, and assembled into a version-2 experiment manifest.
Both sessions attested their exact definition fingerprint and matched the
catalog's ownership, required capability subset, evidence source, transport,
and handoff boundary after provider negotiation.

One paired diagnostic ran `waiting out a silence they asked for`, chosen because
its decisive event is elapsed time rather than answer knowledge. Under the
local Qwen/Whisper/Fish deployment, P did not perform the requested later
check-in; T did and passed, with the one recorded check-in heard about 1,219 ms
after its trigger. P's one other heard reaction was about 920 ms. The
comparison reported `0/1` versus `1/1` and then correctly refused the apparent
100-point difference: ten of eleven scenarios were not attempted and the tree
was modified. One sample cannot support a latency claim either.

This diagnostic supports two narrower facts only. First, the catalog-backed
definition → runtime handshake → authored cell → scenario artifact chain is
mechanically coherent. Second, this scenario distinguishes the two current
controllers and is worth retaining in the full experiment. It is not evidence
that transcript policy is generally better. The next reportable P/T run must
use the complete suite and clean provenance; the N cell still waits for a
foreground satisfying the shared hybrid capability definition.

Two additional paired interaction cases were used as counterchecks rather than
selected because they favored T. Both P and T passed `asked not to be
interrupted`. Both failed `somebody else's conversation`, speaking to the two
nearby third-party turns. The latter is important negative evidence: replacing
predicates with the current transcript policy did not supply speaker/addressing
understanding that the evidence path itself does not contain. These remain
single-run, filtered, dirty-tree diagnostics and support no rate or latency
claim. Together with requested silence they suggest the next architecture
revision should enrich policy evidence with speaker/addressing state rather
than merely enlarging the text-policy model or treating T as a universal fix.

### Exact evidence attestation and the invalid complete diagnostic (2026-08-27)

A later diagnostic completed all eleven scenarios for P and T. It is not an
architecture result even though both processes completed and every task
carried a live architecture identity. T was launched with direct visual input
while its immutable definition claimed only the coarse `transcript` evidence
source. The old status shape could not report that extra channel, so the
authoring and per-task gates could not discover the confound. The observed
pass totals therefore support no P/T ranking and are intentionally omitted
from the claims above.

The defect was architectural rather than a one-off benchmark check.
`InteractionStatus` now attests transcript, acoustic activity, silence clock,
conversation state, tool state, speaker identity, addressing, visual
description, direct visual input, and native model state independently. Stack
capabilities remain a lower bound—unselected native capabilities may honestly
remain present—but selected interaction evidence must equal the definition
exactly. An extra visual channel now closes the runtime and independently fails
cell authoring and per-task validation.

New exact-evidence revisions supersede the coarse experiment definitions
without mutating them: `cascade.controlled@2`, `cascade.text-policy@2`,
`omni.external-predicates@3`, `omni.external-policy@3`, and
`omni.native-policy@2`. Narrated visual, direct visual, and speaker-aware text
policies are separate derived definitions over the same component runtime.
The speaker embedder and visual narrator are pinned as non-treatment component
identities. Addressing remains false: voice identity does not prove who an
utterance addresses.

The failure shape remains useful engineering evidence. Predicate control did
better on some immediate menu/waiter cases; text policy did better on durable
instructions, immediate correction, counting, and a clock-triggered check-in;
both missed third-party conversation and spoke too early on one still-running
visual task. That pattern argues for composable architectures and evidence
channels, not a universal controller. A complete rerun must use the current
exact definitions, remove undeclared direct vision, control speaker and
narrator identity, repeat cells, and start from clean provenance before any
performance claim is reportable.

An exact-evidence rerun then exercised every scenario once with the current
definitions. P passed 6/11 and T passed 8/11. Both cells used the same Qwen
foreground and slow model, Whisper recognizer, Fish Speech voice, SpeechBrain
speaker embedder, session narrator, video observer, tools, authority, and
fixture. All twenty-two task sessions carried live evidence vectors matching
their definitions. The run is still non-reportable: its worktree was modified
and one sample per scenario is not a population estimate.

The per-scenario shape is more useful than the aggregate. T alone passed
counting as the user went, interrupting a user who was doing something wrong,
waiting out requested silence, and reporting what it saw. P alone passed the
recorded-menu and waiter-ordering cases. Both passed the no-interruption,
simultaneous-translation, acknowledgement, and ordinary-question cases. Both
failed the third-party-conversation case.

Three engineering conclusions follow without turning the diagnostic into a
ranking. First, T solved the visual case from pinned narration while its exact
evidence correctly reported no direct pixels; retaining pixels in the policy
was not necessary for that case. Second, the shared speaker embedder did not
solve third-party speech: identity is not addressing, and the addressing-aware
catalog branch must remain unavailable until a real producer exists. Third, a
general text policy can express durable semantic and clock-driven acts that
narrow predicates miss, but it can also add latency and content error: T
missed the menu deadline and invented waiter dishes, while the grounded narrow
mechanism passed those cases.

The next clean experiment should repeat the exact P and T cells across the
complete suite. It should add addressing only after an independently attested
addressing producer exists, and should not manufacture an N result until one
foreground genuinely supports both external typed-act and native-interaction
selection over the same weights.

### Exact controller composition and T/C experiment authoring (2026-08-27)

The evidence diagnostic exposed another treatment that the version-3 artifact
could not state. The old T process reported an act-model floor but retained
immediate predicate barge-in, and the architecture definition did not attest
either choice. Thus “T” could mean a model-controlled floor, a predicate floor,
or a mixed controller depending on launch flags. Exact evidence alone did not
make those runs exact controller experiments.

`InteractionStatus` now independently attests selected predicate, text-policy,
native, and remote controllers plus their arbitration. A single selector uses
`single`. The new C level selects predicates plus text policy under
`predicate-floor`: predicates retain endpoint and overlap decisions; the text
policy retains durable semantic, visual, quiet, and silent-tool acts. Pure T
now routes overlap through the same enumerated-act model as the floor, choosing
`keep-speaking` or `stop-speaking` after transcript evidence rather than
canceling at bare acoustic onset. Its floor and overlap adapters inherit the
same policy deadline the cell attests rather than hiding a shorter local
timeout. Liveness, stale-decision, duplicate-action, and authority fences
remain engine safety invariants rather than policy votes.

New immutable revisions preserve all historical fingerprints:
`cascade.controlled@3`, `cascade.text-policy@3`,
`cascade.composed-policy@1`, `cascade.text-policy-visual-speaker@2`, and
`cascade.composed-policy-visual-speaker@1`. Current Omni, native, full-native,
and remote revisions likewise attest one selected controller. Benchmark
manifest and result schemas are version 4; version-2 and version-3 results stay
readable diagnostics but cannot become current evidence.

The enriched T and C cells were launched, inspected through ordinary Realtime
sessions, and authored into one version-4 manifest. They used identical Qwen
foreground/policy/slow models, Whisper recognizer, Fish Speech voice,
SpeechBrain embedder, session narrator, observer set, evidence vector, tools,
authority, fixture, and hardware. T attested `text-policy/single` with act-owned
floor and barge-in; C attested `predicates,text-policy/predicate-floor` with the
shipped endpoint and immediate-barge-in policies. Scoring waited until the
unrelated audit stopped using the shared model services, so compute contention
was not silently added to the treatment.

The full eleven-scenario diagnostic then completed once per cell. T passed
7/11 and C passed 6/11. The cells satisfy the T/C architecture-only adjacency
and non-treatment identity checks, but the comparison refused publication
before emitting a claim because the worktree was modified; one run per scenario
also remains engineering evidence rather than a rate estimate. T alone passed
requested no-interruption and cutting in on something wrong. C alone passed
the recorded-menu and waiter-ordering cases. Both passed
simultaneous translation, a requested clock-triggered check-in,
acknowledgement overlap, and an ordinary question; both failed durable counting
and third-party conversation. T passed the narrated visual completion case
while C did not in this sample.

The aggregate does not select a winner. It falsifies the stronger claim that
the first fixed composition is automatically the best of both mechanisms.
Predicate endpoint/overlap jurisdiction recovered two narrow, time-critical
action windows, but it also removed the learned controller's semantic
interruption behavior and failed one explicit long-silence policy that T
honored. Pure T retained those semantic decisions but missed the menu and
waiter deadlines. Both cells had `addressing=false`, and both answered nearby
third-party speech; controller arbitration cannot reconstruct evidence neither
controller receives.

The next controller experiment should therefore vary jurisdiction or
arbitration, not invent another binding or model family. Candidate cells should
make endpoint, take-floor, yield-floor, response veto, concurrent speech, and
silent action authority explicit enough that a durable quiet policy can veto a
predicate response opportunity while a grounded fast path can still preserve a
deadline. Those routes must be catalog-selected and live-attested before they
are scored. Repeating T and C from clean provenance remains necessary; this
single diagnostic is a design input, not a performance conclusion.

## F57 - the first caller pays for loading the models, and health says otherwise

The control - one finished question, nothing in the way - failed its first run
and passed the other nine, twice in a row at ten repeats. The opening turn came
back reasoner-led at two seconds where every later one was the voice alone at
seventy milliseconds.

A local model server answers its health check long before it answers a request
at speed: weights are mapped, the graph is not captured, the prefix cache is
empty. Nothing was wrong with any decision.

Warming the models was not enough on its own, because health still answered ok
the instant the listener bound and the first session raced the warm-up. Health
is what a caller asks before deciding the server is ready, and a caller asking
that is asking whether its next turn will be answered properly. It now reports
warming until the voice and the model that decides whether it speaks have each
answered once.

Control 9/10 to 10/10, and hearing latency in the counting scenario from ~5000ms
p50 to 467ms - the cold-start tax was being paid by more than the first
scenario, and by all sixty-odd suite runs before this.

## F58 - every rewrite of somebody's request is a chance to ask for something else

The agent said "capybara. heron." where a count belonged. The policy it was
carrying out said "say the animals out loud as I mention them"; the person had
said "count".

Three rewrites in sequence, each plausible alone:

  - the restriction split into a policy of its own, leaving the counting policy
    reading as though nobody had restricted it
  - "count" generalised to "say", which drops the operation and leaves the
    agent naming the animal instead of numbering it
  - a bare "say nothing else" emitted as a policy with nothing to qualify,
    which asks for nothing and forbids everything - and since a restricting
    policy withdraws the ordinary reply, the agent went mute through the
    sentence it was meant to correct

Stating the principle did not fix the second: told to keep the operation verb,
the pass still rewrote it away on all six readings of one phrasing. What held
was the rule underneath. "Count the animals out loud as I mention them and say
nothing else" is already an instruction to an agent and needs nothing done to
it. Rewrite only what must be rewritten - their you and me into the agent and
them - and leave the rest as they said it.

The third came from an instruction added two commits earlier that used "say
nothing else" as an example. An example of half a policy is a thing a model
will occasionally emit whole.

## F59 - the pass was shown the pieces its own utterance was made of

The utterance the standing pass reads is the whole turn joined, so the
conversation lines that make up that turn are already inside it. Both were
shown, and a repetition reads as emphasis.

Measured on the sentence that had been costing the interrupting scenario:
"Right? So let me plan this out" with "user: Right?" also among the recent
lines pinned "do not reply until they have finished planning this out" one
reading in six; without the duplicate, none in six. Reproducing it took
rendering the prompt exactly as the runtime does - the same sentence tested
with a hand-written wrapper came back none six times out of six, and that
disagreement was the whole clue.

Same de-duplication the conversation window already does for the interaction
model, arriving at the other reader of the same trajectory.

## F60 - where the suite stands, and what the session's failures had in common

50/55, eight scenarios at 5/5, none below 3/5. An earlier reading of this
finding said 48/55 and five at 5/5; the two unit fixes below took it further.

The last structural fault was two questions sharing one unit. "What did they
ask for" is read in the turn it was asked in, and a turn ends when the agent
speaks - which is right, and is what lets a policy be read whole across a
recogniser's splits. "How much has the agent covered" cannot use that unit: the
mark is taken at the moment the agent speaks, so the turn it would be compared
against begins immediately after it and the mark is never a prefix of it.

The symptom was an absence rather than a wrong value. The fact the voice is
handed about what it has already spoken for vanished from the prompt on every
reading after the first interjection, and the same fault silenced the guard
against acting on the sentence that set a policy - so the voice counted through
the instruction itself, "One." after "as I mentioned them" and "Two." four
times after "and say nothing else", before any animal existed.

Speaking does not unsay what somebody said. Both are now measured against the
person's own speech, which runs on through the agent's interjections.

Five at 5/5: asked not to be interrupted, a
recorded menu, cutting in on something wrong, an acknowledgement is not an
interruption, and - since the cold-start fix - the control.

Against the start of this work: count-as-they-go 0/5 to 4/5, a recorded menu
0/5 to 5/5, waiting out a silence 0/5 to 4/5, somebody else's conversation 0/5
to 4/5, cutting in 3/5 to 5/5, ordering from a waiter 2/5 to 4/5.

Twenty-odd distinct defects were found and fixed. Not one of them was the
interaction model deciding wrongly. Every one was something upstream corrupting
what it was asked: a recogniser translating instead of transcribing, silence
rendered as "Thank you.", a wait rendered as a result, an empty continuation
rendered as a turn, an announcement read as a rule of silence, a request
rewritten into a different request, a guard pointed at the wrong text, a server
reporting ready before it was. The model was consistently right about the
situation it was shown. The situation was wrong.

The second pattern is about the fixes rather than the faults: every
compensating mechanism added to work around a symptom later cost more than it
bought, and each dissolved once the cause was found. A blanket ban on
mid-sentence counts, a delay before the first answer, a guard keyed on
"whatever extraction last read" - all removed rather than tuned.

## F61 - the interpreting failure, reproduced and unfixed

One instruction reaches the standing pass as six committed pieces, and the
agent answers every one of them:

	user: "My colleague"                      agent: "I'm ready to interpret for you..."
	user: "only speaks"                       agent: "I'm ready to interpret..."
	user: "Mandarin."                         agent: "Your colleague"
	user: "Translate everything he says..."   agent: "My colleague only speaks Mandarin."
	                                          agent: "I will translate everything he says..."
	user: "As he goes."                       agent: "Understood. I will translate..."
	user: "And don't wait for him to finish." agent: "I will translate as he speaks..."

By the time the colleague says anything there is no window left, and the
sentence that was supposed to be interpreted goes past.

What this is not: the fact is there. Replaying the request that produced
"Understood. I will translate..." - the real one from the dump, not a
reconstruction - the prompt carries "you have already spoken once for this much
of what they are saying: My colleague only speaks Mandarin. Translate
everything he says int...", correctly. The system prompt also says, in as many
words, that a recogniser breaks a sentence wherever the speaker draws breath,
that one request often arrives as several each looking complete on its own, and
that if you have already said you would do the thing they are still describing
you should say nothing rather than agree again.

The voice answers anyway, eight times out of eight at temperature zero. It is a
rule it has been given and does not follow, which is a different kind of defect
from the twenty-odd in this document - every one of those was the situation
being wrong, and this one is the situation being right.

One attempt at it made things worse: an instruction to act rather than promise
when the thing has already happened left interpreting unchanged at 2/5 and took
the visual case from 5/5 to 1/5, where the agent stopped speaking when the
build finished. Reverted.

The structural fix is not obvious either. The guard that stops a stretch being
answered twice releases when the new text finishes a sentence, and "As he
goes." finishes one - the recogniser's punctuation says so, and this is one
place where believing it is wrong. Distinguishing that from the counting case,
where new text inside one sentence genuinely does warrant a new answer, is a
judgement about whether an occurrence has happened, which is content.

Left standing at 2/5, with the reproduction recorded, rather than fixed by
adding instructions until a number moves.

Corrected after four attempts. The voice is not failing to translate. Asked
with the colleague's speech in front of it, from the same dump, it answers
"Hello, nice to meet you." and "Hello, I'm very happy to meet you." - which is
what the check wants. What fails is that the answer arrives after the window,
because the window was spent playing six acknowledgements, and speech takes
seconds to play even when the decision behind it took fifty milliseconds.

So the defect is not the voice ignoring a rule. It is the agent taking six
turns where one was asked for, and each of those turns costing real time on the
wire. The acknowledgements are the fault; the late translation is the symptom,
and I had them the wrong way round.

Four wordings were tried against the reproduction and none moved it: pointing
the rule at the coverage fact, naming the trailing clause, generalising to
qualifying detail, and scoping the acknowledgement to once per arrangement.
Three were neutral and one made a different case worse. The first of them did
fix a separate failure - a sentence with nothing in it to count went from
"One." to <wait>, eight out of eight - and was kept for that.

The interaction model already sees that this is new since it last spoke, and
chooses to answer anyway. Whether the fix belongs there, in a bound on how
often one stretch of speech may be answered aloud, is the open question. It is
not another paragraph in the voice's prompt.

## F62 - the suite's variance is larger than the differences I was reading

Two full runs of identical code, suite65 and suite68, with nothing changed
between them but the clock:

	scenario                 suite65   suite68
	count-as-they-go               3         2
	asked not to be interrupted    5         5
	a recorded menu                5         5
	cutting in on something wrong  5         2
	ordering from a waiter         4         5
	translating as they speak      3         2
	waiting out a silence          5         4
	somebody else's conversation   5         4
	an acknowledgement             5         5
	telling them what it saw       5         3
	an ordinary question           5         5
	                          --------  --------
	                             50/55     42/55

Eight points, and one scenario swinging five to two.

This invalidates a stretch of work rather than merely qualifying it. The
setting-guard narrowing was judged on suite66 against suite65 and called a net
loss of three; its revert was judged on suite68 and looks like a loss of eight.
Neither number means what I read into it. The same is true of the instruction I
added and reverted before it: interpreting "stayed at 2/5" across runs whose
own noise is wider than that.

What the earlier findings established still holds, because those were measured
the other way: a defect reproduced from the dump, replayed at temperature zero,
fixed, and replayed again - the cold start at 9/10 to 10/10, the extraction
cases at 6/6, the recogniser transcribing Mandarin as Mandarin, the speaker
thresholds against measured cosine distributions. None of those rest on a suite
delta.

What does not hold is anything I concluded from comparing one full run against
another. F55 said a single pass cannot separate versions differing by a
scenario or two; the honest version is stronger. A single pass cannot separate
versions differing by eight points, and the per-scenario numbers move by three
and five on identical code.

The instrument needs repeats before it can rank configurations at all. Until
then, scenario-level probes at five or ten repeats against a reproduced input
are the only measurements here that carry information, and the suite total is a
direction rather than a score.

## F63 - a baseline at fifteen repeats, and what five could never have told us

140/165, 85%, and for the first time these are rates rather than draws:

	asked not to be interrupted        15/15   100%
	a recorded menu                    15/15   100%
	waiting out a silence they asked   15/15   100%
	an acknowledgement is not an       15/15   100%
	an ordinary question (control)     15/15   100%
	somebody else's conversation       14/15    93%
	cutting in on something wrong      13/15    87%
	translating as they speak          11/15    73%
	telling them what it saw           11/15    73%
	count-as-they-go                    8/15    53%
	ordering from a waiter              8/15    53%

Five scenarios pass every run. Three of those - the menu, the silence, and
somebody else's conversation at fourteen - were at nought in five when this
work started.

What five repeats could not have told us, taking counting at its measured 53%:
a five-run sample lands on 2/5 or 3/5 six times in ten, on 1/5 or 4/5 three
times in ten, and on 0/5 or 5/5 the rest. Every reading taken of that scenario
today is inside that spread, which is why the swings between them carried no
information about the changes made in between.

It cuts both ways, and the correction matters as much as the retraction.
Interpreting measures 73%, so the 2/5 readings that prompted four attempts at
it were unlucky draws rather than evidence of a deep fault - F61 overstated it.
Cutting in measures 87%, at which a 0/5 is a one-in-a-thousand event: those
readings were not noise, and the bare-restriction fix that moved it really did
move it.

So the rule is not "suite numbers mean nothing". It is that a swing of two is
noise at these rates and a swing of five is not, and telling them apart needs
the repeats. This baseline is the reference every later change is measured
against.

Measured before the guard that makes a second answer inside one stretch wait
for a pause rather than punctuation, which is therefore still unmeasured.

## F64 - a fix that is real offline and invisible in the scenario

The agree-once rule, pointed at the coverage fact rather than at a judgement of
sameness, took the sentence with nothing in it to count from "One." eight times
out of eight to <wait> eight out of eight. That is a real change in a real
request replayed from a dump.

Counting measures 9/15 with it, against 8/15 without. One run at fifteen
repeats is noise.

Both readings are correct and they are about different things. The offline case
is one turn: given this exact prompt, does the voice answer correctly. The
scenario is a conversation: the same policy fires perhaps fifteen times, the
recogniser splits differently on every run, and a turn that now answers
correctly is one of many that must all go right for the run to pass. Fixing one
of them moves the run's odds by less than the run-to-run spread.

That is not an argument against offline cases - they are the only measurement
here that reproduces a defect exactly and shows it gone. It is an argument
against expecting a scenario to move when one turn improves, and against
reading a scenario that does not move as evidence the turn did not.

What still fails in counting, from the baseline run's own transcript: the agent
says "one" 49ms after "It was a warm afternoon and I was walking along by the
river" commits, then "two" and "three" before the capybara sentence arrives at
all. The waiter fails the same way - "I will order the ribeye steak" at 14.6s,
before any dish has been named, then the correct "sea bass with fennel and new
potatoes" at 28.5s, outside the window it was needed in.

Both are the agent acting before the thing it was watching for has happened,
which is the judgement the voice makes and the runtime cannot check.

## F65 - an example inside these prompts gets imitated, not generalised

Three times now, measured:

  - the extraction pass was told a restriction belongs to its policy, with
    "count them and say nothing else" as the example. It then emitted "say
    nothing else" as a policy of its own, with nothing to qualify, and the
    agent went mute through a sentence it was meant to correct.
  - the agree-once rule was given the interpreting instruction as an example.
    It scored worse than the wording it replaced: 2/5 against 3/5.
  - the watched-for rule was given "a waiter who says there are three specials
    has named no dish". It did not help the waiter and broke a counting case
    that had been passing: 2/8 against 3/8.

The pattern is not that examples are useless - the extraction prompt is built
almost entirely from worked examples and they carry it. It is that an example
of a thing not to do, or of one specific situation, is read as a template. The
model produces the shape it was shown.

What has worked instead, every time, is naming the property rather than the
instance: whether a policy asks for a running count, whether it forbids
everything else, whether the words already read as an instruction, how much of
what they are saying has already been answered. Those are questions about the
case in front of it, and they generalise because there is nothing to copy.

## F66 - the premature-action failure resists prompt changes

Both remaining weak scenarios fail the same way, and it is now reproduced at
turn level in tools/voicecases. The agent acts before the thing it is watching
for has arrived: it counts 49ms after a sentence with no animal in it, and it
orders a dish at a waiter who has said only "tonight we have three specials".

Current state on those eight cases: 4/8. Five candidates have been measured
against it and all five rejected:

	act rather than promise when it has already happened   scenario 5/5 to 1/5
	a trailing clause belongs to the sentence before it    2/5 against 3/5
	qualifying detail is not a new request                 2/5 against 3/5
	somebody saying the thing is coming is not the thing   2/8 against 3/8
	you must be able to name it from their words alone     3/8 against 4/8

The last of these was written deliberately as a property rather than an
instance, following F65, and it still lost - so the rule from F65 is necessary
and not sufficient.

What the surviving rules have in common is that they point at a fact the
runtime supplies: how much has already been spoken for, whether this policy
asks for a count, whether it forbids everything else, how long ago it was set.
The voice can check those against the situation in front of it. "Has the thing
happened yet" has no such fact behind it - the runtime does not know what the
thing is, because knowing would mean reading the policy, which is the judgement
being delegated in the first place.

That is the honest shape of what is left. It is not a defect with a known fix
being deferred; it is a judgement the voice makes from the words alone, wrong
about a third of the time, and five attempts to improve it by instruction have
each made something else worse.

## F67 - the voice needs room to think, and it was the phase given least

The voice decides whether this is a turn to speak at all. The interaction model
only offers it the chance - a turn the interaction model withholds can never be
recovered, while a turn it offers can still be declined, so the asymmetry says
offer generously and judge carefully. Judging carefully is what the voice had
no capacity for: its effort was compiled in as minimal, which is right for a
small instruct model and wrong for a model that can think.

Measured on eight turns replayed from real runs:

	Qwen3-VL-30B-A3B-Instruct-FP8, no budget   4/8
	Gemini 3.5 Flash, no budget                4/8
	Gemini 3.5 Flash, with a budget            5/8

The two it gains are the interpreting fragments that five rewordings of the
prompt could not fix. Without a budget Gemini fails the way the small model
does, including writing "*(Listening intently, ready to count the animals as
soon as you mention them)*" - a stage direction the prompt forbids in as many
words.

End to end on the interpreting scenario at fifteen repeats:

	Qwen voice      11/15   73%
	Gemini voice    13/15   87%

That is the turn-level gain surviving into conversations, which F64 warned is
not guaranteed and did not happen for the agree-once fix.

The cost is 1781ms a turn against 30ms, and it is a deployment's to make rather
than this code's: a phone agent may want the speed, an interpreter sitting
behind a second speaker may want the judgement. Hence -fast-effort, and hence
the settings file - ninety-seven flags is not a place to record a decision like
this one.

What is left in that scenario is a new failure rather than the old one: the
voice now comments on the transcription instead of interpreting it, and once
acknowledged the instruction at length while the colleague was already talking.
Better judgement, still imperfect.

## F68 - what the thinking voice buys, and where it costs more than it buys

Three scenarios at fifteen repeats, the same code with only the voice changed:

	                          qwen    gemini   heard p50 (gemini)
	count-as-they-go          8/15     11/15      6938ms
	translating as they speak 11/15    13/15       610ms
	ordering from a waiter    8/15      8/15      5541ms

Counting and interpreting are the scenarios where the voice has to decide
whether this is a moment to speak at all, and both improve by three and two.
That is the judgement the budget buys, and it is the judgement no rewording of
the prompt reached in five attempts.

The waiter does not move, and it fails differently: with the small voice it
ordered the wrong dish, with the thinking voice it says nothing. The scenario's
own note explains why - the moment worth acting on passes if you wait for a
pause - and 5.5 seconds to be heard is longer than the moment lasts. The
judgement improved and arrived after the window it was needed in.

So the trade is not one a single setting should make for every deployment. A
policy that is answered while somebody keeps talking wants the thinking; a
policy whose moment passes in a second cannot afford it. That is why this is a
setting rather than a default, and why the settings file describes what each
phase is for rather than listing what it can be set to.

The honest summary of the model question: on this GPU, for the phase that
decides whether to speak, a reasoning model with a budget beats a larger
instruct model without one - and beats itself without one, which is the part
worth remembering. The parameter count was never the variable that mattered.

## F69 - the waiter's answer is right and two seconds late, and the reasoner is why

The failing runs with a thinking voice do not order the wrong dish. They order
the right one, after the window:

	22557ms  heard  "The third is a seedless with pheno."   (sea bass with fennel, mangled)
	25522ms  heard  "and new potatoes."
	28368ms  window closes
	30712ms  says   "I'll take the third special, please - the fish with fennel and new potatoes!"

That answer contains "fish", which the check accepts. It is two and a third
seconds too late.

The turn profiles say where the time goes, and it is not the voice's thinking:
voice turns run 161ms to 957ms. The turns that carry this answer read
reason=1308ms then voice=957ms - the reasoner is in the path before the voice
speaks, so the chain from the last committed piece to audio is the recogniser's
lag plus 2.3 seconds of model, and the window is gone.

With the small voice the same chain was about a second shorter, which is why it
sometimes landed - not because its judgement was better. Its failures were
wrong dishes; these are right dishes, late.

So the waiter is not evidence against a thinking voice. It is evidence that a
time-critical act must not wait on the reasoner, and that this scenario is the
only one in the suite where the two are in tension: everywhere else the answer
is worth more than the second it costs.

## F70 - the reasoner's effort is on the critical path, and it was set to high

F69 found the waiter ordering the right dish two seconds late, with the
reasoner in the chain between the last word heard and the first word spoken.
Lowering its effort, and changing nothing else:

	ordering from a waiter, gemini voice, slow effort high      8/15
	ordering from a waiter, gemini voice, slow effort minimal  11/15

Which completes the picture on the voice question. Against the fifteen-repeat
baseline with the local instruct voice:

	                          before    after
	count-as-they-go           8/15     11/15
	translating as they speak 11/15     13/15
	ordering from a waiter     8/15     11/15

All three scenarios that had resisted everything, improved by the same two
changes: give the phase that decides whether to speak room to think, and take
the phase that is never heard off the path where it makes people wait.

The second is the one that had hidden longest. The reasoner defaulted to high
effort on the reasoning that it is never heard and therefore free, and that is
true of its output and false of its timing: the voice defers to it on most
turns, so it sits in front of the answer. "Never heard" was read as "not on the
critical path", and those are different claims.

## F71 - the whole comparison, with the tool-call bug out of it

Both voices at fifteen repeats, everything else the same:

	scenario                        qwen   gemini
	count-as-they-go                   8       12   +4
	asked not to be interrupted       15       15
	a recorded menu                   15       14   -1
	cutting in on something wrong     13        6   -7
	ordering from a waiter             8       11   +3
	translating as they speak         11       12   +1
	waiting out a silence             15       15
	somebody else's conversation      14       14
	an acknowledgement                15       15
	telling them what it saw          11       15   +4
	an ordinary question              15       14   -1
	                                -----   ------
	                                  140      143

The menu figure needed the tool-call fix before it meant anything: with the
signature dropped it read 8/15 and errored outright, which is a measurement of
a bug rather than of a voice. Fixed, it is 14/15 and the error count is zero.

Three scenarios gain three or four. One loses seven, and it is the one that
cannot afford to think: cutting into somebody's sentence is the most
speed-critical act here, and the scenario's own note says waiting until they
finish makes the correction useless. A voice that thinks for a second and a
half cannot do it, and no amount of judgement compensates.

So the finding is not "use the reasoning model". It is that the budget buys
judgement and spends time, and this suite contains scenarios of both kinds. The
totals are three apart and the per-scenario differences are four and seven -
reporting the total alone would hide the only two facts worth having.

What that implies for the design is a per-act budget rather than a per-provider
one: the act the interaction model chose says how long there is. An interrupt
has no time; a count being carried out while somebody keeps talking has
seconds. Effort currently lives on the adapter descriptor, so this needs the
request to carry it, which is the next change rather than one measured here.

## F72 - an interrupt needs the judgement more than it needs the speed

The act-shaped budget was wrong, and measuring it said so: cutting in went
6/15 to 2/15 when an interrupt was given minimal effort.

The reasoning behind it read well. A reasoning budget gains three or four runs
on counting, the visual case and the waiter and loses seven on cutting in; the
act says how much time there is; an interrupt has none, because waiting until
they finish makes the correction useless. Every step is true except the
conclusion.

What the transcripts show is that the correction is a judgement before it is a
race. The deployment instruction says the deadline is the third of the month;
the person says they will ship by the thirteenth; the agent has to notice that
those contradict. Thirteen of fifteen runs heard "13th" correctly and only two
corrected it, so the evidence was there and the reading was not. Taking the
thinking away took the reading away, and speed bought nothing because there was
nothing to say quickly.

Two other things fell out of the same transcripts and are worth keeping.

A partial can be a different word rather than a shorter one. "Thirteenth" cut
mid-word commits as "the third", which is a valid date and the right one, so
the agent correctly declines to correct a sentence that no longer contains an
error. That happened in two runs of fifteen here and three of fifteen earlier -
not the dominant cause, but a real one, and it is not the agent being wrong.

And the earlier 6/15 was never mostly latency either. The thinking voice loses
this scenario because a correction that arrives after the sentence is useless -
but so is one that never comes, and at minimal effort it mostly never comes.

Reverted. The per-turn effort field stays: it is the right mechanism and this
was the wrong policy for it.

## F73 - the thinking voice is more reluctant to speak, which is usually right

Cutting in, with only the voice's budget changed:

	qwen (no budget)      13/15
	gemini, minimal        2/15
	gemini, low            6/15
	gemini, high           2/15

Effort is not the variable. Neither is latency: at high effort the agent was
heard 67ms after a trigger at the median, and it still said nothing at all
through the window. It declines.

That single fact explains every other result in F71. A voice with room to think
is more conservative about speaking, and this suite is mostly scenarios where
speaking was the failure: counting a sentence with no animal in it, ordering a
dish nobody named, acknowledging an instruction for the fourth time,
interpreting the transcription instead of the speech. Restraint gains three or
four runs in each. Cutting in is the one scenario that punishes restraint - it
asks the agent to talk over somebody who is mid-sentence and wrong - and the
same disposition loses seven.

Which is the asymmetry from the other side. The interaction model should offer
generously because a withheld turn cannot be recovered; the voice should judge
carefully because it can still decline. A voice that judges more carefully
declines more often, and that is the intended behaviour right up until the
moment the right answer was to speak.

So the model question has no single answer, and the honest configuration advice
is by deployment rather than by benchmark total: an agent whose job is to stay
out of the way wants the budget, and an agent whose job is to catch people
before they finish a wrong sentence does not.

## F74 - the interrupting scenario was rewarding the behaviour it exists to test against

This retracts F73's reading of cutting in, and part of F71's.

A passing run, with the local voice:

	 956ms  user   "Right?"
	4820ms  agent  "The deadline is the third of the month, not the fifth."
	6393ms  user   "and then ship it by the third."

The agent corrects a date before the person has mentioned one, invents "the
fifth" to correct it to, and passes - because the check looked for the word
"third" anywhere in a window covering the whole line. It then talks through the
rest of the sentence: "Understood...", "Confirmed...", "Got it...", which is
the behaviour every other scenario in this suite penalises.

Eleven of fifteen passing runs corrected before any date had been said. That is
where 13/15 came from. The voice that waited until it heard something wrong
scored 2/15, and it was right every time it stayed quiet.

Two runs in fifteen are worse than that. The recogniser commits "thirteenth"
mid-word as "the third", which is a valid date and the correct one, so in those
runs there is no error to catch and silence is the only right answer - scored
as a failure.

So the comparison in F71 said the local voice was better at cutting in by
seven runs, and what it measured was which model chatters more. The thinking
voice's restraint, which F73 correctly identified, was being punished for being
right.

The scenario is now split where the mistake starts: a first line with nothing
wrong in it, checked for silence, and the correction required inside the line
that contains the error. A scenario about reacting to a mistake needs a moment
before the mistake in which reacting is wrong.

The general lesson is the one this document keeps arriving at from new
directions. A check that looks for the right words in a wide window measures
vocabulary, not judgement, and a model that says more will pass it more often.

## F75 - measured against a scenario that measures it, neither voice can do this

Cutting in, before and after the scenario was split where the mistake starts:

	                      flawed check   corrected check
	qwen voice                  13/15              1/15
	gemini voice, low            6/15              1/15

The seven-run gap was the check. With a moment before the error in which
speaking is wrong, both models fail the same scenario the same amount, and the
failures are different from each other but neither is the one the old numbers
implied.

The local voice chatters through the first line - "spoke 2322ms during
0-5015ms, which should have been silent" - and then says nothing when the wrong
date arrives. It was passing by talking, and the correction it appeared to make
was a sentence it would have said anyway.

The thinking voice stays quiet through the first line, which is right, and then
speaks without correcting: "it sounds like you're mapping out a solid timeline.
what's the next step". It reaches the moment and misses the error.

So this scenario is unsolved by either model, and the honest ranking on it is a
tie at the bottom rather than a win for the model that says more. Three
readings of it are now retracted: 13/15 (chatter scored as judgement), the
seven-run gap in F71, and F73's conclusion that a thinking voice is penalised
here - it is not penalised, it simply also fails.

What this does not change is the rest of F71. Counting, the visual case, the
waiter and interpreting were measured against checks that ask for a specific
thing at a specific moment, and the thinking voice gains three or four runs in
each. Those scenarios were never vulnerable to this, because saying more does
not produce the word they look for.

## F76

An output-token ceiling is spent on thinking before it is spent on speech, so a
number chosen to keep a spoken turn short silences a voice that reasons.
Measured against the live Gemini endpoint on the interrupting turn, varying
only the ceiling:

    maxOutputTokens=96   MAX_TOKENS   93 thought    0 spoken  ''
    maxOutputTokens=128  MAX_TOKENS  118 thought    6 spoken  'Wait, the thirteenth? The'
    maxOutputTokens=512  STOP        281 thought   20 spoken  "Wait, actually, the client's deadline is..."
    no ceiling, LOW      STOP        367 thought   18 spoken  "Actually, the client's deadline is the third..."

Two conclusions. The ceiling was never what kept turns short: uncapped, the
same turns answer in eighteen to thirty-two tokens, because the instruction
asks for something said out loud. And every uncapped setting corrects the date,
including a thinking budget of zero, so the content failure recorded in F75 was
this - the reply that "reaches the moment and misses the error" was a truncated
one, not a wrong one.

The two quantities are separate and only one belongs in a limit. Thinking gets
a budget; speech gets an instruction. Gemini rejects a level and a budget
together - "you can only set only one of thinking budget and thinking level" -
so an effort is one field holding either.

The budget is a target rather than a cap. Asking for 128 produced 258 to 300
thought tokens; asking for 512 produced 281 to 311. Latency follows whether it
thinks at all rather than how much:

    MINIMAL / budget 0    first word 0.5-0.8s     0 thought
    budget 128            first word 1.6-2.1s   258-300 thought
    budget 512            first word 1.5-1.9s   281-311 thought
    level LOW             first word 2.2s       369-393 thought

So thinking costs about 1.2s a turn, and buying more of it past the first
budget costs almost nothing.

## F77

The narrator starves the interaction model when they share a server. With the
video narrator moved onto the same local vLLM that serves the interaction model
- everything else unchanged - interaction decisions fell from 1.66/s to 0.25/s,
and their median latency stopped being measurable at all.

The effect on behaviour is total rather than gradual. Across 1310 logged rows
the situation never once said anybody else was speaking: every decision landed
in a gap, because the decisions that would have landed during speech never ran.
`interrupt` and `speak-through` stayed on the menu of available acts while the
world they described had nobody to interrupt, and the model correctly never
chose them. Cutting in, the waiter, and somebody else's conversation - the
three scenarios that turn on acting mid-speech - scored 0/5, 1/5 and 0/5.

The interaction model was not deciding wrongly. It was being asked about a
world that had already moved on, which is the same shape as every other defect
in this file. Image requests are heavy and text decisions are 17ms, so sharing
the GPU does not slow the decisions down evenly - it stops them happening.

## F78

Four defects stood between the interaction model choosing to interrupt and
anything being heard. All four were upstream of the choice, and the interaction
model was right every time - it offered the interrupt 153 times in one episode.

**A policy nobody set.** The extractor read "Right? So... Let me plan this out"
as "pin turn do not reply until they have finished planning this out". Policies
outrank the deployment's instructions by design, so an agent told to correct a
wrong date mid-sentence was told by nobody to stay quiet instead. The prompt's
prose already forbade this and named the sentence; the worked examples showed
the opposite, and the examples won.

**Being spoken over read as being refuted.** A composed reply was discarded when
the turn was overtaken while the provider was writing it. For an answer that is
right. For the three acts that mean "do this while they are still talking" the
condition tested is the premise, so it holds permanently and the veto never
loses. With a voice that thinks, a third of everything composed died here,
against six per cent for an instant voice - the rule converts latency into
silence.

**A turn that said nothing counted as having spoken.** The mark that tells the
next turn "you have already spoken for this much of what they are saying" was
set in two places that could not know. The interjection path set it before the
model was asked; the publish path set it whenever publishing returned no error,
and withholding is not an error - it succeeds at not speaking. <wait> is text,
so the emptiness test beside it did not catch it either. Twenty voice turns were
told they had already covered the sentence containing the wrong date.

**The narrator starving the decision loop**, F77, which made the situation say
nobody was speaking at all.

With all four fixed the voice produces the correction. Captured from the live
request dump, the fast phase answered "The third." - and the scenario still
fails, for a fifth reason that is structural rather than a defect:

    interrupt offered by the interaction model : 153
    voice calls actually made                  :  22
    fast calls reaching the model per repeat   :   3

The three that reach the model arrive at finalized-utterance boundaries. The
voice is not shown "by the 13th" until the sentence containing it has ended, at
15150ms, and the moment worth interrupting closes at 11902ms. So the correction
is right, and late, and the reason it is late is that the admission path between
the act and the model is coarse where the act is fine-grained.

Counting, which turns on the same machinery without the timing, went 2/5 to 5/5
across these fixes.

## F79

Interrupting is late by construction, and the reason is an ordering rather than
a judgement.

Choosing `interrupt` produces `EndpointDecision{Ended: true, Projected: true}`.
The floor then forces the acoustic stop, the recogniser finalises the fragment
it was cut off in, an ordinary turn is built from that, and the voice answers.
Every one of those steps happens after the moment worth interrupting at, so the
words cannot reach the world until the moment has passed. Measured on the
interrupting scenario, with the four defects in F78 fixed: the interaction model
chose interrupt 153 times, the voice was shown the sentence only once the
recogniser had committed it whole, and the correction it composed - "The third.",
captured from the live request dump, exactly what the scenario asks for -
was written at 15150ms for a window that closed at 11902ms.

Taking the floor is not the error. It is what separates interrupting from
speaking through, and the act vocabulary means it: four tests in actfloor assert
it and they are right to. The error is that taking the floor is a precondition
for speaking rather than a consequence of it. When a person cuts in, the words
and the floor happen together - the other speaker trails off because you spoke,
not before it.

So the fix is to start the speech on the interjection path at the moment the act
is chosen, and let the endpoint close the turn alongside it rather than ahead of
it. The already-spoken mark is what stops the closing turn answering the same
stretch twice, which is the job it exists for.

Not implemented here. The generalisation this needs - interjectFor, which takes
the reason as a parameter so the voice knows it is correcting rather than
carrying out a standing policy - is being written in this tree by another
session and is not yet committed. Building a second copy of it would collide
with that work rather than add to it.

## F80

Qwen3-VL-30B-A3B-Instruct-FP8 against Qwen3-VL-8B-Instruct as the interaction
model, both served locally, measured on the current prompts at three repeats:

                          30B-A3B      8B
    interaction passed     72/96      78/96
      acting               39/45      30/45
      restraint            33/51      48/51
    standing-instruction   78/78      75/78
    slowest decision       112ms       62ms

The 8B wins the aggregate and loses the number that decides it.

Acting and restraint are not worth the same, and the whole shape of this system
says so. The interaction model offers the voice a chance to speak; the voice
still decides whether to take it. A turn offered wrongly costs a model call the
voice can decline. A turn withheld wrongly cannot be recovered by anything
downstream, because nothing downstream runs. So a point of acting is worth more
than a point of restraint, and an aggregate that adds them ranks these two
models backwards.

Read that way the gap is wide. The 30B misses 6 of 45 moments; the 8B misses
15, two and a half times as many, and every one of them is a moment no later
layer can recover. The 8B's profile - cautious, restraint 48/51 - is precisely
the failure this arrangement is built to avoid.

The standing-instruction pass points the same way, and it matters more than its
size suggests: it produces the policies that outrank the deployment's own
instructions, so an error there governs everything afterwards. The 30B takes it
perfectly. The 8B fails the silence-scope case 3/3 - the same case fixed today
for the 30B, which shows the fix rests on capability the 8B does not have
rather than on wording alone.

Latency does not decide it. Both are far inside the budget for a decision taken
many times a second, and the 50ms is bought back many times over by not having
to recover a moment that was never offered.

So the 30B-A3B, on the criterion that the offer is generous and the judgement is
the voice's. Note that it is the lower aggregate score, deliberately: the
aggregate is the wrong instrument here, not a tiebreaker to be overridden when
inconvenient.

The continuer decision settles it without needing the argument. Measured on the
same models over the backchannel cases:

                          30B-A3B      8B
    backchannel passed     36/39      27/39
      acting                9/9        0/9
      restraint            27/30      27/30

The 8B never says a continuer at all. Not rarely - never, across nine
situations where a listener plainly owes one, three repeats each. The two models
have identical restraint here and the whole difference is that one of them
participates. A policy that always answers none is not a cautious policy, it is
an absent one, and it would be indistinguishable from switching the feature off.

This is the same profile as its 30/45 acting on the interaction cases, seen
without the ambiguity: the 8B is not trading accuracy for caution, it is
declining to act.

## F81

F79's fix, measured. Cutting in now speaks and takes the floor at the same time
rather than one after the other, and the scenarios that turn on a moment
passing all moved together:

                                    qwen   before  after
    cutting in on something wrong    2/5     0/5    5/5
    ordering from a waiter           5/5     2/5    5/5
    a recorded menu                  5/5     4/5    5/5
    count-as-they-go                 2/5     5/5    5/5
    somebody else's conversation     4/5     3/5    5/5
    translating as they speak        2/5     1/5    4/5
    asked not to be interrupted      5/5     5/5    5/5
    suite total                     42/55   40/55  49/55

The waiter moved for the same reason as cutting in, which is the confirmation
that matters more than either number: its moment also passes if you wait for a
pause, and it was the scenario a thinking voice had been losing worst. One cause,
two scenarios, neither of them touched directly.

Asked-not-to-be-interrupted does not move, and that is the check on the change
rather than a null result. It is the scenario most at risk from making
interruptions cheaper, and what refuses those is the interaction model declining
to choose the act - which this does not touch. Making interrupting work did not
make it happen more.

Two cells fell in the same run - the visual case to 2/5 and waiting out a
silence to 3/5 - and both came back 5/5 when re-run against the identical
binary. That is evidence these cells are noisy at five repeats, and it is not
evidence the suite is really 54/55: re-running a failing cell until it passes is
how a number is talked into existence. The total stands at 49/55 until a second
independent run says otherwise.

Neither scenario has a path to the change. Both have nobody speaking, so there
is no floor to take and the interrupt branch is never reached.

A second full run, same binary and services, says 47/55. Both runs together:

                                    run 1  run 2   before
    asked not to be interrupted      5/5    5/5     5/5
    a recorded menu                  5/5    5/5     4/5
    cutting in on something wrong    5/5    5/5     0/5
    ordering from a waiter           5/5    5/5     2/5
    somebody else's conversation     5/5    5/5     3/5
    an acknowledgement                5/5    5/5     5/5
    an ordinary question             5/5    5/5     5/5
    count-as-they-go                 5/5    3/5     5/5
    translating as they speak        4/5    2/5     1/5
    waiting out a silence            3/5    4/5     5/5
    telling them what it saw         2/5    3/5     5/5
    total                           49/55  47/55   40/55

The three scenarios the change was aimed at hold at five out of five in both
runs, from 0/5, 2/5 and 4/5. That is the result, and it is the part that
reproduces.

The rest of the table is the honest caveat. Four scenarios move by two or three
runs between two runs of the same binary, so at five repeats they measure the
day as much as the code, and no claim about them - in either direction - is
supported by these numbers. The 2/5 on the visual case in run 1 and the 3/5 in
run 2 are the same cell that returned 5/5 when run on its own; that spread is
the measurement, not a trend.

What the totals support is the difference between 47-49 and 40 on identical
configuration, which is larger than the spread of either. What they do not
support is a ranking of the four noisy scenarios, which needs more repeats than
a scenario costing four minutes a run affords.

## F82

The noisy scenario cells in F81 are not only noise. The recogniser was failing.

    transcription failed: CUDA failed with error out of memory

repeated through the run, and the binding surfaces that as a session error which
scores the whole run as failed. So a cell reading 2/5 can be one model behaving
differently and three transcriptions that never happened, and nothing in the
scenario output distinguishes them - the failure arrives as "the session
reported a failure", which reads like the system rather than the machine.

The GPU was at 97205 MiB of 97887, 682 free. Two vLLM servers (49.7GB and
23.8GB), speech synthesis at 15.4GB, and three small services. A recogniser
needing a few hundred megabytes for a forward pass gets them or does not,
depending on what else is mid-request, which is exactly the shape of an
intermittent failure that looks like variance.

Some of it was mine. I had started a second copy of the recogniser to isolate
my measurements from a shared one that was failing - for this same reason, which
I had not diagnosed yet - and the duplicate was 2.7GB of the pressure that made
it fail. Stopping it and going back to the shared instance freed 2.7GB and
removed the duplication.

So F81's phrasing needs correcting: "these cells are noisy at five repeats"
attributes to sampling something that is at least partly a machine running out
of memory. The three scenarios the interrupt change was aimed at held at 5/5 in
both runs and are unaffected by this - a cell that fails this way fails, it does
not pass twice by luck. But no reading of the four moving cells is worth
anything until they are measured with headroom.

The general form is the one this file keeps arriving at from a different
direction: infrastructure failing quietly upstream of the thing being measured,
and the measurement reading as behaviour. It was the recogniser translating
instead of transcribing, silence transcribed as "Thank you.", the narrator
starving the decision loop, a health check answering before the models were
warm. This is the same defect wearing the machine's clothes rather than the
model's.

## F83

Interpreting, measured at fifteen repeats with the recogniser no longer running
out of memory, is 6/15. It had been reading anywhere from 1/5 to 5/5, and the
spread was the sampling; the level is the code. Two defects, both upstream of
any judgement, take it to 9/15.

**A third party's speech had no name on it.** One microphone carries everybody,
and somebody else in the room arrives with the same authority as the user, so
bare text tells the voice the user said it. Asked to interpret for a colleague,
the voice reads the colleague's Mandarin as the user's own words and cannot tell
that the thing it was asked to translate has arrived. The interaction model was
never confused about this - its situation names the speaker - so the fact
existed and was dropped on the way to the layer that had to act on it. 6/15 to
7/15.

**Stability was computed in the wrong unit.** The settled prefix of a partial
came from `strings.Fields`, and Chinese, Japanese and Thai are written without
spaces, so the whole utterance is one token that agrees only when nothing
changed. Stable is therefore empty on every partial of every sentence in those
scripts, and the interjection path falls back to the whole revision when stable
is empty - acting on exactly the unsettled tail the mechanism exists to hold
back. The mechanism did not fail loudly; it silently turned off, and the
fallback undid it. 7/15 to 9/15.

The cost is script-specific, which is why it never showed up in English. A
truncation mid-syllable in Chinese usually lands on another real word:

    said     你好，很高兴见到你      "hello, very pleased to meet you"
    partial  ...很高|              cut mid-syllable
    heard    高雄                  Kaohsiung, the city
    spoken   "hello, nice to meet you in kaohsiung"

The same truncation in English gives "thir-", which is nothing, and no damage
appears. Synthesis and recognition round-trip cleanly end to end - 你好，很高兴
见到你 comes back verbatim - so nothing that tested whole utterances could see
this. It lives entirely in the partials.

Both fixes are general rather than about Mandarin. The first is about any voice
that is not the user's, the second about any script without word spacing.

## F84

A fix that measured worse, kept here because the reasoning was sound and the
result was not.

The recogniser re-detects the language on every partial, and a fragment is not
enough evidence for that. Measured, one interpreting run produced user lines in
six languages nobody was speaking - "それでは、ご視聴ありがとうございました",
"감사합니다", "Und Gott", "O meu...", "C'est pas un conçu", "Amen" - each a phrase
common in that language's training data, each entering the conversation as
something the user said. Not a mistranscription but an invention, and worse than
a missed word because nothing downstream can tell.

The obvious reading is that nobody changes language mid-utterance and a listener
does not re-decide what they are hearing every three hundred milliseconds. So
the first hypothesis of an utterance detects, and the rest are told what it
found.

Interpreting went 9/15 to 6/15.

The reason is in a measurement already taken and not read carefully enough. The
first 200ms of the Mandarin line detects as English - there is not enough of it
to tell yet, which is the same fact the fix was built on. Holding the first
answer therefore holds the least informed one, and locks the whole utterance
into it. Re-detecting is wrong because it decides too often on too little;
holding the first is wrong because it decides once on even less.

What would follow the principle is holding the answer taken from the most
evidence rather than the earliest - detection deferred until the buffer is long
enough to support it, and held from there. That is a different change and is not
made here.

Reverted. The two fixes in F83 stand at 9/15 on their own.
