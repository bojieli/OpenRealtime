package scenario

import (
	"fmt"
	"math"

	"github.com/bojieli/OpenRealtime/bench"
)

const (
	holdFrameMS         = 20
	holdMinimumRMS      = 128 // About -48 dBFS; digital silence and low-level dither are not speech.
	holdMaximumWindowMS = 120_000
)

// HoldMeasurement retains the activity evidence for one acknowledgement.
// Durations concern the agent channel on the recorded playout clock, never
// text events or the arrival time of a prefetched audio packet. Activity is
// an acoustic proxy, not proof that the same semantic explanation continued.
type HoldMeasurement struct {
	Line           int     `json:"line"`
	FromMS         int     `json:"from_ms"`
	TriggerStartMS int     `json:"trigger_start_ms"`
	TriggerEndMS   int     `json:"trigger_end_ms"`
	ToMS           int     `json:"to_ms"`
	BeforeActiveMS float64 `json:"before_active_ms"`
	DuringActiveMS float64 `json:"during_active_ms"`
	AfterActiveMS  float64 `json:"after_active_ms"`
	LongestGapMS   float64 `json:"longest_gap_ms"`
	GapLimitMS     int     `json:"gap_limit_ms"`
}

func heldAcross(check Check, timeline Timeline, capture *bench.SessionAudioCapture) (HoldMeasurement, string) {
	measurement := HoldMeasurement{Line: check.Line, GapLimitMS: check.MaxGapMS}
	if err := validateCheck(check, timeline); err != nil {
		return measurement, "invalid scenario check: " + err.Error()
	}
	span := timeline.Spans[check.Line]
	if span.StartMS < check.BeforeMS {
		return measurement, "invalid scenario check: held-across lookback precedes the recording"
	}
	from, to := span.StartMS-check.BeforeMS, span.EndMS+check.AfterMS
	if to > timeline.TotalMS || to-from > holdMaximumWindowMS {
		return measurement, "invalid scenario check: held-across window exceeds the recording or two minutes"
	}
	measurement.FromMS, measurement.ToMS = from, to
	measurement.TriggerStartMS, measurement.TriggerEndMS = span.StartMS, span.EndMS
	if capture == nil {
		return measurement, "NOT VERIFIED: holding through an acknowledgement requires captured agent audio"
	}
	if capture.SampleRateHz != 24_000 {
		return measurement, "NOT VERIFIED: held-across audio must be the harness's 24 kHz PCM capture"
	}
	previousEnd := 0.0
	for _, chunk := range capture.Agent {
		if math.IsNaN(chunk.AtMS) || math.IsInf(chunk.AtMS, 0) || chunk.AtMS < previousEnd {
			return measurement, "NOT VERIFIED: held-across audio has invalid or overlapping playout positions"
		}
		previousEnd = chunk.AtMS + float64(len(chunk.PCM16))/24
		if math.IsInf(previousEnd, 0) || previousEnd > float64(math.MaxInt/24) {
			return measurement, "NOT VERIFIED: held-across audio exceeds the supported playout clock"
		}
	}
	// Clip to this short window before converting positions to sample offsets.
	// A distant valid chunk must not overflow integer arithmetic or influence
	// activity here. Round to the same 24 kHz sample grid as retained media.
	samples := make([]int16, (to-from)*24)
	for _, chunk := range capture.Agent {
		chunkEnd := chunk.AtMS + float64(len(chunk.PCM16))/24
		if chunk.AtMS >= float64(to) || chunkEnd <= float64(from) {
			continue
		}
		offset := int(math.Round((chunk.AtMS - float64(from)) * 24))
		begin, end := max(0, -offset), min(len(chunk.PCM16), len(samples)-offset)
		if begin < end {
			copy(samples[offset+begin:offset+end], chunk.PCM16[begin:end])
		}
	}
	const frameSamples = holdFrameMS * 24
	lastActiveEnd := -1.0
	for offset := 0; offset < len(samples); offset += frameSamples {
		end := min(offset+frameSamples, len(samples))
		energy := 0.0
		for _, sample := range samples[offset:end] {
			energy += float64(sample) * float64(sample)
		}
		if energy < float64(end-offset)*holdMinimumRMS*holdMinimumRMS {
			continue
		}
		startMS, endMS := float64(from)+float64(offset)/24, float64(from)+float64(end)/24
		measurement.BeforeActiveMS += overlapMS(startMS, endMS, float64(from), float64(span.StartMS))
		measurement.DuringActiveMS += overlapMS(startMS, endMS, float64(span.StartMS), float64(span.EndMS))
		measurement.AfterActiveMS += overlapMS(startMS, endMS, float64(span.EndMS), float64(to))
		if lastActiveEnd >= 0 {
			measurement.LongestGapMS = max(measurement.LongestGapMS, startMS-lastActiveEnd)
		}
		lastActiveEnd = endMS
	}
	if measurement.BeforeActiveMS <= audibleMS {
		return measurement, fmt.Sprintf("not speaking before acknowledgement on line %d: %.0fms active (%s)", check.Line, measurement.BeforeActiveMS, check.Note)
	}
	if measurement.DuringActiveMS <= min(float64(audibleMS), float64(span.EndMS-span.StartMS)/2) {
		return measurement, fmt.Sprintf("did not speak through acknowledgement on line %d: %.0fms active (%s)", check.Line, measurement.DuringActiveMS, check.Note)
	}
	if measurement.AfterActiveMS <= audibleMS {
		return measurement, fmt.Sprintf("stopped at acknowledgement on line %d: %.0fms active afterwards (%s)", check.Line, measurement.AfterActiveMS, check.Note)
	}
	if measurement.LongestGapMS > float64(check.MaxGapMS) {
		return measurement, fmt.Sprintf("paused %.0fms across acknowledgement on line %d, beyond %dms (%s)", measurement.LongestGapMS, check.Line, check.MaxGapMS, check.Note)
	}
	return measurement, ""
}

func overlapMS(start, end, from, to float64) float64 {
	return max(0, min(end, to)-max(start, from))
}
