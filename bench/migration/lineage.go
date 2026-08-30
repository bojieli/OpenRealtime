package migration

import (
	"fmt"
	"sort"
)

// Reference returns the canonical identities a later campaign run must
// predeclare for this report.
func (report Report) Reference() (ReportReference, error) {
	if err := report.Verify(); err != nil {
		return ReportReference{}, err
	}
	return archiveReference(report)
}

// ReferenceWithHistory returns a content-bound reference after proving the
// report's complete transitive campaign history.
func (report Report) ReferenceWithHistory(history []Report) (ReportReference, error) {
	if err := report.VerifyWithHistory(history); err != nil {
		return ReportReference{}, err
	}
	return archiveReference(report)
}

func archiveReference(report Report) (ReportReference, error) {
	reference := reportReference(report)
	if !validSHA256(reference.ArtifactSHA256) {
		return ReportReference{}, fmt.Errorf(
			"migration report %q cannot be represented as a canonical artifact",
			report.Manifest.Campaign.RunID)
	}
	return reference, nil
}

func reportReference(report Report) ReportReference {
	return ReportReference{
		RunID: report.Manifest.Campaign.RunID, ReportID: report.ReportID,
		ArtifactSHA256: report.ArtifactSHA256(),
	}
}

func historyReferences(history []Report) []ReportReference {
	result := make([]ReportReference, len(history))
	for index, report := range history {
		result[index] = reportReference(report)
	}
	return canonicalReportReferences(result)
}

func validateLineage(manifest Manifest, history []Report) []Finding {
	if findings := validateLineageBounds(manifest, history); len(findings) > 0 {
		sortFindings(findings)
		return findings
	}
	history = canonicalHistory(history)
	validity := validateHistoryLocally(history)
	findings := validateLineageShallowCached(manifest, history, validity)
	findings = append(findings, validateRetainedHistory(history, validity)...)
	sortFindings(findings)
	return findings
}

func validateHistoryLocally(history []Report) map[string]error {
	result := make(map[string]error, len(history))
	for _, report := range history {
		err := report.verifyLocal()
		runID := report.Manifest.Campaign.RunID
		if previous, exists := result[runID]; !exists || (previous == nil && err != nil) {
			result[runID] = err
		}
	}
	return result
}

func canonicalHistory(history []Report) []Report {
	result := append(make([]Report, 0, len(history)), history...)
	sort.Slice(result, func(left, right int) bool {
		leftRun := result[left].Manifest.Campaign.RunID
		rightRun := result[right].Manifest.Campaign.RunID
		if leftRun != rightRun {
			return leftRun < rightRun
		}
		if result[left].ReportID != result[right].ReportID {
			return result[left].ReportID < result[right].ReportID
		}
		return digestJSON(result[left]) < digestJSON(result[right])
	})
	return result
}

func validateLineageBounds(manifest Manifest, history []Report) []Finding {
	var findings []Finding
	if len(history) > maxCampaignPredecessors {
		findings = append(findings, Finding{Code: "lineage.history_limit", Scope: "lineage/history",
			Message: fmt.Sprintf("supplied history exceeds the limit of %d reports", maxCampaignPredecessors)})
	}
	if len(manifest.Campaign.Predecessors) > maxCampaignPredecessors {
		findings = append(findings, Finding{Code: "lineage.predecessor_limit", Scope: "lineage/history",
			Message: fmt.Sprintf("declared history exceeds the limit of %d reports", maxCampaignPredecessors)})
	}
	totalAttempts := 0
	totalBootstrapWork := uint64(0)
	for _, predecessor := range history {
		if len(predecessor.Attempts) > maxHistoryAttempts-totalAttempts {
			findings = append(findings, Finding{Code: "lineage.attempt_limit", Scope: "lineage/history",
				Message: fmt.Sprintf("retained history exceeds the limit of %d observed attempts",
					maxHistoryAttempts)})
			break
		}
		totalAttempts += len(predecessor.Attempts)
		work, overflow := manifestBootstrapWorkUnits(predecessor.Manifest)
		if overflow || work > maxHistoryBootstrapWorkUnits-totalBootstrapWork {
			findings = append(findings, Finding{Code: "lineage.bootstrap_work_limit", Scope: "lineage/history",
				Message: fmt.Sprintf("retained history exceeds the verification limit of %d bootstrap work units",
					maxHistoryBootstrapWorkUnits)})
			break
		}
		totalBootstrapWork += work
	}
	return findings
}

