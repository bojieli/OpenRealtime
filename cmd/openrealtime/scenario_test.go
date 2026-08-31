package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	projectarch "github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

func TestScenarioGraphAttestorCoversAllElevenExactSessionScopes(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	snapshot := inspectionSnapshotForFixture(fixture, requirement)
	const deploymentBearer = "scenario-deployment-bearer"

	suite := scenario.Suite()
	if len(suite) != 11 {
		t.Fatalf("scenario suite has %d paths, want the reviewed eleven", len(suite))
	}
	accessByPath := make(map[string]openrealtime.InspectionAccess, len(suite))
	accessByScenario := make(map[string]openrealtime.InspectionAccess, len(suite))
	for index, item := range suite {
		access := scenarioInspectionAccess(fmt.Sprintf("sess_scenario_%02d", index+1), byte(index+1))
		accessByPath[access.Path] = access
		accessByScenario[item.Name] = access
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		access, found := accessByPath[request.URL.EscapedPath()]
		if !found {
			t.Errorf("inspection requested unreviewed session path %q", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "" {
			t.Errorf("deployment bearer leaked to management route: %q", got)
		}
		if got := request.Header.Get(management.CapabilityHeader); got != access.Token {
			t.Errorf("session management capability = %q, want %q", got, access.Token)
		}
		if access.Token == deploymentBearer {
			t.Error("deployment bearer was reused as session inspection authority")
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(writer).Encode(snapshot); err != nil {
			t.Errorf("encode inspection snapshot: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	var environmentReads atomic.Int32
	config, err := configureScenarioSession(bench.SessionConfig{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Timeout: time.Second,
	}, requirement, fixture.graphPath, "SCENARIO_DEPLOYMENT_TOKEN", func(name string) string {
		environmentReads.Add(1)
		if name != "SCENARIO_DEPLOYMENT_TOKEN" {
			t.Errorf("deployment credential environment = %q", name)
		}
		return deploymentBearer
	})
	if err != nil {
		t.Fatalf("configure scenario graph attestor: %v", err)
	}
	if config.Token != deploymentBearer || environmentReads.Load() != 1 {
		t.Fatalf("deployment token = %q, environment reads = %d",
			config.Token, environmentReads.Load())
	}
	if _, ok := config.RuntimeAttestor.(bench.GraphAttestor); !ok ||
		!config.CaptureRuntimeEvidence || config.AttestationScope != "" {
		t.Fatalf("scenario session evidence configuration = %+v, attestor %T",
			config, config.RuntimeAttestor)
	}

	for _, item := range suite {
		taskID := item.Name + "#1"
		taskConfig := scenarioSessionForTask(config, taskID)
		if taskConfig.AttestationScope != taskID {
			t.Fatalf("scenario %q scope = %q", item.Name, taskConfig.AttestationScope)
		}
		access := accessByScenario[item.Name]
		evidence, err := taskConfig.RuntimeAttestor.Attest(context.Background(), bench.AttestationRequest{
			Scope: taskConfig.AttestationScope,
			Status: binding.Status{Graph: binding.ArchitectureIdentity{
				ID: fixture.graph.ID, Revision: int(fixture.graph.Revision),
				Fingerprint: fixture.graph.Fingerprint,
			}},
			Inspection: &access,
		})
		if err != nil {
			t.Fatalf("scenario %q authenticated attestation: %v", item.Name, err)
		}
		if evidence.Scope != taskID {
			t.Fatalf("scenario %q evidence scope = %q", item.Name, evidence.Scope)
		}
		if err := requirement.Match(&evidence); err != nil {
			t.Fatalf("scenario %q evidence does not meet reviewed execution: %v", item.Name, err)
		}
		encoded, err := json.Marshal(evidence)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(encoded, []byte(deploymentBearer)) ||
			bytes.Contains(encoded, []byte(access.Token)) {
			t.Fatalf("scenario %q retained an authentication authority", item.Name)
		}
	}
	if got := requests.Load(); got != int32(len(suite)) {
		t.Fatalf("authenticated inspection requests = %d, want %d", got, len(suite))
	}
	if config.AttestationScope != "" {
		t.Fatalf("shared scenario config was mutated to scope %q", config.AttestationScope)
	}
}

func TestScenarioRequiresGraphNativeManifestBeforeArtifactReservation(t *testing.T) {
	working := t.TempDir()
	t.Chdir(working)
	directory := filepath.Join(working, "must-not-be-created")
	var output bytes.Buffer
	err := runScenario([]string{
		"-review-dir", directory,
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "graph-native -architecture-manifest") {
		t.Fatalf("missing manifest error = %v", err)
	}
	for _, path := range []string{directory, filepath.Join(working, benchmarkArtifactDirectory)} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("invalid scenario invocation reserved %s: %v", path, statErr)
		}
	}
}

func TestScenarioSignalsCancelExecutionThenEvidenceCleanup(t *testing.T) {
	execution, cancelExecution := context.WithCancel(context.Background())
	cleanup, cancelCleanup := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 2)
	done := make(chan struct{})
	go relayScenarioSignals(signals, done, cancelExecution, cancelCleanup)
	t.Cleanup(func() {
		close(done)
		cancelExecution()
		cancelCleanup()
	})
	signals <- os.Interrupt
	select {
	case <-execution.Done():
	case <-time.After(time.Second):
		t.Fatal("first signal did not cancel scenario execution")
	}
	if cleanup.Err() != nil {
		t.Fatalf("first signal canceled evidence cleanup: %v", cleanup.Err())
	}
	signals <- os.Interrupt
	select {
	case <-cleanup.Done():
	case <-time.After(time.Second):
		t.Fatal("second signal did not cancel scenario evidence cleanup")
	}
}

func TestScenarioFirstSignalDuringReviewPreservesPublicationUntilSecondSignal(t *testing.T) {
	_, cancelExecution := context.WithCancel(context.Background())
	cleanup, cancelCleanup := context.WithCancel(context.Background())
	// Model the phase handoff: behavioral execution has already ended before
	// the signal relay starts receiving signals for a long advisory review.
	cancelExecution()
	signals := make(chan os.Signal, 2)
	done := make(chan struct{})
	go relayScenarioSignals(signals, done, cancelExecution, cancelCleanup)
	t.Cleanup(func() {
		close(done)
		cancelCleanup()
	})
	signals <- syscall.SIGTERM
	select {
	case <-cleanup.Done():
		t.Fatal("first signal during review canceled evidence publication")
	case <-time.After(25 * time.Millisecond):
	}
	signals <- syscall.SIGTERM
	select {
	case <-cleanup.Done():
	case <-time.After(time.Second):
		t.Fatal("second signal during review did not cancel evidence publication")
	}
}

func TestScenarioOSSignalSealsAttemptedResultOnlySourceWithoutCallingReviewer(t *testing.T) {
	t.Chdir("../..")
	arguments, sourceDirectory, sourceReceipt := scenarioSignalCommandFixture(t)
	t.Setenv("GEMINI_API_KEY", "scenario-signal-gemini-key-fixture-long-enough")
	executorEntered := make(chan struct{})
	returned := make(chan error, 1)
	var output bytes.Buffer
	go func() {
		returned <- runScenarioWithExecutor(
			arguments, &output,
			func(graphnative.LiveExecutorConfig) (graphnative.AttemptExecutor, error) {
				return func(
					ctx context.Context, key graphnative.AttemptKey, _ scenario.Scenario,
				) (graphnative.AttemptObservation, error) {
					close(executorEntered)
					<-ctx.Done()
					return graphnative.AttemptObservation{
						Result: scenario.Result{Scenario: key.CaseName},
					}, context.Cause(ctx)
				}, nil
			},
		)
	}()
	select {
	case <-executorEntered:
	case err := <-returned:
		t.Fatalf("scenario command returned before signal: %v\n%s", err, output.String())
	case <-time.After(3 * time.Second):
		t.Fatal("scenario command did not begin its first attempt")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var runErr error
	select {
	case runErr = <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not finish scenario source publication")
	}
	if !errors.Is(runErr, context.Canceled) ||
		!strings.Contains(runErr.Error(), "no media-complete attempts") {
		t.Fatalf("signaled scenario error = %v\n%s", runErr, output.String())
	}
	receipt, err := graphnative.ReadSourceReceipt(sourceReceipt)
	if err != nil {
		t.Fatalf("read signaled source receipt: %v\n%s", err, output.String())
	}
	source, err := graphnative.VerifySourceBundle(
		context.Background(), graphnative.SourceBundleOptions{Directory: sourceDirectory}, receipt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if source.Manifest.PopulationComplete || len(source.Manifest.Attempts) != 1 ||
		source.Manifest.Attempts[0].Audio != nil ||
		source.Manifest.Attempts[0].MediaManifest != nil ||
		source.Checklist.Executed != 1 || source.Checklist.Complete {
		t.Fatalf("signaled scenario source = %+v", source.Manifest)
	}
	if _, err := os.Lstat(sourceDirectory + ".evaluations"); !os.IsNotExist(err) {
		t.Fatalf("zero-media signaled run created evaluation output: %v", err)
	}
	if _, err := os.Lstat(sourceDirectory + ".evaluations.receipt.json"); !os.IsNotExist(err) {
		t.Fatalf("zero-media signaled run created evaluation receipt: %v", err)
	}
}

func scenarioSignalCommandFixture(t *testing.T) ([]string, string, string) {
	t.Helper()
	graphFixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, graphFixture)
	selection, _, adapterFingerprint := scenarioGraphCommandFixture(t)
	plan := selection.Profile.Plan
	plan.GraphID = graphFixture.graph.ID
	plan.GraphRevision = graphFixture.graph.Revision
	plan.GraphFingerprint = graphFixture.graph.Fingerprint
	plan.PlanFingerprint = ""
	plan.PlanFingerprint = scenarioGraphTestPlanFingerprint(t, plan)
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	profileDraft := selection.Profile
	profileDraft.Fingerprint = ""
	profileDraft.Plan = plan
	profile, err := launchprofile.Freeze(profileDraft)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	profilePath := filepath.Join(directory, "scenario.launch.yaml")
	profilePayload, err := launchprofile.MarshalYAML(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePath, profilePayload, 0o600); err != nil {
		t.Fatal(err)
	}
	const cellName = "signal-graph-native"
	manifestPath := filepath.Join(directory, "architecture.json")
	if err := archbench.WriteManifest(manifestPath, archbench.Manifest{
		Version: archbench.ManifestVersion, Name: "scenario-signal-fixture",
		Suite: "scenario", FixtureRevision: "scenario-signal-v1",
		Cells: []archbench.Cell{scenarioSignalArchitectureCell(
			t, cellName, profile.Adapter.ProfileName, adapterFingerprint, requirement,
		)},
	}); err != nil {
		t.Fatal(err)
	}
	sourceDirectory := filepath.Join(directory, "source")
	sourceReceipt := sourceDirectory + ".receipt.json"
	return []string{
		"-url", "ws://127.0.0.1:8765/v1/realtime",
		"-speech-url", "http://127.0.0.1:8081/v1/audio/speech",
		"-architecture-manifest", manifestPath,
		"-architecture-cell", cellName,
		"-launch-profile", profilePath,
		"-inspection-graph", graphFixture.graphPath,
		"-review-dir", sourceDirectory,
		"-review-receipt", sourceReceipt,
		"-review-parallel", "1",
		"-review-timeout", "1s",
	}, sourceDirectory, sourceReceipt
}

func scenarioSignalArchitectureCell(
	t *testing.T,
	name, runtimeBinding, profile string,
	requirement bench.ExecutionRequirement,
) archbench.Cell {
	t.Helper()
	definition, err := projectarch.Default().Resolve("omni.external-policy@4")
	if err != nil {
		t.Fatal(err)
	}
	ownership := binding.Ownership{
		Perception: binding.OwnerModel, FastCognition: binding.OwnerModel,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerModel,
		Interaction: binding.OwnerEngine, Floor: binding.OwnerEngine,
	}
	capabilities := binding.StackCapabilities{
		AudioInput: true, AudioOutput: true, Transcription: true, TurnGeneration: true,
		ConcurrentIO: true, NativeFloor: true, NativeInteraction: true,
		InteractionActs: true, TextInjection: true,
	}
	interactionIdentity := archbench.InteractionIdentity{
		PolicyName: "model:policy-3b",
		Model: archbench.ModelIdentity{
			Provider: "fixture", Model: "policy-3b", Revision: "policy-r1",
		},
		InstructionRevision: "interaction-instruction-r2", DecisionTimeoutMS: 150,
		Evidence: archbench.EvidenceIdentity{
			Source:       archbench.EvidenceTranscript,
			Capabilities: *definition.Interaction.EvidenceCapabilities,
			Recognizer: archbench.ModelIdentity{
				Provider: "fixture", Model: "asr", Revision: "asr-r1",
				AdapterRevision: "asr-adapter-r1",
			},
		},
		Protocol: archbench.ProtocolIdentity{
			Transport: "sidecar", Version: 2, Handoff: archbench.HandoffTyped,
		},
		NativeSuppressionContract: definition.Interaction.NativeSuppression,
		Control:                   *definition.Interaction.Control,
	}
	cell := archbench.Cell{
		Name: name, Availability: archbench.AvailabilityRunnable, Execution: requirement,
		Architecture: archbench.Architecture{
			Definition: definition, Level: archbench.LevelTextPolicy,
			RuntimeBinding: runtimeBinding, Profile: profile,
			Foreground: archbench.ModelIdentity{
				Provider: "fixture", Model: "foreground", Revision: "foreground-r1",
			},
			Slow: archbench.ModelIdentity{
				Provider: "fixture", Model: "slow", Revision: "slow-r1",
			},
			Ownership: ownership, Capabilities: capabilities,
			Policies: interaction.Report{
				Trigger: "endpoint", Preparation: "endpoint", Rollout: "slow-only",
				Floor: "model:foreground", BargeIn: "never", Commitment: "complete",
				Repair: "audible", Backchannel: "none", TurnProjection: "vad",
				Overlap: "unclassified", Deferral: "always",
				Interaction: interactionIdentity.PolicyName, Extraction: "unset",
			},
			Observers: []string{"fixture:foreground"}, Interaction: interactionIdentity,
			ToolAuthority: binding.ToolStatus{
				Fast: "propose", Slow: "execute", Authorization: "engine",
				Execution: "engine-or-client",
			},
		},
	}
	if err := cell.Architecture.Validate(); err != nil {
		t.Fatal(err)
	}
	return cell
}

func TestScenarioReviewCleanupBudgetCoversEveryWorkerBatch(t *testing.T) {
	options := scenarioEvaluationRunOptions{Parallel: 4, Timeout: 12 * time.Minute}
	if got, want := scenarioReviewPublicationTimeout(165, options), 8*time.Hour+29*time.Minute; got != want {
		t.Fatalf("165-attempt cleanup timeout = %v, want %v", got, want)
	}
	if got, want := scenarioReviewPublicationTimeout(1, options), 17*time.Minute; got != want {
		t.Fatalf("one-attempt cleanup timeout = %v, want %v", got, want)
	}
}

func TestPrepareAutomaticScenarioEvaluationPreflightsExactReviewerBeforeAttempt(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "gemini-review-key-fixture-long-enough")
	parent := t.TempDir()
	source := filepath.Join(parent, "scenario-source")
	receipt := source + ".receipt.json"
	options, registry, err := prepareAutomaticScenarioEvaluation(
		t.Context(), source, receipt, "google.gemini-3.7-flash", 4, 12*time.Minute,
	)
	if err != nil {
		t.Fatal(err)
	}
	if registry == nil || options.SourceDirectory != source || options.SourceReceipt != receipt ||
		options.OutputDirectory != source+".evaluations" ||
		options.OutputReceipt != source+".evaluations.receipt.json" || options.Parallel != 4 {
		t.Fatalf("prepared automatic scenario evaluation = %+v, registry=%v", options, registry)
	}
	for _, path := range []string{
		options.SourceDirectory, options.SourceReceipt,
		options.OutputDirectory, options.OutputReceipt,
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("scenario preflight mutated create-only path %s: %v", path, err)
		}
	}
	arguments := scenarioEvaluationArguments(options)
	joined := strings.Join(arguments, " ")
	for _, exact := range []string{source, receipt, "google.gemini-3.7-flash", "12m0s"} {
		if !strings.Contains(joined, exact) {
			t.Fatalf("scenario evaluation arguments %q omit %q", joined, exact)
		}
	}
}

func TestPrepareAutomaticScenarioEvaluationRefusesCredentialAndPathBeforeAttempt(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "scenario-source")
	receipt := source + ".receipt.json"
	t.Setenv("GEMINI_API_KEY", "")
	if _, _, err := prepareAutomaticScenarioEvaluation(
		t.Context(), source, receipt, "google.gemini-3.7-flash", 4, 12*time.Minute,
	); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("missing scenario reviewer credential error = %v", err)
	}
	for _, path := range []string{source, receipt, source + ".evaluations"} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("credential refusal mutated %s: %v", path, err)
		}
	}
	if err := os.WriteFile(receipt, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEMINI_API_KEY", "gemini-review-key-fixture-long-enough")
	if _, _, err := prepareAutomaticScenarioEvaluation(
		t.Context(), source, receipt, "google.gemini-3.7-flash", 4, 12*time.Minute,
	); err == nil || !strings.Contains(err.Error(), "create-only path already exists") {
		t.Fatalf("existing source receipt error = %v", err)
	}
	if payload, err := os.ReadFile(receipt); err != nil || string(payload) != "owned" {
		t.Fatalf("existing receipt changed: %q, %v", payload, err)
	}
	if _, _, err := prepareAutomaticScenarioEvaluation(
		t.Context(), source, filepath.Join(parent, "fresh.receipt"), "google.latest", 4, 12*time.Minute,
	); err == nil || !strings.Contains(err.Error(), "exact google.gemini-3.7-flash") {
		t.Fatalf("reviewer identity error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := prepareAutomaticScenarioEvaluation(
		canceled, filepath.Join(parent, "canceled-source"), filepath.Join(parent, "canceled-receipt"),
		"google.gemini-3.7-flash", 4, 12*time.Minute,
	); err != context.Canceled {
		t.Fatalf("canceled reviewer preflight error = %v", err)
	}
}

func TestScenarioRejectsReviewedGraphDriftBeforeCredentialWork(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	directory := t.TempDir()
	differentGraphPath := filepath.Join(directory, "different.ir.json")
	differentGraph, err := fixture.unbound.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(differentGraphPath, differentGraph, 0o600); err != nil {
		t.Fatal(err)
	}

	changeNode := func(change func(*bench.GraphNodeEvidence)) bench.ExecutionRequirement {
		changed := requirement
		graph := *requirement.Graph
		graph.Nodes = append([]bench.GraphNodeEvidence(nil), requirement.Graph.Nodes...)
		change(&graph.Nodes[0])
		changed.Graph = &graph
		return changed
	}
	tests := []struct {
		name        string
		requirement bench.ExecutionRequirement
		graphPath   string
		want        string
	}{
		{
			name: "missing exact graph", requirement: requirement,
			want: "-inspection-graph",
		},
		{
			name: "absent graph", requirement: requirement,
			graphPath: filepath.Join(directory, "absent.ir.json"), want: "read inspection Graph IR",
		},
		{
			name: "graph identity", requirement: requirement,
			graphPath: differentGraphPath, want: "inspection graph identity",
		},
		{
			name: "node configuration",
			requirement: changeNode(func(node *bench.GraphNodeEvidence) {
				node.Config.Digest = "sha256:" + strings.Repeat("9", 64)
			}),
			graphPath: fixture.graphPath, want: "does not reproduce the reviewed node configuration",
		},
		{
			name: "node implementation",
			requirement: changeNode(func(node *bench.GraphNodeEvidence) {
				node.Implementation = "go://test/drifted-model/v2"
			}),
			graphPath: fixture.graphPath, want: "Graph IR selects",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var environmentReads atomic.Int32
			_, err := configureScenarioSession(bench.SessionConfig{
				Endpoint: "ws://127.0.0.1:8765/v1/realtime",
			}, test.requirement, test.graphPath, "SCENARIO_TOKEN", func(string) string {
				environmentReads.Add(1)
				return "credential-must-not-be-read"
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("scenario graph drift refusal = %v, want %q", err, test.want)
			}
			if environmentReads.Load() != 0 {
				t.Fatalf("credential was read %d time(s) before graph refusal", environmentReads.Load())
			}
			if strings.Contains(err.Error(), "credential-must-not-be-read") {
				t.Fatalf("scenario graph refusal exposed a credential: %v", err)
			}
		})
	}
}

func TestScenarioGraphAttestorRejectsForgedLiveInspectionEvidence(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	tests := []struct {
		name       string
		mutate     func(*inspect.Live)
		attestWant string
		matchWant  string
	}{
		{
			name: "mounted graph",
			mutate: func(snapshot *inspect.Live) {
				snapshot.Fingerprint = "sha256:" + strings.Repeat("8", 64)
			},
			attestWant: "live graph is",
		},
		{
			name: "configuration artifact",
			mutate: func(snapshot *inspect.Live) {
				snapshot.Configuration.Digest = "sha256:" + strings.Repeat("7", 64)
			},
			attestWant: "live configuration",
		},
		{
			name: "node runtime",
			mutate: func(snapshot *inspect.Live) {
				node := snapshot.Nodes["model"]
				node.Resolution.Runtime.Revision = "image:forged"
				snapshot.Nodes["model"] = node
			},
			matchWant: "resolutions differ",
		},
		{
			name: "node capability",
			mutate: func(snapshot *inspect.Live) {
				node := snapshot.Nodes["model"]
				node.Resolution.Capabilities[0].Provider.Revision = "weights:forged"
				snapshot.Nodes["model"] = node
			},
			matchWant: "capabilit",
		},
		{
			name: "node implementation",
			mutate: func(snapshot *inspect.Live) {
				node := snapshot.Nodes["model"]
				node.Resolution.Implementation = "go://test/forged/v9"
				snapshot.Nodes["model"] = node
			},
			attestWant: "implementation",
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := inspectionSnapshotForFixture(fixture, requirement)
			test.mutate(&snapshot)
			access := scenarioInspectionAccess(fmt.Sprintf("sess_forged_%02d", index+1), byte(index+32))
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.EscapedPath() != access.Path ||
					request.Header.Get(management.CapabilityHeader) != access.Token {
					t.Errorf("forged-evidence request was not session authenticated")
				}
				writer.Header().Set("Content-Type", "application/json")
				writer.Header().Set("Cache-Control", "no-store")
				if err := json.NewEncoder(writer).Encode(snapshot); err != nil {
					t.Errorf("encode forged inspection snapshot: %v", err)
				}
			}))
			t.Cleanup(server.Close)

			config, err := configureScenarioSession(bench.SessionConfig{
				Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
			}, requirement, fixture.graphPath, "SCENARIO_TOKEN", func(string) string {
				return "deployment-bearer"
			})
			if err != nil {
				t.Fatal(err)
			}
			taskConfig := scenarioSessionForTask(config, "an ordinary question#1")
			evidence, err := taskConfig.RuntimeAttestor.Attest(
				context.Background(), bench.AttestationRequest{
					Scope: taskConfig.AttestationScope,
					Status: binding.Status{Graph: binding.ArchitectureIdentity{
						ID: fixture.graph.ID, Revision: int(fixture.graph.Revision),
						Fingerprint: fixture.graph.Fingerprint,
					}},
					Inspection: &access,
				},
			)
			if test.attestWant != "" {
				if err == nil || !strings.Contains(err.Error(), test.attestWant) {
					t.Fatalf("forged %s attestation refusal = %v, want %q",
						test.name, err, test.attestWant)
				}
				if evidence.Fingerprint != "" || evidence.Graph != nil {
					t.Fatalf("forged %s produced evidence: %+v", test.name, evidence)
				}
				return
			}
			if err != nil {
				t.Fatalf("capture independently observed %s evidence: %v", test.name, err)
			}
			if matchErr := requirement.Match(&evidence); matchErr == nil ||
				!strings.Contains(matchErr.Error(), test.matchWant) {
				t.Fatalf("forged %s requirement refusal = %v, want %q",
					test.name, matchErr, test.matchWant)
			}
		})
	}
}

