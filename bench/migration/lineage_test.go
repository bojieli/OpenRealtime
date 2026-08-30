package migration

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCampaignRequiresDiagnosticThenCompleteFullRerun(t *testing.T) {
	failed := failedFullReport(t)
	failedReference := mustReference(t, failed)

	diagnosticManifest := testManifest()
	diagnosticManifest.Campaign = CampaignPlan{
		CampaignID: "migration-release-1", RunID: "diagnostic-safety-1", Kind: RunDiagnostic,
		Predecessors: []ReportReference{failedReference},
		DiagnosedFailures: []FailureReference{{
			ReportID: failed.ReportID, Gate: "synthetic/safety/zero_tolerance",
		}},
	}
	baseline, candidate := testAttempts()
	diagnostic := CompareWithHistory(diagnosticManifest, baseline, candidate, []Report{failed})
	if !diagnostic.Reportable || diagnostic.Accepted || diagnostic.CampaignGate.Passed {
		t.Fatalf("diagnostic release state = reportable %v accepted %v gate %+v refusals %+v",
			diagnostic.Reportable, diagnostic.Accepted, diagnostic.CampaignGate, diagnostic.Refusals)
	}
	if !strings.Contains(diagnostic.CampaignGate.Reason, "diagnostic") {
		t.Fatalf("diagnostic refusal reason = %q", diagnostic.CampaignGate.Reason)
	}
	if err := diagnostic.VerifyWithHistory([]Report{failed}); err != nil {
		t.Fatalf("verify diagnostic lineage: %v", err)
	}

	finalManifest := testManifest()
	finalManifest.Campaign = CampaignPlan{
		CampaignID: "migration-release-1", RunID: "full-final-1", Kind: RunFull,
		// This is deliberately the complete transitive history, not only
		// the immediately preceding diagnostic report.
		Predecessors: []ReportReference{failedReference,
			mustReferenceWithHistory(t, diagnostic, []Report{failed})},
		DiagnosedFailures: []FailureReference{{
			ReportID: failed.ReportID, Gate: "synthetic/safety/zero_tolerance",
		}},
	}
	final := CompareWithHistory(finalManifest, baseline, candidate, []Report{diagnostic, failed})
	if !final.Reportable || !final.Accepted || !final.CampaignGate.Passed {
		t.Fatalf("complete full rerun did not close the campaign: %+v", final)
	}
	if len(final.Attempts) != 12 || len(final.Matched) != 6 {
		t.Fatalf("final rerun was not complete: attempts %d matched %d",
			len(final.Attempts), len(final.Matched))
	}
	if err := final.VerifyWithHistory([]Report{failed, diagnostic}); err != nil {
		t.Fatalf("verify final lineage: %v", err)
	}
	if _, err := final.Marshal(); err == nil || !strings.Contains(err.Error(), "campaign history") {
		t.Fatalf("history-bearing report marshaled without history: %v", err)
	}
	payload, err := final.MarshalWithHistory([]Report{failed, diagnostic})
	if err != nil {
		t.Fatalf("marshal final lineage: %v", err)
	}
	if _, err := DecodeWithHistory(bytes.NewReader(payload), []Report{diagnostic, failed}); err != nil {
		t.Fatalf("decode final lineage: %v", err)
	}
	path := filepath.Join(t.TempDir(), "campaign", "full-final-1.json")
	if err := final.WriteWithHistory(path, []Report{diagnostic, failed}); err != nil {
		t.Fatalf("write final lineage: %v", err)
	}
	written, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(written, payload) {
		t.Fatalf("written lineage artifact differs: read error %v", err)
	}
	if err := final.WriteWithHistory(path, []Report{failed, diagnostic}); err == nil {
		t.Fatal("history-bearing immutable report path was overwritten")
	}
	if _, err := DecodeWithHistory(bytes.NewReader(payload), []Report{failed}); err == nil ||
		!strings.Contains(err.Error(), "campaign history") {
		t.Fatalf("decode accepted incomplete history: %v", err)
	}
	if reference := mustReferenceWithHistory(t, final, []Report{failed, diagnostic}); reference.ReportID != final.ReportID ||
		reference.ArtifactSHA256 != final.ArtifactSHA256() {
		t.Fatalf("final reference = %+v", reference)
	}
}

