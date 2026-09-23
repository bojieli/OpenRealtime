# Interaction capability: development findings

Status: protocol development, 2026-09-23. This is not the preregistered pilot.
No capability claim is established. Companion to the
[plan](../interaction-capability-plan.md) and [design](../interaction-capability-study.md).

## Evidence status

The first development round (2026-09-22/23) ran on a checkout that was deleted
on 2026-09-23 to reclaim disk space. Its run bundles, traces and recordings are
gone, and so is the original implementation. The study code has been rebuilt on
a fresh clone (`bench/capability`, `tools/interactionstudy`). Every finding
below is therefore **provisional** until the rebuilt pipeline reproduces it.
Each section says what re-establishes it. The campaign `pilot-dev-v3v4-20260923-10`
re-runs the key contrasts.

## Protocol findings (provisional)

**Self-cancelling revision.** With the original pending-segment wording ("When
a segment is pending, wait or revise it"), the prompted Qwen3-8B A2 policy
revised every just-started segment to identical text. That cancelled its own
audio: 58 identical revisions and zero completed segments over four branches on
st-02, even with 13 s of silence before feedback. Text decisions took about
2.2 s, so each next observation showed a freshly restarted segment. In
fixed-state replays of the trigger states, adding the segment's age or a
"starting" note changed nothing (32/32 identical revisions). Rewording the
affordance to "wait to let it play; revise only to change its words" moved the
act to wait (p 0.78–0.91).

**Over-correction, twice.** Showing that pending-state line in idle states too
(v2) produced wait in 37/37 idle states. v3 replaced the idle-state line with
"Nothing is pending: speak or continue…", which caused verbatim repetition of
the last completed sentence: 96 of 292 played transitions across all 24 pairs,
in every family except steering, where v3 had been designed. In 24 such states,
v3 repeated 24/24, while the v1 line or no line mostly waited (21–22/24). v4
keeps v1 wording everywhere except the pending-state line. **Re-established by:**
the interleaved v3/v4 campaign's repetition rate and completed-segment counts.

**Timing contrast.** On st-02 with a 10 s gap, the untimed cell A1 spoke during
the user's opening question in 12/12 branches and A2 in 0/12 (three repeats,
one pair). **Re-established by:** an A1/A2 run on the same fixtures.

**Mid-speech adaptation.** With v3 across 24 pairs, the lexical screen
recorded six feedback-only passes. Three survived inspection. In sc-02, the
policy revised "First, melt the butter…" into "First, melt the olive oil in a
pan over medium heat" while speaking, and independent Nemotron ASR confirmed
the audible change against a muted control that continued with butter and
onions. In co-02 it said "Bonjour" for "good morning". In pa-03 it said
"That's unsafe. Adding more acid to neutralize a spill is incorrect." The
steering deepen variants never discriminated: their required content is the
default next step. Revision pairs never realised their before/after contrast,
because options had not been heard when the authored late constraint arrived.
Late-constraint fixtures (`gap_fixture.py --variants after-playing`) address
this. **Re-established by:** paired verdicts plus window ASR in the new
campaign.

**Native reference.** On st-02 with a 10 s gap, DuplexCascade (native grammar)
cancelled its backlog after the deepen request and audibly began the feeding
schedule 3.5 s later. Its muted control played stale backlog, and skip did not
adapt. The model sent markdown to TTS, and generated audio ran up to 70 s ahead
of playback. Two runner defects were fixed: a cancelled reader crashed the
campaign, and a `BaseException` left a trial recorded as "running".

**Capacity versus representation.** In fixed post-feedback states, the 8B
policy without thinking waited or repeated. With thinking it revised content in
1 of 2 states per branch, at 3.5–15 s per decision. That is suggestive that
per-tick policy capacity, not the A2 representation, limits adaptation. It is
not established.

## Scoring lessons

- The completed-playback lexical screen over-credits. The paired check
  (`summarize.py`) counts a feedback pass only when the muted control fails an
  upper-bound screen over played text. The semantic judge (`semantic_judge.py`)
  judges window ASR for the feedback branch and its control, after known-answer
  validation.
- Null variants ("unchanged", "continue") are expected to be non-discriminating.
- Kyutai synthesis is stochastic: re-preparing the same text changes durations
  and word times. Freeze fixtures by retained PCM hash, not by text.
- Temperature-0 decisions were not bit-identical under shared vLLM batching.

## Re-run on the fresh clone: v3 and v4 over all 24 pilot pairs (2026-09-23)

