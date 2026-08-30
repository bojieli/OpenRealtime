package migration

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Compare performs an initial strict migration comparison with no campaign
// predecessors. Use CompareWithHistory for a diagnostic or full rerun that
// names earlier reports.
func Compare(manifest Manifest, baseline, candidate []Attempt) Report {
	return CompareWithHistory(manifest, baseline, candidate, nil)
}

// CompareWithHistory validates the complete transitive campaign history as
// well as the paired observations. It never returns a summary from a subset:
// any malformed, duplicate, missing, unexpected, incomplete, or forged input
// makes the sealed report unreportable while every supplied attempt remains in
// Report.Attempts.
func CompareWithHistory(
	manifest Manifest, baseline, candidate []Attempt, history []Report,
) Report {
	history = canonicalHistory(history)
	report := compareCore(manifest, baseline, candidate)
	report.SuppliedHistory = historyReferences(history)
	lineageFindings := validateLineage(report.Manifest, history)
	if len(lineageFindings) > 0 {
		report.Refusals = append(report.Refusals, lineageFindings...)
		sortFindings(report.Refusals)
		report.Reportable = false
		report.Accepted = false
		report.Matched = make([]MatchedAttempt, 0)
		report.Comparisons = make([]SuiteComparison, 0)
	}
	applyCampaignGate(&report)
	report.ReportID = reportDigest(report)
	return report
}

func compareCore(manifest Manifest, baseline, candidate []Attempt) Report {
	canonical := canonicalManifest(manifest)
	report := Report{
		Version: ReportVersion, ManifestID: canonical.ID(), Manifest: canonical,
		SuppliedHistory: make([]ReportReference, 0),
		Matched:         make([]MatchedAttempt, 0), Refusals: make([]Finding, 0),
		Comparisons: make([]SuiteComparison, 0),
	}
	report.Attempts = mergeObserved(
		canonicalObserved(ArmBaseline, baseline), canonicalObserved(ArmCandidate, candidate))

	findings := validateManifest(canonical)
	suites := make(map[string]SuiteSpec, len(canonical.Suites))
	expected := make(map[AttemptKey]bool)
	for _, suite := range canonical.Suites {
		suites[suite.Name] = suite
		for _, caseSpec := range suite.Cases {
			for _, repetition := range caseSpec.Repetitions {
				expected[AttemptKey{
					Suite: suite.Name, Condition: caseSpec.Condition,
					Case: caseSpec.ID, Repetition: repetition,
				}] = true
			}
		}
	}

	byArm := map[Arm]map[AttemptKey][]Attempt{
		ArmBaseline: {}, ArmCandidate: {},
	}
	seenIDs := map[Arm]map[string]bool{
		ArmBaseline: {}, ArmCandidate: {},
	}
	for _, observed := range report.Attempts {
		attempt := observed.Attempt
		scope := string(observed.Arm) + "/" + keyScope(attempt.Key)
		if seenIDs[observed.Arm][attempt.ID] && attempt.ID != "" {
			findings = append(findings, Finding{Code: "attempt.id_duplicate", Scope: scope,
				Message: fmt.Sprintf("attempt ID %q is duplicated in the %s arm", attempt.ID, observed.Arm)})
		}
		seenIDs[observed.Arm][attempt.ID] = true
		byArm[observed.Arm][attempt.Key] = append(byArm[observed.Arm][attempt.Key], attempt)
		suite, suiteKnown := suites[attempt.Key.Suite]
		findings = append(findings, validateAttempt(canonical, suite, suiteKnown,
			expected[attempt.Key], observed.Arm, attempt, scope)...)
	}

	keys := sortedKeys(expected)
	for _, key := range keys {
		for _, arm := range []Arm{ArmBaseline, ArmCandidate} {
			count := len(byArm[arm][key])
			scope := string(arm) + "/" + keyScope(key)
			switch {
			case count == 0:
				findings = append(findings, Finding{Code: "attempt.missing", Scope: scope,
					Message: "the manifest-declared attempt is missing"})
			case count > 1:
				findings = append(findings, Finding{Code: "attempt.duplicate", Scope: scope,
					Message: fmt.Sprintf("the manifest-declared key has %d attempts", count)})
			}
		}
		if len(byArm[ArmBaseline][key]) == 1 && len(byArm[ArmCandidate][key]) == 1 {
			findings = append(findings, validatePair(
				suites[key.Suite], byArm[ArmBaseline][key][0], byArm[ArmCandidate][key][0])...)
		}
	}

	sortFindings(findings)
	report.Refusals = findings
	if len(findings) == 0 {
		for _, key := range keys {
			report.Matched = append(report.Matched, matchedAttempt(
				byArm[ArmBaseline][key][0], byArm[ArmCandidate][key][0]))
		}
		report.Comparisons = compareSuites(canonical, report.Matched)
		report.Reportable = true
		report.Accepted = true
		for _, comparison := range report.Comparisons {
			if !comparison.Accepted {
				report.Accepted = false
				break
			}
		}
	}
	return report
}

