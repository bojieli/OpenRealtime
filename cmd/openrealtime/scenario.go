package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

// runScenario drives the end-to-end interaction suite.
//
// It sits beside bench rather than under it because it scores a different
// thing. A benchmark plays somebody else's recording and asks whether the task
// was completed; a scenario scripts a conversation and asks whether the agent
// spoke at the right moments. A system can pass every benchmark here while
// talking over everybody in it.
func runScenario(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("scenario", flag.ContinueOnError)
	flags.SetOutput(output)
	var (
		endpoint         = flags.String("url", "ws://127.0.0.1:8765/v1/realtime", "session endpoint")
		tokenEnv         = flags.String("token-env", "", "environment variable holding the session credential")
		model            = flags.String("model", "", "model to request; empty selects the server's own")
		speech           = flags.String("speech-url", "http://127.0.0.1:8081/v1/audio/speech", "speech endpoint that gives the participants voices")
		voice            = flags.String("speech-model", "fishaudio/s2-pro", "speech model")
		only             = flags.String("only", "", "run one scenario by name")
		timeout          = flags.Duration("timeout", 3*time.Minute, "bound on one scenario")
		record           = flags.String("record", "", "write the timed record of each scenario to this file")
		repeat           = flags.Int("repeat", 1, "runs per scenario; latency from one run is noise, so a latency claim needs several")
		experiment       = flags.String("architecture-manifest", "", "versioned P/T/C/N architecture experiment manifest")
		architectureCell = flags.String("architecture-cell", "", "cell name in -architecture-manifest")
	)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("scenario accepts flags only")
	}

	var manifest archbench.Manifest
	var selectedCell archbench.Cell
	architectureRun := strings.TrimSpace(*experiment) != ""
	if architectureRun {
		var err error
		manifest, err = archbench.Read(*experiment)
		if err != nil {
			return err
		}
		if manifest.Suite != "scenario" {
			return fmt.Errorf("architecture manifest suite must be scenario, got %q", manifest.Suite)
		}
		name := strings.TrimSpace(*architectureCell)
		if name == "" && len(manifest.Cells) == 1 {
			name = manifest.Cells[0].Name
		}
		if name == "" {
			return errors.New("-architecture-cell is required when the manifest has multiple cells")
		}
		selectedCell, err = manifest.Cell(name)
		if err != nil {
			return err
		}
		if selectedCell.Availability != archbench.AvailabilityRunnable {
			return fmt.Errorf("architecture cell %q is unavailable: %s",
				selectedCell.Name, selectedCell.UnavailableReason)
		}
	} else if strings.TrimSpace(*architectureCell) != "" {
		return errors.New("-architecture-cell requires -architecture-manifest")
	}

	speaker := scenario.SpeechVoice{
		Endpoint: *speech, Model: *voice, Default: "default",
		// The second party gets a different voice. A phone menu that sounds
		// exactly like the caller removes the difficulty the case exists to
		// pose.
		Voices: map[string]string{"other": "alloy"},
	}
	config := bench.SessionConfig{
		Endpoint: *endpoint, Token: os.Getenv(*tokenEnv), Model: *model,
		Timeout: *timeout, Quiet: true, CaptureRuntimeEvidence: architectureRun,
	}

	var selected []scenario.Scenario
	fullSuite := scenario.Suite()
	for _, item := range fullSuite {
		if *only == "" || item.Name == *only {
			selected = append(selected, item)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("no scenario named %q", *only)
	}

	var results []scenario.Result
	passed := 0
	runs := max(1, *repeat)
	var architectureResult *archbench.Result
	if architectureRun {
		// -only is a diagnostic filter, not a smaller definition of the suite.
		// The missing scenarios stay expected so a convenient smoke run cannot
		// become a publishable architecture result.
		started := archbench.NewResult(manifest, selectedCell, len(fullSuite)*runs)
		architectureResult = &started
		fmt.Fprintf(output, "  architecture %s  F52=%s\n", selectedCell.Name, selectedCell.Architecture.Level)
	}
	for _, item := range selected {
		var attempts []scenario.Result
		for run := 0; run < runs; run++ {
			taskID := fmt.Sprintf("%s#%d", item.Name, run+1)
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			result, err := scenario.Play(ctx, speaker, config, item)
			cancel()
			if err != nil {
				fmt.Fprintf(output, "  ERR  %-28s %v\n", item.Name, err)
			}
			attempts = append(attempts, result)
			results = append(results, result)
			if result.Passed {
				passed++
			}
			if architectureResult != nil {
				architectureResult.Measurement.Tasks = append(
					architectureResult.Measurement.Tasks, scenarioTask(taskID, result, err))
				if result.Transcript.Runtime != nil {
					architectureResult.Observed = append(architectureResult.Observed, archbench.Observation{
						TaskID: taskID, Status: *result.Transcript.Runtime,
					})
				}
				record, marshalErr := json.Marshal(result)
				if marshalErr != nil {
					return marshalErr
				}
				architectureResult.Records = append(architectureResult.Records, record)
			}
		}
		reportScenario(output, item, attempts)
	}
	fmt.Fprintf(output, "\n  scenarios %d/%d\n", passed, len(results))

	if architectureResult != nil {
		architectureResult.Finish()
		fmt.Fprintf(output, "  measured    %d/%d tasks completed\n",
			architectureResult.Measurement.Summary.Completed, architectureResult.Measurement.Expected)
		if err := architectureResult.Reportable(); err != nil {
			fmt.Fprintf(output, "  NOT REPORTABLE: %v\n", err)
		} else {
			fmt.Fprintln(output, "  reportable architecture cell")
		}
	}

	if strings.TrimSpace(*record) != "" && architectureResult != nil {
		if err := architectureResult.Write(*record); err != nil {
			return fmt.Errorf("write the architecture record: %w", err)
		}
	} else if strings.TrimSpace(*record) != "" {
		encoded, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*record, encoded, 0o644); err != nil {
			return fmt.Errorf("write the record: %w", err)
		}
	}
	return nil
}

