package main

import (
	"strings"
	"testing"
)

// What a surface says it can do has to be what it can do.
//
// The first version of this warned about changing files and running commands
// on a surface that declared neither, because it was written once for the
// configuration it was written in. A warning that lists powers the process
// does not have is a warning that gets read once and then ignored, which
// leaves the one that matters unread with it.
func TestTheStartupWarningNamesOnlyWhatWasDeclared(t *testing.T) {
	for name, testCase := range map[string]struct {
		files    []string
		browser  string
		contains []string
		absent   []string
	}{
		"read-only and no browser": {
			absent: []string{"files", "browser"},
		},
		"read-only with a browser": {
			browser:  "surface-browser",
			contains: []string{"browser it is attached to", "surface-browser"},
			absent:   []string{"change your files", "wait for you to approve"},
		},
		"mutating with no browser": {
			files:    []string{"write_file", "run_command"},
			contains: []string{"change your files", "write_file, run_command", "wait for you to approve"},
			absent:   []string{"browser"},
		},
		"mutating with a browser": {
			files:   []string{"write_file"},
			browser: "surface-browser",
			contains: []string{"write_file", "browser it is attached to", "wait for you to approve",
				"without asking"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			warning := describePowers(testCase.files, testCase.browser)
			if len(testCase.contains) == 0 && warning != "" {
				t.Fatalf("a surface that can do nothing must warn about nothing, got %q", warning)
			}
			for _, required := range testCase.contains {
				if !strings.Contains(warning, required) {
					t.Errorf("expected %q in the warning, got %q", required, warning)
				}
			}
			for _, forbidden := range testCase.absent {
				if strings.Contains(warning, forbidden) {
					t.Errorf("did not expect %q in the warning, got %q", forbidden, warning)
				}
			}
		})
	}
}
