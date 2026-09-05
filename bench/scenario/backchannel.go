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
	Line                 int            `json:"line"`
	FromMS               int            `json:"from_ms"`
	TriggerStartMS       int            `json:"trigger_start_ms"`
	TriggerEndMS         int            `json:"trigger_end_ms"`
	ToMS                 int            `json:"to_ms"`
	BeforeActiveMS       float64        `json:"before_active_ms"`
	DuringActiveMS       float64        `json:"during_active_ms"`
	AfterActiveMS        float64        `json:"after_active_ms"`
	LongestGapMS         float64        `json:"longest_gap_ms"`
	GapLimitMS           int            `json:"gap_limit_ms"`
	ResponseEvidence     string         `json:"response_evidence,omitempty"`
	UnattributedActiveMS float64        `json:"unattributed_active_ms,omitempty"`
	Responses            []HoldResponse `json:"responses,omitempty"`
}

// HoldResponse records the terminal status of one response whose audio packets
// overlap the measured playout window. A completed status describes protocol
// completion, not semantic completeness. A cancelled status does not by itself
// prove that this acknowledgement caused cancellation. An aborted response
// cannot earn hold credit when its terminal event is at or before the end of
// the measured window, even if another response supplies continuous audio.
type HoldResponse struct {
	ResponseID   string  `json:"response_id"`
	AudioFromMS  float64 `json:"audio_from_ms"`
	AudioToMS    float64 `json:"audio_to_ms"`
	Status       string  `json:"status"`
	TerminalAtMS float64 `json:"terminal_at_ms,omitempty"`
	Reason       string  `json:"reason,omitempty"`
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
	samples, problem := holdWindowSamples(measurement, capture)
	if problem != "" {
		return measurement, problem
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
		return measurement, fmt.Sprintf("insufficient continuation after acknowledgement on line %d: %.0fms active afterwards (%s)", check.Line, measurement.AfterActiveMS, check.Note)
	}
	if measurement.LongestGapMS > float64(check.MaxGapMS) {
		return measurement, fmt.Sprintf("paused %.0fms across acknowledgement on line %d, beyond %dms (%s)", measurement.LongestGapMS, check.Line, check.MaxGapMS, check.Note)
	}
	return measurement, ""
}

