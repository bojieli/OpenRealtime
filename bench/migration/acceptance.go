package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const AcceptanceBasisVersion = 1

// AcceptanceDecision is every statistical and measurement choice that must
// be frozen from baseline-only evidence before a candidate is observed.
type AcceptanceDecision struct {
	Inference             BootstrapPolicy `json:"inference"`
	Pass                  RatePolicy      `json:"pass"`
	Interaction           OutcomePolicy   `json:"interaction"`
	Deadline              OutcomePolicy   `json:"deadline"`
	Safety                SafetyPolicy    `json:"safety"`
	Latencies             []LatencyPolicy `json:"latencies"`
	RequireEvidence       bool            `json:"require_evidence"`
	RequiredEvidenceKinds []string        `json:"required_evidence_kinds"`
}

// AcceptanceBasis is the canonical, suite-specific baseline variance record.
// BaselineEvidence points to the immutable observations or analysis inputs;
// Decision is compared byte-for-byte with the suite policy at registration.
type AcceptanceBasis struct {
	Version            int                `json:"version"`
	BasisID            string             `json:"basis_id"`
	Suite              string             `json:"suite"`
	CreatedAt          string             `json:"created_at"`
	MinimumRepetitions int                `json:"minimum_repetitions"`
	BaselineEvidence   []EvidenceRef      `json:"baseline_evidence"`
	Decision           AcceptanceDecision `json:"decision"`
}

func acceptanceDecision(policy SuitePolicy) AcceptanceDecision {
	return canonicalAcceptanceDecision(AcceptanceDecision{
		Inference: policy.Inference, Pass: policy.Pass, Interaction: policy.Interaction,
		Deadline: policy.Deadline, Safety: policy.Safety, Latencies: policy.Latencies,
		RequireEvidence:       policy.RequireEvidence,
		RequiredEvidenceKinds: policy.RequiredEvidenceKinds,
	})
}

func canonicalAcceptanceDecision(input AcceptanceDecision) AcceptanceDecision {
	policy := canonicalSuitePolicy(SuitePolicy{
		Inference: input.Inference, Pass: input.Pass, Interaction: input.Interaction,
		Deadline: input.Deadline, Safety: input.Safety, Latencies: input.Latencies,
		RequireEvidence:       input.RequireEvidence,
		RequiredEvidenceKinds: input.RequiredEvidenceKinds,
	})
	return AcceptanceDecision{
		Inference: policy.Inference, Pass: policy.Pass, Interaction: policy.Interaction,
		Deadline: policy.Deadline, Safety: policy.Safety, Latencies: policy.Latencies,
		RequireEvidence:       policy.RequireEvidence,
		RequiredEvidenceKinds: policy.RequiredEvidenceKinds,
	}
}

func canonicalAcceptanceBasis(input AcceptanceBasis) AcceptanceBasis {
	result := input
	result.BaselineEvidence = append([]EvidenceRef(nil), input.BaselineEvidence...)
	sort.Slice(result.BaselineEvidence, func(left, right int) bool {
		if result.BaselineEvidence[left].Kind != result.BaselineEvidence[right].Kind {
			return result.BaselineEvidence[left].Kind < result.BaselineEvidence[right].Kind
		}
		if result.BaselineEvidence[left].Location != result.BaselineEvidence[right].Location {
			return result.BaselineEvidence[left].Location < result.BaselineEvidence[right].Location
		}
		return result.BaselineEvidence[left].SHA256 < result.BaselineEvidence[right].SHA256
	})
	result.Decision = canonicalAcceptanceDecision(input.Decision)
	return result
}

func (basis AcceptanceBasis) ID() string {
	basis = canonicalAcceptanceBasis(basis)
	basis.BasisID = ""
	return digestJSON(basis)
}

func SealAcceptanceBasis(basis AcceptanceBasis) (AcceptanceBasis, error) {
	basis = canonicalAcceptanceBasis(basis)
	basis.BasisID = ""
	basis.BasisID = basis.ID()
	if err := basis.Validate(); err != nil {
		return AcceptanceBasis{}, err
	}
	return basis, nil
}

func (basis AcceptanceBasis) Validate() error {
	if basis.Version != AcceptanceBasisVersion || !validSHA256(basis.BasisID) || basis.ID() != basis.BasisID {
		return errors.New("acceptance basis version or digest is invalid")
	}
	if err := canonicalCensusText("acceptance-basis suite", basis.Suite); err != nil {
		return err
	}
	createdAt, err := time.Parse(time.RFC3339Nano, basis.CreatedAt)
	if err != nil || createdAt.Location() != time.UTC {
		return errors.New("acceptance basis creation time must be canonical UTC RFC3339")
	}
	if basis.MinimumRepetitions <= 0 || basis.MinimumRepetitions > maxRepetitionsPerCase {
		return errors.New("acceptance basis has an invalid repetition minimum")
	}
	if len(basis.BaselineEvidence) == 0 || len(basis.BaselineEvidence) > maxFixedAndTreatmentAxes {
		return errors.New("acceptance basis needs a bounded non-empty baseline evidence set")
	}
	seen := map[string]bool{}
	for _, reference := range basis.BaselineEvidence {
		if strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.Location) == "" ||
			!validSHA256(reference.SHA256) {
			return errors.New("acceptance basis contains an invalid baseline evidence reference")
		}
		identity := reference.Kind + "\x00" + reference.Location
		if seen[identity] {
			return fmt.Errorf("acceptance basis repeats evidence %q", reference.Location)
		}
		seen[identity] = true
	}
	if len(basis.Decision.Latencies) == 0 || len(basis.Decision.Latencies) > maxLatenciesPerSuite {
		return errors.New("acceptance basis needs a bounded latency decision set")
	}
	return nil
}

func MarshalAcceptanceBasis(basis AcceptanceBasis) ([]byte, error) {
	if err := basis.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonicalAcceptanceBasis(basis), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func DecodeAcceptanceBasis(reader io.Reader) (AcceptanceBasis, error) {
	if reader == nil {
		return AcceptanceBasis{}, errors.New("decode acceptance basis: reader is nil")
	}
	payload, err := readBoundedJSONArtifact(reader, "migration acceptance basis")
	if err != nil {
		return AcceptanceBasis{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var basis AcceptanceBasis
	if err := decoder.Decode(&basis); err != nil {
		return AcceptanceBasis{}, fmt.Errorf("decode acceptance basis: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return AcceptanceBasis{}, errors.New("decode acceptance basis: trailing JSON")
	}
	if err := basis.Validate(); err != nil {
		return AcceptanceBasis{}, err
	}
	basis = canonicalAcceptanceBasis(basis)
	canonical, err := MarshalAcceptanceBasis(basis)
	if err != nil || !bytes.Equal(payload, canonical) {
		return AcceptanceBasis{}, errors.New("decode acceptance basis: artifact bytes are not canonical")
	}
	return basis, nil
}
