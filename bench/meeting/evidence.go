package meeting

import (
	"context"
	"errors"

	"github.com/bojieli/OpenRealtime/bench"
)

// EvidenceAttempt identifies one exact Meeting Assistant evaluation before
// any browser, media, or Realtime work begins. Trial is one because the v1
// runner owns a single repetition; an outer migration runner may construct a
// fresh evidence plug-in for each preregistered repetition.
type EvidenceAttempt struct {
	Suite                string                     `json:"suite"`
	Case                 string                     `json:"case"`
	Trial                int                        `json:"trial"`
	Task                 Task                       `json:"task"`
	ExecutionRequirement bench.ExecutionRequirement `json:"execution_requirement,omitempty"`
}

func (attempt EvidenceAttempt) validate() error {
	if attempt.Suite != SuiteName || attempt.Case != attempt.Task.ID || attempt.Trial != 1 {
		return errors.New("meeting evidence attempt does not identify one exact v1 task")
	}
	if err := attempt.Task.Validate(); err != nil {
		return err
	}
	return attempt.ExecutionRequirement.Validate()
}

// EvidenceCompletion is the deterministic result and protocol transcript
// presented to an evidence plug-in after scoring. The benchmark result remains
// authoritative: a secondary reviewer may describe disagreement but cannot
// replace Outcome.Passed.
type EvidenceCompletion struct {
	Attempt    EvidenceAttempt
	Outcome    bench.TaskOutcome
	Transcript bench.Transcript
}

// AttemptEvidence receives exact session media and then one terminal outcome.
// Implementations own their resources from BeginAttempt until Complete or
// Abort returns. Capture callbacks are synchronous, serialized by the shared
// session driver, and receive owned byte/sample storage.
type AttemptEvidence interface {
	CaptureAudio(bench.SessionAudioCapture) error
	CaptureVideo(bench.SessionVideoCapture) error
	Complete(context.Context, EvidenceCompletion) error
	Abort() error
}

// EvidencePlugin is the UI- and provider-neutral Meeting Assistant evidence
// extension point. Implementations may retain media, invoke a caller-selected
// reviewer, or export another create-only artifact format. The runner has no
// Gemini, encoder, filesystem layout, or presentation-layer dependency.
type EvidencePlugin interface {
	BeginAttempt(context.Context, EvidenceAttempt) (AttemptEvidence, error)
	FinishSuite(context.Context, bench.Result) error
}
