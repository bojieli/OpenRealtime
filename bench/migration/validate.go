package migration

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// These limits are deliberately above the repository's largest declared
// benchmark (FD-Bench currently has 6,147 cases), while keeping a malformed
// or adversarial manifest from turning validation into an unbounded compute
// request. They are schema limits, not benchmark acceptance evidence.
const (
	maxManifestSuites                   = 256
	maxFixedAndTreatmentAxes            = 256
	maxCampaignPredecessors             = 64
	maxDiagnosedFailures                = 16_384
	maxHistoryAttempts                  = 500_000
	maxCasesPerSuite                    = 100_000
	maxAttemptsPerSuite                 = 1_000_000
	maxRepetitionsPerCase               = 100_000
	maxLatenciesPerSuite                = 128
	maxLatencyLimits                    = 16
	maxBootstrapResamples               = 100_000
	maxBootstrapWorkUnits        uint64 = 1_000_000_000
	maxHistoryBootstrapWorkUnits        = 4_000_000_000
	maxCanonicalTextBytes               = 16 << 10
	maxAttemptErrorBytes                = 64 << 10
)

// Validate checks that a manifest is complete and predeclares every decision
// the comparator will make.
func (manifest Manifest) Validate() error {
	findings := validateManifest(canonicalManifest(manifest))
	if len(findings) == 0 {
		return nil
	}
	messages := make([]string, len(findings))
	for index, finding := range findings {
		messages[index] = finding.Scope + ": " + finding.Message
	}
	return errors.New(strings.Join(messages, "; "))
}

func validateManifest(manifest Manifest) []Finding {
	var findings []Finding
	add := func(code, scope, message string) {
		findings = append(findings, Finding{Code: code, Scope: scope, Message: message})
	}
	if manifest.Version != ManifestVersion {
		add("manifest.version", "manifest", fmt.Sprintf(
			"version must be %d, got %d", ManifestVersion, manifest.Version))
	}
	findings = append(findings, validateCampaign(manifest.Campaign)...)
	validateCanonicalText(&findings, "manifest.arm", "manifest/baseline", "arm name", manifest.Baseline.Name)
	validateCanonicalText(&findings, "manifest.arm", "manifest/candidate", "arm name", manifest.Candidate.Name)
	if manifest.Baseline.Name != "" && manifest.Baseline.Name == manifest.Candidate.Name {
		add("manifest.arm_duplicate", "manifest", "baseline and candidate names must differ")
	}
	if len(manifest.FixedAxes) == 0 {
		add("manifest.fixed_axes_empty", "manifest/fixed_axes",
			"at least one fixed provenance axis is required")
	}
	if len(manifest.Treatment) == 0 {
		add("manifest.treatment_empty", "manifest/treatment",
			"at least one explicit treatment delta is required")
	}
	if len(manifest.FixedAxes)+len(manifest.Treatment) > maxFixedAndTreatmentAxes {
		add("manifest.axes_limit", "manifest", fmt.Sprintf(
			"fixed and treatment axes exceed the limit of %d", maxFixedAndTreatmentAxes))
	}

	axes := make(map[string]string, len(manifest.FixedAxes)+len(manifest.Treatment))
	for index, axis := range manifest.FixedAxes {
		scope := fmt.Sprintf("manifest/fixed_axes/%d", index)
		validateCanonicalText(&findings, "manifest.axis_name", scope, "axis name", axis.Name)
		validateCanonicalText(&findings, "manifest.axis_value", scope, "axis value", axis.Value)
		validateNamedDigest(&findings, "manifest.axis_digest", scope, axis.Name, axis.Value)
		if previous, exists := axes[axis.Name]; exists {
			add("manifest.axis_duplicate", scope, fmt.Sprintf(
				"axis %q is already declared by %s", axis.Name, previous))
		} else if axis.Name != "" {
			axes[axis.Name] = scope
		}
	}
	for index, delta := range manifest.Treatment {
		scope := fmt.Sprintf("manifest/treatment/%d", index)
		validateCanonicalText(&findings, "manifest.axis_name", scope, "axis name", delta.Axis)
		validateCanonicalText(&findings, "manifest.axis_value", scope, "baseline value", delta.Baseline)
		validateCanonicalText(&findings, "manifest.axis_value", scope, "candidate value", delta.Candidate)
		validateNamedDigest(&findings, "manifest.axis_digest", scope, delta.Axis, delta.Baseline)
		validateNamedDigest(&findings, "manifest.axis_digest", scope, delta.Axis, delta.Candidate)
		if delta.Baseline != "" && delta.Baseline == delta.Candidate {
			add("manifest.treatment_unchanged", scope, fmt.Sprintf(
				"treatment axis %q has the same value in both arms", delta.Axis))
		}
		if previous, exists := axes[delta.Axis]; exists {
			add("manifest.axis_duplicate", scope, fmt.Sprintf(
				"axis %q is already declared by %s", delta.Axis, previous))
		} else if delta.Axis != "" {
			axes[delta.Axis] = scope
		}
	}

	if len(manifest.Suites) == 0 {
		add("manifest.suites_empty", "manifest/suites", "at least one suite is required")
	}
	if len(manifest.Suites) > maxManifestSuites {
		add("manifest.suite_limit", "manifest/suites", fmt.Sprintf(
			"suite count exceeds the limit of %d", maxManifestSuites))
	}
	seenSuites := make(map[string]bool, len(manifest.Suites))
	for index, suite := range manifest.Suites {
		scope := fmt.Sprintf("manifest/suites/%d", index)
		validateCanonicalText(&findings, "manifest.suite_name", scope, "suite name", suite.Name)
		if seenSuites[suite.Name] {
			add("manifest.suite_duplicate", scope, fmt.Sprintf("suite %q is duplicated", suite.Name))
		}
		seenSuites[suite.Name] = true
		findings = append(findings, validateSuite(suite, scope)...)
	}
	totalBootstrapWork, totalBootstrapWorkOverflow := manifestBootstrapWorkUnits(manifest)
	if totalBootstrapWorkOverflow || totalBootstrapWork > maxBootstrapWorkUnits {
		add("manifest.bootstrap_work_limit", "manifest/suites", fmt.Sprintf(
			"total declared bootstrap work exceeds the limit of %d units", maxBootstrapWorkUnits))
	}
	sortFindings(findings)
	return findings
}

