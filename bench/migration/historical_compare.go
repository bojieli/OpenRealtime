package migration

import (
	"fmt"
	"sort"
)

// HistoricalComparisonReportVersion is the only candidate-to-recorded-
// baseline report schema emitted by this package.
const HistoricalComparisonReportVersion = 1

// HistoricalRateComparison compares a fully evidenced candidate rate with an
// owner-accepted historical numerator and denominator. No paired transition
// or confidence interval is fabricated when historical attempts did not
// survive.
type HistoricalRateComparison struct {
	Name          string         `json:"name"`
	Baseline      HistoricalRate `json:"baseline"`
	Candidate     HistoricalRate `json:"candidate"`
	BaselineRate  float64        `json:"baseline_rate"`
	CandidateRate float64        `json:"candidate_rate"`
	Difference    float64        `json:"difference"`
	Policy        *RatePolicy    `json:"policy,omitempty"`
	Gate          GateResult     `json:"gate"`
}

// HistoricalSafetyComparison keeps the accepted original violation count
// visible while applying a hard zero-violation candidate gate.
type HistoricalSafetyComparison struct {
	Baseline  *HistoricalSafety `json:"baseline,omitempty"`
	Candidate HistoricalSafety  `json:"candidate"`
	Gate      GateResult        `json:"gate"`
}

// HistoricalLatencyLimitResult applies a manifest-pinned direct limit to one
// recorded distribution statistic. It intentionally has no synthetic paired
// bootstrap bound.
type HistoricalLatencyLimitResult struct {
	Statistic        LatencyStatistic `json:"statistic"`
	Baseline         float64          `json:"baseline"`
	Candidate        float64          `json:"candidate"`
	ObservedIncrease float64          `json:"observed_increase"`
	MaximumIncrease  float64          `json:"maximum_increase"`
	Gate             GateResult       `json:"gate"`
}

// HistoricalLatencyComparison exposes both complete distributions and every
// preregistered direct non-regression decision.
type HistoricalLatencyComparison struct {
	Name            string                         `json:"name"`
	Unit            string                         `json:"unit"`
	ApplicableCases int                            `json:"applicable_cases"`
	Baseline        Distribution                   `json:"baseline"`
	Candidate       Distribution                   `json:"candidate"`
	Limits          []HistoricalLatencyLimitResult `json:"limits"`
}

// HistoricalPartitionComparison is the condition/case view. It is emitted
// only for partitions whose manifest policy is acceptance-authoritative.
type HistoricalPartitionComparison struct {
	Condition   string                        `json:"condition"`
	Case        string                        `json:"case,omitempty"`
	Accepted    bool                          `json:"accepted"`
	Pass        HistoricalRateComparison      `json:"pass"`
	Interaction *HistoricalRateComparison     `json:"interaction,omitempty"`
	Deadline    *HistoricalRateComparison     `json:"deadline,omitempty"`
	Safety      HistoricalSafetyComparison    `json:"safety"`
	Latencies   []HistoricalLatencyComparison `json:"latencies"`
	Gates       []GateResult                  `json:"gates"`
}

// HistoricalSuiteComparison is never pooled with another suite.
type HistoricalSuiteComparison struct {
	Suite       string                          `json:"suite"`
	Accepted    bool                            `json:"accepted"`
	Pass        HistoricalRateComparison        `json:"pass"`
	PassFloor   GateResult                      `json:"pass_floor"`
	Interaction *HistoricalRateComparison       `json:"interaction,omitempty"`
	Deadline    *HistoricalRateComparison       `json:"deadline,omitempty"`
	Safety      HistoricalSafetyComparison      `json:"safety"`
	Latencies   []HistoricalLatencyComparison   `json:"latencies"`
	Conditions  []HistoricalPartitionComparison `json:"conditions"`
	Cases       []HistoricalPartitionComparison `json:"cases"`
	Gates       []GateResult                    `json:"gates"`
}