`pilot-dev-v3v4-combined-20260923-12` covers the 24 pilot pairs under A2 with
history omitted, v3 and v4 interleaved per pair in alternating order, one
repeat. The 15 mid-speech pairs use gap10 fixtures. Silence and proactive use
original timing. Revision uses the late-constraint fixtures. Five pairs are
taken from an interleaved supplement: four had hit a runner defect that
aborted a pair when a branch correctly stayed silent (now fixed), and one had
a stalled Qwen request. Their originals are retained. The causal audit passes
all 12,717 requests in both campaigns. Two v3 branches failed with genuine
policy errors, where the action overran the 128-token budget; they stay
recorded as failures.

| | v3 | v4 |
| --- | ---: | ---: |
| Played sentences | 511 | 248 |
| Verbatim repeats of the previous sentence | 188, in 75/96 branches | 33, in 28/96 branches |
| 500 ms deadline misses | 957 | 628 |
| Lexical feedback-only passes | 1 (ov-03) | 1 (st-01 skip) |

**Repetition: revised.** v4 cuts verbatim repetition by about 80% but does not
remove it, so v3's idle line was not its only cause. v4 speaks half as much and
stayed silent through the whole correction window in sc-02 and sc-03. Neither
wording is adequate. The trade-off between repetition and silence is the
policy's, not the protocol's.

**Adaptation: partly re-established, still not reliable.** A single-rater
review of independent Nemotron ASR of each window, feedback against muted
control, for the semantic-correction and steering change variants:
- v3, sc-02: "…if you're using olive oil it will add…" against the control's
  butter. This reproduces the olive-oil adaptation. The lexical screen missed
  it because the required second word group was absent.