func validateCampaign(campaign CampaignPlan) []Finding {
	var findings []Finding
	const scope = "manifest/campaign"
	validateCanonicalText(&findings, "campaign.id", scope, "campaign ID", campaign.CampaignID)
	validateCanonicalText(&findings, "campaign.run_id", scope, "run ID", campaign.RunID)
	if campaign.Kind != RunFull && campaign.Kind != RunDiagnostic {
		findings = append(findings, Finding{Code: "campaign.kind", Scope: scope,
			Message: fmt.Sprintf("run kind must be %q or %q, got %q", RunFull, RunDiagnostic, campaign.Kind)})
	}
	if len(campaign.Predecessors) > maxCampaignPredecessors {
		findings = append(findings, Finding{Code: "campaign.predecessor_limit", Scope: scope,
			Message: fmt.Sprintf("predecessor count exceeds the limit of %d", maxCampaignPredecessors)})
	}
	if len(campaign.DiagnosedFailures) > maxDiagnosedFailures {
		findings = append(findings, Finding{Code: "campaign.failure_limit", Scope: scope,
			Message: fmt.Sprintf("diagnosed failure count exceeds the limit of %d", maxDiagnosedFailures)})
	}
	if campaign.Kind == RunDiagnostic && len(campaign.Predecessors) == 0 {
		findings = append(findings, Finding{Code: "campaign.diagnostic_predecessor_missing", Scope: scope,
			Message: "a diagnostic run must follow a retained failed campaign report"})
	}
	if campaign.Kind == RunDiagnostic && len(campaign.DiagnosedFailures) == 0 {
		findings = append(findings, Finding{Code: "campaign.diagnostic_target_missing", Scope: scope,
			Message: "a diagnostic run must name at least one predecessor failure"})
	}
	seenRuns := make(map[string]bool, len(campaign.Predecessors))
	seenReports := make(map[string]bool, len(campaign.Predecessors))
	for index, predecessor := range campaign.Predecessors {
		predecessorScope := fmt.Sprintf("%s/predecessors/%d", scope, index)
		validateCanonicalText(&findings, "campaign.predecessor_run", predecessorScope,
			"predecessor run ID", predecessor.RunID)
		if !validSHA256(predecessor.ReportID) {
			findings = append(findings, Finding{Code: "campaign.predecessor_report_digest",
				Scope: predecessorScope, Message: "predecessor report ID must be a SHA-256 digest"})
		}
		if !validSHA256(predecessor.ArtifactSHA256) {
			findings = append(findings, Finding{Code: "campaign.predecessor_artifact_digest",
				Scope: predecessorScope, Message: "predecessor artifact SHA-256 must be valid"})
		}
		if predecessor.RunID == campaign.RunID && campaign.RunID != "" {
			findings = append(findings, Finding{Code: "campaign.lineage_cycle", Scope: predecessorScope,
				Message: "a run cannot name itself as a predecessor"})
		}
		if seenRuns[predecessor.RunID] {
			findings = append(findings, Finding{Code: "campaign.predecessor_run_duplicate",
				Scope: predecessorScope, Message: fmt.Sprintf("predecessor run %q is duplicated", predecessor.RunID)})
		}
		if seenReports[predecessor.ReportID] {
			findings = append(findings, Finding{Code: "campaign.predecessor_report_duplicate",
				Scope: predecessorScope, Message: fmt.Sprintf("predecessor report %q is duplicated", predecessor.ReportID)})
		}
		seenRuns[predecessor.RunID] = true
		seenReports[predecessor.ReportID] = true
	}
	seenFailures := make(map[string]bool, len(campaign.DiagnosedFailures))
	for index, failure := range campaign.DiagnosedFailures {
		failureScope := fmt.Sprintf("%s/diagnosed_failures/%d", scope, index)
		if !validSHA256(failure.ReportID) {
			findings = append(findings, Finding{Code: "campaign.failure_report_digest",
				Scope: failureScope, Message: "failure report ID must be a SHA-256 digest"})
		}
		gateSet := strings.TrimSpace(failure.Gate) != ""
		findingSet := strings.TrimSpace(failure.FindingCode) != ""
		if gateSet == findingSet {
			findings = append(findings, Finding{Code: "campaign.failure_selector", Scope: failureScope,
				Message: "exactly one of gate and finding_code must be set"})
		}
		if gateSet {
			validateCanonicalText(&findings, "campaign.failure_gate", failureScope,
				"failed gate", failure.Gate)
		}
		if findingSet {
			validateCanonicalText(&findings, "campaign.failure_finding", failureScope,
				"finding code", failure.FindingCode)
		}
		if !seenReports[failure.ReportID] {
			findings = append(findings, Finding{Code: "campaign.failure_not_predecessor", Scope: failureScope,
				Message: "diagnosed failure does not name a declared predecessor"})
		}
		identity := failure.ReportID + "\x00" + failure.Gate + "\x00" + failure.FindingCode
		if seenFailures[identity] {
			findings = append(findings, Finding{Code: "campaign.failure_duplicate", Scope: failureScope,
				Message: "diagnosed failure reference is duplicated"})
		}
		seenFailures[identity] = true
	}
	return findings
}