// HistoricalComparisonReport contains only candidate attempts. Historical
// authority remains the exact canonical registry; no historical Attempt row,
// evidence reference, media file, or transition is invented.
type HistoricalComparisonReport struct {
	Version      int                         `json:"version"`
	ManifestID   string                      `json:"manifest_id"`
	RegistryID   string                      `json:"registry_id"`
	ReportID     string                      `json:"report_id"`
	Manifest     Manifest                    `json:"manifest"`
	Registry     HistoricalBaselineRegistry  `json:"registry"`
	Candidates   []Attempt                   `json:"candidates"`
	Refusals     []Finding                   `json:"refusals"`
	Reportable   bool                        `json:"reportable"`
	Accepted     bool                        `json:"accepted"`
	CampaignGate GateResult                  `json:"campaign_gate"`
	Comparisons  []HistoricalSuiteComparison `json:"comparisons"`
}

// CompareCandidateToHistorical validates the exact candidate population and
// compares it with trusted original numbers. Missing historical per-attempt
// artifacts are neither requested nor synthesized. Any missing, duplicate,
// incomplete, unattested, or malformed candidate remains a structural
// refusal and prevents all aggregate comparisons.
func CompareCandidateToHistorical(
	manifest Manifest, registry HistoricalBaselineRegistry, candidate []Attempt,
) HistoricalComparisonReport {
	manifest = canonicalManifest(manifest)
	registry = canonicalHistoricalBaselineRegistry(registry)
	report := HistoricalComparisonReport{
		Version:    HistoricalComparisonReportVersion,
		ManifestID: manifest.ID(), RegistryID: registry.RegistryID,
		Manifest: manifest, Registry: registry,
		Candidates:  canonicalCandidateAttempts(candidate),
		Refusals:    []Finding{},
		Comparisons: []HistoricalSuiteComparison{},
	}

	findings := validateManifest(manifest)
	if err := registry.ValidateAgainst(manifest); err != nil {
		findings = append(findings, Finding{Code: "historical.registry_invalid",
			Scope: "historical-registry", Message: err.Error()})
	}

	suites := make(map[string]SuiteSpec, len(manifest.Suites))
	expected := make(map[AttemptKey]bool)
	for _, suite := range manifest.Suites {
		suites[suite.Name] = suite
		for _, item := range suite.Cases {
			for _, repetition := range item.Repetitions {
				expected[AttemptKey{Suite: suite.Name, Condition: item.Condition,
					Case: item.ID, Repetition: repetition}] = true
			}
		}
	}
	byKey := make(map[AttemptKey][]Attempt, len(expected))
	seenIDs := make(map[string]bool, len(report.Candidates))
	for _, attempt := range report.Candidates {
		scope := "candidate/" + keyScope(attempt.Key)
		if seenIDs[attempt.ID] && attempt.ID != "" {
			findings = append(findings, Finding{Code: "attempt.id_duplicate", Scope: scope,
				Message: fmt.Sprintf("attempt ID %q is duplicated in the candidate arm", attempt.ID)})
		}
		seenIDs[attempt.ID] = true
		byKey[attempt.Key] = append(byKey[attempt.Key], attempt)
		suite, known := suites[attempt.Key.Suite]
		findings = append(findings, validateAttempt(manifest, suite, known,
			expected[attempt.Key], ArmCandidate, attempt, scope)...)
	}
	for _, key := range sortedKeys(expected) {
		count := len(byKey[key])
		scope := "candidate/" + keyScope(key)
		switch {
		case count == 0:
			findings = append(findings, Finding{Code: "attempt.missing", Scope: scope,
				Message: "the manifest-declared candidate attempt is missing"})
		case count > 1:
			findings = append(findings, Finding{Code: "attempt.duplicate", Scope: scope,
				Message: fmt.Sprintf("the manifest-declared candidate key has %d attempts", count)})
		}
	}
	sortFindings(findings)
	report.Refusals = findings
	if len(findings) == 0 {
		report.Comparisons = compareHistoricalSuites(manifest, registry, report.Candidates)
		report.Reportable = true
		report.Accepted = true
		for _, comparison := range report.Comparisons {
			if !comparison.Accepted {
				report.Accepted = false
				break
			}
		}
	}
	applyHistoricalCampaignGate(&report)
	report.ReportID = historicalComparisonDigest(report)
	return report
}

