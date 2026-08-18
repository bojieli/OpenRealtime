// Command livebench runs established realtime spoken-dialogue benchmarks
// against provider APIs without a Python runtime.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/livebench"
)

const maxRetryBackoff = 30 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "livebench:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("expected a subcommand: inspect, run, rescore, or summarize")
	}
	switch arguments[0] {
	case "inspect":
		return inspect(arguments[1:])
	case "run":
		return runBenchmark(arguments[1:])
	case "summarize":
		return summarize(arguments[1:])
	case "rescore":
		return rescore(arguments[1:])
	default:
		return fmt.Errorf("unknown subcommand %q; expected inspect, run, rescore, or summarize", arguments[0])
	}
}

type repeatedStrings []string

func (values *repeatedStrings) String() string { return strings.Join(*values, ",") }

func (values *repeatedStrings) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func rescore(arguments []string) error {
	flags := flag.NewFlagSet("rescore", flag.ContinueOnError)
	manifestPath := flags.String("manifest", "", "run manifest to rescore in place")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *manifestPath == "" {
		return errors.New("--manifest is required")
	}
	manifest, err := livebench.RescoreManifest(*manifestPath)
	if err != nil {
		return err
	}
	return printSummary(*manifestPath, manifest)
}

func summarize(arguments []string) error {
	flags := flag.NewFlagSet("summarize", flag.ContinueOnError)
	var manifests repeatedStrings
	flags.Var(&manifests, "manifest", "run manifest; repeat for multiple files")
	output := flags.String("output", "", "optional JSON report path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if len(manifests) == 0 {
		return errors.New("at least one --manifest is required")
	}
	report, err := livebench.SummarizeManifests(manifests)
	if err != nil {
		return err
	}
	if *output != "" {
		if err := livebench.WriteSummaryReport(*output, report); err != nil {
			return err
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func inspect(arguments []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	datasetRoot := flags.String("dataset-root", "", "Full-Duplex-Bench v1.5 dataset root")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *datasetRoot == "" {
		return errors.New("--dataset-root is required")
	}
	samples, err := livebench.DiscoverFDB15(*datasetRoot)
	if err != nil {
		return err
	}
	counts := make(map[string]int)
	for _, sample := range samples {
		counts[sample.Scenario]++
	}
	output := struct {
		Benchmark string             `json:"benchmark"`
		Revision  string             `json:"revision"`
		Count     int                `json:"count"`
		Scenarios map[string]int     `json:"scenarios"`
		Samples   []livebench.Sample `json:"samples"`
	}{livebench.FullDuplexBenchName, livebench.FullDuplexBenchRevision, len(samples), counts, samples}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}

func runBenchmark(arguments []string) error {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	datasetRoot := flags.String("dataset-root", "", "Full-Duplex-Bench v1.5 dataset root")
	outputRoot := flags.String("output-root", "artifacts/livebench", "result root")
	provider := flags.String("provider", "", "openrealtime, openai, gemini, or groq")
	model := flags.String("model", "", "exact provider model or snapshot")
	scenario := flags.String("scenario", "", "optional scenario filter")
	sampleID := flags.String("sample-id", "", "optional sample ID filter")
	conditionList := flags.String("conditions", "overlap,clean", "comma-separated overlap and/or clean")
	limit := flags.Int("limit", 0, "maximum samples after filtering; zero means all")
	selection := flags.String("selection", "even", "when limited: even or head")
	replicates := flags.Int("replicates", 1, "number of trials per sample and condition")
	trialAttempts := flags.Int("trial-attempts", 3, "bounded attempts for each live trial")
	retryDelay := flags.Duration("retry-delay", time.Second, "base delay between live trial attempts")
	trialTimeout := flags.Duration("trial-timeout", 5*time.Minute, "timeout for each live trial")
	tailDuration := flags.Duration("tail-duration", 5*time.Second, "receive time after input ends")
	continueOnError := flags.Bool("continue-on-error", true, "record failures and continue")
	requireComplete := flags.Bool("require-complete", false, "exit nonzero after the sweep unless every planned trial completed")
	resume := flags.Bool("resume", true, "reuse matching completed trial files")
	dryRun := flags.Bool("dry-run", false, "write a run manifest without provider calls")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *datasetRoot == "" || *provider == "" {
		return errors.New("--dataset-root and --provider are required")
	}
	if *replicates < 1 || *trialAttempts < 1 || *trialAttempts > 10 || *retryDelay < 0 || *limit < 0 || *trialTimeout <= 0 || *tailDuration < 0 {
		return errors.New("replicates, trial-attempts, retry-delay, limit, trial-timeout, or tail-duration is invalid")
	}
	conditions, err := parseConditions(*conditionList)
	if err != nil {
		return err
	}
	samples, err := livebench.DiscoverFDB15(*datasetRoot)
	if err != nil {
		return err
	}
	samples = filterSamples(samples, *scenario, *sampleID)
	if len(samples) == 0 {
		return errors.New("sample filters matched no data")
	}
	if *limit > 0 && len(samples) > *limit {
		switch *selection {
		case "even":
			samples = selectEvenly(samples, *limit)
		case "head":
			samples = samples[:*limit]
		default:
			return fmt.Errorf("unknown --selection %q; expected even or head", *selection)
		}
	}

	adapter, descriptor, err := makeAdapter(*provider, *model, *tailDuration, *dryRun)
	if err != nil {
		return err
	}
	manifest := livebench.RunManifest{
		SchemaVersion: livebench.ResultSchemaVersion, CreatedAt: time.Now().UTC(),
		Benchmark: livebench.FullDuplexBenchName, Revision: livebench.FullDuplexBenchRevision,
		Descriptor: descriptor, Conditions: conditions, Replicates: *replicates,
		TrialAttempts: *trialAttempts, Samples: samples,
	}
	manifestPrefix := "run-"
	if *dryRun {
		manifestPrefix = "plan-"
	}
	manifestPath := filepath.Join(*outputRoot, manifestPrefix+sanitize(descriptor.Provider+"-"+descriptor.Model)+".json")
	if *resume && !*dryRun {
		manifest, err = resumeRunManifest(manifestPath, manifest)
		if err != nil {
			return err
		}
	}
	if err := livebench.WriteRunManifest(manifestPath, manifest); err != nil {
		return err
	}
	if *dryRun {
		return printSummary(manifestPath, manifest)
	}

	for replicate := range *replicates {
		for _, sample := range samples {
			for _, condition := range conditions {
				trialConfig := livebench.TrialConfig{OutputRoot: *outputRoot, Condition: condition, Replicate: replicate}
				attemptHistory := trialAttemptOutcomes(manifest.Attempts, sample, trialConfig)
				_, recordedSuccess, attemptErr := livebench.AnalyzeAttemptOutcomes(attemptHistory, *trialAttempts)
				if attemptErr != nil {
					return fmt.Errorf("attempt ledger for %s/%s %s replicate=%d: %w", sample.Scenario, sample.ID, condition, replicate, attemptErr)
				}
				if *resume {
					prior, exists, loadErr := livebench.LoadTrialResult(descriptor, sample, trialConfig)
					if loadErr != nil {
						return loadErr
					}
					if recordedSuccess && !exists {
						return fmt.Errorf("attempt ledger records success for %s/%s %s replicate=%d but its verified result is missing", sample.Scenario, sample.ID, condition, replicate)
					}
					if exists && recordedSuccess {
						successfulAttempt := attemptHistory[len(attemptHistory)-1].Number
						if prior.Attempt != successfulAttempt {
							return fmt.Errorf("verified result for %s/%s %s replicate=%d records attempt %d but the ledger records success at attempt %d", sample.Scenario, sample.ID, condition, replicate, prior.Attempt, successfulAttempt)
						}
						fmt.Fprintf(os.Stderr, "[%s] %s/%s %s replicate=%d (resume)\n", descriptor.Provider, sample.Scenario, sample.ID, condition, replicate)
						manifest.Completed = append(manifest.Completed, prior)
						if err := livebench.WriteRunManifest(manifestPath, manifest); err != nil {
							return err
						}
						continue
					}
					if exists {
						fmt.Fprintf(os.Stderr, "[%s] %s/%s %s replicate=%d has an uncertified prior result; rerunning within the remaining attempt budget\n", descriptor.Provider, sample.Scenario, sample.ID, condition, replicate)
					}
				}
				attemptOffset := len(attemptHistory)
				result, attempts, trialErr := runTrialAttempts(
					context.Background(), adapter, sample, trialConfig, attemptOffset, *trialAttempts, *trialTimeout, *retryDelay,
					func(attempt int) {
						fmt.Fprintf(os.Stderr, "[%s] %s/%s %s replicate=%d attempt=%d/%d\n", descriptor.Provider, sample.Scenario, sample.ID, condition, replicate, attempt, *trialAttempts)
					},
				)
				manifest.Attempts = append(manifest.Attempts, attempts...)
				if trialErr == nil {
					manifest.Completed = append(manifest.Completed, result)
				}
				if trialErr != nil {
					manifest.Failures = append(manifest.Failures, livebench.RunFailure{
						SampleID: sample.ID, Scenario: sample.Scenario, Condition: condition,
						Replicate: replicate, Attempts: attemptOffset + len(attempts), Error: trialErr.Error(),
					})
				}
				if err := livebench.WriteRunManifest(manifestPath, manifest); err != nil {
					return err
				}
				if trialErr != nil && !*continueOnError {
					return trialErr
				}
			}
		}
	}
	if err := printSummary(manifestPath, manifest); err != nil {
		return err
	}
	if *requireComplete {
		planned := len(samples) * len(conditions) * *replicates
		return livebench.ValidateRunCompletion(planned, len(manifest.Completed), len(manifest.Failures))
	}
	return nil
}

func resumeRunManifest(filename string, planned livebench.RunManifest) (livebench.RunManifest, error) {
	prior, err := livebench.ReadRunManifest(filename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return planned, nil
		}
		return livebench.RunManifest{}, err
	}
	plannedSamples, err := json.Marshal(planned.Samples)
	if err != nil {
		return livebench.RunManifest{}, fmt.Errorf("encode planned samples: %w", err)
	}
	priorSamples, err := json.Marshal(prior.Samples)
	if err != nil {
		return livebench.RunManifest{}, fmt.Errorf("encode prior samples: %w", err)
	}
	if prior.Benchmark != planned.Benchmark || prior.Revision != planned.Revision ||
		prior.Descriptor != planned.Descriptor || prior.Replicates != planned.Replicates ||
		prior.TrialAttempts != planned.TrialAttempts ||
		!slices.Equal(prior.Conditions, planned.Conditions) || string(priorSamples) != string(plannedSamples) {
		return livebench.RunManifest{}, fmt.Errorf("prior manifest %s does not match the requested run plan", filename)
	}
	planned.CreatedAt = prior.CreatedAt
	planned.Attempts = append([]livebench.RunAttempt(nil), prior.Attempts...)
	return planned, nil
}

func trialAttemptOutcomes(attempts []livebench.RunAttempt, sample livebench.Sample, config livebench.TrialConfig) []livebench.AttemptOutcome {
	var outcomes []livebench.AttemptOutcome
	for _, attempt := range attempts {
		if attempt.SampleID == sample.ID && attempt.Scenario == sample.Scenario &&
			attempt.Condition == config.Condition && attempt.Replicate == config.Replicate {
			outcomes = append(outcomes, livebench.AttemptOutcome{Number: attempt.Attempt, Succeeded: attempt.Succeeded})
		}
	}
	return outcomes
}

func runTrialAttempts(
	ctx context.Context,
	adapter livebench.Adapter,
	sample livebench.Sample,
	config livebench.TrialConfig,
	attemptOffset int,
	attemptLimit int,
	trialTimeout time.Duration,
	retryDelay time.Duration,
	onStart func(int),
) (livebench.TrialResult, []livebench.RunAttempt, error) {
	attemptNumbers, err := livebench.RemainingAttemptNumbers(attemptOffset, attemptLimit)
	if err != nil {
		return livebench.TrialResult{}, nil, err
	}
	if len(attemptNumbers) == 0 {
		return livebench.TrialResult{}, nil, fmt.Errorf("attempt budget exhausted: used=%d maximum=%d", attemptOffset, attemptLimit)
	}
	var records []livebench.RunAttempt
	var lastErr error
	for index, attempt := range attemptNumbers {
		if onStart != nil {
			onStart(attempt)
		}
		config.Attempt = attempt
		startedAt := time.Now().UTC()
		trialCtx, cancel := context.WithTimeout(ctx, trialTimeout)
		result, runErr := livebench.RunTrial(trialCtx, adapter, sample, config)
		cancel()
		finishedAt := time.Now().UTC()
		record := livebench.RunAttempt{
			SampleID: sample.ID, Scenario: sample.Scenario, Condition: config.Condition,
			Replicate: config.Replicate, Attempt: attempt, StartedAt: startedAt, FinishedAt: finishedAt,
			DurationMS: float64(finishedAt.Sub(startedAt)) / float64(time.Millisecond), Succeeded: runErr == nil,
		}
		if runErr != nil {
			record.Error = runErr.Error()
		}
		records = append(records, record)
		if runErr == nil {
			return result, records, nil
		}
		lastErr = runErr
		if index < len(attemptNumbers)-1 {
			delay := retryBackoff(retryDelay, attempt)
			if err := sleepContext(ctx, delay); err != nil {
				return livebench.TrialResult{}, records, err
			}
		}
	}
	return livebench.TrialResult{}, records, lastErr
}

func retryBackoff(base time.Duration, failedAttempts int) time.Duration {
	if base <= 0 {
		return 0
	}
	delay := base
	for attempt := 1; attempt < failedAttempts; attempt++ {
		if delay >= maxRetryBackoff/2 {
			return maxRetryBackoff
		}
		delay *= 2
	}
	return min(delay, maxRetryBackoff)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func makeAdapter(provider, model string, tail time.Duration, dryRun bool) (livebench.Adapter, livebench.Descriptor, error) {
	switch strings.ToLower(provider) {
	case "openrealtime":
		if model == "" {
			model = "openrealtime-local"
		}
		endpoint := os.Getenv("OPENREALTIME_BASE_URL")
		if endpoint == "" {
			endpoint = "ws://127.0.0.1:8765/v1/realtime"
		}
		config := livebench.OpenAIConfig{
			APIKey: os.Getenv("OPENREALTIME_API_KEY"), Model: model,
			Endpoint: endpoint, TailDuration: tail,
			Provider: "openrealtime", Architecture: "canonical-local-asr-fast-slow-tts",
			Profile: "fdb-v1.5-openai-realtime-adapter-i1-qg-v1",
		}
		descriptor := livebench.Descriptor{
			Provider: config.Provider, Model: model, Transport: "websocket-openai-realtime",
			Architecture: config.Architecture, Profile: config.Profile,
			InputSampleRate: 24_000, OutputSampleRate: 24_000,
		}
		if dryRun {
			return nil, descriptor, nil
		}
		adapter, err := livebench.NewOpenAIAdapter(config)
		return adapter, descriptor, err
	case "openai":
		if model == "" {
			model = livebench.DefaultGPT4oRealtimeModel
		}
		descriptor := livebench.Descriptor{Provider: "openai", Model: model, Transport: "websocket-openai-realtime", Architecture: "native-audio-to-audio", Profile: "fdb-v1.5-server-vad-v1", InputSampleRate: 24_000, OutputSampleRate: 24_000}
		if dryRun {
			return nil, descriptor, nil
		}
		adapter, err := livebench.NewOpenAIAdapter(livebench.OpenAIConfig{APIKey: os.Getenv("OPENAI_API_KEY"), Model: model, TailDuration: tail})
		return adapter, descriptor, err
	case "gemini", "google":
		if model == "" {
			model = livebench.DefaultGeminiLiveModel
		}
		descriptor := livebench.Descriptor{Provider: "google", Model: model, Transport: "websocket-v1beta", Architecture: "native-audio-to-audio", Profile: "fdb-v1.5-minimal-multisession-v1", InputSampleRate: 16_000, OutputSampleRate: 24_000}
		if dryRun {
			return nil, descriptor, nil
		}
		adapter, err := livebench.NewGeminiAdapter(livebench.GeminiConfig{APIKey: os.Getenv("GEMINI_API_KEY"), Model: model, TailDuration: tail})
		return adapter, descriptor, err
	case "groq":
		if model != "" {
			return nil, livebench.Descriptor{}, errors.New("Groq is a three-model cascade; configure exact components in code rather than passing one ambiguous --model")
		}
		combinedModel := strings.Join([]string{livebench.DefaultGroqSTTModel, livebench.DefaultGroqLLMModel, livebench.DefaultGroqTTSModel}, "+")
		descriptor := livebench.Descriptor{Provider: "groq", Model: combinedModel, Transport: "https", Architecture: "cascaded-stt-llm-tts", Profile: "fdb-v1.5-energy-vad-cascade-v2", InputSampleRate: 16_000, OutputSampleRate: 24_000}
		if dryRun {
			return nil, descriptor, nil
		}
		adapter, err := livebench.NewGroqAdapter(livebench.GroqConfig{APIKey: os.Getenv("GROQ_API_KEY"), TailDuration: tail})
		return adapter, descriptor, err
	default:
		return nil, livebench.Descriptor{}, fmt.Errorf("unsupported provider %q", provider)
	}
}

func parseConditions(value string) ([]string, error) {
	var result []string
	for _, condition := range strings.Split(value, ",") {
		condition = strings.TrimSpace(condition)
		if condition != "overlap" && condition != "clean" {
			return nil, fmt.Errorf("unknown condition %q", condition)
		}
		if !slices.Contains(result, condition) {
			result = append(result, condition)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("at least one condition is required")
	}
	return result, nil
}

func filterSamples(samples []livebench.Sample, scenario, id string) []livebench.Sample {
	result := samples[:0]
	for _, sample := range samples {
		if scenario != "" && sample.Scenario != scenario {
			continue
		}
		if id != "" && sample.ID != id {
			continue
		}
		result = append(result, sample)
	}
	return result
}

func selectEvenly(samples []livebench.Sample, count int) []livebench.Sample {
	if count >= len(samples) {
		return samples
	}
	if count == 1 {
		return samples[len(samples)/2 : len(samples)/2+1]
	}
	result := make([]livebench.Sample, 0, count)
	for index := range count {
		position := index * (len(samples) - 1) / (count - 1)
		result = append(result, samples[position])
	}
	return result
}

func sanitize(value string) string {
	value = strings.Map(func(character rune) rune {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			return character
		}
		return '-'
	}, value)
	return strings.Trim(value, "-")
}

func printSummary(manifestPath string, manifest livebench.RunManifest) error {
	output := struct {
		Manifest  string `json:"manifest"`
		Samples   int    `json:"samples"`
		Completed int    `json:"completed"`
		Failures  int    `json:"failures"`
	}{manifestPath, len(manifest.Samples), len(manifest.Completed), len(manifest.Failures)}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(output)
}
