package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
)

// runScenario drives the end-to-end interaction suite.
//
// It sits beside bench rather than under it because it scores a different
// thing. A benchmark plays somebody else's recording and asks whether the task
// was completed; a scenario scripts a conversation and asks whether the agent
// spoke at the right moments. A system can pass every benchmark here while
// talking over everybody in it.
func runScenario(arguments []string, output io.Writer) (returnErr error) {
	flags := flag.NewFlagSet("scenario", flag.ContinueOnError)
	flags.SetOutput(output)
	var caseNames []string
	flags.Func("case", "exact case name; repeat to select a diagnostic subset matching -launch-profile (default: all cases)", func(name string) error {
		caseNames = append(caseNames, name)
		return nil
	})
	var (
		list             = flags.Bool("list", false, "list exact scenario names and stop; no profile or services needed")
		endpoint         = flags.String("url", "ws://127.0.0.1:8765/v1/realtime", "session endpoint")
		tokenEnv         = flags.String("token-env", "", "environment variable holding the session credential")
		model            = flags.String("model", "", "model to request; empty selects the server's own")
		speech           = flags.String("speech-url", "http://127.0.0.1:8081/v1/audio/speech", "speech endpoint that gives the participants voices")
		voice            = flags.String("speech-model", "fishaudio/s2-pro", "speech model")
		transcribe       = flags.String("transcribe-url", "", "OpenAI-shaped transcription endpoint used to score what the agent was audibly saying; required by the resumed-after-interruption case, which is scored against the agent's own recorded waveform rather than against the transcript the wire delivered")
		transcribeModel  = flags.String("transcribe-model", "Systran/faster-whisper-base.en", "recogniser the scoring endpoint should load")
		timeout          = flags.Duration("timeout", 3*time.Minute, "bound on one scenario")
		record           = flags.String("record", "", "write the timed record of each scenario to this file")
		reviewDir        = flags.String("review-dir", "", "create this new directory with per-attempt stereo WAVs, a manifest, and a Markdown review")
		reviewReceipt    = flags.String("review-receipt", "", "write the graph-native source receipt outside -review-dir (default: <review-dir>.receipt.json)")
		repeat           = flags.Int("repeat", 1, "runs per scenario; latency from one run is noise, so a latency claim needs several")
		experiment       = flags.String("architecture-manifest", "", "versioned P/T/C/N architecture experiment manifest")
		architectureCell = flags.String("architecture-cell", "", "cell name in -architecture-manifest")
		launchProfile    = flags.String("launch-profile", "", "strict graph launch profile for a graph-native scenario checklist")
		inspectionGraph  = flags.String("inspection-graph", "", benchmarkInspectionGraphFlagHelp)
	)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("scenario accepts flags only")
	}
	contract, err := graphnative.BuildContract(caseNames...)
	if err != nil {
		return err
	}
	if *list {
		for _, item := range contract.Cases {
			fmt.Fprintln(output, item.Name)
		}
		return nil
	}
	if strings.TrimSpace(*experiment) == "" {
		switch {
		case strings.TrimSpace(*architectureCell) != "":
			return errors.New("-architecture-cell requires -architecture-manifest")
		case strings.TrimSpace(*launchProfile) != "":
			return errors.New("-launch-profile requires a graph-native architecture cell")
		case strings.TrimSpace(*reviewReceipt) != "":
			return errors.New("-review-receipt requires a graph-native architecture cell")
		case strings.TrimSpace(*inspectionGraph) != "":
			return errors.New("-inspection-graph requires a graph-native -execution requirement")
		default:
			return errors.New("scenario requires -architecture-manifest with a reviewed graph-native cell")
		}
	}
	manifest, err := archbench.Read(*experiment)
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
	selectedCell, err := manifest.Cell(name)
	if err != nil {
		return err
	}
	if selectedCell.Availability != archbench.AvailabilityRunnable {
		return fmt.Errorf("architecture cell %q is unavailable: %s",
			selectedCell.Name, selectedCell.UnavailableReason)
	}
	requirement, err := scenarioGraphExecutionRequirement(selectedCell)
	if err != nil {
		return err
	}

	selected := scenarioGraphCases(contract)

	runs := max(1, *repeat)
	if strings.TrimSpace(*reviewDir) == "" {
		return errors.New("graph-native scenario execution requires -review-dir")
	}
	graphSelection, err := prepareScenarioGraphSelection(
		*launchProfile, selectedCell, requirement, runs, caseNames...,
	)
	if err != nil {
		return err
	}
	started := archbench.NewResult(manifest, selectedCell, len(selected)*runs)
	architectureResult := &started
	fmt.Fprintf(output, "  architecture %s  F52=%s\n", selectedCell.Name, selectedCell.Architecture.Level)
	if len(selected) != len(scenario.Suite()) {
		fmt.Fprintf(output, "  DIAGNOSTIC SUBSET: %d/%d cases; no full-suite acceptance credit\n",
			len(selected), len(scenario.Suite()))
	}

	config := bench.SessionConfig{
		Endpoint: *endpoint, Model: *model,
		Timeout: *timeout, Quiet: true, CaptureRuntimeEvidence: true,
	}
	config, err = configureScenarioSession(
		config, requirement, *inspectionGraph, *tokenEnv, os.Getenv,
	)
	if err != nil {
		return err
	}

	speaker := scenario.SpeechVoice{
		Endpoint: *speech, Model: *voice, Default: "default",
		// Deliberately a second endpoint rather than the session's own
		// recogniser. The claim it serves is that nothing involved in
		// producing the audio gets to say what was in it.
		Listen: scenario.Hearing{Endpoint: *transcribe, Model: *transcribeModel},
		// The second party gets a different voice. A phone menu that sounds
		// exactly like the caller removes the difficulty the case exists to
		// pose.
		Voices: map[string]string{"other": "alloy"},
	}

	var graphChecklist graphnative.Checklist
	graphReview, err := newScenarioGraphReviewBundle(
		*reviewDir, runs, requirement, []string{config.Token}, selected...,
	)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "  review       %s\n", graphReview.Directory())
	defer func() {
		if err := graphReview.Close(); err != nil {
			returnErr = errors.Join(returnErr, err)
		}
	}()
	outcome, err := executeScenarioGraphChecklist(
		context.Background(), graphSelection, requirement,
		selectedCell.Architecture.Profile, runs, *timeout, graphReview,
		speaker, config, graphnative.NewLiveExecutor,
	)
	if err != nil {
		return err
	}
	graphChecklist = outcome.Checklist
	if err := appendScenarioGraphArchitectureAttempts(architectureResult, outcome.Attempts); err != nil {
		return err
	}
	reportScenarioGraphOutcome(output, outcome, runs)

	architectureResult.Finish()
	fmt.Fprintf(output, "  measured    %d/%d tasks completed\n",
		architectureResult.Measurement.Summary.Completed, architectureResult.Measurement.Expected)
	if err := architectureResult.Reportable(); err != nil {
		fmt.Fprintf(output, "  NOT REPORTABLE: %v\n", err)
	} else {
		fmt.Fprintln(output, "  reportable architecture cell")
	}
	architecturePayload, err := marshalScenarioArchitectureResult(*architectureResult)
	if err != nil {
		return err
	}

	if strings.TrimSpace(*record) != "" {
		if err := writeScenarioArchitectureRecord(*record, architecturePayload); err != nil {
			return fmt.Errorf("write the architecture record: %w", err)
		}
	}
	origin, err := scenarioGraphSourceOrigin(config.Endpoint, config.Transport)
	if err != nil {
		return err
	}
	receiptPath := strings.TrimSpace(*reviewReceipt)
	if receiptPath == "" {
		receiptPath = graphReview.Directory() + ".receipt.json"
	} else {
		receiptPath, err = filepath.Abs(receiptPath)
		if err != nil {
			return fmt.Errorf("resolve graph-native source receipt path: %w", err)
		}
	}
	receipt, err := graphReview.Finalize(
		context.Background(), graphChecklist, architecturePayload, origin, receiptPath,
	)
	if err != nil {
		return fmt.Errorf("finalize graph-native scenario source bundle: %w", err)
	}
	fmt.Fprintf(output, "  source       %s\n", receipt.ManifestSHA256)
	fmt.Fprintf(output, "  receipt      %s\n", receiptPath)

	return nil
}