func validateSuite(suite SuiteSpec, scope string) []Finding {
	var findings []Finding
	add := func(code, child, message string) {
		findings = append(findings, Finding{Code: code, Scope: scope + child, Message: message})
	}
	if suite.ExpectedCases <= 0 {
		add("manifest.expected_cases", "", "expected cases must be positive")
	}
	if suite.ExpectedAttempts <= 0 {
		add("manifest.expected_attempts", "", "expected attempts must be positive")
	}
	if suite.MinimumRepetitions <= 0 {
		add("manifest.minimum_repetitions", "", "minimum repetitions must be positive")
	}
	if suite.MinimumRepetitions > maxRepetitionsPerCase {
		add("manifest.repetition_limit", "", fmt.Sprintf(
			"minimum repetitions exceed the per-case limit of %d", maxRepetitionsPerCase))
	}
	if suite.ExpectedCases > maxCasesPerSuite {
		add("manifest.case_limit", "", fmt.Sprintf(
			"expected cases exceed the limit of %d", maxCasesPerSuite))
	}
	if suite.ExpectedAttempts > maxAttemptsPerSuite {
		add("manifest.attempt_limit", "", fmt.Sprintf(
			"expected attempts exceed the limit of %d", maxAttemptsPerSuite))
	}
	if len(suite.Cases) > maxCasesPerSuite {
		add("manifest.case_limit", "/cases", fmt.Sprintf(
			"listed cases exceed the limit of %d", maxCasesPerSuite))
	}
	if len(suite.Populations) == 0 {
		add("manifest.populations_empty", "/populations", "at least one condition population is required")
	}
	if len(suite.Cases) == 0 {
		add("manifest.cases_empty", "/cases", "the exact case population is required")
	}

	type counts struct{ cases, attempts int }
	actual := make(map[string]counts)
	seenCases := make(map[string]bool, len(suite.Cases))
	totalAttempts := 0
	for index, caseSpec := range suite.Cases {
		caseScope := fmt.Sprintf("/cases/%d", index)
		validateCanonicalText(&findings, "manifest.condition", scope+caseScope,
			"condition", caseSpec.Condition)
		validateCanonicalText(&findings, "manifest.case_id", scope+caseScope,
			"case ID", caseSpec.ID)
		identity := caseSpec.Condition + "\x00" + caseSpec.ID
		if seenCases[identity] {
			add("manifest.case_duplicate", caseScope, fmt.Sprintf(
				"case %q in condition %q is duplicated", caseSpec.ID, caseSpec.Condition))
		}
		seenCases[identity] = true
		if len(caseSpec.Repetitions) == 0 {
			add("manifest.repetitions_empty", caseScope,
				"every case must declare at least one repetition")
		}
		if len(caseSpec.Repetitions) < suite.MinimumRepetitions {
			add("manifest.repetition_minimum", caseScope, fmt.Sprintf(
				"case declares %d repetitions, below the suite minimum %d",
				len(caseSpec.Repetitions), suite.MinimumRepetitions))
		}
		if len(caseSpec.Repetitions) > maxRepetitionsPerCase {
			add("manifest.repetition_limit", caseScope, fmt.Sprintf(
				"repetitions exceed the per-case limit of %d", maxRepetitionsPerCase))
		}
		seenRepetitions := make(map[string]bool, len(caseSpec.Repetitions))
		for repetitionIndex, repetition := range caseSpec.Repetitions {
			repetitionScope := fmt.Sprintf("%s/repetitions/%d", caseScope, repetitionIndex)
			validateCanonicalText(&findings, "manifest.repetition_id", scope+repetitionScope,
				"repetition ID", repetition)
			if seenRepetitions[repetition] {
				add("manifest.repetition_duplicate", repetitionScope, fmt.Sprintf(
					"repetition %q is duplicated", repetition))
			}
			seenRepetitions[repetition] = true
		}
		if caseSpec.RequirePolicy && caseSpec.Policy == nil {
			add("manifest.case_policy_missing", caseScope+"/policy",
				"this exact case requires a preregistered partition policy")
		}
		if caseSpec.Policy != nil {
			findings = append(findings, validatePartitionPolicy(
				*caseSpec.Policy, suite.Policy, len(caseSpec.Repetitions), 1,
				scope+caseScope+"/policy")...)
		}
		value := actual[caseSpec.Condition]
		value.cases++
		value.attempts += len(caseSpec.Repetitions)
		actual[caseSpec.Condition] = value
		totalAttempts += len(caseSpec.Repetitions)
	}
	if len(suite.Cases) != suite.ExpectedCases {
		add("manifest.case_count", "/cases", fmt.Sprintf(
			"listed %d cases, expected %d", len(suite.Cases), suite.ExpectedCases))
	}
	if totalAttempts != suite.ExpectedAttempts {
		add("manifest.attempt_count", "/cases", fmt.Sprintf(
			"listed %d attempts, expected %d", totalAttempts, suite.ExpectedAttempts))
	}

	seenPopulations := make(map[string]bool, len(suite.Populations))
	populationCases, populationAttempts := 0, 0
	for index, population := range suite.Populations {
		populationScope := fmt.Sprintf("/populations/%d", index)
		validateCanonicalText(&findings, "manifest.condition", scope+populationScope,
			"condition", population.Condition)
		if seenPopulations[population.Condition] {
			add("manifest.population_duplicate", populationScope, fmt.Sprintf(
				"condition %q is duplicated", population.Condition))
		}
		seenPopulations[population.Condition] = true
		if population.ExpectedCases <= 0 || population.ExpectedAttempts <= 0 {
			add("manifest.population_count", populationScope,
				"expected cases and attempts must both be positive")
		}
		if population.RequirePolicy && population.Policy == nil {
			add("manifest.condition_policy_missing", populationScope+"/policy",
				"this condition requires a preregistered partition policy")
		}
		if population.Policy != nil {
			findings = append(findings, validatePartitionPolicy(
				*population.Policy, suite.Policy, population.ExpectedAttempts,
				population.ExpectedCases, scope+populationScope+"/policy")...)
		}
		observed, exists := actual[population.Condition]
		if !exists {
			add("manifest.population_missing_cases", populationScope, fmt.Sprintf(
				"condition %q has no listed cases", population.Condition))
		} else if observed.cases != population.ExpectedCases ||
			observed.attempts != population.ExpectedAttempts {
			add("manifest.population_mismatch", populationScope, fmt.Sprintf(
				"condition %q lists %d cases and %d attempts, expected %d and %d",
				population.Condition, observed.cases, observed.attempts,
				population.ExpectedCases, population.ExpectedAttempts))
		}
		populationCases += population.ExpectedCases
		populationAttempts += population.ExpectedAttempts
	}
	for condition := range actual {
		if !seenPopulations[condition] {
			add("manifest.population_undeclared", "/populations", fmt.Sprintf(
				"listed cases use undeclared condition %q", condition))
		}
	}
	if populationCases != suite.ExpectedCases || populationAttempts != suite.ExpectedAttempts {
		add("manifest.population_total", "/populations", fmt.Sprintf(
			"populations total %d cases and %d attempts, suite expects %d and %d",
			populationCases, populationAttempts, suite.ExpectedCases, suite.ExpectedAttempts))
	}

	findings = append(findings, validateSuitePolicy(suite.Policy, suite, scope+"/policy")...)
	return findings
}

