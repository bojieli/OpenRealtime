package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

// runSubturnCases is the focused authoring runner for a selected sub-turn slab.
// The production scenario command does not use it: promoted sub-turn cases run
// through the graph-native checklist, media verifier, and source receipt. It is
// retained as a small diagnostic seam for developing future boundary checks
// before they are admitted to the canonical contract.
// scenarioPlayer is the one seam in this file. Everything else here is
// bookkeeping around a live session, and a test that had to open one to check
// the bookkeeping would be checking the session instead.
var scenarioPlayer = scenario.Play

type subturnOutcome struct {
	Case     string   `json:"case"`
	Trial    int      `json:"trial"`
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures,omitempty"`
	RunError string   `json:"run_error,omitempty"`
}

func runSubturnCases(
	ctx context.Context, output io.Writer, voice scenario.Voice,
	session bench.SessionConfig, cases []scenario.Scenario, runs int, timeout time.Duration,
) ([]subturnOutcome, error) {
	if len(cases) == 0 || runs <= 0 {
		return nil, nil
	}
	fmt.Fprintf(output, "\n  additional   %d case(s) outside the gated contract, reported not gated\n",
		len(cases))
	outcomes := make([]subturnOutcome, 0, len(cases)*runs)
	for _, item := range cases {
		for trial := 1; trial <= runs; trial++ {
			attempt, err := playSubturnCase(ctx, voice, session, item, timeout)
			if err != nil {
				// One case failing to run is not a reason to abandon the rest,
				// and it is not a behaviour result either. It is recorded as
				// what it is and the run carries on.
				attempt = subturnOutcome{Case: item.Name, Trial: trial, RunError: err.Error()}
			}
			attempt.Case, attempt.Trial = item.Name, trial
			outcomes = append(outcomes, attempt)
			report(output, attempt)
		}
	}
	return outcomes, nil
}

func playSubturnCase(
	ctx context.Context, voice scenario.Voice, session bench.SessionConfig,
	item scenario.Scenario, timeout time.Duration,
) (subturnOutcome, error) {
	bounded, cancel := context.WithTimeout(ctx, timeout+time.Minute)
	defer cancel()
	// A fresh copy per attempt. Play fills in the scenario's own instructions,
	// tools, schedule and trailing silence, and an attempt that inherited the
	// last one's would be measuring the previous case.
	task := session
	task.AttestationScope = ""
	result, err := scenarioPlayer(bounded, voice, task, item)
	if err != nil {
		return subturnOutcome{Failures: result.Failures}, err
	}
	return subturnOutcome{Passed: result.Passed, Failures: result.Failures}, nil
}

func report(output io.Writer, attempt subturnOutcome) {
	switch {
	case attempt.RunError != "":
		fmt.Fprintf(output, "  %-44s #%d  did not run: %s\n",
			attempt.Case, attempt.Trial, attempt.RunError)
	case attempt.Passed:
		fmt.Fprintf(output, "  %-44s #%d  passed\n", attempt.Case, attempt.Trial)
	default:
		fmt.Fprintf(output, "  %-44s #%d  failed\n", attempt.Case, attempt.Trial)
		for _, failure := range attempt.Failures {
			fmt.Fprintf(output, "      %s\n", failure)
		}
	}
}

// writeSubturnRecord stores focused authoring attempts beside an architecture
// record rather than presenting them as graph-native checklist rows.
//
// Inside it they would join a population whose size is checked against a
// registered expectation, and an ungated case counted into a gated population
// is the whole failure this separation exists to prevent.
func writeSubturnRecord(path string, outcomes []subturnOutcome) error {
	if strings.TrimSpace(path) == "" || len(outcomes) == 0 {
		return nil
	}
	payload, err := json.MarshalIndent(map[string]any{
		"format":   "openrealtime.scenario.ungated",
		"version":  1,
		"attempts": outcomes,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode the ungated scenario record: %w", err)
	}
	beside := strings.TrimSuffix(path, filepath.Ext(path)) + ".ungated.json"
	if err := os.MkdirAll(filepath.Dir(beside), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(beside, append(payload, '\n'), 0o644); err != nil {
		return errors.Join(errors.New("write the ungated scenario record"), err)
	}
	return nil
}
