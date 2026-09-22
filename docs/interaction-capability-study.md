# Interaction capability: timed representations and incremental speech

**Status:** proposed research design, 2026-09-22. No experiments in this study
have been executed. Repository inspected at
`293e4f74b43b3116e2e2e2786e379381dde3e77f`.

**Companion:** [implementation and GPU execution plan](interaction-capability-plan.md).
This study builds on the existing [streaming integration plan](full-duplex-streaming-plan.md)
and [architecture experiments](architecture-experiments.md). It adds research
questions and controlled treatments; it does not replace those contracts or
promote earlier smoke measurements into capability results.

## Research objective

Determine how far an agent can go using a sparse, time-aligned representation
of conversation, and identify which behaviors require richer acoustic evidence
or tighter coupling between language and speech generation.

The hypothesis is not that cascades must win, or that native audio must win.
We want to distinguish limitations of information, learned policy, expression,
and execution. A poorly timed recognizer is not evidence that timed text is
inherently insufficient. A prompted controller is not evidence about the ceiling
of a trained micro-turn language model. A pleasant voice is not evidence of
successful interaction.

The intended outcome is a capability map with paired recordings and causal
diagnostics, rather than a single model leaderboard. Finite experiments can
expose a bottleneck or demonstrate a capability; they cannot prove a universal
architecture ceiling.

## Conceptual model

Let `H(t)` be the audio and interaction history available up to time `t`, `Z(t)`
its compressed representation, and `A(t)` the next interaction action, including
content and delivery where relevant. Compression preserves task capability if
`P(A(t) | H(t)) = P(A(t) | Z(t))` for the target task distribution. This is a
sufficiency condition, not an assumption that transcripts already satisfy it.

Two distinctions are essential:

1. **Interaction schedule:** complete turns versus repeated opportunities to
   perceive, remain silent, speak, or revise.
2. **Information representation:** text, timed text, acoustic annotations,
   learned acoustic features, or native audio representations.

