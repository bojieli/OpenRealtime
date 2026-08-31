package releasevalidation

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStrictMatrixValidation(t *testing.T) {
	valid := testMatrix(testGate("local.pass", []string{"/bin/sh", "-c", "true"}))
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid matrix: %v", err)
	}

	mutations := map[string]func(*Matrix){
		"version": func(matrix *Matrix) { matrix.Version++ },
		"unsorted": func(matrix *Matrix) {
			matrix.Gates = append([]Gate{testGate("local.z", []string{"true"})}, matrix.Gates...)
		},
		"duplicate ID":      func(matrix *Matrix) { matrix.Gates = append(matrix.Gates, matrix.Gates[0]) },
		"unknown selection": func(matrix *Matrix) { matrix.Gates[0].Selection = "maybe" },
		"unrequired":        func(matrix *Matrix) { matrix.Gates[0].Required = false },
		"unsafe workdir":    func(matrix *Matrix) { matrix.Gates[0].WorkingDir = "../outside" },
		"unknown token":     func(matrix *Matrix) { matrix.Gates[0].Command = []string{"{mystery}"} },
		"undeclared environment": func(matrix *Matrix) {
			matrix.Gates[0].Command = []string{"true", "{env:OPENREALTIME_UNDECLARED}"}
		},
		"zero timeout": func(matrix *Matrix) { matrix.Gates[0].Timeout = "0s" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			candidate := cloneMatrix(valid)
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("invalid matrix was accepted")
			}
		})
	}
}

func TestLoadRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "matrix.json")
	for name, payload := range map[string]string{
		"unknown":   `{"version":1,"gates":[],"typo":true}`,
		"trailing":  `{"version":1,"gates":[]} {}`,
		"duplicate": `{"version":1,"version":1,"gates":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("malformed matrix was accepted")
			}
		})
	}
}

func TestExecuteKeepsUnprovisionedRequiredGateBlockedAndReleaseIncomplete(t *testing.T) {
	external := testGate("external.model", []string{"/bin/sh", "-c", "exit 99"})
	external.Availability = AvailabilityProvisioned
	external.Selection = SelectionOptIn
	external.Prerequisites = []Prerequisite{{
		Kind: "env", Value: "OPENREALTIME_TEST_MODEL_ENDPOINT",
		Description: "test model deployment",
	}}
	local := testGate("local.pass", []string{"/bin/sh", "-c", "printf 'done\\n'"})
	local.Assertions = []Assertion{{Kind: "stdout_regex", Value: `(?m)^done$`}}
	matrix := testMatrix(external, local)

	report := executeTestMatrix(t, matrix, Options{Mode: ModeRun, Scope: ScopeLocal})
	if report.SelectedOutcome != "passed" || report.ReleaseComplete {
		t.Fatalf("report outcome=%s complete=%t", report.SelectedOutcome, report.ReleaseComplete)
	}
	if !slices.Equal(report.MissingRequiredGates, []string{"external.model"}) {
		t.Fatalf("missing gates = %v", report.MissingRequiredGates)
	}
	if report.Gates[0].Selected || report.Gates[0].Status != StatusBlocked ||
		!strings.Contains(report.Gates[0].Reason, "OPENREALTIME_TEST_MODEL_ENDPOINT") {
		t.Fatalf("external result = %+v", report.Gates[0])
	}
	if !report.Gates[1].Selected || report.Gates[1].Status != StatusPassed {
		t.Fatalf("local result = %+v", report.Gates[1])
	}
}

func TestExecuteFailsClosedOnSkipAndPostcondition(t *testing.T) {
	for name, gate := range map[string]Gate{
		"skip": func() Gate {
			gate := testGate("local.skip", []string{"/bin/sh", "-c", "printf '%s\\n' '    --- SKIP: TestClaim/subclaim'"})
			return gate
		}(),
		"json skip": func() Gate {
			gate := testGate("local.json-skip", []string{
				"/bin/sh", "-c", `printf '%c%s%c\n' 123 '"Action":"skip"' 125`,
			})
			return gate
		}(),
		"postcondition": func() Gate {
			gate := testGate("local.postcondition", []string{"/bin/sh", "-c", "printf 'NOT REPORTABLE\\n'"})
			gate.Assertions = []Assertion{{Kind: "stdout_regex", Value: `(?m)^reportable$`}}
			return gate
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			report := executeTestMatrix(t, testMatrix(gate), Options{Mode: ModeRun, Scope: ScopeLocal})
			if report.SelectedOutcome != "failed" || report.Gates[0].Status != StatusFailed || report.ReleaseComplete {
				t.Fatalf("report = %+v", report)
			}
		})
	}
}

func TestSetEnvironmentReplacesWithoutDuplicatingUnrelatedAssignments(t *testing.T) {
	source := []string{"PATH=/bin", "TOKEN=old", "LANG=C"}
	result := setEnvironment(source, "TOKEN", "new")
	want := []string{"PATH=/bin", "LANG=C", "TOKEN=new"}
	if !slices.Equal(result, want) {
		t.Fatalf("environment = %v, want %v", result, want)
	}
}

func TestPlanReportsBlockedRatherThanPass(t *testing.T) {
	gate := testGate("external.signed", []string{"/bin/sh", "-c", "true"})
	gate.Availability = AvailabilityProvisioned
	gate.Selection = SelectionOptIn
	gate.Prerequisites = []Prerequisite{{Kind: "os", Value: "definitely-not-this-os", Description: "foreign runner"}}
	report := executeTestMatrix(t, testMatrix(gate), Options{Mode: ModePlan, Scope: ScopeAll})
	if report.SelectedOutcome != "blocked" || report.Gates[0].Status != StatusBlocked || report.ReleaseComplete {
		t.Fatalf("plan = %+v", report)
	}
}

func TestURLPrerequisiteRejectsCredentialsQueriesAndNonNetworkSchemes(t *testing.T) {
	root := repositoryRoot(t)
	goBinary := filepath.Join(root, ".runtime", "toolchains", "go1.25.0", "bin", "go")
	for name, value := range map[string]string{
		"userinfo": "wss://token@example.test/v1/realtime",
		"query":    "wss://example.test/v1/realtime?token=secret",
		"fragment": "https://example.test/api#secret",
		"file":     "file://example.test/tmp/socket",
	} {
		t.Run(name, func(t *testing.T) {
			resolver, err := newResolver(root, goBinary,
				[]string{"OPENREALTIME_TEST_URL=" + value}, filepath.Join(t.TempDir(), "artifacts"))
			if err != nil {
				t.Fatal(err)
			}
			available, _ := resolver.checkPrerequisite(context.Background(), Prerequisite{
				Kind: "env_url", Value: "OPENREALTIME_TEST_URL", Description: "test URL",
			})
			if available {
				t.Fatalf("unsafe URL %q was accepted", value)
			}
		})
	}
}

func TestAnyEnvironmentPrerequisiteRequiresOneDeclaredAlternative(t *testing.T) {
	root := repositoryRoot(t)
	resolver, err := newResolver(
		root,
		filepath.Join(root, ".runtime", "toolchains", "go1.25.0", "bin", "go"),
		[]string{"SECOND_PROVIDER_KEY=available"},
		filepath.Join(t.TempDir(), "artifacts"),
	)
	if err != nil {
		t.Fatal(err)
	}
	prerequisite := Prerequisite{
		Kind: "any_env", Alternatives: []string{"FIRST_PROVIDER_KEY", "SECOND_PROVIDER_KEY"},
		Description: "one supported credential source",
	}
	if err := prerequisite.validate(); err != nil {
		t.Fatal(err)
	}
	if available, reason := resolver.checkPrerequisite(context.Background(), prerequisite); !available {
		t.Fatalf("available alternative was refused: %s", reason)
	}
	resolver.env = map[string]string{}
	if available, _ := resolver.checkPrerequisite(context.Background(), prerequisite); available {
		t.Fatal("missing alternatives were accepted")
	}
}

func TestGateTimeoutFailsAndIsRecorded(t *testing.T) {
	gate := testGate("local.timeout", []string{"/bin/sh", "-c", "sleep 5"})
	gate.Timeout = "50ms"
	report := executeTestMatrix(t, testMatrix(gate), Options{Mode: ModeRun, Scope: ScopeLocal})
	result := report.Gates[0]
	if result.Status != StatusFailed || !result.TimedOut || result.DurationMS > 2000 {
		t.Fatalf("timeout result = %+v", result)
	}
}

func TestCheckedMatrixCoversEveryGoModuleAndFuzzTarget(t *testing.T) {
	root := repositoryRoot(t)
	matrix, err := Load(filepath.Join(root, "scripts", "release-matrix.json"))
	if err != nil {
		t.Fatalf("load checked matrix: %v", err)
	}
	covered := make(map[string]bool)
	for _, gate := range matrix.Gates {
		for _, claim := range gate.Covers {
			covered[claim] = true
		}
	}

	for _, module := range discoverModules(t, root) {
		for _, kind := range []string{"normal", "race", "vet"} {
			claim := "go:" + kind + ":" + module
			if !covered[claim] {
				t.Errorf("Go module release coverage is missing %s", claim)
			}
		}
	}
	for _, fuzz := range discoverFuzzTargets(t, root) {
		if !covered[fuzz] {
			t.Errorf("fuzz release coverage is missing %s", fuzz)
		}
		delete(covered, fuzz)
	}
	for claim := range covered {
		if strings.HasPrefix(claim, "fuzz:") {
			t.Errorf("release matrix names nonexistent fuzz target %s", claim)
		}
	}
}

func TestCheckedMatrixPinsFailClosedSpecialGates(t *testing.T) {
	root := repositoryRoot(t)
	matrix, err := Load(filepath.Join(root, "scripts", "release-matrix.json"))
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Gate, len(matrix.Gates))
	for _, gate := range matrix.Gates {
		byID[gate.ID] = gate
		if gate.Availability == AvailabilityProvisioned && len(gate.Prerequisites) == 0 {
			t.Errorf("provisioned gate %s has no explicit prerequisites", gate.ID)
		}
	}
	candidateIDs := []string{
		"external.benchmark.candidate.fdb15",
		"external.benchmark.candidate.fdb3",
		"external.benchmark.candidate.fdbench",
		"external.benchmark.meeting.cascade",
		"external.benchmark.realtime-cu",
		"external.benchmark.scenario",
		"external.benchmark.tau.control",
		"external.benchmark.tau.regular",
	}
	specialIDs := append([]string{}, candidateIDs...)
	specialIDs = append(specialIDs,
		"external.benchmark.dynacu",
		"external.benchmark.meeting.omni",
		"external.macos.signed-e2e",
		"external.model.live-presentation",
		"external.model.scenario-review",
		"external.tau.upstream",
		"local.client.swift-linux",
		"local.presentation.chromium",
		"local.presentation.shared-server",
		"local.scenario.profiled-websocket",
		"local.sdk.official",
		"performance.review-bundles",
	)
	for _, id := range specialIDs {
		gate, exists := byID[id]
		if !exists || !gate.Required {
			t.Errorf("required special gate is missing: %s", id)
		}
	}
	livePresentation := byID["external.model.live-presentation"]
	if !slices.Equal(livePresentation.Command, []string{
		"{go}", "test", "-count=1", "-v", "./presentation/browser", "-run",
		"^TestLiveComposablePresentationClientAgainstRealModelInChromium$",
	}) || livePresentation.Environment["OPENREALTIME_PRESENTATION_LIVE_ENDPOINT"] !=
		"{env:OPENREALTIME_PRESENTATION_LIVE_ENDPOINT}" ||
		livePresentation.Environment["OPENREALTIME_RELEASE_GATE"] != "1" ||
		livePresentation.SkipPolicy != SkipForbid {
		t.Fatalf("live composable presentation gate was weakened: %+v", livePresentation)
	}
	foundLiveCompletion := false
	for _, assertion := range livePresentation.Assertions {
		foundLiveCompletion = foundLiveCompletion ||
			(assertion.Kind == "stdout_regex" &&
				assertion.Value == `(?m)^live composable presentation client completed$`)
	}
	if !foundLiveCompletion {
		t.Fatalf("live composable presentation gate has no exact completion assertion: %+v", livePresentation)
	}
	scenarioWebSocket := byID["local.scenario.profiled-websocket"]
	if scenarioWebSocket.Availability != AvailabilityLocal ||
		scenarioWebSocket.Selection != SelectionDefault ||
		scenarioWebSocket.SkipPolicy != SkipForbid ||
		!slices.Equal(scenarioWebSocket.Command, []string{
			"{go}", "test", "-count=1", "-v", "./bench/scenario/graphnative", "-run",
			"^TestProfiledGraphNativeWebSocketExercisesExactElevenScenarioContract$",
		}) {
		t.Fatalf("profiled scenario WebSocket gate was weakened: %+v", scenarioWebSocket)
	}
	performance := byID["performance.review-bundles"]
	if performance.Selection != SelectionOptIn ||
		!slices.Contains(performance.Command, "./bench/meeting") ||
		!slices.Contains(performance.Command, "./bench/realtimecu") ||
		argumentAfter(performance.Command, "-benchtime") != "1x" {
		t.Fatalf("performance protocol was weakened: %+v", performance)
	}
	for _, benchmark := range []string{
		"BenchmarkMeetingReviewBundleFourCases",
		"BenchmarkReviewBundleSixteenHermeticReviewedCases",
	} {
		found := false
		for _, assertion := range performance.Assertions {
			found = found || (assertion.Kind == "stdout_regex" && assertion.Value == benchmark)
		}
		if !found {
			t.Errorf("performance gate omits %s", benchmark)
		}
	}
	conditions := argumentAfter(byID["external.benchmark.candidate.fdbench"].Command, "-conditions")
	if got := len(strings.Split(conditions, ",")); got != 21 {
		t.Fatalf("FD-Bench candidate condition count = %d, want 21", got)
	}

	for _, id := range candidateIDs {
		if !slices.Contains(byID[id].Command, "-inspection-graph") {
			t.Errorf("graph-native candidate gate %s has no authenticated inspection input", id)
		}
	}
	for _, gate := range matrix.Gates {
		if strings.HasPrefix(gate.ID, "external.benchmark.baseline.") {
			t.Errorf("release matrix still executes legacy baseline gate %s", gate.ID)
		}
		for _, argument := range gate.Command {
			if strings.Contains(argument, "migration") || strings.Contains(argument, "MIGRATION") {
				t.Errorf("release gate %s retains legacy migration argument %q", gate.ID, argument)
			}
		}
		for _, prerequisite := range gate.Prerequisites {
			if strings.Contains(prerequisite.Value, "MIGRATION") {
				t.Errorf("release gate %s retains legacy migration prerequisite %q",
					gate.ID, prerequisite.Value)
			}
		}
	}

	scenarioReview := byID["external.model.scenario-review"]
	if scenarioReview.Availability != AvailabilityProvisioned ||
		scenarioReview.Selection != SelectionOptIn || scenarioReview.SkipPolicy != SkipForbid ||
		argumentAfter(scenarioReview.Command, "-source-dir") !=
			"{artifacts}/candidate-scenario-review" ||
		argumentAfter(scenarioReview.Command, "-source-receipt") !=
			"{artifacts}/candidate-scenario-review.receipt.json" ||
		argumentAfter(scenarioReview.Command, "-provider") != "google.gemini-3.7-flash" ||
		argumentAfter(scenarioReview.Command, "-parallel") != "4" {
		t.Fatalf("sealed scenario model-review gate was weakened: %+v", scenarioReview)
	}
	for _, artifact := range []string{
		"{artifacts}/candidate-scenario-evaluations/manifest.json",
		"{artifacts}/candidate-scenario-evaluations/REVIEW.md",
		"{artifacts}/candidate-scenario-evaluations/0001-case-01-trial-001.evaluation/media-001.wav",
		"{artifacts}/candidate-scenario-evaluations/0001-case-01-trial-001.receipt.json",
		"{artifacts}/candidate-scenario-evaluations/0165-case-11-trial-015.evaluation/media-001.wav",
		"{artifacts}/candidate-scenario-evaluations/0165-case-11-trial-015.receipt.json",
		"{artifacts}/candidate-scenario-evaluations.receipt.json",
	} {
		if !gateHasAssertion(scenarioReview, "file_nonempty", artifact) {
			t.Errorf("scenario review gate omits retained artifact %q", artifact)
		}
	}

}

func TestMarshalReportIsStableIndentedJSON(t *testing.T) {
	report := Report{Version: 1, MatrixVersion: 1, Mode: ModePlan, Scope: ScopeAll,
		StartedAt: time.Unix(0, 0).UTC(), FinishedAt: time.Unix(1, 0).UTC(),
		SelectedOutcome: "blocked"}
	payload, err := MarshalReport(report)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(payload, []byte("\n")) || !bytes.Contains(payload, []byte("\n  \"version\"")) {
		t.Fatalf("report is not indented canonical presentation: %q", payload)
	}
	var decoded Report
	if err := json.Unmarshal(payload, &decoded); err != nil || decoded.SelectedOutcome != "blocked" {
		t.Fatalf("decode report: %+v, %v", decoded, err)
	}
}

func TestMatrixDigestChangesWithGateSemanticsNotWhitespace(t *testing.T) {
	matrix := testMatrix(testGate("local.pass", []string{"true"}))
	first, err := matrix.Digest()
	if err != nil || len(first) != 64 {
		t.Fatalf("matrix digest = %q, %v", first, err)
	}
	clone := cloneMatrix(matrix)
	second, err := clone.Digest()
	if err != nil || second != first {
		t.Fatalf("equivalent matrix digest = %q, want %q (%v)", second, first, err)
	}
	clone.Gates[0].Timeout = "11s"
	changed, err := clone.Digest()
	if err != nil || changed == first {
		t.Fatalf("changed matrix digest = %q, original %q (%v)", changed, first, err)
	}
}

func testGate(id string, command []string) Gate {
	return Gate{
		ID: id, Description: id, Availability: AvailabilityLocal,
		Selection: SelectionDefault, Required: true, WorkingDir: ".",
		Command: command, Timeout: "10s", SkipPolicy: SkipForbid,
	}
}

func testMatrix(gates ...Gate) Matrix {
	slices.SortFunc(gates, func(left, right Gate) int { return strings.Compare(left.ID, right.ID) })
	return Matrix{Version: MatrixVersion, Gates: gates}
}

func cloneMatrix(matrix Matrix) Matrix {
	payload, _ := json.Marshal(matrix)
	var clone Matrix
	_ = json.Unmarshal(payload, &clone)
	return clone
}

func executeTestMatrix(t *testing.T, matrix Matrix, options Options) Report {
	t.Helper()
	root := repositoryRoot(t)
	options.Root = root
	options.GoBinary = filepath.Join(root, ".runtime", "toolchains", "go1.25.0", "bin", "go")
	options.ArtifactsDir = filepath.Join(t.TempDir(), "artifacts")
	options.Environment = []string{"PATH=" + os.Getenv("PATH")}
	report, err := Execute(context.Background(), matrix, options)
	if err != nil {
		t.Fatalf("execute matrix: %v", err)
	}
	return report
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func discoverModules(t *testing.T, root string) []string {
	t.Helper()
	var modules []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root && ignoredDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		if !entry.IsDir() && entry.Name() == "go.mod" {
			directory := filepath.Dir(path)
			relative, relErr := filepath.Rel(root, directory)
			if relErr != nil {
				return relErr
			}
			modules = append(modules, filepath.ToSlash(relative))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(modules)
	return modules
}

func discoverFuzzTargets(t *testing.T, root string) []string {
	t.Helper()
	var targets []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path != root && ignoredDirectory(entry.Name()) {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		directory, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && strings.HasPrefix(function.Name.Name, "Fuzz") {
				targets = append(targets, "fuzz:"+filepath.ToSlash(directory)+"/"+function.Name.Name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(targets)
	return targets
}

func ignoredDirectory(name string) bool {
	return name == ".git" || name == ".runtime" || name == ".build" ||
		name == "node_modules" || name == "vendor"
}

func argumentAfter(arguments []string, flag string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == flag {
			return arguments[index+1]
		}
	}
	return ""
}

func gateHasAssertion(gate Gate, kind, value string) bool {
	return slices.ContainsFunc(gate.Assertions, func(assertion Assertion) bool {
		return assertion.Kind == kind && assertion.Value == value
	})
}