func scenarioGraphExecutionRequirement(cell archbench.Cell) (bench.ExecutionRequirement, error) {
	if cell.Execution.Kind != bench.ExecutionGraphNative {
		return bench.ExecutionRequirement{}, fmt.Errorf(
			"scenario architecture cell %q requires an exact graph-native execution requirement",
			cell.Name,
		)
	}
	if err := cell.Execution.Validate(); err != nil {
		return bench.ExecutionRequirement{}, fmt.Errorf(
			"scenario architecture cell %q execution requirement: %w", cell.Name, err,
		)
	}
	return cell.Execution, nil
}

func marshalScenarioArchitectureResult(result archbench.Result) ([]byte, error) {
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the architecture record: %w", err)
	}
	return append(payload, '\n'), nil
}

func writeScenarioArchitectureRecord(path string, payload []byte) error {
	if strings.TrimSpace(path) == "" || len(payload) == 0 {
		return errors.New("an architecture result needs a path and payload")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, payload, 0o644)
}

// configureScenarioSession selects evidence for a CLI-owned Realtime session.
// The ordinary bearer remains SessionConfig.Token. GraphAttestor receives the
// distinct, ephemeral management capability negotiated by each session via
// AttestationRequest; no management token is accepted from flags or retained
// in a result. The shared preparation path reconciles reviewed Graph IR before
// getenv, speech synthesis, or Realtime protocol work.
func configureScenarioSession(
	config bench.SessionConfig,
	requirement bench.ExecutionRequirement,
	inspectionGraph string,
	tokenEnvironment string,
	getenv func(string) string,
) (bench.SessionConfig, error) {
	if requirement.Kind != bench.ExecutionGraphNative {
		return bench.SessionConfig{}, errors.New(
			"scenario execution requires a graph-native -execution requirement",
		)
	}
	attestor, deploymentToken, err := configureSessionBenchmarkAttestor(
		requirement, inspectionGraph, config.Endpoint, tokenEnvironment, getenv,
	)
	if err != nil {
		return bench.SessionConfig{}, err
	}
	config.Token = deploymentToken
	config.RuntimeAttestor = attestor
	if attestor != nil {
		config.CaptureRuntimeEvidence = true
	}
	return config, nil
}

// scenarioSessionForTask binds observed routes and live resolution to the
// exact scenario attempt. A shared attestor is safe; the server-issued
// InspectionAccess still selects the current session, never this task label.
func scenarioSessionForTask(config bench.SessionConfig, taskID string) bench.SessionConfig {
	if config.RuntimeAttestor != nil {
		config.AttestationScope = taskID
	}
	return config
}

// scenarioTask adapts one owned interaction scenario to the generic benchmark
// result without throwing away its richer raw record.
func scenarioTask(id string, result scenario.Result, runErr error) bench.TaskOutcome {
	outcome := bench.TaskOutcome{ID: id, Completed: runErr == nil, Passed: result.Passed}
	outcome.AttachExecution(result.Transcript)
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
