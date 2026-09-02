package releasevalidation

import (
	"encoding/json"
	"os"
	"path/filepath"
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
			want: "population is incomplete",
		},
		{
			name: "forged summary",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Summary.Passed++
			},
			want: "stored summary differs",
		},
		{
			name: "build identity mismatch",
			mutate: func(fixture *behavioralFixture) {
				fixture.result.Provenance.ExecutableSHA256 = strings.Repeat("c", 64)
			},
			want: "executable digest differs",
		},
		{
			name: "execution identity mismatch",
			mutate: func(fixture *behavioralFixture) {
				fixture.candidate.Suites[0].ExecutionRequirementSHA256 = digestFor('9')
			},
			want: "execution identity differs",
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
		"meeting-omni":    {ResultKindBench, "openrealtime-meeting-assistant-v1", 4},
		"realtime-cu":     {ResultKindBench, "openrealtime-realtime-cu-v1", 16},
		"scenario":        {ResultKindArchitecture, "scenario", 165},
		"tau-control":     {ResultKindBench, "tau-voice", 278},
		"tau-regular":     {ResultKindBench, "tau-voice", 278},
	}
	if len(targets.Suites) != len(want) {
		t.Fatalf("checked target suite count = %d, want %d", len(targets.Suites), len(want))
	}
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
	}
	if len(want) != 0 {
		t.Fatalf("checked targets omit final suites: %+v", want)
	}
}

type behavioralFixture struct {
	targets    BehavioralTargets
	candidate  FrozenCandidate
	result     bench.Result
	resultPath string
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
	candidate := FrozenCandidate{
		FormatVersion: FrozenCandidateVersion, CandidateID: "fixture-final",
		Revision: revision, ExecutableSHA256: "sha256:" + strings.Repeat("b", 64), Machine: machine,
		Suites: []FrozenCandidateSuite{{
			ID: "fixture", ExecutionRequirementSHA256: digestBytes(requirementPayload),
			Lineage: []RunLineage{{CampaignID: "final", Kind: RunFinalFull, Population: 2}},
		}},
	}
	return behavioralFixture{targets: targets, candidate: candidate,
		result: result, resultPath: filepath.Join(t.TempDir(), "result.json")}
}

func (fixture *behavioralFixture) writeResult(t *testing.T) {
	t.Helper()
	if err := fixture.result.Write(fixture.resultPath); err != nil {
		t.Fatal(err)
	}
}

func (fixture behavioralFixture) evaluate(t *testing.T) BehavioralAcceptanceReport {
	t.Helper()
	fixture.writeResult(t)
	return EvaluateBehavioralAcceptance(
		fixture.targets, digestFor('1'), fixture.candidate, digestFor('2'),
		[]BehavioralResultInput{{ID: "fixture", Path: fixture.resultPath}},
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
