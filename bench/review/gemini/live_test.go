package gemini

import (
	"encoding/json"
	"os"
	"os/exec"
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
	root := t.TempDir()
	videoPath := filepath.Join(root, "synthetic-synchronized-av.mp4")
	command := exec.CommandContext(t.Context(), "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=64x64:rate=5:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=24000:duration=1",
		"-map", "0:v:0", "-map", "1:a:0", "-c:v", "mpeg4", "-q:v", "5",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "-b:a", "32k", "-shortest",
		"-movflags", "+faststart", "-y", videoPath,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generate synchronized Gemini video smoke fixture: %v: %s", err, output)
	}
	fixtures := []struct {
		source, target, kind, role, mediaType string
	}{
		{"../../realtimecu/testdata/audio/static.wav", "static.wav", "audio", "transport_smoke_audio", "audio/wav"},
		{"../../scenario/testdata/build-finished.png", "build-finished.png", "image", "transport_smoke_image", "image/png"},
		{videoPath, "review-synchronized-av.mp4", "video", "transport_smoke_synchronized_av", "video/mp4"},
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
	registry, err := review.NewRegistry([]review.Registration{
		Registration(EnvironmentAPIKey),
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(t.Context(), RegistrationName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Errorf("close Gemini review lease: %v", closeErr)
		}
	}()
	evaluation, err := review.Evaluate(t.Context(), lease, request)
	if err != nil {
		t.Fatal(err)
	}
	if evaluation.Record.Provider.Model != ModelID ||
		evaluation.Record.ReportedModel != ModelID {
		t.Fatalf("live review provenance = %+v", evaluation.Record)
	}
	t.Logf("pinned model %s completed a store=false request (provider ID state %q, ID %q)",
		ModelID, evaluation.Record.ProviderRequestIDState, evaluation.Record.ProviderRequestID)
}
