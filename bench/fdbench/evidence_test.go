package fdbench

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

type refusingEvidencePlugin struct {
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