func validateSuitePolicy(policy SuitePolicy, suite SuiteSpec, scope string) []Finding {
	var findings []Finding
	add := func(code, child, message string) {
		findings = append(findings, Finding{Code: code, Scope: scope + child, Message: message})
	}
	if policy.Inference.ConfidenceNonFinite != "" {
		add("manifest.confidence", "/inference", fmt.Sprintf(
			"confidence must be finite (observed %s)", policy.Inference.ConfidenceNonFinite))
	} else if !finite(policy.Inference.Confidence) ||
		policy.Inference.Confidence <= 0.5 || policy.Inference.Confidence >= 1 {
		add("manifest.confidence", "/inference", "confidence must be greater than 0.5 and less than 1")
	}
	if policy.Inference.Resamples < 1000 {
		add("manifest.resamples", "/inference", "at least 1000 bootstrap resamples are required")
	}
	if policy.Inference.Resamples > maxBootstrapResamples {
		add("manifest.resamples_limit", "/inference", fmt.Sprintf(
			"bootstrap resamples exceed the limit of %d", maxBootstrapResamples))
	}
	if policy.Inference.ConfidenceNonFinite == "" && finite(policy.Inference.Confidence) &&
		policy.Inference.Resamples > 0 &&
		float64(policy.Inference.Resamples)*(1-policy.Inference.Confidence) < 20 {
		add("manifest.bootstrap_tail_resolution", "/inference",
			"bootstrap confidence tail must contain at least 20 resamples")
	}
	if policy.Inference.Seed == 0 {
		add("manifest.bootstrap_seed", "/inference", "a non-zero predeclared bootstrap seed is required")
	}
	findings = append(findings, validateAcceptanceBasis(policy.AcceptanceBasis,
		scope+"/acceptance_basis")...)
	if !policy.RequireEvidence {
		add("manifest.evidence_required", "/require_evidence",
			"migration comparisons must require immutable per-attempt evidence")
	}
	if len(policy.RequiredEvidenceKinds) == 0 {
		add("manifest.evidence_kinds_empty", "/required_evidence_kinds",
			"at least one immutable per-attempt evidence kind is required")
	}
	if len(policy.RequiredEvidenceKinds) > maxFixedAndTreatmentAxes {
		add("manifest.evidence_kinds_limit", "/required_evidence_kinds", fmt.Sprintf(
			"required evidence kinds exceed the limit of %d", maxFixedAndTreatmentAxes))
	}
	seenEvidenceKinds := make(map[string]bool, len(policy.RequiredEvidenceKinds))
	for index, kind := range policy.RequiredEvidenceKinds {
		kindScope := fmt.Sprintf("%s/required_evidence_kinds/%d", scope, index)
		validateCanonicalText(&findings, "manifest.evidence_kind", kindScope,
			"required evidence kind", kind)
		if seenEvidenceKinds[kind] {
			add("manifest.evidence_kind_duplicate", fmt.Sprintf("/required_evidence_kinds/%d", index),
				fmt.Sprintf("required evidence kind %q is duplicated", kind))
		}
		seenEvidenceKinds[kind] = true
	}
	findings = append(findings, validateRatePolicy(policy.Pass, suite, scope+"/pass")...)
	findings = append(findings, validateOutcomePolicy(policy.Interaction, suite,
		scope+"/interaction")...)
	findings = append(findings, validateOutcomePolicy(policy.Deadline, suite,
		scope+"/deadline")...)

	seenLatency := make(map[string]bool, len(policy.Latencies))
	requiredLatencyGates := 0
	if len(policy.Latencies) == 0 {
		add("manifest.latencies_empty", "/latencies",
			"at least one latency measurement is required for a migration gate")
	}
	if len(policy.Latencies) > maxLatenciesPerSuite {
		add("manifest.latency_limit", "/latencies", fmt.Sprintf(
			"latency measurements exceed the limit of %d", maxLatenciesPerSuite))
	}
	for index, latency := range policy.Latencies {
		latencyScope := fmt.Sprintf("%s/latencies/%d", scope, index)
		validateCanonicalText(&findings, "manifest.latency_name", latencyScope,
			"latency name", latency.Name)
		validateCanonicalText(&findings, "manifest.latency_unit", latencyScope,
			"latency unit", latency.Unit)
		if seenLatency[latency.Name] {
			add("manifest.latency_duplicate", fmt.Sprintf("/latencies/%d", index),
				fmt.Sprintf("latency %q is duplicated", latency.Name))
		}
		seenLatency[latency.Name] = true
		if latency.Gate == nil {
			if latency.Required {
				add("manifest.latency_gate_missing", fmt.Sprintf("/latencies/%d", index),
					"a required latency must have a predeclared gate")
			}
			continue
		}
		if latency.Required {
			requiredLatencyGates++
		}
		if latency.Gate.MinimumAttempts <= 0 || latency.Gate.MinimumCases <= 0 {
			add("manifest.latency_minimum", fmt.Sprintf("/latencies/%d/gate", index),
				"latency minimum attempts and cases must both be positive")
		}
		if latency.Gate.MinimumCases == 1 {
			add("manifest.latency_clusters", fmt.Sprintf("/latencies/%d/gate", index),
				"a case-cluster bootstrap requires at least two applicable cases")
		}
		if latency.Gate.MinimumAttempts > suite.ExpectedAttempts ||
			latency.Gate.MinimumCases > suite.ExpectedCases {
			add("manifest.latency_minimum_impossible", fmt.Sprintf("/latencies/%d/gate", index),
				"latency minimum samples exceed the declared suite population")
		}
		if len(latency.Gate.Limits) == 0 {
			add("manifest.latency_limits_empty", fmt.Sprintf("/latencies/%d/gate", index),
				"a latency gate must declare at least one statistic limit")
		}
		if len(latency.Gate.Limits) > maxLatencyLimits {
			add("manifest.latency_limits_limit", fmt.Sprintf("/latencies/%d/gate", index),
				fmt.Sprintf("latency statistic limits exceed the limit of %d", maxLatencyLimits))
		}
		seenStatistics := make(map[LatencyStatistic]bool, len(latency.Gate.Limits))
		for limitIndex, limit := range latency.Gate.Limits {
			limitScope := fmt.Sprintf("/latencies/%d/gate/limits/%d", index, limitIndex)
			if !validStatistic(limit.Statistic) {
				add("manifest.latency_statistic", limitScope, fmt.Sprintf(
					"unknown latency statistic %q", limit.Statistic))
			}
			if seenStatistics[limit.Statistic] {
				add("manifest.latency_statistic_duplicate", limitScope, fmt.Sprintf(
					"latency statistic %q is duplicated", limit.Statistic))
			}
			seenStatistics[limit.Statistic] = true
			if limit.MaximumIncreaseNonFinite != "" {
				add("manifest.latency_limit", limitScope, fmt.Sprintf(
					"maximum increase must be finite (observed %s)",
					limit.MaximumIncreaseNonFinite))
			} else if !finite(limit.MaximumIncrease) {
				add("manifest.latency_limit", limitScope, "maximum increase must be finite")
			}
		}
		if latency.Required {
			if !seenStatistics[StatisticP50] {
				add("manifest.latency_p50_gate_missing", fmt.Sprintf("/latencies/%d/gate", index),
					"a required latency must gate p50")
			}
			if !seenStatistics[StatisticP90] && !seenStatistics[StatisticP95] &&
				!seenStatistics[StatisticP99] {
				add("manifest.latency_tail_gate_missing", fmt.Sprintf("/latencies/%d/gate", index),
					"a required latency must gate at least one of p90, p95, or p99")
			}
		}
	}
	if requiredLatencyGates == 0 {
		add("manifest.required_latency_empty", "/latencies",
			"at least one required latency with a p50 and tail gate is required")
	}
	if work, overflow := bootstrapWorkUnits(suite); overflow || work > maxBootstrapWorkUnits {
		add("manifest.bootstrap_work_limit", "/inference", fmt.Sprintf(
			"declared bootstrap work exceeds the limit of %d units", maxBootstrapWorkUnits))
	}
	return findings
}

