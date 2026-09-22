package capability

import (
	"fmt"
	"strings"
	"time"
)

// The annotated diagnostic's baseline schedule.
//
// These are the release times a fixture is authored against. They are not
// claims about any particular recogniser: they are the reference the delay
// sweep moves away from, chosen to match what the host's own streaming stack
// does so that an annotated run is not accidentally more responsive than
// anything that could be built.
const (
	// wordPace is how long one word of ordinary conversational speech takes.
	wordPace = 320 * time.Millisecond
	// recognitionDelay is how long after a word finishes before a recogniser
	// has committed it. It matches the 480 ms Voxtral realtime delay the
	// duplex-plan profiles declare.
	recognitionDelay = 480 * time.Millisecond
	// cueDelay is how long after a phrase ends before an annotation of how
	// it was said is available. A cue describing a whole phrase cannot be
	// had while the phrase is still being said.
	cueDelay = 200 * time.Millisecond
)

// script lays out one stretch of conversation in source time and derives each
// event's availability from it, so a fixture states when things were said and
// never has to restate when they could be known.
type script struct {
	events []Event
	clock  time.Duration
	seq    int
}

func newScript(from time.Duration) *script { return &script{clock: from} }

// at moves the clock to an absolute moment.
func (s *script) at(when time.Duration) *script { s.clock = when; return s }

// gap leaves silence. Nothing is emitted for it: silence is what the absence
// of sound events means, and a cell without the sound channel genuinely
// cannot tell it from a recogniser that has said nothing.
func (s *script) gap(how time.Duration) *script { s.clock += how; return s }

// say lays one phrase down from the current clock and advances past it.
//
// The phrase becomes one sound event covering it, one word event per word,
// and, when a cue is given, one annotation available once the phrase is over.
// They share a group so the phrase can be removed whole for the negative
// control.
func (s *script) say(group, speaker, text string) *script { return s.cue(group, speaker, text, "") }

// cue is say with an acoustic annotation attached to the phrase.
func (s *script) cue(group, speaker, text, annotation string) *script {
	words := strings.Fields(text)
	if len(words) == 0 {
		panic("capability: a phrase with no words")
	}
	start := s.clock
	end := start + time.Duration(len(words))*wordPace
	s.events = append(s.events, Event{
		ID: s.id(group, "sound"), Group: group, Kind: KindSound, Speaker: speaker,
		SourceStart: start, SourceEnd: end, AvailableAt: start,
	})
	for index, word := range words {
		wordStart := start + time.Duration(index)*wordPace
		wordEnd := wordStart + wordPace
		s.events = append(s.events, Event{
			ID: s.id(group, fmt.Sprintf("w%d", index)), Group: group, Kind: KindWord, Speaker: speaker,
			Text: word, SourceStart: wordStart, SourceEnd: wordEnd,
			AvailableAt: wordEnd + recognitionDelay,
		})
	}
	if annotation != "" {
		s.events = append(s.events, Event{
			ID: s.id(group, "cue"), Group: group, Kind: KindCue, Speaker: speaker,
			Cue: annotation, SourceStart: start, SourceEnd: end,
			AvailableAt: end + cueDelay,
		})
	}
	s.clock = end
	return s
}

func (s *script) id(group, suffix string) string {
	s.seq++
	return fmt.Sprintf("%s-%s", group, suffix)
}

// lastWord is the event ID of the final word laid down, which is what an
// expectation opens its window after: the moment the feedback was complete
// and available, not the moment it started.
func (s *script) lastWord() string {
	for index := len(s.events) - 1; index >= 0; index-- {
		if s.events[index].Kind == KindWord {
			return s.events[index].ID
		}
	}
	return ""
}

// ends is where the clock has reached.
func (s *script) ends() time.Duration { return s.clock }

func (s *script) done() []Event { return s.events }
