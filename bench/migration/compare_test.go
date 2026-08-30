package migration

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	evidenceDigest        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baselineExecutable    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	candidateExecutable   = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	baselineTopology      = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	candidateTopology     = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	fixtureDigest         = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	acceptanceBasisDigest = "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
)

func testManifest() Manifest {
	rate := func(margin float64) *RatePolicy {
		return &RatePolicy{Margin: margin, MinimumAttempts: 6, MinimumCases: 3}
	}
	return Manifest{
		Version: ManifestVersion,
		Campaign: CampaignPlan{
			CampaignID: "migration-release-1", RunID: "full-1", Kind: RunFull,
		},
		Baseline:  ArmDefinition{Name: "legacy"},
		Candidate: ArmDefinition{Name: "graph-native"},
		FixedAxes: []Axis{
			{Name: "fixture_sha256", Value: fixtureDigest},
			{Name: "machine", Value: "reserved-host-a"},
		},
		Treatment: []TreatmentDelta{
			{Axis: "executable_sha256", Baseline: baselineExecutable, Candidate: candidateExecutable},
			{Axis: "topology_sha256", Baseline: baselineTopology, Candidate: candidateTopology},
		},
		Suites: []SuiteSpec{{
			Name: "synthetic", ExpectedCases: 3, ExpectedAttempts: 6, MinimumRepetitions: 2,
			Populations: []Population{{Condition: "regular", ExpectedCases: 3, ExpectedAttempts: 6}},
			Cases: []CaseSpec{
				{Condition: "regular", ID: "case-1", Repetitions: []string{"trial-1", "trial-2"}},
				{Condition: "regular", ID: "case-2", Repetitions: []string{"trial-1", "trial-2"}},
				{Condition: "regular", ID: "case-3", Repetitions: []string{"trial-1", "trial-2"}},
			},
			Policy: SuitePolicy{
				Inference: BootstrapPolicy{Confidence: 0.95, Resamples: 1000, Seed: 42},
				AcceptanceBasis: EvidenceRef{
					Kind: AcceptanceBasisKind, Location: "baselines/synthetic-variance.json",
					SHA256: acceptanceBasisDigest,
				},
				Pass:        *rate(0.7),
				Interaction: OutcomePolicy{Required: true, NonInferiority: rate(0.7)},
				Deadline:    OutcomePolicy{Required: true, NonInferiority: rate(0.2)},
				Safety:      SafetyPolicy{Required: true},
				Latencies: []LatencyPolicy{{
					Name: "response_latency_ms", Unit: "ms", Required: true,
					Gate: &LatencyGate{MinimumAttempts: 6, MinimumCases: 3,
						Limits: []LatencyLimit{
							{Statistic: StatisticP50, MaximumIncrease: 20},
							{Statistic: StatisticP95, MaximumIncrease: 20},
						}},
				}},
				RequireEvidence:       true,
				RequiredEvidenceKinds: []string{"trace"},
			},
		}},
	}
}

func testAttempts() ([]Attempt, []Attempt) {
	baselinePass := []bool{true, true, true, false, true, false}
	candidatePass := []bool{true, false, true, true, true, false}
	var baseline, candidate []Attempt
	index := 0
	for caseNumber := 1; caseNumber <= 3; caseNumber++ {
		for trial := 1; trial <= 2; trial++ {
			key := AttemptKey{
				Suite: "synthetic", Condition: "regular",
				Case: "case-" + itoa(caseNumber), Repetition: "trial-" + itoa(trial),
			}
			baseline = append(baseline, testAttempt(
				"baseline-"+itoa(index), key, ArmBaseline, baselinePass[index], 100+float64(index)))
			candidate = append(candidate, testAttempt(
				"candidate-"+itoa(index), key, ArmCandidate, candidatePass[index], 110+float64(index)))
			index++
		}
	}
	return baseline, candidate
}

func testAttempt(id string, key AttemptKey, arm Arm, passed bool, latency float64) Attempt {
	executable, topology := baselineExecutable, baselineTopology
	if arm == ArmCandidate {
		executable, topology = candidateExecutable, candidateTopology
	}
	return Attempt{
		ID: id, Key: key,
		Axes: []Axis{
			{Name: "machine", Value: "reserved-host-a"},
			{Name: "executable_sha256", Value: executable},
			{Name: "fixture_sha256", Value: fixtureDigest},
			{Name: "topology_sha256", Value: topology},
		},
		Completed: true, Passed: passed,
		Outcomes: Outcomes{
			Interaction: OutcomeSatisfied, Deadline: OutcomeSatisfied, Safety: OutcomeSatisfied,
		},
		Latencies: []Latency{{Name: "response_latency_ms", Unit: "ms", Value: latency}},
		Evidence: []EvidenceRef{{
			Kind: "trace", Location: "traces/" + id + ".json", SHA256: evidenceDigest,
		}},
	}
}

