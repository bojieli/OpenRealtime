# Building an interaction model: what the measurements said

> [!NOTE]
> **Research note.** This is a record of findings, failed assumptions, and
> corrected measurements—not a setup guide or current API contract. See
> [Architecture](architecture.md) for shipped behavior and
> [Measurement](measurement.md) for the underlying evidence.

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

## The acts were right and the machinery around them was not

Every defect in this section was found the same way: by reading what each model
was actually handed, rather than by reasoning about what it should have
decided. All of them looked, from outside, like the interaction model choosing
wrongly. None of them were.

**An interruption was cancelled by the sentence it was interrupting.** The
shipped barge-in policy yields the floor the moment the user speaks. Speech the
agent began deliberately over somebody who already had the floor is the one
case where that is wrong, and the policy could not tell the two apart. In the
recording the difference is unmistakable: a turn produced in silence emits four
to six seconds of audio, a turn produced over somebody emits exactly one
hundred-millisecond frame and stops. The guard for this already existed for
backchannels, on an argument that was never about backchannels - cancelling
overlap the agent chose is the agent interrupting itself.

A first attempt read the marker from the duplex state when the audio was
queued, and failed on exactly the case it was for: the recogniser cuts one
sentence into several stretches, so by then the stretch being answered had
closed and reopened. It has to come from the act, which is the thing that
knew.

**The sentence an interjection exists for was never in the conversation.** It
went into the instruction - "what they have said so far is ..." - and nowhere
else, so the conversation handed to the provider ended with whatever the agent
last said. Asked to continue from its own last turn with nothing new addressed
to it, a provider says nothing:

| what the conversation ends with | three calls |
| --- | --- |
| the agent's own "1" | `''` `''` `''` |
| the same call, the next animal as a user turn | `'2'` `'2'` `'2'` |

Five instruction variants were tried before that, down to six hundred
characters. Every one returned empty. This was the whole of three separate
failures - the second animal, the second sentence of an interpretation, the
dish that fits - and every one of them had been filed as a prompt problem.

**A recogniser cuts where somebody breathes, and both halves read alone are
wrong.** "Tell me the moment the build finishes and don't say anything else"
arrives as two utterances. The front half pins a truncation; the back half,
capitalised and punctuated like a sentence of its own, revokes it - five times
out of five against the model that runs the pass. Joined, the same model pins
the whole instruction with a scope that outlives the turn.

Which pieces belong together is a fact about the clock rather than the words,
and the runtime already measures it for the interaction model: 115 and 210
milliseconds on the splits this was found on, against seconds for a genuine new
turn.

**A picture reached only the model that can see.** A client attaching an image
is the ordinary way to show an agent a screen. Everything else in the session
was handed the sentence "The user attached an image", including the layer
deciding whether this was a moment to speak at - so on the visual case it was
choosing between silence and speech about a screen it had been told nothing
about. Reading a whole run of its situations back, no description of a screen
appears anywhere. A video observer narrates every frame for this reason; the
attachment path had nothing.

And the description is a narrator's account, not something the person said, so
the pass that lifts standing policies out of speech must not read it: one
screen description in five revoked the policy the conversation was running on.

**One target, one repair obligation.** An assistant item can be cut off,
corrected, and cut off again. The canonical order recorded a target's place
each time it became pending rather than the first time it was seen, so the
second cycle listed it twice - and the caller turned each entry into a
resolving item, producing a batch that resolved the same repair twice. The
second is rejected, the batch is refused whole, the obligation never clears,
and the session dies a few seconds later.

### What this cost, and what it says about method

Six defects, all in the space between a decision and its execution. The
interaction model's accuracy was never the thing standing in the way, and three
separate attempts to improve it by rewording something were wasted.

The instrument that ended it was small: record what a turn was asked for
whenever it produces nothing anybody hears. Three things can silence a turn -
the model returns nothing, the output never reaches a safe point, the
commitment policy holds it - and all three returned silently. From outside they
are indistinguishable from the decision layer being wrong.

## A probe that was right about the case it asked about

Twice today the same method worked: reconstruct the exact input a model was
given, ask it directly, change one thing, count. It found the empty
continuation, the revoked policy, and the truncated pin, and each time the fix
that followed held up in the suite.

The third time it did not.

A recogniser splits a request in two, so the tail arrives looking like a
request of its own and the agent answers it - a second acknowledgement of
something already agreed to. Told plainly that the tail was a tail, the model
listened seven times out of seven instead. Clean result, obvious fix, and the
suite disagreed:

