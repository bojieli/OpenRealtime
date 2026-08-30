package migration

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
)

func TestSuccessfulFullPredecessorFreezesPopulationAndPolicy(t *testing.T) {
	baseline, candidate := testAttempts()
	predecessor := Compare(testManifest(), baseline, candidate)
	if !predecessor.Accepted {
		t.Fatalf("accepted predecessor fixture failed: %+v", predecessor)
	}

	manifest := testManifest()
	manifest.Campaign.RunID = "full-shrunk-after-success"
	manifest.Campaign.Predecessors = []ReportReference{mustReference(t, predecessor)}
	suite := &manifest.Suites[0]
	suite.Cases = suite.Cases[:2]
	suite.ExpectedCases = 2
	suite.ExpectedAttempts = 4
	suite.Populations[0].ExpectedCases = 2
	suite.Populations[0].ExpectedAttempts = 4
	suite.Policy.Pass.MinimumAttempts, suite.Policy.Pass.MinimumCases = 4, 2
	suite.Policy.Interaction.NonInferiority.MinimumAttempts = 4
	suite.Policy.Interaction.NonInferiority.MinimumCases = 2
	suite.Policy.Deadline.NonInferiority.MinimumAttempts = 4
	suite.Policy.Deadline.NonInferiority.MinimumCases = 2
	suite.Policy.Latencies[0].Gate.MinimumAttempts = 4
	suite.Policy.Latencies[0].Gate.MinimumCases = 2

	report := CompareWithHistory(manifest, baseline[:4], candidate[:4], []Report{predecessor})
	if report.Reportable || !hasFinding(report, "lineage.full_plan_changed") {
		t.Fatalf("successful full plan was silently shrunk: %+v", report.Refusals)
	}
}