// validateLineageShallowCached derives the lineage state for one report. The
// outer validator applies it independently to every retained predecessor, so
// a predecessor cannot smuggle in invented lineage refusals or accepted state.
func validateLineageShallowCached(
	manifest Manifest, history []Report, validity map[string]error,
) []Finding {
	var findings []Finding
	add := func(code, scope, message string) {
		findings = append(findings, Finding{Code: code, Scope: scope, Message: message})
	}
	expectedByRun := make(map[string]ReportReference, len(manifest.Campaign.Predecessors))
	for _, reference := range manifest.Campaign.Predecessors {
		expectedByRun[reference.RunID] = reference
	}
	actualByRun := make(map[string]Report, len(history))
	actualByID := make(map[string]Report, len(history))
	referenceByRun := make(map[string]ReportReference, len(history))
	for index, predecessor := range history {
		scope := fmt.Sprintf("lineage/history/%d", index)
		runID := predecessor.Manifest.Campaign.RunID
		if _, exists := actualByRun[runID]; exists {
			add("lineage.run_duplicate", scope, fmt.Sprintf("history run %q is duplicated", runID))
		}
		if _, exists := actualByID[predecessor.ReportID]; exists {
			add("lineage.report_duplicate", scope, fmt.Sprintf(
				"history report %q is duplicated", predecessor.ReportID))
		}
		actualByRun[runID] = predecessor
		actualByID[predecessor.ReportID] = predecessor
		referenceByRun[runID] = reportReference(predecessor)
		if err := validity[runID]; err != nil {
			add("lineage.predecessor_invalid", scope,
				"predecessor report is not internally valid: "+err.Error())
		}
		if predecessor.Manifest.Campaign.CampaignID != manifest.Campaign.CampaignID {
			add("lineage.campaign_mismatch", scope, fmt.Sprintf(
				"predecessor belongs to campaign %q, expected %q",
				predecessor.Manifest.Campaign.CampaignID, manifest.Campaign.CampaignID))
		}
		findings = append(findings, validateStudyContinuity(
			manifest, predecessor.Manifest, scope)...)
		expected, declared := expectedByRun[runID]
		if !declared {
			add("lineage.predecessor_unexpected", scope, fmt.Sprintf(
				"history run %q is not predeclared", runID))
			continue
		}
		actual := referenceByRun[runID]
		if actual.ReportID != expected.ReportID {
			add("lineage.report_forged", scope, fmt.Sprintf(
				"run %q report ID is %q, expected %q", runID, actual.ReportID, expected.ReportID))
		}
		if actual.ArtifactSHA256 != expected.ArtifactSHA256 {
			add("lineage.artifact_forged", scope, fmt.Sprintf(
				"run %q artifact digest is %q, expected %q",
				runID, actual.ArtifactSHA256, expected.ArtifactSHA256))
		}
	}
	for runID := range expectedByRun {
		if _, exists := actualByRun[runID]; !exists {
			add("lineage.predecessor_missing", "lineage/history", fmt.Sprintf(
				"predeclared predecessor run %q was not supplied", runID))
		}
	}

	// Every predecessor list is a complete transitive history. Validate its
	// references against the same supplied immutable reports before checking the
	// graph for cycles.
	edges := make(map[string][]string, len(history)+1)
	currentRun := manifest.Campaign.RunID
	for _, reference := range manifest.Campaign.Predecessors {
		edges[currentRun] = append(edges[currentRun], reference.RunID)
	}
	for _, predecessor := range history {
		from := predecessor.Manifest.Campaign.RunID
		for _, reference := range predecessor.Manifest.Campaign.Predecessors {
			edges[from] = append(edges[from], reference.RunID)
			if reference.RunID == currentRun {
				add("lineage.cycle", pathIdentity("lineage", from),
					"a predecessor history points back to the current run")
				continue
			}
			target, exists := actualByRun[reference.RunID]
			if !exists {
				add("lineage.history_incomplete", pathIdentity("lineage", from), fmt.Sprintf(
					"transitive predecessor run %q was not supplied", reference.RunID))
				continue
			}
			actual := referenceByRun[target.Manifest.Campaign.RunID]
			if actual.ReportID != reference.ReportID ||
				actual.ArtifactSHA256 != reference.ArtifactSHA256 {
				add("lineage.history_reference_forged", pathIdentity("lineage", from), fmt.Sprintf(
					"transitive reference to run %q does not match its supplied report", reference.RunID))
			}
		}
	}
	if hasLineageCycle(currentRun, edges) {
		add("lineage.cycle", "lineage", "campaign predecessor graph contains a cycle")
	}

	for index, failure := range manifest.Campaign.DiagnosedFailures {
		scope := fmt.Sprintf("lineage/diagnosed_failures/%d", index)
		predecessor, exists := actualByID[failure.ReportID]
		if !exists {
			add("lineage.failure_report_missing", scope,
				"diagnosed failure report was not supplied in campaign history")
			continue
		}
		if failure.Gate != "" && !containsFailedGate(predecessor, failure.Gate) {
			add("lineage.failure_gate_forged", scope, fmt.Sprintf(
				"report %q does not contain failed evaluated gate %q", failure.ReportID, failure.Gate))
		}
		if failure.FindingCode != "" && !containsFinding(predecessor, failure.FindingCode) {
			add("lineage.failure_finding_forged", scope, fmt.Sprintf(
				"report %q does not contain refusal %q", failure.ReportID, failure.FindingCode))
		}
	}
	if manifest.Campaign.Kind == RunDiagnostic &&
		len(manifest.Campaign.DiagnosedFailures) == 0 {
		add("lineage.diagnostic_target_missing", "lineage/diagnosed_failures",
			"a diagnostic rerun must name the failure it diagnoses")
	}
	if manifest.Campaign.Kind == RunFull {
		for _, predecessor := range history {
			if hasObservedFailure(predecessor) {
				declared := diagnosedFailureSet(manifest.Campaign.DiagnosedFailures, predecessor.ReportID)
				for _, failure := range observedFailures(predecessor) {
					if declared[failureSelector(failure)] {
						continue
					}
					selector := "finding " + failure.FindingCode
					if failure.Gate != "" {
						selector = "gate " + failure.Gate
					}
					add("lineage.failed_report_not_superseded", "lineage/diagnosed_failures", fmt.Sprintf(
						"failed predecessor report %q %s is not named by a diagnosed failure reference",
						predecessor.ReportID, selector))
				}
			}
			findings = append(findings, validateRerunCoverage(manifest, predecessor)...)
		}
	}
	sortFindings(findings)
	return findings
}