Micro-turns and native audio are compatible. The primary-source
[Thinking Machines description](https://thinkingmachines.ai/blog/interaction-models/)
itself uses time-aligned micro-turns. Therefore this study must not attribute
all benefits of continuous interaction to audio-native representations.

We also distinguish three output horizons:

- **Planning horizon:** how much future content has been formulated.
- **Synthesis lookahead:** how much future text can condition the current sound.
- **Committed output:** speech already heard, which cannot be revised.

A long tentative plan need not become a long irrevocable utterance. Model-time
audio/text offsets are not automatically wall-clock waiting times.

## Hypotheses and evidence that would challenge them

| Hypothesis | Supporting observation | Observation that challenges it |
| --- | --- | --- |
| H1: timing and continuous input explain substantial interaction gains | A timed-text condition beats a whole-turn condition with matched models on online correction and silence-dependent tasks | Little gain after the timed policy has been trained and its execution validated |
| H2: some failures are information bottlenecks | Identical words with different prosody require different actions; timed text fails while timely acoustic evidence helps | Acoustic additions provide no gain on sufficiently varied matched pairs |
| H3: feedback-sensitive planning matters more than merely shortening text context for conversational delivery | Incremental planning improves adaptive response and human-rated interaction at matched synthesis settings | Improvements follow synthesis context alone, or incremental planning harms fluency without helping adaptation |
| H4: learned micro-turn control exceeds a prompted stop/continue controller | The trained model incorporates new evidence into ongoing content on held-out scenarios | Benefits vanish outside training templates or are entirely explained by a stronger backbone |

Failure to reject a difference is not proof of equivalence. If reporting
noninferiority, preregister the task-specific margin and justify sample size
from pilot variance before collecting the held-out evaluation.

## Study A: information and temporal representation

Use the same backbone, voice, synthesis model, task instructions, and output
action vocabulary wherever technically possible. Change one factor at a time.

| Cell | Policy input and schedule | Purpose |
| --- | --- | --- |
| A0 | Whole-turn transcript, conventional endpoint-triggered answering | Conventional behavior baseline |
| A1 | Incremental transcript with its clock/empty intervals removed | Ablation of explicit temporal evidence; evaluator retains true timing |
| A2 | Timed text, observed silence, own heard speech, tentative plan | Sparse temporal representation |
| A3 | A2 plus causally available acoustic cues | Test information lost by transcription |
| A4 | Audio-native reference through its supported interface | External capability reference, not a matched architecture ablation |

A0 changes both schedule and evidence relative to A2; A1/A2 is the narrower
timing comparison. Every cell records the selected input channels. No acoustic
activity predicate may secretly decide actions in a text-only condition.
If a treatment includes a fallback, name and measure it separately.

Run two input regimes:

- **Annotated diagnostic:** manually aligned words and cues released only when
  that evidence is causally observable. No full future transcript, future intent
  label, or full-utterance emotion classification is available early. Include a
  fixed evidence-delay sweep. This isolates representation from recognition.
- **Real perception:** actual streaming ASR and, for A3, actual cue extraction.
  Record source time and availability time separately, including revisions.

Annotated diagnostic results are optimistic references, not deployable scores.
Estimated word timestamps remain marked estimated. The encoder must distinguish
observed silence, sound without decoded words, absence of a new transcript,
and missing input.

Start with a prompted, bounded micro-turn policy shared across A1–A3. Then repeat
key contrasts with a trained policy. Match adaptation data and compute when
claiming a representation effect; testing one trained condition against an
untrained condition does not establish that effect.

## Study B: planning versus synthesis

Use two stages so that changes in wording do not masquerade as TTS effects.

**B1: synthesis-only diagnostic.** Keep text, voice, model, punctuation, and
available style controls fixed. Compare supported model lookahead settings, if
the checkpoint actually supports them. Separately compare incremental text
delivery schedules. Preserve one synthesis context and record flush events.

**B2: interactive factorial.** Compare:

- Full-response planning with a fixed continuation.
- Full-response planning with an explicitly revisable continuation.
- Incremental planning, reconsidered after each admitted observation.

Cross those with two or three supported synthesis-context settings. If the TTS
has fixed lookahead, use text-delivery conditions instead and label the result
as delivery/commitment, not model-lookahead, research. Do not invent a full-sentence
lookahead condition by merely sending a full sentence to a locally causal model.

The existing Kyutai wrapper reads a two-word lookahead from its model state
machine and has a delayed audio stream. It currently exposes no validated
runtime sweep of model lookahead. Fish and Kyutai are model substitutions, not
two levels of a clean lookahead ablation.

Report audible latency and quality jointly. Add a matched onset-delay listening
condition where feasible to separate preference for faster responses from
preference for speech style. Never shift the entire input/output recording to
hide missed interaction windows.

Preregister separate listener questions for intelligibility, pleasantness,
conversational naturalness, and responsiveness to the other speaker. Extra
hesitations, random pauses, and self-corrections are not automatically desirable.

Relevant mechanisms, not outcome guarantees:
[incremental planning in conversational systems](https://www.sciencedirect.com/science/article/pii/S0885230812000411),
[lookahead in incremental TTS](https://www.isca-archive.org/interspeech_2020/stephenson20_interspeech.html),
and [pseudo-lookahead](https://arxiv.org/abs/2012.12612).

## Study C: continual content adaptation

The present [sidecar](../sidecars/microturn_sidecar.py) is an orchestrated control
question followed by a separate streamed answer. Preserve that implementation
as a baseline. Add an experimental path where each model update jointly chooses
an action and a bounded content continuation using the ongoing history.

Required actions are wait, speak/continue, backchannel, yield, and revise.
Optional delivery intent describes emphasis, uncertainty, or a trailing phrase;
if the synthesizer cannot consume it, record it as unsupported. Never render
internal act tokens as spoken words.

The protocol separates heard text, current partial speech, and pending content.
It must not label generated words as heard. It can preserve internal inference
state or reconstruct an equivalent causal history; KV persistence alone is not
the definition of the research treatment. No hidden stop/continue controller
may override the joint policy in the clean comparison.

For a released trained model, use its native grammar and training-time schedule.
For a new model, define and version the schema, then train and evaluate it. A
prompted schema-compliant model is useful for debugging and is reported as such.
The absent/gated DuplexCascade checkpoint remains unavailable, never silently
replaced and scored under its name.

## Task families and matched pairs

Reuse the scenario recording and playback machinery, extending the existing
twelve scenarios with held-out material:

| Family | Matched variation | Required evidence of success |
| --- | --- | --- |
| Semantic correction during speech | “Kyoto, not Tokyo” versus neutral acknowledgement | Subsequent content reflects Kyoto, without assuming unheard Tokyo details were communicated |
| Mid-explanation steering | “I know that part” versus “Explain that part” | Different, appropriate continuations after the same opening |
| Prosodic acknowledgement | Same words, affirmative versus questioning intonation | Continue versus clarification when context and prosody jointly support it |
| Silence and hesitation | Same transcript, different pause placement/duration | Appropriate waiting or response; no credit for always staying silent |
| Addressing and overlap | Assistant-directed correction versus speech to another person | Adapt only to the relevant utterance |
| Proactive semantic action | Correct an error as heard versus an explicitly permitted fictional statement | Timely, content-correct intervention with low false-intervention rate |
| Concurrent task | Count or translate while the user continues | Correct incremental content at the required times |
| Output revision | New constraint arrives before/after a phrase has played | Revise pending words; explicitly repair already-spoken mistakes |

Some task labels require context and cannot be inferred from prosody alone.
Collect independent human label agreement; retain ambiguity instead of forcing
a binary ground truth. Use real consented recordings for prosody-dependent tests;
synthetic audio is a controlled diagnostic, not sufficient validation.

## Evaluation and artifacts

Pilot: 24 matched pairs across the eight families, three repeats per cell.
Freeze scoring and prompts afterwards. Initial held-out study: 80 new pairs
(10 per family), three repeats; each pair has two conditions. Thus one cell
has 480 executions, not 240. These are proposed budgets, not a power claim.
Keep semantic templates, speakers, and recordings disjoint from adaptation data.

Primary measures:

- Correct changed content within the scenario's preregistered time window.
- False intervention and missed intervention rates, reported separately.
- Cue-to-appropriate-audible-response delay; acknowledgement is not task completion.
- Heard-history consistency and successful continuation through backchannels.
- Blind human ratings of paired full interactions, including the user channel.

Report per-family outcomes, timeouts, failures, sample counts, and paired
confidence intervals. Cluster resampling by scenario pair, not by repeated run
or audio frame. Randomize cell order within blocks to reduce shared-host and
provider drift. For human evaluation, randomize presentation order, conceal
system labels, and report rater counts and disagreement.

Each run retains exact source/checkpoint/configuration identity; input audio and
causal evidence schedule; exact model-visible input; actions and text deltas;
playback-aligned output; and scorer decisions. Private reasoning traces are not
needed. Report real-time deadline failures rather than concealing them with
slower-than-real-time execution. Slowed diagnostic runs are a separate condition.

## Interpretation rules

- A2 success shows a capability is achievable with that sparse representation.
- A3 improvement suggests useful evidence missing from A2; it does not establish
  that a monolithic audio model is necessary.
- Annotated success plus real-ASR failure points toward perception/availability.
- Trained-policy gains over prompted control point toward policy learning.
- A native model advantage is a system-level observation unless training,
  evidence, capacity, and timing are controlled.
- Better incremental-planning ratings do not imply shorter TTS context is always
  better. A full plan that stays revisable is an important competing explanation.

The first milestone is a set of paired recordings demonstrating genuine
mid-speech content adaptation, with sufficient traces to explain successes and
failures. Only then expand to training and broader native-model comparisons.