func TestCampaignCannotRetuneControlAxesOrBaselineTreatment(t *testing.T) {
	baseline, candidate := testAttempts()
	predecessor := Compare(testManifest(), baseline, candidate)
	tests := []struct {
		name string
		code string
		edit func(*Manifest, []Attempt, []Attempt)
	}{
		{
			name: "fixed axis", code: "lineage.fixed_axes_changed",
			edit: func(manifest *Manifest, baseline, candidate []Attempt) {
				manifest.FixedAxes[1].Value = "reserved-host-b"
				for index := range baseline {
					setAxis(&baseline[index], "machine", "reserved-host-b")
					setAxis(&candidate[index], "machine", "reserved-host-b")
				}
			},
		},
		{
			name: "baseline treatment", code: "lineage.treatment_contract_changed",
			edit: func(manifest *Manifest, baseline, _ []Attempt) {
				manifest.Treatment[0].Baseline = "retuned-legacy-binary"
				for index := range baseline {
					setAxis(&baseline[index], "executable_sha256", "retuned-legacy-binary")
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := testManifest()
			manifest.Campaign.RunID = "full-retuned-" + strings.ReplaceAll(test.name, " ", "-")
			manifest.Campaign.Predecessors = []ReportReference{mustReference(t, predecessor)}
			left, right := slices.Clone(baseline), slices.Clone(candidate)
			test.edit(&manifest, left, right)
			report := CompareWithHistory(manifest, left, right, []Report{predecessor})
			if report.Reportable || !hasFinding(report, test.code) {
				t.Fatalf("campaign retune was accepted: %+v", report.Refusals)
			}
		})
	}
}

func TestLocalVerificationDoesNotSkipDerivedFieldsForLineageRefusal(t *testing.T) {
	manifest := testManifest()
	manifest.Campaign.RunID = "full-missing-predecessor"
	manifest.Campaign.Predecessors = []ReportReference{{
		RunID: "missing", ReportID: strings.Repeat("a", 64),
		ArtifactSHA256: strings.Repeat("b", 64),
	}}
	baseline, candidate := testAttempts()
	report := CompareWithHistory(manifest, baseline, candidate, nil)
	if !hasFinding(report, "lineage.predecessor_missing") {
		t.Fatalf("fixture has no lineage refusal: %+v", report.Refusals)
	}
	report.CampaignGate.Name = "forged/campaign/gate"
	report.ReportID = reportDigest(report)
	if err := report.Verify(); err == nil || !strings.Contains(err.Error(), "derived comparison") {
		t.Fatalf("forged derived state passed local verification: %v", err)
	}
}

func TestSuppliedHistoryReconstructsEveryPredecessorLineageState(t *testing.T) {
	baseline, candidate := testAttempts()
	forged := Compare(testManifest(), baseline, candidate)
	forged.Refusals = []Finding{{
		Code: "lineage.predecessor_missing", Scope: "lineage/history",
		Message: "invented predecessor failure",
	}}
	forged.Reportable = false
	forged.Accepted = false
	forged.Matched = []MatchedAttempt{}
	forged.Comparisons = []SuiteComparison{}
	applyCampaignGate(&forged)
	forged.ReportID = reportDigest(forged)
	// A lineage claim is not self-proving. The unexported local reconstruction
	// remains useful to the lineage validator, but public verification must
	// reconstruct the supplied (empty here) history and reject the invention.
	if err := forged.Verify(); err == nil || !strings.Contains(err.Error(), "campaign history") {
		t.Fatalf("invented external-lineage fixture verified: %v", err)
	}

	manifest := testManifest()
	manifest.Campaign.RunID = "full-after-forged-predecessor"
	manifest.Campaign.Predecessors = []ReportReference{reportReference(forged)}
	manifest.Campaign.DiagnosedFailures = []FailureReference{{
		ReportID: forged.ReportID, FindingCode: "lineage.predecessor_missing",
	}}
	report := CompareWithHistory(manifest, baseline, candidate, []Report{forged})
	if report.Reportable || !hasFinding(report, "lineage.predecessor_state_forged") {
		t.Fatalf("invented predecessor lineage state was accepted: %+v", report.Refusals)
	}
}

func TestDecodeRequiresExactCanonicalArtifactBytes(t *testing.T) {
	baseline, candidate := testAttempts()
	report := Compare(testManifest(), baseline, candidate)
	compact, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(bytes.NewReader(compact)); err == nil ||
		!strings.Contains(err.Error(), "canonical archived JSON") {
		t.Fatalf("reformatted artifact impersonated canonical bytes: %v", err)
	}
}

func TestControlCharactersCannotCollideClusterOrEvidenceIdentities(t *testing.T) {
	manifest := testManifest()
	manifest.Suites[0].Cases[0].Condition = "regular\x00case"
	manifest.Suites[0].Populations[0].Condition = "regular\x00case"
	baseline, candidate := testAttempts()
	for index := 0; index < 2; index++ {
		baseline[index].Key.Condition = "regular\x00case"
		candidate[index].Key.Condition = "regular\x00case"
	}
	report := Compare(manifest, baseline, candidate)
	if report.Reportable || !hasFinding(report, "manifest.condition") {
		t.Fatalf("control-character identity entered cluster keys: %+v", report.Refusals)
	}
}

func TestBootstrapPolicyRejectsPseudoConfidenceAndUnboundedWork(t *testing.T) {
	tests := []struct {
		name string
		code string
		edit func(*Manifest)
	}{
		{
			name: "one cluster", code: "manifest.rate_clusters",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Pass.MinimumCases = 1
			},
		},
		{
			name: "too many resamples", code: "manifest.resamples_limit",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Inference.Resamples = maxBootstrapResamples + 1
			},
		},
		{
			name: "unresolved confidence tail", code: "manifest.bootstrap_tail_resolution",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Inference.Confidence = 0.999
			},
		},
		{
			name: "work budget", code: "manifest.bootstrap_work_limit",
			edit: func(manifest *Manifest) {
				suite := &manifest.Suites[0]
				suite.ExpectedCases = maxCasesPerSuite
				suite.ExpectedAttempts = maxAttemptsPerSuite
				suite.Policy.Inference.Resamples = maxBootstrapResamples
			},
		},
		{
			name: "aggregate work budget", code: "manifest.bootstrap_work_limit",
			edit: func(manifest *Manifest) {
				first := &manifest.Suites[0]
				first.ExpectedCases = maxCasesPerSuite
				first.ExpectedAttempts = maxCasesPerSuite
				second := canonicalSuite(*first)
				second.Name = "synthetic-second"
				manifest.Suites = append(manifest.Suites, second)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := testManifest()
			test.edit(&manifest)
			baseline, candidate := testAttempts()
			report := Compare(manifest, baseline, candidate)
			if report.Reportable || !hasFinding(report, test.code) {
				t.Fatalf("unsafe bootstrap policy was accepted: %+v", report.Refusals)
			}
		})
	}
}