func diagnosedFailureSet(failures []FailureReference, reportID string) map[string]bool {
	result := make(map[string]bool)
	for _, failure := range failures {
		if failure.ReportID == reportID {
			result[failureSelector(failure)] = true
		}
	}
	return result
}

func observedFailures(report Report) []FailureReference {
	seen := make(map[string]bool)
	var result []FailureReference
	for _, finding := range report.Refusals {
		failure := FailureReference{ReportID: report.ReportID, FindingCode: finding.Code}
		identity := failureSelector(failure)
		if !seen[identity] {
			seen[identity] = true
			result = append(result, failure)
		}
	}
	for _, suite := range report.Comparisons {
		for _, gate := range suite.Gates {
			if !gate.Evaluated || gate.Passed {
				continue
			}
			failure := FailureReference{ReportID: report.ReportID, Gate: gate.Name}
			identity := failureSelector(failure)
			if !seen[identity] {
				seen[identity] = true
				result = append(result, failure)
			}
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return failureSelector(result[left]) < failureSelector(result[right])
	})
	return result
}

func failureSelector(failure FailureReference) string {
	if failure.Gate != "" {
		return "gate\x00" + failure.Gate
	}
	return "finding\x00" + failure.FindingCode
}

func validateStudyContinuity(current, predecessor Manifest, scope string) []Finding {
	var findings []Finding
	add := func(code, message string) {
		findings = append(findings, Finding{Code: code, Scope: scope, Message: message})
	}
	if current.Baseline != predecessor.Baseline || current.Candidate != predecessor.Candidate {
		add("lineage.arm_changed", "baseline or candidate arm identity changed within the campaign")
	}
	if canonicalJSON(canonicalManifest(current).FixedAxes) !=
		canonicalJSON(canonicalManifest(predecessor).FixedAxes) {
		add("lineage.fixed_axes_changed", "fixed provenance axes changed within the campaign")
	}
	treatmentContract := func(manifest Manifest) []TreatmentDelta {
		canonical := canonicalManifest(manifest).Treatment
		result := make([]TreatmentDelta, len(canonical))
		for index, delta := range canonical {
			result[index] = TreatmentDelta{Axis: delta.Axis, Baseline: delta.Baseline}
		}
		return result
	}
	if canonicalJSON(treatmentContract(current)) != canonicalJSON(treatmentContract(predecessor)) {
		add("lineage.treatment_contract_changed",
			"treatment axes or baseline treatment values changed within the campaign")
	}
	if current.Campaign.Kind == RunFull && predecessor.Campaign.Kind == RunFull &&
		hasValidFrozenSuitePlan(predecessor) {
		currentSuites := canonicalManifest(current).Suites
		predecessorSuites := canonicalManifest(predecessor).Suites
		if canonicalJSON(currentSuites) != canonicalJSON(predecessorSuites) {
			add("lineage.full_plan_changed",
				"a full rerun changed the frozen suite population or acceptance policy")
		}
	}
	return findings
}