func TestCampaignLineageRejectsMissingForgedAndUnacknowledgedHistory(t *testing.T) {
	failed := failedFullReport(t)
	failedReference := mustReference(t, failed)
	baseline, candidate := testAttempts()

	t.Run("missing predecessor", func(t *testing.T) {
		manifest := testManifest()
		manifest.Campaign.RunID = "full-missing-history"
		manifest.Campaign.Predecessors = []ReportReference{failedReference}
		manifest.Campaign.DiagnosedFailures = []FailureReference{{
			ReportID: failed.ReportID, Gate: "synthetic/safety/zero_tolerance",
		}}
		report := CompareWithHistory(manifest, baseline, candidate, nil)
		assertLineageRefusal(t, report, "lineage.predecessor_missing")
	})

	t.Run("forged artifact digest", func(t *testing.T) {
		manifest := testManifest()
		manifest.Campaign.RunID = "full-forged-history"
		forged := failedReference
		forged.ArtifactSHA256 = strings.Repeat("b", 64)
		manifest.Campaign.Predecessors = []ReportReference{forged}
		manifest.Campaign.DiagnosedFailures = []FailureReference{{
			ReportID: failed.ReportID, Gate: "synthetic/safety/zero_tolerance",
		}}
		report := CompareWithHistory(manifest, baseline, candidate, []Report{failed})
		assertLineageRefusal(t, report, "lineage.artifact_forged")
	})

	t.Run("forged derived report", func(t *testing.T) {
		forgedReport := failed
		forgedReport.Accepted = true
		forgedReport.ReportID = reportDigest(forgedReport)
		manifest := testManifest()
		manifest.Campaign.RunID = "full-forged-derived-report"
		manifest.Campaign.Predecessors = []ReportReference{reportReference(forgedReport)}
		manifest.Campaign.DiagnosedFailures = []FailureReference{{
			ReportID: forgedReport.ReportID, Gate: "synthetic/safety/zero_tolerance",
		}}
		report := CompareWithHistory(manifest, baseline, candidate, []Report{forgedReport})
		assertLineageRefusal(t, report, "lineage.predecessor_invalid")
	})

	t.Run("forged report digest", func(t *testing.T) {
		manifest := testManifest()
		manifest.Campaign.RunID = "full-forged-report"
		forged := failedReference
		forged.ReportID = strings.Repeat("c", 64)
		manifest.Campaign.Predecessors = []ReportReference{forged}
		manifest.Campaign.DiagnosedFailures = []FailureReference{{
			ReportID: forged.ReportID, Gate: "synthetic/safety/zero_tolerance",
		}}
		report := CompareWithHistory(manifest, baseline, candidate, []Report{failed})
		assertLineageRefusal(t, report, "lineage.report_forged")
	})

	t.Run("failed report not named", func(t *testing.T) {
		manifest := testManifest()
		manifest.Campaign.RunID = "full-unacknowledged-failure"
		manifest.Campaign.Predecessors = []ReportReference{failedReference}
		report := CompareWithHistory(manifest, baseline, candidate, []Report{failed})
		assertLineageRefusal(t, report, "lineage.failed_report_not_superseded")
	})

	t.Run("forged diagnosed gate", func(t *testing.T) {
		manifest := testManifest()
		manifest.Campaign.RunID = "full-forged-failure"
		manifest.Campaign.Predecessors = []ReportReference{failedReference}
		manifest.Campaign.DiagnosedFailures = []FailureReference{{
			ReportID: failed.ReportID, Gate: "synthetic/pass/non_inferiority",
		}}
		report := CompareWithHistory(manifest, baseline, candidate, []Report{failed})
		assertLineageRefusal(t, report, "lineage.failure_gate_forged")
	})
}

