package scenario

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
)

// Ears transcribes the agent's own audio.
//
// It exists for one kind of claim, and that kind is worth naming, because most
// of this suite does not need it. Every other check here asks what the agent
// said, and the wire already answers that: the transcript of a turn is
// delivered with the turn. This asks what the user *heard*, which is a
// different question the moment somebody interrupts - the wire carries the
// whole sentence and the loudspeaker stopped partway through it.
//
// The runtime has its own answer to that question now, and that answer is
// exactly what is under test. Scoring against it would be scoring the runtime
// against itself: a runtime that decided it had said everything would be marked
// correct for carrying on as though it had. So the check is grounded in the
// waveform the harness recorded, transcribed by something that was not involved
// in producing it.
//
// A Voice plug-in that can also listen implements this. One that cannot leaves
// the claim unmade, and a check that needs it fails saying so rather than
// passing quietly.
type Ears interface {
	Hear(ctx context.Context, samples []int16, rateHz int) (string, error)
}

// heard is how a check asks what the agent was audibly saying in a window.
// Nil means nothing can answer, which is a failure rather than a pass.
type heard func(fromMS, toMS int) (string, error)

// agentAudioBetween rebuilds the agent's own output over a window of the
// episode clock.
//
// Silence is preserved rather than squeezed out. A recogniser handed the
// agent's speech with every pause removed hears one run-on utterance and
// punctuates it wherever it likes; handed the gaps, it segments the way the
// agent actually spoke.
func agentAudioBetween(capture bench.SessionAudioCapture, fromMS, toMS int) []int16 {
	rate := int(capture.SampleRateHz)
	if rate == 0 {
		rate = 24_000
	}
	if toMS <= fromMS {
		return nil
	}
	window := make([]int16, (toMS-fromMS)*rate/1000)
	for _, chunk := range capture.Agent {
		offset := (int(chunk.AtMS) - fromMS) * rate / 1000
		for index, sample := range chunk.PCM16 {
			position := offset + index
			if position < 0 {
				continue
			}
			if position >= len(window) {
				break
			}
			window[position] = sample
		}
	}
	return window
}

// hearing returns the accessor a scored run uses, transcribing each window
// once and remembering it.
func hearing(ctx context.Context, ears Ears, capture bench.SessionAudioCapture) heard {
	if ears == nil {
		return nil
	}
	rate := int(capture.SampleRateHz)
	if rate == 0 {
		rate = 24_000
	}
	cache := map[string]string{}
	return func(fromMS, toMS int) (string, error) {
		key := fmt.Sprintf("%d-%d", fromMS, toMS)
		if text, known := cache[key]; known {
			return text, nil
		}
		samples := agentAudioBetween(capture, fromMS, toMS)
		if len(samples) == 0 {
			cache[key] = ""
			return "", nil
		}
		text, err := ears.Hear(ctx, samples, rate)
		if err != nil {
			return "", err
		}
		cache[key] = text
		return text, nil
	}
}

// numbersIn pulls the counting sequence out of a transcript.
//
// Both spellings, because which one arrives is the recogniser's decision rather
// than the agent's: the same "twenty-one" comes back as "21" from one model and
// as two words from another, and a check that understood only one of them would
// be measuring the recogniser.
func numbersIn(text string) []int {
	words := strings.FieldsFunc(strings.ToLower(text), func(character rune) bool {
		return !('a' <= character && character <= 'z') && !('0' <= character && character <= '9')
	})
	var numbers []int
	pending, open := 0, false
	flush := func() {
		if open {
			numbers = append(numbers, pending)
		}
		pending, open = 0, false
	}
	for _, word := range words {
		if value, err := strconv.Atoi(word); err == nil {
			flush()
			numbers = append(numbers, value)
			continue
		}
		if value, ok := smallNumbers[word]; ok {
			// "twenty" then "one" is one number; "one" then "two" is two.
			if open && pending%10 == 0 && pending >= 20 && value < 10 {
				pending += value
				flush()
				continue
			}
			flush()
			pending, open = value, true
			continue
		}
		flush()
	}
	flush()
	return numbers
}

var smallNumbers = map[string]int{
	"zero": 0, "oh": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
	"twelve": 12, "thirteen": 13, "fourteen": 14, "fifteen": 15, "sixteen": 16,
	"seventeen": 17, "eighteen": 18, "nineteen": 19, "twenty": 20, "thirty": 30,
	"forty": 40, "fourty": 40, "fifty": 50, "sixty": 60, "seventy": 70,
	"eighty": 80, "ninety": 90,
}