// hasValidFrozenSuitePlan distinguishes an observed failure under a valid,
// predeclared study from a refusal caused by a malformed study definition. A
// valid full-run plan is immutable for every later full rerun. A malformed
// plan must be repairable (for example, by adding the required repetitions),
// while the independently checked arm, fixed-axis, and baseline-treatment
// contracts remain frozen.
func hasValidFrozenSuitePlan(manifest Manifest) bool {
	canonical := canonicalManifest(manifest)
	if len(canonical.Suites) == 0 || len(canonical.Suites) > maxManifestSuites {
		return false
	}
	seen := make(map[string]bool, len(canonical.Suites))
	for index, suite := range canonical.Suites {
		var nameFindings []Finding
		validateCanonicalText(&nameFindings, "manifest.suite_name",
			fmt.Sprintf("manifest/suites/%d", index), "suite name", suite.Name)
		if len(nameFindings) > 0 || seen[suite.Name] {
			return false
		}
		seen[suite.Name] = true
		if len(validateSuite(suite, fmt.Sprintf("manifest/suites/%d", index))) > 0 {
			return false
		}
	}
	return true
}

func validateRetainedHistory(history []Report, validity map[string]error) []Finding {
	actualByRun := make(map[string]Report, len(history))
	for _, report := range history {
		actualByRun[report.Manifest.Campaign.RunID] = report
	}
	var findings []Finding
	for index, report := range history {
		if err := validity[report.Manifest.Campaign.RunID]; err != nil {
			// validateLineageShallowCached already emits the more specific invalid-report
			// finding. Avoid comparing an object that has no valid local derivation.
			continue
		}
		invocationHistory := make([]Report, 0, len(report.SuppliedHistory))
		for _, reference := range report.SuppliedHistory {
			if ancestor, exists := actualByRun[reference.RunID]; exists {
				invocationHistory = append(invocationHistory, ancestor)
			}
		}
		expected := rebuildWithLineage(report.Manifest, report.Attempts, invocationHistory, validity)
		if canonicalJSON(expected) != canonicalJSON(report) {
			findings = append(findings, Finding{
				Code: "lineage.predecessor_state_forged", Scope: fmt.Sprintf("lineage/history/%d", index),
				Message: fmt.Sprintf("predecessor run %q does not match its declared retained history",
					report.Manifest.Campaign.RunID),
			})
		}
	}
	return findings
}

func rebuildWithLineage(
	manifest Manifest, observed []ObservedAttempt, history []Report, validity map[string]error,
) Report {
	var baseline, candidate []Attempt
	for _, row := range observed {
		switch row.Arm {
		case ArmBaseline:
			baseline = append(baseline, row.Attempt)
		case ArmCandidate:
			candidate = append(candidate, row.Attempt)
		}
	}
	rebuilt := compareCore(manifest, baseline, candidate)
	rebuilt.SuppliedHistory = historyReferences(history)
	lineage := validateLineageShallowCached(rebuilt.Manifest, history, validity)
	if len(lineage) > 0 {
		rebuilt.Refusals = append(rebuilt.Refusals, lineage...)
		sortFindings(rebuilt.Refusals)
		rebuilt.Reportable = false
		rebuilt.Accepted = false
		rebuilt.Matched = make([]MatchedAttempt, 0)
		rebuilt.Comparisons = make([]SuiteComparison, 0)
	}
	applyCampaignGate(&rebuilt)
	rebuilt.ReportID = reportDigest(rebuilt)
	return rebuilt
}