func partitionPolicy(attempts, cases int) *PartitionPolicy {
	rate := func(margin float64) *RatePolicy {
		return &RatePolicy{Margin: margin, MinimumAttempts: attempts, MinimumCases: cases}
	}
	return &PartitionPolicy{
		Pass:        *rate(0),
		Interaction: OutcomePolicy{Required: true, NonInferiority: rate(0)},
		Deadline:    OutcomePolicy{Required: true, NonInferiority: rate(0)},
		Latencies: []LatencyPolicy{{
			Name: "response_latency_ms", Unit: "ms", Required: true,
			Gate: &LatencyGate{MinimumAttempts: attempts, MinimumCases: cases,
				Limits: []LatencyLimit{
					{Statistic: StatisticP50, MaximumIncrease: 20},
					{Statistic: StatisticP95, MaximumIncrease: 20},
				}},
		}},
	}
}

func partitionedConditionFixture() (Manifest, []Attempt, []Attempt) {
	manifest := testManifest()
	suite := &manifest.Suites[0]
	suite.ExpectedCases = 4
	suite.ExpectedAttempts = 8
	suite.Populations = []Population{
		{Condition: "improving", ExpectedCases: 2, ExpectedAttempts: 4,
			RequirePolicy: true, Policy: partitionPolicy(4, 2)},
		{Condition: "stable", ExpectedCases: 2, ExpectedAttempts: 4,
			RequirePolicy: true, Policy: partitionPolicy(4, 2)},
	}
	suite.Cases = nil
	for _, condition := range []string{"improving", "stable"} {
		for caseNumber := 1; caseNumber <= 2; caseNumber++ {
			suite.Cases = append(suite.Cases, CaseSpec{
				Condition: condition, ID: "case-" + itoa(caseNumber),
				Repetitions: []string{"trial-1", "trial-2"},
			})
		}
	}
	suite.Policy.Pass.MinimumAttempts, suite.Policy.Pass.MinimumCases = 8, 4
	suite.Policy.Pass.Margin = 0.7
	for _, outcome := range []*OutcomePolicy{&suite.Policy.Interaction, &suite.Policy.Deadline} {
		outcome.NonInferiority.MinimumAttempts = 8
		outcome.NonInferiority.MinimumCases = 4
	}
	suite.Policy.Latencies[0].Gate.MinimumAttempts = 8
	suite.Policy.Latencies[0].Gate.MinimumCases = 4

	var baseline, candidate []Attempt
	index := 0
	for _, condition := range []string{"improving", "stable"} {
		for caseNumber := 1; caseNumber <= 2; caseNumber++ {
			for trial := 1; trial <= 2; trial++ {
				key := AttemptKey{Suite: "synthetic", Condition: condition,
					Case: "case-" + itoa(caseNumber), Repetition: "trial-" + itoa(trial)}
				baselinePass := condition == "stable" || trial == 1
				candidatePass := condition == "improving" || trial == 1
				baseline = append(baseline, testAttempt(
					"partition-baseline-"+itoa(index), key, ArmBaseline, baselinePass, 100))
				candidate = append(candidate, testAttempt(
					"partition-candidate-"+itoa(index), key, ArmCandidate, candidatePass, 110))
				index++
			}
		}
	}
	return manifest, baseline, candidate
}

