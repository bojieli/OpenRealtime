package spoken

import "strings"

// PriorMSPerWeight is how long one unit of weigh() is assumed to take before
// anything has been measured.
//
// It exists for one window and no other: the moment between the first audio
// frame going out and synthesis finishing, when the total duration of the
// utterance is not yet known. Estimating against the audio produced so far in
// that window would say the utterance is nearly over every time, because the
// audio produced so far is all the audio there is so far.
//
// Calibrated against the synthesisers this ships with, which speak at roughly
// a hundred and sixty words a minute. It is a floor on the expected duration
// rather than a claim about it: the moment the real duration is longer, the
// real duration is used, and the moment the audio is measured this is gone.
const PriorMSPerWeight = 9

// Estimate lays text out over a duration proportionally.
//
// It is the fallback, and it is a real one rather than a placeholder. A
// synthesiser speaks at a fairly even rate within one utterance, so
// distributing its duration across the words by length puts each boundary
// within about a word of where it belongs - which is the resolution the
// decisions downstream actually need. What it cannot do is notice that the
// voice paused, hesitated, or read a number as three words, and that is why
// Measured stays false.
//
// audioMS is what is known of the utterance's duration. When synthesis is still
// running that is less than the whole, so the prior supplies a floor: the
// layout is made over whichever is longer, and the words that fall past the
// audio produced so far simply have not been reached yet. Passing zero lays the
// text out over the prior alone, which is what a caller has before any audio
// exists.
func Estimate(text string, audioMS uint64) Timeline {
	words := Words(text)
	timeline := Timeline{Text: strings.TrimSpace(text), AudioMS: audioMS}
	if len(words) == 0 {
		return timeline
	}
	total := 0
	for _, word := range words {
		total += weigh(word)
	}
	if total == 0 {
		return timeline
	}
	span := audioMS
	if prior := uint64(total * PriorMSPerWeight); prior > span {
		span = prior
	}
	timeline.Words = make([]Word, 0, len(words))
	covered := 0
	for _, word := range words {
		start := uint64(covered) * span / uint64(total)
		covered += weigh(word)
		end := uint64(covered) * span / uint64(total)
		timeline.Words = append(timeline.Words, Word{Text: word, StartMS: start, EndMS: end})
	}
	return timeline
}

// Complete rebuilds a timeline once the utterance's real duration is known.
//
// The prior in Estimate is a floor on a duration nobody had yet. Once synthesis
// ends the duration is a fact, and a layout still stretched over the prior
// would report a completed utterance as unfinished - the agent would resume
// mid-sentence after saying the whole thing. Only an estimated layout is
// rebuilt: measured times came from the audio and are not improved by scaling.
func (timeline Timeline) Complete(audioMS uint64) Timeline {
	if timeline.Measured || audioMS == 0 {
		return timeline
	}
	return Estimate(timeline.Text, audioMS)
}