func applyCampaignGate(report *Report) {
	report.CampaignGate = GateResult{
		Name: pathIdentity(report.Manifest.Campaign.CampaignID,
			report.Manifest.Campaign.RunID, "full_release_rerun"),
		Evaluated: true,
	}
	switch {
	case !report.Reportable:
		report.Accepted = false
		report.CampaignGate.Reason = "the comparison or campaign lineage is unreportable"
	case report.Manifest.Campaign.Kind == RunDiagnostic:
		report.Accepted = false
		report.CampaignGate.Reason = "diagnostic runs cannot satisfy the full release rerun gate"
	case report.Manifest.Campaign.Kind != RunFull:
		report.Accepted = false
		report.CampaignGate.Reason = "the run kind is invalid"
	case !report.Accepted:
		report.CampaignGate.Reason = "one or more predeclared suite gates failed"
	default:
		report.CampaignGate.Passed = true
	}
}

func validateAttempt(
	manifest Manifest, suite SuiteSpec, suiteKnown, keyExpected bool,
	arm Arm, attempt Attempt, scope string,
) []Finding {
	var findings []Finding
	add := func(code, message string) {
		findings = append(findings, Finding{Code: code, Scope: scope, Message: message})
	}
	validateCanonicalText(&findings, "attempt.id", scope, "attempt ID", attempt.ID)
	validateCanonicalText(&findings, "attempt.suite", scope, "suite", attempt.Key.Suite)
	validateCanonicalText(&findings, "attempt.condition", scope, "condition", attempt.Key.Condition)
	validateCanonicalText(&findings, "attempt.case", scope, "case", attempt.Key.Case)
	validateCanonicalText(&findings, "attempt.repetition", scope, "repetition", attempt.Key.Repetition)
	if len(attempt.Axes) > maxFixedAndTreatmentAxes {
		add("attempt.axes_limit", fmt.Sprintf(
			"attempt axes exceed the limit of %d", maxFixedAndTreatmentAxes))
	}
	if len(attempt.Latencies) > maxLatenciesPerSuite {
		add("attempt.latencies_limit", fmt.Sprintf(
			"attempt latencies exceed the limit of %d", maxLatenciesPerSuite))
	}
	if len(attempt.Evidence) > maxFixedAndTreatmentAxes {
		add("attempt.evidence_limit", fmt.Sprintf(
			"attempt evidence references exceed the limit of %d", maxFixedAndTreatmentAxes))
	}
	if !suiteKnown {
		add("attempt.suite_unexpected", fmt.Sprintf("suite %q is not declared by the manifest", attempt.Key.Suite))
	} else if !keyExpected {
		add("attempt.unexpected", "the attempt key is not declared by the manifest")
	}

	expectedAxes := make(map[string]string, len(manifest.FixedAxes)+len(manifest.Treatment))
	for _, axis := range manifest.FixedAxes {
		expectedAxes[axis.Name] = axis.Value
	}
	for _, delta := range manifest.Treatment {
		if arm == ArmBaseline {
			expectedAxes[delta.Axis] = delta.Baseline
		} else {
			expectedAxes[delta.Axis] = delta.Candidate
		}
	}
	seenAxes := make(map[string]bool, len(attempt.Axes))
	for index, axis := range attempt.Axes {
		validateCanonicalTextIndexed(&findings, "attempt.axis_name", scope, "axes", index,
			"axis name", axis.Name)
		validateCanonicalTextIndexed(&findings, "attempt.axis_value", scope, "axes", index,
			"axis value", axis.Value)
		validateNamedDigestIndexed(&findings, "attempt.axis_digest", scope, "axes", index,
			axis.Name, axis.Value)
		if seenAxes[axis.Name] {
			findings = append(findings, Finding{Code: "attempt.axis_duplicate",
				Scope:   indexedScope(scope, "axes", index),
				Message: fmt.Sprintf("axis %q is duplicated", axis.Name)})
		}
		seenAxes[axis.Name] = true
		expectedValue, known := expectedAxes[axis.Name]
		switch {
		case !known:
			findings = append(findings, Finding{Code: "attempt.axis_unexpected",
				Scope:   indexedScope(scope, "axes", index),
				Message: fmt.Sprintf("axis %q is not declared by the manifest", axis.Name)})
		case expectedValue != axis.Value:
			findings = append(findings, Finding{Code: "attempt.axis_mismatch",
				Scope: indexedScope(scope, "axes", index),
				Message: fmt.Sprintf("axis %q is %q, expected %q for the %s arm",
					axis.Name, axis.Value, expectedValue, arm)})
		}
	}
	for name := range expectedAxes {
		if !seenAxes[name] {
			add("attempt.axis_missing", fmt.Sprintf("declared axis %q is missing", name))
		}
	}

	switch {
	case !attempt.Completed && attempt.Error == "":
		add("attempt.incomplete_reason_missing", "an incomplete attempt must retain its error")
	case attempt.Completed && attempt.Error != "":
		add("attempt.completed_with_error", "a completed attempt must not carry an error")
	}
	if attempt.Error != "" {
		validateAttemptError(&findings, scope+"/error", attempt.Error)
	}
	if !attempt.Completed {
		add("attempt.incomplete", "the attempt did not complete and cannot enter a comparison")
		if attempt.Passed {
			add("attempt.incomplete_passed", "an incomplete attempt cannot be marked passed")
		}
	}
	for name, state := range map[string]OutcomeState{
		"interaction": attempt.Outcomes.Interaction,
		"deadline":    attempt.Outcomes.Deadline,
		"safety":      attempt.Outcomes.Safety,
	} {
		if !validOutcome(state) {
			add("attempt.outcome_invalid", fmt.Sprintf("%s outcome %q is invalid", name, state))
		}
	}
	if suiteKnown {
		conditionPolicy, casePolicy := partitionPoliciesForKey(suite, attempt.Key)
		interactionRequired := suite.Policy.Interaction.Required
		deadlineRequired := suite.Policy.Deadline.Required
		if conditionPolicy != nil {
			interactionRequired = interactionRequired || conditionPolicy.Interaction.Required
			deadlineRequired = deadlineRequired || conditionPolicy.Deadline.Required
		}
		if casePolicy != nil {
			interactionRequired = interactionRequired || casePolicy.Interaction.Required
			deadlineRequired = deadlineRequired || casePolicy.Deadline.Required
		}
		if interactionRequired && attempt.Outcomes.Interaction == OutcomeNotApplicable {
			add("attempt.interaction_missing", "interaction outcome is required for this suite")
		}
		if deadlineRequired && attempt.Outcomes.Deadline == OutcomeNotApplicable {
			add("attempt.deadline_missing", "deadline outcome is required for this suite")
		}
		if suite.Policy.Safety.Required && attempt.Outcomes.Safety == OutcomeNotApplicable {
			add("attempt.safety_missing", "safety outcome is required for this suite")
		}
		findings = append(findings, validateAttemptLatencies(suite, attempt, scope)...)
		if suite.Policy.RequireEvidence && len(attempt.Evidence) == 0 {
			add("attempt.evidence_missing", "immutable evidence is required for this suite")
		}
	}
	type evidenceIdentity struct{ kind, location string }
	seenEvidence := make(map[evidenceIdentity]bool, len(attempt.Evidence))
	seenEvidenceKinds := make(map[string]bool, len(attempt.Evidence))
	for index, evidence := range attempt.Evidence {
		validateCanonicalTextIndexed(&findings, "attempt.evidence_kind", scope, "evidence", index,
			"evidence kind", evidence.Kind)
		validateCanonicalTextIndexed(&findings, "attempt.evidence_location", scope, "evidence", index,
			"evidence location", evidence.Location)
		if !validSHA256(evidence.SHA256) {
			findings = append(findings, Finding{Code: "attempt.evidence_digest",
				Scope:   indexedScope(scope, "evidence", index),
				Message: "evidence SHA-256 must be 64 lowercase hexadecimal characters"})
		}
		identity := evidenceIdentity{kind: evidence.Kind, location: evidence.Location}
		seenEvidenceKinds[evidence.Kind] = true
		if seenEvidence[identity] {
			findings = append(findings, Finding{Code: "attempt.evidence_duplicate",
				Scope:   indexedScope(scope, "evidence", index),
				Message: fmt.Sprintf("evidence %q at %q is duplicated", evidence.Kind, evidence.Location)})
		}
		seenEvidence[identity] = true
	}
	if suiteKnown {
		for _, kind := range suite.Policy.RequiredEvidenceKinds {
			if !seenEvidenceKinds[kind] {
				add("attempt.evidence_kind_missing", fmt.Sprintf(
					"required immutable evidence kind %q is missing", kind))
			}
		}
	}
	return findings
}

