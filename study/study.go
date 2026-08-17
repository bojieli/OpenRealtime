// Package study assembles the published milestone reports without collapsing
// incomparable workloads into a synthetic leaderboard.
package study

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"

	"github.com/bojieli/OpenRealtime/analysis"
	"github.com/bojieli/OpenRealtime/baseline"
	m2experiment "github.com/bojieli/OpenRealtime/experiments/m2"
	m3experiment "github.com/bojieli/OpenRealtime/experiments/m3"
	m4experiment "github.com/bojieli/OpenRealtime/experiments/m4"
	m5experiment "github.com/bojieli/OpenRealtime/experiments/m5"
)

const SchemaVersion = "0.1.0"

type Condition struct {
	ID                  string  `json:"id"`
	Family              string  `json:"family"`
	Workload            string  `json:"workload"`
	ComparabilityGroup  string  `json:"comparability_group"`
	Status              string  `json:"status"`
	LatencyMetric       string  `json:"latency_metric,omitempty"`
	LatencyP50NS        *uint64 `json:"latency_p50_ns,omitempty"`
	LatencyP95NS        *uint64 `json:"latency_p95_ns,omitempty"`
	FinalLatencyP50NS   *uint64 `json:"final_latency_p50_ns,omitempty"`
	QualityP50          *uint64 `json:"quality_p50,omitempty"`
	ComputeP50          *uint64 `json:"compute_p50,omitempty"`
	FailureCount        *uint64 `json:"failure_count,omitempty"`
	Trials              uint64  `json:"trials,omitempty"`
	ProtocolProfile     string  `json:"protocol_profile,omitempty"`
	EvidenceSource      string  `json:"evidence_source,omitempty"`
	UnavailabilityCause string  `json:"unavailability_cause,omitempty"`
}

type BootstrapInterval struct {
	Statistic string `json:"statistic"`
	Resamples uint64 `json:"resamples"`
	Seed      uint64 `json:"seed"`
	LowerNS   int64  `json:"lower_ns"`
	UpperNS   int64  `json:"upper_ns"`
}

type PairedEffect struct {
	Condition          string                      `json:"condition"`
	Baseline           string                      `json:"baseline"`
	Samples            uint64                      `json:"samples"`
	Wins               uint64                      `json:"wins"`
	Ties               uint64                      `json:"ties"`
	Losses             uint64                      `json:"losses"`
	Difference         analysis.SignedDistribution `json:"observed_minus_baseline_ns"`
	MedianBootstrap95  BootstrapInterval           `json:"paired_median_bootstrap_95"`
	InterpretationRule string                      `json:"interpretation_rule"`
}

type Claim struct {
	ID             string   `json:"id"`
	Statement      string   `json:"statement"`
	Scope          string   `json:"scope"`
	Evidence       []string `json:"evidence"`
	Counterexample string   `json:"counterexample_or_limit"`
}

type Study struct {
	SchemaVersion      string         `json:"schema_version"`
	ReleaseID          string         `json:"release_id"`
	EvidenceMode       string         `json:"evidence_mode"`
	Seed               uint64         `json:"seed"`
	Conditions         []Condition    `json:"conditions"`
	PairedEffects      []PairedEffect `json:"paired_effects"`
	Claims             []Claim        `json:"claims"`
	BlockedComparisons []string       `json:"blocked_comparisons"`
	Limitations        []string       `json:"limitations"`
}

