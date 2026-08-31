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
	benchreview "github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/gemini"
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
	var (
		endpoint         = flags.String("url", "ws://127.0.0.1:8765/v1/realtime", "session endpoint")
		tokenEnv         = flags.String("token-env", "", "environment variable holding the session credential")
		model            = flags.String("model", "", "model to request; empty selects the server's own")
		speech           = flags.String("speech-url", "http://127.0.0.1:8081/v1/audio/speech", "speech endpoint that gives the participants voices")
		voice            = flags.String("speech-model", "fishaudio/s2-pro", "speech model")
		only             = flags.String("only", "", "run one scenario by name")
		timeout          = flags.Duration("timeout", 3*time.Minute, "bound on one scenario")
		record           = flags.String("record", "", "write the graph-native architecture result to this file")
		reviewDir        = flags.String("review-dir", "", "fresh graph-native source directory; omission reserves a private path under ./artifacts")
		reviewReceipt    = flags.String("review-receipt", "", "write the graph-native source receipt outside -review-dir (default: <review-dir>.receipt.json)")
		repeat           = flags.Int("repeat", 1, "runs per scenario; latency from one run is noise, so a latency claim needs several")
		experiment       = flags.String("architecture-manifest", "", "versioned P/T/C/N architecture experiment manifest")
		architectureCell = flags.String("architecture-cell", "", "cell name in -architecture-manifest")
		launchProfile    = flags.String("launch-profile", "", "strict graph launch profile for a graph-native scenario checklist")
		inspectionGraph  = flags.String("inspection-graph", "", benchmarkInspectionGraphFlagHelp)
		reviewProvider   = flags.String("review-provider", gemini.RegistrationName, "offline reviewer plug-in (exactly google.gemini-3.7-flash)")
		reviewParallel   = flags.Int("review-parallel", 4, "concurrent offline Gemini reviews (1..16)")
		reviewTimeout    = flags.Duration("review-timeout", 12*time.Minute, "bound each offline Gemini review and retention")
	)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("scenario accepts flags only")
	}
	if strings.TrimSpace(*experiment) == "" {
		return errors.New("scenario benchmark execution requires a graph-native -architecture-manifest")
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
	requirement := selectedCell.Execution

	fullSuite := scenario.Suite()
	runs := max(1, *repeat)
	if requirement.Kind != bench.ExecutionGraphNative {
		return errors.New("scenario benchmark execution requires an exact graph-native execution requirement")
	}
	resolvedReviewDirectory, err := resolveBenchmarkReviewDestination(
		"scenario", *reviewDir, false, automaticBenchmarkArtifactPath,
	)
	if err != nil {
		return err
	}
	*reviewDir = resolvedReviewDirectory
	if strings.TrimSpace(*only) != "" {
		return errors.New("graph-native scenario checklists require the complete scenario suite; remove -only")
	}
	graphSelection, err := prepareScenarioGraphSelection(
		*launchProfile, selectedCell, requirement, runs,
	)
	if err != nil {
		return err
	}
	started := archbench.NewResult(manifest, selectedCell, len(fullSuite)*runs)
	architectureResult := &started
	fmt.Fprintf(output, "  architecture %s  F52=%s\n", selectedCell.Name, selectedCell.Architecture.Level)

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
		// The second party gets a different voice. A phone menu that sounds
		// exactly like the caller removes the difficulty the case exists to
		// pose.
		Voices: map[string]string{"other": "alloy"},
	}

	receiptPath, err := resolveScenarioSourceReceiptPath(*reviewDir, *reviewReceipt)
	if err != nil {
		return err
	}
	evaluationOptions, registry, err := prepareAutomaticScenarioEvaluation(
		context.Background(), *reviewDir, receiptPath, *reviewProvider,
		*reviewParallel, *reviewTimeout,
	)
	if err != nil {
		return err
	}
	graphReview, err := newScenarioGraphReviewBundle(
		*reviewDir, runs, requirement, []string{config.Token},
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
	graphChecklist := outcome.Checklist
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
	receipt, err := graphReview.Finalize(
		context.Background(), graphChecklist, architecturePayload, origin, receiptPath,
	)
	if err != nil {
		return fmt.Errorf("finalize graph-native scenario source bundle: %w", err)
	}
	fmt.Fprintf(output, "  source       %s\n", receipt.ManifestSHA256)
	fmt.Fprintf(output, "  receipt      %s\n", receiptPath)
	if err := runScenarioEvaluationContext(
		context.Background(), scenarioEvaluationArguments(evaluationOptions), output, registry,
	); err != nil {
		return fmt.Errorf("run mandatory exact Gemini 3.7 Flash scenario review: %w", err)
	}
	return nil
}