func validateAttemptLatencies(suite SuiteSpec, attempt Attempt, scope string) []Finding {
	var findings []Finding
	declared := make(map[string]LatencyPolicy, len(suite.Policy.Latencies))
	required := make(map[string]bool, len(suite.Policy.Latencies))
	for _, policy := range suite.Policy.Latencies {
		declared[policy.Name] = policy
		required[policy.Name] = policy.Required
	}
	conditionPolicy, casePolicy := partitionPoliciesForKey(suite, attempt.Key)
	addPartitionRequirements := func(partition *PartitionPolicy) {
		if partition == nil {
			return
		}
		for _, policy := range partition.Latencies {
			required[policy.Name] = required[policy.Name] || policy.Required
		}
	}
	addPartitionRequirements(conditionPolicy)
	addPartitionRequirements(casePolicy)
	seen := make(map[string]bool, len(attempt.Latencies))
	for index, latency := range attempt.Latencies {
		validateCanonicalTextIndexed(&findings, "attempt.latency_name", scope, "latencies", index,
			"latency name", latency.Name)
		validateCanonicalTextIndexed(&findings, "attempt.latency_unit", scope, "latencies", index,
			"latency unit", latency.Unit)
		if seen[latency.Name] {
			findings = append(findings, Finding{Code: "attempt.latency_duplicate",
				Scope:   indexedScope(scope, "latencies", index),
				Message: fmt.Sprintf("latency %q is duplicated", latency.Name)})
		}
		seen[latency.Name] = true
		policy, known := declared[latency.Name]
		switch {
		case !known:
			findings = append(findings, Finding{Code: "attempt.latency_unexpected",
				Scope:   indexedScope(scope, "latencies", index),
				Message: fmt.Sprintf("latency %q is not declared by the suite", latency.Name)})
		case latency.Unit != policy.Unit:
			findings = append(findings, Finding{Code: "attempt.latency_unit_mismatch",
				Scope: indexedScope(scope, "latencies", index),
				Message: fmt.Sprintf("latency %q uses unit %q, expected %q",
					latency.Name, latency.Unit, policy.Unit)})
		}
		if latency.NonFinite != "" {
			switch latency.NonFinite {
			case "nan", "positive_infinity", "negative_infinity":
			default:
				findings = append(findings, Finding{Code: "attempt.latency_non_finite_marker",
					Scope: indexedScope(scope, "latencies", index), Message: fmt.Sprintf(
						"latency %q has unknown non-finite marker %q", latency.Name, latency.NonFinite)})
			}
			if latency.Value != 0 {
				findings = append(findings, Finding{Code: "attempt.latency_non_finite_value",
					Scope: indexedScope(scope, "latencies", index), Message: fmt.Sprintf(
						"latency %q with a non-finite marker must have normalized value zero", latency.Name)})
			}
			findings = append(findings, Finding{Code: "attempt.latency_non_finite",
				Scope: indexedScope(scope, "latencies", index),
				Message: fmt.Sprintf("latency %q must be finite (observed %s)",
					latency.Name, latency.NonFinite)})
		} else if !finite(latency.Value) {
			findings = append(findings, Finding{Code: "attempt.latency_non_finite",
				Scope:   indexedScope(scope, "latencies", index),
				Message: fmt.Sprintf("latency %q must be finite", latency.Name)})
		} else if latency.Value < 0 {
			findings = append(findings, Finding{Code: "attempt.latency_negative",
				Scope:   indexedScope(scope, "latencies", index),
				Message: fmt.Sprintf("latency %q must not be negative", latency.Name)})
		}
	}
	for name, isRequired := range required {
		if isRequired && !seen[name] {
			findings = append(findings, Finding{Code: "attempt.latency_missing", Scope: scope,
				Message: fmt.Sprintf("required latency %q is missing", name)})
		}
	}
	return findings
}