func Build(root string) (Study, error) {
	if root == "" {
		return Study{}, errors.New("study root must not be empty")
	}
	m1, err := readJSON[baseline.Report](filepath.Join(root, "benchmarks", "m1", "reference", "report.json"))
	if err != nil {
		return Study{}, err
	}
	m2, err := readJSON[m2experiment.Report](filepath.Join(root, "benchmarks", "m2", "reference", "report.json"))
	if err != nil {
		return Study{}, err
	}
	m3, err := readJSON[m3experiment.Report](filepath.Join(root, "benchmarks", "m3", "reference", "report.json"))
	if err != nil {
		return Study{}, err
	}
	m4, err := readJSON[m4experiment.Report](filepath.Join(root, "benchmarks", "m4", "reference", "report.json"))
	if err != nil {
		return Study{}, err
	}
	m5, err := readJSON[m5experiment.Report](filepath.Join(root, "benchmarks", "m5", "reference", "report.json"))
	if err != nil {
		return Study{}, err
	}
	result := Study{
		SchemaVersion: SchemaVersion, ReleaseID: "openrealtime-benchmarks-v0.1.0",
		EvidenceMode: "deterministic_project_authored_reference", Seed: m2.Seed,
		Conditions: []Condition{{
			ID: "native_realtime", Family: "native", Workload: "not_collected",
			ComparabilityGroup: "external_access_required", Status: "not_run",
			UnavailabilityCause: "no provider credential or redistributable provider output was placed in scope",
		}},
		BlockedComparisons: []string{
			"Native realtime performance was not measured because provider access and redistributable outputs were unavailable.",
			"No participant data were collected; the human study is preregistered but not run.",
			"Fast/slow, translation, duplex, and rapid-game numbers use different tasks and must not be ranked against response-onset values.",
		},
		Limitations: []string{
			"All audio is a nonsemantic project-authored signal fixture with injected deterministic component delays.",
			"Quality and compute units are authored instrumentation, not model evaluation, billing, or hardware utilization.",
			"Bootstrap intervals characterize seeded reference trials only and do not imply population or deployment uncertainty.",
			"Schema-valid traces demonstrate wire conformance, not equivalence to an OpenAI-hosted server implementation.",
		},
	}
	if err := addResponseOnset(&result, m1, m2); err != nil {
		return Study{}, err
	}
	if err := addDuplex(&result, m3); err != nil {
		return Study{}, err
	}
	if err := addFastSlow(&result, m4); err != nil {
		return Study{}, err
	}
	if err := addDemonstrations(&result, m5); err != nil {
		return Study{}, err
	}
	result.Claims = referenceClaims()
	return result, nil
}

func addResponseOnset(result *Study, m1 baseline.Report, m2 m2experiment.Report) error {
	baselineLatency, ok := m1.Distributions["observed_response_latency_ns"]
	if !ok || baselineLatency.Count == 0 {
		return errors.New("M1 report lacks observed response latency")
	}
	result.Conditions = append(result.Conditions, Condition{
		ID: "endpointed_b0", Family: "endpointed", Workload: "symbolic_reference_audio",
		ComparabilityGroup: "m1_m2_response_onset", Status: "complete",
		LatencyMetric: "endpoint_to_first_output_playback", LatencyP50NS: pointer(baselineLatency.P50NS),
		LatencyP95NS: pointer(baselineLatency.P95NS), Trials: baselineLatency.Count,
		ProtocolProfile: "realtime", EvidenceSource: "benchmarks/m1/reference/report.json",
	})
	for _, name := range []string{"fixed_50ms", "revision_event"} {
		condition, ok := findM2(m2.Conditions, name)
		if !ok {
			return fmt.Errorf("M2 report lacks condition %q", name)
		}
		latency, ok := condition.Distributions["observed_latency_ns"]
		if !ok || latency.Count == 0 {
			return fmt.Errorf("M2 condition %q lacks observed latency", name)
		}
		result.Conditions = append(result.Conditions, Condition{
			ID: "microturn_" + name, Family: "microturn", Workload: "symbolic_reference_audio",
			ComparabilityGroup: "m1_m2_response_onset", Status: "complete",
			LatencyMetric: "endpoint_to_first_output_playback", LatencyP50NS: pointer(latency.P50NS),
			LatencyP95NS: pointer(latency.P95NS), Trials: latency.Count,
			ProtocolProfile: "realtime", EvidenceSource: "benchmarks/m2/reference/report.json",
		})
		effect, err := pairedEffect(condition, result.Seed^uint64(len(result.PairedEffects)+1))
		if err != nil {
			return err
		}
		result.PairedEffects = append(result.PairedEffects, effect)
	}
	return nil
}

