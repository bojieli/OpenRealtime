package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/tauvoice"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestRepositoryCensusPinsOwnedSuites(t *testing.T) {
	census := RepositoryCensus()
	if err := census.Validate(); err != nil {
		t.Fatalf("repository census: %v", err)
	}
	wants := map[string]int{SuiteScenario: 11, SuiteMeeting: 4, SuiteRealtimeCU: 16}
	for name, want := range wants {
		suite, exists := censusSuiteNamed(census, name)
		if !exists || len(suite.Cases) != want {
			t.Fatalf("suite %s = %d cases, want %d", name, len(suite.Cases), want)
		}
	}
	scenario, _ := censusSuiteNamed(census, SuiteScenario)
	if scenario.MinimumRepetitions != 15 {
		t.Fatalf("scenario repetition floor = %d, want 15", scenario.MinimumRepetitions)
	}
}

func TestReviewedDatasetManifestsPinRequiredPopulations(t *testing.T) {
	read := func(name string) []byte {
		payload, err := os.ReadFile(filepath.Join("..", "..", "datasets", "manifests", name))
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	if counts, err := parseFDB15Manifest(read("full-duplex-bench-v1.5.json")); err != nil || sumCounts(counts) != 498 {
		t.Fatalf("FDB v1.5 manifest = %v, %v", counts, err)
	}
	if counts, err := parseFDBV3Manifest(read("full-duplex-bench-v3.json")); err != nil || sumCounts(counts) != 100 {
		t.Fatalf("FDB v3 manifest = %v, %v", counts, err)
	}
	if counts, err := parseFDBenchManifest(read("fd-bench.json")); err != nil ||
		len(counts) != 21 || sumCounts(counts) != 6147 {
		t.Fatalf("FD-Bench manifest = %v, %v", counts, err)
	}
	if err := validateTauManifest(read("tau-voice.json")); err != nil {
		t.Fatalf("tau-Voice manifest: %v", err)
	}
	if _, err := parseFDB15Manifest([]byte(`{"observed_complete_samples":498,"observed_complete_samples":1}`)); err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("duplicate population field error = %v", err)
	}
}

func TestBuildExternalCensusUsesRealSuiteLoadersForCompleteMatrix(t *testing.T) {
	root := t.TempDir()
	fdbRoot := filepath.Join(root, "fdb")
	for condition, count := range map[string]int{
		"background_speech": 100, "talking_to_other": 100,
		"user_backchannel": 98, "user_interruption": 200,
	} {
		for index := 1; index <= count; index++ {
			directory := filepath.Join(fdbRoot, condition, strconv.Itoa(index))
			mustWriteFixture(t, filepath.Join(directory, "metadata.json"), []byte(
				`{"context_text":"hello","current_turn_text":"event","timestamps":[0.1,0.2]}`))
			mustWriteFixture(t, filepath.Join(directory, "input.wav"), nil)
		}
	}
	fdbv3Root := filepath.Join(root, "fdbv3")
	for domain, count := range map[string]int{
		"travel_identity": 20, "finance_billing": 25,
		"housing_location": 26, "ecommerce_support": 29,
	} {
		for index := 1; index <= count; index++ {
			name := domain + "-" + strconv.Itoa(index)
			directory := filepath.Join(fdbv3Root, name)
			metadata, _ := json.Marshal(map[string]any{
				"id": name, "domain": domain, "title": name, "difficulty": "test",
				"expected_tool_calls": []any{}, "disfluency_features": []any{},
			})
			mustWriteFixture(t, filepath.Join(directory, "metadata.json"), metadata)
			mustWriteFixture(t, filepath.Join(directory, "input.wav"), nil)
		}
	}
	fdbenchRoot := filepath.Join(root, "fdbench")
	for condition, count := range requiredFDBenchConditions() {
		for index := 1; index <= count; index++ {
			name := "conversation-" + strconv.Itoa(index)
			mustWriteFixture(t, filepath.Join(fdbenchRoot, condition, name+".wav"), nil)
			mustWriteFixture(t, filepath.Join(fdbenchRoot, condition, name+".timestamps"),
				[]byte(`[{"start":0,"end":16000}]`))
		}
	}
	inventory := TauTaskInventory{Version: 1, Revision: tauvoice.PinnedRevision}
	for _, domain := range tauvoice.Domains {
		count := map[string]int{
			"airline": tauvoice.AirlineTaskCount,
			"retail":  tauvoice.RetailTaskCount,
			"telecom": tauvoice.TelecomTaskCount,
		}[domain]
		for index := 1; index <= count; index++ {
			inventory.Tasks = append(inventory.Tasks, TauTask{
				Domain: domain, ID: "task-" + strconv.Itoa(index),
			})
		}
	}
	inventoryPayload, err := tauvoice.MarshalTaskInventory(inventory)
	if err != nil {
		t.Fatal(err)
	}
	manifest := func(name string) []byte {
		payload, err := os.ReadFile(filepath.Join("..", "..", "datasets", "manifests", name))
		if err != nil {
			t.Fatal(err)
		}
		return payload
	}
	source := func(index int) EvidenceRef {
		return EvidenceRef{Kind: "census-source", Location: "sources/" + strconv.Itoa(index),
			SHA256: strings.Repeat(strconv.Itoa(index), 64)}
	}
	external, err := BuildExternalCensus(ExternalCensusInput{
		FDB15Root: fdbRoot, FDB15Manifest: manifest("full-duplex-bench-v1.5.json"), FDB15Source: source(1),
		FDBV3Root: fdbv3Root, FDBV3Manifest: manifest("full-duplex-bench-v3.json"), FDBV3Source: source(2),
		FDBenchRoot: fdbenchRoot, FDBenchManifest: manifest("fd-bench.json"), FDBenchSource: source(3),
		TauManifest: manifest("tau-voice.json"), TauSource: source(4),
		TauInventory: inventoryPayload, TauInventorySource: source(5),
	})
	if err != nil {
		t.Fatalf("build external census through real loaders: %v", err)
	}
	if err := ValidateRequiredMatrix(MergeCensuses(RepositoryCensus(), external)); err != nil {
		t.Fatalf("loaded required matrix: %v", err)
	}
}

