# Building an interaction model: what the measurements said

This records what was learned building the model that decides *when* an agent
acts, so that the next person does not rediscover it. The design itself is
ADR-0009; the numbers are in `measurement.md` under F13–F18. This is the part
that is neither: the bugs, and the shape they kept taking.

## Almost every bug was a missing input

Nine defects were found across this work. Seven were the same bug wearing
different clothes: **a model was asked a question and not given what it needed
to answer it**, and each time the wrong answer looked exactly like a model that
was not good enough.

| what was missing | what it looked like |
| --- | --- |
| the utterance, once its speaker stopped | the model answered "listen" to a finished question |
| a tool, while `call-tool` was offered | it never pressed a key at a phone menu |
| the conversation, at the pause decision | it talked through every pause somebody asked it not to |
| the policies in force, at extraction | every revocation came back "none" |
| the recent turns, at extraction | a continuation of an instruction read as a reversal of it |
| the live partial, at the voice | told to count, it counted from one to ten |
| the agent's contract, at the decision | it never once corrected a wrong date |

The lesson is procedural rather than architectural. When a decision is wrong,
read the exact bytes the model was given before touching the prompt. Four of
these were "fixed" with prompt rewrites first, and the rewrites did nothing,
because the prompt was not the problem.

`OPENREALTIME_CONTEXT_TRACE` writes every compiled request to a file for this
reason, and `-interaction-shadow` does the same for interaction decisions,
including the pause decisions that were invisible for most of this work and
where two of the bugs were.

## Two measurements that were not measurements

**The harness.** The speech backend returns 44.1 kHz whatever `sample_rate`
asks for. Those samples in a 24 kHz pipeline stretch by 1.84 and drop an
octave, so "a heron landed on the far bank" reached the recogniser as "the horn
mounted on the far bank". That reads like a weak recogniser and is arithmetic.
Every scenario number taken before the resampler is void.

**A passing test.** An intermediate build scored 3/3 on the scenario it was
hardest to pass, and it was a floor that had frozen: it cached its verdict by
revision, and the last call before a pause is made while the speaker is still
audible, where answering is refused. The refusal was served back for the whole
pause and the turn never ended. On the transcript that is indistinguishable
from honouring "don't interrupt me". Fixing it returned the scenario to 0/3.

Both numbers came from the same code. Only one of them was a measurement.

## Scoring that flatters

The first version of the end-to-end suite reported 4/5 and every pass was
false. Asked to count animals, the agent said "How many animals did you see?" -
the check asked only whether it had spoken near the animal. Asked to correct a
date mid-sentence, it cut in with "What would you like to plan?" - the check
asked only whether it spoke during the monologue, which an agent that waited
politely until the end also satisfies. At a phone menu it said "One moment,
I'll check on your order" out loud, to a recording - the check asked only
whether the key was pressed.

A check that a system can pass by making a noise at the right time is not
measuring the capability. Checks now bind to a window, can assert or forbid
content, and can require that something happened *before* a line ended.

The same applies to aggregate scoring. Acting and restraint are reported
separately because at a boundary whose commonest right answer is to do nothing,
a model that always acts and one that never acts post identical totals while
being opposite bugs - and the first two runs of this suite were exactly that
pair.

## Prompt effects that are real and reproducible

**Format consistency was worth more than any wording.** Worked examples written
as compressed one-liners while the live input was a labelled block cost ten
points, because the model spent capacity translating between two shapes of the
same thing. Examples now render through the same function as a real decision.

**The name of the do-nothing act is a threshold, not a quality lever.** Asked to
explain a wrong answer, a model said it chose "listen" *to keep track of what
the speaker was saying* - it had understood the task and read the act as an
instruction to pay attention. Renaming it slides a model along a trade and
leaves discrimination unchanged: listen 40%/85%, wait 60%/73%, stay-silent
75%/54% on the same model and prompt.

**Examples teach the wrong thing as readily as the right thing.** A single
turn-scoped example had its wording copied onto rules nothing like it: "let me
know if I start slouching" came back as "stay quiet until they finish their
current thought". And an example containing "count them as I mention them"
primed the exact behaviour the paragraph around it forbade - moving that rule
to the end of the prompt, where recency should have helped, made it worse.

**Instruction capacity is real.** One added paragraph moved unrelated decisions
from 8/15 to 3/15 in an earlier measurement. Every instruction added here was
checked against a boundary it had nothing to do with; the ones that survived
cost nothing.

## The two phases want different models

| | interaction decision | the voice |
| --- | --- | --- |
| asked | several times a second | once a turn |
| Qwen3-30B-A3B | 30–40ms, balanced 0.76 | performs a standing arrangement on being told about it |
| Qwen3-8B | 33–60ms, balanced 0.68 | acknowledges correctly, then invents an animal |
| Gemini 3.5 Flash, minimal thinking | 700–900ms — unusable | gets every turn right |

Reasoning is not available to the interaction decision at all: enabled on the
8B it scored 9/23 against 13/23 with it off, and took 5.5 seconds.

And the capability needs both halves. A better voice alone leaves the counting
demo at 0/2; the interaction model alone left it at 0/3; together it is 5/6.
The interaction model supplies acts at the right instants and cannot control
what is said into them; the voice does what the act asked and has no idea when
to be asked.
