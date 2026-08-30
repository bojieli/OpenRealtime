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

const (
	LaunchIntentVersion  = 1
	LaunchOutcomeVersion = 1

	EvidenceKindLaunchIntent  = "migration-launch-intent"
	EvidenceKindLaunchOutcome = "migration-launch-outcome"
)

// LaunchIntent is the immutable pre-endpoint binding between a registration
// and the exact repetition slabs one suite invocation will attempt.
type LaunchIntent struct {
	Version      int          `json:"version"`
	IntentID     string       `json:"intent_id"`
	Registration EvidenceRef  `json:"registration"`
	Arm          Arm          `json:"arm"`
	Suite        string       `json:"suite"`
	ObservedAt   string       `json:"observed_at"`
	Keys         []AttemptKey `json:"keys"`
	Axes         []Axis       `json:"prelaunch_axes"`
}

// LaunchOutcome is a create-only sidecar around an unchanged suite result.
// Importers use it to obtain the arm/repetition assignment from an artifact
// that existed before endpoint work instead of accepting a post-hoc CLI label.
type LaunchOutcome struct {
	Version    int         `json:"version"`
	OutcomeID  string      `json:"outcome_id"`
	Intent     EvidenceRef `json:"intent"`
	Result     EvidenceRef `json:"result"`
	RetainedAt string      `json:"retained_at"`
}

func (intent LaunchIntent) ID() string {
	intent = canonicalLaunchIntent(intent)
	intent.IntentID = ""
	return digestJSON(intent)
}

func canonicalLaunchIntent(input LaunchIntent) LaunchIntent {
	result := input
	result.Keys = append([]AttemptKey(nil), input.Keys...)
	result.Axes = append([]Axis(nil), input.Axes...)
	sort.Slice(result.Keys, func(left, right int) bool {
		return keyScope(result.Keys[left]) < keyScope(result.Keys[right])
	})
	sort.Slice(result.Axes, func(left, right int) bool {
		return result.Axes[left].Name < result.Axes[right].Name
	})
	return result
}

func (intent LaunchIntent) Validate() error {
	if intent.Version != LaunchIntentVersion || !validSHA256(intent.IntentID) || intent.ID() != intent.IntentID {
		return errors.New("launch intent version or digest is invalid")
	}
	if intent.Registration.Kind != EvidenceKindRegistration || strings.TrimSpace(intent.Suite) == "" ||
		(intent.Arm != ArmBaseline && intent.Arm != ArmCandidate) {
		return errors.New("launch intent has an invalid registration, suite, or arm")
	}
	if strings.TrimSpace(intent.Registration.Location) == "" || !validSHA256(intent.Registration.SHA256) {
		return errors.New("launch intent has an invalid registration reference")
	}
	when, err := time.Parse(time.RFC3339Nano, intent.ObservedAt)
	if err != nil || when.Location() != time.UTC {
		return errors.New("launch intent observation time must be canonical UTC RFC3339")
	}
	if len(intent.Keys) == 0 || len(intent.Keys) > maxAttemptsPerSuite {
		return errors.New("launch intent has an invalid attempt population")
	}
	seenKeys := map[AttemptKey]bool{}
	for _, key := range intent.Keys {
		if key.Suite != intent.Suite || seenKeys[key] {
			return errors.New("launch intent has a foreign or duplicate attempt key")
		}
		for label, value := range map[string]string{
			"condition": key.Condition, "case": key.Case, "repetition": key.Repetition,
		} {
			if err := canonicalCensusText("launch "+label, value); err != nil {
				return err
			}
		}
		seenKeys[key] = true
	}
	if len(intent.Axes) == 0 || len(intent.Axes) > maxFixedAndTreatmentAxes {
		return errors.New("launch intent has an invalid prelaunch axis set")
	}
	seenAxes := map[string]bool{}
	for _, axis := range intent.Axes {
		if strings.TrimSpace(axis.Name) == "" || strings.TrimSpace(axis.Value) == "" || seenAxes[axis.Name] {
			return errors.New("launch intent has an empty or duplicate prelaunch axis")
		}
		seenAxes[axis.Name] = true
	}
	return nil
}

