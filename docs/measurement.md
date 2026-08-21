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
ships a runner for each rather than a copy. The environments own the domains,
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

DynaCU-Bench doubles as a functional release gate — that video observation and
action grounding work end to end — which is a narrower question than the suite
answers and gets a yes or no in seconds.

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

**`immediate` remains the default.** It is the safer failure, and the
alternative is a documented configuration with a number attached rather than a
recommendation. Which one is right depends on whether a deployment's users
backchannel, which is what the full cells will say.