func TestScenarioInspectionGraphCannotAttestUnattestedExecution(t *testing.T) {
	var environmentReads atomic.Int32
	_, err := configureScenarioSession(bench.SessionConfig{
		Endpoint: "ws://127.0.0.1:8765/v1/realtime",
	}, bench.ExecutionRequirement{}, filepath.Join(t.TempDir(), "must-not-be-read.ir.json"),
		"SCENARIO_TOKEN", func(string) string {
			environmentReads.Add(1)
			return "secret"
		})
	if err == nil || !strings.Contains(err.Error(),
		"exact graph-native execution requirement") {
		t.Fatalf("unattested scenario inspection graph refusal = %v", err)
	}
	if environmentReads.Load() != 0 {
		t.Fatal("credential was read for an unattested inspection graph")
	}

	err = runScenario([]string{
		"-inspection-graph", filepath.Join(t.TempDir(), "must-not-be-read.ir.json"),
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(),
		"graph-native -architecture-manifest") {
		t.Fatalf("top-level scenario inspection flag was silently ignored: %v", err)
	}
}

func scenarioInspectionAccess(sessionID string, fill byte) openrealtime.InspectionAccess {
	return openrealtime.InspectionAccess{
		SessionID: sessionID,
		Path:      management.APIPrefix + "/sessions/" + sessionID + "/live",
		Token: "mgmt_" + base64.RawURLEncoding.EncodeToString(
			bytes.Repeat([]byte{fill}, 32),
		),
		ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli(),
	}
}

func TestScenarioTaskPreservesTaskExecutionEvidence(t *testing.T) {
	fixture := writeGraphExecutionFixture(t)
	requirement := requirementForGraphFixture(t, fixture)
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion,
		Kind:          bench.ExecutionGraphNative,
		Scope:         "ordinary-question#1",
		Graph:         requirement.Graph,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := scenario.Result{
		Passed: true,
		Transcript: bench.Transcript{
			Execution:      &evidence,
			ExecutionError: "",
		},
	}
	outcome := scenarioTask("ordinary-question#1", result, nil)
	if outcome.Execution == nil || outcome.Execution.Fingerprint != evidence.Fingerprint {
		t.Fatalf("scenario execution evidence was dropped: %+v", outcome)
	}
	if outcome.Execution.Scope != outcome.ID {
		t.Fatalf("scenario evidence scope = %q, want task %q", outcome.Execution.Scope, outcome.ID)
	}

	result.Transcript.Execution.Graph.Nodes[0].Node = "mutated"
	if outcome.Execution.Graph.Nodes[0].Node == "mutated" {
		t.Fatal("scenario task retained an alias into transcript execution evidence")
	}
}

func TestScenarioTaskPreservesExecutionAttestationFailure(t *testing.T) {
	result := scenario.Result{
		Transcript: bench.Transcript{ExecutionError: "live graph inspector timed out"},
	}
	outcome := scenarioTask("ordinary-question#1", result, nil)
	if outcome.Execution != nil || outcome.ExecutionError != result.Transcript.ExecutionError {
		t.Fatalf("scenario attestation failure was dropped: %+v", outcome)
	}
}
