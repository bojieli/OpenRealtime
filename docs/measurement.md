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
| F1 | Binding | cascade · omni-Qwen3 · omni-MiniCPM-o · duplex-Moshi · upstream | `-binding`, `-sidecar` |
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

The suite had five scenarios and covered the demos and the phone menu. It had
nothing for ordering from a waiter or for simultaneous speech, which are the
two cases that came from somebody actually using this. With those and a
backchannel case added, eight scenarios and twenty-one checks:

| scenario | passes | source |
| --- | --- | --- |
| a recorded menu | 2/2 | production: IVR navigation |
| an acknowledgement is not an interruption | 2/2 | overlap |
| an ordinary question | 2/2 | control |
| translating as they speak | 2/2 | production: simultaneous speech |
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