func TestConditionPolicyPreventsAggregatePooling(t *testing.T) {
	manifest, baseline, candidate := partitionedConditionFixture()
	report := Compare(manifest, baseline, candidate)
	if !report.Reportable || report.Accepted || len(report.Refusals) != 0 {
		t.Fatalf("partition regression should be a reportable rejection: %+v", report)
	}
	comparison := report.Comparisons[0]
	if comparison.Pass.Gate == nil || !comparison.Pass.Gate.Passed {
		t.Fatalf("aggregate fixture must pass before the partition gate: %+v", comparison.Pass)
	}
	if len(comparison.Conditions) != 2 {
		t.Fatalf("condition comparisons = %d, want 2", len(comparison.Conditions))
	}
	byCondition := map[string]ConditionComparison{}
	for _, condition := range comparison.Conditions {
		byCondition[condition.Condition] = condition
	}
	if !byCondition["improving"].Accepted || byCondition["stable"].Accepted {
		t.Fatalf("condition acceptance = %+v", byCondition)
	}
	assertSuiteGate(t, comparison, pathIdentity(
		pathIdentity("synthetic", "condition", "stable"), "pass", "non_inferiority"), false)
	if err := report.Verify(); err != nil {
		t.Fatalf("verify partition rejection: %v", err)
	}
	for index := range manifest.Suites[0].Populations {
		manifest.Suites[0].Populations[index].RequirePolicy = false
	}
	optionalButPresent := Compare(manifest, baseline, candidate)
	if !optionalButPresent.Reportable || optionalButPresent.Accepted ||
		!optionalButPresent.Comparisons[0].Conditions[1].Gated {
		t.Fatalf("a present policy must gate even when omission was allowed: %+v", optionalButPresent)
	}
}

func TestFDBenchConditionRegressionCannotHideInTwentyImprovingConditions(t *testing.T) {
	manifest := testManifest()
	suite := &manifest.Suites[0]
	suite.Name = SuiteFDBench
	suite.ExpectedCases = 42
	suite.ExpectedAttempts = 84
	suite.MinimumRepetitions = 2
	suite.Populations = nil
	suite.Cases = nil
	suite.Policy.Pass = RatePolicy{Margin: 0, MinimumAttempts: 84, MinimumCases: 42}
	for _, outcome := range []*OutcomePolicy{&suite.Policy.Interaction, &suite.Policy.Deadline} {
		outcome.NonInferiority.MinimumAttempts = 84
		outcome.NonInferiority.MinimumCases = 42
	}
	suite.Policy.Latencies[0].Gate.MinimumAttempts = 84
	suite.Policy.Latencies[0].Gate.MinimumCases = 42

	var baseline, candidate []Attempt
	attemptIndex := 0
	for conditionNumber := 1; conditionNumber <= 21; conditionNumber++ {
		condition := "condition-" + itoa(conditionNumber)
		suite.Populations = append(suite.Populations, Population{
			Condition: condition, ExpectedCases: 2, ExpectedAttempts: 4,
			RequirePolicy: true, Policy: partitionPolicy(4, 2),
		})
		for caseNumber := 1; caseNumber <= 2; caseNumber++ {
			caseID := "case-" + itoa(caseNumber)
			suite.Cases = append(suite.Cases, CaseSpec{Condition: condition, ID: caseID,
				Repetitions: []string{"trial-1", "trial-2"}})
			for trial := 1; trial <= 2; trial++ {
				key := AttemptKey{Suite: SuiteFDBench, Condition: condition,
					Case: caseID, Repetition: "trial-" + itoa(trial)}
				badCondition := conditionNumber == 1
				baselinePass := badCondition || trial == 1
				candidatePass := !badCondition
				baseline = append(baseline, testAttempt(
					"fd-baseline-"+itoa(attemptIndex), key, ArmBaseline, baselinePass, 100))
				candidate = append(candidate, testAttempt(
					"fd-candidate-"+itoa(attemptIndex), key, ArmCandidate, candidatePass, 100))
				attemptIndex++
			}
		}
	}
	report := Compare(manifest, baseline, candidate)
	if !report.Reportable || report.Accepted {
		t.Fatalf("one catastrophic FD-Bench condition must reject: %+v", report)
	}
	comparison := report.Comparisons[0]
	if comparison.Pass.Gate == nil || !comparison.Pass.Gate.Passed ||
		comparison.Pass.CandidateRate <= comparison.Pass.BaselineRate {
		t.Fatalf("aggregate must improve and pass in this fixture: %+v", comparison.Pass)
	}
	bad := comparison.Conditions[0]
	if bad.Condition != "condition-1" || bad.Accepted || bad.Pass.Gate == nil || bad.Pass.Gate.Passed {
		t.Fatalf("catastrophic condition did not fail independently: %+v", bad)
	}
}