// runsOn reports whether numbers continue a count without a gap. A recogniser
// drops the odd word, so one missing number in the middle is tolerated; two in
// a row is the agent skipping.
func runsOn(numbers []int) (int, bool) {
	if len(numbers) < 2 {
		return 0, true
	}
	for index := 1; index < len(numbers); index++ {
		if step := numbers[index] - numbers[index-1]; step < 1 || step > 2 {
			return index, false
		}
	}
	return 0, true
}

// describeNumbers renders a sequence compactly for a failure message.
func describeNumbers(numbers []int) string {
	if len(numbers) == 0 {
		return "nothing"
	}
	parts := make([]string, 0, len(numbers))
	for _, number := range numbers {
		parts = append(parts, strconv.Itoa(number))
	}
	return strings.Join(parts, " ")
}

// resumed scores the one claim that has to be grounded in the waveform: after
// being cut off and told to carry on, the agent continued from the number the
// user actually heard.
//
// Three failures are being separated, and they are separate because they come
// from different beliefs. Starting again from the beginning is an agent that
// knows it was interrupted and not where. Carrying on past numbers nobody heard
// is an agent that believes it said everything it wrote. Saying nothing at all
// is an agent that lost the thread entirely. Reported as one "wrong number",
// none of them would be diagnosable.
func resumed(check Check, timeline Timeline, listen heard) string {
	if check.Interrupted < 0 || check.Interrupted >= len(timeline.Spans) ||
		check.Line < 0 || check.Line >= len(timeline.Spans) {
		return fmt.Sprintf("the resumed check names lines %d and %d, and the script has %d (%s)",
			check.Interrupted, check.Line, len(timeline.Spans), check.Note)
	}
	if listen == nil {
		// Never a quiet pass. This claim is about what a loudspeaker produced,
		// and with nothing to listen to that recording the claim has not been
		// checked - which is a different thing from having been met.
		return "NOT VERIFIED: continuing from what was actually heard is scored " +
			"against the agent's own recorded audio, and no transcription endpoint " +
			"was configured for the harness (-transcribe-url)"
	}
	before, err := listen(0, timeline.Spans[check.Interrupted].StartMS)
	if err != nil {
		return fmt.Sprintf("NOT VERIFIED: could not transcribe what the agent had said: %v", err)
	}
	after, err := listen(timeline.Spans[check.Line].EndMS,
		timeline.Spans[check.Line].EndMS+check.AfterMS)
	if err != nil {
		return fmt.Sprintf("NOT VERIFIED: could not transcribe what the agent said next: %v", err)
	}

	heardBefore, heardAfter := numbersIn(before), numbersIn(after)
	if len(heardBefore) == 0 {
		return fmt.Sprintf("the agent had counted nothing audible before being stopped, "+
			"so there was nothing to carry on from; it was heard saying %q (%s)",
			truncateSaid(before), check.Note)
	}
	if len(heardAfter) == 0 {
		return fmt.Sprintf("after being told to carry on it counted nothing; it had reached %s "+
			"and was then heard saying %q (%s)",
			describeNumbers(lastFew(heardBefore)), truncateSaid(after), check.Note)
	}
	reached := heardBefore[len(heardBefore)-1]
	next := heardAfter[0]
	switch {
	case next <= 1 && reached > 2:
		return fmt.Sprintf("it had counted to %d and started again from %d: it knew it was "+
			"interrupted and not where (%s)", reached, next, check.Note)
	case next > reached+1:
		return fmt.Sprintf("it had counted to %d and carried on from %d, past %d number(s) "+
			"nobody heard: it continued from where its own text had got to rather than from "+
			"where its voice had (%s)", reached, next, next-reached-1, check.Note)
	case next < reached:
		return fmt.Sprintf("it had counted to %d and went back to %d, repeating %d number(s) "+
			"the user had already heard (%s)", reached, next, reached-next+1, check.Note)
	}
	if at, ordered := runsOn(heardAfter); !ordered {
		return fmt.Sprintf("it carried on from %d and then jumped from %d to %d (%s)",
			next, heardAfter[at-1], heardAfter[at], check.Note)
	}
	return ""
}

// lastFew is the tail of a count, for a failure message that should show where
// the agent had got to without printing the whole sequence.
func lastFew(numbers []int) []int {
	if len(numbers) <= 4 {
		return numbers
	}
	return numbers[len(numbers)-4:]
}
