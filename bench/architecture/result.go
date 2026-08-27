package architecture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/interaction"
)

const ResultVersion = 4

// Observation binds one task to the runtime status negotiated for its live
// session. Intended configuration and observed configuration are retained
// separately so a label cannot substitute for what actually ran.
type Observation struct {
	TaskID string         `json:"task_id"`
	Status binding.Status `json:"status"`
}

// Result is a scenario/benchmark cell plus its structured architecture
// identity and raw suite records.
type Result struct {
	Version         int               `json:"version"`
	Experiment      string            `json:"experiment"`
	ManifestID      string            `json:"manifest_id"`
	FixtureRevision string            `json:"fixture_revision"`
	Cell            Cell              `json:"architecture_cell"`
	Measurement     bench.Result      `json:"measurement"`
	Observed        []Observation     `json:"observed"`
	Records         []json.RawMessage `json:"records,omitempty"`
}

// ReadResult decodes an artifact without treating a refusal as a parse error.
// A partial, dirty-tree, or immediately preceding schema run must remain
// inspectable; Reportable and Pair are the authorities on which claims it may
// support. Version 2 predates exact evidence vectors and can never become
// reportable under current rules, but rejecting it at read time would erase
// the diagnostic history which motivated those rules. Version 3 added exact
// evidence but predates exact controller/arbitration attestation, so it is
// likewise inspectable and non-reportable under version 4.
func ReadResult(path string) (Result, error) {
	if strings.TrimSpace(path) == "" {
		return Result{}, errors.New("an architecture result needs a path")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return Result{}, err
	}
	var result Result
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Result{}, fmt.Errorf("decode architecture result: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Result{}, errors.New("decode architecture result: trailing JSON value")
		}
		return Result{}, fmt.Errorf("decode architecture result trailing content: %w", err)
	}
	if result.Version != ResultVersion && result.Version != 3 && result.Version != 2 {
		return Result{}, fmt.Errorf("architecture result version must be 2, 3, or %d, got %d", ResultVersion, result.Version)
	}
	return result, nil
}

// NewResult starts one architecture-aware cell.
func NewResult(manifest Manifest, cell Cell, expected int) Result {
	return Result{
		Version: ResultVersion, Experiment: manifest.Name, ManifestID: manifest.ID(),
		FixtureRevision: manifest.FixtureRevision, Cell: cell,
		Measurement: bench.Result{
			Suite: manifest.Suite, Cell: cell.MeasurementCell(), Expected: expected,
			Provenance: bench.Capture(),
		},
	}
}

// Finish derives the generic benchmark summary.
func (result *Result) Finish() { result.Measurement.Finish() }

// Reportable enforces both the generic benchmark rules and architecture
// attestation for every expected task.
func (result Result) Reportable() error {
	failures := result.architectureFailures()
	if err := result.Measurement.Reportable(); err != nil {
		failures = append(failures, err.Error())
	}
	return result.reportabilityError(failures)
}

// architectureFailures validates the structured cell and its live
// attestation without repeating the generic benchmark checks. Pair delegates
// those once to bench.Pair so a refusal stays concise and actionable.
func (result Result) architectureFailures() []string {
	var failures []string
	if result.Version != ResultVersion {
		failures = append(failures, fmt.Sprintf("result version must be %d", ResultVersion))
	}
	if err := result.Cell.Architecture.Validate(); err != nil {
		failures = append(failures, err.Error())
	}
	if result.Cell.Availability != AvailabilityRunnable {
		failures = append(failures, fmt.Sprintf("cell is unavailable: %s", result.Cell.UnavailableReason))
	}
	if result.Measurement.Cell.ID() != result.Cell.MeasurementCell().ID() {
		failures = append(failures, "measurement F52 level disagrees with the structured architecture cell")
	}
	if len(result.Observed) != result.Measurement.Expected {
		failures = append(failures, fmt.Sprintf(
			"%d of %d tasks carry live architecture evidence", len(result.Observed), result.Measurement.Expected))
	}
	measured := make(map[string]bool, len(result.Measurement.Tasks))
	for _, task := range result.Measurement.Tasks {
		identity := strings.TrimSpace(task.ID)
		if identity == "" || measured[identity] {
			failures = append(failures, fmt.Sprintf(
				"measured task identity %q is empty or duplicated", task.ID))
			continue
		}
		measured[identity] = true
	}
	seen := make(map[string]bool, len(result.Observed))
	for _, observed := range result.Observed {
		identity := strings.TrimSpace(observed.TaskID)
		if identity == "" || seen[identity] {
			failures = append(failures, fmt.Sprintf("live evidence task identity %q is empty or duplicated", observed.TaskID))
			continue
		}
		seen[identity] = true
		if !measured[identity] {
			failures = append(failures, fmt.Sprintf(
				"live evidence task %q is not a measured task", observed.TaskID))
		}
		if err := result.Cell.ValidateObserved(observed.Status); err != nil {
			failures = append(failures, fmt.Sprintf("task %q: %v", observed.TaskID, err))
		}
	}
	for identity := range measured {
		if !seen[identity] {
			failures = append(failures, fmt.Sprintf(
				"measured task %q has no live architecture evidence", identity))
		}
	}
	return failures
}

func (result Result) reportabilityError(failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("architecture result %q is not reportable: %s", result.Cell.Name, strings.Join(failures, "; "))
}

