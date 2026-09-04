package releasevalidation

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestBehavioralAcceptancePassesOnlyCompleteFrozenCandidate(t *testing.T) {
	fixture := newBehavioralFixture(t)
	report := fixture.evaluate(t)
	if !report.Accepted || report.Outcome != BehavioralPassed || len(report.Suites) != 1 ||
		report.Suites[0].Outcome != BehavioralPassed {
		t.Fatalf("acceptance report = %+v", report)
	}
	if report.Suites[0].Execution == nil ||
		!sha256Pattern.MatchString(report.Suites[0].ClosureSHA256) ||
		report.Suites[0].Execution.RequirementSHA256 != fixture.candidate.Suites[0].ExecutionRequirementSHA256 ||
		report.Suites[0].Execution.GraphFingerprint == "" ||
		report.Suites[0].Execution.ConfigurationSHA256 == "" ||
		report.Suites[0].Execution.DeploymentSHA256 == "" ||
		report.Suites[0].Execution.RuntimeSetSHA256 == "" {
		t.Fatalf("execution identities were not reported: %+v", report.Suites[0].Execution)
	}
	payload, err := MarshalBehavioralAcceptanceReport(report)
	if err != nil || !strings.HasSuffix(string(payload), "\n") {
		t.Fatalf("marshal report: %v, %q", err, payload)
	}
}

func TestBehavioralAcceptanceFailsClosedOnCandidateAndEvidenceGaps(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*behavioralFixture)
		want   string
	}{
		{
			name: "incomplete population",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Tasks = fixture.result.Tasks[:1]
				fixture.result.Finish()
			},
			want: "campaign result population differs from closure",
		},
		{
			name: "forged summary",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Summary.Passed++
			},
			want: "stored summary differs",
		},
		{
			name: "completed row hides an error",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Tasks[0].Error = "provider failed"
			},
			want: "completed task outcome carries an error",
		},
		{
			name: "row claims evidence and attestation failure",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Tasks[0].ExecutionError = "inspector failed"
			},
			want: "both execution evidence and an execution error",
		},
		{
			name: "noncanonical metric identity",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Tasks[0].Metrics[" reply_latency_ms"] = 1
				fixture.result.Finish()
			},
			want: "metric name",
		},
		{
			name: "noncanonical note value",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Tasks[0].Notes = map[string]string{"detail": " padded"}
			},
			want: "leading or trailing whitespace",
		},
		{
			name: "build identity mismatch",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Provenance.ExecutableSHA256 = strings.Repeat("c", 64)
			},
			want: "campaign result build or machine differs from closure",
		},
		{
			name: "execution identity mismatch",
			mutate: func(fixture *behavioralFixture) {
				fixture.candidate.Suites[0].ExecutionRequirementSHA256 = digestFor('9')
			},
			want: "campaign result execution requirement differs from closure",
		},
		{
			name: "missing safety evidence",
			mutate: func(fixture *behavioralFixture) {
				delete(fixture.result.Tasks[1].Metrics, "safety_failure_count")
				fixture.result.Finish()
			},
			want: "safety evidence",
		},
		{
			name: "missing deadline evidence",
			mutate: func(fixture *behavioralFixture) {
				delete(fixture.result.Tasks[0].Metrics, "deadline_miss_count")
				fixture.result.Finish()
			},
			want: "deadline evidence",
		},
		{
			name: "missing latency metric",
			mutate: func(fixture *behavioralFixture) {
				delete(fixture.result.Tasks[0].Metrics, "reply_latency_ms")
				delete(fixture.result.Tasks[1].Metrics, "reply_latency_ms")
				fixture.result.Finish()
			},
			want: "latency evidence",
		},
		{
			name: "unregistered latency metric",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Tasks[0].Metrics["new_queue_latency_ms"] = 3
				fixture.result.Tasks[1].Metrics["new_queue_latency_ms"] = 4
				fixture.result.Finish()
			},
			want: "latency metrics lack preregistered targets",
		},
		{
			name: "per-case regression",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Tasks[0].Passed = false
				fixture.result.Finish()
			},
			want: `case "case-a"`,
		},
		{
			name: "diagnostic-only final lineage",
			mutate: func(fixture *behavioralFixture) {
				fixture.candidate.Suites[0].Lineage = []RunLineage{{
					CampaignID: "focus-only", Kind: RunFocusedDiagnostic,
					Population: 1, ArtifactSHA256: digestFor('8'),
				}}
			},
			want: "lineage ends in diagnostic-only evidence",
		},
		{
			name: "final lineage population is partial",
			mutate: func(fixture *behavioralFixture) {
				fixture.candidate.Suites[0].Lineage[0].Population = 1
			},
			want: "not a full final population",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBehavioralFixture(t)
			test.mutate(&fixture)
			report := fixture.evaluate(t)
			if report.Accepted || report.Outcome == BehavioralPassed ||
				!strings.Contains(strings.Join(report.Failures, "\n"), test.want) {
				t.Fatalf("report did not fail for %q: %+v", test.want, report)
			}
		})
	}
}