func canonicalCandidateAttempts(input []Attempt) []Attempt {
	observed := canonicalObserved(ArmCandidate, input)
	result := make([]Attempt, len(observed))
	for index := range observed {
		result[index] = observed[index].Attempt
	}
	return result
}

func compareHistoricalSuites(
	manifest Manifest, registry HistoricalBaselineRegistry, candidate []Attempt,
) []HistoricalSuiteComparison {
	registryBySuite := make(map[string]HistoricalSuiteBaseline, len(registry.Suites))
	for _, suite := range registry.Suites {
		registryBySuite[suite.Suite] = suite
	}
	result := make([]HistoricalSuiteComparison, 0, len(manifest.Suites))
	for _, suite := range manifest.Suites {
		var attempts []Attempt
		for _, attempt := range candidate {
			if attempt.Key.Suite == suite.Name {
				attempts = append(attempts, attempt)
			}
		}
		result = append(result, compareHistoricalSuite(
			suite, registryBySuite[suite.Name], attempts))
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Suite < result[right].Suite })
	return result
}

func compareHistoricalSuite(
	suite SuiteSpec, baseline HistoricalSuiteBaseline, candidate []Attempt,
) HistoricalSuiteComparison {
	result := HistoricalSuiteComparison{
		Suite: suite.Name, Accepted: true,
		Pass: compareHistoricalRate(pathIdentity(suite.Name, "pass", "non_inferiority"),
			baseline.Pass, candidateRate(candidate, outcomePass), &suite.Policy.Pass),
		Safety: compareHistoricalSafety(pathIdentity(suite.Name, "safety", "zero_tolerance"),
			baseline.Safety, candidateSafety(candidate)),
		Latencies: compareHistoricalLatencies(pathIdentity(suite.Name, "latency"),
			baseline.Latencies, candidate, suite.Policy.Latencies),
		Conditions: []HistoricalPartitionComparison{},
		Cases:      []HistoricalPartitionComparison{},
		Gates:      []GateResult{},
	}
	if baseline.Interaction != nil {
		policy := suite.Policy.Interaction.NonInferiority
		comparison := compareHistoricalRate(
			pathIdentity(suite.Name, "interaction", "non_inferiority"),
			*baseline.Interaction, candidateRate(candidate, outcomeInteraction), policy)
		result.Interaction = &comparison
	}
	if baseline.Deadline != nil {
		policy := suite.Policy.Deadline.NonInferiority
		comparison := compareHistoricalRate(
			pathIdentity(suite.Name, "deadline", "non_inferiority"),
			*baseline.Deadline, candidateRate(candidate, outcomeDeadline), policy)
		result.Deadline = &comparison
	}
	result.PassFloor = historicalPassFloor(suite.Name, result.Pass)
	result.Gates = appendHistoricalComparisonGates(result.Gates,
		result.Pass, result.Interaction, result.Deadline, result.Safety, result.Latencies)

	populationByCondition := make(map[string]Population, len(suite.Populations))
	for _, population := range suite.Populations {
		populationByCondition[population.Condition] = population
	}
	for _, partition := range baseline.Conditions {
		population := populationByCondition[partition.Condition]
		var subset []Attempt
		for _, attempt := range candidate {
			if attempt.Key.Condition == partition.Condition {
				subset = append(subset, attempt)
			}
		}
		result.Conditions = append(result.Conditions, compareHistoricalPartition(
			suite.Name, partition, subset, *population.Policy))
	}
	caseByIdentity := make(map[string]CaseSpec, len(suite.Cases))
	for _, item := range suite.Cases {
		caseByIdentity[pathIdentity(item.Condition, item.ID)] = item
	}
	for _, partition := range baseline.Cases {
		item := caseByIdentity[pathIdentity(partition.Condition, partition.Case)]
		var subset []Attempt
		for _, attempt := range candidate {
			if attempt.Key.Condition == partition.Condition && attempt.Key.Case == partition.Case {
				subset = append(subset, attempt)
			}
		}
		result.Cases = append(result.Cases, compareHistoricalPartition(
			suite.Name, partition, subset, *item.Policy))
	}
	for _, condition := range result.Conditions {
		result.Gates = append(result.Gates, condition.Gates...)
	}
	for _, item := range result.Cases {
		result.Gates = append(result.Gates, item.Gates...)
	}
	result.Gates = append(result.Gates, result.PassFloor)
	sort.Slice(result.Gates, func(left, right int) bool { return result.Gates[left].Name < result.Gates[right].Name })
	for _, gate := range result.Gates {
		if !gate.Passed {
			result.Accepted = false
		}
	}
	return result
}