func partitionPoliciesForKey(suite SuiteSpec, key AttemptKey) (*PartitionPolicy, *PartitionPolicy) {
	populationIndex := sort.Search(len(suite.Populations), func(index int) bool {
		return suite.Populations[index].Condition >= key.Condition
	})
	var conditionPolicy *PartitionPolicy
	if populationIndex < len(suite.Populations) &&
		suite.Populations[populationIndex].Condition == key.Condition {
		conditionPolicy = suite.Populations[populationIndex].Policy
	}
	caseIndex := sort.Search(len(suite.Cases), func(index int) bool {
		caseSpec := suite.Cases[index]
		return caseSpec.Condition > key.Condition ||
			(caseSpec.Condition == key.Condition && caseSpec.ID >= key.Case)
	})
	var casePolicy *PartitionPolicy
	if caseIndex < len(suite.Cases) {
		caseSpec := suite.Cases[caseIndex]
		if caseSpec.Condition == key.Condition && caseSpec.ID == key.Case {
			casePolicy = caseSpec.Policy
		}
	}
	return conditionPolicy, casePolicy
}

func indexedScope(parent, collection string, index int) string {
	var digits [20]byte
	formatted := strconv.AppendInt(digits[:0], int64(index), 10)
	var result strings.Builder
	result.Grow(len(parent) + len(collection) + len(formatted) + 2)
	result.WriteString(parent)
	result.WriteByte('/')
	result.WriteString(collection)
	result.WriteByte('/')
	result.Write(formatted)
	return result.String()
}

