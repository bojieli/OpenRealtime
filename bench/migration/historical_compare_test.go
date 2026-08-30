package migration

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func historicalRegistryFromAttempts(
	t *testing.T, manifest Manifest, baseline []Attempt,
) HistoricalBaselineRegistry {
	t.Helper()
	bySuite := make(map[string][]Attempt)
	for _, attempt := range baseline {
		bySuite[attempt.Key.Suite] = append(bySuite[attempt.Key.Suite], attempt)
	}
	registry := HistoricalBaselineRegistry{
		Version: HistoricalBaselineRegistryVersion, Authority: "benchmark-owner",
		Acceptance: "The benchmark owner accepts these surviving original numbers without reconstructing historical attempts.",
		ManifestID: manifest.ID(), Suites: []HistoricalSuiteBaseline{},
	}
	for _, suite := range manifest.Suites {
		attempts := bySuite[suite.Name]
		historical := HistoricalSuiteBaseline{
			Suite: suite.Name, ExpectedCases: suite.ExpectedCases,
			ExpectedAttempts: suite.ExpectedAttempts,
			Trail:            []string{"owner-recorded original result trail for " + suite.Name},
			Pass:             candidateRate(attempts, outcomePass),
			Latencies:        historicalLatenciesFromAttempts(attempts, suite.Policy.Latencies),
			Conditions:       []HistoricalPartitionBaseline{},
			Cases:            []HistoricalPartitionBaseline{},
		}
		if value := candidateRate(attempts, outcomeInteraction); value.ApplicableAttempts > 0 {
			historical.Interaction = &value
		}
		if value := candidateRate(attempts, outcomeDeadline); value.ApplicableAttempts > 0 {
			historical.Deadline = &value
		}
		if value := candidateSafety(attempts); value.ApplicableAttempts > 0 {
			historical.Safety = &value
		}
		for _, population := range suite.Populations {
			if !population.RequirePolicy && population.Policy == nil {
				continue
			}
			var subset []Attempt
			for _, attempt := range attempts {
				if attempt.Key.Condition == population.Condition {
					subset = append(subset, attempt)
				}
			}
			historical.Conditions = append(historical.Conditions,
				historicalPartitionFromAttempts(population.Condition, "", subset, *population.Policy))
		}
		for _, item := range suite.Cases {
			if !item.RequirePolicy && item.Policy == nil {
				continue
			}
			var subset []Attempt
			for _, attempt := range attempts {
				if attempt.Key.Condition == item.Condition && attempt.Key.Case == item.ID {
					subset = append(subset, attempt)
				}
			}
			historical.Cases = append(historical.Cases,
				historicalPartitionFromAttempts(item.Condition, item.ID, subset, *item.Policy))
		}
		registry.Suites = append(registry.Suites, historical)
	}
	sealed, err := SealHistoricalBaselineRegistry(registry, manifest)
	if err != nil {
		t.Fatalf("seal derived historical registry: %v", err)
	}
	return sealed
}

func historicalPartitionFromAttempts(
	condition, caseID string, attempts []Attempt, policy PartitionPolicy,
) HistoricalPartitionBaseline {
	result := HistoricalPartitionBaseline{Condition: condition, Case: caseID,
		Pass:      candidateRate(attempts, outcomePass),
		Latencies: historicalLatenciesFromAttempts(attempts, policy.Latencies)}
	if policy.Interaction.Required || policy.Interaction.NonInferiority != nil {
		value := candidateRate(attempts, outcomeInteraction)
		result.Interaction = &value
	}
	if policy.Deadline.Required || policy.Deadline.NonInferiority != nil {
		value := candidateRate(attempts, outcomeDeadline)
		result.Deadline = &value
	}
	value := candidateSafety(attempts)
	result.Safety = &value
	return result
}

func historicalLatenciesFromAttempts(
	attempts []Attempt, policies []LatencyPolicy,
) []HistoricalLatency {
	result := make([]HistoricalLatency, 0, len(policies))
	for _, policy := range policies {
		var values []float64
		cases := map[string]bool{}
		for _, attempt := range attempts {
			for _, latency := range attempt.Latencies {
				if latency.Name == policy.Name {
					values = append(values, latency.Value)
					cases[pathIdentity(attempt.Key.Condition, attempt.Key.Case)] = true
				}
			}
		}
		result = append(result, HistoricalLatency{Name: policy.Name, Unit: policy.Unit,
			ApplicableCases: len(cases), Distribution: distribution(values, policy.Unit)})
	}
	return result
}

