package spoken

import "strings"

// PriorMSPerWeight is how long one unit of weigh() is assumed to take before
// the audio has proved otherwise.
//
// Calibrated against the synthesisers this ships with: an English word averages
// about eleven units and about three hundred and fifty milliseconds, which is
// roughly a hundred and fifty words a minute. It is deliberately a little slow.
// The two ways of being wrong here are not symmetrical - a layout that runs
// fast reports words as heard that nobody heard, and the agent then carries on
// past them, which is the exact failure this package exists to remove, while a
// layout that runs slow has the agent repeat a word somebody already heard,
// which is what a person does anyway when they are interrupted.
const PriorMSPerWeight = 33

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
// audioMS is how much audio exists so far, which while synthesis is running is
// not how much there will be. That distinction is the whole of this function's
// difficulty. A synthesiser is paced to realtime by the planner that consumes
// it, so "the audio produced so far" tracks "the audio played so far" almost
// exactly - and laying the text out over it would report every utterance as
// nearly finished at every moment of its life. So the layout is made over
// whichever is longer, the prior or the audio plus one prior word, and the
// second term is what keeps the last word from ever landing inside audio that
// has not been produced yet.
//
// Complete is what replaces all of this with the truth once there is one.
func Estimate(text string, audioMS uint64) Timeline {
	words := Words(text)
	timeline := layout(text, words, ExpectedSpan(text, audioMS))
	timeline.AudioMS = audioMS
	return timeline
}

// ExpectedSpan is how long an utterance whose synthesis is still running is
// believed to run in total.
//
// It is the prior, or the audio that already exists plus one prior word,
// whichever is longer. The second term carries the invariant that matters:
// audio nobody has produced cannot have been played, so while more is coming
// the last word of the utterance must sit past everything that exists.
func ExpectedSpan(text string, audioMS uint64) uint64 {
	words := Words(text)
	prior := PriorDuration(words)
	if len(words) == 0 {
		if audioMS > prior {
			return audioMS
		}
		return prior
	}
	if headroom := audioMS + prior/uint64(len(words)); headroom > prior {
		return headroom
	}
	return prior
}

// Complete rebuilds a layout once the utterance's real duration is known.
//
// Until synthesis ends, the duration is a guess with headroom in it, and a
// layout still carrying that headroom reports a completed utterance as
// unfinished - the agent would resume mid-sentence after saying the whole
// thing. This is the moment the guess is replaced, and it matters whether or
// not anything ever listened to the audio.
//
// A measured layout is rescaled rather than thrown away: its anchors came from
// the audio and are the best information there is about where the words are.
// Only the tail that was extrapolated past the audio is moved, and it is moved
// to end exactly where the audio does.
func (timeline Timeline) Complete(audioMS uint64) Timeline {
	if audioMS == 0 || len(timeline.Words) == 0 {
		return timeline
	}
	if !timeline.Measured {
		completed := layout(timeline.Text, Words(timeline.Text), audioMS)
		completed.AudioMS = audioMS
		return completed
	}
	completed := timeline
	completed.AudioMS = audioMS
	completed.Words = append([]Word(nil), timeline.Words...)
	last := completed.Words[len(completed.Words)-1]
	if last.EndMS == audioMS {
		return completed
	}
	// Everything from the first word that runs past the audio is redistributed
	// over what is left of it. Words wholly inside the audio keep the times
	// something actually measured.
	first := len(completed.Words)
	for index, word := range completed.Words {
		if word.EndMS > audioMS {
			first = index
			break
		}
	}
	if first >= len(completed.Words) {
		// The audio outlasted every word, which means the tail was cut short
		// rather than overrun. Stretch the last word to the end so a finished
		// utterance reads as finished.
		completed.Words[len(completed.Words)-1].EndMS = audioMS
		return completed
	}
	from := uint64(0)
	if first > 0 {
		from = completed.Words[first-1].EndMS
	}
	if from > audioMS {
		from = audioMS
	}
	tail := completed.Words[first:]
	texts := make([]string, 0, len(tail))
	for _, word := range tail {
		texts = append(texts, word.Text)
	}
	spread(tail, texts, from, audioMS)
	return completed
}

// PriorDuration is how long these words are expected to take before anything
// has measured them.
func PriorDuration(words []string) uint64 {
	total := 0
	for _, word := range words {
		total += weigh(word)
	}
	return uint64(total * PriorMSPerWeight)
}

// layout distributes a span across words in proportion to how long each is
// expected to take.
func layout(text string, words []string, span uint64) Timeline {
	timeline := Timeline{Text: strings.TrimSpace(text)}
	if len(words) == 0 {
		return timeline
	}
	timeline.Words = make([]Word, len(words))
	for index, word := range words {
		timeline.Words[index].Text = word
	}
	spread(timeline.Words, words, 0, span)
	return timeline
}

// spread lays a run of words across an interval by weight.
func spread(into []Word, words []string, from, to uint64) {
	if len(into) == 0 {
		return
	}
	if to < from {
		to = from
	}
	total := 0
	for _, word := range words {
		total += weigh(word)
	}
	if total == 0 {
		for index := range into {
			into[index].StartMS, into[index].EndMS = from, to
		}
		return
	}
	covered := 0
	width := to - from
	for index, word := range words {
		into[index].StartMS = from + uint64(covered)*width/uint64(total)
		covered += weigh(word)
		into[index].EndMS = from + uint64(covered)*width/uint64(total)
	}
}
