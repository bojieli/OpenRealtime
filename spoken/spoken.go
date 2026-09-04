// Package spoken answers one question the rest of the runtime keeps asking and
// has never been able to answer: of the words the agent decided to say, which
// ones did the user actually hear?
//
// A cascade decides a whole sentence, hands it to a synthesiser, and paces the
// audio out at the rate it plays. When somebody interrupts, the audio stops
// wherever it had got to. Everything downstream then had two choices, and both
// are wrong. Believing the agent said the whole sentence makes it carry on past
// words nobody heard - ask it to read a long number sequence, interrupt it, and
// it resumes at a number that was never spoken. Believing it said none of the
// sentence makes it start again from the beginning, so the same words are said
// twice and the person who interrupted is answered with a repetition.
//
// The honest answer is a boundary inside the sentence, and a boundary inside a
// sentence needs the sentence laid out against its own audio. That is what a
// Timeline is: the utterance's own words, each with the interval of its
// utterance-relative audio. Given how much audio was paced out before playback
// stopped, the split is then arithmetic rather than guesswork.
//
// Two things produce a Timeline. Measured times come from listening to the
// synthesised audio with a recogniser that reports word times, reconciled back
// onto the words the synthesiser was asked to say - the recogniser's job here
// is timing, never wording, because the wording is already known exactly.
// Estimated times come from a proportional model over the same words. Which one
// a Timeline holds is recorded rather than inferred, because a caller deciding
// whether to trust a boundary needs to know whether anything listened to the
// audio.
package spoken

import (
	"strings"
	"unicode"
)

// Word is one word of an utterance and where its audio sits inside it.
//
// Times are relative to the first sample of that utterance, not to the session
// clock. An utterance is the unit that gets cancelled, so it is the unit whose
// internal offsets stay meaningful when everything around them moves.
type Word struct {
	Text    string `json:"text"`
	StartMS uint64 `json:"start_ms"`
	EndMS   uint64 `json:"end_ms"`
}

// Timeline is one utterance's text laid out against its own audio.
//
// Words carries the utterance's own words in order, with the punctuation and
// casing the synthesiser was given, because what a Timeline is for is
// reconstructing sayable text on either side of a cut. AudioMS is how much
// audio the whole utterance turned out to be; a Timeline built while synthesis
// is still running carries what is known so far.
type Timeline struct {
	Text    string `json:"text"`
	Words   []Word `json:"words,omitempty"`
	AudioMS uint64 `json:"audio_ms,omitempty"`
	// Measured says a recogniser listened to this audio. False means the times
	// are a proportional model of the same words, which is a good deal better
	// than nothing and is not the same claim.
	Measured bool `json:"measured,omitempty"`
}

// Mark is what an utterance had actually said by a moment in its playback.
//
// The three fields are deliberately not a two-way split. A cut almost never
// lands on a word boundary, and the word it lands inside was neither heard nor
// unheard: the listener got part of it. Folding that word into Spoken makes the
// agent skip it when it resumes, and folding it silently into Pending loses the
// one fact that explains why the resumption repeats a word. It is named.
type Mark struct {
	// Spoken is the text whose audio finished before playback stopped. It is
	// what the user heard, and it is the only thing that may be presented to a
	// later model as something the agent said.
	Spoken string `json:"spoken,omitempty"`
	// Cut is the single word playback stopped inside, empty when the stop fell
	// on a word boundary. It is not in Spoken, and it opens Pending: resuming
	// from a half-said word is what a person does.
	Cut string `json:"cut,omitempty"`
	// Pending is everything the user never heard whole, the cut word included.
	Pending string `json:"pending,omitempty"`
	// Measured says the boundary came from listening to the audio rather than
	// from a proportional model of it.
	Measured bool `json:"measured,omitempty"`
	// PlayedMS is the playback position this mark was taken at, kept so that a
	// record of a cut carries the duration it was derived from.
	PlayedMS uint64 `json:"played_ms,omitempty"`
}

// Complete reports that the whole utterance was heard.
func (mark Mark) Complete() bool { return strings.TrimSpace(mark.Pending) == "" }