func TestFiniteExtremeLatenciesDoNotOverflowDerivedEvidence(t *testing.T) {
	baseline, candidate := testAttempts()
	for index := range baseline {
		if index%2 == 0 {
			baseline[index].Latencies[0].Value = math.MaxFloat64
			candidate[index].Latencies[0].Value = 0
		} else {
			baseline[index].Latencies[0].Value = 0
			candidate[index].Latencies[0].Value = math.MaxFloat64
		}
	}
	report := Compare(testManifest(), baseline, candidate)
	if !report.Reportable {
		t.Fatalf("finite extreme readings became structurally invalid: %+v", report.Refusals)
	}
	differences := report.Comparisons[0].Latencies[0].PairedDifference
	if !finite(differences.Mean) || !finite(report.Comparisons[0].Latencies[0].Baseline.Mean) ||
		!finite(report.Comparisons[0].Latencies[0].Candidate.Mean) {
		t.Fatalf("finite inputs overflowed distributions: %+v", report.Comparisons[0].Latencies[0])
	}
	if err := report.Verify(); err != nil {
		t.Fatalf("extreme finite report did not seal: %v", err)
	}
}

func TestNonFiniteAndNegativeLatenciesAreRefused(t *testing.T) {
	tests := []struct {
		name  string
		value float64
		code  string
	}{
		{name: "nan", value: math.NaN(), code: "attempt.latency_non_finite"},
		{name: "positive infinity", value: math.Inf(1), code: "attempt.latency_non_finite"},
		{name: "negative", value: -1, code: "attempt.latency_negative"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseline, candidate := testAttempts()
			candidate[0].Latencies[0].Value = test.value
			report := Compare(testManifest(), baseline, candidate)
			if report.Reportable || !hasFinding(report, test.code) {
				t.Fatalf("invalid latency was accepted: %+v", report.Refusals)
			}
			if test.code == "attempt.latency_non_finite" {
				if err := report.Verify(); err != nil {
					t.Fatalf("non-finite refusal was not sealed: %v", err)
				}
				payload, err := report.Marshal()
				if err != nil {
					t.Fatalf("non-finite refusal was not archivable: %v", err)
				}
				if _, err := Decode(bytes.NewReader(payload)); err != nil {
					t.Fatalf("non-finite refusal did not round trip: %v", err)
				}
			}
		})
	}
}

