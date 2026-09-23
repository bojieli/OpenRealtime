# Draft listening rubric for the interaction capability pilot

Status: protocol development; no human ratings collected. This rubric implements
the evaluation requirements in the study design and must be piloted for agreement
before use as a validated semantic scorer. It does not replace the 24-pair,
three-repeat pilot or authorize an assistant transcript review to count as human
listening.

## Presentation and retained evidence

Present both complete interactions in a pair, with room/user audio and assistant
audio audible together. Include an optional replay of the original response
window. Use opaque item IDs, conceal model/cell/run names, and randomize pair and
within-pair presentation order with a retained seed and private mapping. Preserve
playback speed, silence, interruption, failures and truncation. Do not normalize
time by deleting pauses or extend response windows after seeing outputs.

Provide the task facts needed to judge correctness, the authored response-window
start/end, and the same neutral instructions for every system. Do not show model
text, ASR hypotheses, action labels, previous scores, or hypothesized outcomes
before the listener has submitted an independent judgment. Model-generated text
is not evidence that content was heard. Record whether the listener actually
heard both full interactions and which intervals they replayed.

Use at least two independent listeners for the agreement pilot; report actual
rater counts per item. Keep original judgments when resolving disagreement.
Adjudication is a separate record, with a reason and provenance. Do not force
ambiguous acoustic intent into a binary label. Do not call this convenience
sample a power calculation or a representative population estimate.

## Common judgment record

Record the following for each branch before comparing the pair:

- Execution: complete recording, failed execution, or incomplete evidence. Record
  missing channel, corrupt audio, truncation, or connection failure explicitly.
  Intentional silence in a complete run is distinct from failed collection.
- Opportunity: for mid-speech tasks, was substantive assistant speech underway
  when feedback began? Mark yes, no, or uncertain, with an audible interval.
  A prior greeting alone is not substantive initial task execution. No opportunity
  means the requested capability was not demonstrated, not a scored success.
- Content: fulfilled, partly fulfilled, incorrect, absent, or uncertain within
  the original response window. Cite the audible words and interval supporting
  the judgment. Acknowledgement alone cannot fulfill a content-change request.
- Timing: first appropriate content onset and completion, when identifiable.
  Mark timestamps uncertain rather than infer them from generated-text times.
- Heard-history consistency: consistent, inconsistent, not applicable, or
  uncertain. Identify any claim that unheard material was already communicated.
- Interaction quality: separate 1–5 ratings for comprehensibility, continuity,
  and appropriateness of interruption, each with a brief reason. Anchors are
  1 unusable, 2 major disruption, 3 usable with noticeable problems, 4 minor
  problems, 5 no observed problem. These are draft scales requiring agreement
  checks, not established psychometric measures.

For the pair, judge whether the branches show the required opposite or neutral
behavior. Review feedback-removal controls separately with the original timing
anchor. A control producing the same arbitrary change defeats the discrimination
claim; a silent control does not rescue a failed treatment branch. Pair success
requires both branch requirements and the relevant opportunity to be demonstrated.
Execution failures stay in the attempted-run denominator and are reported
separately from semantic errors. Report uncertain cases and sensitivity to their
treatment; never silently discard them.

## Family-specific content criteria

| Family | Required judgment | Common false positive to reject |
| --- | --- | --- |
| Semantic correction | Subsequent audible task content incorporates the corrected fact; neutral branch remains appropriate to the original fact | Merely naming Kyoto, saying sorry, or producing correct content only after the window |
| Steering | Skip branch moves beyond the known part and fulfills the next task; deepen branch substantively explains the requested part | Both branches restarting the same introduction or acknowledging without explanation |
| Prosodic acknowledgement | Context and independently validated acoustic intent support continuing versus clarification | Counting supplied cue labels as successful perception; forcing ambiguous intonation |
| Silence/hesitation | Appropriate waiting at an unfinished phrase plus eventual fulfillment after completion | Always staying silent, or speaking only after collection ends |
| Addressing/overlap | Update the task for assistant-directed feedback and avoid acting on speech directed elsewhere | Treating the same words as an instruction regardless of addressee |
| Proactive action | Timely, correct intervention on a real error, with restraint for explicitly permitted fiction | Keyword-triggered interruption or an incorrect correction |
| Concurrent task | Correct incremental sequence at the authored times while input continues | Correct final total or translation with no required incremental behavior |
| Output revision | Change unspoken material or explicitly repair already-heard content, according to actual playback | Using authored early/late timestamps as proof of what the listener heard |

Prosody and addressing intent require independent input-label validation and
appropriate recordings. The present synthetic diagnostics do not satisfy that
requirement. Revision judgments require playback evidence for the relevant phrase;
a sample receipt without word alignment may leave the classification uncertain.

## Reporting

Retain one row per listener, item and branch, plus paired judgment and control
judgment, timestamps, replay record, uncertainty and rationale. Keep system
identity in the private mapping until judgments are locked. Report per-family
attempts, execution failures, missing opportunities, fulfilled/partial/incorrect
content, missed interventions and false interventions, with rater disagreement.
Compute uncertainty by resampling scenario pairs with all their conditions,
repeats and ratings together; do not treat repeated executions as independent
pairs. No numerical confidence interval is justified by the current development
coverage alone.
