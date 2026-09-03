package realtimecu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
)

const (
	EvidenceOriginProduction = "production-shared-realtime"
	EvidenceOriginHermetic   = "hermetic-test"
)

// EvidenceRunOrigin distinguishes a production session from package-private
// hermetic runner plumbing. Both are useful evidence, but only the production
// path can contribute to a reportable live benchmark result.
type EvidenceRunOrigin struct {
	Kind           string `json:"kind"`
	Live           bool   `json:"live"`
	Transport      string `json:"transport"`
	EndpointSHA256 string `json:"endpoint_sha256"`
}

func (origin EvidenceRunOrigin) validate() error {
	if origin.Kind != EvidenceOriginProduction && origin.Kind != EvidenceOriginHermetic {
		return errors.New("realtime computer-use evidence run origin is invalid")
	}
	if origin.Live != (origin.Kind == EvidenceOriginProduction) {
		return errors.New("realtime computer-use evidence run origin has inconsistent live status")
	}
	if origin.Transport != bench.TransportWebSocket {
		return errors.New("realtime computer-use evidence requires the shared WebSocket Realtime transport")
	}
	if len(origin.EndpointSHA256) != len("sha256:")+sha256.Size*2 ||
		!strings.HasPrefix(origin.EndpointSHA256, "sha256:") {
		return errors.New("realtime computer-use evidence endpoint identity is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(origin.EndpointSHA256, "sha256:")); err != nil {
		return errors.New("realtime computer-use evidence endpoint identity is invalid")
	}
	return nil
}

func endpointIdentity(endpoint string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(endpoint)))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// EvidenceAttempt identifies one exact v1 task/grounding combination before
// browser, media, or Realtime work begins.
type EvidenceAttempt struct {
	Suite                string                     `json:"suite"`
	Case                 string                     `json:"case"`
	Trial                int                        `json:"trial"`
	Task                 Task                       `json:"task"`
	Grounding            Grounding                  `json:"grounding"`
	Origin               EvidenceRunOrigin          `json:"run_origin"`
	Observers            []string                   `json:"observers,omitempty"`
	ExecutionRequirement bench.ExecutionRequirement `json:"execution_requirement,omitempty"`
}

func (attempt EvidenceAttempt) validate() error {
	if attempt.Suite != SuiteName || attempt.Trial != 1 ||
		attempt.Case != (Case{Task: attempt.Task, Grounding: attempt.Grounding}).ID() {
		return errors.New("realtime computer-use evidence attempt does not identify one exact v1 case")
	}
	if err := attempt.Task.Validate(); err != nil {
		return err
	}
	canonical, ok := canonicalCase(attempt.Case)
	if !ok || !equalCase(canonical, Case{Task: attempt.Task, Grounding: attempt.Grounding}) {
		return errors.New("realtime computer-use evidence attempt differs from the authored v1 suite")
	}
	if err := attempt.Origin.validate(); err != nil {
		return err
	}
	if err := validateEvidenceObservers(attempt.Observers); err != nil {
		return err
	}
	if attempt.Origin.Live && len(attempt.Observers) == 0 {
		return errors.New("live realtime computer-use evidence requires an exact observer selection")
	}
	return attempt.ExecutionRequirement.Validate()
}

func validateEvidenceObservers(observers []string) error {
	for index, observer := range observers {
		if strings.TrimSpace(observer) == "" || observer != strings.TrimSpace(observer) ||
			strings.ContainsAny(observer, "\x00\r\n") {
			return errors.New("realtime computer-use evidence observer identity is invalid")
		}
		if slices.Contains(observers[:index], observer) {
			return errors.New("realtime computer-use evidence repeats an observer identity")
		}
	}
	return nil
}

func canonicalCase(id string) (Case, bool) {
	cases, err := Select(nil, nil)
	if err != nil {
		return Case{}, false
	}
	for _, item := range cases {
		if item.ID() == id {
			return cloneCase(item), true
		}
	}
	return Case{}, false
}

func equalCase(left, right Case) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

// EvidenceCompletion is the deterministic scorer record presented after an
// attempt. Outcome.Passed remains authoritative; a secondary model reviewer
// may describe disagreement but cannot replace it.
type EvidenceCompletion struct {
	Attempt    EvidenceAttempt   `json:"attempt"`
	Outcome    bench.TaskOutcome `json:"outcome"`
	Transcript bench.Transcript  `json:"transcript"`
	Page       PageResult        `json:"page_result"`
	Actions    []ActionRecord    `json:"actions"`
	TimedOut   bool              `json:"timed_out"`
}

