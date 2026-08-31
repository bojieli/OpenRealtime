package meeting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
)

const (
	EvidenceOriginProduction = "production-shared-realtime"
	EvidenceOriginHermetic   = "hermetic-test"
)

// EvidenceRunOrigin states whether an attempt traversed the production shared
// Realtime/session path or package-private hermetic test plumbing. EndpointSHA256
// binds the exact configured endpoint without retaining credentials or query
// parameters in a review bundle. A hermetic origin is useful integration
// evidence, but can never support a live/reportable benchmark claim.
type EvidenceRunOrigin struct {
	Kind           string `json:"kind"`
	Live           bool   `json:"live"`
	Transport      string `json:"transport"`
	EndpointSHA256 string `json:"endpoint_sha256"`
}

func (origin EvidenceRunOrigin) validate() error {
	if origin.Kind != EvidenceOriginProduction && origin.Kind != EvidenceOriginHermetic {
		return errors.New("meeting evidence run origin is invalid")
	}
	if origin.Live != (origin.Kind == EvidenceOriginProduction) {
		return errors.New("meeting evidence run origin has inconsistent live status")
	}
	if origin.Transport != bench.TransportWebSocket && origin.Transport != bench.TransportWebRTC {
		return errors.New("meeting evidence run origin transport is invalid")
	}
	if len(origin.EndpointSHA256) != len("sha256:")+sha256.Size*2 ||
		!strings.HasPrefix(origin.EndpointSHA256, "sha256:") {
		return errors.New("meeting evidence run origin endpoint identity is invalid")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(origin.EndpointSHA256, "sha256:")); err != nil {
		return errors.New("meeting evidence run origin endpoint identity is invalid")
	}
	return nil
}

func meetingEndpointIdentity(endpoint string) string {
	digest := sha256.Sum256([]byte(endpoint))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// EvidenceAttempt identifies one exact Meeting Assistant evaluation before
// any browser, media, or Realtime work begins. Trial is one because the v1
// candidate runner owns a single preregistered repetition.
type EvidenceAttempt struct {
	Suite string `json:"suite"`
	Case  string `json:"case"`
	Trial int    `json:"trial"`
	Task  Task   `json:"task"`
	// Cell and Provenance bind the evidence/reviewer request to the exact run
	// configuration and build identity that will later appear in bench.Result.
	// Keeping them on the attempt prevents a valid media exchange from being
	// re-indexed under a different execution requirement or source build.
	Cell                 bench.Cell                 `json:"cell"`
	Provenance           bench.Provenance           `json:"provenance"`
	Origin               EvidenceRunOrigin          `json:"run_origin"`
	ExecutionRequirement bench.ExecutionRequirement `json:"execution_requirement,omitempty"`
}

func (attempt EvidenceAttempt) validate() error {
	if attempt.Suite != SuiteName || attempt.Case != attempt.Task.ID || attempt.Trial != 1 {
		return errors.New("meeting evidence attempt does not identify one exact v1 task")
	}
	canonical := false
	for _, task := range Suite() {
		if attempt.Case == task.ID && reflect.DeepEqual(attempt.Task, task) {
			canonical = true
			break
		}
	}
	if !canonical {
		return errors.New("meeting evidence attempt task differs from the canonical v1 suite")
	}
	if err := attempt.Task.Validate(); err != nil {
		return err
	}
	if err := attempt.Origin.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(attempt.Cell.Name) == "" || len(attempt.Cell.Levels) == 0 {
		return errors.New("meeting evidence attempt has no exact benchmark cell")
	}
	if !reflect.DeepEqual(attempt.ExecutionRequirement, attempt.Cell.Execution) {
		return errors.New("meeting evidence execution requirement differs from its exact cell")
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

// EvidenceError reports an advisory retention/review failure without changing
// the deterministic TaskOutcome. Callers can use errors.As to distinguish an
// evidence failure from a session, environment, or scoring failure.
type EvidenceError struct {
	Case  string
	Stage string
	cause error
}

func (failure *EvidenceError) Error() string {
	if failure == nil {
		return "meeting evidence failed"
	}
	identity := "meeting evidence"
	if failure.Case != "" {
		identity += " for " + failure.Case
	}
	if failure.Stage != "" {
		identity += " during " + failure.Stage
	}
	if failure.cause == nil {
		return identity + " failed"
	}
	return identity + ": " + failure.cause.Error()
}

func (failure *EvidenceError) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.cause
}

func meetingEvidenceError(caseID, stage string, err error) error {
	if err == nil {
		return nil
	}
	return &EvidenceError{Case: caseID, Stage: stage, cause: err}
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
