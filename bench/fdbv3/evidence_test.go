package fdbv3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

type rejectingCandidatePlugin struct {
	attempt candidate.Attempt
	result  bench.Result
	cause   error
}

type recoveringCandidatePlugin struct {
	beginCalls   int
	recoverCalls int
	result       bench.Result
	completion   func(candidate.Attempt) (candidate.Completion, error)
}

type recoveredTranscript struct{ transcript bench.Transcript }

func (evidence recoveredTranscript) ReopenTranscript(context.Context) (bench.Transcript, error) {
	return evidence.transcript, nil
}
func (recoveredTranscript) CommitValidated(context.Context) error { return nil }

func (plugin *recoveringCandidatePlugin) BindRun(
	_ context.Context, _ string, _ bench.Cell, provenance bench.Provenance, _ candidate.RunOrigin,
) (bench.Provenance, error) {
	return provenance, nil
}

func (plugin *recoveringCandidatePlugin) BeginAttempt(
	context.Context, candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	plugin.beginCalls++
	return nil, errors.New("recovered FDB v3 attempt must not begin recording")
}

func (plugin *recoveringCandidatePlugin) RecoverAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.Recovery, bool, error) {
	plugin.recoverCalls++
	var completion candidate.Completion
	var err error
	if plugin.completion != nil {
		completion, err = plugin.completion(attempt)
	} else {
		completion = candidate.Completion{
			Attempt: attempt,
			Outcome: bench.TaskOutcome{ID: attempt.Case, Completed: true, Passed: true},
		}
	}
	return candidate.Recovery{
		Completion: completion,
		Evidence:   recoveredTranscript{transcript: completion.Transcript},
	}, err == nil, err
}

func (plugin *recoveringCandidatePlugin) FinishSuite(_ context.Context, result bench.Result) error {
	plugin.result = result
	return nil
}

func (plugin *rejectingCandidatePlugin) BeginAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	plugin.attempt = attempt
	return nil, plugin.cause
}

func (plugin *rejectingCandidatePlugin) FinishSuite(
	_ context.Context, result bench.Result,
) error {
	plugin.result = result
	return nil
}