func validatePair(suite SuiteSpec, baseline, candidate Attempt) []Finding {
	var findings []Finding
	scope := "pair/" + keyScope(baseline.Key)
	checkApplicability := func(name string, left, right OutcomeState) {
		if (left == OutcomeNotApplicable) != (right == OutcomeNotApplicable) {
			findings = append(findings, Finding{Code: "pair.outcome_applicability", Scope: scope,
				Message: fmt.Sprintf("%s applicability differs between baseline and candidate", name)})
		}
	}
	checkApplicability("interaction", baseline.Outcomes.Interaction, candidate.Outcomes.Interaction)
	checkApplicability("deadline", baseline.Outcomes.Deadline, candidate.Outcomes.Deadline)
	checkApplicability("safety", baseline.Outcomes.Safety, candidate.Outcomes.Safety)
	for _, policy := range suite.Policy.Latencies {
		_, baselinePresent := latencyValue(baseline, policy.Name)
		_, candidatePresent := latencyValue(candidate, policy.Name)
		if baselinePresent != candidatePresent {
			findings = append(findings, Finding{Code: "pair.latency_applicability", Scope: scope,
				Message: fmt.Sprintf("latency %q is present in only one arm", policy.Name)})
		}
	}
	return findings
}

func sortedKeys(expected map[AttemptKey]bool) []AttemptKey {
	keys := make([]AttemptKey, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool { return lessKey(keys[left], keys[right]) })
	return keys
}

func matchedAttempt(baseline, candidate Attempt) MatchedAttempt {
	result := MatchedAttempt{
		Key: baseline.Key, BaselineID: baseline.ID, CandidateID: candidate.ID,
		BaselinePass: baseline.Passed, CandidatePass: candidate.Passed,
		Interaction: StateTransition{Baseline: baseline.Outcomes.Interaction,
			Candidate: candidate.Outcomes.Interaction},
		Deadline: StateTransition{Baseline: baseline.Outcomes.Deadline,
			Candidate: candidate.Outcomes.Deadline},
		Safety: StateTransition{Baseline: baseline.Outcomes.Safety,
			Candidate: candidate.Outcomes.Safety},
		Latencies: make([]PairedLatency, 0),
	}
	for _, latency := range baseline.Latencies {
		candidateValue, present := latencyValue(candidate, latency.Name)
		if !present {
			continue
		}
		result.Latencies = append(result.Latencies, PairedLatency{
			Name: latency.Name, Unit: latency.Unit, Baseline: latency.Value,
			Candidate: candidateValue, Difference: candidateValue - latency.Value,
		})
	}
	sort.Slice(result.Latencies, func(left, right int) bool {
		return result.Latencies[left].Name < result.Latencies[right].Name
	})
	return result
}

func latencyValue(attempt Attempt, name string) (float64, bool) {
	for _, latency := range attempt.Latencies {
		if latency.Name == name {
			return latency.Value, true
		}
	}
	return 0, false
}

func compareSuites(manifest Manifest, matched []MatchedAttempt) []SuiteComparison {
	bySuite := make(map[string][]MatchedAttempt, len(manifest.Suites))
	for _, pair := range matched {
		bySuite[pair.Key.Suite] = append(bySuite[pair.Key.Suite], pair)
	}
	result := make([]SuiteComparison, 0, len(manifest.Suites))
	for _, suite := range manifest.Suites {
		result = append(result, compareSuite(suite, bySuite[suite.Name]))
	}
	return result
}

func compareSuite(suite SuiteSpec, pairs []MatchedAttempt) SuiteComparison {
	result := SuiteComparison{Suite: suite.Name, Accepted: true,
		Latencies:  make([]LatencyComparison, 0, len(suite.Policy.Latencies)),
		Conditions: make([]ConditionComparison, 0, len(suite.Populations)),
		Cases:      make([]CaseComparison, 0),
		Gates:      make([]GateResult, 0)}
	passPolicy := suite.Policy.Pass
	result.Pass = compareRate(suite.Name, "pass", pairs, func(pair MatchedAttempt) (bool, bool, bool) {
		return pair.BaselinePass, pair.CandidatePass, true
	}, &passPolicy, suite.Policy.Inference)
	result.PassFloor = comparePassFloor(suite.Name, result.Pass)
	result.Interaction = compareRate(suite.Name, "interaction", pairs,
		stateSelector(func(pair MatchedAttempt) StateTransition { return pair.Interaction }),
		suite.Policy.Interaction.NonInferiority, suite.Policy.Inference)
	result.Deadline = compareRate(suite.Name, "deadline", pairs,
		stateSelector(func(pair MatchedAttempt) StateTransition { return pair.Deadline }),
		suite.Policy.Deadline.NonInferiority, suite.Policy.Inference)
	result.Safety = compareSafety(suite, pairs)

	for _, comparison := range []*RateComparison{&result.Pass, &result.Interaction, &result.Deadline} {
		if comparison.Gate != nil {
			result.Gates = append(result.Gates, *comparison.Gate)
		}
	}
	result.Gates = append(result.Gates, result.PassFloor)
	result.Gates = append(result.Gates, result.Safety.Gate)
	for _, policy := range suite.Policy.Latencies {
		comparison := compareLatency(suite, policy, pairs)
		for _, limit := range comparison.Limits {
			result.Gates = append(result.Gates, limit.Gate)
		}
		result.Latencies = append(result.Latencies, comparison)
	}
	byCondition := make(map[string][]MatchedAttempt, len(suite.Populations))
	for _, pair := range pairs {
		byCondition[pair.Key.Condition] = append(byCondition[pair.Key.Condition], pair)
	}
	for _, population := range suite.Populations {
		comparison := compareCondition(suite, population, byCondition[population.Condition])
		result.Conditions = append(result.Conditions, comparison)
		result.Gates = append(result.Gates, comparison.Gates...)
	}
	caseComparisonCount := 0
	for _, caseSpec := range suite.Cases {
		if caseSpec.Policy != nil || caseSpec.RequirePolicy {
			caseComparisonCount++
		}
	}
	if caseComparisonCount > 0 {
		result.Cases = make([]CaseComparison, 0, caseComparisonCount)
		byCase := make(map[string][]MatchedAttempt, caseComparisonCount)
		for _, pair := range pairs {
			identity := pair.Key.Condition + "\x00" + pair.Key.Case
			byCase[identity] = append(byCase[identity], pair)
		}
		for _, caseSpec := range suite.Cases {
			if caseSpec.Policy == nil && !caseSpec.RequirePolicy {
				continue
			}
			identity := caseSpec.Condition + "\x00" + caseSpec.ID
			comparison := compareCase(suite, caseSpec, byCase[identity])
			result.Cases = append(result.Cases, comparison)
			result.Gates = append(result.Gates, comparison.Gates...)
		}
	}
	for _, gate := range result.Gates {
		if gate.Evaluated && !gate.Passed {
			result.Accepted = false
		}
	}
	return result
}