func validatePartitionPolicy(
	policy PartitionPolicy, suitePolicy SuitePolicy, expectedAttempts, expectedCases int, scope string,
) []Finding {
	var findings []Finding
	findings = append(findings, validatePartitionRatePolicy(
		policy.Pass, expectedAttempts, expectedCases, scope+"/pass")...)
	findings = append(findings, validatePartitionOutcomePolicy(
		policy.Interaction, expectedAttempts, expectedCases, scope+"/interaction")...)
	findings = append(findings, validatePartitionOutcomePolicy(
		policy.Deadline, expectedAttempts, expectedCases, scope+"/deadline")...)
	if suitePolicy.Interaction.Required &&
		(!policy.Interaction.Required || policy.Interaction.NonInferiority == nil) {
		findings = append(findings, Finding{Code: "manifest.partition_interaction_policy_missing",
			Scope:   scope + "/interaction",
			Message: "a suite-required interaction outcome needs a partition non-inferiority gate"})
	}
	if suitePolicy.Deadline.Required &&
		(!policy.Deadline.Required || policy.Deadline.NonInferiority == nil) {
		findings = append(findings, Finding{Code: "manifest.partition_deadline_policy_missing",
			Scope:   scope + "/deadline",
			Message: "a suite-required deadline outcome needs a partition non-inferiority gate"})
	}

	declared := make(map[string]LatencyPolicy, len(suitePolicy.Latencies))
	for _, latency := range suitePolicy.Latencies {
		declared[latency.Name] = latency
	}
	seen := make(map[string]LatencyPolicy, len(policy.Latencies))
	if len(policy.Latencies) > maxLatenciesPerSuite {
		findings = append(findings, Finding{Code: "manifest.partition_latency_limit",
			Scope: scope + "/latencies", Message: fmt.Sprintf(
				"partition latency measurements exceed the limit of %d", maxLatenciesPerSuite)})
	}
	for index, latency := range policy.Latencies {
		latencyScope := fmt.Sprintf("%s/latencies/%d", scope, index)
		validateCanonicalText(&findings, "manifest.partition_latency_name", latencyScope,
			"partition latency name", latency.Name)
		validateCanonicalText(&findings, "manifest.partition_latency_unit", latencyScope,
			"partition latency unit", latency.Unit)
		if _, duplicated := seen[latency.Name]; duplicated {
			findings = append(findings, Finding{Code: "manifest.partition_latency_duplicate",
				Scope: latencyScope, Message: fmt.Sprintf(
					"partition latency %q is duplicated", latency.Name)})
		}
		seen[latency.Name] = latency
		parent, known := declared[latency.Name]
		switch {
		case !known:
			findings = append(findings, Finding{Code: "manifest.partition_latency_undeclared",
				Scope: latencyScope, Message: fmt.Sprintf(
					"partition latency %q is not declared by the suite", latency.Name)})
		case latency.Unit != parent.Unit:
			findings = append(findings, Finding{Code: "manifest.partition_latency_unit_mismatch",
				Scope: latencyScope, Message: fmt.Sprintf(
					"partition latency %q uses unit %q, expected %q",
					latency.Name, latency.Unit, parent.Unit)})
		}
		findings = append(findings, validatePartitionLatencyPolicy(
			latency, expectedAttempts, expectedCases, latencyScope)...)
	}
	for _, parent := range suitePolicy.Latencies {
		if !parent.Required {
			continue
		}
		latency, present := seen[parent.Name]
		if !present || !latency.Required || latency.Gate == nil {
			findings = append(findings, Finding{Code: "manifest.partition_latency_policy_missing",
				Scope: scope + "/latencies", Message: fmt.Sprintf(
					"suite-required latency %q needs a required partition gate", parent.Name)})
		}
	}
	return findings
}

