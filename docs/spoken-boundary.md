# The spoken boundary

When a user interrupts an answer, generated text and heard speech diverge.
OpenRealtime tracks the boundary within the utterance so a later response can
continue from what the listener actually heard.

For example, if the model generated “one, two, three, four” but playback stopped
partway through “three”, the history should distinguish completed words, the
cut word, and pending words. Recording the entire sentence as spoken skips
unheard content; recording none of it causes repetition.

This technical note explains the representation, timing sources, provider
context, and evaluation. Start with [Core concepts](concepts.md) for the
session model, or jump to [Running it](#running-it) for word-timing setup.

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

## Text segmentation before synthesis

The scenario conversation graph uses `minimum_runes: 1` for complete sentences
and `minimum_clause_runes: 12` for comma, semicolon, colon, and dash breaks.
This keeps a short introduction such as `First,` attached to its phrase, while
`Yes.` and individual complete counting sentences can still be released during
generation. Completed source text flushes immediately even without punctuation
or the minimum length. Other graph profiles that omit `minimum_clause_runes`
keep their existing `minimum_runes` behavior. These text boundaries do not prove
continuous playback; recorded audio is still required to measure synthesis
and playout gaps.

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
openrealtime scenario \
  -architecture-manifest .runtime/architecture/f52-cascade-local.json \
  -review-dir .runtime/scenario-review \
  -transcribe-url http://127.0.0.1:8003/v1/audio/transcriptions
```

Without `-transcribe-url` the case reports `NOT VERIFIED` and does not pass.

The case is part of the fingerprinted twelve-case graph-native contract. Its
promotion changes the reportable population from 165 to 180 attempts and gives
it the same retained scorer result, stereo WAV, checklist row, source manifest,
and portable receipt as every other case. The historical 140/165 result remains
historical evidence for the earlier eleven-case contract; it is not reused as a
threshold for the larger population. The twelve-case release gate requires all
180 attempts to pass, so adding this case cannot weaken the prior gate by
allowing it to fail silently.

## Retained twelve-case checkpoint (v28)

The completed Deepgram/Qwen/Gemini checkpoint is retained under
`.runtime/deepgram-scenario-media-v28`. It contains exactly twelve canonical
stereo WAVs, one per case, and the deterministic checklist reports 12/12
passed. The secondary Gemini 3.7 Flash review evaluated all 12/12 recordings
and also reports 12/12 pass. The portable source and review receipts are,
respectively:

- `sha256:6be3a0ee4b405af7d543cfa3ff5910ed888481ef6a182bae3487ab62da50bf7e`
- `sha256:5d8fbd014096e50d56fafe81059f891a5a7ee00be2ae31cc5333de3c73381e26`

The twelfth row is **picking up where it was cut off**, trial 1, behavior
`passed`. Its canonical recording is
`12-picking-up-where-it-was-cut-off-trial-01.stereo.wav` with digest
`sha256:19a9458cf80883850fa342eb664323e97605ac0e6672eefaab7ae630732495b7`.
The reviewer’s per-case evaluation and receipt are retained alongside the
other eleven; the source and evaluation bundles reopen successfully with
`review verify-scenario`.