func MarshalLaunchIntent(intent LaunchIntent) ([]byte, error) {
	if err := intent.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonicalLaunchIntent(intent), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func DecodeLaunchIntent(reader io.Reader) (LaunchIntent, error) {
	if reader == nil {
		return LaunchIntent{}, errors.New("decode launch intent: reader is nil")
	}
	payload, err := readBoundedJSONArtifact(reader, "migration launch intent")
	if err != nil {
		return LaunchIntent{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var intent LaunchIntent
	if err := decoder.Decode(&intent); err != nil {
		return LaunchIntent{}, fmt.Errorf("decode launch intent: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return LaunchIntent{}, errors.New("decode launch intent: trailing JSON")
	}
	if err := intent.Validate(); err != nil {
		return LaunchIntent{}, err
	}
	intent = canonicalLaunchIntent(intent)
	canonical, err := MarshalLaunchIntent(intent)
	if err != nil || !bytes.Equal(payload, canonical) {
		return LaunchIntent{}, errors.New("decode launch intent: artifact bytes are not canonical")
	}
	return intent, nil
}

func RegisterLaunchIntent(
	store LocalStore, registration EvidenceRef, request LaunchRequest,
) (LaunchIntent, EvidenceRef, error) {
	if err := ValidateLaunch(store, registration, request); err != nil {
		return LaunchIntent{}, EvidenceRef{}, err
	}
	registered, err := ResolveRegistration(store, registration)
	if err != nil {
		return LaunchIntent{}, EvidenceRef{}, err
	}
	_, _, profiles, err := VerifyRegistration(store, registered, request.ObservedAt)
	if err != nil {
		return LaunchIntent{}, EvidenceRef{}, err
	}
	profile := profiles[request.Suite]
	axes, err := launchAxes(request.Cell, request.Provenance, profile, request.Arm)
	if err != nil {
		return LaunchIntent{}, EvidenceRef{}, err
	}
	intent := canonicalLaunchIntent(LaunchIntent{
		Version: LaunchIntentVersion, Registration: registration, Arm: request.Arm,
		Suite: request.Suite, ObservedAt: request.ObservedAt.UTC().Format(time.RFC3339Nano),
		Keys: request.Keys, Axes: axes,
	})
	intent.IntentID = intent.ID()
	payload, err := MarshalLaunchIntent(intent)
	if err != nil {
		return LaunchIntent{}, EvidenceRef{}, err
	}
	reference, err := store.ArchiveBytes(EvidenceKindLaunchIntent, ".json", payload)
	if err != nil {
		return LaunchIntent{}, EvidenceRef{}, err
	}
	return intent, reference, nil
}

func (outcome LaunchOutcome) ID() string {
	outcome.OutcomeID = ""
	return digestJSON(outcome)
}

func (outcome LaunchOutcome) Validate() error {
	if outcome.Version != LaunchOutcomeVersion || !validSHA256(outcome.OutcomeID) || outcome.ID() != outcome.OutcomeID {
		return errors.New("launch outcome version or digest is invalid")
	}
	if outcome.Intent.Kind != EvidenceKindLaunchIntent || outcome.Result.Kind != EvidenceKindResult {
		return errors.New("launch outcome has the wrong intent or result evidence kind")
	}
	for _, reference := range []EvidenceRef{outcome.Intent, outcome.Result} {
		if strings.TrimSpace(reference.Location) == "" || !validSHA256(reference.SHA256) {
			return errors.New("launch outcome has an invalid evidence reference")
		}
	}
	when, err := time.Parse(time.RFC3339Nano, outcome.RetainedAt)
	if err != nil || when.Location() != time.UTC {
		return errors.New("launch outcome retention time must be canonical UTC RFC3339")
	}
	return nil
}

func MarshalLaunchOutcome(outcome LaunchOutcome) ([]byte, error) {
	if err := outcome.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(outcome, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func DecodeLaunchOutcome(reader io.Reader) (LaunchOutcome, error) {
	if reader == nil {
		return LaunchOutcome{}, errors.New("decode launch outcome: reader is nil")
	}
	payload, err := readBoundedJSONArtifact(reader, "migration launch outcome")
	if err != nil {
		return LaunchOutcome{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var outcome LaunchOutcome
	if err := decoder.Decode(&outcome); err != nil {
		return LaunchOutcome{}, fmt.Errorf("decode launch outcome: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return LaunchOutcome{}, errors.New("decode launch outcome: trailing JSON")
	}
	if err := outcome.Validate(); err != nil {
		return LaunchOutcome{}, err
	}
	canonical, err := MarshalLaunchOutcome(outcome)
	if err != nil || !bytes.Equal(payload, canonical) {
		return LaunchOutcome{}, errors.New("decode launch outcome: artifact bytes are not canonical")
	}
	return outcome, nil
}

// RetainLaunchOutcome archives result bytes first, then binds them to the
// preregistered intent. If sidecar validation fails, the result reference is
// still returned so a failed attempt cannot disappear.
func RetainLaunchOutcome(
	store LocalStore, intentReference EvidenceRef, resultPayload []byte,
) (EvidenceRef, EvidenceRef, error) {
	resultReference, err := store.ArchiveBytes(EvidenceKindResult, ".json", resultPayload)
	if err != nil {
		return EvidenceRef{}, EvidenceRef{}, err
	}
	intentPayload, err := store.Resolve(intentReference)
	if err != nil {
		return resultReference, EvidenceRef{}, err
	}
	intent, err := DecodeLaunchIntent(bytes.NewReader(intentPayload))
	if err != nil {
		return resultReference, EvidenceRef{}, err
	}
	if err := validateResultForIntent(store, intent, resultPayload); err != nil {
		return resultReference, EvidenceRef{}, err
	}
	outcome := LaunchOutcome{
		Version: LaunchOutcomeVersion, Intent: intentReference, Result: resultReference,
		RetainedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	outcome.OutcomeID = outcome.ID()
	payload, err := MarshalLaunchOutcome(outcome)
	if err != nil {
		return resultReference, EvidenceRef{}, err
	}
	outcomeReference, err := store.ArchiveBytes(EvidenceKindLaunchOutcome, ".json", payload)
	return resultReference, outcomeReference, err
}

func validateResultForIntent(store LocalStore, intent LaunchIntent, payload []byte) error {
	registration, err := ResolveRegistration(store, intent.Registration)
	if err != nil {
		return err
	}
	observedAt, _ := time.Parse(time.RFC3339Nano, intent.ObservedAt)
	_, _, profiles, err := VerifyRegistration(store, registration, observedAt)
	if err != nil {
		return err
	}
	profile, exists := profiles[intent.Suite]
	if !exists {
		return fmt.Errorf("launch intent has no registered profile for %q", intent.Suite)
	}
	result, _, err := decodeCompatibleResult(payload, profile.Format)
	if err != nil {
		return err
	}
	if !compatibleSuiteName(intent.Suite, result.Suite) {
		return fmt.Errorf("retained result suite %q does not match launch intent %q",
			result.Suite, intent.Suite)
	}
	started, err := time.Parse(time.RFC3339Nano, result.Provenance.StartedAt)
	if err != nil {
		return fmt.Errorf("retained result has invalid start time: %w", err)
	}
	if started.Add(time.Second).Before(observedAt) {
		return errors.New("retained result predates its launch intent")
	}
	axes, err := launchAxes(result.Cell, result.Provenance, profile, intent.Arm)
	if err != nil {
		return err
	}
	if canonicalJSON(axes) != canonicalJSON(intent.Axes) {
		return errors.New("retained result axes differ from its launch intent")
	}
	return nil
}

func ResolveLaunchOutcome(
	store LocalStore, reference EvidenceRef,
) (LaunchOutcome, LaunchIntent, error) {
	if reference.Kind != EvidenceKindLaunchOutcome {
		return LaunchOutcome{}, LaunchIntent{}, fmt.Errorf(
			"launch outcome reference kind must be %q", EvidenceKindLaunchOutcome)
	}
	payload, err := store.Resolve(reference)
	if err != nil {
		return LaunchOutcome{}, LaunchIntent{}, err
	}
	outcome, err := DecodeLaunchOutcome(bytes.NewReader(payload))
	if err != nil {
		return LaunchOutcome{}, LaunchIntent{}, err
	}
	intentPayload, err := store.Resolve(outcome.Intent)
	if err != nil {
		return LaunchOutcome{}, LaunchIntent{}, err
	}
	intent, err := DecodeLaunchIntent(bytes.NewReader(intentPayload))
	if err != nil {
		return LaunchOutcome{}, LaunchIntent{}, err
	}
	if _, err := store.Resolve(outcome.Result); err != nil {
		return LaunchOutcome{}, LaunchIntent{}, err
	}
	return outcome, intent, nil
}