func validatePartitionOutcomePolicy(
	policy OutcomePolicy, expectedAttempts, expectedCases int, scope string,
) []Finding {
	if policy.NonInferiority == nil {
		if policy.Required {
			return []Finding{{Code: "manifest.partition_outcome_gate_missing", Scope: scope,
				Message: "a required partition outcome needs a predeclared non-inferiority gate"}}
		}
		return nil
	}
	return validatePartitionRatePolicy(
		*policy.NonInferiority, expectedAttempts, expectedCases, scope+"/non_inferiority")
}

func validatePartitionRatePolicy(
	policy RatePolicy, expectedAttempts, expectedCases int, scope string,
) []Finding {
	var findings []Finding
	if policy.MarginNonFinite != "" {
		findings = append(findings, Finding{Code: "manifest.partition_rate_margin", Scope: scope,
			Message: fmt.Sprintf("non-inferiority margin must be finite (observed %s)",
				policy.MarginNonFinite)})
	} else if !finite(policy.Margin) || policy.Margin < 0 || policy.Margin >= 1 {
		findings = append(findings, Finding{Code: "manifest.partition_rate_margin", Scope: scope,
			Message: "non-inferiority margin must be finite, non-negative, and less than 1"})
	}
	if policy.MinimumAttempts <= 0 || policy.MinimumCases <= 0 {
		findings = append(findings, Finding{Code: "manifest.partition_rate_minimum", Scope: scope,
			Message: "minimum attempts and cases must both be positive"})
	}
	if policy.MinimumAttempts > expectedAttempts || policy.MinimumCases > expectedCases {
		findings = append(findings, Finding{Code: "manifest.partition_rate_minimum_impossible",
			Scope: scope, Message: "minimum samples exceed the declared partition population"})
	}
	return findings
}

func validatePartitionLatencyPolicy(
	latency LatencyPolicy, expectedAttempts, expectedCases int, scope string,
) []Finding {
	var findings []Finding
	if latency.Gate == nil {
		if latency.Required {
			findings = append(findings, Finding{Code: "manifest.partition_latency_gate_missing",
				Scope: scope, Message: "a required partition latency needs a predeclared gate"})
		}
		return findings
	}
	gate := latency.Gate
	if gate.MinimumAttempts <= 0 || gate.MinimumCases <= 0 {
		findings = append(findings, Finding{Code: "manifest.partition_latency_minimum", Scope: scope,
			Message: "latency minimum attempts and cases must both be positive"})
	}
	if gate.MinimumAttempts > expectedAttempts || gate.MinimumCases > expectedCases {
		findings = append(findings, Finding{Code: "manifest.partition_latency_minimum_impossible",
			Scope: scope, Message: "latency minimum samples exceed the declared partition population"})
	}
	if len(gate.Limits) == 0 {
		findings = append(findings, Finding{Code: "manifest.partition_latency_limits_empty",
			Scope: scope, Message: "a partition latency gate must declare a statistic limit"})
	}
	if len(gate.Limits) > maxLatencyLimits {
		findings = append(findings, Finding{Code: "manifest.partition_latency_limits_limit",
			Scope: scope, Message: fmt.Sprintf(
				"partition latency statistic limits exceed the limit of %d", maxLatencyLimits)})
	}
	seenStatistics := make(map[LatencyStatistic]bool, len(gate.Limits))
	for index, limit := range gate.Limits {
		limitScope := fmt.Sprintf("%s/gate/limits/%d", scope, index)
		if !validStatistic(limit.Statistic) {
			findings = append(findings, Finding{Code: "manifest.partition_latency_statistic",
				Scope: limitScope, Message: fmt.Sprintf(
					"unknown latency statistic %q", limit.Statistic)})
		}
		if seenStatistics[limit.Statistic] {
			findings = append(findings, Finding{Code: "manifest.partition_latency_statistic_duplicate",
				Scope: limitScope, Message: fmt.Sprintf(
					"latency statistic %q is duplicated", limit.Statistic)})
		}
		seenStatistics[limit.Statistic] = true
		if limit.MaximumIncreaseNonFinite != "" {
			findings = append(findings, Finding{Code: "manifest.partition_latency_limit",
				Scope: limitScope, Message: fmt.Sprintf(
					"maximum increase must be finite (observed %s)", limit.MaximumIncreaseNonFinite)})
		} else if !finite(limit.MaximumIncrease) {
			findings = append(findings, Finding{Code: "manifest.partition_latency_limit",
				Scope: limitScope, Message: "maximum increase must be finite"})
		}
	}
	if latency.Required {
		if !seenStatistics[StatisticP50] {
			findings = append(findings, Finding{Code: "manifest.partition_latency_p50_gate_missing",
				Scope: scope, Message: "a required partition latency must gate p50"})
		}
		if !seenStatistics[StatisticP90] && !seenStatistics[StatisticP95] &&
			!seenStatistics[StatisticP99] {
			findings = append(findings, Finding{Code: "manifest.partition_latency_tail_gate_missing",
				Scope: scope, Message: "a required partition latency must gate a tail statistic"})
		}
	}
	return findings
}

