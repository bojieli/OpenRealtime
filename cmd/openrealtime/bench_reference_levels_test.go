package main

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

// A benchmark artifact states the configuration it measured, and that claim is
// what makes two cells comparable. resolveCell used to ignore everything except
// a single varied factor, so a run against local recognisers and models still
// declared the canonical reference levels -- qwen3-asr, a hosted slow model --
// and the artifact asserted providers it had never contacted.
//
// A mislabel of that shape survives every count: the totals are right, the
// tasks are right, and only the header is wrong.
func TestResolveCellRecordsWhatTheDeploymentActuallyRuns(t *testing.T) {
	t.Parallel()
	reference, err := resolveCell("", "", "", "")
	if err != nil {
		t.Fatalf("reference cell: %v", err)
	}
	if got := reference.Levels[bench.FactorRecognizer]; got != "qwen3-asr" {
		t.Fatalf("reference recogniser = %q, want qwen3-asr", got)
	}

	local, err := resolveCell("local-stack", "F12=sensevoice,F6=local-high", "", "")
	if err != nil {
		t.Fatalf("local cell: %v", err)
	}
	if got := local.Levels[bench.FactorRecognizer]; got != "sensevoice" {
		t.Fatalf("recorded recogniser = %q, want sensevoice", got)
	}
	if got := local.Levels[bench.FactorSlowModel]; got != "local-high" {
		t.Fatalf("recorded slow model = %q, want local-high", got)
	}
	if local.Name != "local-stack" {
		t.Fatalf("cell name = %q, want local-stack", local.Name)
	}
	// Factors the deployment did not move stay at the reference, so a cell
	// remains comparable rather than describing only its differences.
	if got := local.Levels[bench.FactorBinding]; got != "cascade" {
		t.Fatalf("untouched factor = %q, want the reference level", got)
	}

	// Overrides and a varied factor compose: the fixed levels describe the
	// deployment, the varied one is what the pair is measuring.
	varied, err := resolveCell("variant", "F12=sensevoice", "F2", "fast-only")
	if err != nil {
		t.Fatalf("varied cell: %v", err)
	}
	if got := varied.Levels[bench.FactorRecognizer]; got != "sensevoice" {
		t.Fatalf("varied cell recogniser = %q, want sensevoice", got)
	}
	if got := varied.Levels[bench.FactorCognition]; got != "fast-only" {
		t.Fatalf("varied cell cognition = %q, want fast-only", got)
	}
	if len(varied.Varies) != 1 || varied.Varies[0] != bench.FactorCognition {
		t.Fatalf("varies = %v, want exactly the varied factor", varied.Varies)
	}
}

func TestResolveCellRefusesOverridesItCannotRecord(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		levels string
		want   string
	}{
		{"unknown factor", "F99=something", "unknown reference factor"},
		{"missing level", "F12=", "invalid reference level"},
		{"missing factor", "=sensevoice", "invalid reference level"},
		{"not an assignment", "F12", "invalid reference level"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := resolveCell("local", test.levels, "", "")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("resolveCell = %v, want one containing %q", err, test.want)
			}
		})
	}
}
