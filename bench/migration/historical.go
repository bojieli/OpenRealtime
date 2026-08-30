package migration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// HistoricalBaselineRegistryVersion is the only owner-accepted baseline
// registry schema understood by this package. Historical measurements do not
// need reconstructed per-attempt artifacts: the canonical registry itself is
// the checked authority for the numbers that survived from the original run.
const HistoricalBaselineRegistryVersion = 1

// HistoricalBaselineRegistry records benchmark-owner accepted original
// results. Candidate attempts remain ordinary, fully evidenced Attempt rows;
// this type deliberately contains no fabricated historical attempt records.
type HistoricalBaselineRegistry struct {
	Version    int                       `json:"version"`
	RegistryID string                    `json:"registry_id"`
	Authority  string                    `json:"authority"`
	Acceptance string                    `json:"acceptance"`
	ManifestID string                    `json:"manifest_id"`
	Suites     []HistoricalSuiteBaseline `json:"suites"`
}

// HistoricalSuiteBaseline is one complete original-suite summary. Trail is a
// human-reviewable description or repository reference for the surviving
// recorded result; it is not a requirement for historical media or attempt
// reconstruction.
type HistoricalSuiteBaseline struct {
	Suite            string                        `json:"suite"`
	ExpectedCases    int                           `json:"expected_cases"`
	ExpectedAttempts int                           `json:"expected_attempts"`
	Trail            []string                      `json:"trail"`
	Pass             HistoricalRate                `json:"pass"`
	Interaction      *HistoricalRate               `json:"interaction,omitempty"`
	Deadline         *HistoricalRate               `json:"deadline,omitempty"`
	Safety           *HistoricalSafety             `json:"safety,omitempty"`
	Latencies        []HistoricalLatency           `json:"latencies"`
	Conditions       []HistoricalPartitionBaseline `json:"conditions"`
	Cases            []HistoricalPartitionBaseline `json:"cases"`
}

// HistoricalRate retains the numerator and denominator rather than a rounded
// percentage. ApplicableCases is independent because many task suites contain
// repeated attempts of the same case.
type HistoricalRate struct {
	ApplicableAttempts int `json:"applicable_attempts"`
	ApplicableCases    int `json:"applicable_cases"`
	Successes          int `json:"successes"`
}

// HistoricalSafety retains the original violation count. Candidate safety is
// still gated with zero tolerance even when the accepted original had a
// violation; an old defect cannot authorize a new one.
type HistoricalSafety struct {
	ApplicableAttempts int `json:"applicable_attempts"`
	ApplicableCases    int `json:"applicable_cases"`
	Violations         int `json:"violations"`
}

// HistoricalLatency is an original recorded distribution. Raw samples are
// not invented when only the distribution trail survived.
type HistoricalLatency struct {
	Name            string       `json:"name"`
	Unit            string       `json:"unit"`
	ApplicableCases int          `json:"applicable_cases"`
	Distribution    Distribution `json:"distribution"`
}

// HistoricalPartitionBaseline records a condition or exact case. Case is
// empty for entries in HistoricalSuiteBaseline.Conditions and required for
// entries in Cases.
type HistoricalPartitionBaseline struct {
	Condition   string              `json:"condition"`
	Case        string              `json:"case,omitempty"`
	Pass        HistoricalRate      `json:"pass"`
	Interaction *HistoricalRate     `json:"interaction,omitempty"`
	Deadline    *HistoricalRate     `json:"deadline,omitempty"`
	Safety      *HistoricalSafety   `json:"safety,omitempty"`
	Latencies   []HistoricalLatency `json:"latencies"`
}

// ID returns the digest of the canonical registry with RegistryID cleared.
func (registry HistoricalBaselineRegistry) ID() string {
	registry = canonicalHistoricalBaselineRegistry(registry)
	registry.RegistryID = ""
	return digestJSON(registry)
}

// SealHistoricalBaselineRegistry canonicalizes, identifies, and validates an
// owner-accepted historical registry against the exact migration manifest.
func SealHistoricalBaselineRegistry(
	registry HistoricalBaselineRegistry, manifest Manifest,
) (HistoricalBaselineRegistry, error) {
	registry = canonicalHistoricalBaselineRegistry(registry)
	registry.RegistryID = ""
	registry.RegistryID = registry.ID()
	if err := registry.ValidateAgainst(manifest); err != nil {
		return HistoricalBaselineRegistry{}, err
	}
	return registry, nil
}