func TestNonFiniteManifestDecisionsProduceSealedRefusals(t *testing.T) {
	tests := []struct {
		name string
		code string
		edit func(*Manifest)
	}{
		{
			name: "confidence", code: "manifest.confidence",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Inference.Confidence = math.NaN()
			},
		},
		{
			name: "rate margin", code: "manifest.rate_margin",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Pass.Margin = math.Inf(1)
			},
		},
		{
			name: "latency limit", code: "manifest.latency_limit",
			edit: func(manifest *Manifest) {
				manifest.Suites[0].Policy.Latencies[0].Gate.Limits[0].MaximumIncrease = math.NaN()
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := testManifest()
			test.edit(&manifest)
			baseline, candidate := testAttempts()
			report := Compare(manifest, baseline, candidate)
			if report.Reportable || report.ReportID == "" || !hasFinding(report, test.code) {
				t.Fatalf("non-finite manifest decision was not sealed: %+v", report)
			}
			if err := report.Verify(); err != nil {
				t.Fatalf("verify non-finite manifest refusal: %v", err)
			}
			payload, err := report.Marshal()
			if err != nil {
				t.Fatalf("marshal non-finite manifest refusal: %v", err)
			}
			if _, err := Decode(bytes.NewReader(payload)); err != nil {
				t.Fatalf("decode non-finite manifest refusal: %v", err)
			}
		})
	}
}

func TestAttemptErrorMustRemainCanonicalAndBounded(t *testing.T) {
	tests := []struct {
		name  string
		error string
		code  string
	}{
		{name: "invalid utf8", error: string([]byte{0xff}), code: "attempt.error_utf8"},
		{name: "nul", error: "provider\x00timeout", code: "attempt.error_control"},
		{name: "oversized", error: strings.Repeat("x", maxAttemptErrorBytes+1), code: "attempt.error_limit"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			baseline, candidate := testAttempts()
			candidate[0].Completed = false
			candidate[0].Passed = false
			candidate[0].Error = test.error
			report := Compare(testManifest(), baseline, candidate)
			if report.Reportable || !hasFinding(report, test.code) {
				t.Fatalf("noncanonical error was accepted: %+v", report.Refusals)
			}
			if test.name == "invalid utf8" {
				if _, err := report.Marshal(); err == nil ||
					!strings.Contains(err.Error(), "losslessly") {
					t.Fatalf("lossy invalid-UTF-8 artifact was emitted: %v", err)
				}
			}
		})
	}
}

func TestLogicalGatePathsEscapeUserIdentifiersInjectively(t *testing.T) {
	gateName := func(suiteName string) string {
		manifest := testManifest()
		manifest.Suites[0].Name = suiteName
		baseline, candidate := testAttempts()
		for index := range baseline {
			baseline[index].Key.Suite = suiteName
			candidate[index].Key.Suite = suiteName
		}
		report := Compare(manifest, baseline, candidate)
		if !report.Reportable {
			t.Fatalf("escaped suite %q was refused: %+v", suiteName, report.Refusals)
		}
		return report.Comparisons[0].Pass.Gate.Name
	}
	withSlash := gateName("suite/partition")
	withEscapeText := gateName("suite%2Fpartition")
	if withSlash == withEscapeText || !strings.Contains(withSlash, "%2F") ||
		!strings.Contains(withEscapeText, "%252F") {
		t.Fatalf("logical paths collided: %q and %q", withSlash, withEscapeText)
	}
}

func TestCampaignHistoryHasHardReportBound(t *testing.T) {
	manifest := testManifest()
	manifest.Campaign.RunID = "history-over-limit"
	manifest.Campaign.Predecessors = make([]ReportReference, maxCampaignPredecessors+1)
	for index := range manifest.Campaign.Predecessors {
		manifest.Campaign.Predecessors[index] = ReportReference{
			RunID: "run-" + itoa(index), ReportID: strings.Repeat("a", 64),
			ArtifactSHA256: strings.Repeat("b", 64),
		}
	}
	baseline, candidate := testAttempts()
	report := CompareWithHistory(manifest, baseline, candidate, nil)
	if report.Reportable || !hasFinding(report, "campaign.predecessor_limit") ||
		!hasFinding(report, "lineage.predecessor_limit") {
		t.Fatalf("oversized campaign history was accepted: %+v", report.Refusals)
	}
}