type historicalOutcomeKind int

const (
	outcomePass historicalOutcomeKind = iota
	outcomeInteraction
	outcomeDeadline
)

func candidateRate(attempts []Attempt, kind historicalOutcomeKind) HistoricalRate {
	result := HistoricalRate{}
	cases := map[string]bool{}
	for _, attempt := range attempts {
		applicable, successful := true, false
		switch kind {
		case outcomePass:
			successful = attempt.Passed
		case outcomeInteraction:
			applicable = attempt.Outcomes.Interaction != OutcomeNotApplicable
			successful = attempt.Outcomes.Interaction == OutcomeSatisfied
		case outcomeDeadline:
			applicable = attempt.Outcomes.Deadline != OutcomeNotApplicable
			successful = attempt.Outcomes.Deadline == OutcomeSatisfied
		}
		if !applicable {
			continue
		}
		result.ApplicableAttempts++
		cases[pathIdentity(attempt.Key.Condition, attempt.Key.Case)] = true
		if successful {
			result.Successes++
		}
	}
	result.ApplicableCases = len(cases)
	return result
}

func candidateSafety(attempts []Attempt) HistoricalSafety {
	result := HistoricalSafety{}
	cases := map[string]bool{}
	for _, attempt := range attempts {
		if attempt.Outcomes.Safety == OutcomeNotApplicable {
			continue
		}
		result.ApplicableAttempts++
		cases[pathIdentity(attempt.Key.Condition, attempt.Key.Case)] = true
		if attempt.Outcomes.Safety == OutcomeFailed {
			result.Violations++
		}
	}
	result.ApplicableCases = len(cases)
	return result
}

func compareHistoricalRate(
	name string, baseline, candidate HistoricalRate, policy *RatePolicy,
) HistoricalRateComparison {
	result := HistoricalRateComparison{Name: name, Baseline: baseline, Candidate: candidate,
		Policy: cloneRatePolicy(policy)}
	result.BaselineRate = historicalRateValue(baseline)
	result.CandidateRate = historicalRateValue(candidate)
	result.Difference = result.CandidateRate - result.BaselineRate
	if policy == nil {
		return result
	}
	result.Gate = GateResult{Name: name, Evaluated: true}
	switch {
	case candidate.ApplicableAttempts < policy.MinimumAttempts:
		result.Gate.Reason = fmt.Sprintf("candidate has %d applicable attempts, minimum is %d",
			candidate.ApplicableAttempts, policy.MinimumAttempts)
	case candidate.ApplicableCases < policy.MinimumCases:
		result.Gate.Reason = fmt.Sprintf("candidate has %d applicable cases, minimum is %d",
			candidate.ApplicableCases, policy.MinimumCases)
	case result.Difference < -policy.Margin:
		result.Gate.Reason = fmt.Sprintf(
			"candidate-minus-recorded-baseline rate %.9g is below allowed %.9g",
			result.Difference, -policy.Margin)
	default:
		result.Gate.Passed = true
	}
	return result
}

func historicalRateValue(rate HistoricalRate) float64 {
	if rate.ApplicableAttempts == 0 {
		return 0
	}
	return float64(rate.Successes) / float64(rate.ApplicableAttempts)
}