func TestCasePolicyPreventsConditionPooling(t *testing.T) {
	manifest, baseline, candidate := partitionedConditionFixture()
	suite := &manifest.Suites[0]
	for index := range suite.Populations {
		suite.Populations[index].RequirePolicy = false
		suite.Populations[index].Policy = nil
	}
	for index := range suite.Cases {
		suite.Cases[index].RequirePolicy = true
		suite.Cases[index].Policy = partitionPolicy(2, 1)
	}
	report := Compare(manifest, baseline, candidate)
	if !report.Reportable || report.Accepted {
		t.Fatalf("case regression should be a reportable rejection: %+v", report)
	}
	comparison := report.Comparisons[0]
	if len(comparison.Cases) != 4 {
		t.Fatalf("case comparisons = %d, want 4", len(comparison.Cases))
	}
	failed := 0
	for _, caseComparison := range comparison.Cases {
		if caseComparison.Condition == "stable" && !caseComparison.Accepted {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("stable case failures = %d, comparisons %+v", failed, comparison.Cases)
	}
	assertSuiteGate(t, comparison, pathIdentity(
		pathIdentity("synthetic", "case", "stable", "case-1"), "pass", "non_inferiority"), false)
}

func TestRequiredPartitionPolicyAndMeasurementAreStructural(t *testing.T) {
	t.Run("condition policy", func(t *testing.T) {
		manifest, baseline, candidate := partitionedConditionFixture()
		manifest.Suites[0].Populations[0].Policy = nil
		report := Compare(manifest, baseline, candidate)
		if report.Reportable || !hasFinding(report, "manifest.condition_policy_missing") {
			t.Fatalf("required condition policy was not structural: %+v", report.Refusals)
		}
	})

	t.Run("case policy", func(t *testing.T) {
		manifest, baseline, candidate := partitionedConditionFixture()
		manifest.Suites[0].Cases[0].RequirePolicy = true
		report := Compare(manifest, baseline, candidate)
		if report.Reportable || !hasFinding(report, "manifest.case_policy_missing") {
			t.Fatalf("required case policy was not structural: %+v", report.Refusals)
		}
	})

	t.Run("partition-only latency", func(t *testing.T) {
		manifest, baseline, candidate := partitionedConditionFixture()
		suite := &manifest.Suites[0]
		suite.Policy.Latencies = append(suite.Policy.Latencies, LatencyPolicy{
			Name: "partition_latency_ms", Unit: "ms",
		})
		for index := range suite.Populations {
			policy := suite.Populations[index].Policy
			policy.Latencies = append(policy.Latencies, LatencyPolicy{
				Name: "partition_latency_ms", Unit: "ms", Required: true,
				Gate: &LatencyGate{MinimumAttempts: 4, MinimumCases: 2,
					Limits: []LatencyLimit{
						{Statistic: StatisticP50, MaximumIncrease: 1},
						{Statistic: StatisticP95, MaximumIncrease: 1},
					}},
			})
		}
		report := Compare(manifest, baseline, candidate)
		if report.Reportable || !hasFinding(report, "attempt.latency_missing") {
			t.Fatalf("required partition measurement was not structural: %+v", report.Refusals)
		}
	})
}

func TestPartitionPoliciesAreCanonicalAndSnapshotted(t *testing.T) {
	manifest, baseline, candidate := partitionedConditionFixture()
	report := Compare(manifest, baseline, candidate)
	if !report.Reportable {
		t.Fatalf("fixture refused: %+v", report.Refusals)
	}
	originalManifestID, originalReportID := report.ManifestID, report.ReportID
	slices.Reverse(manifest.Suites[0].Populations)
	for index := range manifest.Suites[0].Populations {
		policy := manifest.Suites[0].Populations[index].Policy
		slices.Reverse(policy.Latencies)
		for latencyIndex := range policy.Latencies {
			if policy.Latencies[latencyIndex].Gate != nil {
				slices.Reverse(policy.Latencies[latencyIndex].Gate.Limits)
			}
		}
	}
	reordered := Compare(manifest, baseline, candidate)
	if reordered.ManifestID != originalManifestID || reordered.ReportID != originalReportID {
		t.Fatalf("partition authoring order changed identity: %s/%s vs %s/%s",
			originalManifestID, originalReportID, reordered.ManifestID, reordered.ReportID)
	}
	manifest.Suites[0].Populations[0].Policy.Pass.Margin = 0.99
	if report.Manifest.Suites[0].Populations[0].Policy.Pass.Margin == 0.99 {
		t.Fatal("caller mutation rewrote the sealed partition policy")
	}
}

func TestPartitionDecisionCannotBeForgedInArchivedReport(t *testing.T) {
	manifest, baseline, candidate := partitionedConditionFixture()
	report := Compare(manifest, baseline, candidate)
	if !report.Reportable || report.Accepted {
		t.Fatalf("fixture must be a reportable partition rejection: %+v", report)
	}
	for index := range report.Comparisons[0].Conditions {
		condition := &report.Comparisons[0].Conditions[index]
		if condition.Condition == "stable" {
			condition.Accepted = true
			condition.Gates[0].Passed = true
		}
	}
	// Even an attacker who recomputes the public content digest cannot replace
	// the comparison derived from the retained manifest and attempt rows.
	report.ReportID = reportDigest(report)
	if err := report.Verify(); err == nil || !strings.Contains(err.Error(), "derived comparison") {
		t.Fatalf("forged partition decision verified: %v", err)
	}
}

func TestPartitionSchemaDoesNotSilentlyInterpretVersionOneArtifacts(t *testing.T) {
	manifest, baseline, candidate := partitionedConditionFixture()
	manifest.Version = 1
	refused := Compare(manifest, baseline, candidate)
	if refused.Reportable || !hasFinding(refused, "manifest.version") {
		t.Fatalf("version-one manifest entered partition comparison: %+v", refused.Refusals)
	}

	manifest.Version = ManifestVersion
	report := Compare(manifest, baseline, candidate)
	report.Version = 1
	report.ReportID = reportDigest(report)
	if err := report.Verify(); err == nil || !strings.Contains(err.Error(), "version must be 2") {
		t.Fatalf("version-one report was silently reinterpreted: %v", err)
	}
}

func TestCompareProducesCompleteDeterministicPairedEvidence(t *testing.T) {
	manifest := testManifest()
	baseline, candidate := testAttempts()
	report := Compare(manifest, baseline, candidate)
	if !report.Reportable || !report.Accepted {
		t.Fatalf("valid comparison was refused: %+v", report.Refusals)
	}
	if len(report.Attempts) != 12 || len(report.Matched) != 6 {
		t.Fatalf("attempt retention = %d, matched = %d", len(report.Attempts), len(report.Matched))
	}
	if len(report.Comparisons) != 1 {
		t.Fatalf("comparisons = %d", len(report.Comparisons))
	}
	comparison := report.Comparisons[0]
	wantTransitions := TransitionCounts{
		SuccessToSuccess: 3, SuccessToFailure: 1,
		FailureToSuccess: 1, FailureToFailure: 1,
	}
	if comparison.Pass.Transitions != wantTransitions {
		t.Fatalf("pass transitions = %+v, want %+v", comparison.Pass.Transitions, wantTransitions)
	}
	if comparison.Pass.ApplicableAttempts != 6 || comparison.Pass.ApplicableCases != 3 {
		t.Fatalf("pass population = %+v", comparison.Pass)
	}
	if len(comparison.Conditions) != 1 || comparison.Conditions[0].Condition != "regular" ||
		comparison.Conditions[0].Pass.Transitions != wantTransitions {
		t.Fatalf("condition breakdown = %+v", comparison.Conditions)
	}
	if comparison.Pass.LowerBound == nil || comparison.Pass.LowerBound.Direction != "lower" ||
		comparison.Pass.LowerBound.Method != bootstrapMethod {
		t.Fatalf("pass confidence evidence = %+v", comparison.Pass.LowerBound)
	}
	latency := comparison.Latencies[0]
	if latency.Baseline.P50 != 102.5 || latency.Candidate.P50 != 112.5 ||
		latency.PairedDifference.P95 != 10 {
		t.Fatalf("latency distributions = %+v", latency)
	}
	for _, limit := range latency.Limits {
		if !limit.Gate.Passed || limit.UpperBound == nil || limit.UpperBound.Value != 10 {
			t.Fatalf("latency limit = %+v", limit)
		}
	}
	if err := report.Verify(); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// Input order and authoring order are not hidden experimental factors.
	slices.Reverse(baseline)
	slices.Reverse(candidate)
	slices.Reverse(manifest.FixedAxes)
	slices.Reverse(manifest.Treatment)
	slices.Reverse(manifest.Suites[0].Cases)
	reordered := Compare(manifest, baseline, candidate)
	if reordered.ReportID != report.ReportID || reordered.ManifestID != report.ManifestID {
		t.Fatalf("canonical identities changed: %s/%s vs %s/%s",
			report.ManifestID, report.ReportID, reordered.ManifestID, reordered.ReportID)
	}

	// Compare snapshots the input; later caller mutation cannot rewrite evidence.
	baseline[0].Axes[0].Value = "mutated"
	if err := report.Verify(); err != nil {
		t.Fatalf("input mutation changed the sealed report: %v", err)
	}
}

func TestComparisonPermitsOnlyDeclaredTreatmentChanges(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Attempt)
		code string
	}{
		{
			name: "fixed axis changed",
			edit: func(attempt *Attempt) { setAxis(attempt, "machine", "another-host") },
			code: "attempt.axis_mismatch",
		},
		{
			name: "undeclared treatment value",
			edit: func(attempt *Attempt) { setAxis(attempt, "executable_sha256", "third-binary") },
			code: "attempt.axis_mismatch",
		},
		{
			name: "missing provenance axis",
			edit: func(attempt *Attempt) { attempt.Axes = attempt.Axes[1:] },
			code: "attempt.axis_missing",
		},
		{
			name: "unexpected provenance axis",
			edit: func(attempt *Attempt) {
				attempt.Axes = append(attempt.Axes, Axis{Name: "hidden_model_alias", Value: "latest"})
			},
			code: "attempt.axis_unexpected",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseline, candidate := testAttempts()
			test.edit(&candidate[0])
			report := Compare(testManifest(), baseline, candidate)
			if report.Reportable || !hasFinding(report, test.code) {
				t.Fatalf("undeclared change was not refused: %+v", report.Refusals)
			}
			if len(report.Attempts) != 12 {
				t.Fatalf("refusal discarded attempts: %d", len(report.Attempts))
			}
			if err := report.Verify(); err != nil {
				t.Fatalf("refusal artifact did not verify: %v", err)
			}
		})
	}
}

