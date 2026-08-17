package livebench

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type Distribution struct {
	N             int     `json:"n"`
	Mean          float64 `json:"mean"`
	Median        float64 `json:"median"`
	P95           float64 `json:"p95"`
	MeanCILower95 float64 `json:"mean_ci_lower_95"`
	MeanCIUpper95 float64 `json:"mean_ci_upper_95"`
}

type ConditionSummary struct {
	Descriptor        Descriptor   `json:"descriptor"`
	Scenario          string       `json:"scenario"`
	Condition         string       `json:"condition"`
	Completed         int          `json:"completed"`
	Failures          int          `json:"failures"`
	UsageComplete     int          `json:"usage_complete"`
	ConnectionSetupMS Distribution `json:"connection_setup_ms"`
	ConnectionCount   Distribution `json:"connection_count"`
	TransportRetries  Distribution `json:"transport_retries"`
	InputDurationMS   Distribution `json:"input_duration_ms"`
	ElapsedMS         Distribution `json:"elapsed_ms"`
	FirstAudioMS      Distribution `json:"first_audio_ms"`
	OutputAudioMS     Distribution `json:"output_audio_ms"`
	FirstOutputVADMS  Distribution `json:"first_output_vad_ms"`
	OverlapSpeechMS   Distribution `json:"overlap_speech_ms"`
	StopLatencyMS     Distribution `json:"stop_latency_ms"`
	ResponseLatencyMS Distribution `json:"response_latency_ms"`
	InputTokens       Distribution `json:"input_tokens"`
	OutputTokens      Distribution `json:"output_tokens"`
	InputAudioTokens  Distribution `json:"input_audio_tokens"`
	OutputAudioTokens Distribution `json:"output_audio_tokens"`
}

type PairedSummary struct {
	Descriptor              Descriptor   `json:"descriptor"`
	Scenario                string       `json:"scenario"`
	Pairs                   int          `json:"pairs"`
	FirstAudioDeltaMS       Distribution `json:"first_audio_delta_ms_overlap_minus_clean"`
	OutputAudioDeltaMS      Distribution `json:"output_audio_delta_ms_overlap_minus_clean"`
	FirstOutputVADDeltaMS   Distribution `json:"first_output_vad_delta_ms_overlap_minus_clean"`
	OverlapSpeechDeltaMS    Distribution `json:"overlap_speech_delta_ms_overlap_minus_clean"`
	StopLatencyDeltaMS      Distribution `json:"stop_latency_delta_ms_overlap_minus_clean"`
	ResponseLatencyDeltaMS  Distribution `json:"response_latency_delta_ms_overlap_minus_clean"`
	ConnectionCountDelta    Distribution `json:"connection_count_delta_overlap_minus_clean"`
	SpeechDuringOverlapRate float64      `json:"speech_during_overlap_rate"`
}

type SummaryReport struct {
	SchemaVersion string             `json:"schema_version"`
	GeneratedAt   time.Time          `json:"generated_at"`
	Method        string             `json:"method"`
	Scorer        string             `json:"scorer"`
	Manifests     []string           `json:"manifests"`
	Conditions    []ConditionSummary `json:"conditions"`
	Pairs         []PairedSummary    `json:"pairs"`
}