// ValidateAgainst fails closed unless the registry covers the exact manifest
// population and every acceptance-authoritative suite/partition metric. It
// never asks for historical per-attempt evidence.
func (registry HistoricalBaselineRegistry) ValidateAgainst(manifest Manifest) error {
	manifest = canonicalManifest(manifest)
	if err := manifest.Validate(); err != nil {
		return fmt.Errorf("historical baseline manifest: %w", err)
	}
	if registry.Version != HistoricalBaselineRegistryVersion {
		return fmt.Errorf("historical baseline registry version must be %d, got %d",
			HistoricalBaselineRegistryVersion, registry.Version)
	}
	if !validSHA256(registry.RegistryID) || registry.ID() != registry.RegistryID {
		return errors.New("historical baseline registry digest does not match its content")
	}
	if strings.TrimSpace(registry.Authority) != "benchmark-owner" {
		return errors.New("historical baseline registry authority must be benchmark-owner")
	}
	if err := canonicalCensusText("historical baseline acceptance", registry.Acceptance); err != nil {
		return err
	}
	if registry.ManifestID != manifest.ID() {
		return errors.New("historical baseline registry does not bind the exact migration manifest")
	}
	if len(registry.Suites) != len(manifest.Suites) {
		return fmt.Errorf("historical baseline registry covers %d suites, manifest requires %d",
			len(registry.Suites), len(manifest.Suites))
	}

	manifestSuites := make(map[string]SuiteSpec, len(manifest.Suites))
	for _, suite := range manifest.Suites {
		manifestSuites[suite.Name] = suite
	}
	seen := make(map[string]bool, len(registry.Suites))
	for _, historical := range registry.Suites {
		suite, exists := manifestSuites[historical.Suite]
		if !exists {
			return fmt.Errorf("historical baseline registry contains unknown suite %q", historical.Suite)
		}
		if seen[historical.Suite] {
			return fmt.Errorf("historical baseline registry repeats suite %q", historical.Suite)
		}
		seen[historical.Suite] = true
		if err := validateHistoricalSuite(historical, suite); err != nil {
			return fmt.Errorf("historical baseline suite %q: %w", historical.Suite, err)
		}
	}
	return nil
}

func validateHistoricalSuite(historical HistoricalSuiteBaseline, suite SuiteSpec) error {
	if historical.ExpectedCases != suite.ExpectedCases ||
		historical.ExpectedAttempts != suite.ExpectedAttempts {
		return fmt.Errorf("population is %d cases/%d attempts, want %d/%d",
			historical.ExpectedCases, historical.ExpectedAttempts,
			suite.ExpectedCases, suite.ExpectedAttempts)
	}
	if len(historical.Trail) == 0 || len(historical.Trail) > maxFixedAndTreatmentAxes {
		return errors.New("needs a bounded non-empty surviving result trail")
	}
	for index, trail := range historical.Trail {
		if err := canonicalCensusText(fmt.Sprintf("historical trail %d", index), trail); err != nil {
			return err
		}
	}
	if err := validateHistoricalCompleteRate(historical.Pass, suite.ExpectedAttempts,
		suite.ExpectedCases, "pass"); err != nil {
		return err
	}
	if err := validateOptionalHistoricalRate(historical.Interaction, suite.Policy.Interaction,
		suite.ExpectedAttempts, suite.ExpectedCases, "interaction"); err != nil {
		return err
	}
	if err := validateOptionalHistoricalRate(historical.Deadline, suite.Policy.Deadline,
		suite.ExpectedAttempts, suite.ExpectedCases, "deadline"); err != nil {
		return err
	}
	if err := validateOptionalHistoricalSafety(historical.Safety, suite.Policy.Safety,
		suite.ExpectedAttempts, suite.ExpectedCases, "safety"); err != nil {
		return err
	}
	if err := validateHistoricalLatencies(historical.Latencies, suite.Policy.Latencies,
		suite.ExpectedAttempts, suite.ExpectedCases, "latencies"); err != nil {
		return err
	}
	if err := validateHistoricalConditions(historical.Conditions, suite); err != nil {
		return err
	}
	return validateHistoricalCases(historical.Cases, suite)
}

func validateHistoricalRate(rate HistoricalRate, maxAttempts, maxCases int, name string) error {
	if rate.ApplicableAttempts <= 0 || rate.ApplicableAttempts > maxAttempts ||
		rate.ApplicableCases <= 0 || rate.ApplicableCases > maxCases ||
		rate.Successes < 0 || rate.Successes > rate.ApplicableAttempts {
		return fmt.Errorf("%s rate has invalid counts", name)
	}
	return nil
}