func TestCompareCandidateToHistoricalAcceptsCompleteEvidenceWithoutInventingBaselineAttempts(t *testing.T) {
	manifest := testManifest()
	baseline, candidate := testAttempts()
	registry := historicalRegistryFromAttempts(t, manifest, baseline)
	report := CompareCandidateToHistorical(manifest, registry, candidate)
	if !report.Reportable || !report.Accepted || !report.CampaignGate.Passed ||
		len(report.Refusals) != 0 || len(report.Candidates) != 6 || len(report.Comparisons) != 1 {
		t.Fatalf("historical comparison = %+v", report)
	}
	comparison := report.Comparisons[0]
	if comparison.Pass.Baseline.Successes != 4 || comparison.Pass.Candidate.Successes != 4 ||
		comparison.Pass.Difference != 0 || !comparison.Pass.Gate.Passed ||
		comparison.Safety.Candidate.Violations != 0 || !comparison.Safety.Gate.Passed ||
		len(comparison.Latencies) != 1 || len(comparison.Latencies[0].Limits) != 2 {
		t.Fatalf("suite comparison = %+v", comparison)
	}
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"baseline_id", "matched", "paired_difference", "transitions"} {
		if strings.Contains(string(payload), `"`+forbidden+`"`) {
			t.Fatalf("historical report fabricated paired field %q", forbidden)
		}
	}

	firstID := report.ReportID
	slices.Reverse(candidate)
	reordered := CompareCandidateToHistorical(manifest, registry, candidate)
	if reordered.ReportID != firstID {
		t.Fatalf("candidate order changed report ID: %s != %s", reordered.ReportID, firstID)
	}
}

func TestCompareCandidateToHistoricalFailsClosedOnCandidatePopulationAndEvidence(t *testing.T) {
	manifest := testManifest()
	baseline, candidate := testAttempts()
	registry := historicalRegistryFromAttempts(t, manifest, baseline)
	tests := map[string]struct {
		edit func([]Attempt) []Attempt
		code string
	}{
		"missing": {func(values []Attempt) []Attempt { return values[:5] }, "attempt.missing"},
		"duplicate": {func(values []Attempt) []Attempt {
			return append(values, values[0])
		}, "attempt.duplicate"},
		"incomplete": {func(values []Attempt) []Attempt {
			values[0].Completed, values[0].Passed, values[0].Error = false, false, "provider timeout"
			return values
		}, "attempt.incomplete"},
		"evidence": {func(values []Attempt) []Attempt {
			values[0].Evidence = nil
			return values
		}, "attempt.evidence_kind_missing"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			copy := make([]Attempt, len(candidate))
			payload, _ := json.Marshal(candidate)
			_ = json.Unmarshal(payload, &copy)
			report := CompareCandidateToHistorical(manifest, registry, test.edit(copy))
			if report.Reportable || report.Accepted || len(report.Comparisons) != 0 ||
				!hasHistoricalFinding(report, test.code) {
				t.Fatalf("structural refusal = %+v", report)
			}
		})
	}
}

func TestCompareCandidateToHistoricalKeepsRegressionsReportableAndRejected(t *testing.T) {
	for name, edit := range map[string]func(*Manifest, []Attempt){
		"pass": func(manifest *Manifest, candidate []Attempt) {
			manifest.Suites[0].Policy.Pass.Margin = 0
			for index := range candidate {
				candidate[index].Passed = false
			}
		},
		"safety": func(_ *Manifest, candidate []Attempt) {
			candidate[0].Outcomes.Safety = OutcomeFailed
		},
		"latency": func(_ *Manifest, candidate []Attempt) {
			for index := range candidate {
				candidate[index].Latencies[0].Value += 100
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := testManifest()
			baseline, candidate := testAttempts()
			edit(&manifest, candidate)
			registry := historicalRegistryFromAttempts(t, manifest, baseline)
			report := CompareCandidateToHistorical(manifest, registry, candidate)
			if !report.Reportable || report.Accepted || len(report.Refusals) != 0 ||
				report.CampaignGate.Passed {
				t.Fatalf("behavioral regression = %+v", report)
			}
		})
	}
}