func mustWriteFixture(t testing.TB, path string, payload []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRequiredMatrixRejectsPopulationSubstitution(t *testing.T) {
	valid := requiredTestCensus(t)
	if err := ValidateRequiredMatrix(valid); err != nil {
		t.Fatalf("valid required matrix: %v", err)
	}

	tests := []struct {
		name string
		edit func(*Census)
		want string
	}{
		{
			name: "repository task substitution",
			edit: func(census *Census) {
				suite := mutableCensusSuite(census, SuiteMeeting)
				suite.Cases[0].ID += "-substitute"
			},
			want: "repository-owned exact task census",
		},
		{
			name: "missing fd bench row",
			edit: func(census *Census) {
				suite := mutableCensusSuite(census, SuiteFDBench)
				suite.Cases = suite.Cases[:len(suite.Cases)-1]
			},
			want: "condition populations",
		},
		{
			name: "condition substitution at same total",
			edit: func(census *Census) {
				suite := mutableCensusSuite(census, SuiteFDBench)
				suite.Cases[0].Condition = suite.Cases[len(suite.Cases)-1].Condition
			},
			want: "condition populations",
		},
		{
			name: "tau modes differ",
			edit: func(census *Census) {
				suite := mutableCensusSuite(census, SuiteTauRegular)
				suite.Cases[0].ID += "-substitute"
			},
			want: "same exact tasks",
		},
		{
			name: "scenario diagnostic repetition count",
			edit: func(census *Census) {
				mutableCensusSuite(census, SuiteScenario).MinimumRepetitions = 14
			},
			want: "at least 15 repetitions",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			census := cloneCensus(valid)
			test.edit(&census)
			census = sealCensus(census)
			if err := ValidateRequiredMatrix(census); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("matrix mutation error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestManifestCensusRequiresUniformPreregisteredRepetitionSlabs(t *testing.T) {
	census := requiredTestCensus(t)
	cell := bench.Reference()
	cell.Execution = legacyRequirement(t)
	bases := map[string]EvidenceRef{}
	for index, suite := range census.Suites {
		bases[suite.Name] = EvidenceRef{
			Kind:     AcceptanceBasisKind,
			Location: "baselines/" + strings.ReplaceAll(suite.Name, "/", "-") + ".json",
			SHA256:   strings.Repeat(strconv.Itoa((index%8)+1), 64),
		}
	}
	manifest := requiredTestManifest(census, bases, cell, nil, reproducibleProvenance(time.Now().UTC()))
	scenario, _ := suiteNamed(manifest, SuiteScenario)
	scenario.Cases[1].Repetitions[0] = "trial-substituted"
	for index := range manifest.Suites {
		if manifest.Suites[index].Name == SuiteScenario {
			manifest.Suites[index] = scenario
		}
	}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("repetition-substitution fixture should remain internally valid: %v", err)
	}
	if err := ValidateManifestCensus(manifest, census); err == nil ||
		!strings.Contains(err.Error(), "one preregistered repetition set") {
		t.Fatalf("nonuniform repetitions error = %v", err)
	}
}

func TestLocalStoreIsDigestVerifiedCreateOnlyAndRootConfined(t *testing.T) {
	root := t.TempDir()
	store := LocalStore{Root: root}
	reference, err := store.ArchiveBytes(EvidenceKindResult, ".json", []byte("first\n"))
	if err != nil {
		t.Fatalf("archive bytes: %v", err)
	}
	repeated, err := store.ArchiveBytes(EvidenceKindResult, ".json", []byte("first\n"))
	if err != nil || repeated != reference {
		t.Fatalf("idempotent archive = %+v, %v", repeated, err)
	}
	fragment := reference
	fragment.Location += "#task=one"
	if payload, err := store.Resolve(fragment); err != nil || string(payload) != "first\n" {
		t.Fatalf("resolve fragment = %q, %v", payload, err)
	}
	listed, err := store.List(EvidenceKindResult)
	if err != nil || len(listed) != 1 || listed[0] != reference {
		t.Fatalf("listed content-addressed evidence = %+v, %v", listed, err)
	}

	if _, err := store.Put("test", "campaign/fixed.json", []byte("one")); err != nil {
		t.Fatalf("put fixed artifact: %v", err)
	}
	if _, err := store.Put("test", "campaign/fixed.json", []byte("two")); err == nil ||
		!strings.Contains(err.Error(), "different bytes") {
		t.Fatalf("create-only overwrite error = %v", err)
	}
	badDigest := reference
	badDigest.SHA256 = strings.Repeat("0", 64)
	if _, err := store.Resolve(badDigest); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("digest mutation error = %v", err)
	}
	if _, err := store.Put("test", "../outside", []byte("escape")); err == nil {
		t.Fatal("parent traversal was accepted")
	}

	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatalf("make escape symlink: %v", err)
	}
	if _, err := store.Put("test", "escape/written", []byte("escape")); err == nil {
		t.Fatal("out-of-root symlink was accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "written")); !os.IsNotExist(err) {
		t.Fatalf("store wrote through escape symlink: %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(reference.Location)),
		[]byte("externally corrupted\n"), 0o644); err != nil {
		t.Fatalf("corrupt archived evidence: %v", err)
	}
	listed, err = store.List(EvidenceKindResult)
	if err != nil || len(listed) != 1 || listed[0] != reference {
		t.Fatalf("list must retain advertised identity for later verification: %+v, %v", listed, err)
	}
	if _, err := store.Resolve(listed[0]); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("listed corrupted evidence resolved without a digest error: %v", err)
	}
}

func TestLocalStoreListingRejectsNonCanonicalEntries(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "artifacts", EvidenceKindResult)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "not-content-addressed.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (LocalStore{Root: root}).List(EvidenceKindResult); err == nil ||
		!strings.Contains(err.Error(), "not content-addressed") {
		t.Fatalf("non-content-addressed listing error = %v", err)
	}

	valid := strings.Repeat("a", 64) + ".json"
	if digest, err := artifactFilenameDigest(valid); err != nil || digest != strings.Repeat("a", 64) {
		t.Fatalf("valid content-addressed filename = %q, %v", digest, err)
	}
	for _, invalid := range []string{
		strings.Repeat("A", 64) + ".json",
		strings.Repeat("a", 64) + "json",
		strings.Repeat("a", 64) + "..json",
		strings.Repeat("a", 64) + ".extension-too-long",
	} {
		if _, err := artifactFilenameDigest(invalid); err == nil {
			t.Errorf("invalid content-addressed filename %q was accepted", invalid)
		}
	}
}

func TestImportCompatibilityRetainsRowsAndExactLegacyIdentity(t *testing.T) {
	store := LocalStore{Root: t.TempDir()}
	contract, err := store.ArchiveBytes("suite-contract", ".json", []byte("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := SealImportProfile(ImportProfile{
		Version: ImportProfileVersion, Suite: "small", Format: FormatBenchResult,
		BaselineExecutionKind:  bench.ExecutionLegacy,
		CandidateExecutionKind: bench.ExecutionGraphNative,
		Interaction:            StateRule{Kind: RuleTaskPassed},
		Deadline:               StateRule{Kind: RuleCompleted},
		Safety:                 StateRule{Kind: RuleMetricAtMost, Metric: "violations", Threshold: 0},
		Latencies:              []LatencyMapping{{Name: "latency", Metric: "latency_ms", Unit: "ms"}},
		Evidence:               []EvidenceRef{contract},
	})
	if err != nil {
		t.Fatalf("seal profile: %v", err)
	}
	profilePayload, _ := MarshalImportProfile(profile)
	profileRef, err := store.ArchiveBytes(EvidenceKindImportProfile, ".json", profilePayload)
	if err != nil {
		t.Fatal(err)
	}

	requirement := legacyRequirement(t)
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionLegacy,
		Scope: "case-1", Legacy: &bench.LegacyEvidence{
			Binding: "cascade", RuntimeDigest: "sha256:" + strings.Repeat("a", 64),
		},
	})
	if err != nil {
		t.Fatalf("freeze legacy evidence: %v", err)
	}
	result := bench.Result{
		Suite: "small", Cell: bench.Reference(), Expected: 1,
		Provenance: reproducibleProvenance(time.Now().UTC().Add(-time.Minute)),
		Tasks: []bench.TaskOutcome{{
			ID: "case-1", Completed: true, Passed: true,
			Metrics: map[string]float64{"latency_ms": 12, "violations": 0}, Execution: &evidence,
		}},
	}
	result.Cell.Execution = requirement
	result.Finish()
	resultPayload, _ := json.MarshalIndent(result, "", "  ")
	resultPayload = append(resultPayload, '\n')
	resultRef, err := store.ArchiveBytes(EvidenceKindResult, ".json", resultPayload)
	if err != nil {
		t.Fatal(err)
	}
	census := sealCensus(Census{Version: CensusVersion, Sources: []EvidenceRef{},
		Suites: []CensusSuite{{Name: "small", MinimumRepetitions: 1,
			Cases: []CensusCase{{Condition: "regular", ID: "case-1"}}}}})
	attempts, err := ImportBenchResult(store, census, profile, profileRef, ArmBaseline,
		ResultImport{Suite: "small", Reference: resultRef, Repetition: "trial-1"})
	if err != nil {
		t.Fatalf("import result: %v", err)
	}
	if len(attempts) != 1 || !attempts[0].Completed || attempts[0].Key.Repetition != "trial-1" {
		t.Fatalf("imported attempts = %+v", attempts)
	}
	for _, name := range []string{"graph_sha256", "configuration_sha256", "deployment_sha256",
		"runtime_sha256", "legacy_runtime_sha256"} {
		if _, present := axisValue(attempts[0].Axes, name); !present {
			t.Fatalf("imported legacy attempt omits %s", name)
		}
	}

	result.Tasks[0].ID = "unknown"
	result.Finish()
	resultPayload, _ = json.MarshalIndent(result, "", "  ")
	resultPayload = append(resultPayload, '\n')
	unknownRef, _ := store.ArchiveBytes(EvidenceKindResult, ".json", resultPayload)
	attempts, err = ImportBenchResult(store, census, profile, profileRef, ArmBaseline,
		ResultImport{Suite: "small", Reference: unknownRef, Repetition: "trial-1"})
	if err != nil || len(attempts) < 1 || attempts[0].Completed ||
		!strings.Contains(attempts[0].Error, "authoritative census") {
		t.Fatalf("unknown row was not retained as an incomplete refusal: %+v, %v", attempts, err)
	}
}