- v3, sc-01 and sc-03: audible acknowledgement-and-redirect ("I heard you meant
  Kyoto. Let me suggest…") without the corrected content inside the window.
- v4, st-01 skip: "…establishes a connection using either HTTP or HTTPS",
  against the control still locating the server.
- v4, st-02 deepen: "The feeding schedule involves feeding…".
- v4, st-03 deepen: "…the refrigerant cycle starts with a compressor that
  pressurizes…", against the control's repeated "similar to a refrigerator in
  reverse".
- All other skips failed.

The lexical screen caught one of these four genuine changes, and its other
pass (v3, ov-03) is only an acknowledgement ("let's explore options that meet
this requirement"). Lexical verdicts are therefore not a usable capability
score.

**Semantic judge.** Qwen3-8B as judge failed validation and refuses to score.
`gemini-3.8-flash`, as requested, gave the following results:
- Audio mode is unusable. Given nine seconds of digital silence, it reported
  hearing "Heat a couple tablespoons of olive oil over medium heat".
- Transcript mode, on independent Nemotron ASR of each window, scores 17/18 on
  known answers. The 18 include real campaign transcripts. Its one error
  credits an acknowledgement of a constraint (ov-03) as fulfilment.
- A requirement-based rewording of the goal made it worse (14/18) and was
  reverted.
- The judge runs at a declared 0.9 threshold, and every pass is reviewed.

Over the 90 judged variant pairs, `gemini-judge-transcript-03` reports 16
feedback-only passes (v3 9, v4 7) against 2 from the lexical screen. Single-rater
review of each pass against its transcripts and control:
- **Genuine (7):**
  - v3: sc-02 olive oil; pa-02 "the numbers don't add up"; pa-03 "that
    instruction is unsafe".
  - v4: st-01 skip to the connection; st-03 deepen into the refrigerant cycle;
    pa-02 "revenue decreased from six million to four million"; si-01 answers
    after a finished sentence.
- **Weak (3):** v4 ov-02 offers a 20-minute podcast under a 30-minute limit;
  v4 pr-01 continues after "okay" while the control is silent; v3 si-02 asks
  to confirm the new date.
- **Not genuine (4):** v3 ov-03, acknowledgement only; v3 sc-01 and sc-03,
  acknowledge and redirect with no corrected content; v4 pr-02, the same
  question as its control.
- **Null variants (2):** v3 sc-01 and sc-02 "unchanged" are no-change
  conditions, not adaptation tests.

Genuine audible adaptation therefore occurs in about 7 of 90 variant pairs
(single repeat), spread over correction, steering, proactive and silence
families, under both affordances. Gemini's passes are about 60% precise on
review, so it screens candidates and does not replace review. Its recall is
not measured.

**Opportunity.** 115 of 192 branches had assistant speech playing at the
variant's onset. The gap fixtures do create mid-speech situations in most
branches.

## A1 against A2 over all 24 pilot pairs, v4 (2026-09-23)

`pilot-dev-a1a2-20260923-13`: the same pinned fixtures, A1 (untimed transcript)
and A2 (timed) interleaved per pair, v4 affordance, one repeat. All 48 runs
completed, and the causal audit passes 11,288 requests.

| | A1 | A2 |
| --- | ---: | ---: |
| Branches talking over the user's opening question | 8/96 (2 pairs) | 4/96 (1 pair) |
| Feedback branches talking over the user's feedback | 25/48 | 25/48 |
| Branches that never speak | 20/96 | 9/96 |
| Played sentences | 178 | 249 |
| Verbatim repeats | 17 | 43 |
| 500 ms deadline misses | 391 | 603 |

**Timing contrast: revised.** The earlier result (A1 talked over the opening
question in 12/12 branches, A2 in 0/12, st-02) does not generalise. Under v4,
neither cell talks over st-02's opening. The remaining overlaps are all
revision-family greetings ("Welcome to the podcast recommendations…") in both
cells. The earlier effect was an interaction between missing timing and v3's
idle line, which invited an unprompted greeting. Under v4, removing timing
mainly makes the policy quieter. Both cells talk over the user's feedback in
about half of the feedback branches. Neither has a policy for yielding.

**Adaptation.** The Gemini judge (validation 17/18, same known failure)
reported 8 feedback-only passes. Review:
- **A2: 4 genuine.**
  - ov-01: "I can make it dairy free with a vegan chocolate sauce", revising
    its lava-cake suggestion after "nothing with dairy".
  - pa-02: "the revenue decreased from six million to four million".
  - st-02 deepen: "the feeding schedule typically involves feeding your starter
    every twelve…".
  - st-03 deepen: "the refrigerant cycle starts with a compressor…".
- **A1: 2 genuine** (pa-03 "that's a dangerous instruction"; si-01 asks the
  booking time), **1 weak** (ov-01 promises dairy-free options) and 1
  null-variant pass.

**Repeatability.** pa-02, st-02 deepen and st-03 deepen adapted audibly under A2
with v4 in both independent campaigns (`…-combined-20260923-12` and this one).
This is the first evidence that particular adaptations recur rather than
occurring once. It is still a handful of pairs out of 24, with one repeat per
campaign and single-rater review.

## Three-campaign repeatability, A2 with v4 (2026-09-23)

A third A2/v4 campaign (`pilot-dev-a2v4-repeat3-20260923-14`: 24 runs, 5,597
audited requests, zero violations) gives three independent runs per pair
under the same fixtures. Gemini-judged feedback-only passes, reviewed, per pair:

| Pair / variant | Passes | Reviewed as |
| --- | ---: | --- |
| st-03 deepen | 3/3 | genuine every time |
| pa-02 should-correct | 3/3 | genuine every time |
| ov-01 before-playing | 2/3 | one genuine revision, one promise only |
| st-01 skip, st-02 deepen, si-01 sentence-finished, ov-02 | 1/3 each | genuine or weak once |
| pr-01, pr-02 affirmative | 1/3 each | weak |
| sc-01 unchanged | 1/3 | null variant |

Correction to the A1/A2 section: counting only the Gemini verdicts, st-02
deepen passed in one of the three campaigns, not two. The earlier count mixed
a manual review of campaign 1 with the judge's verdict in campaign 2.

**Milestone evidence (partial).** In st-03 deepen, the assistant was
mid-sentence ("Even when it's cold, there's still some heat in the air") when
the user said "go back and explain the refrigerant cycle more slowly", in all
three runs. It let that sentence finish, and its next content turned to the
refrigerant cycle and compressor. The muted control never reached that content.
This is repeatable, paired, audible mid-speech content adaptation, verified by
independent ASR, the Gemini judge and review. It rests on one pair. The same
pair's skip branch fails 3/3, so P2's acceptance (opposite continuations for
"I know that part" and "explain that part") is not met. pa-02 recurs 3/3 but is
proactive correction: the assistant was not speaking at the user's claim.

Open items this development round cannot complete alone:
- The native reference: the DuplexCascade checkpoint is no longer in the local
  cache, and re-downloading needs approval and disk.
- The P3 memory probe: it needs about 25 GB of free GPU memory or a small
  local model with weights.
- A3: it needs validated, consented prosody recordings.
- Human listening ratings, and the prompt/scorer freeze before the
  preregistered pilot.
