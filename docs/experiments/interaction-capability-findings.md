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

**Semantic judge: not validated.** With Qwen3-8B as judge, validation failed.
It first rejected the confirmed olive-oil correction because the window opens
on the interrupted "butter" fragment. After a rubric rule for interrupted
fragments, it rejected a skip that moves to baking after such a fragment. The
judge refuses to score until it passes. A stronger judge (a larger local model,
or an external service if approved) or human raters are needed before any
semantic score is reported.

**Opportunity.** 115 of 192 branches had assistant speech playing at the
variant's onset. The gap fixtures do create mid-speech situations in most
branches.