// holdWindowSamples uses the same sample grid as the retained stereo WAV.
// Callers must have validated the bounded measurement window first.
func holdWindowSamples(measurement HoldMeasurement, capture *bench.SessionAudioCapture) ([]int16, string) {
	from, to := measurement.FromMS, measurement.ToMS
	if capture == nil {
		return nil, "NOT VERIFIED: holding through an acknowledgement requires captured agent audio"
	}
	if capture.SampleRateHz != 24_000 {
		return nil, "NOT VERIFIED: held-across audio must be the harness's 24 kHz PCM capture"
	}
	previousEnd := 0.0
	for _, chunk := range capture.Agent {
		if math.IsNaN(chunk.AtMS) || math.IsInf(chunk.AtMS, 0) || chunk.AtMS < previousEnd {
			return nil, "NOT VERIFIED: held-across audio has invalid or overlapping playout positions"
		}
		previousEnd = chunk.AtMS + float64(len(chunk.PCM16))/24
		if math.IsInf(previousEnd, 0) || previousEnd > float64(math.MaxInt/24) {
			return nil, "NOT VERIFIED: held-across audio exceeds the supported playout clock"
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
	return samples, ""
}

// heldResponseContinuity supplements the acoustic check. It does not infer a
// cancellation cause or require one wire response for the whole explanation:
// naturally completed speech segments may cross the acknowledgement boundary.
func heldResponseContinuity(measurement *HoldMeasurement, transcript bench.Transcript, capture *bench.SessionAudioCapture) string {
	if measurement.ResponseEvidence != "recorded" {
		return fmt.Sprintf("NOT VERIFIED: response evidence is %s across acknowledgement on line %d", measurement.ResponseEvidence, measurement.Line)
	}
	for _, response := range measurement.Responses {
		switch response.Status {
		case "completed":
		case "cancelled", "failed", "incomplete":
			// A later intentional interruption must not fail an earlier hold.
			// Earlier terminal events still matter when queued audio plays in
			// this window: more audible bytes do not undo an aborted response.
			if response.TerminalAtMS <= float64(measurement.ToMS) {
				return fmt.Sprintf("response %q was %s at %.0fms before the end of acknowledgement hold on line %d; continuous replacement audio cannot establish turn preservation", response.ResponseID, response.Status, response.TerminalAtMS, measurement.Line)
			}
		default:
			return fmt.Sprintf("NOT VERIFIED: response %q has %s terminal evidence across acknowledgement on line %d", response.ResponseID, response.Status, measurement.Line)
		}
	}
	// A completed packet elsewhere in the window must not certify unattributed
	// speech. Reopened WAVs include silence between packets, so require the
	// nonzero samples of acoustically active frames, not padded silence, to be
	// covered by the response-to-playout join. Use sample rounding throughout.
	samples, problem := holdWindowSamples(*measurement, capture)
	if problem != "" {
		return problem
	}
	attributed := make([]bool, len(samples))
	for _, moment := range transcript.Moments {
		if moment.Kind != bench.MomentAgentAudio || moment.ResponseID == "" {
			continue
		}
		from, to := moment.PlayoutAtMS, moment.PlayoutAtMS+moment.AudioMS
		if math.IsNaN(from) || math.IsNaN(to) || math.IsInf(to, 0) || from < 0 || to <= from ||
			to <= float64(measurement.FromMS) || from >= float64(measurement.ToMS) {
			continue
		}
		begin := int(math.Round((max(from, float64(measurement.FromMS)) - float64(measurement.FromMS)) * 24))
		end := int(math.Round((min(to, float64(measurement.ToMS)) - float64(measurement.FromMS)) * 24))
		for index := begin; index < end; index++ {
			attributed[index] = true
		}
	}
	missing := 0
	for offset := 0; offset < len(samples); offset += holdFrameMS * 24 {
		end := min(offset+holdFrameMS*24, len(samples))
		energy := 0.0
		for _, sample := range samples[offset:end] {
			energy += float64(sample) * float64(sample)
		}
		if energy < float64(end-offset)*holdMinimumRMS*holdMinimumRMS {
			continue
		}
		for index := offset; index < end; index++ {
			if samples[index] != 0 && !attributed[index] {
				missing++
			}
		}
	}
	measurement.UnattributedActiveMS = float64(missing) / 24
	if missing > 0 {
		return fmt.Sprintf("NOT VERIFIED: %.3fms of active audio is not attributed to a response across acknowledgement on line %d", measurement.UnattributedActiveMS, measurement.Line)
	}
	return ""
}

func retainHoldResponses(measurement *HoldMeasurement, transcript bench.Transcript) {
	measurement.ResponseEvidence = "unavailable"
	if measurement.ToMS <= measurement.FromMS {
		return
	}
	indices := make(map[string]int)
	missing := false
	for _, moment := range transcript.Moments {
		if moment.Kind != bench.MomentAgentAudio {
			continue
		}
		from, to := moment.PlayoutAtMS, moment.PlayoutAtMS+moment.AudioMS
		if math.IsNaN(from) || math.IsNaN(to) || math.IsInf(to, 0) || from < 0 || to <= from {
			missing = true
			continue
		}
		if to <= float64(measurement.FromMS) || from >= float64(measurement.ToMS) {
			continue
		}
		if moment.ResponseID == "" {
			// Older recordings cannot establish a response-to-playout join.
			missing = true
			continue
		}
		index, found := indices[moment.ResponseID]
		if !found {
			index = len(measurement.Responses)
			indices[moment.ResponseID] = index
			measurement.Responses = append(measurement.Responses, HoldResponse{
				ResponseID: moment.ResponseID, AudioFromMS: from, AudioToMS: to, Status: "unobserved",
			})
		} else {
			response := &measurement.Responses[index]
			response.AudioFromMS = min(response.AudioFromMS, from)
			response.AudioToMS = max(response.AudioToMS, to)
		}
	}
	seen := make(map[string]bool)
	for _, moment := range transcript.Moments {
		index, found := indices[moment.ResponseID]
		if moment.Kind != bench.MomentResponseDone || !found {
			continue
		}
		response := &measurement.Responses[index]
		if seen[moment.ResponseID] {
			response.Status, response.Reason, response.TerminalAtMS = "ambiguous", "", 0
			continue
		}
		seen[moment.ResponseID] = true
		if math.IsNaN(moment.AtMS) || math.IsInf(moment.AtMS, 0) || moment.AtMS < 0 {
			response.Status = "unrecognized"
			continue
		}
		switch moment.ResponseStatus {
		case "completed", "cancelled", "failed", "incomplete":
			response.Status, response.Reason, response.TerminalAtMS = moment.ResponseStatus, moment.ResponseStatusReason, moment.AtMS
		default:
			response.Status = "unrecognized"
		}
	}
	if len(measurement.Responses) > 0 {
		measurement.ResponseEvidence = "recorded"
		if missing {
			measurement.ResponseEvidence = "partial"
		}
	}
}

func overlapMS(start, end, from, to float64) float64 {
	return max(0, min(end, to)-max(start, from))
}