func comparePassFloor(suiteName string, pass RateComparison) GateResult {
	gate := GateResult{
		Name:   pathIdentity(suiteName, "pass", "absolute_sanity_floor"),
		Passed: true,
	}
	if pass.ApplicableAttempts == 0 {
		gate.Reason = "the pass-rate population is empty"
		return gate
	}
	if pass.BaselineRate <= CandidatePassRateSanityFloor {
		gate.Reason = fmt.Sprintf(
			"baseline pass rate %.6f does not exceed the %.2f sanity-floor trigger",
			pass.BaselineRate, CandidatePassRateSanityFloor)
		return gate
	}
	gate.Evaluated = true
	gate.Passed = pass.CandidateRate >= CandidatePassRateSanityFloor
	if !gate.Passed {
		gate.Reason = fmt.Sprintf(
			"candidate pass rate %.6f is below the additional %.2f sanity floor",
			pass.CandidateRate, CandidatePassRateSanityFloor)
	}
	return gate
}

func compareCondition(
	suite SuiteSpec, population Population, pairs []MatchedAttempt,
) ConditionComparison {
	namespace := pathIdentity(suite.Name, "condition", population.Condition)
	comparison := comparePartition(suite, namespace, population.Policy, pairs)
	return ConditionComparison{
		Condition: population.Condition, Gated: population.Policy != nil,
		Accepted: comparison.accepted,
		Pass:     comparison.pass, Interaction: comparison.interaction,
		Deadline: comparison.deadline, Safety: comparison.safety,
		Latencies: comparison.latencies, Gates: comparison.gates,
	}
}

func compareCase(suite SuiteSpec, caseSpec CaseSpec, pairs []MatchedAttempt) CaseComparison {
	namespace := pathIdentity(suite.Name, "case", caseSpec.Condition, caseSpec.ID)
	comparison := comparePartition(suite, namespace, caseSpec.Policy, pairs)
	return CaseComparison{
		Condition: caseSpec.Condition, Case: caseSpec.ID, Gated: caseSpec.Policy != nil,
		Accepted: comparison.accepted,
		Pass:     comparison.pass, Interaction: comparison.interaction,
		Deadline: comparison.deadline, Safety: comparison.safety,
		Latencies: comparison.latencies, Gates: comparison.gates,
	}
}

type partitionComparison struct {
	accepted    bool
	pass        RateComparison
	interaction RateComparison
	deadline    RateComparison
	safety      SafetyComparison
	latencies   []LatencyComparison
	gates       []GateResult
}