func TestCandidateEvidenceRetainsFDBV3AttemptIdentityBeforePlayback(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "recording-001")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"id":"ignored","domain":"ecommerce","title":"Track an order","difficulty":"easy","expected_tool_calls":[{"function":"track_order","args":{"order_id":"BOB12"}}],"disfluency_features":["spelled_identifier"]}`
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "input.wav"), []byte("invalid on purpose"), 0o600); err != nil {
		t.Fatal(err)
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:1/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("candidate recorder refused")
	plugin := &rejectingCandidatePlugin{cause: refusal}
	result, runErr := Run(t.Context(), Options{
		Root: root, Endpoint: "ws://127.0.0.1:1/v1/realtime", Cell: bench.Reference(),
		Model: "  model-a  ", Timeout: 17 * time.Second,
		Evidence: plugin, EvidenceOrigin: origin, releasedInventory: fixtureInventory(t, root),
	})
	if !errors.Is(runErr, refusal) {
		t.Fatalf("run error = %v, want recorder refusal", runErr)
	}
	if plugin.attempt.Suite != "fdb-v3" || plugin.attempt.Case != "recording-001" ||
		plugin.attempt.Trial != 1 || len(plugin.attempt.Context) == 0 {
		t.Fatalf("attempt = %+v", plugin.attempt)
	}
	for _, identity := range []string{
		releaseValidityScorerIdentity, upstreamFallbackScorerIdentity,
		openRealtimeHistoricalScorerIdentity, semanticRepairedScorerIdentity,
		semanticReferenceErrataIdentity, fixtureDispositionRegistryIdentity,
		releaseEvidenceScorerIdentity, harnessIdentity, upstreamToolCatalogIdentity, simulatorIdentity,
	} {
		if !bytes.Contains(plugin.attempt.Context, []byte(identity)) {
			t.Fatalf("attempt context omits score identity %q: %s", identity, plugin.attempt.Context)
		}
	}
	var retained attemptContext
	if err := json.Unmarshal(plugin.attempt.Context, &retained); err != nil {
		t.Fatal(err)
	}
	if retained.Execution.Harness != harnessIdentity ||
		retained.Execution.UpstreamToolCatalog != upstreamToolCatalogIdentity ||
		retained.Execution.Simulator != simulatorIdentity ||
		retained.Execution.Model != "model-a" || retained.Execution.TimeoutNS != int64(17*time.Second) ||
		retained.Execution.Transport != bench.TransportWebSocket ||
		retained.Execution.EndpointSHA256 != origin.EndpointSHA256 ||
		retained.Execution.InventoryRevision != "test-revision" ||
		!strings.HasPrefix(retained.Execution.ToolCatalogSHA256, "sha256:") ||
		!strings.HasPrefix(retained.Execution.InstructionsSHA256, "sha256:") {
		t.Fatalf("retained execution identity = %+v", retained.Execution)
	}
	if retained.Scoring.FixtureDispositions != fixtureDispositionRegistryIdentity {
		t.Fatalf("retained scoring identity = %+v", retained.Scoring)
	}
	if retained.Scoring.ReleaseEvidence != releaseEvidenceScorerIdentity {
		t.Fatalf("retained release-evidence identity = %+v", retained.Scoring)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].Completed || result.Summary.Complete {
		t.Fatalf("result = %+v", result)
	}
	if len(plugin.result.Tasks) != 1 || plugin.result.Tasks[0].ID != "recording-001" {
		t.Fatalf("finish result = %+v", plugin.result)
	}
}

func TestCandidateEvidenceRecoveryRefusesMissingOrInconsistentScoreEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*candidate.Completion)
	}{
		{name: "bare passed outcome", mutate: func(completion *candidate.Completion) {
			completion.Transcript = bench.Transcript{}
			completion.Outcome = bench.TaskOutcome{ID: completion.Attempt.Case, Completed: true, Passed: true}
		}},
		{name: "missing tool result", mutate: func(completion *candidate.Completion) {
			completion.Transcript.Moments = completion.Transcript.Moments[:2]
		}},
		{name: "forged tool result", mutate: func(completion *candidate.Completion) {
			completion.Transcript.Moments[2].Text = `{"status":"success"}`
		}},
		{name: "forged passed bit", mutate: func(completion *candidate.Completion) {
			completion.Outcome.Passed = false
		}},
		{name: "missing deterministic metrics", mutate: func(completion *candidate.Completion) {
			completion.Outcome.Metrics = nil
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			root, origin, inventory := candidateRecoveryFixture(t)
			plugin := &recoveringCandidatePlugin{completion: func(attempt candidate.Attempt) (candidate.Completion, error) {
				completion, err := validRecoveredCompletion(attempt)
				if err == nil {
					testCase.mutate(&completion)
				}
				return completion, err
			}}
			result, err := Run(t.Context(), Options{
				Root: root, Endpoint: "ws://127.0.0.1:1/v1/realtime", Cell: bench.Reference(),
				Model:    "model-a",
				Evidence: plugin, EvidenceOrigin: origin, releasedInventory: inventory,
			})
			if err == nil || len(result.Tasks) != 1 || result.Tasks[0].Completed ||
				result.Tasks[0].Passed || result.Tasks[0].Error == "" || result.Summary.Complete ||
				plugin.beginCalls != 0 || plugin.recoverCalls != 1 {
				t.Fatalf("invalid recovery: err=%v result=%+v plugin=%+v", err, result, plugin)
			}
		})
	}
}

func TestAttemptContextChangesWithBehaviorAffectingExecutionIdentity(t *testing.T) {
	root, origin, inventory := candidateRecoveryFixture(t)
	tasks, err := loadDataset(root, 0, inventory)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := Catalog(tasks)
	if err != nil {
		t.Fatal(err)
	}
	base := Options{
		Endpoint: "ws://127.0.0.1:1/v1/realtime", Model: "model-a", Timeout: 17 * time.Second,
		EvidenceOrigin: origin,
	}
	first, err := buildAttemptContext(base, tasks[0], catalog, *inventory)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []struct {
		name  string
		apply func(*Options, *[]json.RawMessage, *releasedDatasetInventory)
	}{
		{name: "model", apply: func(options *Options, _ *[]json.RawMessage, _ *releasedDatasetInventory) {
			options.Model = "model-b"
		}},
		{name: "timeout", apply: func(options *Options, _ *[]json.RawMessage, _ *releasedDatasetInventory) {
			options.Timeout++
		}},
		{name: "catalog", apply: func(_ *Options, catalog *[]json.RawMessage, _ *releasedDatasetInventory) {
			*catalog = append(*catalog, json.RawMessage(`{"type":"function","name":"other"}`))
		}},
		{name: "inventory revision", apply: func(_ *Options, _ *[]json.RawMessage, inventory *releasedDatasetInventory) {
			inventory.Revision += "-other"
		}},
		{name: "inventory digest", apply: func(_ *Options, _ *[]json.RawMessage, inventory *releasedDatasetInventory) {
			inventory.ArtifactDigest = strings.Repeat("a", 64)
		}},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			options := base
			mutatedCatalog := append([]json.RawMessage(nil), catalog...)
			mutatedInventory := *inventory
			mutation.apply(&options, &mutatedCatalog, &mutatedInventory)
			second, err := buildAttemptContext(options, tasks[0], mutatedCatalog, mutatedInventory)
			if err != nil {
				t.Fatal(err)
			}
			if reflect.DeepEqual(first, second) {
				t.Fatal("behavior-affecting mutation did not change attempt context")
			}
		})
	}
	if model, err := canonicalExecutionModel(
		"ws://127.0.0.1:1/v1/realtime?model=url-model", "ignored",
	); err != nil || model != "url-model" {
		t.Fatalf("endpoint-selected model = %q, err=%v", model, err)
	}
	if _, err := canonicalExecutionModel("ws://127.0.0.1:1/v1/realtime", ""); err == nil {
		t.Fatal("candidate evidence accepted an unidentified endpoint-default model")
	}
	if _, err := canonicalExecutionModel(
		"ws://127.0.0.1:1/v1/realtime?model=a&model=b", "ignored",
	); err == nil {
		t.Fatal("candidate evidence accepted an ambiguous endpoint model")
	}
}

func TestCandidateEvidenceRefusesOriginThatDiffersFromExecution(t *testing.T) {
	root, _, inventory := candidateRecoveryFixture(t)
	for _, testCase := range []struct {
		name      string
		transport string
		endpoint  string
	}{
		{name: "different endpoint", transport: bench.TransportWebSocket, endpoint: "ws://127.0.0.1:2/v1/realtime"},
		{name: "different transport", transport: bench.TransportWebRTC, endpoint: "ws://127.0.0.1:1/v1/realtime"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			origin, err := candidate.NewRunOrigin(
				candidate.OriginHermetic, testCase.transport, testCase.endpoint,
			)
			if err != nil {
				t.Fatal(err)
			}
			plugin := &rejectingCandidatePlugin{cause: errors.New("must not reach recorder")}
			result, err := Run(t.Context(), Options{
				Root: root, Endpoint: "ws://127.0.0.1:1/v1/realtime", Model: "model-a",
				Cell: bench.Reference(), Evidence: plugin, EvidenceOrigin: origin,
				releasedInventory: inventory,
			})
			if err == nil || !strings.Contains(err.Error(), "differs from the execution") ||
				len(result.Tasks) != 0 || plugin.attempt.Case != "" {
				t.Fatalf("origin mismatch: err=%v result=%+v attempt=%+v", err, result, plugin.attempt)
			}
		})
	}
}

func TestCandidateEvidenceRecoverySkipsFDBV3Playback(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "recording-001")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"id":"ignored","domain":"ecommerce","title":"Track an order","difficulty":"easy","expected_tool_calls":[{"function":"track_order","args":{"order_id":"BOB12"}}],"disfluency_features":["spelled_identifier"]}`
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "input.wav"), []byte("must not be read"), 0o600); err != nil {
		t.Fatal(err)
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:1/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	plugin := &recoveringCandidatePlugin{completion: validRecoveredCompletion}
	result, err := Run(t.Context(), Options{
		Root: root, Endpoint: "ws://127.0.0.1:1/v1/realtime", Cell: bench.Reference(),
		Model:    "model-a",
		Evidence: plugin, EvidenceOrigin: origin, releasedInventory: fixtureInventory(t, root),
	})
	if err != nil {
		t.Fatal(err)
	}
	if plugin.beginCalls != 0 || plugin.recoverCalls != 1 || len(result.Tasks) != 1 ||
		!result.Tasks[0].Completed || !result.Tasks[0].Passed || !plugin.result.Summary.Complete {
		t.Fatalf("recovered run: begin=%d recover=%d result=%+v finish=%+v",
			plugin.beginCalls, plugin.recoverCalls, result, plugin.result)
	}
}

