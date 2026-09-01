package fdbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

type refusingEvidencePlugin struct {
	attempt candidate.Attempt
	result  bench.Result
	cause   error
}

type recoveringEvidencePlugin struct {
	beginCalls   int
	recoverCalls int
	result       bench.Result
}

func (plugin *recoveringEvidencePlugin) BindRun(
	_ context.Context, _ string, _ bench.Cell, provenance bench.Provenance, _ candidate.RunOrigin,
) (bench.Provenance, error) {
	return provenance, nil
}

func (plugin *recoveringEvidencePlugin) BeginAttempt(
	context.Context, candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	plugin.beginCalls++
	return nil, errors.New("recovered FD-Bench attempt must not begin recording")
}

func (plugin *recoveringEvidencePlugin) RecoverAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.Completion, bool, error) {
	plugin.recoverCalls++
	return candidate.Completion{
		Attempt: attempt,
		Outcome: bench.TaskOutcome{ID: attempt.Case, Completed: true, Passed: true},
	}, true, nil
}

func (plugin *recoveringEvidencePlugin) FinishSuite(_ context.Context, result bench.Result) error {
	plugin.result = result
	return nil
}

func (plugin *refusingEvidencePlugin) BeginAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	plugin.attempt = attempt
	return nil, plugin.cause
}

func (plugin *refusingEvidencePlugin) FinishSuite(
	_ context.Context, result bench.Result,
) error {
	plugin.result = result
	return nil
}

func TestCandidateEvidenceBeginsForExactConditionBeforeAudioPlayback(t *testing.T) {
	root := t.TempDir()
	condition := "clean-easy"
	directory := filepath.Join(root, condition)
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "conversation-001.timestamps"),
		[]byte(`[{"start":0,"end":16000},{"start":24000,"end":32000}]`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "conversation-001.wav"), []byte("invalid on purpose"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:1/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("candidate recording unavailable")
	plugin := &refusingEvidencePlugin{cause: refusal}
	result, runErr := Run(t.Context(), Options{
		Root: root, Conditions: []string{condition},
		Endpoint: "ws://127.0.0.1:1/v1/realtime", Cell: bench.Reference(),
		Evidence: plugin, EvidenceOrigin: origin,
	})
	if !errors.Is(runErr, refusal) {
		t.Fatalf("run error = %v, want recorder refusal", runErr)
	}
	if plugin.attempt.Suite != "fd-bench" ||
		plugin.attempt.Case != condition+"/conversation-001" ||
		plugin.attempt.Trial != 1 || len(plugin.attempt.Context) == 0 {
		t.Fatalf("attempt = %+v", plugin.attempt)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].Completed || result.Summary.Complete {
		t.Fatalf("result = %+v", result)
	}
	if len(plugin.result.Tasks) != 1 || plugin.result.Tasks[0].ID != plugin.attempt.Case {
		t.Fatalf("finish result = %+v", plugin.result)
	}
}

func TestCandidateEvidenceRecoverySkipsFDPlayback(t *testing.T) {
	root := t.TempDir()
	condition := "clean-easy"
	directory := filepath.Join(root, condition)
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, "conversation-001.timestamps"),
		[]byte(`[{"start":0,"end":16000}]`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	// If recovery does not short-circuit bench.Play, this invalid input makes
	// the run fail before it can report a completed deterministic row.
	if err := os.WriteFile(
		filepath.Join(directory, "conversation-001.wav"), []byte("must not be read"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:1/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	plugin := &recoveringEvidencePlugin{}
	result, err := Run(t.Context(), Options{
		Root: root, Conditions: []string{condition}, Endpoint: "ws://127.0.0.1:1/v1/realtime",
		Cell: bench.Reference(), Evidence: plugin, EvidenceOrigin: origin,
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
