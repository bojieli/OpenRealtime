package gemini

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/bench/review"
)

// TestLiveGeminiMultimodalReview is an opt-in transport/contract smoke, not a
// benchmark result. It uses repository-owned fixture inputs only and does not
// write a review bundle or claim behavioral coverage.
func TestLiveGeminiMultimodalReview(t *testing.T) {
	if os.Getenv("OPENREALTIME_GEMINI_LIVE") != "1" {
		t.Skip("set OPENREALTIME_GEMINI_LIVE=1 to call the pinned Google endpoint")
	}
	apiKey, err := EnvironmentAPIKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	fixtures := []struct {
		source, target, kind, role, mediaType string
	}{
		{"../../realtimecu/testdata/audio/static.wav", "static.wav", "audio", "transport_smoke_audio", "audio/wav"},
		{"../../scenario/testdata/build-finished.png", "build-finished.png", "image", "transport_smoke_image", "image/png"},
	}
	request := review.Request{
		AttemptID: "transport-smoke/gemini-3.7-flash/1",
		Suite:     "transport-smoke", Case: "gemini-multimodal", Trial: 1,
		RootDirectory: root,
		Context: json.RawMessage(`{
          "deterministic_outcome":"not_applicable",
          "purpose":"provider transport and structured-output validation only",
          "reportable_benchmark_evidence":false
        }`),
	}
	for _, fixture := range fixtures {
		payload, readErr := os.ReadFile(fixture.source)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if writeErr := os.WriteFile(filepath.Join(root, fixture.target), payload, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		request.Media = append(request.Media, review.Media{
			Path: fixture.target, Kind: fixture.kind, Role: fixture.role,
			MediaType: fixture.mediaType, SHA256: digest(payload),
		})
	}
	plugin, err := New(apiKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := review.Evaluate(t.Context(), plugin, request)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Record.Provider.Model != ModelID ||
		evaluation.Record.ReportedModel != ModelID {
		t.Fatalf("live review provenance = %+v", evaluation.Record)
	}
	t.Logf("pinned model %s completed a store=false request (provider ID %q)",
		ModelID, evaluation.Record.ProviderRequestID)
}