func BenchmarkValidateRequiredMatrix(b *testing.B) {
	census := requiredTestCensus(b)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := ValidateRequiredMatrix(census); err != nil {
			b.Fatal(err)
		}
	}
}

func requiredTestCensus(tb testing.TB) Census {
	tb.Helper()
	census := RepositoryCensus()
	for index := 0; index < 5; index++ {
		census.Sources = append(census.Sources, EvidenceRef{
			Kind: "source", Location: "sources/" + strconv.Itoa(index) + ".json",
			SHA256: strings.Repeat(strconv.Itoa(index+1), 64),
		})
	}
	appendCounts := func(name string, counts map[string]int, id func(string, int) string) {
		suite := CensusSuite{Name: name, MinimumRepetitions: 1}
		conditions := make([]string, 0, len(counts))
		for condition := range counts {
			conditions = append(conditions, condition)
		}
		sortStrings(conditions)
		for _, condition := range conditions {
			for index := 1; index <= counts[condition]; index++ {
				suite.Cases = append(suite.Cases, CensusCase{
					Condition: condition, ID: id(condition, index),
				})
			}
		}
		census.Suites = append(census.Suites, suite)
	}
	appendCounts(SuiteFDB15, map[string]int{
		"background_speech": 100, "talking_to_other": 100,
		"user_backchannel": 98, "user_interruption": 200,
	}, func(condition string, index int) string { return condition + "/" + strconv.Itoa(index) })
	appendCounts(SuiteFDBV3, map[string]int{
		"travel_identity": 20, "finance_billing": 25,
		"housing_location": 26, "ecommerce_support": 29,
	}, func(condition string, index int) string { return condition + "-" + strconv.Itoa(index) })
	appendCounts(SuiteFDBench, requiredFDBenchConditions(),
		func(condition string, index int) string { return condition + "/" + strconv.Itoa(index) })
	tau := CensusSuite{Name: SuiteTauControl, MinimumRepetitions: 1}
	for domainIndex, domain := range []string{"airline", "retail", "telecom"} {
		count := 93
		if domainIndex == 2 {
			count = 92
		}
		for index := 1; index <= count; index++ {
			tau.Cases = append(tau.Cases, CensusCase{
				Condition: domain, ID: domain + "/task-" + strconv.Itoa(index),
			})
		}
	}
	census.Suites = append(census.Suites, tau)
	tau.Name = SuiteTauRegular
	tau.Cases = append([]CensusCase(nil), tau.Cases...)
	census.Suites = append(census.Suites, tau)
	census = sealCensus(census)
	if err := ValidateRequiredMatrix(census); err != nil {
		tb.Fatalf("construct required census: %v", err)
	}
	return census
}