// Started reports that any of the utterance was heard at all.
//
// It is separate from Complete because the two failures are different: nothing
// heard is a turn that can simply be said again, while something heard cannot
// be taken back and constrains whatever is said next.
func (mark Mark) Started() bool {
	return strings.TrimSpace(mark.Spoken) != "" || strings.TrimSpace(mark.Cut) != ""
}

// At splits the utterance at a playback position.
//
// A word counts as spoken only when its audio finished, because a word whose
// last syllable was cut off is not a word the listener received. The one word
// straddling the boundary is reported as the cut rather than assigned to
// either side.
func (timeline Timeline) At(playedMS uint64) Mark {
	mark := Mark{Measured: timeline.Measured, PlayedMS: playedMS}
	if len(timeline.Words) == 0 {
		// Nothing is known about where the words are. Reporting the whole text
		// as spoken would be the exact failure this package exists to remove,
		// so an unknown layout says only what the caller already knew: audio
		// went out, and which words it carried is not established.
		mark.Pending = strings.TrimSpace(timeline.Text)
		return mark
	}
	spoken := make([]string, 0, len(timeline.Words))
	pending := make([]string, 0, len(timeline.Words))
	for _, word := range timeline.Words {
		switch {
		case word.EndMS <= playedMS:
			spoken = append(spoken, word.Text)
		case word.StartMS < playedMS && mark.Cut == "":
			mark.Cut = word.Text
			pending = append(pending, word.Text)
		default:
			pending = append(pending, word.Text)
		}
	}
	mark.Spoken = strings.Join(spoken, " ")
	mark.Pending = strings.Join(pending, " ")
	return mark
}

// Distribute splits one utterance's mark back across the separate pieces of
// text that were joined to make it.
//
// One utterance may carry several assistant items: a turn committed as two
// sentences is two items in the trajectory and one thing to say out loud. The
// cut is a fact about the audio, so it is discovered once, for the utterance,
// and then has to be recorded against each item it covers - otherwise an item
// that was fully spoken and an item that was never reached are both marked with
// whatever happened to the utterance as a whole.
//
// Matching is by word count rather than by text search, because the same words
// recur inside one turn constantly - a count-out-loud turn is nothing but
// repeated shapes - and a search would attach the boundary to the first
// occurrence rather than to the right one.
func Distribute(mark Mark, pieces []string) []Mark {
	marks := make([]Mark, len(pieces))
	heard := len(strings.Fields(mark.Spoken))
	cut := strings.TrimSpace(mark.Cut) != ""
	for index, piece := range pieces {
		words := strings.Fields(piece)
		part := Mark{Measured: mark.Measured, PlayedMS: mark.PlayedMS}
		switch {
		case heard >= len(words):
			part.Spoken = strings.Join(words, " ")
			heard -= len(words)
		default:
			part.Spoken = strings.Join(words[:heard], " ")
			remaining := words[heard:]
			if cut && len(remaining) > 0 {
				part.Cut = remaining[0]
				cut = false
			}
			part.Pending = strings.Join(remaining, " ")
			heard = 0
		}
		marks[index] = part
	}
	return marks
}

// Words splits text the way a Timeline holds it: on whitespace, keeping
// everything else. Punctuation stays attached to the word it belongs to so
// that rejoining a prefix produces text somebody could read out.
func Words(text string) []string { return strings.Fields(text) }

// weigh is how long a word is expected to take to say, relative to the others
// in the same utterance.
//
// Letters and digits carry the sound; punctuation carries a pause rather than a
// sound, and the constant carries the gap between one word and the next. None
// of this is a phonetic model and it is not meant to be: it distributes a known
// duration over known words, and the error it makes is bounded by the length of
// one word.
func weigh(word string) int {
	weight := 2
	for _, character := range word {
		switch {
		case unicode.IsLetter(character) || unicode.IsDigit(character):
			weight += 2
		case character == ',' || character == ';' || character == ':':
			weight += 3
		case character == '.' || character == '?' || character == '!':
			weight += 4
		}
	}
	return weight
}