func TestFinalRerunCannotShrinkOrRetuneAnAffectedFullSuite(t *testing.T) {
	failed := failedFullReport(t)
	baseline, candidate := testAttempts()
	tests := []struct {
		name string
		edit func(*Manifest, *[]Attempt, *[]Attempt)
	}{
		{
			name: "shrink population",
			edit: func(manifest *Manifest, baseline, candidate *[]Attempt) {
				suite := &manifest.Suites[0]
				suite.Cases = suite.Cases[:2]
				suite.ExpectedCases = 2
				suite.ExpectedAttempts = 4
				suite.Populations[0].ExpectedCases = 2
				suite.Populations[0].ExpectedAttempts = 4
				suite.Policy.Pass.MinimumAttempts = 4
				suite.Policy.Pass.MinimumCases = 2
				suite.Policy.Interaction.NonInferiority.MinimumAttempts = 4
				suite.Policy.Interaction.NonInferiority.MinimumCases = 2
				suite.Policy.Deadline.NonInferiority.MinimumAttempts = 4
				suite.Policy.Deadline.NonInferiority.MinimumCases = 2
				suite.Policy.Latencies[0].Gate.MinimumAttempts = 4
				suite.Policy.Latencies[0].Gate.MinimumCases = 2
				*baseline = (*baseline)[:4]
				*candidate = (*candidate)[:4]
			},
		},
		{
			name: "relax margin after observing failure",
			edit: func(manifest *Manifest, _, _ *[]Attempt) {
				manifest.Suites[0].Policy.Pass.Margin = 0.99
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manifest := testManifest()
			manifest.Campaign.RunID = "full-retuned-" + strings.ReplaceAll(test.name, " ", "-")
			manifest.Campaign.Predecessors = []ReportReference{mustReference(t, failed)}
			manifest.Campaign.DiagnosedFailures = []FailureReference{{
				ReportID: failed.ReportID, Gate: "synthetic/safety/zero_tolerance",
			}}
			left := append([]Attempt(nil), baseline...)
			right := append([]Attempt(nil), candidate...)
			test.edit(&manifest, &left, &right)
			report := CompareWithHistory(manifest, left, right, []Report{failed})
			if report.Reportable || !hasFinding(report, "lineage.affected_suite_changed") {
				t.Fatalf("changed affected suite was accepted: %+v", report.Refusals)
			}
		})
	}
}

func TestCampaignLineageRejectsCycles(t *testing.T) {
	t.Run("self predecessor", func(t *testing.T) {
		manifest := testManifest()
		manifest.Campaign.RunID = "cycle-self"
		manifest.Campaign.Predecessors = []ReportReference{{
			RunID: "cycle-self", ReportID: strings.Repeat("a", 64),
			ArtifactSHA256: strings.Repeat("b", 64),
		}}
		if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "cannot name itself") {
			t.Fatalf("self-cycle manifest validated: %v", err)
		}
		baseline, candidate := testAttempts()
		report := CompareWithHistory(manifest, baseline, candidate, nil)
		if !hasFinding(report, "campaign.lineage_cycle") {
			t.Fatalf("self-cycle missing refusal: %+v", report.Refusals)
		}
	})

	t.Run("predecessor points to current run", func(t *testing.T) {
		predecessor := failedFullReport(t)
		currentRun := "cycle-current"
		predecessor.Manifest.Campaign.Predecessors = []ReportReference{{
			RunID: currentRun, ReportID: strings.Repeat("c", 64),
			ArtifactSHA256: strings.Repeat("d", 64),
		}}
		predecessor.ManifestID = predecessor.Manifest.ID()
		predecessor.ReportID = reportDigest(predecessor)

		manifest := testManifest()
		manifest.Campaign.RunID = currentRun
		manifest.Campaign.Predecessors = []ReportReference{reportReference(predecessor)}
		manifest.Campaign.DiagnosedFailures = []FailureReference{{
			ReportID: predecessor.ReportID, Gate: "synthetic/safety/zero_tolerance",
		}}
		baseline, candidate := testAttempts()
		report := CompareWithHistory(manifest, baseline, candidate, []Report{predecessor})
		assertLineageRefusal(t, report, "lineage.cycle")
	})
}

