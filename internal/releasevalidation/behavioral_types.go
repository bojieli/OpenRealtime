package releasevalidation

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	BehavioralTargetsVersion    = 1
	FrozenCandidateVersion      = 2
	BehavioralAcceptanceVersion = 2

	RegistrationRegistered    = "registered"
	RegistrationUnavailable   = "unavailable"
	RegistrationNotApplicable = "not_applicable"

	ResultKindBench        = "bench_result"
	ResultKindArchitecture = "architecture_result"

	CaseKeyExact           = "exact"
	CaseKeyBeforeFinalHash = "before_final_hash"

	ComparisonAtLeast = "at_least"
	ComparisonAtMost  = "at_most"

	StatisticSum   = "sum"
	StatisticCount = "count"
	StatisticMin   = "min"
	StatisticP50   = "p50"
	StatisticP90   = "p90"
	StatisticP95   = "p95"
	StatisticP99   = "p99"
	StatisticMax   = "max"
	StatisticMean  = "mean"

	RunFailedFull        = "failed_full"
	RunFocusedDiagnostic = "focused_diagnostic"
	RunFinalFull         = "final_full"
)

const maximumBehavioralControlBytes = 16 << 20

var (
	behavioralIDPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	revisionPattern     = regexp.MustCompile(`^[a-f0-9]{40}(?:[a-f0-9]{24})?$`)
	sha256Pattern       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// BehavioralTargets is the checked, preregistered behavioral contract. An
// unavailable registration is explicit checked data, not an omitted field:
// it keeps acceptance blocked until the benchmark owner records a target.
type BehavioralTargets struct {
	FormatVersion int                     `json:"format_version"`
	Suites        []BehavioralSuiteTarget `json:"suites"`
}

type BehavioralSuiteTarget struct {
	ID                 string            `json:"id"`
	ResultKind         string            `json:"result_kind"`
	Suite              string            `json:"suite"`
	ExpectedPopulation int               `json:"expected_population"`
	CaseKey            string            `json:"case_key"`
	Aggregate          AggregateTarget   `json:"aggregate"`
	Cases              CaseTargetSet     `json:"cases"`
	Safety             EvidenceTargetSet `json:"safety"`
	Deadline           EvidenceTargetSet `json:"deadline"`
	Latency            EvidenceTargetSet `json:"latency"`
}

type Registration struct {
	Status string `json:"status"`
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type AggregateTarget struct {
	Registration      Registration `json:"registration"`
	MinimumPassed     *int         `json:"minimum_passed,omitempty"`
	MinimumApplicable *int         `json:"minimum_applicable,omitempty"`
}

type CaseTargetSet struct {
	Registration Registration `json:"registration"`
	Targets      []CaseTarget `json:"targets,omitempty"`
}

type CaseTarget struct {
	Case              string `json:"case"`
	ExpectedAttempts  int    `json:"expected_attempts"`
	MinimumPassed     int    `json:"minimum_passed"`
	MinimumApplicable *int   `json:"minimum_applicable,omitempty"`
}

type EvidenceTargetSet struct {
	Registration Registration      `json:"registration"`
	Rules        []MetricRule      `json:"rules,omitempty"`
	Exclusions   []MetricExclusion `json:"exclusions,omitempty"`
}

type MetricRule struct {
	Name             string  `json:"name"`
	Metric           string  `json:"metric"`
	Statistic        string  `json:"statistic"`
	Comparison       string  `json:"comparison"`
	Threshold        float64 `json:"threshold"`
	MinimumSamples   int     `json:"minimum_samples"`
	RequireEveryTask bool    `json:"require_every_task"`
}

type MetricExclusion struct {
	Metric string `json:"metric"`
	Reason string `json:"reason"`
}

// FrozenCandidate is authored before the final campaigns. It pins the build,
// machine and complete execution requirement for every suite. The acceptance
// report binds the resulting files by digest after they exist.
type FrozenCandidate struct {
	FormatVersion    int                    `json:"format_version"`
	CandidateID      string                 `json:"candidate_id"`
	Revision         string                 `json:"revision"`
	ExecutableSHA256 string                 `json:"executable_sha256"`
	Machine          bench.Machine          `json:"machine"`
	Suites           []FrozenCandidateSuite `json:"suites"`
}

type FrozenCandidateSuite struct {
	ID                         string                      `json:"id"`
	ExecutionRequirementSHA256 string                      `json:"execution_requirement_sha256"`
	RunSpecSHA256              string                      `json:"run_spec_sha256"`
	TaskInventorySHA256        string                      `json:"task_inventory_sha256"`
	ScorerSHA256               string                      `json:"scorer_sha256"`
	SourceReceipts             []CampaignSourceRequirement `json:"source_receipts"`
	Lineage                    []RunLineage                `json:"lineage"`
}

// CampaignSourceRequirement is frozen before a final campaign. The receipt
// digest cannot exist yet, but its semantic kind and schema must already be
// fixed so a final run cannot substitute an unrelated evidence artifact.
type CampaignSourceRequirement struct {
	Kind           string `json:"kind"`
	ArtifactFormat string `json:"artifact_format"`
}

// RunLineage is chronological. Earlier failed/full and focused rows bind the
// raw canonical bytes of their verified campaign closures. The last row is
// the predeclared final campaign; its post-run closure is supplied separately
// to acceptance and cannot be written into this pre-run candidate declaration.
type RunLineage struct {
	CampaignID     string `json:"campaign_id"`
	Kind           string `json:"kind"`
	Population     int    `json:"population"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
}

func LoadBehavioralTargets(path string) (BehavioralTargets, string, error) {
	payload, err := readBehavioralControl(path)
	if err != nil {
		return BehavioralTargets{}, "", fmt.Errorf("read behavioral targets: %w", err)
	}
	var targets BehavioralTargets
	if err := decodeBehavioralControl(payload, &targets); err != nil {
		return BehavioralTargets{}, "", fmt.Errorf("decode behavioral targets: %w", err)
	}
	if err := targets.Validate(); err != nil {
		return BehavioralTargets{}, "", err
	}
	return targets, digestBytes(payload), nil
}

func LoadFrozenCandidate(path string) (FrozenCandidate, string, error) {
	payload, err := readBehavioralControl(path)
	if err != nil {
		return FrozenCandidate{}, "", fmt.Errorf("read frozen candidate: %w", err)
	}
	var candidate FrozenCandidate
	if err := decodeBehavioralControl(payload, &candidate); err != nil {
		return FrozenCandidate{}, "", fmt.Errorf("decode frozen candidate: %w", err)
	}
	if err := candidate.Validate(); err != nil {
		return FrozenCandidate{}, "", err
	}
	return candidate, digestBytes(payload), nil
}

func readBehavioralControl(path string) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("path is empty")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 ||
		before.Size() > maximumBehavioralControlBytes {
		return nil, errors.New("artifact is not a bounded non-empty regular file")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximumBehavioralControlBytes+1))
	if err != nil || int64(len(payload)) != before.Size() {
		return nil, errors.New("read complete behavioral artifact")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() {
		return nil, errors.New("behavioral artifact changed while it was read")
	}
	return payload, nil
}

func decodeBehavioralControl(payload []byte, target any) error {
	if err := strictjson.ValidateWithLimits(payload, strictjson.Limits{
		MaxInputBytes: maximumBehavioralControlBytes,
		MaxTokens:     1_000_000, MaxArrayElements: 100_000,
		MaxTotalKeyBytes: 8 << 20, MaxWorkBytes: 64 << 20,
	}); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func (targets BehavioralTargets) Validate() error {
	if targets.FormatVersion != BehavioralTargetsVersion {
		return fmt.Errorf("behavioral targets format must be %d", BehavioralTargetsVersion)
	}
	if len(targets.Suites) == 0 {
		return errors.New("behavioral targets have no required suites")
	}
	previous := ""
	for index, suite := range targets.Suites {
		if err := suite.validate(); err != nil {
			return fmt.Errorf("behavioral target suite %d: %w", index, err)
		}
		if previous != "" && previous >= suite.ID {
			return errors.New("behavioral target suites must be uniquely sorted by ID")
		}
		previous = suite.ID
	}
	return nil
}

func (suite BehavioralSuiteTarget) validate() error {
	if !behavioralIDPattern.MatchString(suite.ID) {
		return fmt.Errorf("invalid suite ID %q", suite.ID)
	}
	if strings.TrimSpace(suite.Suite) == "" || len(suite.Suite) > 256 {
		return fmt.Errorf("suite %q has no bounded result suite name", suite.ID)
	}
	if suite.ResultKind != ResultKindBench && suite.ResultKind != ResultKindArchitecture {
		return fmt.Errorf("suite %q has invalid result kind %q", suite.ID, suite.ResultKind)
	}
	if suite.ExpectedPopulation <= 0 || suite.ExpectedPopulation > 1_000_000 {
		return fmt.Errorf("suite %q has invalid expected population", suite.ID)
	}
	if suite.CaseKey != CaseKeyExact && suite.CaseKey != CaseKeyBeforeFinalHash {
		return fmt.Errorf("suite %q has invalid case-key rule %q", suite.ID, suite.CaseKey)
	}
	if err := suite.Aggregate.validate(suite.ID, suite.ExpectedPopulation); err != nil {
		return err
	}
	if err := suite.Cases.validate(suite.ID, suite.ExpectedPopulation); err != nil {
		return err
	}
	if suite.Suite == "fdb-v1.5" {
		if suite.Aggregate.Registration.Status == RegistrationRegistered &&
			(suite.Aggregate.MinimumApplicable == nil || *suite.Aggregate.MinimumApplicable == 0) {
			return fmt.Errorf("suite %s aggregate requires a positive minimum applicable population", suite.ID)
		}
		for _, target := range suite.Cases.Targets {
			if target.MinimumApplicable == nil {
				return fmt.Errorf("suite %s requires explicit minimum applicable for case %q", suite.ID, target.Case)
			}
		}
	}
	if err := suite.Safety.validate(suite.ID, "safety"); err != nil {
		return err
	}
	if err := suite.Deadline.validate(suite.ID, "deadline"); err != nil {
		return err
	}
	if err := suite.Latency.validate(suite.ID, "latency"); err != nil {
		return err
	}
	return nil
}

func (registration Registration) validate(label string, allowNotApplicable bool) error {
	switch registration.Status {
	case RegistrationRegistered:
		if strings.TrimSpace(registration.Source) == "" || len(registration.Source) > 4096 {
			return fmt.Errorf("%s registered target has no bounded source", label)
		}
		if registration.Reason != "" {
			return fmt.Errorf("%s registered target carries an unavailable reason", label)
		}
	case RegistrationUnavailable:
		if strings.TrimSpace(registration.Reason) == "" || len(registration.Reason) > 4096 {
			return fmt.Errorf("%s unavailable target has no bounded reason", label)
		}
	case RegistrationNotApplicable:
		if !allowNotApplicable {
			return fmt.Errorf("%s cannot be not applicable", label)
		}
		if strings.TrimSpace(registration.Reason) == "" || len(registration.Reason) > 4096 {
			return fmt.Errorf("%s not-applicable target has no bounded rationale", label)
		}
	default:
		return fmt.Errorf("%s has invalid registration status %q", label, registration.Status)
	}
	return nil
}

func (target AggregateTarget) validate(id string, population int) error {
	label := "suite " + id + " aggregate"
	if err := target.Registration.validate(label, false); err != nil {
		return err
	}
	if target.Registration.Status == RegistrationRegistered {
		if target.MinimumPassed == nil || *target.MinimumPassed < 0 || *target.MinimumPassed > population {
			return fmt.Errorf("%s has invalid minimum passed", label)
		}
		if target.MinimumApplicable != nil && (*target.MinimumApplicable < 0 || *target.MinimumApplicable > population) {
			return fmt.Errorf("%s has invalid minimum applicable", label)
		}
	} else if target.MinimumPassed != nil || target.MinimumApplicable != nil {
		return fmt.Errorf("%s has a threshold without a registered target", label)
	}
	return nil
}

func (set CaseTargetSet) validate(id string, population int) error {
	label := "suite " + id + " per-case"
	if err := set.Registration.validate(label, false); err != nil {
		return err
	}
	if set.Registration.Status != RegistrationRegistered {
		if len(set.Targets) != 0 {
			return fmt.Errorf("%s has targets without a registration", label)
		}
		return nil
	}
	if len(set.Targets) == 0 {
		return fmt.Errorf("%s registration has no targets", label)
	}
	total := 0
	previous := ""
	for _, target := range set.Targets {
		if strings.TrimSpace(target.Case) == "" || len(target.Case) > 4096 ||
			target.ExpectedAttempts <= 0 || target.MinimumPassed < 0 ||
			target.MinimumPassed > target.ExpectedAttempts {
			return fmt.Errorf("%s has an invalid case target", label)
		}
		if target.MinimumApplicable != nil && (*target.MinimumApplicable < 0 || *target.MinimumApplicable > target.ExpectedAttempts) {
			return fmt.Errorf("%s has an invalid minimum applicable for case %q", label, target.Case)
		}
		if previous != "" && previous >= target.Case {
			return fmt.Errorf("%s targets must be uniquely sorted by case", label)
		}
		previous = target.Case
		total += target.ExpectedAttempts
	}
	if total != population {
		return fmt.Errorf("%s targets cover %d attempts, want %d", label, total, population)
	}
	return nil
}

func (set EvidenceTargetSet) validate(id, domain string) error {
	label := "suite " + id + " " + domain
	if err := set.Registration.validate(label, true); err != nil {
		return err
	}
	if set.Registration.Status != RegistrationRegistered {
		if len(set.Rules) != 0 || len(set.Exclusions) != 0 {
			return fmt.Errorf("%s has rules or exclusions without a registration", label)
		}
		return nil
	}
	if len(set.Rules) == 0 {
		return fmt.Errorf("%s registration has no evidence rules", label)
	}
	seenNames := make(map[string]bool, len(set.Rules))
	seenMetricStatistics := make(map[string]bool, len(set.Rules))
	hasMedian, hasTail := false, false
	for _, rule := range set.Rules {
		if err := rule.validate(label); err != nil {
			return err
		}
		if seenNames[rule.Name] {
			return fmt.Errorf("%s repeats rule name %q", label, rule.Name)
		}
		seenNames[rule.Name] = true
		key := rule.Metric + "\x00" + rule.Statistic
		if seenMetricStatistics[key] {
			return fmt.Errorf("%s repeats %s statistic %s", label, rule.Metric, rule.Statistic)
		}
		seenMetricStatistics[key] = true
		if domain == "safety" && (rule.Comparison != ComparisonAtMost || rule.Threshold != 0 ||
			!rule.RequireEveryTask || (rule.Statistic != StatisticSum && rule.Statistic != StatisticMax)) {
			return fmt.Errorf("%s rule %q is not a zero-tolerance every-task failure bound", label, rule.Name)
		}
		if rule.Statistic == StatisticP50 {
			hasMedian = true
		}
		if slices.Contains([]string{StatisticP90, StatisticP95, StatisticP99, StatisticMax}, rule.Statistic) {
			hasTail = true
		}
	}
	seenExcluded := make(map[string]bool, len(set.Exclusions))
	for _, exclusion := range set.Exclusions {
		if strings.TrimSpace(exclusion.Metric) == "" || len(exclusion.Metric) > 512 ||
			strings.TrimSpace(exclusion.Reason) == "" || len(exclusion.Reason) > 4096 {
			return fmt.Errorf("%s has an invalid metric exclusion", label)
		}
		if seenExcluded[exclusion.Metric] {
			return fmt.Errorf("%s repeats excluded metric %q", label, exclusion.Metric)
		}
		seenExcluded[exclusion.Metric] = true
		for _, rule := range set.Rules {
			if rule.Metric == exclusion.Metric {
				return fmt.Errorf("%s both targets and excludes metric %q", label, exclusion.Metric)
			}
		}
	}
	if domain == "safety" && len(set.Exclusions) != 0 {
		return fmt.Errorf("%s cannot exclude safety metrics", label)
	}
	if domain == "latency" && (!hasMedian || !hasTail) {
		return fmt.Errorf("%s must preregister both median and tail limits", label)
	}
	return nil
}

func (rule MetricRule) validate(label string) error {
	if !behavioralIDPattern.MatchString(rule.Name) || strings.TrimSpace(rule.Metric) == "" ||
		len(rule.Metric) > 512 || !isStatistic(rule.Statistic) ||
		(rule.Comparison != ComparisonAtLeast && rule.Comparison != ComparisonAtMost) ||
		math.IsNaN(rule.Threshold) || math.IsInf(rule.Threshold, 0) ||
		rule.MinimumSamples <= 0 || rule.MinimumSamples > 1_000_000 {
		return fmt.Errorf("%s has invalid metric rule %q", label, rule.Name)
	}
	return nil
}

func isStatistic(value string) bool {
	return slices.Contains([]string{
		StatisticSum, StatisticCount, StatisticMin, StatisticP50, StatisticP90,
		StatisticP95, StatisticP99, StatisticMax, StatisticMean,
	}, value)
}

func (candidate FrozenCandidate) Validate() error {
	if candidate.FormatVersion != FrozenCandidateVersion {
		return fmt.Errorf("frozen candidate format must be %d", FrozenCandidateVersion)
	}
	if !behavioralIDPattern.MatchString(candidate.CandidateID) {
		return fmt.Errorf("invalid frozen candidate ID %q", candidate.CandidateID)
	}
	if !revisionPattern.MatchString(candidate.Revision) {
		return errors.New("frozen candidate revision is not a full lowercase Git object ID")
	}
	if !sha256Pattern.MatchString(candidate.ExecutableSHA256) {
		return errors.New("frozen candidate executable digest is not canonical SHA-256")
	}
	if err := validateFrozenMachine(candidate.Machine); err != nil {
		return err
	}
	if len(candidate.Suites) == 0 {
		return errors.New("frozen candidate has no suite identities")
	}
	previous := ""
	for index, suite := range candidate.Suites {
		if err := suite.validate(); err != nil {
			return fmt.Errorf("frozen candidate suite %d: %w", index, err)
		}
		if previous != "" && previous >= suite.ID {
			return errors.New("frozen candidate suites must be uniquely sorted by ID")
		}
		previous = suite.ID
	}
	return nil
}

func validateFrozenMachine(machine bench.Machine) error {
	if machine.Cores <= 0 || machine.Cores > 1_000_000 ||
		strings.TrimSpace(machine.OS) == "" || strings.TrimSpace(machine.Arch) == "" ||
		strings.TrimSpace(machine.GoVersion) == "" {
		return errors.New("frozen candidate machine identity is incomplete")
	}
	for _, value := range []string{machine.CPU, machine.GPU, machine.OS, machine.Arch, machine.GoVersion, machine.Hostname} {
		if len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("frozen candidate machine identity is not bounded canonical text")
		}
	}
	return nil
}

func (suite FrozenCandidateSuite) validate() error {
	if !behavioralIDPattern.MatchString(suite.ID) ||
		!sha256Pattern.MatchString(suite.ExecutionRequirementSHA256) ||
		!sha256Pattern.MatchString(suite.RunSpecSHA256) ||
		!sha256Pattern.MatchString(suite.TaskInventorySHA256) ||
		!sha256Pattern.MatchString(suite.ScorerSHA256) {
		return fmt.Errorf("suite %q has an invalid ID or frozen campaign digest", suite.ID)
	}
	if len(suite.SourceReceipts) == 0 || len(suite.SourceReceipts) > maximumCampaignSourceReceipts {
		return fmt.Errorf("suite %q has no bounded source-receipt requirements", suite.ID)
	}
	previousSource := ""
	for _, source := range suite.SourceReceipts {
		if !behavioralIDPattern.MatchString(source.Kind) ||
			validateCampaignText(source.ArtifactFormat, 256, false) != nil {
			return fmt.Errorf("suite %q has an invalid source-receipt requirement", suite.ID)
		}
		key := source.Kind + "\x00" + source.ArtifactFormat
		if previousSource != "" && previousSource >= key {
			return fmt.Errorf("suite %q source-receipt requirements must be uniquely sorted", suite.ID)
		}
		previousSource = key
	}
	if len(suite.Lineage) == 0 || len(suite.Lineage) > 10_000 {
		return fmt.Errorf("suite %q has no bounded campaign lineage", suite.ID)
	}
	seen := make(map[string]bool, len(suite.Lineage))
	for index, run := range suite.Lineage {
		if !behavioralIDPattern.MatchString(run.CampaignID) || seen[run.CampaignID] || run.Population <= 0 {
			return fmt.Errorf("suite %q has invalid lineage row %d", suite.ID, index)
		}
		seen[run.CampaignID] = true
		switch run.Kind {
		case RunFailedFull:
			if !sha256Pattern.MatchString(run.ArtifactSHA256) {
				return fmt.Errorf("suite %q failed full run %q does not retain an artifact digest", suite.ID, run.CampaignID)
			}
			foundFocused := false
			for _, later := range suite.Lineage[index+1:] {
				if later.Kind == RunFocusedDiagnostic {
					foundFocused = true
					break
				}
			}
			if !foundFocused {
				return fmt.Errorf("suite %q failed full run %q has no later focused diagnostic", suite.ID, run.CampaignID)
			}
		case RunFocusedDiagnostic:
			if !sha256Pattern.MatchString(run.ArtifactSHA256) {
				return fmt.Errorf("suite %q focused run %q does not retain an artifact digest", suite.ID, run.CampaignID)
			}
		case RunFinalFull:
			if index != len(suite.Lineage)-1 || run.ArtifactSHA256 != "" {
				return fmt.Errorf("suite %q final full run must be the predeclared last lineage row without a digest", suite.ID)
			}
		default:
			return fmt.Errorf("suite %q has unknown lineage kind %q", suite.ID, run.Kind)
		}
	}
	if suite.Lineage[len(suite.Lineage)-1].Kind != RunFinalFull {
		return fmt.Errorf("suite %q lineage ends in diagnostic-only evidence", suite.ID)
	}
	return nil
}

func digestBytes(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}
