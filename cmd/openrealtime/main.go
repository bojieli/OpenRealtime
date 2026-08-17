// Command openrealtime exposes protocol conformance and deterministic replay tools.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/baseline"
	m2experiment "github.com/bojieli/OpenRealtime/experiments/m2"
	m3experiment "github.com/bojieli/OpenRealtime/experiments/m3"
	m4experiment "github.com/bojieli/OpenRealtime/experiments/m4"
	m5experiment "github.com/bojieli/OpenRealtime/experiments/m5"
	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/internal/simtime"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	benchmarkrelease "github.com/bojieli/OpenRealtime/release"
	"github.com/bojieli/OpenRealtime/replay"
	"github.com/bojieli/OpenRealtime/study"
	"github.com/bojieli/OpenRealtime/trace"
	"github.com/bojieli/OpenRealtime/visualization/ablation"
	"github.com/bojieli/OpenRealtime/visualization/demonstrations"
	"github.com/bojieli/OpenRealtime/visualization/frontier"
	"github.com/bojieli/OpenRealtime/visualization/timeline"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "openrealtime:", err)
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return usageError()
	}
	switch arguments[0] {
	case "fixture":
		return runFixture(arguments[1:], output)
	case "replay":
		return runReplay(arguments[1:], output)
	case "protocol":
		return runProtocol(arguments[1:], output)
	case "trace":
		return runTrace(arguments[1:], output)
	case "benchmark":
		return runBenchmark(arguments[1:], output)
	case "study":
		return runStudy(arguments[1:], output)
	case "release":
		return runRelease(arguments[1:], output)
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: openrealtime <fixture|replay|protocol|trace|benchmark|study|release> <command> [options]")
}

func runStudy(arguments []string, output io.Writer) error {
	if len(arguments) == 0 || arguments[0] != "build" {
		return errors.New("usage: openrealtime study build --root <repository> --output <study.json>")
	}
	flags := flag.NewFlagSet("study build", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	root := flags.String("root", ".", "repository root")
	outputPath := flags.String("output", "", "comparative study JSON output")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *outputPath == "" {
		return errors.New("study build requires --output")
	}
	result, err := study.Build(*root)
	if err != nil {
		return err
	}
	if err := writeAtomicJSON(*outputPath, result); err != nil {
		return err
	}
	return writeJSON(output, map[string]any{
		"output": *outputPath, "release_id": result.ReleaseID,
		"conditions": len(result.Conditions), "paired_effects": len(result.PairedEffects), "claims": len(result.Claims),
	})
}

func runRelease(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime release <build|verify> [options]")
	}
	switch arguments[0] {
	case "build":
		flags := flag.NewFlagSet("release build", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		root := flags.String("root", ".", "repository root")
		outputPath := flags.String("output", "", "release manifest JSON output")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *outputPath == "" {
			return errors.New("release build requires --output")
		}
		manifest, err := benchmarkrelease.Build(*root)
		if err != nil {
			return err
		}
		if err := writeAtomicJSON(*outputPath, manifest); err != nil {
			return err
		}
		return writeJSON(output, map[string]any{
			"output": *outputPath, "release_id": manifest.ReleaseID, "files": len(manifest.Files),
		})
	case "verify":
		flags := flag.NewFlagSet("release verify", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		root := flags.String("root", ".", "repository root")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("release verify requires one manifest path")
		}
		manifest, err := benchmarkrelease.Load(flags.Arg(0))
		if err != nil {
			return err
		}
		if err := benchmarkrelease.Verify(*root, manifest); err != nil {
			return err
		}
		return writeJSON(output, map[string]any{
			"release_id": manifest.ReleaseID, "files": len(manifest.Files), "verified": true,
		})
	default:
		return errors.New("usage: openrealtime release <build|verify> [options]")
	}
}

func runFixture(arguments []string, output io.Writer) error {
	if len(arguments) == 0 || arguments[0] != "generate" {
		return errors.New("usage: openrealtime fixture generate <output.wav>")
	}
	flags := flag.NewFlagSet("fixture generate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("fixture generate requires one output path")
	}
	path := flags.Arg(0)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	digest, err := audio.GenerateFixture(path)
	if err != nil {
		return err
	}
	return writeJSON(output, map[string]any{
		"path":           path,
		"sha256":         hex.EncodeToString(digest[:]),
		"sample_rate_hz": audio.OpenAIPCMSampleRate,
		"sample_count":   audio.OpenAIPCMSampleRate,
		"duration_ns":    uint64(1_000_000_000),
	})
}

func runReplay(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime replay <input.wav> --events <events.jsonl> --trace <trace.jsonl> [--frame-ms 20]")
	}
	inputPath := arguments[0]
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	eventsPath := flags.String("events", "", "OpenAI-compatible client event JSONL output")
	tracePath := flags.String("trace", "", "timed OpenRealtime trace JSONL output")
	sessionID := flags.String("session-id", "m0-replay", "deterministic session ID")
	frameMS := flags.Uint("frame-ms", 20, "input frame duration in milliseconds")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *eventsPath == "" || *tracePath == "" {
		return errors.New("usage: openrealtime replay <input.wav> --events <events.jsonl> --trace <trace.jsonl> [--frame-ms 20]")
	}
	if *frameMS > uint(^uint32(0)) {
		return errors.New("frame-ms exceeds uint32")
	}
	events, err := newAtomicOutput(*eventsPath)
	if err != nil {
		return err
	}
	defer events.Abort()
	traces, err := newAtomicOutput(*tracePath)
	if err != nil {
		return err
	}
	defer traces.Abort()
	summary, err := replay.WAV(inputPath, replay.Options{
		SessionID:       *sessionID,
		FrameDurationMS: uint32(*frameMS),
		Events:          events.File,
		Trace:           traces.File,
	})
	if err != nil {
		return err
	}
	if err := events.Commit(); err != nil {
		return err
	}
	if err := traces.Commit(); err != nil {
		return err
	}
	return writeJSON(output, summary)
}

