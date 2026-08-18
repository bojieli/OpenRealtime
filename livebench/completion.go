package livebench

import "fmt"

// AttemptOutcome is the provider-call portion of an append-only attempt
// ledger. Benchmark-specific manifests may carry more provenance around it,
// but every resumable runner shares these ordering and terminal-state rules.
type AttemptOutcome struct {
	Number    int
	Succeeded bool
}

// RemainingAttemptNumbers returns the exact unused portion of a fixed attempt
// budget. The maximum is a lifetime bound for one benchmark trial, not a fresh
// allowance for each process invocation.
func RemainingAttemptNumbers(used, maximum int) ([]int, error) {
	if maximum < 1 || used < 0 || used > maximum {
		return nil, fmt.Errorf("invalid attempt budget: used=%d maximum=%d", used, maximum)
	}
	remaining := make([]int, 0, maximum-used)
	for number := used + 1; number <= maximum; number++ {
		remaining = append(remaining, number)
	}
	return remaining, nil
}

// AnalyzeAttemptOutcomes validates the per-trial prefix of an append-only
// attempt ledger. Attempt numbers must be contiguous, and success is terminal:
// a resumed runner must never call the provider again after a recorded success.
func AnalyzeAttemptOutcomes(outcomes []AttemptOutcome, maximum int) (remaining []int, succeeded bool, err error) {
	remaining, err = RemainingAttemptNumbers(len(outcomes), maximum)
	if err != nil {
		return nil, false, err
	}
	for index, outcome := range outcomes {
		expected := index + 1
		if outcome.Number != expected {
			return nil, false, fmt.Errorf("attempt ledger is not contiguous: position=%d number=%d expected=%d", expected, outcome.Number, expected)
		}
		if outcome.Succeeded {
			if index != len(outcomes)-1 {
				return nil, false, fmt.Errorf("attempt ledger continues after successful attempt %d", outcome.Number)
			}
			return nil, true, nil
		}
	}
	return remaining, false, nil
}

// ValidateRunCompletion enforces the publication boundary for a full
// benchmark cell. Runners may continue after individual infrastructure errors
// so every sample gets an attempt and the failures remain inspectable, but a
// partial population must not advance to evaluation or be labeled complete.
func ValidateRunCompletion(planned, completed, failures int) error {
	if planned < 1 || completed < 0 || failures < 0 || completed > planned || failures > planned {
		return fmt.Errorf("invalid benchmark completion counts: planned=%d completed=%d failures=%d", planned, completed, failures)
	}
	if completed != planned || failures != 0 {
		return fmt.Errorf("benchmark population incomplete: planned=%d completed=%d failures=%d", planned, completed, failures)
	}
	return nil
}
