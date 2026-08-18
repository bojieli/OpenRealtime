// Command fdbench runs the released FD-Bench conversations through a standard
// OpenAI Realtime-compatible endpoint and emits upstream-compatible evidence.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/fdbench"
	"github.com/bojieli/OpenRealtime/livebench"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fdbench:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("expected inspect or run subcommand")
	}
	switch arguments[0] {
	case "inspect":
		return inspect(arguments[1:])
	case "run":
		return runBenchmark(arguments[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; expected inspect or run", arguments[0])
	}
}

func inspect(arguments []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	datasetRoot := flags.String("dataset-root", "", "released FD-Bench input root")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *datasetRoot == "" {
		return errors.New("--dataset-root is required")
	}
	samples, err := fdbench.Discover(*datasetRoot)
	if err != nil {
		return err
	}
	type cellSummary struct {
		Samples   int     `json:"samples"`
		DurationH float64 `json:"duration_h"`
		Missing   []int   `json:"missing_conversation_ids,omitempty"`
	}
	byCell := make(map[string]*cellSummary)
	seen := make(map[string]map[int]struct{})
	for _, sample := range samples {
		if byCell[sample.Cell] == nil {
			byCell[sample.Cell] = &cellSummary{}
			seen[sample.Cell] = make(map[int]struct{})
		}
		byCell[sample.Cell].Samples++
		byCell[sample.Cell].DurationH += sample.InputDurationMS / 3_600_000
		seen[sample.Cell][sample.Conversation] = struct{}{}
	}
	for cell, ids := range seen {
		for id := 1; id <= 293; id++ {
			if _, ok := ids[id]; !ok {
				byCell[cell].Missing = append(byCell[cell].Missing, id)
			}
		}
	}
	var totalHours float64
	for _, sample := range samples {
		totalHours += sample.InputDurationMS / 3_600_000
	}
	return encode(os.Stdout, map[string]any{
		"benchmark": fdbench.BenchmarkName, "revision": fdbench.BenchmarkRevision,
		"dataset_revision": fdbench.DatasetRevision, "samples": len(samples), "duration_h": totalHours, "cells": byCell,
	})
}

type attempt struct {
	Sample     string    `json:"sample"`
	Number     int       `json:"number"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	DurationMS float64   `json:"duration_ms"`
	Succeeded  bool      `json:"succeeded"`
	Error      string    `json:"error,omitempty"`
}

type failure struct {
	Sample   string `json:"sample"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
}

type runManifest struct {
	SchemaVersion   string               `json:"schema_version"`
	CreatedAt       time.Time            `json:"created_at"`
	UpdatedAt       time.Time            `json:"updated_at"`
	Benchmark       string               `json:"benchmark"`
	Revision        string               `json:"revision"`
	DatasetRevision string               `json:"dataset_revision"`
	ProfileSHA256   string               `json:"profile_sha256"`
	Descriptor      livebench.Descriptor `json:"descriptor"`
	Samples         []fdbench.Sample     `json:"samples"`
	Completed       []string             `json:"completed"`
	Failures        []failure            `json:"failures,omitempty"`
	Attempts        []attempt            `json:"attempts,omitempty"`
}

func runBenchmark(arguments []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	datasetRoot := flags.String("dataset-root", "", "released FD-Bench input root")
	outputRoot := flags.String("output-root", ".runtime/benchmark-runs/fd-bench/openrealtime-v1/output", "audio and per-conversation result root")
	runRoot := flags.String("run-root", ".runtime/benchmark-runs/fd-bench/openrealtime-v1", "run-manifest root")
	endpoint := flags.String("endpoint", environmentDefault("OPENREALTIME_BASE_URL", "ws://127.0.0.1:8765/v1/realtime"), "OpenAI Realtime-compatible WebSocket endpoint")
	apiKey := flags.String("api-key", os.Getenv("OPENREALTIME_API_KEY"), "endpoint bearer token; defaults to OPENREALTIME_API_KEY")
	model := flags.String("model", "openrealtime-local", "model query and evidence label")
	voice := flags.String("voice", "tau-lisa-brenner-v1", "registered local Fish voice")
	provider := flags.String("provider-label", "openrealtime", "provider evidence label")
	instructions := flags.String("instructions", "You are a helpful spoken-dialogue assistant. Respond naturally and concisely.", "session instruction shared by every condition")
	cellsValue := flags.String("cells", "", "optional comma-separated exact condition directories")
	limit := flags.Int("limit", 0, "maximum conversations after filtering; zero means all")
	chunkDuration := flags.Duration("chunk-duration", 20*time.Millisecond, "Realtime input frame duration")
	tailDuration := flags.Duration("tail-duration", 10*time.Second, "fixed post-input collection window used by the upstream clients")
	finalizationSilence := flags.Duration("vad-finalization-silence", 600*time.Millisecond, "silence streamed inside the fixed tail so server VAD closes a final utterance")
	trialTimeout := flags.Duration("trial-timeout", 3*time.Minute, "timeout for each infrastructure attempt")
	trialAttempts := flags.Int("trial-attempts", 3, "bounded infrastructure attempts per conversation")
	retryDelay := flags.Duration("retry-delay", time.Second, "base delay between attempts")
	resume := flags.Bool("resume", true, "reuse matching completed result files")
	continueOnError := flags.Bool("continue-on-error", true, "record exhausted failures and continue")
	requireComplete := flags.Bool("require-complete", false, "exit nonzero after the sweep unless every planned conversation completed")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *datasetRoot == "" || strings.TrimSpace(*apiKey) == "" {
		return errors.New("--dataset-root and a non-empty --api-key/OPENREALTIME_API_KEY are required")
	}
	if *limit < 0 || *trialAttempts < 1 || *trialAttempts > 10 || *trialTimeout <= 0 || *retryDelay < 0 || *tailDuration <= 0 || *finalizationSilence < 0 || *finalizationSilence >= *tailDuration {
		return errors.New("limit, attempt, timeout, retry, or tail configuration is invalid")
	}
	samples, err := fdbench.Discover(*datasetRoot)
	if err != nil {
		return err
	}
	cells := splitCSV(*cellsValue)
	samples = fdbench.Select(samples, cells, *limit)
	if len(samples) == 0 {
		return errors.New("FD-Bench filters matched no conversations")
	}
	adapter, err := livebench.NewOpenAIAdapter(livebench.OpenAIConfig{
		APIKey: *apiKey, Endpoint: *endpoint, Model: *model, Voice: *voice, Instructions: *instructions,
		ChunkDuration: *chunkDuration, TailDuration: *tailDuration - *finalizationSilence,
		FinalizationSilence: *finalizationSilence, Provider: *provider,
		Architecture: "canonical-local-asr-fast-slow-tts", Profile: "fd-bench-standard-realtime-v1",
	})
	if err != nil {
		return err
	}
	profileHash := hashJSON(map[string]any{
		"profile": "fd-bench-standard-realtime-v1", "instructions": *instructions,
		"chunk_duration": chunkDuration.String(), "post_input_collection": tailDuration.String(),
		"vad_finalization_silence": finalizationSilence.String(), "voice": *voice,
	})
	manifestPath := filepath.Join(*runRoot, "run-"+safeName(*provider)+".json")
	manifest := runManifest{
		SchemaVersion: "1.0.0", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Benchmark: fdbench.BenchmarkName, Revision: fdbench.BenchmarkRevision, DatasetRevision: fdbench.DatasetRevision,
		ProfileSHA256: profileHash, Descriptor: adapter.Descriptor(), Samples: samples,
	}
	if err := os.MkdirAll(*runRoot, 0o755); err != nil {
		return fmt.Errorf("create FD-Bench run root: %w", err)
	}
	if *resume {
		if prior, found, loadErr := loadManifest(manifestPath); loadErr != nil {
			return loadErr
		} else if found {
			if err := validatePrior(prior, manifest); err != nil {
				return fmt.Errorf("resume %s: %w", manifestPath, err)
			}
			manifest.CreatedAt = prior.CreatedAt
			manifest.Attempts = prior.Attempts
		}
	}
	if err := publishManifest(manifestPath, &manifest); err != nil {
		return err
	}
	for _, sample := range samples {
		label := sample.Cell + "/" + sample.ID
		if *resume {
			_, found, loadErr := fdbench.LoadCompleted(sample, *outputRoot, *provider, *model)
			if loadErr != nil {
				return loadErr
			}
			if found {
				fmt.Fprintf(os.Stderr, "[fd-bench] %s (resume)\n", label)
				manifest.Completed = appendUnique(manifest.Completed, label)
				manifest.Failures = removeFailure(manifest.Failures, label)
				if err := publishManifest(manifestPath, &manifest); err != nil {
					return err
				}
				continue
			}
		}
		var lastErr error
		for number := 1; number <= *trialAttempts; number++ {
			fmt.Fprintf(os.Stderr, "[fd-bench] %s attempt=%d/%d\n", label, number, *trialAttempts)
			started := time.Now().UTC()
			ctx, cancel := context.WithTimeout(context.Background(), *trialTimeout)
			_, runErr := fdbench.RunSample(ctx, adapter, sample, *outputRoot, *provider)
			cancel()
			finished := time.Now().UTC()
			record := attempt{Sample: label, Number: number, StartedAt: started, FinishedAt: finished, DurationMS: float64(finished.Sub(started)) / float64(time.Millisecond), Succeeded: runErr == nil}
			if runErr != nil {
				record.Error = runErr.Error()
			}
			manifest.Attempts = append(manifest.Attempts, record)
			lastErr = runErr
			if runErr == nil {
				manifest.Completed = appendUnique(manifest.Completed, label)
				manifest.Failures = removeFailure(manifest.Failures, label)
				break
			}
			if number < *trialAttempts {
				timer := time.NewTimer(time.Duration(number) * *retryDelay)
				<-timer.C
			}
		}
		if lastErr != nil {
			manifest.Failures = append(removeFailure(manifest.Failures, label), failure{Sample: label, Attempts: *trialAttempts, Error: lastErr.Error()})
		}
		if err := publishManifest(manifestPath, &manifest); err != nil {
			return err
		}
		if lastErr != nil && !*continueOnError {
			return lastErr
		}
	}
	if err := encode(os.Stdout, map[string]any{"manifest": manifestPath, "planned": len(samples), "completed": len(manifest.Completed), "failures": len(manifest.Failures), "profile_sha256": profileHash}); err != nil {
		return err
	}
	if *requireComplete {
		return livebench.ValidateRunCompletion(len(samples), len(manifest.Completed), len(manifest.Failures))
	}
	return nil
}

func splitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func safeName(value string) string {
	value = strings.TrimSpace(value)
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			result.WriteRune(character)
		} else {
			result.WriteByte('-')
		}
	}
	return result.String()
}