func compareHistoricalSafety(
	name string, baseline *HistoricalSafety, candidate HistoricalSafety,
) HistoricalSafetyComparison {
	result := HistoricalSafetyComparison{Baseline: cloneHistoricalSafety(baseline),
		Candidate: candidate, Gate: GateResult{Name: name, Evaluated: true}}
	if candidate.Violations == 0 {
		result.Gate.Passed = true
	} else {
		result.Gate.Reason = fmt.Sprintf("candidate has %d safety violations; zero are allowed",
			candidate.Violations)
	}
	return result
}

func historicalPassFloor(suite string, pass HistoricalRateComparison) GateResult {
	result := GateResult{Name: pathIdentity(suite, "pass", "absolute_sanity_floor"), Evaluated: true}
	if pass.BaselineRate <= CandidatePassRateSanityFloor ||
		pass.CandidateRate >= CandidatePassRateSanityFloor {
		result.Passed = true
	} else {
		result.Reason = fmt.Sprintf(
			"recorded baseline rate %.9g exceeds %.2f but candidate rate %.9g is below it",
			pass.BaselineRate, CandidatePassRateSanityFloor, pass.CandidateRate)
	}
	return result
}

func compareHistoricalLatencies(
	scope string, baseline []HistoricalLatency, candidate []Attempt,
	policies []LatencyPolicy,
) []HistoricalLatencyComparison {
	baselineByName := make(map[string]HistoricalLatency, len(baseline))
	for _, latency := range baseline {
		baselineByName[latency.Name] = latency
	}
	result := make([]HistoricalLatencyComparison, 0, len(policies))
	for _, policy := range policies {
		values := make([]float64, 0, len(candidate))
		cases := map[string]bool{}
		for _, attempt := range candidate {
			for _, latency := range attempt.Latencies {
				if latency.Name == policy.Name {
					values = append(values, latency.Value)
					cases[pathIdentity(attempt.Key.Condition, attempt.Key.Case)] = true
				}
			}
		}
		original := baselineByName[policy.Name]
		comparison := HistoricalLatencyComparison{Name: policy.Name, Unit: policy.Unit,
			ApplicableCases: len(cases), Baseline: original.Distribution,
			Candidate: distribution(values, policy.Unit),
			Limits:    []HistoricalLatencyLimitResult{}}
		if policy.Gate != nil {
			for _, limit := range policy.Gate.Limits {
				baselineValue := historicalDistributionStatistic(original.Distribution, limit.Statistic)
				candidateValue := historicalDistributionStatistic(comparison.Candidate, limit.Statistic)
				gateName := pathIdentity(scope, policy.Name, string(limit.Statistic), "direct_limit")
				reading := HistoricalLatencyLimitResult{Statistic: limit.Statistic,
					Baseline: baselineValue, Candidate: candidateValue,
					ObservedIncrease: candidateValue - baselineValue,
					MaximumIncrease:  limit.MaximumIncrease,
					Gate:             GateResult{Name: gateName, Evaluated: true}}
				switch {
				case comparison.Candidate.Count < policy.Gate.MinimumAttempts:
					reading.Gate.Reason = fmt.Sprintf(
						"candidate has %d latency attempts, minimum is %d",
						comparison.Candidate.Count, policy.Gate.MinimumAttempts)
				case comparison.ApplicableCases < policy.Gate.MinimumCases:
					reading.Gate.Reason = fmt.Sprintf(
						"candidate has %d latency cases, minimum is %d",
						comparison.ApplicableCases, policy.Gate.MinimumCases)
				case reading.ObservedIncrease > limit.MaximumIncrease:
					reading.Gate.Reason = fmt.Sprintf(
						"candidate-minus-recorded-baseline %s increase %.9g exceeds %.9g",
						limit.Statistic, reading.ObservedIncrease, limit.MaximumIncrease)
				default:
					reading.Gate.Passed = true
				}
				comparison.Limits = append(comparison.Limits, reading)
			}
		}
		result = append(result, comparison)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Name < result[right].Name })
	return result
}

func historicalDistributionStatistic(distribution Distribution, statistic LatencyStatistic) float64 {
	switch statistic {
	case StatisticMean:
		return distribution.Mean
	case StatisticP50:
		return distribution.P50
	case StatisticP90:
		return distribution.P90
	case StatisticP95:
		return distribution.P95
	case StatisticP99:
		return distribution.P99
	default:
		return 0
	}
}

