package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

// The ungated cases are reported rather than gated, which is exactly the shape
// that can quietly stop running: nothing fails when nothing happens. So what is
// asserted is that every case was actually played, the right number of times.
func TestEveryUngatedCaseIsActuallyPlayed(t *testing.T) {
	cases := scenario.SubturnSuite()
	if len(cases) == 0 {
		t.Fatal("there are no cases outside the gated contract; the wiring below reports nothing")
	}
	played := map[string]int{}
	original := scenarioPlayer
	defer func() { scenarioPlayer = original }()
	scenarioPlayer = func(
		_ context.Context, _ scenario.Voice, _ bench.SessionConfig, item scenario.Scenario,
	) (scenario.Result, error) {
		played[item.Name]++
		return scenario.Result{Scenario: item.Name, Passed: true}, nil
	}

	output := &bytes.Buffer{}
	outcomes, err := runSubturnCases(
		context.Background(), output, stubVoice{}, bench.SessionConfig{}, cases, 2, time.Second,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != len(cases)*2 {
		t.Fatalf("%d attempts for %d cases at 2 runs", len(outcomes), len(cases))
	}
	for _, item := range cases {
		if played[item.Name] != 2 {
			t.Fatalf("%q was played %d times, want 2", item.Name, played[item.Name])
		}
	}
	if !strings.Contains(output.String(), "reported not gated") {
		t.Fatalf("the heading does not say these are ungated:\n%s", output)
	}
}

// A failing ungated case must print why. A line that said only "failed" would
// make the one case in here indistinguishable from a missing endpoint.
func TestAnUngatedFailurePrintsItsReasons(t *testing.T) {
	original := scenarioPlayer
	defer func() { scenarioPlayer = original }()
	scenarioPlayer = func(
		_ context.Context, _ scenario.Voice, _ bench.SessionConfig, item scenario.Scenario,
	) (scenario.Result, error) {
		return scenario.Result{
			Scenario: item.Name, Passed: false,
			Failures: []string{"NOT VERIFIED: no transcription endpoint was configured"},
		}, nil
	}

	output := &bytes.Buffer{}
	outcomes, err := runSubturnCases(context.Background(), output, stubVoice{}, bench.SessionConfig{},
		scenario.SubturnSuite()[:1], 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 1 || outcomes[0].Passed {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	if !strings.Contains(output.String(), "NOT VERIFIED") {
		t.Fatalf("the reason was not printed:\n%s", output)
	}

	// And it is recorded beside the architecture record rather than inside it:
	// an ungated attempt counted into a gated population is the whole failure
	// the separation exists to prevent.
	directory := t.TempDir()
	record := filepath.Join(directory, "run.json")
	if err := writeSubturnRecord(record, outcomes); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("the ungated attempts were written into the architecture record: %v", err)
	}
	payload, err := os.ReadFile(filepath.Join(directory, "run.ungated.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Format   string           `json:"format"`
		Attempts []subturnOutcome `json:"attempts"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Format != "openrealtime.scenario.ungated" || len(decoded.Attempts) != 1 {
		t.Fatalf("ungated record = %+v", decoded)
	}
}

type stubVoice struct{}

func (stubVoice) Speak(context.Context, string, string) ([]int16, error) { return nil, nil }

// The focused grouping stays independently selectable, while every case in it
// must also be present in the canonical graph-native evidence population.
func TestEverySubturnCaseIsInTheCanonicalSuite(t *testing.T) {
	canonical := map[string]bool{}
	for _, item := range scenario.Suite() {
		canonical[item.Name] = true
	}
	for _, item := range scenario.SubturnSuite() {
		if !canonical[item.Name] {
			t.Fatalf("sub-turn case %q is absent from the canonical evidence suite", item.Name)
		}
	}
}