func mutableCensusSuite(census *Census, name string) *CensusSuite {
	for index := range census.Suites {
		if census.Suites[index].Name == name {
			return &census.Suites[index]
		}
	}
	panic("unknown census suite " + name)
}

func cloneCensus(census Census) Census {
	result := census
	result.Sources = append([]EvidenceRef(nil), census.Sources...)
	result.Suites = append([]CensusSuite(nil), census.Suites...)
	for index := range result.Suites {
		result.Suites[index].Cases = append([]CensusCase(nil), result.Suites[index].Cases...)
	}
	return result
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for current := index; current > 0 && values[current] < values[current-1]; current-- {
			values[current], values[current-1] = values[current-1], values[current]
		}
	}
}

func reproducibleProvenance(started time.Time) bench.Provenance {
	return bench.Provenance{
		Revision: "revision", ExecutableSHA256: strings.Repeat("b", 64),
		Machine:   bench.Machine{CPU: "test", Cores: 2, OS: "linux", Arch: "amd64", GoVersion: "go1.25"},
		StartedAt: started.UTC().Format(time.RFC3339Nano),
	}
}

func legacyRequirement(t testing.TB) bench.ExecutionRequirement {
	t.Helper()
	requirement, err := bench.RequireLegacy("cascade", binding.ArchitectureIdentity{})
	if err != nil {
		t.Fatalf("legacy requirement: %v", err)
	}
	return requirement
}

func graphExecutionFixture(t testing.TB) (bench.Cell, bench.GraphAttestor) {
	t.Helper()
	digest := func(character string) string { return "sha256:" + strings.Repeat(character, 64) }
	message := element.Event(element.Named("text.Message"))
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "migration-test-agent", Revision: 1,
		Nodes: []ir.Node{
			{
				ID: "source", Element: element.Identity{Name: "test.Source", Revision: 1, Digest: digest("1")},
				Implementation: "go://test/source@1", ConfigReference: "values://test/source",
				ConfigDigest: digest("a"), Ports: []ir.Port{{
					Name: "out", Direction: element.Output, Type: message, Cardinality: element.One,
				}},
			},
			{
				ID: "sink", Element: element.Identity{Name: "test.Sink", Revision: 1, Digest: digest("2")},
				Implementation: "go://test/sink@1", ConfigReference: "values://test/sink",
				ConfigDigest: digest("b"), Ports: []ir.Port{{
					Name: "in", Direction: element.Input, Type: message, Cardinality: element.One,
				}},
			},
		},
		Edges: []ir.Edge{{
			ID: "source-to-sink", From: ir.Endpoint{Node: "source", Port: "out"},
			To: ir.Endpoint{Node: "sink", Port: "in"}, Type: message,
			Delivery: ir.Lossless, Ordering: "fifo", Depth: 1,
		}},
	})
	if err != nil {
		t.Fatalf("freeze graph fixture: %v", err)
	}
	configuration := bench.ArtifactIdentity{
		ID: "values://migration-test", Revision: "v1", Digest: digest("c"),
	}
	nodes := map[string]ir.Node{}
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	resolution := bench.LiveResolution{Elements: []bench.ElementResolution{
		{
			Node: "source", Element: nodes["source"].Element,
			Implementation: nodes["source"].Implementation,
			Runtime:        bench.ArtifactIdentity{ID: "builtin://source", Revision: "v1"},
		},
		{
			Node: "sink", Element: nodes["sink"].Element,
			Implementation: nodes["sink"].Implementation,
			Runtime:        bench.ArtifactIdentity{ID: "builtin://sink", Revision: "v1"},
		},
	}}
	requirement, err := bench.RequireGraph(graph, configuration, resolution)
	if err != nil {
		t.Fatalf("require graph fixture: %v", err)
	}
	cell := bench.Reference()
	cell.Execution = requirement
	attestor := bench.GraphAttestor{
		Graph: graph, Configuration: configuration,
		Resolve: func(context.Context, bench.AttestationRequest) (bench.LiveResolution, error) {
			return resolution, nil
		},
	}
	return cell, attestor
}

func axisValue(axes []Axis, name string) (string, bool) {
	for _, axis := range axes {
		if axis.Name == name {
			return axis.Value, true
		}
	}
	return "", false
}

