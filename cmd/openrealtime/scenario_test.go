package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/graph/inspect"
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

func TestScenarioReviewDirectoryRequiresTheCompleteSuite(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "must-not-be-created")
	var output bytes.Buffer
	err := runScenario([]string{
		"-review-dir", directory,
		"-only", scenario.Suite()[0].Name,
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "requires the complete scenario suite") {
		t.Fatalf("partial review error = %v", err)
	}
	if _, statErr := os.Stat(directory); !os.IsNotExist(statErr) {
		t.Fatalf("partial review created a directory: %v", statErr)
	}
}

func TestScenarioReviewDirectoryIsCreateOnlyBeforeSpeechOrSessionWork(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "existing-review")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "owned-by-user")
	if err := os.WriteFile(marker, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runScenario([]string{
		"-review-dir", directory,
		"-url", "ws://127.0.0.1:1/v1/realtime",
		"-speech-url", "http://127.0.0.1:1/v1/audio/speech",
	}, &output)
	if err == nil || !strings.Contains(err.Error(), "exclusively") {
		t.Fatalf("existing review error = %v", err)
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "preserve" {
		t.Fatalf("existing review contents were changed: %q, %v", got, readErr)
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
		"requires a graph-native -execution requirement") {
		t.Fatalf("unattested scenario inspection graph refusal = %v", err)
	}
	if environmentReads.Load() != 0 {
		t.Fatal("credential was read for an unattested inspection graph")
	}

	err = runScenario([]string{
		"-inspection-graph", filepath.Join(t.TempDir(), "must-not-be-read.ir.json"),
	}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(),
		"requires a graph-native -execution requirement") {
		t.Fatalf("top-level scenario inspection flag was silently ignored: %v", err)
	}
}

func TestScenarioUnattestedModeRemainsDiagnosticOnly(t *testing.T) {
	var environmentReads atomic.Int32
	config, err := configureScenarioSession(bench.SessionConfig{
		Endpoint: "ws://127.0.0.1:8765/v1/realtime",
		Model:    "diagnostic-model",
	}, bench.ExecutionRequirement{}, "", "SCENARIO_TOKEN", func(string) string {
		environmentReads.Add(1)
		return "diagnostic-bearer"
	})
	if err != nil {
		t.Fatalf("configure scenario diagnostic: %v", err)
	}
	if config.Token != "diagnostic-bearer" || config.Model != "diagnostic-model" ||
		environmentReads.Load() != 1 || config.CaptureRuntimeEvidence || config.RuntimeAttestor != nil {
		t.Fatalf("scenario diagnostic config = %+v, reads=%d", config, environmentReads.Load())
	}
	taskConfig := scenarioSessionForTask(config, "an ordinary question#1")
	if taskConfig.AttestationScope != "" {
		t.Fatalf("unattested scenario gained scope %q", taskConfig.AttestationScope)
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