// scenarioTask adapts one owned interaction scenario to the generic benchmark
// result without throwing away its richer raw record.
func scenarioTask(id string, result scenario.Result, runErr error) bench.TaskOutcome {
	outcome := bench.TaskOutcome{ID: id, Completed: runErr == nil, Passed: result.Passed}
	if runErr != nil {
		outcome.Error = runErr.Error()
	}
	var heard []float64
	missed, negative := 0, 0
	for _, latency := range result.Latencies {
		switch {
		case !latency.Heard:
			missed++
		case latency.MS < 0:
			negative++
		default:
			heard = append(heard, latency.MS)
		}
	}
	outcome.Metrics = map[string]float64{
		"missed_reaction_triggers": float64(missed),
		"overlap_before_trigger":   float64(negative),
		"failed_checks":            float64(len(result.Failures)),
	}
	if len(heard) > 0 {
		distribution := bench.Summarise(heard)
		outcome.Metrics["reaction_latency_p50_ms"] = distribution.P50
		outcome.Metrics["reaction_latency_p90_ms"] = distribution.P90
	}
	if len(result.Failures) > 0 {
		outcome.Notes = map[string]string{"failures": strings.Join(result.Failures, " | ")}
	}
	return outcome
}

// report renders one scenario's attempts.
//
// Failures are listed from the first attempt that had them rather than from
// every attempt, because the same failure repeated five times is one fact.
// Latency is pooled across all of them, because one sample of a latency is not
// a measurement of anything.
func reportScenario(output io.Writer, item scenario.Scenario, attempts []scenario.Result) {
	passed := 0
	var failures []string
	for _, attempt := range attempts {
		if attempt.Passed {
			passed++
			continue
		}
		if failures == nil {
			failures = attempt.Failures
		}
	}
	mark := "FAIL"
	if passed == len(attempts) {
		mark = "ok  "
	} else if passed > 0 {
		mark = "part"
	}
	if len(attempts) > 1 {
		fmt.Fprintf(output, "  %s %-28s %d/%d  %s\n", mark, item.Name, passed, len(attempts), item.Note)
	} else {
		fmt.Fprintf(output, "  %s %-28s %s\n", mark, item.Name, item.Note)
	}
	for _, failure := range failures {
		fmt.Fprintf(output, "         %s\n", failure)
	}
	reportLatency(output, attempts)
}

// reportLatency prints the waits a person in the room would have sat through,
// pooled across every attempt.
//
// Only the ones the agent actually answered: a trigger it ignored is a
// correctness result and belongs in the checks, and averaging it in as a very
// large latency would make one silence look like a slow reply. The count is
// printed so a number drawn from two samples cannot be mistaken for one drawn
// from fifty.
func reportLatency(output io.Writer, attempts []scenario.Result) {
	var spoken, seen []float64
	for _, attempt := range attempts {
		for _, entry := range attempt.Latencies {
			if !entry.Heard || entry.MS < 0 {
				continue
			}
			if strings.HasPrefix(entry.After, "saw ") {
				seen = append(seen, entry.MS)
				continue
			}
			spoken = append(spoken, entry.MS)
		}
	}
	reportWaits(output, "heard after", spoken)
	// Sight is reported apart from speech because pooling them says something
	// false. In the visual scenario most triggers are lines of the script and
	// one is the frame the whole case is about, so a pooled median describes
	// the speech and the reader takes it for the vision path: pooled, that
	// scenario read 402ms p50, and the frame that mattered took 494ms while
	// another the agent was right to ignore sat in the tail at 9.3s.
	reportWaits(output, "saw and spoke after", seen)
}

// reportWaits prints one pooled distribution, or nothing when there is none.
func reportWaits(output io.Writer, label string, waits []float64) {
	if len(waits) == 0 {
		return
	}
	sort.Float64s(waits)
	pick := func(fraction float64) float64 {
		return waits[int(fraction*float64(len(waits)-1))]
	}
	fmt.Fprintf(output, "         %s %.0fms p50, %.0fms p90, %.0fms worst, over %d triggers\n",
		label, pick(0.5), pick(0.9), waits[len(waits)-1], len(waits))
}