func addDuplex(result *Study, report m3experiment.Report) error {
	for _, scenario := range report.Scenarios {
		condition := Condition{
			ID: "duplex_" + string(scenario.Scenario), Family: "duplex", Workload: "duplex_safety_scenario",
			ComparabilityGroup: "m3_duplex", Status: "complete", Trials: uint64(len(scenario.Trials)),
			ProtocolProfile: "realtime", EvidenceSource: "benchmarks/m3/reference/report.json",
		}
		failures := scenario.FalseStopCount + scenario.FailureToStopCount + scenario.HistoryViolationCount
		condition.FailureCount = pointer(failures)
		if scenario.StopLatency != nil {
			condition.LatencyMetric = "interruption_evidence_to_stop"
			condition.LatencyP50NS = pointer(scenario.StopLatency.P50NS)
			condition.LatencyP95NS = pointer(scenario.StopLatency.P95NS)
		}
		result.Conditions = append(result.Conditions, condition)
	}
	return nil
}

func addFastSlow(result *Study, report m4experiment.Report) error {
	for _, item := range report.Conditions {
		result.Conditions = append(result.Conditions, Condition{
			ID: "cognition_" + string(item.Kind), Family: "fast_slow", Workload: "symbolic_difficult_questions",
			ComparabilityGroup: "m4_cognition", Status: "complete",
			LatencyMetric: "first_truthful_progress", LatencyP50NS: pointer(item.FirstTruthfulProgress.P50NS),
			LatencyP95NS: pointer(item.FirstTruthfulProgress.P95NS), FinalLatencyP50NS: pointer(item.FinalAnswer.P50NS),
			QualityP50: pointer(item.Quality.P50), ComputeP50: pointer(item.Compute.P50),
			FailureCount: pointer(item.TruthViolationCount), Trials: item.Quality.Count,
			EvidenceSource: "benchmarks/m4/reference/report.json",
		})
	}
	return nil
}

func addDemonstrations(result *Study, report m5experiment.Report) error {
	for _, item := range report.Translation.Conditions {
		result.Conditions = append(result.Conditions, Condition{
			ID: "translation_" + string(item.Policy), Family: "translation", Workload: "symbolic_simultaneous_translation",
			ComparabilityGroup: "m5_translation", Status: "complete", LatencyMetric: "mean_segment_lag",
			LatencyP50NS: pointer(item.MeanLag.P50NS), LatencyP95NS: pointer(item.MeanLag.P95NS),
			FinalLatencyP50NS: pointer(item.CompletionLag.P50NS), QualityP50: pointer(item.Quality.P50),
			ComputeP50: pointer(item.Compute.P50), FailureCount: pointer(item.FailureCount), Trials: item.Quality.Count,
			ProtocolProfile: "translation", EvidenceSource: "benchmarks/m5/reference/report.json",
		})
	}
	for _, item := range report.Game.Conditions {
		result.Conditions = append(result.Conditions, Condition{
			ID: "game_" + string(item.Condition), Family: "rapid_game", Workload: report.Game.Name,
			ComparabilityGroup: "m5_signal_match", Status: "complete", LatencyMetric: "cue_to_input_reaction",
			LatencyP50NS: pointer(item.ReactionLatency.P50NS), LatencyP95NS: pointer(item.ReactionLatency.P95NS),
			QualityP50: pointer(item.Quality.P50), ComputeP50: pointer(item.Compute.P50),
			FailureCount: pointer(item.FailureCount), Trials: item.Quality.Count,
			ProtocolProfile: "realtime", EvidenceSource: "benchmarks/m5/reference/report.json",
		})
	}
	return nil
}

func pairedEffect(condition m2experiment.Condition, seed uint64) (PairedEffect, error) {
	if len(condition.Trials) == 0 {
		return PairedEffect{}, errors.New("paired effect requires trials")
	}
	differences := make([]int64, 0, len(condition.Trials))
	var wins, ties, losses uint64
	for _, trial := range condition.Trials {
		difference := trial.ObservedMinusBaselineNS
		differences = append(differences, difference)
		switch {
		case difference < 0:
			wins++
		case difference == 0:
			ties++
		default:
			losses++
		}
	}
	distribution, err := analysis.SummarizeSigned(differences)
	if err != nil {
		return PairedEffect{}, err
	}
	interval := bootstrapMedian(differences, seed, 10_000)
	return PairedEffect{
		Condition: condition.Policy.Name, Baseline: "endpointed_b0", Samples: uint64(len(differences)),
		Wins: wins, Ties: ties, Losses: losses, Difference: distribution, MedianBootstrap95: interval,
		InterpretationRule: "negative values favor the microturn condition; interval applies only to seeded reference trials",
	}, nil
}

