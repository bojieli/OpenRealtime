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
| DynaCU-Bench | 100 dynamic + 50 static | video observation, action grounding. F1, F3, F7 |

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

A subset of it is also the functional release gate — that video observation and
action grounding work end to end — which is a narrower question than the suite
answers: `openrealtime bench dynacu -category S_static -limit 5` is minutes
rather than hours and is reported incomplete, because it is.

## Reporting rules

- **No incomplete cell is reported.** A partially executed cell is not a
  smaller result, it is a different one.
- **Every cell declares its source revision and executable hash.** A number
  that cannot be traced to a build is a number that will eventually be wrong.
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

### What still cannot be built, and why

An agent that goes quiet while it reasons ought to say so. The reasoning half
is silent by construction, the gap is a property of the question, and a caller
cannot distinguish a hard question from a broken agent - an independent listener
called one repaired recording "broken" for exactly this, on a call where
everything else had gone right.

The turn that would fill it has been written twice and withdrawn twice, and it
is worth recording why rather than leaving it to be rediscovered.

It has to run *while* the reasoner runs, which means arriving as a parallel
branch. Two things stood in the way. The deferred set was merged flat before it
ran, and a merged batch carries one triage, so a parallel branch inherited the
deferral of whatever routine traffic sat beside it - that half is fixed. The
half that remains is that the loop is driven by one goroutine: while a batch is
being processed, the driver is inside that call and cannot pick up the branch,
so it is planned correctly and runs when the work it was reporting on has
already finished. Saying "still checking" as the answer arrives is worse than
saying nothing.

Making that concurrent means a second driver through the machinery that
guarantees safe points and commit ordering. Getting it subtly wrong reorders the
log, which is a worse failure than dead air, so the feature waits for that work
rather than shipping ahead of it.

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