func TestComparisonRejectsMissingDuplicateUnexpectedAndIncompleteAttempts(t *testing.T) {
	tests := []struct {
		name         string
		edit         func(*[]Attempt, *[]Attempt)
		codes        []string
		wantAttempts int
	}{
		{
			name:  "missing",
			edit:  func(_ *[]Attempt, candidate *[]Attempt) { *candidate = (*candidate)[:5] },
			codes: []string{"attempt.missing"}, wantAttempts: 11,
		},
		{
			name: "duplicate retry retained",
			edit: func(_ *[]Attempt, candidate *[]Attempt) {
				retry := (*candidate)[0]
				retry.ID = "candidate-retry-that-cannot-replace-the-failure"
				*candidate = append(*candidate, retry)
			},
			codes: []string{"attempt.duplicate"}, wantAttempts: 13,
		},
		{
			name: "unexpected",
			edit: func(_ *[]Attempt, candidate *[]Attempt) {
				extra := (*candidate)[0]
				extra.ID = "candidate-unexpected"
				extra.Key.Case = "case-not-in-manifest"
				*candidate = append(*candidate, extra)
			},
			codes: []string{"attempt.unexpected"}, wantAttempts: 13,
		},
		{
			name: "incomplete failure retained",
			edit: func(_ *[]Attempt, candidate *[]Attempt) {
				(*candidate)[0].Completed = false
				(*candidate)[0].Passed = false
				(*candidate)[0].Error = "provider timeout"
			},
			codes: []string{"attempt.incomplete"}, wantAttempts: 12,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseline, candidate := testAttempts()
			test.edit(&baseline, &candidate)
			report := Compare(testManifest(), baseline, candidate)
			if report.Reportable || report.Accepted || len(report.Matched) != 0 ||
				len(report.Comparisons) != 0 {
				t.Fatalf("invalid population produced a selected-subset result: %+v", report)
			}
			if len(report.Attempts) != test.wantAttempts {
				t.Fatalf("retained attempts = %d, want %d", len(report.Attempts), test.wantAttempts)
			}
			for _, code := range test.codes {
				if !hasFinding(report, code) {
					t.Fatalf("missing finding %q in %+v", code, report.Refusals)
				}
			}
			if err := report.Verify(); err != nil {
				t.Fatalf("verify refusal: %v", err)
			}
		})
	}
}

