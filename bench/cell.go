package bench

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Factor names one axis the measurement program varies.
type Factor string

const (
	FactorBinding    Factor = "F1"
	FactorCognition  Factor = "F2"
	FactorObservers  Factor = "F3"
	FactorCadence    Factor = "F4"
	FactorFloor      Factor = "F5"
	FactorSlowModel  Factor = "F6"
	FactorComponents Factor = "F7"
	FactorPolicy     Factor = "F8"
	FactorFastModel  Factor = "F9"
	FactorFastAction Factor = "F10"
	FactorVideoRate  Factor = "F11"
	FactorRecognizer Factor = "F12"
	// FactorTransport records whether the evaluated sensor/executor reached the
	// same protocol over WebSocket or WebRTC. Meeting cells use it because RTP
	// pacing, data-channel ordering, and media codecs are part of the deployed
	// system rather than invisible harness plumbing.
	FactorTransport Factor = "F13"
	// FactorInteractionArchitecture selects who makes interaction decisions and
	// from what evidence: shipped predicates (P), an external text-policy model
	// (T), or a model-native interaction head (N). The level is only the compact
	// column used by the generic report machinery. A reportable F52 comparison
	// also carries the structured architecture identity in bench/architecture;
	// this string by itself is never sufficient evidence.
	FactorInteractionArchitecture Factor = "F52"
)

// Description is what a factor varies, for reports that a person reads.
func (factor Factor) Description() string {
	switch factor {
	case FactorBinding:
		return "binding"
	case FactorCognition:
		return "cognition rollout"
	case FactorObservers:
		return "observer set"
	case FactorCadence:
		return "trigger cadence"
	case FactorFloor:
		return "floor source"
	case FactorSlowModel:
		return "slow model and effort"
	case FactorComponents:
		return "observer components"
	case FactorPolicy:
		return "policy models"
	case FactorFastModel:
		return "fast model"
	case FactorFastAction:
		return "fast action lane"
	case FactorVideoRate:
		return "video frame rate"
	case FactorRecognizer:
		return "recogniser"
	case FactorTransport:
		return "client transport"
	case FactorInteractionArchitecture:
		return "interaction architecture"
	default:
		return string(factor)
	}
}

// Reference is the configuration every paired cell is measured against.
//
// A full cross-product of the factors is both infeasible and
// uninterpretable. Paired cells that change exactly one factor are what make a
// difference attributable to that factor rather than to the others that
// also moved.
func Reference() Cell {
	return Cell{
		Name: "reference",
		Levels: map[Factor]string{
			FactorBinding:    "cascade",
			FactorCognition:  "fast+slow",
			FactorObservers:  "audio",
			FactorCadence:    "200ms",
			FactorFloor:      "engine",
			FactorSlowModel:  "hosted-high",
			FactorComponents: "narration",
			FactorPolicy:     "none",
			FactorFastModel:  "local-text",
			FactorFastAction: "slow-only",
			FactorVideoRate:  "3fps",
			FactorRecognizer: "qwen3-asr",
		},
	}
}

// Cell is one measured configuration.
type Cell struct {
	// Name is how the cell appears in a report.
	Name string `json:"name"`
	// Levels is the complete configuration, including the factors that did not
	// change. A cell that recorded only its differences could not be compared
	// against a reference that later moved.
	Levels map[Factor]string `json:"levels"`
	// Varies names the factors that differ from the reference. It is derived
	// rather than declared, so it cannot disagree with the levels.
	Varies []Factor `json:"varies,omitempty"`
	// Execution is the independently attested runtime contract. Historical
	// cells leave it empty and remain inspectable, but only an explicit
	// graph-native requirement can support a graph-native benchmark claim.
	Execution ExecutionRequirement `json:"execution,omitzero"`
}

// Vary produces a paired cell that changes exactly one factor.
func Vary(factor Factor, level string) (Cell, error) {
	return VaryFrom(Reference(), factor, level)
}

// VaryFrom produces a paired cell against a suite-specific reference. A video
// suite must not label its baseline as the voice-only global reference merely
// to reuse comparison plumbing.
func VaryFrom(reference Cell, factor Factor, level string) (Cell, error) {
	if _, known := reference.Levels[factor]; !known {
		return Cell{}, fmt.Errorf("unknown factor %q", factor)
	}
	if strings.TrimSpace(level) == "" {
		return Cell{}, errors.New("a level is required")
	}
	cell := Cell{
		Name:      fmt.Sprintf("%s=%s", factor, level),
		Levels:    make(map[Factor]string, len(reference.Levels)),
		Execution: reference.Execution.canonicalized(),
	}
	for name, value := range reference.Levels {
		cell.Levels[name] = value
	}
	if cell.Levels[factor] == level {
		return Cell{}, fmt.Errorf("%s is already %q in the reference cell", factor, level)
	}
	cell.Levels[factor] = level
	cell.Varies = []Factor{factor}
	return cell, nil
}

// Compare reports which factors differ between two cells.
//
// It is what a report uses to say what a comparison is actually comparing.
// Two cells that differ in three factors are not a measurement of any one of
// them, and this is what makes that visible rather than assumed.
func Compare(left, right Cell) []Factor {
	var differences []Factor
	for factor, value := range left.Levels {
		if right.Levels[factor] != value {
			differences = append(differences, factor)
		}
	}
	for factor, value := range right.Levels {
		if _, present := left.Levels[factor]; !present && value != "" {
			differences = append(differences, factor)
		}
	}
	sort.Slice(differences, func(left, right int) bool {
		return differences[left] < differences[right]
	})
	return differences
}

// Paired reports whether two cells differ in exactly one factor.
func Paired(left, right Cell) (Factor, bool) {
	differences := Compare(left, right)
	if len(differences) != 1 {
		return "", false
	}
	return differences[0], true
}

// ID is a stable identity for the cell's configuration.
func (cell Cell) ID() string {
	if !cell.Execution.Required() {
		// Preserve identities of historical benchmark artifacts.
		return Fingerprint(cell.Levels)
	}
	return Fingerprint(struct {
		Levels    map[Factor]string    `json:"levels"`
		Execution ExecutionRequirement `json:"execution"`
	}{Levels: cell.Levels, Execution: cell.Execution.canonicalized()})
}

// Describe renders the cell for a person.
func (cell Cell) Describe() string {
	factors := make([]Factor, 0, len(cell.Levels))
	for factor := range cell.Levels {
		factors = append(factors, factor)
	}
	sort.Slice(factors, func(left, right int) bool { return factors[left] < factors[right] })
	parts := make([]string, 0, len(factors))
	for _, factor := range factors {
		parts = append(parts, fmt.Sprintf("%s=%s", factor, cell.Levels[factor]))
	}
	if cell.Execution.Required() {
		parts = append(parts, "execution="+string(cell.Execution.Kind))
	}
	return strings.Join(parts, " ")
}