func TestUnavailablePreregisteredTargetBlocksRatherThanPassing(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.targets.Suites[0].Cases = CaseTargetSet{Registration: Registration{
		Status: RegistrationUnavailable,
		Reason: "the benchmark owner has not accepted surviving per-case numbers",
	}}
	report := fixture.evaluate(t)
	if report.Accepted || report.Outcome != BehavioralBlocked || len(report.BlockedBy) != 1 ||
		!strings.Contains(report.BlockedBy[0], "per-case target unavailable") {
		t.Fatalf("unavailable target report = %+v", report)
	}
}

func TestFocusedRepairLineageRequiresRetainedFailureAndCompleteRerun(t *testing.T) {
	fixture := newBehavioralFixture(t)
	fixture.candidate.Suites[0].Lineage = []RunLineage{
		{CampaignID: "failed-full", Kind: RunFailedFull, Population: 2, ArtifactSHA256: digestFor('4')},
		{CampaignID: "focused-fix", Kind: RunFocusedDiagnostic, Population: 1, ArtifactSHA256: digestFor('5')},
		{CampaignID: "final-rerun", Kind: RunFinalFull, Population: 2},
	}
	if report := fixture.evaluate(t); !report.Accepted {
		t.Fatalf("complete focused-then-full lineage was refused: %+v", report)
	}

	fixture = newBehavioralFixture(t)
	fixture.candidate.Suites[0].Lineage = []RunLineage{
		{CampaignID: "failed-full", Kind: RunFailedFull, Population: 2, ArtifactSHA256: digestFor('4')},
		{CampaignID: "final-rerun", Kind: RunFinalFull, Population: 2},
	}
	if err := fixture.candidate.Validate(); err == nil || !strings.Contains(err.Error(), "no later focused") {
		t.Fatalf("failed-full to final shortcut was accepted: %v", err)
	}
}

func TestBehavioralTargetSchemaRequiresCompleteExplicitRegistrations(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*BehavioralSuiteTarget)
		want   string
	}{
		{"aggregate threshold absent", func(target *BehavioralSuiteTarget) {
			target.Aggregate.MinimumPassed = nil
		}, "invalid minimum"},
		{"case coverage partial", func(target *BehavioralSuiteTarget) {
			target.Cases.Targets = target.Cases.Targets[:1]
		}, "cover 1 attempts"},
		{"safety not zero tolerance", func(target *BehavioralSuiteTarget) {
			target.Safety.Rules[0].Threshold = 1
		}, "zero-tolerance"},
		{"latency has no tail", func(target *BehavioralSuiteTarget) {
			target.Latency.Rules = target.Latency.Rules[:1]
		}, "median and tail"},
		{"target order ambiguous", func(target *BehavioralSuiteTarget) {
			target.Cases.Targets[0], target.Cases.Targets[1] = target.Cases.Targets[1], target.Cases.Targets[0]
		}, "uniquely sorted"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			targets := newBehavioralFixture(t).targets
			test.mutate(&targets.Suites[0])
			if err := targets.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid targets were accepted: %v", err)
			}
		})
	}
}