func TestBehavioralAndLatencyGatesRejectRegressionsWithoutRejectingEvidence(t *testing.T) {
	t.Run("pass non-inferiority", func(t *testing.T) {
		manifest := testManifest()
		manifest.Suites[0].Policy.Pass.Margin = 0
		baseline, candidate := testAttempts()
		for index := range baseline {
			baseline[index].Passed = true
			candidate[index].Passed = false
		}
		report := Compare(manifest, baseline, candidate)
		assertReportableRejection(t, report, "synthetic/pass/non_inferiority")
		pass := report.Comparisons[0].Pass
		if pass.Transitions.SuccessToFailure != 6 || pass.LowerBound == nil ||
			pass.LowerBound.Value != -1 {
			t.Fatalf("pass regression evidence = %+v", pass)
		}
	})

	t.Run("safety zero tolerance", func(t *testing.T) {
		baseline, candidate := testAttempts()
		candidate[0].Outcomes.Safety = OutcomeFailed
		report := Compare(testManifest(), baseline, candidate)
		assertReportableRejection(t, report, "synthetic/safety/zero_tolerance")
		safety := report.Comparisons[0].Safety
		if safety.CandidateViolations != 1 || safety.Gate.Passed {
			t.Fatalf("safety regression evidence = %+v", safety)
		}
	})

	t.Run("p50 and tail latency", func(t *testing.T) {
		baseline, candidate := testAttempts()
		for index := range candidate {
			candidate[index].Latencies[0].Value += 100
		}
		report := Compare(testManifest(), baseline, candidate)
		if !report.Reportable || report.Accepted {
			t.Fatalf("latency regression acceptance = reportable %v accepted %v refusals %+v",
				report.Reportable, report.Accepted, report.Refusals)
		}
		latency := report.Comparisons[0].Latencies[0]
		if len(latency.Limits) != 2 {
			t.Fatalf("latency limits = %d", len(latency.Limits))
		}
		for _, limit := range latency.Limits {
			if limit.Gate.Passed || limit.UpperBound == nil || limit.UpperBound.Value != 110 {
				t.Fatalf("tail regression = %+v", limit)
			}
		}
	})

	t.Run("absolute pass floor is additional to paired non-inferiority", func(t *testing.T) {
		baseline, candidate := testAttempts()
		for index := range baseline {
			baseline[index].Passed = true
			candidate[index].Passed = true
		}
		candidate[0].Passed = false
		candidate[2].Passed = false
		report := Compare(testManifest(), baseline, candidate)
		assertReportableRejection(t, report, "synthetic/pass/absolute_sanity_floor")
		comparison := report.Comparisons[0]
		if comparison.Pass.Gate == nil || !comparison.Pass.Gate.Passed {
			t.Fatalf("paired non-inferiority did not remain authoritative: %+v", comparison.Pass)
		}
		if !comparison.PassFloor.Evaluated || comparison.PassFloor.Passed {
			t.Fatalf("absolute sanity floor = %+v", comparison.PassFloor)
		}
	})
}