func TestDiagnosticHistoryMustNameTheFailureBeingInvestigated(t *testing.T) {
	failed := failedFullReport(t)
	manifest := testManifest()
	manifest.Campaign = CampaignPlan{
		CampaignID: "migration-release-1", RunID: "diagnostic-without-target", Kind: RunDiagnostic,
		Predecessors: []ReportReference{mustReference(t, failed)},
	}
	baseline, candidate := testAttempts()
	report := CompareWithHistory(manifest, baseline, candidate, []Report{failed})
	assertLineageRefusal(t, report, "lineage.diagnostic_target_missing")
}

func TestDiagnosticCannotPrecedeFrozenFullPlan(t *testing.T) {
	manifest := testManifest()
	manifest.Campaign = CampaignPlan{
		CampaignID: "migration-release-1", RunID: "diagnostic-before-plan", Kind: RunDiagnostic,
	}
	baseline, candidate := testAttempts()
	report := Compare(manifest, baseline, candidate)
	if report.Reportable || !hasFinding(report, "campaign.diagnostic_predecessor_missing") ||
		!hasFinding(report, "campaign.diagnostic_target_missing") {
		t.Fatalf("unfrozen initial diagnostic was accepted: %+v", report.Refusals)
	}
}

func TestFinalFullRerunMustCloseEveryFailedPredecessorGate(t *testing.T) {
	manifest := testManifest()
	manifest.Campaign.RunID = "full-multiple-failures"
	baseline, regressed := testAttempts()
	regressed[0].Outcomes.Safety = OutcomeFailed
	for index := range regressed {
		regressed[index].Latencies[0].Value += 100
	}
	failed := Compare(manifest, baseline, regressed)
	failures := observedFailures(failed)
	if len(failures) != 3 {
		t.Fatalf("observed failures = %+v, want safety, p50, and p95", failures)
	}

	finalManifest := testManifest()
	finalManifest.Campaign = CampaignPlan{
		CampaignID: "migration-release-1", RunID: "full-after-multiple-failures", Kind: RunFull,
		Predecessors:      []ReportReference{mustReference(t, failed)},
		DiagnosedFailures: []FailureReference{failures[0]},
	}
	cleanBaseline, cleanCandidate := testAttempts()
	partial := CompareWithHistory(finalManifest, cleanBaseline, cleanCandidate, []Report{failed})
	if partial.Reportable || !hasFinding(partial, "lineage.failed_report_not_superseded") {
		t.Fatalf("partial failure acknowledgement closed the campaign: %+v", partial.Refusals)
	}

	finalManifest.Campaign.DiagnosedFailures = failures
	closed := CompareWithHistory(finalManifest, cleanBaseline, cleanCandidate, []Report{failed})
	if !closed.Reportable || !closed.Accepted || !closed.CampaignGate.Passed {
		t.Fatalf("all failed gates did not close the full rerun: %+v", closed)
	}
}

func TestMalformedFullPlanCanBeRepairedWithoutUnlockingIdentity(t *testing.T) {
	invalidManifest := testManifest()
	invalidManifest.Campaign.RunID = "full-invalid-repetition-plan"
	invalidManifest.Suites[0].MinimumRepetitions = 3
	baseline, candidate := testAttempts()
	failed := Compare(invalidManifest, baseline, candidate)
	if failed.Reportable || !hasFinding(failed, "manifest.repetition_minimum") {
		t.Fatalf("invalid study plan did not produce a retainable refusal: %+v", failed)
	}
	failures := observedFailures(failed)
	if len(failures) != 1 || failures[0].FindingCode != "manifest.repetition_minimum" {
		t.Fatalf("invalid-plan failures = %+v", failures)
	}

	corrected := testManifest()
	corrected.Campaign = CampaignPlan{
		CampaignID: "migration-release-1", RunID: "full-corrected-repetition-plan", Kind: RunFull,
		Predecessors:      []ReportReference{mustReference(t, failed)},
		DiagnosedFailures: failures,
	}
	closed := CompareWithHistory(corrected, baseline, candidate, []Report{failed})
	if !closed.Reportable || !closed.Accepted || !closed.CampaignGate.Passed {
		t.Fatalf("corrected full study could not supersede malformed predecessor: %+v", closed)
	}

	retuned := corrected
	retuned.Campaign.RunID = "full-corrected-with-retuned-baseline"
	retuned.FixedAxes[0].Value = "different-fixture"
	retunedBaseline := append([]Attempt(nil), baseline...)
	retunedCandidate := append([]Attempt(nil), candidate...)
	for index := range retunedBaseline {
		setAxis(&retunedBaseline[index], "fixture", "different-fixture")
		setAxis(&retunedCandidate[index], "fixture", "different-fixture")
	}
	refused := CompareWithHistory(retuned, retunedBaseline, retunedCandidate, []Report{failed})
	if refused.Reportable || !hasFinding(refused, "lineage.fixed_axes_changed") {
		t.Fatalf("repairing a malformed suite unlocked baseline identity: %+v", refused.Refusals)
	}
}