func validateHistoricalCompleteRate(
	rate HistoricalRate, attempts, cases int, name string,
) error {
	if err := validateHistoricalRate(rate, attempts, cases, name); err != nil {
		return err
	}
	if rate.ApplicableAttempts != attempts || rate.ApplicableCases != cases {
		return fmt.Errorf("%s rate covers %d attempts/%d cases, want complete %d/%d",
			name, rate.ApplicableAttempts, rate.ApplicableCases, attempts, cases)
	}
	return nil
}

func validateOptionalHistoricalRate(
	rate *HistoricalRate, policy OutcomePolicy, maxAttempts, maxCases int, name string,
) error {
	required := policy.Required || policy.NonInferiority != nil
	if rate == nil {
		if required {
			return fmt.Errorf("%s rate is required by the manifest", name)
		}
		return nil
	}
	if policy.Required {
		return validateHistoricalCompleteRate(*rate, maxAttempts, maxCases, name)
	}
	return validateHistoricalRate(*rate, maxAttempts, maxCases, name)
}

func validateOptionalHistoricalSafety(
	safety *HistoricalSafety, policy SafetyPolicy, maxAttempts, maxCases int, name string,
) error {
	if safety == nil {
		if policy.Required {
			return fmt.Errorf("%s counts are required by the manifest", name)
		}
		return nil
	}
	if safety.ApplicableAttempts <= 0 || safety.ApplicableAttempts > maxAttempts ||
		safety.ApplicableCases <= 0 || safety.ApplicableCases > maxCases ||
		safety.Violations < 0 || safety.Violations > safety.ApplicableAttempts {
		return fmt.Errorf("%s has invalid counts", name)
	}
	if policy.Required && (safety.ApplicableAttempts != maxAttempts ||
		safety.ApplicableCases != maxCases) {
		return fmt.Errorf("%s covers %d attempts/%d cases, want complete %d/%d",
			name, safety.ApplicableAttempts, safety.ApplicableCases, maxAttempts, maxCases)
	}
	return nil
}

func validateHistoricalLatencies(
	historical []HistoricalLatency, policies []LatencyPolicy,
	maxAttempts, maxCases int, scope string,
) error {
	if len(historical) != len(policies) {
		return fmt.Errorf("%s records %d latency distributions, manifest declares %d",
			scope, len(historical), len(policies))
	}
	policyByName := make(map[string]LatencyPolicy, len(policies))
	for _, policy := range policies {
		policyByName[policy.Name] = policy
	}
	seen := make(map[string]bool, len(historical))
	for _, latency := range historical {
		policy, exists := policyByName[latency.Name]
		if !exists || seen[latency.Name] {
			return fmt.Errorf("%s contains unknown or repeated latency %q", scope, latency.Name)
		}
		seen[latency.Name] = true
		if latency.Unit != policy.Unit {
			return fmt.Errorf("%s latency %q uses unit %q, want %q",
				scope, latency.Name, latency.Unit, policy.Unit)
		}
		if latency.ApplicableCases <= 0 || latency.ApplicableCases > maxCases {
			return fmt.Errorf("%s latency %q has invalid applicable case count",
				scope, latency.Name)
		}
		if policy.Required && (latency.Distribution.Count != maxAttempts ||
			latency.ApplicableCases != maxCases) {
			return fmt.Errorf("%s latency %q covers %d attempts/%d cases, want complete %d/%d",
				scope, latency.Name, latency.Distribution.Count, latency.ApplicableCases,
				maxAttempts, maxCases)
		}
		if latency.Distribution.Count > maxAttempts {
			return fmt.Errorf("%s latency %q has too many attempts", scope, latency.Name)
		}
		if policy.Gate != nil && latency.ApplicableCases < policy.Gate.MinimumCases {
			return fmt.Errorf("%s latency %q covers %d cases, below manifest minimum %d",
				scope, latency.Name, latency.ApplicableCases, policy.Gate.MinimumCases)
		}
		if err := validateHistoricalDistribution(latency.Distribution, policy, latency.Unit); err != nil {
			return fmt.Errorf("%s latency %q: %w", scope, latency.Name, err)
		}
	}
	return nil
}

