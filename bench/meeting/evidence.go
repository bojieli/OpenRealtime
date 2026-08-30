package meeting

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
// runner owns a single repetition; an outer migration runner may construct a
// fresh evidence plug-in for each preregistered repetition.
type EvidenceAttempt struct {
	Suite                string                     `json:"suite"`
	Case                 string                     `json:"case"`
	Trial                int                        `json:"trial"`
	Task                 Task                       `json:"task"`
	Origin               EvidenceRunOrigin          `json:"run_origin"`
	ExecutionRequirement bench.ExecutionRequirement `json:"execution_requirement,omitempty"`
}

func (attempt EvidenceAttempt) validate() error {
	if attempt.Suite != SuiteName || attempt.Case != attempt.Task.ID || attempt.Trial != 1 {
		return errors.New("meeting evidence attempt does not identify one exact v1 task")
	}
	if err := attempt.Task.Validate(); err != nil {
		return err
	}
	if err := attempt.Origin.validate(); err != nil {
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
