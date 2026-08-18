package livebench

import "testing"

func TestRemainingAttemptNumbersIsLifetimeBound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		used, maximum int
		want          []int
		wantError     bool
	}{
		{name: "new trial", maximum: 3, want: []int{1, 2, 3}},
		{name: "resumed trial", used: 2, maximum: 3, want: []int{3}},
		{name: "exhausted trial", used: 3, maximum: 3, want: []int{}},
		{name: "over budget", used: 4, maximum: 3, wantError: true},
		{name: "invalid maximum", maximum: 0, wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := RemainingAttemptNumbers(test.used, test.maximum)
			if (err != nil) != test.wantError {
				t.Fatalf("RemainingAttemptNumbers(%d, %d) error=%v, wantError=%v", test.used, test.maximum, err, test.wantError)
			}
			if len(got) != len(test.want) {
				t.Fatalf("RemainingAttemptNumbers(%d, %d)=%v, want %v", test.used, test.maximum, got, test.want)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("RemainingAttemptNumbers(%d, %d)=%v, want %v", test.used, test.maximum, got, test.want)
				}
			}
		})
	}
}

func TestAnalyzeAttemptOutcomes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		outcomes  []AttemptOutcome
		remaining []int
		succeeded bool
		wantError bool
	}{
		{name: "failed prefix", outcomes: []AttemptOutcome{{Number: 1}, {Number: 2}}, remaining: []int{3}},
		{name: "terminal success", outcomes: []AttemptOutcome{{Number: 1}, {Number: 2, Succeeded: true}}, succeeded: true},
		{name: "gap", outcomes: []AttemptOutcome{{Number: 2}}, wantError: true},
		{name: "record after success", outcomes: []AttemptOutcome{{Number: 1, Succeeded: true}, {Number: 2}}, wantError: true},
		{name: "too many", outcomes: []AttemptOutcome{{Number: 1}, {Number: 2}, {Number: 3}, {Number: 4}}, wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			remaining, succeeded, err := AnalyzeAttemptOutcomes(test.outcomes, 3)
			if (err != nil) != test.wantError || succeeded != test.succeeded {
				t.Fatalf("AnalyzeAttemptOutcomes(%v, 3) remaining=%v succeeded=%v error=%v", test.outcomes, remaining, succeeded, err)
			}
			if len(remaining) != len(test.remaining) {
				t.Fatalf("remaining=%v, want %v", remaining, test.remaining)
			}
		})
	}
}

func TestValidateRunCompletion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                  string
		planned, done, failed int
		wantError             bool
	}{
		{name: "complete", planned: 100, done: 100},
		{name: "failed sample", planned: 100, done: 99, failed: 1, wantError: true},
		{name: "unaccounted sample", planned: 100, done: 99, wantError: true},
		{name: "invalid counts", planned: 1, done: 2, wantError: true},
		{name: "empty plan", wantError: true},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateRunCompletion(test.planned, test.done, test.failed)
			if (err != nil) != test.wantError {
				t.Fatalf("ValidateRunCompletion(%d, %d, %d) error=%v, wantError=%v", test.planned, test.done, test.failed, err, test.wantError)
			}
		})
	}
}