func bootstrapMedian(values []int64, seed, resamples uint64) BootstrapInterval {
	random := rand.New(rand.NewPCG(seed^0x8cb92baa5f37d5a7, 0x9e3779b97f4a7c15))
	statistics := make([]int64, resamples)
	sample := make([]int64, len(values))
	for iteration := range resamples {
		for index := range sample {
			sample[index] = values[random.Uint64N(uint64(len(values)))]
		}
		slices.Sort(sample)
		statistics[iteration] = sample[(len(sample)-1)/2]
	}
	slices.Sort(statistics)
	lowerIndex := (25*resamples + 999) / 1000
	upperIndex := (975*resamples + 999) / 1000
	if lowerIndex > 0 {
		lowerIndex--
	}
	if upperIndex > 0 {
		upperIndex--
	}
	return BootstrapInterval{
		Statistic: "paired_median_difference", Resamples: resamples, Seed: seed,
		LowerNS: statistics[lowerIndex], UpperNS: statistics[upperIndex],
	}
}

func referenceClaims() []Claim {
	return []Claim{
		{ID: "C1", Statement: "Fixed 50 ms and revision-triggered scheduling reduce response-onset latency relative to endpointed B0.", Scope: "paired deterministic M1/M2 fixture only", Evidence: []string{"endpointed_b0", "microturn_fixed_50ms", "microturn_revision_event"}, Counterexample: "coarser fixed cadences tie one another on this cue layout and no deployed provider was measured"},
		{ID: "C2", Statement: "The reference duplex policy stops directed interruption without false-stopping backchannels or side speech.", Scope: "four injected M3 scenario labels", Evidence: []string{"duplex_directed_interruption", "duplex_listener_backchannel", "duplex_side_speech"}, Counterexample: "labels are injected and do not measure acoustic classification"},
		{ID: "C3", Statement: "Fast/slow orchestration advances truthful progress but does not improve final-answer latency and adds compute.", Scope: "symbolic M4 difficult-question workload", Evidence: []string{"cognition_single_blocking", "cognition_fast_slow"}, Counterexample: "fast-only is quicker but fails the authored quality threshold"},
		{ID: "C4", Statement: "Stable incremental translation reduces authored segment lag without exact-match loss, at higher compute.", Scope: "symbolic five-segment M5 translation fixture", Evidence: []string{"translation_endpointed", "translation_stable_incremental"}, Counterexample: "aggressive output is faster and cheaper but produces 60 authored segment errors"},
		{ID: "C5", Statement: "The 50 ms microturn Signal Match policy reduces injected reaction latency and deadline misses.", Scope: "symbolic M5 rapid audio game", Evidence: []string{"game_endpointed", "game_microturn_50ms"}, Counterexample: "the timing model is injected and the microturn condition consumes more symbolic compute"},
	}
}

func findM2(conditions []m2experiment.Condition, name string) (m2experiment.Condition, bool) {
	for _, condition := range conditions {
		if condition.Policy.Name == name {
			return condition, true
		}
	}
	return m2experiment.Condition{}, false
}

func readJSON[T any](path string) (T, error) {
	var result T
	file, err := os.Open(path)
	if err != nil {
		return result, fmt.Errorf("open study source %s: %w", path, err)
	}
	defer file.Close()
	const maximumBytes = int64(32 << 20)
	decoder := json.NewDecoder(io.LimitReader(file, maximumBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, fmt.Errorf("decode study source %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return result, fmt.Errorf("study source %s has trailing JSON", path)
		}
		return result, fmt.Errorf("decode study source %s trailing data: %w", path, err)
	}
	return result, nil
}

func pointer[T any](value T) *T { return &value }