func SummarizeManifests(filenames []string) (SummaryReport, error) {
	if len(filenames) == 0 {
		return SummaryReport{}, errors.New("at least one run manifest is required")
	}
	report := SummaryReport{
		SchemaVersion: ResultSchemaVersion, GeneratedAt: time.Now().UTC(),
		Method:    "descriptive statistics and deterministic 10,000-resample percentile bootstrap 95% confidence intervals",
		Scorer:    EnergyVADName + "; Full-Duplex-Bench get_timing.py interval and millisecond de-duplication definitions",
		Manifests: append([]string(nil), filenames...),
	}
	type groupData struct {
		descriptor Descriptor
		scenario   string
		condition  string
		results    []TrialResult
		failures   int
	}
	groups := make(map[string]*groupData)
	type pairData struct {
		descriptor Descriptor
		scenario   string
		results    map[string]map[string]TrialResult
	}
	pairs := make(map[string]*pairData)
	for _, filename := range filenames {
		manifest, err := readRunManifest(filename)
		if err != nil {
			return SummaryReport{}, err
		}
		for _, result := range manifest.Completed {
			key := summaryKey(result.Session.Descriptor, result.Sample.Scenario, result.Condition)
			group := groups[key]
			if group == nil {
				group = &groupData{descriptor: result.Session.Descriptor, scenario: result.Sample.Scenario, condition: result.Condition}
				groups[key] = group
			}
			group.results = append(group.results, result)
			pairKey := summaryKey(result.Session.Descriptor, result.Sample.Scenario, "")
			pairGroup := pairs[pairKey]
			if pairGroup == nil {
				pairGroup = &pairData{descriptor: result.Session.Descriptor, scenario: result.Sample.Scenario, results: make(map[string]map[string]TrialResult)}
				pairs[pairKey] = pairGroup
			}
			trialKey := result.Sample.ID + "/" + replicateFromTrialID(result.TrialID)
			if pairGroup.results[trialKey] == nil {
				pairGroup.results[trialKey] = make(map[string]TrialResult)
			}
			pairGroup.results[trialKey][result.Condition] = result
		}
		for _, failure := range manifest.Failures {
			key := summaryKey(manifest.Descriptor, failure.Scenario, failure.Condition)
			group := groups[key]
			if group == nil {
				group = &groupData{descriptor: manifest.Descriptor, scenario: failure.Scenario, condition: failure.Condition}
				groups[key] = group
			}
			group.failures++
		}
	}
	for _, group := range groups {
		summary := ConditionSummary{
			Descriptor: group.descriptor, Scenario: group.scenario, Condition: group.condition,
			Completed: len(group.results), Failures: group.failures,
		}
		for _, result := range group.results {
			if result.Session.Usage.Complete {
				summary.UsageComplete++
			}
		}
		summary.ConnectionSetupMS = distributionOf(group.results, func(result TrialResult) *float64 { return pointer(result.Session.ConnectionSetupMS) })
		summary.ConnectionCount = distributionOf(group.results, func(result TrialResult) *float64 { return pointer(float64(result.Session.ConnectionCount)) })
		summary.TransportRetries = distributionOf(group.results, func(result TrialResult) *float64 {
			return pointer(float64(countEvents(result.Session.Events, "transport.connect_retry")))
		})
		summary.InputDurationMS = distributionOf(group.results, func(result TrialResult) *float64 { return pointer(result.Session.InputDurationMS) })
		summary.ElapsedMS = distributionOf(group.results, func(result TrialResult) *float64 { return pointer(result.Session.ElapsedMS) })
		summary.FirstAudioMS = distributionOf(group.results, func(result TrialResult) *float64 { return result.Session.FirstAudioMS })
		summary.OutputAudioMS = distributionOf(group.results, func(result TrialResult) *float64 { return pointer(result.Session.OutputAudioMS) })
		summary.FirstOutputVADMS = distributionOf(group.results, func(result TrialResult) *float64 { return result.Timing.FirstOutputMS })
		summary.OverlapSpeechMS = distributionOf(group.results, func(result TrialResult) *float64 { return pointer(result.Timing.OverlapSpeechMS) })
		summary.StopLatencyMS = distributionOf(group.results, func(result TrialResult) *float64 { return result.Timing.MeanStopLatencyMS })
		summary.ResponseLatencyMS = distributionOf(group.results, func(result TrialResult) *float64 { return result.Timing.MeanResponseLatencyMS })
		summary.InputTokens = distributionOf(group.results, func(result TrialResult) *float64 {
			return usageValue(result.Session.Usage, result.Session.Usage.InputTokens)
		})
		summary.OutputTokens = distributionOf(group.results, func(result TrialResult) *float64 {
			return usageValue(result.Session.Usage, result.Session.Usage.OutputTokens)
		})
		summary.InputAudioTokens = distributionOf(group.results, func(result TrialResult) *float64 {
			return usageValue(result.Session.Usage, result.Session.Usage.InputAudioTokens)
		})
		summary.OutputAudioTokens = distributionOf(group.results, func(result TrialResult) *float64 {
			return usageValue(result.Session.Usage, result.Session.Usage.OutputAudioTokens)
		})
		report.Conditions = append(report.Conditions, summary)
	}
	for _, group := range pairs {
		var firstAudio, outputAudio, firstOutput, overlapSpeech, stopLatency, responseLatency, connectionCount []float64
		var overlapCount, speechDuring int
		for _, conditions := range group.results {
			overlap, overlapOK := conditions["overlap"]
			clean, cleanOK := conditions["clean"]
			if !overlapOK || !cleanOK {
				continue
			}
			overlapCount++
			if overlap.Timing.SpeechDuringOverlap {
				speechDuring++
			}
			outputAudio = append(outputAudio, overlap.Session.OutputAudioMS-clean.Session.OutputAudioMS)
			overlapSpeech = append(overlapSpeech, overlap.Timing.OverlapSpeechMS-clean.Timing.OverlapSpeechMS)
			connectionCount = append(connectionCount, float64(overlap.Session.ConnectionCount-clean.Session.ConnectionCount))
			if overlap.Session.FirstAudioMS != nil && clean.Session.FirstAudioMS != nil {
				firstAudio = append(firstAudio, *overlap.Session.FirstAudioMS-*clean.Session.FirstAudioMS)
			}
			if overlap.Timing.FirstOutputMS != nil && clean.Timing.FirstOutputMS != nil {
				firstOutput = append(firstOutput, *overlap.Timing.FirstOutputMS-*clean.Timing.FirstOutputMS)
			}
			if overlap.Timing.MeanStopLatencyMS != nil && clean.Timing.MeanStopLatencyMS != nil {
				stopLatency = append(stopLatency, *overlap.Timing.MeanStopLatencyMS-*clean.Timing.MeanStopLatencyMS)
			}
			if overlap.Timing.MeanResponseLatencyMS != nil && clean.Timing.MeanResponseLatencyMS != nil {
				responseLatency = append(responseLatency, *overlap.Timing.MeanResponseLatencyMS-*clean.Timing.MeanResponseLatencyMS)
			}
		}
		if overlapCount == 0 {
			continue
		}
		report.Pairs = append(report.Pairs, PairedSummary{
			Descriptor: group.descriptor, Scenario: group.scenario, Pairs: overlapCount,
			FirstAudioDeltaMS: summarizeValues(firstAudio), OutputAudioDeltaMS: summarizeValues(outputAudio),
			FirstOutputVADDeltaMS: summarizeValues(firstOutput), OverlapSpeechDeltaMS: summarizeValues(overlapSpeech),
			StopLatencyDeltaMS: summarizeValues(stopLatency), ResponseLatencyDeltaMS: summarizeValues(responseLatency),
			ConnectionCountDelta:    summarizeValues(connectionCount),
			SpeechDuringOverlapRate: float64(speechDuring) / float64(overlapCount),
		})
	}
	slices.SortFunc(report.Conditions, func(left, right ConditionSummary) int {
		return strings.Compare(summaryKey(left.Descriptor, left.Scenario, left.Condition), summaryKey(right.Descriptor, right.Scenario, right.Condition))
	})
	slices.SortFunc(report.Pairs, func(left, right PairedSummary) int {
		return strings.Compare(summaryKey(left.Descriptor, left.Scenario, ""), summaryKey(right.Descriptor, right.Scenario, ""))
	})
	return report, nil
}