func TestCompareCandidateToHistoricalConditionRegressionCannotHideInAggregate(t *testing.T) {
	manifest, baseline, candidate := partitionedConditionFixture()
	registry := historicalRegistryFromAttempts(t, manifest, baseline)
	report := CompareCandidateToHistorical(manifest, registry, candidate)
	if !report.Reportable || report.Accepted || len(report.Refusals) != 0 {
		t.Fatalf("partition comparison = %+v", report)
	}
	comparison := report.Comparisons[0]
	if !comparison.Pass.Gate.Passed || len(comparison.Conditions) != 2 {
		t.Fatalf("aggregate/condition shape = %+v", comparison)
	}
	byCondition := map[string]HistoricalPartitionComparison{}
	for _, condition := range comparison.Conditions {
		byCondition[condition.Condition] = condition
	}
	if !byCondition["improving"].Accepted || byCondition["stable"].Accepted {
		t.Fatalf("condition results = %+v", byCondition)
	}
}

func TestCompareCandidateToHistoricalCaseRegressionCannotHideInCondition(t *testing.T) {
	manifest, baseline, candidate := partitionedConditionFixture()
	for index := range manifest.Suites[0].Cases {
		manifest.Suites[0].Cases[index].RequirePolicy = true
		manifest.Suites[0].Cases[index].Policy = partitionPolicy(2, 1)
	}
	registry := historicalRegistryFromAttempts(t, manifest, baseline)
	report := CompareCandidateToHistorical(manifest, registry, candidate)
	if !report.Reportable || report.Accepted || len(report.Refusals) != 0 {
		t.Fatalf("case comparison = %+v", report)
	}
	comparison := report.Comparisons[0]
	if len(comparison.Cases) != 4 {
		t.Fatalf("case comparisons = %d, want 4", len(comparison.Cases))
	}
	for _, item := range comparison.Cases {
		want := item.Condition == "improving"
		if item.Accepted != want {
			t.Fatalf("case %s/%s accepted=%t, want %t: %+v",
				item.Condition, item.Case, item.Accepted, want, item)
		}
	}
}

func TestCompareCandidateToHistoricalRetainsAvailableUngatedOutcome(t *testing.T) {
	manifest := testManifest()
	manifest.Suites[0].Policy.Interaction.NonInferiority = nil
	manifest.Suites[0].Policy.Interaction.Required = false
	baseline, candidate := testAttempts()
	registry := historicalRegistryFromAttempts(t, manifest, baseline)
	report := CompareCandidateToHistorical(manifest, registry, candidate)
	comparison := report.Comparisons[0]
	if comparison.Interaction == nil || comparison.Interaction.Policy != nil ||
		comparison.Interaction.Gate.Evaluated || comparison.Interaction.Baseline.Successes != 6 ||
		comparison.Interaction.Candidate.Successes != 6 {
		t.Fatalf("descriptive interaction metric was lost or gated: %+v", comparison.Interaction)
	}
}

func TestCompareCandidateToHistoricalRejectsRegistryDriftAndDiagnosticAcceptance(t *testing.T) {
	manifest := testManifest()
	baseline, candidate := testAttempts()
	registry := historicalRegistryFromAttempts(t, manifest, baseline)
	registry.Suites[0].Pass.Successes--
	report := CompareCandidateToHistorical(manifest, registry, candidate)
	if report.Reportable || !hasHistoricalFinding(report, "historical.registry_invalid") {
		t.Fatalf("registry drift was accepted: %+v", report)
	}

	manifest.Campaign.Kind = RunDiagnostic
	manifest.Campaign.Predecessors = []ReportReference{{
		RunID: "failed-full", ReportID: evidenceDigest, ArtifactSHA256: fixtureDigest,
	}}
	manifest.Campaign.DiagnosedFailures = []FailureReference{{
		ReportID: evidenceDigest, Gate: "synthetic/pass/non_inferiority",
	}}
	registry = historicalRegistryFromAttempts(t, manifest, baseline)
	report = CompareCandidateToHistorical(manifest, registry, candidate)
	if !report.Reportable || report.Accepted || report.CampaignGate.Passed {
		t.Fatalf("diagnostic comparison satisfied release gate: %+v", report)
	}
}

func hasHistoricalFinding(report HistoricalComparisonReport, code string) bool {
	for _, finding := range report.Refusals {
		if finding.Code == code {
			return true
		}
	}
	return false
}
