package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench/review"
)

// TestLiveGeminiMultimodalReview is an opt-in transport/contract smoke, not a
// benchmark result. It uses repository-owned fixture inputs only, retains the
// exact provider-verified exchange at a caller-owned create-only path, and
// does not claim behavioral coverage.
func TestLiveGeminiMultimodalReview(t *testing.T) {
	if os.Getenv("OPENREALTIME_GEMINI_LIVE") != "1" {
		t.Skip("set OPENREALTIME_GEMINI_LIVE=1 to call the pinned Google endpoint")
	}
	bundleDirectory, receiptPath := liveReviewDestinations(t)
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
	receipt, err := review.WriteEvaluationBundle(t.Context(), review.EvaluationBundleOptions{
		Directory: bundleDirectory,
	}, evaluation)
	if err != nil {
		t.Fatalf("retain live Gemini review bundle: %v", err)
	}
	opened, err := review.VerifyEvaluationBundle(t.Context(), review.EvaluationBundleOptions{
		Directory: bundleDirectory,
	}, receipt)
	if err != nil {
		t.Fatalf("verify retained live Gemini review bundle: %v", err)
	}
	if opened.Record.Provider.Model != ModelID || opened.Receipt != receipt {
		t.Fatalf("retained live Gemini review changed provenance or receipt")
	}
	writeLiveReviewReceipt(t, receiptPath, receipt)
	t.Logf("retained pinned-model %s store=false exchange at %s (receipt %s; provider ID state %q, ID %q)",
		ModelID, bundleDirectory, receipt.ReceiptSHA256,
		evaluation.Record.ProviderRequestIDState, evaluation.Record.ProviderRequestID)
}

func liveReviewDestinations(t *testing.T) (string, string) {
	t.Helper()
	directory, receiptPath, err := validateLiveReviewDestinations(
		os.Getenv("OPENREALTIME_GEMINI_LIVE_REVIEW_DIR"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return directory, receiptPath
}

func validateLiveReviewDestinations(value string) (string, string, error) {
	directory := strings.TrimSpace(value)
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return "", "", errors.New(
			"OPENREALTIME_GEMINI_LIVE_REVIEW_DIR must name a clean absolute create-only directory",
		)
	}
	if info, err := os.Stat(filepath.Dir(directory)); err != nil || !info.IsDir() {
		return "", "", errors.New(
			"OPENREALTIME_GEMINI_LIVE_REVIEW_DIR parent must be an existing directory",
		)
	}
	receiptPath := directory + ".receipt.json"
	for _, path := range []string{directory, receiptPath} {
		if _, err := os.Lstat(path); err == nil {
			return "", "", fmt.Errorf("live Gemini review destination already exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", fmt.Errorf("inspect live Gemini review destination %s: %w", path, err)
		}
	}
	return directory, receiptPath, nil
}

func writeLiveReviewReceipt(
	t *testing.T, path string, receipt review.EvaluationBundleReceipt,
) {
	t.Helper()
	payload, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatalf("encode live Gemini review receipt: %v", err)
	}
	payload = append(payload, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatalf("create live Gemini review receipt: %v", err)
	}
	complete := false
	closed := false
	defer func() {
		if !closed {
			if closeErr := file.Close(); closeErr != nil && complete {
				t.Errorf("close live Gemini review receipt: %v", closeErr)
			}
		}
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		t.Fatalf("write live Gemini review receipt: %v", err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("sync live Gemini review receipt: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close live Gemini review receipt: %v", err)
	}
	closed = true
	complete = true
}

func TestLiveReviewDestinationsRequireAnAbsentAbsoluteBundleAndReceipt(t *testing.T) {
	root := t.TempDir()
	bundle := filepath.Join(root, "retained-review")
	directory, receipt, err := validateLiveReviewDestinations(bundle)
	if err != nil || directory != bundle || receipt != bundle+".receipt.json" {
		t.Fatalf("validate absent destinations = %q, %q, %v", directory, receipt, err)
	}
	for name, setup := range map[string]func() string{
		"relative": func() string { return "retained-review" },
		"existing bundle": func() string {
			if err := os.Mkdir(bundle, 0o700); err != nil {
				t.Fatal(err)
			}
			return bundle
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := setup()
			if _, _, err := validateLiveReviewDestinations(candidate); err == nil {
				t.Fatalf("validateLiveReviewDestinations(%q) unexpectedly succeeded", candidate)
			}
		})
	}
	receiptOnlyBundle := filepath.Join(root, "receipt-exists")
	if err := os.WriteFile(receiptOnlyBundle+".receipt.json", []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateLiveReviewDestinations(receiptOnlyBundle); err == nil {
		t.Fatal("validateLiveReviewDestinations accepted an existing external receipt")
	}
}
