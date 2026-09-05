package fdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

type rejectingEvidencePlugin struct {
	beginAttempt candidate.Attempt
	finishResult bench.Result
	beginErr     error
}

type recoveringEvidencePlugin struct {
	beginCalls   int
	recoverCalls int
	result       bench.Result
}

type recoveredTranscript struct{ transcript bench.Transcript }

func (evidence recoveredTranscript) ReopenTranscript(context.Context) (bench.Transcript, error) {
	return evidence.transcript, nil
}
func (recoveredTranscript) CommitValidated(context.Context) error { return nil }

func (plugin *recoveringEvidencePlugin) BindRun(
	_ context.Context, _ string, _ bench.Cell, provenance bench.Provenance, _ candidate.RunOrigin,
) (bench.Provenance, error) {
	return provenance, nil
}

func (plugin *recoveringEvidencePlugin) BeginAttempt(
	context.Context, candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	plugin.beginCalls++
	return nil, errors.New("recovered FDB attempt must not begin recording")
}

func (plugin *recoveringEvidencePlugin) RecoverAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.Recovery, bool, error) {
	plugin.recoverCalls++
	transcript := bench.Transcript{}
	outcome, err := validateRecoveredOutcome(context.Background(), attempt, transcript)
	return candidate.Recovery{
		Completion: candidate.Completion{Attempt: attempt, Outcome: outcome, Transcript: transcript},
		Evidence:   recoveredTranscript{transcript: transcript},
	}, err == nil, err
}

func (plugin *recoveringEvidencePlugin) FinishSuite(_ context.Context, result bench.Result) error {
	plugin.result = result
	return nil
}

func (plugin *rejectingEvidencePlugin) BeginAttempt(
	_ context.Context, attempt candidate.Attempt,
) (candidate.AttemptEvidence, error) {
	plugin.beginAttempt = attempt
	return nil, plugin.beginErr
}

func (plugin *rejectingEvidencePlugin) FinishSuite(
	_ context.Context, result bench.Result,
) error {
	plugin.finishResult = result
	if len(result.Tasks) > 0 {
		result.Tasks[0].Notes["plugin_mutation"] = "must not escape"
	}
	return nil
}

func TestCandidateEvidenceBeginsBeforePlaybackAndFinishesIncompleteSuite(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, string(Interruption), "1")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"context_text":"Explain the result","current_turn_text":"Actually stop","timestamps":[1,2]}`
	if err := os.WriteFile(filepath.Join(directory, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	// Playback must never reach this deliberately invalid WAV because the
	// candidate recorder refuses the attempt before a session can be opened.
	if err := os.WriteFile(filepath.Join(directory, "input.wav"), []byte("not a wav"), 0o600); err != nil {
		t.Fatal(err)
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:1/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("create-only recorder unavailable")
	plugin := &rejectingEvidencePlugin{beginErr: refusal}
	result, runErr := Run(t.Context(), Options{
		Root: root, Endpoint: "ws://127.0.0.1:1/v1/realtime", Cell: bench.Reference(),
		Categories: []Category{Interruption}, Evidence: plugin, EvidenceOrigin: origin,
	})
	if !errors.Is(runErr, refusal) {
		t.Fatalf("run error = %v, want evidence refusal", runErr)
	}
	if plugin.beginAttempt.Case != string(Interruption)+"/1" ||
		plugin.beginAttempt.Suite != "fdb-v1.5" || plugin.beginAttempt.Trial != 1 {
		t.Fatalf("begin attempt = %+v", plugin.beginAttempt)
	}
	if plugin.beginAttempt.Origin.Live || plugin.beginAttempt.Context == nil {
		t.Fatal("attempt lost its explicit hermetic origin or deterministic context")
	}
	if len(result.Tasks) != 1 || result.Tasks[0].Completed || result.Summary.Complete {
		t.Fatalf("result should retain one incomplete attempt: %+v", result)
	}
	if len(plugin.finishResult.Tasks) != 1 || plugin.finishResult.Tasks[0].Completed {
		t.Fatalf("finish result = %+v", plugin.finishResult)
	}
	if result.Tasks[0].Notes["plugin_mutation"] != "" {
		t.Fatal("evidence plug-in mutated the runner-owned result")
	}
}

func TestCandidateEvidenceRequiresAnExplicitRunOrigin(t *testing.T) {
	plugin := &rejectingEvidencePlugin{}
	_, err := Run(t.Context(), Options{Evidence: plugin})
	if err == nil || !strings.Contains(err.Error(), "candidate run origin kind is invalid") {
		t.Fatalf("run error = %v, want origin refusal before dataset access", err)
	}
	if plugin.beginAttempt.Suite != "" || plugin.finishResult.Suite != "" {
		t.Fatal("invalid origin reached the evidence plug-in")
	}
}

func TestCandidateEvidenceRecoverySkipsFDBPlayback(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, string(Interruption), "1")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"context_text":"Explain the result","current_turn_text":"Actually stop","timestamps":[1,2]}`
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
	plugin := &recoveringEvidencePlugin{}
	result, err := Run(t.Context(), Options{
		Root: root, Endpoint: "ws://127.0.0.1:1/v1/realtime", Cell: bench.Reference(),
		Categories: []Category{Interruption}, Evidence: plugin, EvidenceOrigin: origin,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plugin.beginCalls != 0 || plugin.recoverCalls != 1 || len(result.Tasks) != 1 ||
		!result.Tasks[0].Completed || result.Tasks[0].Passed ||
		result.Tasks[0].Applicability != bench.NotApplicable || !plugin.result.Summary.Complete {
		t.Fatalf("recovered run: begin=%d recover=%d result=%+v finish=%+v",
			plugin.beginCalls, plugin.recoverCalls, result, plugin.result)
	}
}