// Write saves a complete artifact.
func (result Result) Write(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("an architecture result needs a path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(payload, '\n'), 0o644)
}

// Claim says what a valid comparison can support.
type Claim string

const (
	ClaimArchitecture Claim = "architecture-only"
	ClaimSystem       Claim = "system"
)

// Comparison distinguishes a controlled architecture ablation from a useful
// but confounded system comparison.
type Comparison struct {
	Measurement bench.Comparison `json:"measurement"`
	Claim       Claim            `json:"claim,omitempty"`
	Reportable  bool             `json:"reportable"`
	Differences []string         `json:"non_treatment_differences,omitempty"`
	Refusal     string           `json:"refusal,omitempty"`
}

// Pair compares two architecture results. P/T, T/C, and T/N count as
// architecture evidence only when every non-treatment field is identical.
func Pair(baseline, variant Result) Comparison {
	comparison := Comparison{Measurement: bench.Pair(baseline.Measurement, variant.Measurement)}
	var refusals []string
	if err := baseline.reportabilityError(baseline.architectureFailures()); err != nil {
		refusals = append(refusals, err.Error())
	}
	if err := variant.reportabilityError(variant.architectureFailures()); err != nil {
		refusals = append(refusals, err.Error())
	}
	if baseline.Experiment != variant.Experiment || baseline.ManifestID != variant.ManifestID ||
		baseline.FixtureRevision != variant.FixtureRevision {
		refusals = append(refusals, "experiment manifest or fixture revision differs")
	}
	if !comparison.Measurement.Reportable {
		refusals = append(refusals, comparison.Measurement.Refusal)
	}
	if len(refusals) > 0 {
		comparison.Refusal = strings.Join(unique(refusals), "; ")
		return comparison
	}

	left, right := baseline.Cell.Architecture, variant.Cell.Architecture
	comparison.Differences = nonTreatmentDifferences(left, right)
	if adjacent(left.Level, right.Level) && len(comparison.Differences) == 0 {
		comparison.Claim = ClaimArchitecture
	} else {
		comparison.Claim = ClaimSystem
	}
	comparison.Reportable = true
	return comparison
}

func adjacent(left, right Level) bool {
	return left == LevelPredicates && right == LevelTextPolicy ||
		left == LevelTextPolicy && right == LevelPredicates ||
		left == LevelTextPolicy && right == LevelComposed ||
		left == LevelComposed && right == LevelTextPolicy ||
		left == LevelTextPolicy && right == LevelNative ||
		left == LevelNative && right == LevelTextPolicy
}

type controlIdentity struct {
	Profile         string
	Foreground      ModelIdentity
	Perception      ModelIdentity
	Speech          ModelIdentity
	SpeakerIdentity ModelIdentity
	VisualNarrator  ModelIdentity
	Slow            ModelIdentity
	Ownership       binding.Ownership
	Capabilities    binding.StackCapabilities
	Policies        interaction.Report
	Observers       string
	ToolAuthority   binding.ToolStatus
}

func controls(value Architecture) controlIdentity {
	ownership := value.Ownership.Effective()
	ownership.Interaction = ""
	policies := value.Policies
	// These rows are the interaction-architecture treatment. The text-policy
	// model replaces the narrow predicate implementations of them; requiring
	// their names to remain equal would make the P/T experiment impossible by
	// definition. Trigger, preparation, rollout, commitment, repair, and
	// deferral remain controlled. Extraction is included in the treatment
	// because the text-policy controller also extracts durable interaction
	// instructions.
	policies.Floor = ""
	policies.BargeIn = ""
	policies.Backchannel = ""
	policies.TurnProjection = ""
	policies.Overlap = ""
	policies.Interaction = ""
	policies.Extraction = ""
	return controlIdentity{
		Profile: value.Profile, Foreground: value.Foreground, Perception: value.Perception,
		Speech: value.Speech, SpeakerIdentity: value.SpeakerIdentity,
		VisualNarrator: value.VisualNarrator,
		Slow:           value.Slow, Ownership: ownership,
		Capabilities: value.Capabilities, Policies: policies,
		Observers: strings.Join(value.Observers, "\x00"), ToolAuthority: value.ToolAuthority,
	}
}

func nonTreatmentDifferences(left, right Architecture) []string {
	a, b := controls(left), controls(right)
	checks := []struct {
		name        string
		left, right any
	}{
		{"profile", a.Profile, b.Profile}, {"foreground", a.Foreground, b.Foreground},
		{"perception", a.Perception, b.Perception}, {"speech", a.Speech, b.Speech},
		{"speaker identity", a.SpeakerIdentity, b.SpeakerIdentity},
		{"visual narrator", a.VisualNarrator, b.VisualNarrator},
		{"slow", a.Slow, b.Slow}, {"non-interaction ownership", a.Ownership, b.Ownership},
		{"capability vector", a.Capabilities, b.Capabilities},
		{"non-interaction policies", a.Policies, b.Policies}, {"observers", a.Observers, b.Observers},
		{"tool authority", a.ToolAuthority, b.ToolAuthority},
	}
	var differences []string
	for _, check := range checks {
		if !reflect.DeepEqual(check.left, check.right) {
			differences = append(differences, check.name)
		}
	}
	return differences
}

func unique(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}