func runProtocol(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime protocol <inventory|validate>")
	}
	switch arguments[0] {
	case "inventory":
		definitions := openaiwire.Definitions()
		counts := make(map[string]int)
		for _, definition := range definitions {
			counts[string(definition.Profile)+"/"+string(definition.Direction)]++
		}
		return writeJSON(output, map[string]any{
			"definition_count": len(definitions),
			"counts":           counts,
			"definitions":      definitions,
		})
	case "validate":
		flags := flag.NewFlagSet("protocol validate", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		profileValue := flags.String("profile", "realtime", "realtime, transcription, translation, or beta")
		directionValue := flags.String("direction", "client", "client or server")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("protocol validate requires one JSONL path")
		}
		profile, err := parseProfile(*profileValue)
		if err != nil {
			return err
		}
		direction, err := parseDirection(*directionValue)
		if err != nil {
			return err
		}
		file, err := os.Open(flags.Arg(0))
		if err != nil {
			return err
		}
		defer file.Close()
		validator := openaiwire.NewValidator()
		count, err := eachJSONLine(file, func(line []byte, number uint64) error {
			message, err := openaiwire.Decode(line)
			if err != nil {
				return fmt.Errorf("line %d: %w", number, err)
			}
			if err := validator.Validate(profile, direction, message); err != nil {
				return fmt.Errorf("line %d: %w", number, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		return writeJSON(output, map[string]any{"events": count, "profile": profile, "direction": direction})
	default:
		return errors.New("usage: openrealtime protocol <inventory|validate>")
	}
}

func runTrace(arguments []string, output io.Writer) error {
	if len(arguments) < 2 || (arguments[0] != "validate" && arguments[0] != "summarize") {
		return errors.New("usage: openrealtime trace <validate|summarize> <trace.jsonl>")
	}
	file, err := os.Open(arguments[1])
	if err != nil {
		return err
	}
	defer file.Close()
	validator := openaiwire.NewValidator()
	typeCounts := make(map[string]uint64)
	var firstNS, lastNS uint64
	var sessionID string
	count, err := trace.Read(file, func(record trace.Record) error {
		message, err := openaiwire.Decode(record.Message)
		if err != nil {
			return err
		}
		if err := validator.Validate(record.Profile, record.Direction, message); err != nil {
			return err
		}
		if sessionID == "" {
			sessionID = record.SessionID
			firstNS = record.MonotonicNS
		}
		lastNS = record.MonotonicNS
		typeCounts[string(message.Type())]++
		return nil
	})
	if err != nil {
		return err
	}
	if arguments[0] == "validate" {
		return writeJSON(output, map[string]any{"records": count})
	}
	keys := make([]string, 0, len(typeCounts))
	for key := range typeCounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	orderedCounts := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		orderedCounts = append(orderedCounts, map[string]any{"type": key, "count": typeCounts[key]})
	}
	return writeJSON(output, map[string]any{
		"session_id":  sessionID,
		"records":     count,
		"duration_ns": lastNS - firstNS,
		"type_counts": orderedCounts,
	})
}

func runBenchmark(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime benchmark <m1|m2|m3|m4|m5> [options]")
	}
	switch arguments[0] {
	case "m1":
		return runBenchmarkM1(arguments[1:], output)
	case "m2":
		return runBenchmarkM2(arguments[1:], output)
	case "m3":
		return runBenchmarkM3(arguments[1:], output)
	case "m4":
		return runBenchmarkM4(arguments[1:], output)
	case "m5":
		return runBenchmarkM5(arguments[1:], output)
	default:
		return errors.New("usage: openrealtime benchmark <m1|m2|m3|m4|m5> [options]")
	}
}

func runBenchmarkM5(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("benchmark m5", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fixturePath := flags.String("fixture", "", "24 kHz PCM16 fixture")
	demonstrationPath := flags.String("demonstrations", "", "symbolic translation and game manifest")
	outputDirectory := flags.String("output", "", "benchmark artifact directory")
	trials := flags.Uint64("trials", 30, "number of deterministic trials per condition")
	seed := flags.Uint64("seed", 20260817, "base deterministic random seed")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *fixturePath == "" || *demonstrationPath == "" || *outputDirectory == "" {
		return errors.New("benchmark m5 requires --fixture, --demonstrations, and --output")
	}
	if err := prepareEmptyDirectory(*outputDirectory); err != nil {
		return err
	}
	manifest, err := reference.LoadDemonstrations(*demonstrationPath)
	if err != nil {
		return err
	}
	report, err := m5experiment.Run(context.Background(), m5experiment.Config{
		FixturePath: *fixturePath, DemonstrationPath: *demonstrationPath,
		Demonstrations: manifest, Trials: *trials, Seed: *seed,
	})
	if err != nil {
		return err
	}
	if err := writeAtomicJSON(filepath.Join(*outputDirectory, "report.json"), report); err != nil {
		return err
	}
	translationTraceDirectory := filepath.Join(*outputDirectory, "traces", "translation")
	gameTraceDirectory := filepath.Join(*outputDirectory, "traces", "game")
	if err := os.MkdirAll(translationTraceDirectory, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(gameTraceDirectory, 0o755); err != nil {
		return err
	}
	translationSummary := make(map[string]map[string]any, len(report.Translation.Conditions))
	for _, condition := range report.Translation.Conditions {
		for _, trial := range condition.Trials {
			path := filepath.Join(translationTraceDirectory, fmt.Sprintf("%s-trial-%04d.jsonl", condition.Policy, trial.Index))
			if err := writeAtomicTrace(path, trial.Trace); err != nil {
				return err
			}
		}
		translationSummary[string(condition.Policy)] = map[string]any{
			"mean_lag_ns": condition.MeanLag, "completion_lag_ns": condition.CompletionLag,
			"quality_score": condition.Quality, "failure_count": condition.FailureCount,
			"compute_units": condition.Compute,
		}
	}
	gameSummary := make(map[string]map[string]any, len(report.Game.Conditions))
	for _, condition := range report.Game.Conditions {
		for _, trial := range condition.Trials {
			path := filepath.Join(gameTraceDirectory, fmt.Sprintf("%s-trial-%04d.jsonl", condition.Condition, trial.Index))
			if err := writeAtomicTrace(path, trial.Trace); err != nil {
				return err
			}
		}
		gameSummary[string(condition.Condition)] = map[string]any{
			"reaction_latency_ns": condition.ReactionLatency, "quality_score": condition.Quality,
			"failure_count": condition.FailureCount, "compute_units": condition.Compute,
		}
	}
	visualization, err := newAtomicOutput(filepath.Join(*outputDirectory, "demonstrations.html"))
	if err != nil {
		return err
	}
	if err := demonstrations.Render(visualization.File, report); err != nil {
		visualization.Abort()
		return err
	}
	if err := visualization.Commit(); err != nil {
		visualization.Abort()
		return err
	}
	if err := writeM5Timeline(filepath.Join(*outputDirectory, "timeline-translation-stable-trial-0000.html"), "M5 stable incremental translation — trial 0000", report.Translation.Conditions[1].Trials[0].Trace); err != nil {
		return err
	}
	if err := writeM5Timeline(filepath.Join(*outputDirectory, "timeline-game-microturn-trial-0000.html"), "M5 rapid game microturn — trial 0000", report.Game.Conditions[1].Trials[0].Trace); err != nil {
		return err
	}
	return writeJSON(output, map[string]any{
		"output": *outputDirectory, "trials_per_condition": *trials, "experiment": report.Experiment,
		"translation": translationSummary, "game": gameSummary,
	})
}

func writeM5Timeline(path, title string, records []trace.Record) error {
	artifact, err := newAtomicOutput(path)
	if err != nil {
		return err
	}
	if err := timeline.Render(artifact.File, title, records); err != nil {
		artifact.Abort()
		return err
	}
	if err := artifact.Commit(); err != nil {
		artifact.Abort()
		return err
	}
	return nil
}

func runBenchmarkM4(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("benchmark m4", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	workloadPath := flags.String("workload", "", "symbolic difficult-question workload")
	outputDirectory := flags.String("output", "", "benchmark artifact directory")
	trials := flags.Uint64("trials", 30, "number of deterministic trials per task")
	seed := flags.Uint64("seed", 20260817, "base deterministic random seed")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *workloadPath == "" || *outputDirectory == "" {
		return errors.New("benchmark m4 requires --workload and --output")
	}
	if err := prepareEmptyDirectory(*outputDirectory); err != nil {
		return err
	}
	workload, err := reference.LoadDifficultWorkload(*workloadPath)
	if err != nil {
		return err
	}
	report, err := m4experiment.Run(context.Background(), m4experiment.Config{
		WorkloadPath: *workloadPath, Workload: workload, Trials: *trials, Seed: *seed,
	})
	if err != nil {
		return err
	}
	if err := writeAtomicJSON(filepath.Join(*outputDirectory, "report.json"), report); err != nil {
		return err
	}
	visualization, err := newAtomicOutput(filepath.Join(*outputDirectory, "frontier.html"))
	if err != nil {
		return err
	}
	if err := frontier.Render(visualization.File, report); err != nil {
		visualization.Abort()
		return err
	}
	if err := visualization.Commit(); err != nil {
		visualization.Abort()
		return err
	}
	summaries := make(map[string]map[string]any, len(report.Conditions))
	for _, condition := range report.Conditions {
		summaries[string(condition.Kind)] = map[string]any{
			"first_truthful_progress_ns": condition.FirstTruthfulProgress,
			"final_answer_ns":            condition.FinalAnswer, "quality_score": condition.Quality,
			"compute_units": condition.Compute, "task_success_count": condition.TaskSuccessCount,
			"truth_violation_count": condition.TruthViolationCount,
		}
	}
	return writeJSON(output, map[string]any{
		"output": *outputDirectory, "trials_per_task": *trials,
		"experiment": report.Experiment, "conditions": summaries,
	})
}

func runBenchmarkM1(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("benchmark m1", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fixturePath := flags.String("fixture", "", "24 kHz PCM16 fixture")
	manifestPath := flags.String("manifest", "", "reference adapter manifest")
	outputDirectory := flags.String("output", "", "benchmark artifact directory")
	trials := flags.Uint64("trials", 30, "number of paired deterministic trials")
	seed := flags.Uint64("seed", 20260817, "base deterministic random seed")
	frameMS := flags.Uint("frame-ms", 20, "input frame duration in milliseconds")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *fixturePath == "" || *manifestPath == "" || *outputDirectory == "" {
		return errors.New("benchmark m1 requires --fixture, --manifest, and --output")
	}
	if *frameMS == 0 || *frameMS > uint(^uint32(0)) {
		return errors.New("frame-ms must fit a positive uint32")
	}
	if err := prepareEmptyDirectory(*outputDirectory); err != nil {
		return err
	}
	manifest, err := reference.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	report, err := baseline.Run(context.Background(), baseline.Config{
		FixturePath: *fixturePath,
		Manifest:    manifest,
		Trials:      *trials,
		Seed:        *seed,
		FrameMS:     uint32(*frameMS),
		Timing:      baseline.DefaultTimingModel(),
	})
	if err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(*outputDirectory, "traces"), 0o755); err != nil {
		return err
	}
	if err := writeAtomicJSON(filepath.Join(*outputDirectory, "report.json"), report); err != nil {
		return err
	}
	for _, trial := range report.Trials {
		path := filepath.Join(*outputDirectory, "traces", fmt.Sprintf("trial-%04d.jsonl", trial.Index))
		if err := writeAtomicTrace(path, trial.Trace); err != nil {
			return err
		}
	}
	timelineArtifact, err := newAtomicOutput(filepath.Join(*outputDirectory, "timeline-trial-0000.html"))
	if err != nil {
		return err
	}
	if err := timeline.Render(timelineArtifact.File, "M1 endpointed reference — trial 0000", report.Trials[0].Trace); err != nil {
		timelineArtifact.Abort()
		return err
	}
	if err := timelineArtifact.Commit(); err != nil {
		timelineArtifact.Abort()
		return err
	}
	return writeJSON(output, map[string]any{
		"output": *outputDirectory, "trials": len(report.Trials),
		"condition": report.Condition, "timing_mode": report.TimingMode,
		"distributions": report.Distributions,
	})
}

func runBenchmarkM3(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("benchmark m3", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fixturePath := flags.String("fixture", "", "24 kHz PCM16 fixture")
	manifestPath := flags.String("manifest", "", "reference adapter manifest")
	outputDirectory := flags.String("output", "", "benchmark artifact directory")
	trials := flags.Uint64("trials", 30, "number of deterministic trials per scenario")
	seed := flags.Uint64("seed", 20260817, "base deterministic random seed")
	frameMS := flags.Uint("frame-ms", 20, "input frame duration in milliseconds")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *fixturePath == "" || *manifestPath == "" || *outputDirectory == "" {
		return errors.New("benchmark m3 requires --fixture, --manifest, and --output")
	}
	if *frameMS == 0 || *frameMS > uint(^uint32(0)) {
		return errors.New("frame-ms must fit a positive uint32")
	}
	if err := prepareEmptyDirectory(*outputDirectory); err != nil {
		return err
	}
	manifest, err := reference.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	report, err := m3experiment.Run(context.Background(), m3experiment.Config{
		FixturePath: *fixturePath, Manifest: manifest, Trials: *trials,
		Seed: *seed, FrameMS: uint32(*frameMS),
	})
	if err != nil {
		return err
	}
	if err := writeAtomicJSON(filepath.Join(*outputDirectory, "report.json"), report); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(*outputDirectory, "traces"), 0o755); err != nil {
		return err
	}
	summaries := make(map[string]map[string]any, len(report.Scenarios))
	for _, condition := range report.Scenarios {
		for _, trial := range condition.Trials {
			path := filepath.Join(*outputDirectory, "traces", fmt.Sprintf("%s-trial-%04d.jsonl", condition.Scenario, trial.Index))
			if err := writeAtomicTrace(path, trial.Trace); err != nil {
				return err
			}
		}
		timelineArtifact, err := newAtomicOutput(filepath.Join(*outputDirectory, fmt.Sprintf("timeline-%s-trial-0000.html", condition.Scenario)))
		if err != nil {
			return err
		}
		if err := timeline.Render(timelineArtifact.File, fmt.Sprintf("M3 %s — trial 0000", condition.Scenario), condition.Trials[0].Trace); err != nil {
			timelineArtifact.Abort()
			return err
		}
		if err := timelineArtifact.Commit(); err != nil {
			timelineArtifact.Abort()
			return err
		}
		summaries[string(condition.Scenario)] = map[string]any{
			"stop_latency_ns": condition.StopLatency, "false_stop_count": condition.FalseStopCount,
			"failure_to_stop_count": condition.FailureToStopCount, "repair_count": condition.RepairCount,
			"history_violation_count": condition.HistoryViolationCount,
		}
	}
	return writeJSON(output, map[string]any{
		"output": *outputDirectory, "trials_per_scenario": *trials,
		"experiment": report.Experiment, "scenarios": summaries,
	})
}

func runBenchmarkM2(arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("benchmark m2", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fixturePath := flags.String("fixture", "", "24 kHz PCM16 fixture")
	manifestPath := flags.String("manifest", "", "reference adapter manifest")
	outputDirectory := flags.String("output", "", "benchmark artifact directory")
	trials := flags.Uint64("trials", 30, "number of paired deterministic trials")
	seed := flags.Uint64("seed", 20260817, "base deterministic random seed")
	frameMS := flags.Uint("frame-ms", 20, "input frame duration in milliseconds")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *fixturePath == "" || *manifestPath == "" || *outputDirectory == "" {
		return errors.New("benchmark m2 requires --fixture, --manifest, and --output")
	}
	if *frameMS == 0 || *frameMS > uint(^uint32(0)) {
		return errors.New("frame-ms must fit a positive uint32")
	}
	if err := prepareEmptyDirectory(*outputDirectory); err != nil {
		return err
	}
	manifest, err := reference.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	report, err := m2experiment.Run(context.Background(), m2experiment.Config{
		FixturePath: *fixturePath, Manifest: manifest, Trials: *trials, Seed: *seed,
		FrameMS: uint32(*frameMS), Timing: simtime.DefaultModel(), Policies: m2experiment.DefaultPolicies(),
	})
	if err != nil {
		return err
	}
	if err := writeAtomicJSON(filepath.Join(*outputDirectory, "report.json"), report); err != nil {
		return err
	}
	visualization, err := newAtomicOutput(filepath.Join(*outputDirectory, "ablation.html"))
	if err != nil {
		return err
	}
	if err := ablation.Render(visualization.File, report); err != nil {
		visualization.Abort()
		return err
	}
	if err := visualization.Commit(); err != nil {
		visualization.Abort()
		return err
	}
	summaries := make(map[string]map[string]any, len(report.Conditions))
	for _, condition := range report.Conditions {
		summaries[condition.Policy.Name] = map[string]any{
			"observed_latency_ns":         condition.Distributions["observed_latency_ns"],
			"observed_minus_baseline_ns":  condition.SignedDistributions["observed_minus_baseline_ns"],
			"prepared_pre_endpoint_count": condition.PreparedPreEndpointCount,
		}
	}
	return writeJSON(output, map[string]any{
		"output": *outputDirectory, "trials": *trials, "experiment": report.Experiment, "conditions": summaries,
	})
}

func prepareEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("create benchmark output directory: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect benchmark output directory: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("benchmark output directory must be empty to prevent stale evidence")
	}
	return nil
}

func parseProfile(value string) (openaiwire.Profile, error) {
	profile := openaiwire.Profile(value)
	switch profile {
	case openaiwire.ProfileRealtime, openaiwire.ProfileTranscription, openaiwire.ProfileTranslation, openaiwire.ProfileBeta:
		return profile, nil
	default:
		return "", fmt.Errorf("unknown protocol profile %q", value)
	}
}

func parseDirection(value string) (openaiwire.Direction, error) {
	direction := openaiwire.Direction(value)
	switch direction {
	case openaiwire.DirectionClient, openaiwire.DirectionServer:
		return direction, nil
	default:
		return "", fmt.Errorf("unknown event direction %q", value)
	}
}

func eachJSONLine(input io.Reader, visit func([]byte, uint64) error) (uint64, error) {
	reader := bufio.NewReader(input)
	var count uint64
	for {
		line, err := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			count++
			if visitErr := visit(line, count); visitErr != nil {
				return count - 1, visitErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return count, err
		}
	}
	if count == 0 {
		return 0, errors.New("JSONL input must contain at least one event")
	}
	return count, nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeAtomicJSON(path string, value any) error {
	artifact, err := newAtomicOutput(path)
	if err != nil {
		return err
	}
	defer artifact.Abort()
	if err := writeJSON(artifact.File, value); err != nil {
		return err
	}
	return artifact.Commit()
}

func writeAtomicTrace(path string, records []trace.Record) error {
	if len(records) == 0 {
		return errors.New("cannot write an empty trace")
	}
	artifact, err := newAtomicOutput(path)
	if err != nil {
		return err
	}
	defer artifact.Abort()
	writer := trace.NewWriter(artifact.File)
	for _, record := range records {
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		return err
	}
	return artifact.Commit()
}

type atomicOutput struct {
	File      *os.File
	temporary string
	target    string
	committed bool
}

func newAtomicOutput(target string) (*atomicOutput, error) {
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(directory, ".openrealtime-*")
	if err != nil {
		return nil, err
	}
	return &atomicOutput{File: file, temporary: file.Name(), target: target}, nil
}

func (output *atomicOutput) Commit() error {
	if output.committed {
		return nil
	}
	if err := output.File.Sync(); err != nil {
		return err
	}
	if err := output.File.Chmod(0o644); err != nil {
		return err
	}
	if err := output.File.Close(); err != nil {
		return err
	}
	if err := os.Rename(output.temporary, output.target); err != nil {
		return err
	}
	output.committed = true
	return nil
}

func (output *atomicOutput) Abort() {
	if output == nil || output.committed {
		return
	}
	output.File.Close()
	_ = os.Remove(output.temporary)
}