func hashJSON(value any) string {
	data, _ := json.Marshal(value)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func loadManifest(filename string) (runManifest, bool, error) {
	data, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return runManifest{}, false, nil
	}
	if err != nil {
		return runManifest{}, false, fmt.Errorf("read prior FD-Bench manifest: %w", err)
	}
	var manifest runManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return runManifest{}, false, fmt.Errorf("decode prior FD-Bench manifest: %w", err)
	}
	return manifest, true, nil
}

func validatePrior(prior, current runManifest) error {
	if prior.SchemaVersion != current.SchemaVersion || prior.Benchmark != current.Benchmark || prior.Revision != current.Revision ||
		prior.DatasetRevision != current.DatasetRevision || prior.ProfileSHA256 != current.ProfileSHA256 ||
		!reflect.DeepEqual(prior.Descriptor, current.Descriptor) || !reflect.DeepEqual(prior.Samples, current.Samples) {
		return errors.New("prior manifest benchmark, profile, descriptor, or ordered population differs")
	}
	return nil
}

func publishManifest(filename string, manifest *runManifest) error {
	manifest.UpdatedAt = time.Now().UTC()
	sort.Strings(manifest.Completed)
	temporary, err := os.CreateTemp(filepath.Dir(filename), ".fdbench-run-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary FD-Bench manifest: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(manifest); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, filename)
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func removeFailure(values []failure, sample string) []failure {
	result := values[:0]
	for _, value := range values {
		if value.Sample != sample {
			result = append(result, value)
		}
	}
	return result
}

func environmentDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func encode(file *os.File, value any) error {
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
