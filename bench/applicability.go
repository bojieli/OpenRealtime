package bench

import (
	"errors"
	"fmt"
	"maps"
)

// Applicability records whether a completed task presented the behavior the
// suite is judging. NotApplicable is neither a behavioral pass nor a failure.
type Applicability string

const (
	Applicable    Applicability = "applicable"
	NotApplicable Applicability = "not_applicable"
)

func (outcome TaskOutcome) validateApplicability() error {
	switch outcome.Applicability {
	case "":
		// Older evidence carries no typed applicability. Reopening its receipt
		// must preserve the original score, not reinterpret free-form notes.
		return nil
	case Applicable:
	case NotApplicable:
		if !outcome.Completed || outcome.Passed {
			return errors.New("not-applicable task must be completed without a pass")
		}
	default:
		return fmt.Errorf("unknown task applicability %q", outcome.Applicability)
	}
	if note, present := outcome.Notes["applicable"]; present {
		want := "true"
		if outcome.Applicability == NotApplicable {
			want = "false"
		}
		if note != want {
			return errors.New("task applicability contradicts its retained note")
		}
	}
	return nil
}

func applicabilityPairingFailures(baseline, variant Result) []string {
	var failures []string
	populations := make([]map[string]bool, 2)
	conditional := false
	for index, result := range []Result{baseline, variant} {
		populations[index] = make(map[string]bool)
		for _, task := range result.Tasks {
			conditional = conditional || task.Applicability != ""
			if result.Suite == "fdb-v1.5" && task.Applicability == "" {
				failures = append(failures, fmt.Sprintf("FDB cell %q lacks explicit applicability; historical nominal passes are not comparable quality scores", result.Cell.Name))
				break
			}
			if task.Applicability != NotApplicable && task.Completed {
				populations[index][task.ID] = true
			}
		}
		if result.Summary.Completed-result.Summary.NotApplicable <= 0 {
			failures = append(failures, fmt.Sprintf("cell %q has no applicable tasks; its pass rate is undefined", result.Cell.Name))
		}
	}
	if conditional && !maps.Equal(populations[0], populations[1]) {
		failures = append(failures, "applicable task populations differ; conditional pass rates do not measure the same cases")
	}
	return failures
}