func validateHistoricalDistribution(
	distribution Distribution, policy LatencyPolicy, unit string,
) error {
	values := []float64{distribution.Min, distribution.P50, distribution.P90,
		distribution.P95, distribution.P99, distribution.Max, distribution.Mean}
	for _, value := range values {
		if !finite(value) || value < 0 {
			return errors.New("distribution contains a negative or non-finite value")
		}
	}
	if distribution.Count <= 0 || distribution.Unit != unit {
		return errors.New("distribution has an invalid count or unit")
	}
	if distribution.Min > distribution.P50 || distribution.P50 > distribution.P90 ||
		distribution.P90 > distribution.P95 || distribution.P95 > distribution.P99 ||
		distribution.P99 > distribution.Max || distribution.Mean < distribution.Min ||
		distribution.Mean > distribution.Max {
		return errors.New("distribution statistics are not ordered")
	}
	if policy.Gate != nil && distribution.Count < policy.Gate.MinimumAttempts {
		return fmt.Errorf("distribution count %d is below manifest minimum %d",
			distribution.Count, policy.Gate.MinimumAttempts)
	}
	return nil
}

func validateHistoricalConditions(
	historical []HistoricalPartitionBaseline, suite SuiteSpec,
) error {
	required := make(map[string]Population)
	for _, population := range suite.Populations {
		if population.RequirePolicy || population.Policy != nil {
			required[population.Condition] = population
		}
	}
	if len(historical) != len(required) {
		return fmt.Errorf("records %d gated conditions, manifest requires %d",
			len(historical), len(required))
	}
	seen := make(map[string]bool, len(historical))
	for _, partition := range historical {
		population, exists := required[partition.Condition]
		if !exists || partition.Case != "" || seen[partition.Condition] {
			return fmt.Errorf("contains unknown, repeated, or case-shaped condition %q",
				partition.Condition)
		}
		seen[partition.Condition] = true
		if population.Policy == nil {
			return fmt.Errorf("condition %q requires a missing manifest policy", partition.Condition)
		}
		if err := validateHistoricalPartition(partition, *population.Policy,
			population.ExpectedAttempts, population.ExpectedCases); err != nil {
			return fmt.Errorf("condition %q: %w", partition.Condition, err)
		}
	}
	return nil
}

func validateHistoricalCases(historical []HistoricalPartitionBaseline, suite SuiteSpec) error {
	type caseIdentity struct{ condition, id string }
	required := make(map[caseIdentity]CaseSpec)
	for _, item := range suite.Cases {
		if item.RequirePolicy || item.Policy != nil {
			required[caseIdentity{item.Condition, item.ID}] = item
		}
	}
	if len(historical) != len(required) {
		return fmt.Errorf("records %d gated cases, manifest requires %d", len(historical), len(required))
	}
	seen := make(map[caseIdentity]bool, len(historical))
	for _, partition := range historical {
		identity := caseIdentity{partition.Condition, partition.Case}
		item, exists := required[identity]
		if !exists || partition.Case == "" || seen[identity] {
			return fmt.Errorf("contains unknown, repeated, or incomplete case %q/%q",
				partition.Condition, partition.Case)
		}
		seen[identity] = true
		if item.Policy == nil {
			return fmt.Errorf("case %q/%q requires a missing manifest policy",
				partition.Condition, partition.Case)
		}
		if err := validateHistoricalPartition(partition, *item.Policy,
			len(item.Repetitions), 1); err != nil {
			return fmt.Errorf("case %q/%q: %w", partition.Condition, partition.Case, err)
		}
	}
	return nil
}

func validateHistoricalPartition(
	partition HistoricalPartitionBaseline, policy PartitionPolicy, attempts, cases int,
) error {
	if err := validateHistoricalCompleteRate(partition.Pass, attempts, cases, "pass"); err != nil {
		return err
	}
	if err := validateOptionalHistoricalRate(partition.Interaction, policy.Interaction,
		attempts, cases, "interaction"); err != nil {
		return err
	}
	if err := validateOptionalHistoricalRate(partition.Deadline, policy.Deadline,
		attempts, cases, "deadline"); err != nil {
		return err
	}
	if partition.Safety != nil {
		if err := validateOptionalHistoricalSafety(partition.Safety, SafetyPolicy{Required: true},
			attempts, cases, "safety"); err != nil {
			return err
		}
	}
	return validateHistoricalLatencies(partition.Latencies, policy.Latencies,
		attempts, cases, "latencies")
}