func bootstrapWorkUnits(suite SuiteSpec) (uint64, bool) {
	policy := suite.Policy
	rateGates := uint64(1)
	if policy.Interaction.NonInferiority != nil {
		rateGates++
	}
	if policy.Deadline.NonInferiority != nil {
		rateGates++
	}
	latencyGates := uint64(0)
	for _, latency := range policy.Latencies {
		if latency.Gate != nil {
			latencyGates++
		}
	}
	if suite.ExpectedCases <= 0 || suite.ExpectedAttempts <= 0 || policy.Inference.Resamples <= 0 {
		return 0, false
	}
	// One rate draw visits each case. A latency draw visits each case to
	// choose cluster weights and both arm observations to compute statistics.
	cases, attempts := uint64(suite.ExpectedCases), uint64(suite.ExpectedAttempts)
	rateWork, overflow := checkedMultiply(rateGates, cases)
	if overflow {
		return 0, true
	}
	twiceAttempts, overflow := checkedMultiply(2, attempts)
	if overflow {
		return 0, true
	}
	latencyVisits, overflow := checkedAdd(cases, twiceAttempts)
	if overflow {
		return 0, true
	}
	latencyWork, overflow := checkedMultiply(latencyGates, latencyVisits)
	if overflow {
		return 0, true
	}
	perResample, overflow := checkedAdd(rateWork, latencyWork)
	if overflow {
		return 0, true
	}
	resamples := uint64(policy.Inference.Resamples)
	total, overflow := checkedMultiply(perResample, resamples)
	if overflow {
		return 0, true
	}
	for _, population := range suite.Populations {
		if population.Policy == nil {
			continue
		}
		work, overflow := partitionBootstrapWorkUnits(
			*population.Policy, population.ExpectedCases, population.ExpectedAttempts, resamples)
		if overflow {
			return 0, true
		}
		total, overflow = checkedAdd(total, work)
		if overflow {
			return 0, true
		}
	}
	for _, caseSpec := range suite.Cases {
		if caseSpec.Policy == nil {
			continue
		}
		work, overflow := partitionBootstrapWorkUnits(
			*caseSpec.Policy, 1, len(caseSpec.Repetitions), resamples)
		if overflow {
			return 0, true
		}
		total, overflow = checkedAdd(total, work)
		if overflow {
			return 0, true
		}
	}
	return total, false
}

func partitionBootstrapWorkUnits(
	policy PartitionPolicy, cases, attempts int, resamples uint64,
) (uint64, bool) {
	rateGates := uint64(1)
	if policy.Interaction.NonInferiority != nil {
		rateGates++
	}
	if policy.Deadline.NonInferiority != nil {
		rateGates++
	}
	latencyGates := uint64(0)
	for _, latency := range policy.Latencies {
		if latency.Gate != nil {
			latencyGates++
		}
	}
	rateWork, overflow := checkedMultiply(rateGates, uint64(cases))
	if overflow {
		return 0, true
	}
	twiceAttempts, overflow := checkedMultiply(2, uint64(attempts))
	if overflow {
		return 0, true
	}
	latencyVisits, overflow := checkedAdd(uint64(cases), twiceAttempts)
	if overflow {
		return 0, true
	}
	latencyWork, overflow := checkedMultiply(latencyGates, latencyVisits)
	if overflow {
		return 0, true
	}
	perResample, overflow := checkedAdd(rateWork, latencyWork)
	if overflow {
		return 0, true
	}
	return checkedMultiply(perResample, resamples)
}

func manifestBootstrapWorkUnits(manifest Manifest) (uint64, bool) {
	total := uint64(0)
	for _, suite := range manifest.Suites {
		work, overflow := bootstrapWorkUnits(suite)
		if overflow {
			return 0, true
		}
		total, overflow = checkedAdd(total, work)
		if overflow {
			return 0, true
		}
	}
	return total, false
}

func checkedAdd(left, right uint64) (uint64, bool) {
	if right > ^uint64(0)-left {
		return 0, true
	}
	return left + right, false
}

func checkedMultiply(left, right uint64) (uint64, bool) {
	if left != 0 && right > ^uint64(0)/left {
		return 0, true
	}
	return left * right, false
}

func validateOutcomePolicy(policy OutcomePolicy, suite SuiteSpec, scope string) []Finding {
	if policy.NonInferiority == nil {
		if policy.Required {
			return []Finding{{Code: "manifest.outcome_gate_missing", Scope: scope,
				Message: "a required outcome must have a predeclared non-inferiority gate"}}
		}
		return nil
	}
	return validateRatePolicy(*policy.NonInferiority, suite, scope+"/non_inferiority")
}