| | before | after |
| --- | --- | --- |
| a recorded menu | 2/5 | 0/5 |
| ordering from a waiter | 4/5 | 3/5 |
| translating as they speak | 5/5 | 1/5 |

Every one of those has somebody delivering consecutive sentences with a breath
between them, and the measurement that identifies a tail - how soon the speaker
started again - cannot tell a breath inside a sentence from a breath between
two. Told the next sentence was the rest of one already answered, the agent
listened: the second sentence of an interpretation went untranslated, the next
menu option unpressed, the dish that fits unordered.

The probe was not wrong. It answered the question it was asked, about the case
it was given, and that case is real. What it could not do is say what the same
sentence costs in the cases it was not given - and the difference between the
two is the difference between an experiment and a suite.

The distinction that survives: **the gap is evidence and "this is a tail" is a
conclusion.** Handing a model the conclusion decides the cases it was not
derived from. Handing it the whole sentence - which is what the
standing-instruction pass gets, and where this measured a real gain - is
strictly more information with nothing concluded from it.

## Which model takes the decision, measured on the design as it stands

Both candidates on the same 96 cases, three runs each, guided decoding,
reasoning off, on the one local GPU:

| | balanced | acting | restraint | p50 | p90 | worst |
| --- | --- | --- | --- | --- | --- | --- |
| Qwen3-8B | 0.69 | 33/45 | 33/51 | 35ms | 55ms | 60ms |
| Qwen3-30B-A3B-FP8 | 0.72–0.73 | **36/45** | 33/51 | **30ms** | **38ms** | **41ms** |
| Qwen3.6-35B-A3B-FP8 | **0.81** | 33/45 | **45/51** | 87–107ms | 113–120ms | 166ms |

The larger model is also the faster one, which is what makes the choice easy
rather than a trade. A mixture of experts with three billion parameters active
does less work per token than a dense eight billion, so the 30B answers a
decision in 30ms where the 8B takes 35 and has a longer tail - and it is three
to four points better on the same cases.

Restraint is identical at 33/51. The whole difference is acting: 37/45 against
33/45. The smaller model misses moments it should have spoken at, which is the
failure that is invisible in conversation - nobody notices the sentence that
was not said.

This agrees with the earlier measurement on an older prompt (0.76 against 0.68).

**And then Qwen3.6 changes the answer.** Nine points of balanced accuracy over
the 30B, and every one of them is restraint: 45 of 51 against 33, twelve more
cases where the right move is to do nothing and it does nothing. Acting is
three cases worse. That is the trade one would choose without hesitating - a
missed moment costs a beat, and speaking into one that was not there is the
thing people actually dislike.

It costs about three times the decision: 87 to 107ms against 30. That still
lands inside the 250ms deadline a decision is given and inside the 150ms the
overlap classifier is bounded by, so it is affordable rather than free. Read
the first run of it carefully, though - 84 of 96 cases errored on a cold
server, because the eval fires them together and the first request took 238ms
with everything queued behind it. A model that misses the deadline reports as
broken rather than as slow, which is worth knowing before believing a bad
number.

Both sat behind the same 40k context and the same guided decoding. The 8B needs
16GB of weights against the 30B's FP8 footprint, so on a single card the choice
also costs nothing in memory that matters.

## Everybody in the room was called the user

The decision layer's situation carries a `Speaker` field, and for the whole of
this work one line set it:

```go
Speaker: "user",
```

Every voice that reached the microphone was reported to the model as the person
the agent works for. The conversation window did the same, prefixing every
observation with `user:`.

That is a false statement, not a simplification, and it is worth separating the
two. Reading back what the model was actually handed when it answered two
people discussing the shopping:

```
Recent conversation:
user: I'm just going to get on with this for a bit.
agent: Sounds good. I'll be right here if you need anything.

Now:
heard from user so far: "Did you get the milk on the way in, I looked in the
                         fridge, and there wasn't any."
```

It answered, and invented having added milk to a list. **No model would do
otherwise.** It was told the user asked it a question. This had been filed as
the decision model being over-eager, and a whole model comparison was read
through it.

Four of eleven scenarios have a third party in them and all four were affected.
Two pass anyway, because acting is the right answer there and being wrong about
who is speaking does not change the act: a waiter naming the dish, a colleague
speaking Mandarin. Two do not: a recorded phone menu, reported as the user
reciting menu options, and two people in a room, reported as the user asking
for the shopping.

The label now comes from the observation's own source, which is what the
runtime actually knows. A deployment that separates channels - a phone line's
far end, a second microphone, a recogniser that reports who spoke - is
described correctly with nothing further to change.