func validRecoveredCompletion(attempt candidate.Attempt) (candidate.Completion, error) {
	var context attemptContext
	if err := json.Unmarshal(attempt.Context, &context); err != nil {
		return candidate.Completion{}, err
	}
	arguments := `{"order_id":"BOB12"}`
	result, _ := visibleSimulatorResult("track_order", json.RawMessage(arguments))
	transcript := bench.Transcript{
		PlaybackMS: 1000,
		Moments: []bench.Moment{
			{AtMS: 0, Kind: bench.MomentReady},
			{AtMS: 10, Kind: bench.MomentToolCall, CallID: "call-1", Name: "track_order", Arguments: arguments},
			{AtMS: 20, Kind: bench.MomentToolResult, CallID: "call-1", Name: "track_order", Text: string(result)},
		},
	}
	inventory := releasedDatasetInventory{
		Revision:       context.Execution.InventoryRevision,
		ArtifactDigest: context.Execution.InventoryArtifactSHA256,
		TaskNames:      []string{context.Task.ID},
	}
	outcome, _, err := scoreTaskTranscript(context.Task, transcript, inventory)
	if err != nil {
		return candidate.Completion{}, err
	}
	return candidate.Completion{Attempt: attempt, Outcome: outcome, Transcript: transcript}, nil
}

func fixtureInventory(t *testing.T, root string) *releasedDatasetInventory {
	t.Helper()
	names, digest, err := releasedArtifactIdentity(root)
	if err != nil {
		t.Fatal(err)
	}
	return &releasedDatasetInventory{
		Revision: "test-revision", ArtifactDigest: digest, TaskNames: names,
	}
}

func candidateRecoveryFixture(
	t *testing.T,
) (string, candidate.RunOrigin, *releasedDatasetInventory) {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "recording-001")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"id":"ignored","domain":"ecommerce","title":"Track an order","difficulty":"easy","expected_tool_calls":[{"function":"track_order","args":{"order_id":"BOB12"}}],"disfluency_features":["spelled_identifier"]}`
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "input.wav"), []byte("must not be read"), 0o600); err != nil {
		t.Fatal(err)
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:1/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	return root, origin, fixtureInventory(t, root)
}
