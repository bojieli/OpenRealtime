package scenario

import (
	"fmt"

	"github.com/bojieli/OpenRealtime/bench"
)

// ResumeMeasurement retains the observations that distinguish a sustained
// interrupted count from short answers or an already-silent agent. Numbers
// come from the independent recognizer; activity uses the captured PCM grid.
type ResumeMeasurement struct {
	Line           int     `json:"line"`
	Interrupted    int     `json:"interrupted"`
	BeforeNumbers  []int   `json:"before_numbers,omitempty"`
	AfterNumbers   []int   `json:"after_numbers,omitempty"`
	RecentFromMS   int     `json:"recent_from_ms"`
	RecentToMS     int     `json:"recent_to_ms"`
	RecentActiveMS float64 `json:"recent_active_ms"`
}

func resumedAcross(check Check, timeline Timeline, listen heard, capture *bench.SessionAudioCapture) (ResumeMeasurement, string) {
	measurement := ResumeMeasurement{Line: check.Line, Interrupted: check.Interrupted}
	if err := validateCheck(check, timeline); err != nil {
		return measurement, "invalid scenario check: " + err.Error()
	}
	stop := timeline.Spans[check.Interrupted].StartMS
	resume := timeline.Spans[check.Line].EndMS
	if stop < check.BeforeMS || resume <= stop || !canAddMS(resume, check.AfterMS) ||
		resume+check.AfterMS > timeline.TotalMS || resume+check.AfterMS > maximumReplayMS {
		return measurement, "invalid scenario check: interrupted-count windows exceed or reverse the playback timeline"
	}
	measurement.RecentFromMS, measurement.RecentToMS = stop-check.BeforeMS, stop
	var observed heard
	if listen != nil {
		observed = func(from, to int) (string, error) {
			text, err := listen(from, to)
			if err == nil {
				if from == 0 {
					measurement.BeforeNumbers = numbersIn(text)
				} else {
					measurement.AfterNumbers = numbersIn(text)
				}
			}
			return text, err
		}
	}
	countFailure := resumed(check, timeline, observed)
	// Reuse the exact sample-grid extraction and activity threshold used by
	// acknowledgement scoring. Transported silence is not speech opportunity.
	samples, problem := holdWindowSamples(HoldMeasurement{FromMS: stop - check.BeforeMS, ToMS: stop}, capture)
	if problem == "" {
		for start := 0; start < len(samples); start += holdFrameMS * 24 {
			end := min(start+holdFrameMS*24, len(samples))
			energy := 0.0
			for _, sample := range samples[start:end] {
				energy += float64(sample) * float64(sample)
			}
			if energy >= float64(end-start)*holdMinimumRMS*holdMinimumRMS {
				measurement.RecentActiveMS += float64(end-start) / 24
			}
		}
	}
	if countFailure != "" {
		return measurement, countFailure
	}
	if problem != "" {
		return measurement, "NOT VERIFIED: interrupted counting requires a valid captured agent waveform"
	}
	if measurement.RecentActiveMS <= audibleMS {
		return measurement, fmt.Sprintf("no audible interruption opportunity: only %.0fms active in the %dms before interruption (%s)", measurement.RecentActiveMS, check.BeforeMS, check.Note)
	}
	return measurement, ""
}