**One undiarised microphone still calls everybody in the room the user**, and
that is the honest reading of what it knows rather than a claim. Which means
the side-speech scenario is not passable today by any decision model, and
saying so is more useful than a score: the gap is in perception, before the
decision is ever taken. It is also the strongest argument yet for putting audio
into the model directly - voice identity is in the waveform, and an
ASR-to-text pipeline throws it away before anything can reason about it.

## The audio path answers the question the cascade cannot ask

Two people in a room discussing the shopping is the one scenario no decision
model can pass through the cascade, and the reason is structural rather than a
matter of quality: a single undiarised microphone reaches the recogniser, the
recogniser emits text, and by the time any decision is taken the fact that a
different person said it has been discarded. The decision layer is told the
user asked about the milk, and answers - correctly, given what it was told.

Voice identity is in the waveform. So the question is whether a model that
hears the waveform can do what a model reading the transcript cannot.

The same twelve seconds, synthesised with the harness's own two voices, handed
to Gemini 3.5 Flash as audio with the agent's contract and the seven acts:

| what the clip contains | what it chose |
| --- | --- |
| the user speaks, then a **different voice** asks about the milk | `listen` x5 |
| the user speaks, then **the same voice** asks the agent to look something up | `answer` x5 |

Five out of five each way, and the control matters more than the result: a
model biased towards silence would pass the first row and fail the second, and
this one does not. It is hearing who spoke.

It costs 1145ms on 3.5 Flash and 1463ms on 3.6 for twelve seconds of audio,
against 21-34ms for the local text decision - so this is not a replacement for
the decision taken several times a second. What it is, is proof that the
information the cascade throws away is recoverable, and that a scenario filed
as "not passable by any decision model" is only unpassable on one of the two
paths this project maintains.

Which is the argument for the omni path in one measurement: the cascade's
recogniser is not a neutral component that turns sound into text, it is a lossy
one that decides what the rest of the system is allowed to know.

## Counting fails because its policy lasts one utterance

Read from the decisions rather than the score. At the moment the agent answered
a line with no animal in it, the situation said:

```
   act: answer | policy in force: False
   heard from user so far: "It was a warm afternoon, and I was walking along by the river."
```

No policy in force - four seconds after one was pinned. Every counting policy
in the run was pinned with turn scope:

```
t=  11.9 pin  scope='turn'  'count the animals out loud as I mention them and say not...'
t=  23.0 pin  scope='turn'  'count the animals out loud as I mention them and say not...'
t=  26.9 pin  scope='turn'  'count the animals out loud as I mention them'
```

A turn-scoped policy is dropped when the next observation commits, which is the
next thing the speaker says - seconds later. So the policy governs nothing, the
decision layer has no reason to stay quiet, and the voice never receives the
paragraph that carries `<wait>`, because that paragraph is only attached when a
policy is in force. One mis-scoped pin explains the whole failure, including
why a token measured at six out of six never fired once in a live run.

It is not a runtime bug. Probed directly, the extraction chooses turn five
times out of five for the counting policy and conversation five out of five for
"tell me the moment the build finishes" - two policies with the same shape,
both watching for something that has not happened yet, which the extraction
instruction already says is "always a rule".

Sharpening the instruction did not move it. Told plainly that turn scope
expires when the sentence being spoken now ends, that anything firing more than
once is a conversation policy, and that "as I mention them" describes a series,
the answer was still turn five out of five - while the build case stayed
conversation. The model appears to read "as I mention them" as bounded by the
telling, which is a reasonable thing to think and is not what turn scope means.

So the defect is not that the model ignores a rule. It is that the distinction
lives in the mechanism rather than in the words: "as I mention them" and "until
I finish" sound alike and mean opposite things about how long a policy lasts,
and no amount of restating the rule teaches a difference the sentences do not
carry.

What taught it was a pair of examples that differ only in that:

    "I'll read out the numbers - add them up as I go and say the running total."
        -> pin conversation
    "Let me finish reading this out before you say anything."
        -> pin turn

Counting then extracts as a conversation policy five times out of five, with
the build case and "hang on" both unmoved. In the live runtime every pin in the
run comes back conversation-scoped, and the failure it was causing - speaking
where nothing was asked - is gone.

The example pays for itself rather than costing capacity, which is not what the
record here would have predicted: the standing-instruction eval goes from 52/66
to 56/66 with it. An example that names a distinction the prose cannot appears
to be worth more than the room it takes.