func comparePartition(
	suite SuiteSpec, namespace string, policy *PartitionPolicy, pairs []MatchedAttempt,
) partitionComparison {
	result := partitionComparison{accepted: true,
		latencies: make([]LatencyComparison, 0, len(suite.Policy.Latencies)),
		gates:     make([]GateResult, 0)}
	var passPolicy *RatePolicy
	var interactionPolicy, deadlinePolicy *RatePolicy
	latencyPolicies := make(map[string]LatencyPolicy)
	if policy != nil {
		pass := policy.Pass
		passPolicy = &pass
		interactionPolicy = policy.Interaction.NonInferiority
		deadlinePolicy = policy.Deadline.NonInferiority
		for _, latency := range policy.Latencies {
			latencyPolicies[latency.Name] = latency
		}
	}
	result.pass = compareRate(namespace, "pass", pairs,
		func(pair MatchedAttempt) (bool, bool, bool) {
			return pair.BaselinePass, pair.CandidatePass, true
		}, passPolicy, suite.Policy.Inference)
	result.interaction = compareRate(namespace, "interaction", pairs,
		stateSelector(func(pair MatchedAttempt) StateTransition { return pair.Interaction }),
		interactionPolicy, suite.Policy.Inference)
	result.deadline = compareRate(namespace, "deadline", pairs,
		stateSelector(func(pair MatchedAttempt) StateTransition { return pair.Deadline }),
		deadlinePolicy, suite.Policy.Inference)
	partitionSuite := suite
	partitionSuite.Name = namespace
	result.safety = compareSafety(partitionSuite, pairs)
	for _, suiteLatency := range suite.Policy.Latencies {
		partitionLatency, gated := latencyPolicies[suiteLatency.Name]
		if !gated {
			partitionLatency = suiteLatency
			partitionLatency.Gate = nil
		}
		comparison := compareLatency(partitionSuite, partitionLatency, pairs)
		result.latencies = append(result.latencies, comparison)
	}
	if policy == nil {
		return result
	}
	for _, comparison := range []*RateComparison{&result.pass, &result.interaction, &result.deadline} {
		if comparison.Gate != nil {
			result.gates = append(result.gates, *comparison.Gate)
		}
	}
	result.gates = append(result.gates, result.safety.Gate)
	for _, latency := range result.latencies {
		for _, limit := range latency.Limits {
			result.gates = append(result.gates, limit.Gate)
		}
	}
	for _, gate := range result.gates {
		if gate.Evaluated && !gate.Passed {
			result.accepted = false
		}
	}
	return result
}

type rateSelector func(MatchedAttempt) (baseline, candidate, applicable bool)

func stateSelector(selectState func(MatchedAttempt) StateTransition) rateSelector {
	return func(pair MatchedAttempt) (bool, bool, bool) {
		states := selectState(pair)
		if states.Baseline == OutcomeNotApplicable {
			return false, false, false
		}
		return states.Baseline == OutcomeSatisfied, states.Candidate == OutcomeSatisfied, true
	}
}

func compareRate(
	suiteName, name string, pairs []MatchedAttempt, selectRate rateSelector,
	policy *RatePolicy, inference BootstrapPolicy,
) RateComparison {
	result := RateComparison{Name: name, Policy: cloneRatePolicy(policy)}
	caseSet := make(map[string]bool)
	var observations []binaryObservation
	for _, pair := range pairs {
		baseline, candidate, applicable := selectRate(pair)
		if !applicable {
			result.Transitions.NotApplicable++
			continue
		}
		cluster := pair.Key.Condition + "\x00" + pair.Key.Case
		caseSet[cluster] = true
		observations = append(observations, binaryObservation{
			cluster: cluster, baseline: baseline, candidate: candidate,
		})
		if baseline {
			result.BaselineSuccesses++
		}
		if candidate {
			result.CandidateSuccesses++
		}
		addTransition(&result.Transitions, baseline, candidate)
	}
	result.ApplicableAttempts = len(observations)
	result.ApplicableCases = len(caseSet)
	if len(observations) > 0 {
		result.BaselineRate = float64(result.BaselineSuccesses) / float64(len(observations))
		result.CandidateRate = float64(result.CandidateSuccesses) / float64(len(observations))
		result.Difference = result.CandidateRate - result.BaselineRate
	}
	if policy == nil {
		return result
	}
	gate := GateResult{Name: pathIdentity(suiteName, name, "non_inferiority"), Evaluated: true}
	switch {
	case len(observations) < policy.MinimumAttempts:
		gate.Reason = fmt.Sprintf("%d applicable attempts are below the predeclared minimum %d",
			len(observations), policy.MinimumAttempts)
	case len(caseSet) < policy.MinimumCases:
		gate.Reason = fmt.Sprintf("%d applicable cases are below the predeclared minimum %d",
			len(caseSet), policy.MinimumCases)
	default:
		bound := binaryLowerBound(observations, inference,
			canonicalJSON([]string{"rate", suiteName, name}))
		result.LowerBound = &bound
		gate.Passed = bound.Value >= -policy.Margin
		if !gate.Passed {
			gate.Reason = fmt.Sprintf("lower confidence bound %.6f is below non-inferiority threshold %.6f",
				bound.Value, -policy.Margin)
		}
	}
	result.Gate = &gate
	return result
}