func TestBehavioralControlAndResultParsingAreStrict(t *testing.T) {
	fixture := newBehavioralFixture(t)
	directory := t.TempDir()
	targetPayload, err := json.Marshal(fixture.targets)
	if err != nil {
		t.Fatal(err)
	}
	targetPath := filepath.Join(directory, "targets.json")
	if err := os.WriteFile(targetPath, targetPayload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, digest, err := LoadBehavioralTargets(targetPath); err != nil || !sha256Pattern.MatchString(digest) {
		t.Fatalf("load valid targets: %q, %v", digest, err)
	}
	duplicate := strings.Replace(string(targetPayload), `"format_version":1`,
		`"format_version":1,"format_version":1`, 1)
	if err := os.WriteFile(targetPath, []byte(duplicate), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadBehavioralTargets(targetPath); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate target field was accepted: %v", err)
	}

	fixture.writeResult(t)
	payload, err := os.ReadFile(fixture.resultPath)
	if err != nil {
		t.Fatal(err)
	}
	unknown := strings.Replace(string(payload), `"suite":`, `"unknown":true,"suite":`, 1)
	badPath := filepath.Join(directory, "unknown-result.json")
	if err := os.WriteFile(badPath, []byte(unknown), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readBehavioralResult(badPath, ResultKindBench); err == nil ||
		!strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown result field was accepted: %v", err)
	}
}

func TestCheckedBehavioralTargetsDeclareExactFinalMatrix(t *testing.T) {
	root := repositoryRoot(t)
	targets, digest, err := LoadBehavioralTargets(
		filepath.Join(root, "scripts", "behavioral-acceptance-targets.json"),
	)
	if err != nil {
		t.Fatalf("load checked behavioral targets: %v", err)
	}
	if !sha256Pattern.MatchString(digest) {
		t.Fatalf("checked target digest is not canonical: %q", digest)
	}
	type expectedSuite struct {
		resultKind string
		suite      string
		population int
	}
	want := map[string]expectedSuite{
		"fd-bench":        {ResultKindBench, "fd-bench", 6147},
		"fdb-v1.5":        {ResultKindBench, "fdb-v1.5", 498},
		"fdb-v3":          {ResultKindBench, "fdb-v3", 100},
		"meeting-cascade": {ResultKindBench, "openrealtime-meeting-assistant-v1", 4},
		"realtime-cu":     {ResultKindBench, "openrealtime-realtime-cu-v1", 16},
		"scenario":        {ResultKindArchitecture, "scenario", 180},
		"tau-control":     {ResultKindBench, "tau-voice", 278},
		"tau-regular":     {ResultKindBench, "tau-voice", 278},
	}
	if len(targets.Suites) != len(want) {
		t.Fatalf("checked target suite count = %d, want %d", len(targets.Suites), len(want))
	}
	population := 0
	var scenarioTarget *BehavioralSuiteTarget
	for _, suite := range targets.Suites {
		expected, found := want[suite.ID]
		if !found {
			t.Errorf("checked targets contain unexpected suite %q", suite.ID)
			continue
		}
		delete(want, suite.ID)
		if suite.ResultKind != expected.resultKind || suite.Suite != expected.suite ||
			suite.ExpectedPopulation != expected.population {
			t.Errorf("checked target %s = kind %q, suite %q, population %d; want %+v",
				suite.ID, suite.ResultKind, suite.Suite, suite.ExpectedPopulation, expected)
		}
		population += suite.ExpectedPopulation
		if suite.ID == "scenario" {
			copy := suite
			scenarioTarget = &copy
		}
	}
	if len(want) != 0 {
		t.Fatalf("checked targets omit final suites: %+v", want)
	}
	if population != 7501 {
		t.Fatalf("checked final population = %d, want 7501", population)
	}
	if scenarioTarget == nil || scenarioTarget.Aggregate.MinimumPassed == nil ||
		*scenarioTarget.Aggregate.MinimumPassed != 180 || len(scenarioTarget.Cases.Targets) != 12 {
		t.Fatalf("scenario acceptance target is not the exact 180/180, twelve-case gate: %+v",
			scenarioTarget)
	}
	minimumTotal := 0
	countMinimum := -1
	for _, target := range scenarioTarget.Cases.Targets {
		minimumTotal += target.MinimumPassed
		if target.Case == "count-as-they-go" {
			if target.ExpectedAttempts != 15 {
				t.Fatalf("count-as-they-go attempts = %d, want 15", target.ExpectedAttempts)
			}
			countMinimum = target.MinimumPassed
		}
	}
	if minimumTotal != 180 || countMinimum != 15 {
		t.Fatalf("scenario per-case minima sum/count-as-they-go = %d/%d, want 180/15",
			minimumTotal, countMinimum)
	}
}

type behavioralFixture struct {
	targets     BehavioralTargets
	candidate   FrozenCandidate
	result      bench.Result
	directory   string
	resultPath  string
	closurePath string
}

func newBehavioralFixture(t *testing.T) behavioralFixture {
	t.Helper()
	requirement := behavioralRequirement(t)
	requirementPayload, err := bench.MarshalExecutionRequirement(requirement)
	if err != nil {
		t.Fatal(err)
	}
	revision := strings.Repeat("a", 40)
	machine := bench.Machine{
		CPU: "fixture cpu", Cores: 8, GPU: "fixture gpu", OS: "linux", Arch: "amd64",
		GoVersion: "go1.25.0", Hostname: "fixture-host",
	}
	result := bench.Result{
		Suite: "fixture-suite", Expected: 2,
		Cell: bench.Cell{
			Name: "fixture-final", Levels: bench.Reference().Levels, Execution: requirement,
		},
		Provenance: bench.Provenance{
			Revision: revision, ExecutableSHA256: strings.Repeat("b", 64), Machine: machine,
			StartedAt: "2026-09-02T00:00:00Z",
		},
		Tasks: []bench.TaskOutcome{
			behavioralTask(t, requirement, "case-a", true, 10),
			behavioralTask(t, requirement, "case-b", false, 20),
		},
	}
	result.Finish()
	registered := func(source string) Registration {
		return Registration{Status: RegistrationRegistered, Source: source}
	}
	minimum := 1
	targets := BehavioralTargets{FormatVersion: BehavioralTargetsVersion, Suites: []BehavioralSuiteTarget{{
		ID: "fixture", ResultKind: ResultKindBench, Suite: result.Suite,
		ExpectedPopulation: 2, CaseKey: CaseKeyExact,
		Aggregate: AggregateTarget{Registration: registered("fixture aggregate"), MinimumPassed: &minimum},
		Cases: CaseTargetSet{Registration: registered("fixture cases"), Targets: []CaseTarget{
			{Case: "case-a", ExpectedAttempts: 1, MinimumPassed: 1},
			{Case: "case-b", ExpectedAttempts: 1, MinimumPassed: 0},
		}},
		Safety: EvidenceTargetSet{Registration: registered("fixture safety"), Rules: []MetricRule{{
			Name: "zero-safety-failures", Metric: "safety_failure_count", Statistic: StatisticSum,
			Comparison: ComparisonAtMost, Threshold: 0, MinimumSamples: 2, RequireEveryTask: true,
		}}},
		Deadline: EvidenceTargetSet{Registration: registered("fixture deadline"), Rules: []MetricRule{{
			Name: "zero-deadline-misses", Metric: "deadline_miss_count", Statistic: StatisticSum,
			Comparison: ComparisonAtMost, Threshold: 0, MinimumSamples: 2, RequireEveryTask: true,
		}}},
		Latency: EvidenceTargetSet{Registration: registered("fixture latency"), Rules: []MetricRule{
			{Name: "reply-median", Metric: "reply_latency_ms", Statistic: StatisticP50,
				Comparison: ComparisonAtMost, Threshold: 20, MinimumSamples: 2, RequireEveryTask: true},
			{Name: "reply-tail", Metric: "reply_latency_ms", Statistic: StatisticP95,
				Comparison: ComparisonAtMost, Threshold: 25, MinimumSamples: 2, RequireEveryTask: true},
		}},
	}}}
	directory := t.TempDir()
	candidate := FrozenCandidate{
		FormatVersion: FrozenCandidateVersion, CandidateID: "fixture-final",
		Revision: revision, ExecutableSHA256: "sha256:" + strings.Repeat("b", 64), Machine: machine,
		Suites: []FrozenCandidateSuite{{
			ID: "fixture", ExecutionRequirementSHA256: digestBytes(requirementPayload),
			RunSpecSHA256: digestFor('3'), TaskInventorySHA256: digestFor('4'),
			ScorerSHA256: digestFor('5'),
			SourceReceipts: []CampaignSourceRequirement{{
				Kind: "fixture-source", ArtifactFormat: "fixture.source-receipt",
			}},
			Lineage: []RunLineage{{CampaignID: "final", Kind: RunFinalFull, Population: 2}},
		}},
	}
	return behavioralFixture{targets: targets, candidate: candidate,
		result: result, directory: directory,
		resultPath:  filepath.Join(directory, "result.json"),
		closurePath: filepath.Join(directory, "closure.json")}
}

func (fixture *behavioralFixture) writeResult(t *testing.T) {
	t.Helper()
	if err := fixture.result.Write(fixture.resultPath); err != nil {
		t.Fatal(err)
	}
}

func (fixture *behavioralFixture) publishClosure(t *testing.T) {
	t.Helper()
	fixture.writeResult(t)
	lineage := fixture.candidate.Suites[0].Lineage
	if len(lineage) == 0 {
		return
	}
	predecessors := make([]CampaignArtifact, 0, len(lineage)-1)
	for index, run := range lineage[:len(lineage)-1] {
		population := run.Population
		if population > len(fixture.result.Tasks) {
			population = len(fixture.result.Tasks)
		}
		priorResult := fixture.result
		priorResult.Expected = population
		priorResult.Tasks = slices.Clone(fixture.result.Tasks[:population])
		// A repair changes the executable and therefore creates a new candidate.
		// Predecessor closures must remain admissible across that boundary; only
		// the final accepted populations are required to share one candidate.
		priorResult.Provenance.Revision = strings.Repeat(string(rune('c'+index)), 40)
		priorResult.Provenance.ExecutableSHA256 = strings.Repeat(string(rune('d'+index)), 64)
		priorResult.Finish()
		prefix := fmt.Sprintf("prior-%02d", index)
		priorPath := filepath.Join(fixture.directory, prefix+".closure.json")
		prior := fixture.campaignClosure(t, prefix, run.CampaignID, priorResult, nil)
		prior.CandidateID = fmt.Sprintf("diagnostic-candidate-%02d", index)
		prior.CandidateSHA256 = digestFor(byte('a' + index))
		prior.Revision = priorResult.Provenance.Revision
		prior.ExecutableSHA256 = "sha256:" + priorResult.Provenance.ExecutableSHA256
		if _, err := PublishCampaignClosure(CampaignClosurePublication{Closure: prior, Path: priorPath}); err != nil {
			t.Fatalf("publish predecessor closure: %v", err)
		}
		payload, err := os.ReadFile(priorPath)
		if err != nil {
			t.Fatal(err)
		}
		digest := digestBytes(payload)
		fixture.candidate.Suites[0].Lineage[index].ArtifactSHA256 = digest
		predecessors = append(predecessors, CampaignArtifact{
			Format: CampaignClosureFormat, FormatVersion: CampaignClosureVersion,
			Path: filepath.Base(priorPath), ArtifactSHA256: digest,
		})
	}
	final := fixture.candidate.Suites[0].Lineage[len(lineage)-1]
	closure := fixture.campaignClosure(t, "final", final.CampaignID, fixture.result, predecessors)
	closure.ClosureSHA256 = campaignClosureDigest(closure)
	payload, err := marshalCampaignCanonical(closure)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.closurePath, payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func (fixture *behavioralFixture) campaignClosure(
	t *testing.T, prefix, campaignID string, result bench.Result, predecessors []CampaignArtifact,
) CampaignClosure {
	t.Helper()
	runSpec := CampaignRunSpec{
		Format: CampaignRunSpecFormat, FormatVersion: CampaignRunSpecVersion,
		SuiteID: "fixture", CampaignID: campaignID,
		WorkingDirectory: "repository-root",
		Arguments:        []string{"openrealtime", "bench", "fixture", "-seed", "7"},
		Environment:      []CampaignNamedValue{{Name: "fixture_mode", Value: "release"}},
		Endpoints:        []CampaignNamedDigest{{Name: "agent", SHA256: digestFor('6')}},
	}
	ids := make([]string, len(result.Tasks))
	for index, task := range result.Tasks {
		ids[index] = task.ID
	}
	sort.Strings(ids)
	inventory := CampaignTaskInventory{
		Format: CampaignInventoryFormat, FormatVersion: CampaignInventoryVersion,
		SuiteID: "fixture", ExpectedPopulation: len(ids), TaskIDs: ids,
	}
	scorer := CampaignScorerManifest{
		Format: CampaignScorerFormat, FormatVersion: CampaignScorerVersion,
		SuiteID: "fixture", Identity: "fixture deterministic scorer",
		Revision: "v1", ImplementationSHA256: digestFor('7'),
		Inputs: []CampaignNamedDigest{{Name: "rubric", SHA256: digestFor('8')}},
	}
	runArtifact := fixture.writeCampaignArtifact(t, prefix+".run-spec.json", runSpec,
		CampaignRunSpecFormat, CampaignRunSpecVersion)
	inventoryArtifact := fixture.writeCampaignArtifact(t, prefix+".inventory.json", inventory,
		CampaignInventoryFormat, CampaignInventoryVersion)
	scorerArtifact := fixture.writeCampaignArtifact(t, prefix+".scorer.json", scorer,
		CampaignScorerFormat, CampaignScorerVersion)
	resultName := prefix + ".result.json"
	resultPath := filepath.Join(fixture.directory, resultName)
	if prefix == "final" {
		resultPath = fixture.resultPath
		resultName = filepath.Base(fixture.resultPath)
	} else if err := result.Write(resultPath); err != nil {
		t.Fatal(err)
	}
	resultPayload, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatal(err)
	}
	resultSHA256 := digestBytes(resultPayload)
	rawSourceName := prefix + ".source.raw.json"
	rawSourcePayload := []byte("{\"complete\":true,\"format\":\"fixture.source-receipt\",\"receipt_sha256\":\"" +
		digestFor('9') + "\",\"result_sha256\":\"" + resultSHA256 + "\"}\n")
	if err := os.WriteFile(filepath.Join(fixture.directory, rawSourceName), rawSourcePayload, 0o644); err != nil {
		t.Fatal(err)
	}
	sourceReceipt, err := SealCampaignArtifactReceipt(CampaignArtifactReceipt{
		Format: CampaignArtifactReceiptFormat, FormatVersion: CampaignArtifactReceiptVersion,
		Kind: "fixture-source", ArtifactFormat: "fixture.source-receipt",
		ArtifactPath: rawSourceName, ArtifactSHA256: digestBytes(rawSourcePayload),
		PortableReceiptSHA256: digestFor('9'),
	})
	if err != nil {
		t.Fatal(err)
	}
	sourceArtifact := fixture.writeCampaignArtifact(t, prefix+".source.receipt.json", sourceReceipt,
		CampaignArtifactReceiptFormat, CampaignArtifactReceiptVersion)
	if prefix == "final" {
		fixture.candidate.Suites[0].RunSpecSHA256 = runArtifact.ArtifactSHA256
		fixture.candidate.Suites[0].TaskInventorySHA256 = inventoryArtifact.ArtifactSHA256
		fixture.candidate.Suites[0].ScorerSHA256 = scorerArtifact.ArtifactSHA256
	}
	return CampaignClosure{
		Format: CampaignClosureFormat, FormatVersion: CampaignClosureVersion,
		SuiteID: "fixture", CampaignID: campaignID,
		CandidateID: fixture.candidate.CandidateID, CandidateSHA256: digestFor('2'),
		Revision: fixture.candidate.Revision, ExecutableSHA256: fixture.candidate.ExecutableSHA256,
		Machine:                    fixture.candidate.Machine,
		ExecutionRequirementSHA256: fixture.candidate.Suites[0].ExecutionRequirementSHA256,
		RunSpec:                    runArtifact, Inventory: inventoryArtifact,
		SourceReceipts: []CampaignArtifact{sourceArtifact},
		Result: CampaignArtifact{Format: ResultKindBench, FormatVersion: 1,
			Path: resultName, ArtifactSHA256: resultSHA256},
		Scorer: scorerArtifact, ExpectedPopulation: len(result.Tasks),
		TaskPopulationSHA256: digestTaskPopulation(result.Tasks), Predecessors: predecessors,
	}
}

func (fixture behavioralFixture) writeCampaignArtifact(
	t *testing.T, name string, value any, format string, version int,
) CampaignArtifact {
	t.Helper()
	payload, err := MarshalCampaignArtifact(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.directory, name), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return CampaignArtifact{Format: format, FormatVersion: version, Path: name,
		ArtifactSHA256: digestBytes(payload)}
}

func (fixture behavioralFixture) evaluate(t *testing.T) BehavioralAcceptanceReport {
	t.Helper()
	fixture.publishClosure(t)
	return EvaluateBehavioralAcceptance(
		fixture.targets, digestFor('1'), fixture.candidate, digestFor('2'),
		[]BehavioralClosureInput{{ID: "fixture", Path: fixture.closurePath}},
	)
}

func behavioralTask(
	t *testing.T, requirement bench.ExecutionRequirement, id string, passed bool, latency float64,
) bench.TaskOutcome {
	t.Helper()
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
		Scope: id, Graph: requirement.Graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	return bench.TaskOutcome{
		ID: id, Completed: true, Passed: passed, Execution: &evidence,
		Metrics: map[string]float64{
			"safety_failure_count": 0, "deadline_miss_count": 0, "reply_latency_ms": latency,
		},
	}
}

func behavioralRequirement(t *testing.T) bench.ExecutionRequirement {
	t.Helper()
	digest := digestFor('d')
	event := element.Event(element.Named("fixture.Message"))
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "behavioral-fixture", Revision: 1,
		Nodes: []ir.Node{
			{ID: "source", Element: element.Identity{Name: "fixture.Source", Revision: 1, Digest: digest},
				Implementation: "fixture.Source", ConfigReference: "values://fixture/source", ConfigDigest: digest,
				Ports: []ir.Port{{Name: "out", Direction: element.Output, Type: event, Cardinality: element.One}}},
			{ID: "sink", Element: element.Identity{Name: "fixture.Sink", Revision: 1, Digest: digest},
				Implementation: "fixture.Sink", ConfigReference: "values://fixture/sink", ConfigDigest: digest,
				Ports: []ir.Port{{Name: "in", Direction: element.Input, Type: event, Cardinality: element.One}}},
		},
		Edges: []ir.Edge{{
			ID: "source-to-sink", From: ir.Endpoint{Node: "source", Port: "out"},
			To: ir.Endpoint{Node: "sink", Port: "in"}, Type: event,
			Delivery: ir.Lossless, Ordering: "fifo", Depth: 1,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	requirement, err := bench.RequireGraph(
		graph,
		bench.ArtifactIdentity{ID: "values://fixture", Revision: "v1", Digest: digest},
		bench.LiveResolution{
			Deployment: &inspect.DeploymentEvidence{
				Public:                       inspect.ArtifactIdentity{ID: "deployment://fixture", Revision: "v1", Digest: digest},
				PrivateDeploymentFingerprint: digest,
			},
			Elements: []bench.ElementResolution{
				{Node: "source", Element: graph.Nodes[1].Element, Implementation: graph.Nodes[1].Implementation,
					Runtime: bench.ArtifactIdentity{ID: "runtime://source", Revision: "v1"}},
				{Node: "sink", Element: graph.Nodes[0].Element, Implementation: graph.Nodes[0].Implementation,
					Runtime: bench.ArtifactIdentity{ID: "runtime://sink", Revision: "v1"}},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return requirement
}

func digestFor(character byte) string {
	return "sha256:" + strings.Repeat(string(character), 64)
}
