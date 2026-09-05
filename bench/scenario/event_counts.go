package scenario

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"

	"github.com/bojieli/OpenRealtime/bench"
)

// CountEvent assigns one expected English count (or recognized digit) to a
// source line. WithinMS extends the response allowance past that line's end.
type CountEvent struct {
	Line     int
	Number   int
	WithinMS int
}

// EventCountObservation preserves the external recognition verbatim. A failed
// recognition is not rewritten using generated text or an expected number.
type EventCountObservation struct {
	Line        int     `json:"line"`
	Number      int     `json:"number"`
	FromMS      int     `json:"from_ms"`
	ToMS        int     `json:"to_ms"`
	ActiveMS    float64 `json:"active_ms"`
	HeardFromMS int     `json:"heard_from_ms"`
	HeardToMS   int     `json:"heard_to_ms"`
	Heard       string  `json:"heard,omitempty"`
	Error       string  `json:"error,omitempty"`
}

type EventCountMeasurement struct {
	Events          []EventCountObservation `json:"events"`
	OutsideActiveMS float64                 `json:"outside_active_ms"`
}

func eventCounts(check Check, timeline Timeline, listen heard, capture *bench.SessionAudioCapture) (EventCountMeasurement, string) {
	measurement := EventCountMeasurement{}
	invalid := func() (EventCountMeasurement, string) {
		return measurement, "invalid scenario check: event-count windows exceed or reverse the recorded timeline"
	}
	if err := validateCheck(check, timeline); err != nil {
		return measurement, "invalid scenario check: " + err.Error()
	}
	if timeline.TotalMS <= 0 || timeline.TotalMS > maximumReplayMS {
		return invalid()
	}
	recordedEnd := timeline.TotalMS
	if capture != nil {
		if len(capture.RoomPCM16) > maximumReplaySamples {
			return invalid()
		}
		recordedEnd = max(recordedEnd, (len(capture.RoomPCM16)+23)/24)
		for _, chunk := range capture.Agent {
			end := chunk.AtMS + float64(len(chunk.PCM16))/24
			if math.IsNaN(end) || math.IsInf(end, 0) || end < 0 || end > maximumReplayMS {
				return invalid()
			}
			recordedEnd = max(recordedEnd, int(math.Ceil(end)))
		}
	}
	for index, event := range check.Events {
		span := timeline.Spans[event.Line]
		if span.StartMS < 0 || span.EndMS <= span.StartMS || span.EndMS > timeline.TotalMS || !canAddMS(span.EndMS, event.WithinMS) {
			return invalid()
		}
		end := min(span.EndMS+event.WithinMS, recordedEnd)
		if index+1 < len(check.Events) {
			if timeline.Spans[check.Events[index+1].Line].StartMS < span.EndMS {
				return invalid()
			}
			// The next occurrence starts a new count; its audio may never satisfy
			// the previous one, even when their latency allowances overlap.
			end = min(end, timeline.Spans[check.Events[index+1].Line].StartMS)
		}
		if end <= span.StartMS {
			return invalid()
		}
		measurement.Events = append(measurement.Events, EventCountObservation{Line: event.Line, Number: event.Number, FromMS: span.StartMS, ToMS: end})
	}
	// Validate the complete recording before any provider calls. Missing or
	// malformed waveform evidence cannot be rescued by a confident recognizer.
	if _, problem := holdWindowSamples(HoldMeasurement{FromMS: 0, ToMS: recordedEnd}, capture); problem != "" {
		return measurement, "NOT VERIFIED: event counts require a valid captured agent waveform"
	}
	var failures []string
	last := 0
	for index := range measurement.Events {
		observation := &measurement.Events[index]
		quiet, _ := countActivity(last, observation.FromMS, capture)
		measurement.OutsideActiveMS += quiet
		last = observation.ToMS
		samples := agentAudioBetween(*capture, observation.FromMS, observation.ToMS)
		first, final := len(samples), 0
		for start := 0; start < len(samples); start += holdFrameMS * 24 {
			end := min(start+holdFrameMS*24, len(samples))
			energy := 0.0
			for _, sample := range samples[start:end] {
				energy += float64(sample) * float64(sample)
			}
			if energy >= float64(end-start)*holdMinimumRMS*holdMinimumRMS {
				observation.ActiveMS += float64(end-start) / 24
				first, final = min(first, start), end
			}
		}
		if observation.ActiveMS <= audibleMS {
			failures = append(failures, fmt.Sprintf("count %d has only %.0fms of audible activity", observation.Number, observation.ActiveMS))
			continue
		}
		// Trim only leading/trailing silence. Preserve every interior gap and
		// speech island, including extra words and repeated counts.
		observation.HeardFromMS = max(observation.FromMS, observation.FromMS+first/24-200)
		observation.HeardToMS = min(observation.ToMS, observation.FromMS+(final+23)/24+200)
		if listen == nil {
			observation.Error = "independent recognizer is unavailable"
		} else {
			var err error
			observation.Heard, err = listen(observation.HeardFromMS, observation.HeardToMS)
			if err != nil {
				observation.Error = err.Error()
			}
		}
		if observation.Error != "" {
			failures = append(failures, fmt.Sprintf("NOT VERIFIED: count %d: %s", observation.Number, observation.Error))
		} else if !exactCount(observation.Heard, observation.Number) {
			failures = append(failures, fmt.Sprintf("audible count %d not established: independently heard %q; require exactly the count with no extra words", observation.Number, truncateSaid(observation.Heard)))
		}
	}
	quiet, _ := countActivity(last, recordedEnd, capture)
	measurement.OutsideActiveMS += quiet
	if measurement.OutsideActiveMS > audibleMS {
		failures = append(failures, fmt.Sprintf("said something outside the count windows: %.0fms active", measurement.OutsideActiveMS))
	}
	return measurement, strings.Join(failures, "; ")
}

func exactCount(text string, number int) bool {
	// Punctuation and case do not change a spoken number. All other tokens,
	// including homophones, fillers, repeated digits and reference text, remain
	// evidence of a mismatch or ambiguous recognition, never a quiet pass.
	previous := rune(0)
	for _, r := range text {
		if unicode.IsDigit(r) && (previous == '-' || previous == '+' || previous == '−') {
			return false
		}
		if !unicode.IsSpace(r) {
			previous = r
		}
	}
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) })
	normalized := strings.Join(words, " ")
	return normalized == strconv.Itoa(number) || normalized == countWords(number)
}

func countWords(number int) string {
	if number < 20 {
		return []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}[number]
	}
	word := []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty", "seventy", "eighty", "ninety"}[number/10]
	if number%10 != 0 {
		word += " " + countWords(number%10)
	}
	return word
}
