# The spoken boundary

A cascade decides a whole sentence, hands it to a synthesiser, and paces the
audio out at the rate it plays. When somebody interrupts, the audio stops
wherever it had got to. Until this existed, the runtime recorded that as a
duration — "the agent was audible for 2.1 seconds" — and a duration cannot say
where in the sentence the audio stopped.

Everything downstream then had two options and both are wrong:

- Believe the agent said the whole sentence. Ask it to read a long list,
  interrupt it, and it resumes after the last item it *wrote*, past items nobody
  heard.
- Believe it said none of the sentence. It starts again from the beginning, so
  the person who interrupted is answered with a repetition of what they just
  heard.

The honest answer is a boundary inside the sentence. That is what this is.

## What is measured

`spoken.Timeline` is one utterance's own words laid against its own audio: each
word with the interval of utterance-relative audio that carries it. Given how
much audio was paced out before playback stopped, the split is arithmetic.

`spoken.Mark` is that split, and it deliberately has three parts rather than
two:

| field | meaning |
|---|---|
| `Spoken` | words whose audio finished — what the user actually heard |
| `Cut` | the one word playback stopped inside, if the stop missed a boundary |
| `Pending` | everything never heard whole, the cut word included |

A cut almost never lands on a word boundary, and the word it lands inside was
neither heard nor unheard. Folding it into `Spoken` makes the agent skip it when
it resumes; folding it silently into `Pending` loses the fact that explains why
the resumption repeats a word. So it is named, and it opens `Pending`, because
resuming from a half-said word is what a person does.

## Where the times come from

Two sources, and which one a timeline holds is recorded rather than inferred.

**Measured.** A recogniser with word timestamps listens to the synthesised audio
and its words are reconciled onto the text the synthesiser was given. The
recogniser is used for timing, never for wording: what was said is already known
exactly, and its transcript is a worse copy that re-spells numbers, drops
punctuation, and occasionally mishears a word outright.

The reconciliation aligns *characters* of a canonical form rather than words,
because the disagreements that matter break a word-to-word correspondence:
`23` against "twenty three" is one word against two, "sea bass" against
"seabass" is two against one, and a hyphen is a word boundary in one and not the
other. Canonicalised — lowercased, punctuation dropped, digits spelled out — all
three become the same characters in the same order. An anchor has to cover most
of a word rather than a letter of one, because a global alignment will happily
match the final "e" of "three" to the final "e" of "five".

**Estimated.** No recogniser configured, or none has answered yet: the same
words are laid out over the duration proportionally. A synthesiser speaks at a
fairly even rate within one utterance, so this lands each boundary within about
a word — which is the resolution the decisions downstream need. `Measured` stays
false, and everything that reads a boundary can tell the two apart.

One subtlety is worth knowing about. A synthesiser is back-pressured to realtime
by the planner that consumes it, so "audio produced so far" tracks "audio played
so far" almost exactly. Laying the text out over the audio that exists would
report every utterance as nearly finished at every moment of its life. So while
synthesis is running the layout is made over the longer of a speaking-rate prior
and the audio-plus-one-word, and words the recogniser has not reached are placed
*past* the audio: audio that does not exist yet cannot have been played. When
synthesis ends, the guess is replaced by the real duration.

## Where the boundary goes

**Into the trajectory.** `AssistantState.Heard` records the split beside the
existing `PlayedAudioMS`. A client that truncates playback is authoritative
about what was heard, so a truncation re-splits the same layout at the client's
position rather than merely shortening a duration.

**Into every provider projection.** `continuation.ProviderRuns` — the one place
all three provider adapters go through — rewrites an interrupted assistant turn
into what the user heard, followed by a runtime note carrying what was prepared
and never spoken. The words are still there for the model to continue from; they
are simply no longer presented as something the user was told. Retained
provider-native state for a partly heard turn is not replayed, because the
native message is the whole turn and replaying it would put the unheard half
back.

**Into the interaction model.** While the voice is still speaking, the situation
block carries `the user has already heard: "…"` and `not audible yet: "…"`.
Keep-speaking and stop-speaking are different acts depending on which side of
that line the point of the turn falls on. Both lines are omitted when no
boundary is known: absence means nobody measured, which is not the same as
nothing having been heard.

**Into the voice.** `cognition.Request.Speaking` tells the fast provider what is
already audible and what is written but not yet audible, as two separate
statements, because they license opposite things.

## Running it

Nothing is required. Without a word-timing endpoint the boundary is estimated
and recorded as an estimate.

```bash
./scripts/prepare-wordtimings.sh --serve
./openrealtime serve -word-timings-url http://127.0.0.1:8003/v1/audio/transcriptions
```

The endpoint is the published transcription shape — multipart WAV,
`response_format=verbose_json`, `timestamp_granularities[]=word` — so a hosted
API or any other server answering that request is a drop-in replacement.
`openrealtime.deepgram.yaml` carries a `word-timings:` block for the
Deepgram/Qwen/Gemini/Fish profile.

An endpoint that transcribes but returns no word times is a misconfiguration
that would otherwise degrade silently for ever, so it is named once by error
rather than absorbed.

## How it is evaluated

The scenario suite's `SubturnSuite` holds *picking up where it was cut off*: the
agent is asked to count to forty, stopped partway, and told to carry on.
Counting is the instrument rather than the subject — every item is distinct,
ordered, and equally long, so where the agent resumes says exactly what it
believed it had already said. Interrupted mid-anecdote, an agent that carried on
from the wrong place would merely sound a little disjointed and pass every check
anybody could write.

It is the only case in the suite that cannot be scored from the wire. Every
other check asks what the agent *said*, and the transcript of a turn arrives with
the turn; this asks what the user *heard*, and after an interruption those stop
being the same thing. It is therefore scored against the agent's recorded
waveform, transcribed by something that had no part in producing it — scoring it
against the runtime's own account of where it got to would mark a runtime
correct for carrying on from wherever it believed it had, which is the belief
under test.

```bash
openrealtime bench scenario \
  -architecture-manifest .runtime/architecture/f52-cascade-local.json \
  -review-dir .runtime/scenario-review \
  -transcribe-url http://127.0.0.1:8003/v1/audio/transcriptions
```

Without `-transcribe-url` the case reports `NOT VERIFIED` and does not pass.

It runs **outside** the gated acceptance contract, and that separation is
deliberate. `scenario.Suite()` is fingerprinted: its case list decides a graph
contract digest, and its accepted pass thresholds were measured over that exact
list at a documented population. A suite that grew from eleven cases to twelve
while keeping a minimum of 140 out of 165 would be a weaker gate wearing the same
number — one that passes with the new case failing every time. So the new case is
reported in full, with its own heading and no claim of acceptance, until it has a
measured baseline of its own. `-subturn=false` skips it.