// AttemptEvidence receives exact media from the same shared session used by
// the scorer, followed by exactly one terminal Complete or Abort call.
type AttemptEvidence interface {
	CaptureAudio(bench.SessionAudioCapture) error
	CaptureVideo(bench.SessionVideoCapture) error
	Complete(context.Context, EvidenceCompletion) error
	Abort() error
}

// EvidencePlugin is the provider-, encoder-, filesystem-, browser-, and
// presentation-neutral extension point for Realtime-CU. Implementations may
// retain media and invoke an offline reviewer, but the runner owns scoring and
// the browser remains an external client of the same Realtime endpoint.
type EvidencePlugin interface {
	BeginAttempt(context.Context, EvidenceAttempt) (AttemptEvidence, error)
	FinishSuite(context.Context, bench.Result) error
}

func cloneCase(source Case) Case {
	result := source
	result.Task.Axes = slices.Clone(source.Task.Axes)
	result.Task.Groundings = slices.Clone(source.Task.Groundings)
	return result
}

func cloneEvidenceAttempt(source EvidenceAttempt) (EvidenceAttempt, error) {
	payload, err := json.Marshal(source)
	if err != nil {
		return EvidenceAttempt{}, fmt.Errorf("snapshot realtime computer-use evidence attempt: %w", err)
	}
	var result EvidenceAttempt
	if err := json.Unmarshal(payload, &result); err != nil {
		return EvidenceAttempt{}, fmt.Errorf("snapshot realtime computer-use evidence attempt: %w", err)
	}
	return result, nil
}

func cloneActionRecords(source []ActionRecord) []ActionRecord {
	result := make([]ActionRecord, len(source))
	for index, record := range source {
		result[index] = record
		result[index].Arguments = slices.Clone(record.Arguments)
		if record.PageBefore != nil {
			result[index].PageBefore = clonePageResult(*record.PageBefore)
		}
		if record.PageAfter != nil {
			result[index].PageAfter = clonePageResult(*record.PageAfter)
		}
	}
	return result
}

func cloneTaskOutcome(source bench.TaskOutcome) bench.TaskOutcome {
	result := source
	if source.Metrics != nil {
		result.Metrics = make(map[string]float64, len(source.Metrics))
		for name, value := range source.Metrics {
			result.Metrics[name] = value
		}
	}
	if source.Notes != nil {
		result.Notes = make(map[string]string, len(source.Notes))
		for name, value := range source.Notes {
			result.Notes[name] = value
		}
	}
	if source.Execution != nil {
		copy := source.Execution.Clone()
		result.Execution = &copy
	}
	return result
}

func cloneTranscript(source bench.Transcript) (bench.Transcript, error) {
	payload, err := json.Marshal(source)
	if err != nil {
		return bench.Transcript{}, fmt.Errorf("snapshot realtime computer-use transcript: %w", err)
	}
	var result bench.Transcript
	if err := json.Unmarshal(payload, &result); err != nil {
		return bench.Transcript{}, fmt.Errorf("snapshot realtime computer-use transcript: %w", err)
	}
	return result, nil
}

func cloneEvidenceCompletion(source EvidenceCompletion) (EvidenceCompletion, error) {
	result := source
	attempt, err := cloneEvidenceAttempt(source.Attempt)
	if err != nil {
		return EvidenceCompletion{}, err
	}
	transcript, err := cloneTranscript(source.Transcript)
	if err != nil {
		return EvidenceCompletion{}, err
	}
	result.Attempt = attempt
	result.Outcome = cloneTaskOutcome(source.Outcome)
	result.Transcript = transcript
	result.Actions = cloneActionRecords(source.Actions)
	return result, nil
}

func cloneResult(source bench.Result) (bench.Result, error) {
	result := source
	result.Cell = cloneCell(source.Cell)
	result.Tasks = make([]bench.TaskOutcome, len(source.Tasks))
	for index, outcome := range source.Tasks {
		result.Tasks[index] = cloneTaskOutcome(outcome)
	}
	if source.Summary.Distributions != nil {
		result.Summary.Distributions = make(
			map[string]bench.Distribution, len(source.Summary.Distributions),
		)
		for name, distribution := range source.Summary.Distributions {
			result.Summary.Distributions[name] = distribution
		}
	}
	return result, nil
}
