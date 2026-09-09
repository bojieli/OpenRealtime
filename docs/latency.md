# Response latency

For the current deployed room, see the [September 9 stage and model comparison](room-latency-20260909.md). The historical measurements below use different configurations.

> [!NOTE]
> **Evidence record.** The numbers below belong to the named hardware, models,
> scenarios, and revisions; they are not a blanket latency promise. Use the
> methodology here to reproduce them and [the benchmark harness](benchmarks.md)
> to generate current results for your deployment.

This record measures response latency from the end of a triggering input to
the start of agent audio. It distinguishes that user-facing interval from the
time spent making an interaction decision.

In the recorded configuration, interaction decisions took 25–45 ms while total
waiting time was about one to two seconds. The main optimization question was
therefore where the remaining time went. The repeated-run and stage breakdowns
below investigate recognition, generation, and synthesis.

**Read:** [Method](#how-it-is-measured) · [Initial results](#measured) ·
[Repeated measurements](#measured-again-five-runs-of-every-scenario-f28) ·
[Stage breakdown](#where-the-time-actually-goes-f31).

## How it is measured

`openrealtime scenario -repeat N` plays a scripted conversation, records the
timed transcript, and reports latency pooled across every attempt. Every
trigger in a scenario is measured - each line somebody speaks, each frame the
agent is shown - because which one the agent was answering is not something the
harness can know, and reporting all of them is more honest than guessing at
one.

Three rules keep the numbers meaning what they say.

**Triggers the agent never answered are reported as unheard, not dropped.** A
scenario whose latencies all look excellent because the slow ones vanished is
worse than no numbers.

**The count is printed beside the percentiles.** A p90 drawn from two samples
must not be able to pass for one drawn from fifty.

**One run is not a measurement.** The recogniser, a hosted voice and a
synthesiser each vary, and the spread between runs of one scenario is
comparable to the differences worth caring about. Five is the working minimum.

The arrival of an audio event stands in for the moment it is heard. That is
exact while playback is realtime, and it is a proxy rather than a measurement
of a loudspeaker.

## Measured

SenseVoice recognition, Gemini 3.5 Flash at minimal thinking as the voice,
Qwen3-30B-A3B as the interaction model, pooled across runs:

| what triggered the reply | p50 | why it lands there |
| --- | --- | --- |
| cutting in on something wrong | **313ms** | fires on content; waits for no endpoint |
| interrupting a waiter mid-list | 1435–1720ms | same, plus a longer sentence to hear |
| a frame showing a build finished | **~900ms** | a vision model narrating, then a turn |
| an ordinary finished question | 1589–1806ms | endpoint silence, then recognition, voice, synthesis |
| translating a sentence as it lands | 1829–2113ms | as above, per sentence |
| a backchannel that must not stop the agent | 1730–2331ms | |
| a phone menu naming the right option | 2851–4845ms | the slowest, and the one where waiting costs most |

## The ordering is the finding

**Interrupting is faster than answering.** Answering waits out a silence
threshold to establish that a turn has ended. Interrupting fires on content and
skips that wait entirely, which makes the acts that sound risky the low-latency
ones and the ordinary reply the slow one.

Endpointing, not thinking, is where the time goes. That says where effort pays:
a hundred milliseconds off the voice improves every row a little, while a
hundred off the silence threshold improves only the rows that wait for it -
and those are the slow ones.

It also says something about the voice. Gemini answers in 700–900ms against the
local MoE's 30–40ms, and it is the better voice by a wide margin. Spending that
on the phase asked once a turn is affordable; spending it on the decision asked
several times a second is not. The two phases want different models for reasons
that have nothing to do with which is better.

## Bounds that are asserted

Three scenarios assert a bound, because their claim is about time rather than
behaviour:

- an ordinary question, 2500ms
- a phone menu, 4000ms — a menu moves on, and a key pressed late is pressed
  into the next option
- "tell me the moment the build finishes", 4000ms — "the moment" is a claim
  about latency and is measured as one

The menu bound fails at 4478–4845ms and deserves to.

## A measurement that was wrong twice

The first latency number recorded for the visual case was five to fifteen
seconds. What it had timed was a *second* acknowledgement of the user's opening
instruction rather than the report; against the frame itself the report lands
under a second.

And the first implementation was inverted. `FirstAudioAfter` returns the wait
rather than the moment, so subtracting the offset again turned every late reply
into a large negative number - a bound nothing could fail, which passed its
first test.

## Measured again, five runs of every scenario (F28)

Nine scenarios, five runs each, one server, pooled per scenario. Five is the
floor rather than the target: three runs put two of these rows inside their own
spread, and a number that moves by a scenario between runs is a range being
reported as a figure.

Waveform time throughout: from the last sample of the trigger to the first
sample the agent produces.

| what triggered the reply | p50 | p90 | worst | triggers |
| --- | --- | --- | --- | --- |
| an ordinary finished question | 1711ms | 1783ms | 1911ms | 5 |
| cutting in on something wrong | 1792ms | 1876ms | 2666ms | 4 |
| translating a sentence as it lands | 1945ms | 2076ms | 2356ms | 15 |
| counting an animal as it is mentioned | 1323ms | 6779ms | 8770ms | 20 |
| a waiter naming the right dish | 1304ms | 10411ms | 11327ms | 10 |
| a phone menu naming an option | 2301ms | 7432ms | 11360ms | 8 |
| a backchannel that must not stop the agent | 3055ms | 5951ms | 7104ms | 15 |
| a frame showing a build finished | 100ms | 5945ms | 6054ms | 15 |
| being asked not to be interrupted | 96ms | 96ms | 96ms | 1 |

Two of these rows are not what they look like, and both say something.

**The p50s are honest and the tails are not one number.** Counting is 1.3
seconds at the median and 8.8 in the tail, because a trigger the agent
correctly stays silent for still counts as a trigger with no reply until the
next one - the harness cannot know which of them the agent was answering, and
reporting all of them is the honest version. The rows whose whole point is
restraint have the longest tails for exactly that reason.

**The visual row is the one real latency problem.** 100ms at the median is the
acknowledgement landing near a frame; the number that matters is the report of
the build finishing, and it lands 4.2 to 5.2 seconds after the frame against a
budget of three. Measured directly, narrating one frame with Gemini 3.5 Flash
at minimal thinking costs:

  1173  1260  1447  1516  1522 ms

A picture has to become text before any text layer can reason about it, and
that is 1.45 seconds at the median before the observation exists at all. The
remaining ~2.7 seconds is the ordinary path - decision, voice, synthesis - which
is what every other row pays. The visual case pays both.

That is a real cost of the capability rather than a defect, and it is worth
stating plainly: an agent watching a screen answers about a second and a half
slower than one listening to a voice, unless the model deciding can see.

### The final pass, five runs of every scenario

| what triggered the reply | p50 | p90 | worst | triggers |
| --- | --- | --- | --- | --- |
| translating a sentence as it lands | 521ms | 1878ms | 1971ms | 15 |
| counting an animal as it is mentioned | 1134ms | 6196ms | 7597ms | 20 |
| cutting in on something wrong | 1230ms | 1616ms | 1634ms | 4 |
| an ordinary finished question | 1578ms | 1627ms | 1800ms | 5 |
| a backchannel that must not stop the agent | 1799ms | 2621ms | 3588ms | 10 |
| a waiter naming the right dish | 1827ms | 4914ms | 6780ms | 10 |
| a phone menu naming an option | 2944ms | 4429ms | 9981ms | 8 |
| a frame showing a build finished | 89ms | 4874ms | 5087ms | 15 |

The ordering from the earlier pass survives: interrupting is faster than
answering, because answering waits out a silence threshold and interrupting
fires on content. Interpreting is now the fastest row in the suite at half a
second, which is the same effect - a policy never to yield the floor never
waits for one.

Read the tails with the restraint in mind. A trigger the agent correctly stays
silent for is still a trigger, and its wait runs until the agent next speaks
for any reason, so the scenarios whose whole point is silence carry the longest
p90s. Counting's 6.2s p90 is the pause it was right not to fill.

## Where the time actually goes (F31)

A third of the visual path was unaccounted for after every component had been
measured on its own. Components measured apart do not add up to a path, so the
runtime now times its own stages.

One ordinary question, end to end, measured at 1951ms to first audio:

| stage | cost | where |
| --- | --- | --- |
| the gate waiting out the endpoint silence | ~500ms | local, by design |
| the voice: Gemini 3.5 Flash, first token | 674ms | cloud |
| the voice: whole answer | 807ms | cloud |
| the synthesiser: first byte | 289-911ms | local :8081 |
| the interaction decision | 21-34ms | local |
| everything else | ~150-300ms | local |

Two things this settles.

**The frame is not what makes the visual path slow.** An image costs the voice
300-525ms of extra prefill (525 on 3.5 Flash, 304 on 3.6 - the newer model is
faster with vision and slower on text), and it costs the decision nothing at
all: 21ms with a frame against 23ms without, on a model that reads it correctly
- shown a running build it says "41% complete", shown a finished one "all 214
tests passed". Putting the picture in the decision is free.

**The synthesiser is a bigger cost than the model that decides what to say, by
a factor of thirty.** It had never been measured.

### The synthesiser reads the whole text before it emits anything

With streaming on and PCM out, the wait for the first byte is a function of the
text handed over:

| what it was given | first byte |
| --- | --- |
| one word | 289ms |
| a short clause | 607ms |
| one sentence | 911ms |
| two sentences | 977ms |

That is not what streaming is meant to mean. Whatever the engine does over the
text - prosody, prefill, choosing a length - it does over all of it before the
first sample comes out.

Handing it the first sentence on its own is worth the difference between those
rows, and costs nothing but a second request. It is in, behind
`-speak-by-sentence`, and it saved 20ms on the control scenario - because
"The capital of France is Paris" is one sentence and there was nothing to
split. It will pay on multi-sentence answers and it does not touch the case
that needed it most.

**The honest state: the largest remaining cost in the system is a local
synthesiser taking 900ms to begin a sentence, and the fix for that is in the
engine or the model rather than in the caller.** Splitting further - at commas,
to get the first piece down to the 289ms row - would trade the wait for a risk
of gaps inside a breath, and a gap sounds like a fault where a wait sounds like
thinking. That is a change worth making only with something listening to the
result.

### Streaming the voice into the synthesiser is not worth it

The obvious next idea is to start synthesising the first sentence while the
voice is still writing the rest. Measured, it is not worth the architecture:

| | |
| --- | --- |
| first token | 674ms |
| first complete sentence | 752ms |
| whole answer | 807ms |

Fifty-five milliseconds between the first sentence and the last. The voice is
dominated by time-to-first-token, which is a round trip, and overlapping
generation with synthesis buys almost nothing.