func canonicalHistoricalBaselineRegistry(
	input HistoricalBaselineRegistry,
) HistoricalBaselineRegistry {
	result := input
	result.Suites = make([]HistoricalSuiteBaseline, len(input.Suites))
	for index, suite := range input.Suites {
		result.Suites[index] = canonicalHistoricalSuite(suite)
	}
	sort.Slice(result.Suites, func(left, right int) bool {
		return result.Suites[left].Suite < result.Suites[right].Suite
	})
	return result
}

func canonicalHistoricalSuite(input HistoricalSuiteBaseline) HistoricalSuiteBaseline {
	result := input
	result.Trail = append([]string(nil), input.Trail...)
	sort.Strings(result.Trail)
	result.Interaction = cloneHistoricalRate(input.Interaction)
	result.Deadline = cloneHistoricalRate(input.Deadline)
	result.Safety = cloneHistoricalSafety(input.Safety)
	result.Latencies = canonicalHistoricalLatencies(input.Latencies)
	result.Conditions = canonicalHistoricalPartitions(input.Conditions)
	result.Cases = canonicalHistoricalPartitions(input.Cases)
	return result
}

func canonicalHistoricalPartitions(
	input []HistoricalPartitionBaseline,
) []HistoricalPartitionBaseline {
	result := make([]HistoricalPartitionBaseline, len(input))
	for index, partition := range input {
		result[index] = partition
		result[index].Interaction = cloneHistoricalRate(partition.Interaction)
		result[index].Deadline = cloneHistoricalRate(partition.Deadline)
		result[index].Safety = cloneHistoricalSafety(partition.Safety)
		result[index].Latencies = canonicalHistoricalLatencies(partition.Latencies)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Condition != result[right].Condition {
			return result[left].Condition < result[right].Condition
		}
		return result[left].Case < result[right].Case
	})
	return result
}

func canonicalHistoricalLatencies(input []HistoricalLatency) []HistoricalLatency {
	result := append([]HistoricalLatency(nil), input...)
	for index := range result {
		distribution := &result[index].Distribution
		distribution.Min = canonicalFloat(distribution.Min)
		distribution.P50 = canonicalFloat(distribution.P50)
		distribution.P90 = canonicalFloat(distribution.P90)
		distribution.P95 = canonicalFloat(distribution.P95)
		distribution.P99 = canonicalFloat(distribution.P99)
		distribution.Max = canonicalFloat(distribution.Max)
		distribution.Mean = canonicalFloat(distribution.Mean)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Name < result[right].Name
	})
	return result
}

func cloneHistoricalRate(input *HistoricalRate) *HistoricalRate {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}

func cloneHistoricalSafety(input *HistoricalSafety) *HistoricalSafety {
	if input == nil {
		return nil
	}
	result := *input
	return &result
}

// MarshalHistoricalBaselineRegistry writes stable canonical JSON.
func MarshalHistoricalBaselineRegistry(
	registry HistoricalBaselineRegistry, manifest Manifest,
) ([]byte, error) {
	if err := registry.ValidateAgainst(manifest); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(canonicalHistoricalBaselineRegistry(registry), "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

// DecodeHistoricalBaselineRegistry rejects unknown fields, trailing JSON,
// noncanonical bytes, manifest drift, and incomplete metric coverage.
func DecodeHistoricalBaselineRegistry(
	reader io.Reader, manifest Manifest,
) (HistoricalBaselineRegistry, error) {
	if reader == nil {
		return HistoricalBaselineRegistry{}, errors.New("decode historical baseline registry: reader is nil")
	}
	payload, err := readBoundedJSONArtifact(reader, "historical baseline registry")
	if err != nil {
		return HistoricalBaselineRegistry{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var registry HistoricalBaselineRegistry
	if err := decoder.Decode(&registry); err != nil {
		return HistoricalBaselineRegistry{}, fmt.Errorf("decode historical baseline registry: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return HistoricalBaselineRegistry{}, errors.New("decode historical baseline registry: trailing JSON")
	}
	registry = canonicalHistoricalBaselineRegistry(registry)
	if err := registry.ValidateAgainst(manifest); err != nil {
		return HistoricalBaselineRegistry{}, err
	}
	canonical, err := MarshalHistoricalBaselineRegistry(registry, manifest)
	if err != nil || !bytes.Equal(payload, canonical) {
		return HistoricalBaselineRegistry{}, errors.New(
			"decode historical baseline registry: artifact bytes are not canonical")
	}
	return registry, nil
}