func TestObservationApplicabilityAndMetricSchemaAreStrict(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Attempt)
		code string
	}{
		{
			name: "outcome applicability changed",
			edit: func(attempt *Attempt) { attempt.Outcomes.Deadline = OutcomeNotApplicable },
			code: "attempt.deadline_missing",
		},
		{
			name: "required latency missing",
			edit: func(attempt *Attempt) { attempt.Latencies = nil },
			code: "attempt.latency_missing",
		},
		{
			name: "unexpected latency",
			edit: func(attempt *Attempt) {
				attempt.Latencies = append(attempt.Latencies, Latency{Name: "unplanned_ms", Unit: "ms", Value: 1})
			},
			code: "attempt.latency_unexpected",
		},
		{
			name: "missing evidence",
			edit: func(attempt *Attempt) { attempt.Evidence = nil },
			code: "attempt.evidence_missing",
		},
		{
			name: "invalid evidence digest",
			edit: func(attempt *Attempt) { attempt.Evidence[0].SHA256 = "not-a-digest" },
			code: "attempt.evidence_digest",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseline, candidate := testAttempts()
			test.edit(&candidate[0])
			report := Compare(testManifest(), baseline, candidate)
			if report.Reportable || !hasFinding(report, test.code) {
				t.Fatalf("schema error was accepted: %+v", report.Refusals)
			}
		})
	}
}

func TestIncompleteManifestCannotShrinkTheStudyPopulation(t *testing.T) {
	manifest := testManifest()
	manifest.Suites[0].ExpectedCases = 4
	manifest.Suites[0].ExpectedAttempts = 8
	baseline, candidate := testAttempts()
	report := Compare(manifest, baseline, candidate)
	if report.Reportable || !hasFinding(report, "manifest.case_count") ||
		!hasFinding(report, "manifest.population_total") {
		t.Fatalf("incomplete manifest was accepted: %+v", report.Refusals)
	}
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "listed 3 cases") {
		t.Fatalf("manifest validation did not explain incompleteness: %v", err)
	}
}

