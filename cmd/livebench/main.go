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
	provider := flags.String("provider", "", "openai, gemini, or groq")
	model := flags.String("model", "", "exact provider model or snapshot")
	scenario := flags.String("scenario", "", "optional scenario filter")
	sampleID := flags.String("sample-id", "", "optional sample ID filter")
	conditionList := flags.String("conditions", "overlap,clean", "comma-separated overlap and/or clean")
	limit := flags.Int("limit", 0, "maximum samples after filtering; zero means all")
	selection := flags.String("selection", "even", "when limited: even or head")
	replicates := flags.Int("replicates", 1, "number of trials per sample and condition")
	trialTimeout := flags.Duration("trial-timeout", 5*time.Minute, "timeout for each live trial")
	tailDuration := flags.Duration("tail-duration", 5*time.Second, "receive time after input ends")
	continueOnError := flags.Bool("continue-on-error", true, "record failures and continue")
	resume := flags.Bool("resume", true, "reuse matching completed trial files")
	dryRun := flags.Bool("dry-run", false, "write a run manifest without provider calls")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if *datasetRoot == "" || *provider == "" {
		return errors.New("--dataset-root and --provider are required")
	}
	if *replicates < 1 || *limit < 0 || *trialTimeout <= 0 || *tailDuration < 0 {
		return errors.New("replicates, limit, trial-timeout, or tail-duration is invalid")
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
		Descriptor: descriptor, Conditions: conditions, Replicates: *replicates, Samples: samples,
	}
	manifestPath := filepath.Join(*outputRoot, "run-"+sanitize(descriptor.Provider+"-"+descriptor.Model)+".json")
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
				if *resume {
					prior, exists, loadErr := livebench.LoadTrialResult(descriptor, sample, trialConfig)
					if loadErr != nil {
						return loadErr
					}
					if exists {
						fmt.Fprintf(os.Stderr, "[%s] %s/%s %s replicate=%d (resume)\n", descriptor.Provider, sample.Scenario, sample.ID, condition, replicate)
						manifest.Completed = append(manifest.Completed, prior)
						if err := livebench.WriteRunManifest(manifestPath, manifest); err != nil {
							return err
						}
						continue
					}
				}
				fmt.Fprintf(os.Stderr, "[%s] %s/%s %s replicate=%d\n", descriptor.Provider, sample.Scenario, sample.ID, condition, replicate)
				trialCtx, cancel := context.WithTimeout(context.Background(), *trialTimeout)
				result, trialErr := livebench.RunTrial(trialCtx, adapter, sample, trialConfig)
				cancel()
				if trialErr != nil {
					manifest.Failures = append(manifest.Failures, livebench.RunFailure{
						SampleID: sample.ID, Scenario: sample.Scenario, Condition: condition,
						Replicate: replicate, Error: trialErr.Error(),
					})
				} else {
					manifest.Completed = append(manifest.Completed, result)
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
	return printSummary(manifestPath, manifest)
}

func makeAdapter(provider, model string, tail time.Duration, dryRun bool) (livebench.Adapter, livebench.Descriptor, error) {
	switch strings.ToLower(provider) {
	case "openai":
		if model == "" {
			model = livebench.DefaultGPT4oRealtimeModel
		}
		descriptor := livebench.Descriptor{Provider: "openai", Model: model, Transport: "websocket", Architecture: "native-audio-to-audio", Profile: "fdb-v1.5-server-vad-v1", InputSampleRate: 24_000, OutputSampleRate: 24_000}
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