func TestCampaignHistoryHasAggregateVerificationWorkBound(t *testing.T) {
	const retainedReports = 7
	history := make([]Report, 0, retainedReports)
	for index := 0; index < retainedReports; index++ {
		manifest := testManifest()
		manifest.Campaign.RunID = "expensive-history-" + itoa(index)
		manifest.Suites[0].ExpectedCases = maxCasesPerSuite
		manifest.Suites[0].ExpectedAttempts = maxCasesPerSuite
		history = append(history, Compare(manifest, nil, nil))
	}

	manifest := testManifest()
	manifest.Campaign.RunID = "history-work-over-limit"
	manifest.Campaign.Predecessors = make([]ReportReference, len(history))
	for index, report := range history {
		manifest.Campaign.Predecessors[index] = mustReference(t, report)
	}
	baseline, candidate := testAttempts()
	compared := CompareWithHistory(manifest, baseline, candidate, history)
	if compared.Reportable || !hasFinding(compared, "lineage.bootstrap_work_limit") {
		t.Fatalf("aggregate retained-history work was accepted: %+v", compared.Refusals)
	}
}

func TestInvalidHistoryRefusalIsIndependentOfSuppliedOrder(t *testing.T) {
	baseline, candidate := testAttempts()
	firstManifest := testManifest()
	firstManifest.Campaign.RunID = "historical-a"
	first := Compare(firstManifest, baseline, candidate)
	secondManifest := testManifest()
	secondManifest.Campaign.RunID = "historical-b"
	second := Compare(secondManifest, baseline, candidate)

	current := testManifest()
	current.Campaign.RunID = "current-with-undeclared-history"
	forward := CompareWithHistory(current, baseline, candidate, []Report{first, second})
	reverse := CompareWithHistory(current, baseline, candidate, []Report{second, first})
	if forward.ReportID != reverse.ReportID || canonicalJSON(forward) != canonicalJSON(reverse) {
		t.Fatalf("history order changed sealed refusal:\nforward %s\nreverse %s",
			forward.ReportID, reverse.ReportID)
	}
}

func TestBootstrapBoundsUseConservativeOrderStatistics(t *testing.T) {
	values := make([]float64, 1000)
	for index := range values {
		values[index] = float64(index)
	}
	if got := conservativeLowerQuantile(values, 0.05); got != 49 {
		t.Fatalf("lower order statistic = %v, want 49", got)
	}
	if got := conservativeUpperQuantile(values, 0.95); got != 950 {
		t.Fatalf("upper order statistic = %v, want 950", got)
	}
}

func TestLatencyClusterBootstrapMatchesNaiveResampling(t *testing.T) {
	observations := []latencyObservation{
		{cluster: "a", baseline: 10, candidate: 12},
		{cluster: "a", baseline: 20, candidate: 19},
		{cluster: "b", baseline: 30, candidate: 35},
		{cluster: "c", baseline: 40, candidate: 38},
		{cluster: "c", baseline: 50, candidate: 55},
		{cluster: "c", baseline: 60, candidate: 59},
	}
	policy := BootstrapPolicy{Confidence: 0.95, Resamples: 1000, Seed: 71}
	statistics := []LatencyStatistic{StatisticMean, StatisticP50, StatisticP95}
	got := latencyUpperBounds(observations, statistics, policy, "unequal-clusters")
	want := naiveLatencyBounds(observations, statistics, policy, "unequal-clusters")
	for _, name := range statistics {
		if got[name] != want[name] {
			t.Fatalf("%s optimized bound = %+v, naive %+v", name, got[name], want[name])
		}
	}
}