func validateRerunCoverage(current Manifest, predecessor Report) []Finding {
	if !hasObservedFailure(predecessor) {
		return nil
	}
	currentSuites := make(map[string]SuiteSpec, len(current.Suites))
	for _, suite := range current.Suites {
		currentSuites[suite.Name] = suite
	}
	var findings []Finding
	for _, name := range affectedSuites(predecessor) {
		previousSuite, _ := suiteNamed(predecessor.Manifest, name)
		currentSuite, exists := currentSuites[name]
		scope := pathIdentity("lineage", "rerun", predecessor.ReportID, name)
		if !exists {
			findings = append(findings, Finding{Code: "lineage.affected_suite_missing", Scope: scope,
				Message: fmt.Sprintf("full rerun omits affected suite %q", name)})
			continue
		}
		if predecessor.Manifest.Campaign.Kind == RunFull &&
			hasValidFrozenSuitePlan(predecessor.Manifest) {
			if canonicalJSON(canonicalSuite(currentSuite)) != canonicalJSON(canonicalSuite(previousSuite)) {
				findings = append(findings, Finding{Code: "lineage.affected_suite_changed", Scope: scope,
					Message: fmt.Sprintf(
						"full rerun changed the frozen population or policy of affected suite %q", name)})
			}
			continue
		}
		currentKeys := suiteKeys(currentSuite)
		for key := range suiteKeys(previousSuite) {
			if !currentKeys[key] {
				findings = append(findings, Finding{Code: "lineage.diagnostic_case_missing", Scope: scope,
					Message: fmt.Sprintf("full rerun omits diagnosed attempt %s", keyScope(key))})
			}
		}
	}
	return findings
}

func affectedSuites(report Report) []string {
	seen := make(map[string]bool)
	if len(report.Refusals) > 0 {
		for _, suite := range report.Manifest.Suites {
			seen[suite.Name] = true
		}
	} else {
		for _, comparison := range report.Comparisons {
			for _, gate := range comparison.Gates {
				if gate.Evaluated && !gate.Passed {
					seen[comparison.Suite] = true
					break
				}
			}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func suiteNamed(manifest Manifest, name string) (SuiteSpec, bool) {
	for _, suite := range manifest.Suites {
		if suite.Name == name {
			return suite, true
		}
	}
	return SuiteSpec{}, false
}

func suiteKeys(suite SuiteSpec) map[AttemptKey]bool {
	result := make(map[AttemptKey]bool)
	for _, caseSpec := range suite.Cases {
		for _, repetition := range caseSpec.Repetitions {
			result[AttemptKey{
				Suite: suite.Name, Condition: caseSpec.Condition,
				Case: caseSpec.ID, Repetition: repetition,
			}] = true
		}
	}
	return result
}

func hasObservedFailure(report Report) bool {
	if len(report.Refusals) > 0 {
		return true
	}
	for _, suite := range report.Comparisons {
		for _, gate := range suite.Gates {
			if gate.Evaluated && !gate.Passed {
				return true
			}
		}
	}
	return false
}

func containsFailedGate(report Report, name string) bool {
	for _, suite := range report.Comparisons {
		for _, gate := range suite.Gates {
			if gate.Name == name && gate.Evaluated && !gate.Passed {
				return true
			}
		}
	}
	return false
}

func containsFinding(report Report, code string) bool {
	for _, finding := range report.Refusals {
		if finding.Code == code {
			return true
		}
	}
	return false
}

func hasLineageCycle(root string, edges map[string][]string) bool {
	state := make(map[string]uint8, len(edges))
	var visit func(string) bool
	visit = func(node string) bool {
		switch state[node] {
		case 1:
			return true
		case 2:
			return false
		}
		state[node] = 1
		next := append([]string(nil), edges[node]...)
		sort.Strings(next)
		for _, child := range next {
			if visit(child) {
				return true
			}
		}
		state[node] = 2
		return false
	}
	if visit(root) {
		return true
	}
	for node := range edges {
		if state[node] == 0 && visit(node) {
			return true
		}
	}
	return false
}