func WriteSummaryReport(filename string, report SummaryReport) error {
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return fmt.Errorf("create summary directory: %w", err)
	}
	return writeJSONAtomic(filename, report)
}

func readRunManifest(filename string) (RunManifest, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return RunManifest{}, fmt.Errorf("read manifest %s: %w", filename, err)
	}
	var manifest RunManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return RunManifest{}, fmt.Errorf("decode manifest %s: %w", filename, err)
	}
	if manifest.SchemaVersion != ResultSchemaVersion {
		return RunManifest{}, fmt.Errorf("manifest %s has unsupported schema %q", filename, manifest.SchemaVersion)
	}
	return manifest, nil
}

func summaryKey(descriptor Descriptor, scenario, condition string) string {
	return strings.Join([]string{descriptor.Provider, descriptor.Model, descriptor.Architecture, scenario, condition}, "\x00")
}

func replicateFromTrialID(trialID string) string {
	parts := strings.Split(trialID, "/")
	if len(parts) == 0 {
		return trialID
	}
	return parts[len(parts)-1]
}

func pointer(value float64) *float64 { return &value }

func usageValue(usage Usage, value int64) *float64 {
	if !usage.Complete {
		return nil
	}
	result := float64(value)
	return &result
}

func countEvents(events []WireEvent, eventType string) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func distributionOf(results []TrialResult, selectValue func(TrialResult) *float64) Distribution {
	values := make([]float64, 0, len(results))
	for _, result := range results {
		if value := selectValue(result); value != nil {
			values = append(values, *value)
		}
	}
	return summarizeValues(values)
}

func summarizeValues(values []float64) Distribution {
	if len(values) == 0 {
		return Distribution{}
	}
	sorted := append([]float64(nil), values...)
	slices.Sort(sorted)
	var sum float64
	for _, value := range sorted {
		sum += value
	}
	lower, upper := bootstrapMeanCI(sorted, 10_000)
	return Distribution{
		N: len(sorted), Mean: sum / float64(len(sorted)), Median: percentile(sorted, 0.5),
		P95: percentile(sorted, 0.95), MeanCILower95: lower, MeanCIUpper95: upper,
	}
}

func bootstrapMeanCI(values []float64, replicates int) (float64, float64) {
	if len(values) == 1 {
		return values[0], values[0]
	}
	random := rand.New(rand.NewSource(0x4f70656e5265616c))
	means := make([]float64, replicates)
	for replicate := range replicates {
		var sum float64
		for range len(values) {
			sum += values[random.Intn(len(values))]
		}
		means[replicate] = sum / float64(len(values))
	}
	slices.Sort(means)
	return percentile(means, 0.025), percentile(means, 0.975)
}

func percentile(sorted []float64, probability float64) float64 {
	if len(sorted) == 1 {
		return sorted[0]
	}
	position := probability * float64(len(sorted)-1)
	lower := int(position)
	upper := min(lower+1, len(sorted)-1)
	fraction := position - float64(lower)
	return sorted[lower]*(1-fraction) + sorted[upper]*fraction
}