func resolveScenarioSourceReceiptPath(sourceDirectory, configured string) (string, error) {
	if configured != "" && strings.TrimSpace(configured) != configured {
		return "", errors.New("graph-native source receipt path is noncanonical")
	}
	if configured == "" {
		configured = sourceDirectory + ".receipt.json"
	}
	resolved, err := filepath.Abs(configured)
	if err != nil {
		return "", errors.New("resolve graph-native source receipt path")
	}
	resolved = filepath.Clean(resolved)
	if resolved == filepath.Dir(resolved) {
		return "", errors.New("graph-native source receipt path cannot be a filesystem root")
	}
	return resolved, nil
}

func prepareAutomaticScenarioEvaluation(
	ctx context.Context, sourceDirectory, sourceReceipt, provider string,
	parallel int, timeout time.Duration,
) (scenarioEvaluationRunOptions, *benchreview.Registry, error) {
	if ctx == nil {
		return scenarioEvaluationRunOptions{}, nil, errors.New("prepare scenario evaluation: nil context")
	}
	if err := ctx.Err(); err != nil {
		return scenarioEvaluationRunOptions{}, nil, err
	}
	if provider != gemini.RegistrationName || gemini.ModelID != "gemini-3.7-flash" ||
		gemini.Descriptor().Model != gemini.ModelID {
		return scenarioEvaluationRunOptions{}, nil, errors.New(
			"scenario review provider must be exact google.gemini-3.7-flash",
		)
	}
	options, err := resolveScenarioEvaluationOptions(scenarioEvaluationRunOptions{
		SourceDirectory: sourceDirectory,
		SourceReceipt:   sourceReceipt,
		OutputDirectory: sourceDirectory + ".evaluations",
		OutputReceipt:   sourceDirectory + ".evaluations.receipt.json",
		Provider:        provider,
		Parallel:        parallel,
		Timeout:         timeout,
	})
	if err != nil {
		return scenarioEvaluationRunOptions{}, nil, err
	}
	for _, candidate := range []struct{ label, path string }{
		{"source directory", options.SourceDirectory},
		{"source receipt", options.SourceReceipt},
	} {
		label, path := candidate.label, candidate.path
		if _, err := os.Lstat(path); err == nil {
			return scenarioEvaluationRunOptions{}, nil, fmt.Errorf(
				"scenario review %s create-only path already exists", label,
			)
		} else if !errors.Is(err, os.ErrNotExist) {
			return scenarioEvaluationRunOptions{}, nil, fmt.Errorf("inspect scenario review %s", label)
		}
		if err := validateScenarioEvaluationAncestors(filepath.Dir(path)); err != nil {
			return scenarioEvaluationRunOptions{}, nil, err
		}
	}
	registry, err := benchreview.NewRegistry([]benchreview.Registration{
		gemini.Registration(gemini.EnvironmentAPIKey),
	})
	if err != nil {
		return scenarioEvaluationRunOptions{}, nil, err
	}
	lease, err := registry.Open(ctx, provider)
	if err != nil {
		return scenarioEvaluationRunOptions{}, nil, errors.New(
			"preflight exact Gemini 3.7 Flash scenario reviewer credential",
		)
	}
	descriptor := lease.Descriptor()
	if descriptor.Provider != "google" || descriptor.Model != gemini.ModelID ||
		descriptor.API != gemini.Descriptor().API ||
		descriptor.APIRevision != gemini.Descriptor().APIRevision {
		_ = lease.Close()
		return scenarioEvaluationRunOptions{}, nil, errors.New(
			"preflight scenario reviewer identity differs from exact Gemini 3.7 Flash",
		)
	}
	if err := lease.Close(); err != nil {
		return scenarioEvaluationRunOptions{}, nil, errors.New(
			"close exact Gemini 3.7 Flash scenario reviewer preflight",
		)
	}
	return options, registry, nil
}

func scenarioEvaluationArguments(options scenarioEvaluationRunOptions) []string {
	return []string{
		"-source-dir", options.SourceDirectory,
		"-source-receipt", options.SourceReceipt,
		"-out", options.OutputDirectory,
		"-out-receipt", options.OutputReceipt,
		"-provider", options.Provider,
		"-parallel", fmt.Sprint(options.Parallel),
		"-timeout", options.Timeout.String(),
	}
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
			"scenario benchmark sessions require an exact graph-native execution requirement",
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
