package livebench

import "testing"

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