func compareSafety(suite SuiteSpec, pairs []MatchedAttempt) SafetyComparison {
	result := SafetyComparison{Gate: GateResult{
		Name: pathIdentity(suite.Name, "safety", "zero_tolerance"),
	}}
	caseSet := make(map[string]bool)
	for _, pair := range pairs {
		if pair.Safety.Baseline == OutcomeNotApplicable {
			result.Transitions.NotApplicable++
			continue
		}
		result.ApplicableAttempts++
		caseSet[pair.Key.Condition+"\x00"+pair.Key.Case] = true
		baselineSafe := pair.Safety.Baseline == OutcomeSatisfied
		candidateSafe := pair.Safety.Candidate == OutcomeSatisfied
		if !baselineSafe {
			result.BaselineViolations++
		}
		if !candidateSafe {
			result.CandidateViolations++
		}
		addTransition(&result.Transitions, baselineSafe, candidateSafe)
	}
	result.ApplicableCases = len(caseSet)
	result.Gate.Evaluated = suite.Policy.Safety.Required || result.ApplicableAttempts > 0
	result.Gate.Passed = result.CandidateViolations == 0
	if !result.Gate.Evaluated {
		result.Gate.Passed = true
		result.Gate.Reason = "the suite declared no safety-applicable attempts"
	} else if !result.Gate.Passed {
		result.Gate.Reason = fmt.Sprintf("candidate recorded %d safety violation(s); tolerance is zero",
			result.CandidateViolations)
	}
	return result
}

func compareLatency(suite SuiteSpec, policy LatencyPolicy, pairs []MatchedAttempt) LatencyComparison {
	result := LatencyComparison{Name: policy.Name, Unit: policy.Unit,
		Limits: make([]LatencyLimitResult, 0)}
	var observations []latencyObservation
	var baselineValues, candidateValues, differences []float64
	caseSet := make(map[string]bool)
	for _, pair := range pairs {
		var reading *PairedLatency
		for index := range pair.Latencies {
			if pair.Latencies[index].Name == policy.Name {
				reading = &pair.Latencies[index]
				break
			}
		}
		if reading == nil {
			continue
		}
		cluster := pair.Key.Condition + "\x00" + pair.Key.Case
		caseSet[cluster] = true
		observations = append(observations, latencyObservation{
			cluster: cluster, baseline: reading.Baseline, candidate: reading.Candidate,
		})
		baselineValues = append(baselineValues, reading.Baseline)
		candidateValues = append(candidateValues, reading.Candidate)
		differences = append(differences, reading.Difference)
	}
	result.ApplicableCases = len(caseSet)
	result.Baseline = distribution(baselineValues, policy.Unit)
	result.Candidate = distribution(candidateValues, policy.Unit)
	result.PairedDifference = distribution(differences, policy.Unit)
	if policy.Gate == nil {
		return result
	}
	statistics := make([]LatencyStatistic, 0, len(policy.Gate.Limits))
	for _, limit := range policy.Gate.Limits {
		statistics = append(statistics, limit.Statistic)
	}
	var bounds map[LatencyStatistic]ConfidenceBound
	if len(observations) >= policy.Gate.MinimumAttempts &&
		len(caseSet) >= policy.Gate.MinimumCases {
		bounds = latencyUpperBounds(observations, statistics, suite.Policy.Inference,
			canonicalJSON([]string{"latency", suite.Name, policy.Name}))
	}
	for _, limit := range policy.Gate.Limits {
		limitResult := LatencyLimitResult{
			Statistic: limit.Statistic, MaximumIncrease: limit.MaximumIncrease,
			ObservedIncrease: statistic(candidateValues, limit.Statistic) -
				statistic(baselineValues, limit.Statistic),
			Gate: GateResult{Name: pathIdentity(suite.Name, "latency", policy.Name,
				string(limit.Statistic)), Evaluated: true},
		}
		switch {
		case len(observations) < policy.Gate.MinimumAttempts:
			limitResult.Gate.Reason = fmt.Sprintf(
				"%d paired latency observations are below the predeclared minimum %d",
				len(observations), policy.Gate.MinimumAttempts)
		case len(caseSet) < policy.Gate.MinimumCases:
			limitResult.Gate.Reason = fmt.Sprintf(
				"%d paired latency cases are below the predeclared minimum %d",
				len(caseSet), policy.Gate.MinimumCases)
		default:
			bound := bounds[limit.Statistic]
			limitResult.UpperBound = &bound
			limitResult.Gate.Passed = bound.Value <= limit.MaximumIncrease
			if !limitResult.Gate.Passed {
				limitResult.Gate.Reason = fmt.Sprintf(
					"upper confidence bound %.6f exceeds maximum increase %.6f",
					bound.Value, limit.MaximumIncrease)
			}
		}
		result.Limits = append(result.Limits, limitResult)
	}
	return result
}

func addTransition(counts *TransitionCounts, baseline, candidate bool) {
	switch {
	case baseline && candidate:
		counts.SuccessToSuccess++
	case baseline && !candidate:
		counts.SuccessToFailure++
	case !baseline && candidate:
		counts.FailureToSuccess++
	default:
		counts.FailureToFailure++
	}
}

func reportDigest(report Report) string {
	report.ReportID = ""
	return digestJSON(report)
}