func TestFailedLineageInvocationRemainsRetainableAndSupersedable(t *testing.T) {
	baseline, candidate := testAttempts()
	ancestorManifest := testManifest()
	ancestorManifest.Campaign.RunID = "full-ancestor"
	ancestor := Compare(ancestorManifest, baseline, candidate)

	missingManifest := testManifest()
	missingManifest.Campaign.RunID = "full-history-omitted"
	missingManifest.Campaign.Predecessors = []ReportReference{mustReference(t, ancestor)}
	missing := CompareWithHistory(missingManifest, baseline, candidate, nil)
	if missing.Reportable || !hasFinding(missing, "lineage.predecessor_missing") ||
		len(missing.SuppliedHistory) != 0 {
		t.Fatalf("missing-history invocation was not retained exactly: %+v", missing)
	}
	if _, err := missing.Marshal(); err != nil {
		t.Fatalf("genuine missing-history refusal was not archivable: %v", err)
	}

	finalManifest := testManifest()
	finalManifest.Campaign = CampaignPlan{
		CampaignID: "migration-release-1", RunID: "full-after-history-repair", Kind: RunFull,
		Predecessors: []ReportReference{
			mustReference(t, ancestor), mustReference(t, missing),
		},
		DiagnosedFailures: []FailureReference{{
			ReportID: missing.ReportID, FindingCode: "lineage.predecessor_missing",
		}},
	}
	closed := CompareWithHistory(finalManifest, baseline, candidate, []Report{missing, ancestor})
	if !closed.Reportable || !closed.Accepted || !closed.CampaignGate.Passed {
		t.Fatalf("retained lineage failure could not be superseded: %+v", closed)
	}
}

func failedFullReport(t *testing.T) Report {
	t.Helper()
	manifest := testManifest()
	manifest.Campaign.RunID = "full-failed-1"
	baseline, candidate := testAttempts()
	candidate[0].Outcomes.Safety = OutcomeFailed
	report := Compare(manifest, baseline, candidate)
	if !report.Reportable || report.Accepted {
		t.Fatalf("failed full fixture = %+v", report)
	}
	return report
}

func mustReference(t *testing.T, report Report) ReportReference {
	t.Helper()
	reference, err := report.Reference()
	if err != nil {
		t.Fatalf("reference report: %v", err)
	}
	return reference
}

func mustReferenceWithHistory(t *testing.T, report Report, history []Report) ReportReference {
	t.Helper()
	reference, err := report.ReferenceWithHistory(history)
	if err != nil {
		t.Fatalf("reference report with history: %v", err)
	}
	return reference
}

func assertLineageRefusal(t *testing.T, report Report, code string) {
	t.Helper()
	if report.Reportable || report.Accepted || len(report.Matched) != 0 ||
		len(report.Comparisons) != 0 || !hasFinding(report, code) {
		t.Fatalf("lineage error %q did not seal the report: %+v", code, report)
	}
	if len(report.Attempts) != 12 {
		t.Fatalf("lineage refusal discarded attempts: %d", len(report.Attempts))
	}
}