func TestReportJSONIsStrictAndTamperEvident(t *testing.T) {
	baseline, candidate := testAttempts()
	report := Compare(testManifest(), baseline, candidate)
	payload, err := report.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	decoded, err := Decode(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.ReportID != report.ReportID {
		t.Fatalf("decoded report ID = %q, want %q", decoded.ReportID, report.ReportID)
	}
	if _, err := Decode(nil); err == nil || !strings.Contains(err.Error(), "reader is nil") {
		t.Fatalf("nil report reader was accepted: %v", err)
	}

	var generic map[string]any
	if err := json.Unmarshal(payload, &generic); err != nil {
		t.Fatal(err)
	}
	generic["unknown_field"] = true
	withUnknown, _ := json.Marshal(generic)
	if _, err := Decode(bytes.NewReader(withUnknown)); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown JSON field was accepted: %v", err)
	}

	tampered := report
	tampered.Accepted = !tampered.Accepted
	if err := tampered.Verify(); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered report verified: %v", err)
	}

	path := filepath.Join(t.TempDir(), "results", "full-1.json")
	if err := report.Write(path); err != nil {
		t.Fatalf("write: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(written, payload) {
		t.Fatal("written artifact bytes differ from canonical JSON")
	}
	if report.ArtifactSHA256() == "" {
		t.Fatal("artifact digest is empty")
	}
	if err := report.Write(path); err == nil {
		t.Fatal("immutable report path was overwritten")
	}
}

func TestManifestRequiresBehavioralAndLatencyGates(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Manifest)
		code string
	}{
		{
			name: "acceptance basis",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.AcceptanceBasis = EvidenceRef{}
			},
			code: "manifest.acceptance_basis_digest",
		},
		{
			name: "per-attempt evidence policy",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.RequireEvidence = false
			},
			code: "manifest.evidence_required",
		},
		{
			name: "per-attempt evidence kinds",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.RequiredEvidenceKinds = nil
			},
			code: "manifest.evidence_kinds_empty",
		},
		{
			name: "suite repetition floor",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].MinimumRepetitions = 3
			},
			code: "manifest.repetition_minimum",
		},
		{
			name: "interaction gate",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Interaction.NonInferiority = nil
			},
			code: "manifest.outcome_gate_missing",
		},
		{
			name: "deadline gate",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Deadline.NonInferiority = nil
			},
			code: "manifest.outcome_gate_missing",
		},
		{
			name: "latency population",
			edit: func(manifest *Manifest) { manifest.Suites[0].Policy.Latencies = nil },
			code: "manifest.latencies_empty",
		},
		{
			name: "required latency",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Latencies[0].Required = false
			},
			code: "manifest.required_latency_empty",
		},
		{
			name: "latency p50 gate",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Latencies[0].Gate.Limits = []LatencyLimit{{
					Statistic: StatisticP95, MaximumIncrease: 20,
				}}
			},
			code: "manifest.latency_p50_gate_missing",
		},
		{
			name: "latency tail gate",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Latencies[0].Gate.Limits = []LatencyLimit{{
					Statistic: StatisticP50, MaximumIncrease: 20,
				}}
			},
			code: "manifest.latency_tail_gate_missing",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := testManifest()
			test.edit(&manifest)
			baseline, candidate := testAttempts()
			report := Compare(manifest, baseline, candidate)
			if report.Reportable || !hasFinding(report, test.code) {
				t.Fatalf("missing required gate was accepted: %+v", report.Refusals)
			}
		})
	}
}

func assertReportableRejection(t *testing.T, report Report, gateName string) {
	t.Helper()
	if !report.Reportable || report.Accepted || len(report.Refusals) != 0 {
		t.Fatalf("behavioral rejection should remain reportable: %+v", report)
	}
	for _, gate := range report.Comparisons[0].Gates {
		if gate.Name == gateName {
			if gate.Passed {
				t.Fatalf("gate %q passed unexpectedly", gateName)
			}
			return
		}
	}
	t.Fatalf("gate %q not found in %+v", gateName, report.Comparisons[0].Gates)
}

func assertSuiteGate(t *testing.T, comparison SuiteComparison, name string, passed bool) {
	t.Helper()
	for _, gate := range comparison.Gates {
		if gate.Name == name {
			if gate.Passed != passed {
				t.Fatalf("gate %q passed = %v, want %v: %+v", name, gate.Passed, passed, gate)
			}
			return
		}
	}
	t.Fatalf("gate %q not found in %+v", name, comparison.Gates)
}

func hasFinding(report Report, code string) bool {
	for _, finding := range report.Refusals {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func setAxis(attempt *Attempt, name, value string) {
	for index := range attempt.Axes {
		if attempt.Axes[index].Name == name {
			attempt.Axes[index].Value = value
			return
		}
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}
