package fdbv3

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

type rejectingCandidatePlugin struct {
	attempt candidate.Attempt
	result  bench.Result
	cause   error
}

func TestRunRefusesMissingEvidenceBeforeDatasetAccess(t *testing.T) {
	_, err := Run(t.Context(), Options{Root: filepath.Join(t.TempDir(), "missing")})
	if err == nil || !strings.Contains(err.Error(), "evidence plug-in") {
		t.Fatalf("missing evidence error = %v", err)
	}
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
		Evidence: plugin, EvidenceOrigin: origin,
	})
	if !errors.Is(runErr, refusal) {
		t.Fatalf("run error = %v, want recorder refusal", runErr)
	}
	if plugin.attempt.Suite != "fdb-v3" || plugin.attempt.Case != "recording-001" ||
		plugin.attempt.Trial != 1 || len(plugin.attempt.Context) == 0 {
		t.Fatalf("attempt = %+v", plugin.attempt)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].Completed || result.Summary.Complete {
		t.Fatalf("result = %+v", result)
	}
	if len(plugin.result.Tasks) != 1 || plugin.result.Tasks[0].ID != "recording-001" {
		t.Fatalf("finish result = %+v", plugin.result)
	}
}