func naiveLatencyBounds(
	observations []latencyObservation, statistics []LatencyStatistic,
	policy BootstrapPolicy, label string,
) map[LatencyStatistic]ConfidenceBound {
	clusters := groupLatency(observations)
	seed := derivedSeed(policy.Seed, label)
	random := splitMix64(seed)
	readings := make(map[LatencyStatistic][]float64, len(statistics))
	for _, name := range statistics {
		readings[name] = make([]float64, policy.Resamples)
	}
	for sample := 0; sample < policy.Resamples; sample++ {
		var baseline, candidate []float64
		for range clusters {
			cluster := clusters[random.index(len(clusters))]
			baseline = append(baseline, cluster.baseline...)
			candidate = append(candidate, cluster.candidate...)
		}
		for _, name := range statistics {
			readings[name][sample] = statistic(candidate, name) - statistic(baseline, name)
		}
	}
	result := make(map[LatencyStatistic]ConfidenceBound, len(statistics))
	for _, name := range statistics {
		values := readings[name]
		slices.Sort(values)
		result[name] = ConfidenceBound{
			Level: policy.Confidence, Direction: "upper",
			Value:  conservativeUpperQuantile(values, policy.Confidence),
			Method: bootstrapMethod, Resamples: policy.Resamples, Seed: seed,
		}
	}
	return result
}

func BenchmarkLatencyUpperBoundsFDBench6147(b *testing.B) {
	observations := make([]latencyObservation, 6147)
	for index := range observations {
		observations[index] = latencyObservation{
			cluster: "case-" + itoa(index), baseline: 100 + float64(index%251),
			candidate: 103 + float64((index*17)%251),
		}
	}
	policy := BootstrapPolicy{Confidence: 0.95, Resamples: 1000, Seed: 91}
	statistics := []LatencyStatistic{StatisticP50, StatisticP95, StatisticP99}
	b.ResetTimer()
	for range b.N {
		latencyUpperBounds(observations, statistics, policy, "fd-bench")
	}
}

func BenchmarkCompareFDBench6147(b *testing.B) {
	const cases = 6147
	manifest := testManifest()
	suite := &manifest.Suites[0]
	suite.Name = "fd-bench"
	suite.ExpectedCases = cases
	suite.ExpectedAttempts = cases
	suite.MinimumRepetitions = 1
	suite.Populations = []Population{{
		Condition: "released", ExpectedCases: cases, ExpectedAttempts: cases,
	}}
	suite.Cases = make([]CaseSpec, cases)
	suite.Policy.Pass.MinimumAttempts = cases
	suite.Policy.Pass.MinimumCases = cases
	suite.Policy.Interaction.NonInferiority.MinimumAttempts = cases
	suite.Policy.Interaction.NonInferiority.MinimumCases = cases
	suite.Policy.Deadline.NonInferiority.MinimumAttempts = cases
	suite.Policy.Deadline.NonInferiority.MinimumCases = cases
	suite.Policy.Latencies[0].Gate.MinimumAttempts = cases
	suite.Policy.Latencies[0].Gate.MinimumCases = cases
	baseline := make([]Attempt, cases)
	candidate := make([]Attempt, cases)
	for index := 0; index < cases; index++ {
		caseID := "conversation-" + itoa(index)
		suite.Cases[index] = CaseSpec{
			Condition: "released", ID: caseID, Repetitions: []string{"trial-1"},
		}
		key := AttemptKey{
			Suite: "fd-bench", Condition: "released", Case: caseID, Repetition: "trial-1",
		}
		baseline[index] = testAttempt("baseline-"+itoa(index), key, ArmBaseline,
			index%11 != 0, 100+float64(index%251))
		candidate[index] = testAttempt("candidate-"+itoa(index), key, ArmCandidate,
			index%11 != 0, 103+float64(index%251))
	}
	report := Compare(manifest, baseline, candidate)
	if !report.Reportable {
		b.Fatalf("benchmark fixture was refused: %+v", report.Refusals)
	}
	payload, err := report.Marshal()
	if err != nil {
		b.Fatalf("benchmark fixture was not archivable: %v", err)
	}
	artifactBytes := len(payload)
	b.ResetTimer()
	for range b.N {
		Compare(manifest, baseline, candidate)
	}
	b.ReportMetric(float64(artifactBytes), "artifact-bytes")
}