func TestPreregisteredDocumentsRejectNonCanonicalAndDuplicateJSON(t *testing.T) {
	profile, err := SealImportProfile(ImportProfile{
		Version: ImportProfileVersion, Suite: "small", Format: FormatBenchResult,
		BaselineExecutionKind:  bench.ExecutionLegacy,
		CandidateExecutionKind: bench.ExecutionGraphNative,
		Interaction:            StateRule{Kind: RuleNotApplicable}, Deadline: StateRule{Kind: RuleNotApplicable},
		Safety: StateRule{Kind: RuleNotApplicable}, Evidence: []EvidenceRef{{
			Kind: "contract", Location: "contract.json", SHA256: strings.Repeat("a", 64),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := MarshalImportProfile(profile)
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeImportProfile(bytes.NewReader(compact.Bytes())); err == nil ||
		!strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("compact profile error = %v", err)
	}
	duplicate := []byte(`{"version":1,"version":1}`)
	if _, err := DecodeImportProfile(bytes.NewReader(duplicate)); err == nil ||
		!strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("duplicate profile error = %v", err)
	}
}

func TestRegistrationAndLaunchPreflightFailClosedBeforeWork(t *testing.T) {
	store := LocalStore{Root: t.TempDir()}
	cell := bench.Reference()
	cell.Execution = legacyRequirement(t)
	provenance := reproducibleProvenance(time.Now().UTC().Add(-time.Minute))
	manifest, census, profiles, registeredAt := persistRequiredStudy(t, store, cell, nil, provenance)

	registration, registrationRef, err := RegisterStudy(store, "campaign/registration.json",
		manifest, census, profiles, registeredAt)
	if err != nil {
		t.Fatalf("register study: %v", err)
	}
	if payload, err := store.Resolve(registrationRef); err != nil {
		t.Fatalf("resolve registration: %v", err)
	} else if decoded, err := DecodeRegistration(bytes.NewReader(payload)); err != nil ||
		decoded.RegistrationID != registration.RegistrationID {
		t.Fatalf("decode registration = %+v, %v", decoded, err)
	}
	if _, _, _, err := VerifyRegistration(store, registration, registeredAt); err == nil ||
		!strings.Contains(err.Error(), "precedes") {
		t.Fatalf("equal-time candidate verification error = %v", err)
	}

	loadedManifest, _, _, err := VerifyRegistration(store, registration, registeredAt.Add(time.Second))
	if err != nil {
		t.Fatalf("verify registered study: %v", err)
	}
	tamperedPolicy := loadedManifest
	tamperedPolicy.Suites = append([]SuiteSpec(nil), loadedManifest.Suites...)
	tamperedPolicy.Suites[0].Policy.Pass.Margin = 0.49
	if err := validateAcceptanceBases(store, tamperedPolicy, registration.AcceptanceBases, registeredAt); err == nil || !strings.Contains(err.Error(), "differs from its baseline-only acceptance basis") {
		t.Fatalf("post-baseline policy retuning error = %v", err)
	}
	meeting, _ := suiteNamed(loadedManifest, SuiteMeeting)
	keys := sortedAttemptKeys(suiteKeys(meeting))
	request := LaunchRequest{
		Arm: ArmBaseline, Suite: SuiteMeeting, Keys: keys, Cell: cell,
		Provenance: provenance, ObservedAt: registeredAt.Add(time.Second),
	}
	if err := ValidateLaunch(store, registrationRef, request); err != nil {
		t.Fatalf("complete meeting repetition preflight: %v", err)
	}
	intent, intentRef, err := RegisterLaunchIntent(store, registrationRef, request)
	if err != nil {
		t.Fatalf("register launch intent: %v", err)
	}
	retainedResult := bench.Result{
		Suite: SuiteMeeting, Cell: cell, Provenance: reproducibleProvenance(request.ObservedAt),
		Expected: len(keys),
	}
	retainedResult.Finish()
	retainedPayload, _ := json.MarshalIndent(retainedResult, "", "  ")
	retainedPayload = append(retainedPayload, '\n')
	resultRef, outcomeRef, err := RetainLaunchOutcome(store, intentRef, retainedPayload)
	if err != nil {
		t.Fatalf("retain launch outcome: %v", err)
	}
	outcome, resolvedIntent, err := ResolveLaunchOutcome(store, outcomeRef)
	if err != nil || resolvedIntent.IntentID != intent.IntentID || outcome.Result != resultRef {
		t.Fatalf("resolved launch outcome = %+v intent %+v, %v", outcome, resolvedIntent, err)
	}
	request.Keys = request.Keys[:len(request.Keys)-1]
	if err := ValidateLaunch(store, registrationRef, request); err == nil ||
		!strings.Contains(err.Error(), "filters out") {
		t.Fatalf("filtered launch error = %v", err)
	}
	request.Keys = keys
	request.Provenance.Modified = true
	if err := ValidateLaunch(store, registrationRef, request); err == nil ||
		!strings.Contains(err.Error(), "not reproducible") {
		t.Fatalf("dirty launch error = %v", err)
	}

	scenario, _ := suiteNamed(loadedManifest, SuiteScenario)
	allScenario := suiteKeys(scenario)
	var firstRepetition []AttemptKey
	for key := range allScenario {
		if key.Repetition == "trial-1" {
			firstRepetition = append(firstRepetition, key)
		}
	}
	request = LaunchRequest{
		Arm: ArmBaseline, Suite: SuiteScenario, Keys: firstRepetition, Cell: cell,
		Provenance: provenance, ObservedAt: registeredAt.Add(time.Second),
	}
	if err := ValidateLaunch(store, registrationRef, request); err != nil {
		t.Fatalf("complete scenario repetition slab: %v", err)
	}
}

func TestRegisteredImportArchivesIncompleteComparisonAndRefusesOverwrite(t *testing.T) {
	store := LocalStore{Root: t.TempDir()}
	cell := bench.Reference()
	cell.Execution = legacyRequirement(t)
	provenance := reproducibleProvenance(time.Now().UTC().Add(-time.Hour))
	manifestRef, censusRef, profiles, registeredAt := persistRequiredStudy(t, store, cell, nil, provenance)
	_, registrationRef, err := RegisterStudy(store, "campaign/compare-registration.json",
		manifestRef, censusRef, profiles, registeredAt)
	if err != nil {
		t.Fatalf("register comparison: %v", err)
	}

	meetingCensus := RepositoryCensus()
	meetingSuite, _ := censusSuiteNamed(meetingCensus, SuiteMeeting)
	caseID := meetingSuite.Cases[0].ID
	makeResult := func(started time.Time, candidate, passed bool) EvidenceRef {
		resultCell := cell
		task := bench.TaskOutcome{
			ID: caseID, Completed: true, Passed: passed,
			Metrics: map[string]float64{"latency_ms": 10},
		}
		if !candidate {
			evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
				FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionLegacy,
				Scope: caseID, Legacy: &bench.LegacyEvidence{
					Binding: "cascade", RuntimeDigest: "sha256:" + strings.Repeat("a", 64),
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			task.Execution = &evidence
		}
		result := bench.Result{
			Suite: SuiteMeeting, Cell: resultCell, Provenance: reproducibleProvenance(started),
			Expected: 1, Tasks: []bench.TaskOutcome{task},
		}
		result.Finish()
		payload, _ := json.MarshalIndent(result, "", "  ")
		payload = append(payload, '\n')
		reference, err := store.ArchiveBytes(EvidenceKindResult, ".json", payload)
		if err != nil {
			t.Fatal(err)
		}
		return reference
	}
	baselineRef := makeResult(registeredAt.Add(time.Second), false, true)
	candidateRef := makeResult(registeredAt.Add(2*time.Second), true, true)
	registration, err := ResolveRegistration(store, registrationRef)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _, _, err := VerifyRegistration(store, registration, registeredAt.Add(500*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	meeting, _ := suiteNamed(manifest, SuiteMeeting)
	request := LaunchRequest{
		Arm: ArmBaseline, Suite: SuiteMeeting, Keys: sortedAttemptKeys(suiteKeys(meeting)),
		Cell: cell, Provenance: provenance, ObservedAt: registeredAt.Add(500 * time.Millisecond),
	}
	_, intentRef, err := RegisterLaunchIntent(store, registrationRef, request)
	if err != nil {
		t.Fatal(err)
	}
	baselinePayload, _ := store.Resolve(baselineRef)
	_, baselineOutcome, err := RetainLaunchOutcome(store, intentRef, baselinePayload)
	if err != nil {
		t.Fatal(err)
	}
	baseline := []ResultImport{{Suite: SuiteMeeting, Outcome: baselineOutcome}}
	candidate := []ResultImport{{Suite: SuiteMeeting, Reference: candidateRef, Repetition: "trial-1"}}
	report, reportRef, err := CompareRegistered(store, registrationRef, baseline, candidate, nil,
		"campaign/refused-report.json")
	if err != nil {
		t.Fatalf("compare incomplete registered inputs: %v", err)
	}
	if report.Reportable || report.Accepted || len(report.Attempts) == 0 {
		t.Fatalf("incomplete comparison state = reportable %v accepted %v attempts %d",
			report.Reportable, report.Accepted, len(report.Attempts))
	}
	payload, err := store.Resolve(reportRef)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeWithHistory(bytes.NewReader(payload), nil); err != nil {
		t.Fatalf("archived refusal is not canonical: %v", err)
	}

	changedRef := makeResult(registeredAt.Add(3*time.Second), true, false)
	candidate[0].Reference = changedRef
	if _, _, err := CompareRegistered(store, registrationRef, baseline, candidate, nil,
		"campaign/refused-report.json"); err == nil || !strings.Contains(err.Error(), "different bytes") {
		t.Fatalf("changed rerun overwrote failed report: %v", err)
	}
	if _, err := store.Resolve(changedRef); err != nil {
		t.Fatalf("changed candidate result was not retained: %v", err)
	}
}

func TestCandidateLaunchDiscoveryRetainsOrphansAndAutoImportsOutcomes(t *testing.T) {
	store := LocalStore{Root: t.TempDir()}
	baselineCell := bench.Reference()
	baselineCell.Execution = legacyRequirement(t)
	candidateCell, attestor := graphExecutionFixture(t)
	provenance := reproducibleProvenance(time.Now().UTC().Add(-time.Minute))
	manifestRef, censusRef, profiles, registeredAt := persistRequiredStudy(
		t, store, baselineCell, &candidateCell, provenance)
	_, registrationRef, err := RegisterStudy(store, "campaign/discovery-registration.json",
		manifestRef, censusRef, profiles, registeredAt)
	if err != nil {
		t.Fatal(err)
	}
	registration, _ := ResolveRegistration(store, registrationRef)
	manifest, _, _, err := VerifyRegistration(store, registration, registeredAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	meeting, _ := suiteNamed(manifest, SuiteMeeting)
	candidateProvenance := reproducibleProvenance(registeredAt.Add(2 * time.Second))
	candidateProvenance.Revision = "candidate-revision"
	candidateProvenance.ExecutableSHA256 = strings.Repeat("c", 64)
	request := LaunchRequest{
		Arm: ArmCandidate, Suite: SuiteMeeting, Keys: sortedAttemptKeys(suiteKeys(meeting)),
		Cell: candidateCell, Provenance: candidateProvenance,
		ObservedAt: registeredAt.Add(time.Second),
	}
	_, intentRef, err := RegisterLaunchIntent(store, registrationRef, request)
	if err != nil {
		t.Fatalf("register candidate intent: %v", err)
	}
	report, _, err := CompareRegistered(store, registrationRef, nil, nil, nil,
		"campaign/orphan-report.json")
	if err != nil {
		t.Fatalf("compare orphaned launch: %v", err)
	}
	orphans := 0
	for _, observed := range report.Attempts {
		if strings.Contains(observed.Attempt.Error, "no retained result outcome") {
			orphans++
		}
	}
	if orphans != len(meeting.Cases) {
		t.Fatalf("orphaned attempts = %d, want %d", orphans, len(meeting.Cases))
	}

	caseID := meeting.Cases[0].ID
	evidence, err := attestor.Attest(context.Background(), bench.AttestationRequest{
		Scope: caseID, Status: binding.Status{Graph: binding.ArchitectureIdentity{
			ID:          candidateCell.Execution.Graph.Graph.ID,
			Revision:    int(candidateCell.Execution.Graph.Graph.Revision),
			Fingerprint: candidateCell.Execution.Graph.Graph.Fingerprint,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteMeeting, Cell: candidateCell, Provenance: candidateProvenance,
		Expected: 1, Tasks: []bench.TaskOutcome{{
			ID: caseID, Completed: true, Passed: true,
			Metrics: map[string]float64{"latency_ms": 5}, Execution: &evidence,
		}},
	}
	result.Finish()
	payload, _ := json.MarshalIndent(result, "", "  ")
	payload = append(payload, '\n')
	_, outcomeRef, err := RetainLaunchOutcome(store, intentRef, payload)
	if err != nil {
		t.Fatalf("retain candidate launch outcome: %v", err)
	}
	baselineRequest := LaunchRequest{
		Arm: ArmBaseline, Suite: SuiteMeeting, Keys: sortedAttemptKeys(suiteKeys(meeting)),
		Cell: baselineCell, Provenance: provenance, ObservedAt: registeredAt.Add(time.Second),
	}
	_, baselineIntentRef, err := RegisterLaunchIntent(store, registrationRef, baselineRequest)
	if err != nil {
		t.Fatalf("register baseline intent: %v", err)
	}
	report, _, err = CompareRegistered(store, registrationRef, nil, nil, nil,
		"campaign/auto-import-report.json")
	if err != nil {
		t.Fatalf("auto-import retained outcome: %v", err)
	}
	foundOutcome := false
	for _, observed := range report.Attempts {
		for _, reference := range observed.Attempt.Evidence {
			if reference == outcomeRef {
				foundOutcome = true
			}
		}
	}
	if !foundOutcome {
		t.Fatal("registered comparison did not discover the retained candidate outcome")
	}
	baselineOrphans := 0
	for _, observed := range report.Attempts {
		if observed.Arm != ArmBaseline ||
			!strings.Contains(observed.Attempt.Error, "no retained result outcome") {
			continue
		}
		for _, reference := range observed.Attempt.Evidence {
			if reference == baselineIntentRef {
				baselineOrphans++
			}
		}
	}
	if baselineOrphans != len(meeting.Cases) {
		t.Fatalf("discovered baseline orphan attempts = %d, want %d",
			baselineOrphans, len(meeting.Cases))
	}
}

func TestStoredHistoryResolvesCompleteLineageInAnyOrder(t *testing.T) {
	store := LocalStore{Root: t.TempDir()}
	failed := failedFullReport(t)
	failedPayload, err := failed.MarshalWithHistory(nil)
	if err != nil {
		t.Fatal(err)
	}
	failedRef, err := store.ArchiveBytes(EvidenceKindReport, ".json", failedPayload)
	if err != nil {
		t.Fatal(err)
	}
	manifest := testManifest()
	manifest.Campaign = CampaignPlan{
		CampaignID: manifest.Campaign.CampaignID, RunID: "diagnostic-stored-history", Kind: RunDiagnostic,
		Predecessors: []ReportReference{mustReference(t, failed)},
		DiagnosedFailures: []FailureReference{{
			ReportID: failed.ReportID, Gate: "synthetic/safety/zero_tolerance",
		}},
	}
	baseline, candidate := testAttempts()
	diagnostic := CompareWithHistory(manifest, baseline, candidate, []Report{failed})
	diagnosticPayload, err := diagnostic.MarshalWithHistory([]Report{failed})
	if err != nil {
		t.Fatal(err)
	}
	diagnosticRef, err := store.ArchiveBytes(EvidenceKindReport, ".json", diagnosticPayload)
	if err != nil {
		t.Fatal(err)
	}
	history, err := ResolveReportHistory(store, []EvidenceRef{diagnosticRef, failedRef})
	if err != nil || len(history) != 2 {
		t.Fatalf("resolve reversed complete history = %d reports, %v", len(history), err)
	}
	if _, err := ResolveReportHistory(store, []EvidenceRef{diagnosticRef}); err == nil ||
		!strings.Contains(err.Error(), "complete lineage") {
		t.Fatalf("incomplete stored history error = %v", err)
	}
}

func persistRequiredStudy(
	t testing.TB, store LocalStore, baselineCell bench.Cell, candidateCell *bench.Cell,
	provenance bench.Provenance,
) (EvidenceRef, EvidenceRef, []EvidenceRef, time.Time) {
	t.Helper()
	census := requiredTestCensus(t)
	census.Sources = nil
	for index := 0; index < 5; index++ {
		reference, err := store.Put("census-source", "sources/source-"+strconv.Itoa(index)+".json",
			[]byte("source-"+strconv.Itoa(index)+"\n"))
		if err != nil {
			t.Fatalf("store census source: %v", err)
		}
		census.Sources = append(census.Sources, reference)
	}
	census = sealCensus(census)
	censusPayload, err := MarshalCensus(census)
	if err != nil {
		t.Fatalf("marshal census: %v", err)
	}
	censusRef, err := store.ArchiveBytes(EvidenceKindCensus, ".json", censusPayload)
	if err != nil {
		t.Fatalf("store census: %v", err)
	}
	bases := map[string]EvidenceRef{}
	for suiteIndex, suite := range census.Suites {
		baselineEvidence, err := store.Put("baseline-observation",
			"baselines/"+strings.ReplaceAll(suite.Name, "/", "-")+"-input.json",
			[]byte("baseline input for "+suite.Name+"\n"))
		if err != nil {
			t.Fatalf("store baseline evidence: %v", err)
		}
		basis, err := SealAcceptanceBasis(AcceptanceBasis{
			Version: AcceptanceBasisVersion, Suite: suite.Name,
			CreatedAt:          time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
			MinimumRepetitions: suite.MinimumRepetitions,
			BaselineEvidence:   []EvidenceRef{baselineEvidence},
			Decision:           acceptanceDecision(requiredTestPolicy(suite, suiteIndex, EvidenceRef{})),
		})
		if err != nil {
			t.Fatalf("seal %s acceptance basis: %v", suite.Name, err)
		}
		payload, _ := MarshalAcceptanceBasis(basis)
		reference, err := store.ArchiveBytes(AcceptanceBasisKind, ".json", payload)
		if err != nil {
			t.Fatalf("store %s acceptance basis: %v", suite.Name, err)
		}
		bases[suite.Name] = reference
	}
	manifestValue := requiredTestManifest(census, bases, baselineCell, candidateCell, provenance)
	manifestPayload, err := MarshalManifest(manifestValue)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	manifestRef, err := store.ArchiveBytes(EvidenceKindManifest, ".json", manifestPayload)
	if err != nil {
		t.Fatalf("store manifest: %v", err)
	}

	var profileRefs []EvidenceRef
	for _, suite := range census.Suites {
		contract, err := store.Put("suite-contract", "contracts/"+strings.ReplaceAll(suite.Name, "/", "-")+".json",
			[]byte("contract for "+suite.Name+"\n"))
		if err != nil {
			t.Fatalf("store suite contract: %v", err)
		}
		format := FormatBenchResult
		if suite.Name == SuiteScenario {
			format = FormatArchitectureResult
		}
		profile, err := SealImportProfile(ImportProfile{
			Version: ImportProfileVersion, Suite: suite.Name, Format: format,
			BaselineExecutionKind:  bench.ExecutionLegacy,
			CandidateExecutionKind: bench.ExecutionGraphNative,
			Interaction:            StateRule{Kind: RuleNotApplicable},
			Deadline:               StateRule{Kind: RuleNotApplicable},
			Safety:                 StateRule{Kind: RuleNotApplicable},
			Latencies:              []LatencyMapping{{Name: "latency", Metric: "latency_ms", Unit: "ms"}},
			Evidence:               []EvidenceRef{contract},
		})
		if err != nil {
			t.Fatalf("seal %s profile: %v", suite.Name, err)
		}
		payload, _ := MarshalImportProfile(profile)
		reference, err := store.ArchiveBytes(EvidenceKindImportProfile, ".json", payload)
		if err != nil {
			t.Fatalf("store %s profile: %v", suite.Name, err)
		}
		profileRefs = append(profileRefs, reference)
	}
	return manifestRef, censusRef, profileRefs, time.Now().UTC()
}

func requiredTestManifest(
	census Census, bases map[string]EvidenceRef, baselineCell bench.Cell, candidateCell *bench.Cell,
	provenance bench.Provenance,
) Manifest {
	dummy := func(character string) string { return strings.Repeat(character, 64) }
	manifest := Manifest{
		Version:  ManifestVersion,
		Campaign: CampaignPlan{CampaignID: "required-migration", RunID: "full-1", Kind: RunFull},
		Baseline: ArmDefinition{Name: "legacy"}, Candidate: ArmDefinition{Name: "graph-native"},
		FixedAxes: []Axis{
			{Name: "source_modified", Value: "false"},
			{Name: "machine_sha256", Value: digestJSON(provenance.Machine)},
		},
		Treatment: []TreatmentDelta{
			{Axis: "source_revision", Baseline: provenance.Revision, Candidate: "candidate-revision"},
			{Axis: "executable_sha256", Baseline: provenance.ExecutableSHA256, Candidate: dummy("c")},
			{Axis: "cell_sha256", Baseline: digestJSON(baselineCell), Candidate: dummy("d")},
			{Axis: "execution_kind", Baseline: string(bench.ExecutionLegacy), Candidate: string(bench.ExecutionGraphNative)},
			{Axis: "graph_sha256", Baseline: digestJSON(baselineCell.Execution.Legacy), Candidate: dummy("e")},
			{Axis: "configuration_sha256", Baseline: digestJSON(baselineCell), Candidate: dummy("f")},
			{Axis: "deployment_sha256", Baseline: dummy("1"), Candidate: dummy("2")},
			{Axis: "runtime_sha256", Baseline: dummy("3"), Candidate: dummy("4")},
			{Axis: "legacy_runtime_sha256", Baseline: dummy("5"), Candidate: notApplicableIdentitySHA256},
		},
	}
	if candidateCell != nil {
		graph := candidateCell.Execution.Graph
		manifest.Treatment[1].Candidate = strings.Repeat("c", 64)
		manifest.Treatment[2].Candidate = digestJSON(*candidateCell)
		manifest.Treatment[4].Candidate = digestJSON(graph.Graph)
		manifest.Treatment[5].Candidate = digestJSON(graph.Configuration)
		manifest.Treatment[6].Candidate = digestJSON(graph.Nodes)
		manifest.Treatment[7].Candidate = stableRuntimeDigest(bench.ExecutionEvidence{
			FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
			Graph: graph,
		})
	}
	for suiteIndex, censusSuite := range census.Suites {
		repetitions := censusSuite.MinimumRepetitions
		suite := SuiteSpec{
			Name: censusSuite.Name, ExpectedCases: len(censusSuite.Cases),
			ExpectedAttempts:   len(censusSuite.Cases) * repetitions,
			MinimumRepetitions: repetitions,
			Policy:             requiredTestPolicy(censusSuite, suiteIndex, bases[censusSuite.Name]),
		}
		populations := map[string]*Population{}
		for _, item := range censusSuite.Cases {
			repetitionIDs := make([]string, repetitions)
			for index := range repetitions {
				repetitionIDs[index] = "trial-" + strconv.Itoa(index+1)
			}
			suite.Cases = append(suite.Cases, CaseSpec{
				Condition: item.Condition, ID: item.ID, Repetitions: repetitionIDs,
			})
			population := populations[item.Condition]
			if population == nil {
				population = &Population{Condition: item.Condition}
				populations[item.Condition] = population
			}
			population.ExpectedCases++
			population.ExpectedAttempts += repetitions
		}
		for _, population := range populations {
			suite.Populations = append(suite.Populations, *population)
		}
		manifest.Suites = append(manifest.Suites, suite)
	}
	return canonicalManifest(manifest)
}

func requiredTestPolicy(suite CensusSuite, suiteIndex int, basis EvidenceRef) SuitePolicy {
	attempts := len(suite.Cases) * suite.MinimumRepetitions
	return SuitePolicy{
		Inference:       BootstrapPolicy{Confidence: 0.95, Resamples: 1000, Seed: uint64(suiteIndex + 1)},
		AcceptanceBasis: basis,
		Pass:            RatePolicy{Margin: 0.5, MinimumAttempts: attempts, MinimumCases: len(suite.Cases)},
		Interaction:     OutcomePolicy{}, Deadline: OutcomePolicy{}, Safety: SafetyPolicy{},
		Latencies: []LatencyPolicy{{
			Name: "latency", Unit: "ms", Required: true,
			Gate: &LatencyGate{
				MinimumAttempts: attempts, MinimumCases: len(suite.Cases),
				Limits: []LatencyLimit{
					{Statistic: StatisticP50, MaximumIncrease: 10},
					{Statistic: StatisticP95, MaximumIncrease: 20},
				},
			},
		}},
		RequireEvidence: true,
		RequiredEvidenceKinds: []string{
			EvidenceKindResult, EvidenceKindImportProfile, EvidenceKindExecution,
		},
	}
}

func sortedAttemptKeys(keys map[AttemptKey]bool) []AttemptKey {
	result := make([]AttemptKey, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	sortAttempts(result)
	return result
}

func sortAttempts(keys []AttemptKey) {
	for index := 1; index < len(keys); index++ {
		for current := index; current > 0 && keyScope(keys[current]) < keyScope(keys[current-1]); current-- {
			keys[current], keys[current-1] = keys[current-1], keys[current]
		}
	}
}