func compareHistoricalPartition(
	suite string, baseline HistoricalPartitionBaseline, candidate []Attempt,
	policy PartitionPolicy,
) HistoricalPartitionComparison {
	scopeParts := []string{suite, "condition", baseline.Condition}
	if baseline.Case != "" {
		scopeParts = []string{suite, "case", baseline.Condition, baseline.Case}
	}
	scope := pathIdentity(scopeParts...)
	result := HistoricalPartitionComparison{
		Condition: baseline.Condition, Case: baseline.Case, Accepted: true,
		Pass: compareHistoricalRate(pathIdentity(scope, "pass", "non_inferiority"),
			baseline.Pass, candidateRate(candidate, outcomePass), &policy.Pass),
		Safety: compareHistoricalSafety(pathIdentity(scope, "safety", "zero_tolerance"),
			baseline.Safety, candidateSafety(candidate)),
		Latencies: compareHistoricalLatencies(pathIdentity(scope, "latency"),
			baseline.Latencies, candidate, policy.Latencies),
		Gates: []GateResult{},
	}
	if baseline.Interaction != nil {
		comparison := compareHistoricalRate(pathIdentity(scope, "interaction", "non_inferiority"),
			*baseline.Interaction, candidateRate(candidate, outcomeInteraction),
			policy.Interaction.NonInferiority)
		result.Interaction = &comparison
	}
	if baseline.Deadline != nil {
		comparison := compareHistoricalRate(pathIdentity(scope, "deadline", "non_inferiority"),
			*baseline.Deadline, candidateRate(candidate, outcomeDeadline),
			policy.Deadline.NonInferiority)
		result.Deadline = &comparison
	}
	result.Gates = appendHistoricalComparisonGates(result.Gates,
		result.Pass, result.Interaction, result.Deadline, result.Safety, result.Latencies)
	sort.Slice(result.Gates, func(left, right int) bool { return result.Gates[left].Name < result.Gates[right].Name })
	for _, gate := range result.Gates {
		if !gate.Passed {
			result.Accepted = false
		}
	}
	return result
}

func appendHistoricalComparisonGates(
	gates []GateResult, pass HistoricalRateComparison,
	interaction, deadline *HistoricalRateComparison,
	safety HistoricalSafetyComparison, latencies []HistoricalLatencyComparison,
) []GateResult {
	if pass.Gate.Evaluated {
		gates = append(gates, pass.Gate)
	}
	gates = append(gates, safety.Gate)
	if interaction != nil && interaction.Gate.Evaluated {
		gates = append(gates, interaction.Gate)
	}
	if deadline != nil && deadline.Gate.Evaluated {
		gates = append(gates, deadline.Gate)
	}
	for _, latency := range latencies {
		for _, limit := range latency.Limits {
			gates = append(gates, limit.Gate)
		}
	}
	return gates
}

func applyHistoricalCampaignGate(report *HistoricalComparisonReport) {
	report.CampaignGate = GateResult{Name: pathIdentity(report.Manifest.Campaign.CampaignID,
		report.Manifest.Campaign.RunID, "historical_baseline_full_release_rerun"), Evaluated: true}
	switch {
	case !report.Reportable:
		report.Accepted = false
		report.CampaignGate.Reason = "the candidate or trusted historical registry is unreportable"
	case report.Manifest.Campaign.Kind == RunDiagnostic:
		report.Accepted = false
		report.CampaignGate.Reason = "diagnostic runs cannot satisfy the full release rerun gate"
	case report.Manifest.Campaign.Kind != RunFull:
		report.Accepted = false
		report.CampaignGate.Reason = "the run kind is invalid"
	case !report.Accepted:
		report.CampaignGate.Reason = "one or more recorded-baseline non-regression gates failed"
	default:
		report.CampaignGate.Passed = true
	}
}

func historicalComparisonDigest(report HistoricalComparisonReport) string {
	report.ReportID = ""
	return digestJSON(report)
}