func validateRatePolicy(policy RatePolicy, suite SuiteSpec, scope string) []Finding {
	var findings []Finding
	if policy.MarginNonFinite != "" {
		findings = append(findings, Finding{Code: "manifest.rate_margin", Scope: scope,
			Message: fmt.Sprintf("non-inferiority margin must be finite (observed %s)",
				policy.MarginNonFinite)})
	} else if !finite(policy.Margin) || policy.Margin < 0 || policy.Margin >= 1 {
		findings = append(findings, Finding{Code: "manifest.rate_margin", Scope: scope,
			Message: "non-inferiority margin must be finite, non-negative, and less than 1"})
	}
	if policy.MinimumAttempts <= 0 || policy.MinimumCases <= 0 {
		findings = append(findings, Finding{Code: "manifest.rate_minimum", Scope: scope,
			Message: "minimum attempts and cases must both be positive"})
	}
	if policy.MinimumCases == 1 {
		findings = append(findings, Finding{Code: "manifest.rate_clusters", Scope: scope,
			Message: "a case-cluster bootstrap requires at least two applicable cases"})
	}
	if policy.MinimumAttempts > suite.ExpectedAttempts || policy.MinimumCases > suite.ExpectedCases {
		findings = append(findings, Finding{Code: "manifest.rate_minimum_impossible", Scope: scope,
			Message: "minimum samples exceed the declared suite population"})
	}
	return findings
}

func validateAcceptanceBasis(reference EvidenceRef, scope string) []Finding {
	var findings []Finding
	validateCanonicalText(&findings, "manifest.acceptance_basis_kind", scope,
		"acceptance-basis kind", reference.Kind)
	validateCanonicalText(&findings, "manifest.acceptance_basis_location", scope,
		"acceptance-basis location", reference.Location)
	if reference.Kind != "" && reference.Kind != AcceptanceBasisKind {
		findings = append(findings, Finding{Code: "manifest.acceptance_basis_kind", Scope: scope,
			Message: fmt.Sprintf("acceptance-basis kind must be %q", AcceptanceBasisKind)})
	}
	if !validSHA256(reference.SHA256) {
		findings = append(findings, Finding{Code: "manifest.acceptance_basis_digest", Scope: scope,
			Message: "acceptance-basis SHA-256 must be 64 lowercase hexadecimal characters"})
	}
	return findings
}

func validateNamedDigest(findings *[]Finding, code, scope, name, value string) {
	if !namedDigestInvalid(name, value) {
		return
	}
	*findings = append(*findings, Finding{Code: code, Scope: scope,
		Message: fmt.Sprintf("axis %q must contain a lowercase SHA-256 digest", name)})
}

func validateNamedDigestIndexed(
	findings *[]Finding, code, parent, collection string, index int, name, value string,
) {
	if !namedDigestInvalid(name, value) {
		return
	}
	*findings = append(*findings, Finding{Code: code,
		Scope:   indexedScope(parent, collection, index),
		Message: fmt.Sprintf("axis %q must contain a lowercase SHA-256 digest", name)})
}

func namedDigestInvalid(name, value string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return (strings.HasSuffix(lower, "_sha256") || strings.HasSuffix(lower, ".sha256")) &&
		!validSHA256(value)
}

func validateAttemptError(findings *[]Finding, scope, value string) {
	switch {
	case len(value) > maxAttemptErrorBytes:
		*findings = append(*findings, Finding{Code: "attempt.error_limit", Scope: scope,
			Message: fmt.Sprintf("attempt error exceeds %d UTF-8 bytes", maxAttemptErrorBytes)})
	case !utf8.ValidString(value):
		*findings = append(*findings, Finding{Code: "attempt.error_utf8", Scope: scope,
			Message: "attempt error must be valid UTF-8"})
	case strings.IndexByte(value, 0) >= 0:
		*findings = append(*findings, Finding{Code: "attempt.error_control", Scope: scope,
			Message: "attempt error must not contain NUL"})
	}
}

func validateCanonicalText(findings *[]Finding, code, scope, label, value string) {
	if message := canonicalTextMessage(label, value); message != "" {
		*findings = append(*findings, Finding{Code: code, Scope: scope, Message: message})
	}
}

func validateCanonicalTextIndexed(
	findings *[]Finding, code, parent, collection string, index int, label, value string,
) {
	if message := canonicalTextMessage(label, value); message != "" {
		*findings = append(*findings, Finding{Code: code,
			Scope: indexedScope(parent, collection, index), Message: message})
	}
}

func canonicalTextMessage(label, value string) string {
	switch {
	case strings.TrimSpace(value) == "":
		return label + " is required"
	case value != strings.TrimSpace(value):
		return label + " must not have surrounding whitespace"
	case len(value) > maxCanonicalTextBytes:
		return fmt.Sprintf("%s exceeds %d UTF-8 bytes", label, maxCanonicalTextBytes)
	case !utf8.ValidString(value):
		return label + " must be valid UTF-8"
	case strings.IndexFunc(value, unicode.IsControl) >= 0:
		return label + " must not contain control characters"
	default:
		return ""
	}
}

func validStatistic(value LatencyStatistic) bool {
	switch value {
	case StatisticMean, StatisticP50, StatisticP90, StatisticP95, StatisticP99:
		return true
	default:
		return false
	}
}

func validOutcome(value OutcomeState) bool {
	return value == OutcomeNotApplicable || value == OutcomeSatisfied || value == OutcomeFailed
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func sortFindings(findings []Finding) {
	sort.Slice(findings, func(left, right int) bool {
		if findings[left].Code != findings[right].Code {
			return findings[left].Code < findings[right].Code
		}
		if findings[left].Scope != findings[right].Scope {
			return findings[left].Scope < findings[right].Scope
		}
		return findings[left].Message < findings[right].Message
	})
}
