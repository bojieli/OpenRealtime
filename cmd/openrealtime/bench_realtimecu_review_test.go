package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/realtimecu"
	review "github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/gemini"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	reviewffmpeg "github.com/bojieli/OpenRealtime/bench/review/media/ffmpeg"
)

func TestOpenRealtimeCUReviewCLIComposesExactGeminiAndFullDecodeFFmpeg(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "realtime-cu-review")
	resources, err := openRealtimeCUReviewCLI(context.Background(), realtimeCUReviewCLIConfig{
		Directory: directory, Provider: gemini.RegistrationName,
		APIKeyEnvironment: "TEST_GEMINI_KEY", Concurrency: 4,
	}, "deployment-token-fixture", func(name string) (string, bool) {
		values := map[string]string{
			"TEST_GEMINI_KEY":                  "gemini-key-fixture-long-enough",
			realtimeCULocalModelKeyEnvironment: "local-model-key-fixture-long-enough",
			realtimeCULocalASRKeyEnvironment:   "local-asr-key-fixture-long-enough",
		}
		value, found := values[name]
		return value, found
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resources.close() })
	if resources.bundle == nil || resources.lease == nil ||
		resources.reviewer.Model != "gemini-3.7-flash" ||
		resources.encoder.Name == "" ||
		resources.attestor.Capability != reviewmedia.FullDecodeAttestationCapability {
		t.Fatalf("Realtime-CU review resources = %+v", resources)
	}
	for _, secret := range []string{
		"gemini-key-fixture-long-enough", "deployment-token-fixture",
		"local-model-key-fixture-long-enough", "local-asr-key-fixture-long-enough",
	} {
		if !slices.Contains(resources.sensitive, secret) {
			t.Fatal("review resources omitted a provider credential from leakage protection")
		}
	}
	for _, path := range []string{
		resources.bundle.Directory(), resources.evaluationReceiptDirectory,
		resources.evaluationQuarantineDirectory,
	} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("review path %s info=%v error=%v", path, info, err)
		}
	}
	if _, err := os.Lstat(resources.sourceReceiptPath); !os.IsNotExist(err) {
		t.Fatalf("source receipt became visible before deterministic source publication: %v", err)
	}
}

func TestOpenRealtimeCUReviewCLIRejectsRawAndEncodedSensitivePaths(t *testing.T) {
	secret := "review-key-fixture-long-enough"
	for _, fragment := range []string{
		secret, base64.RawURLEncoding.EncodeToString([]byte(secret)),
	} {
		t.Run(fragment[:8], func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "review-"+fragment)
			resources, err := openRealtimeCUReviewCLI(t.Context(), realtimeCUReviewCLIConfig{
				Directory: directory, Provider: gemini.RegistrationName,
				APIKeyEnvironment: "TEST_GEMINI_KEY", Concurrency: 4,
			}, "", func(name string) (string, bool) {
				return secret, name == "TEST_GEMINI_KEY"
			})
			if err == nil || resources != nil || !strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("sensitive path resources=%+v error=%v", resources, err)
			}
		})
	}
}

func TestOpenRealtimeCUReviewCLIRollsBackExternalDirectoriesAfterBundleFailure(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "review")
	want := errors.New("fixture bundle constructor failed")
	config := realtimeCUReviewCLIConfig{
		Directory: directory, Provider: gemini.RegistrationName,
		APIKeyEnvironment: "TEST_GEMINI_KEY", Concurrency: 4,
		newBundle: func(realtimecu.ReviewBundleOptions) (*realtimecu.ReviewBundle, error) {
			return nil, want
		},
	}
	resources, err := openRealtimeCUReviewCLI(t.Context(), config, "", func(name string) (string, bool) {
		return "gemini-key-fixture-long-enough", name == "TEST_GEMINI_KEY"
	})
	if err == nil || resources != nil {
		t.Fatalf("failed constructor resources=%+v error=%v", resources, err)
	}
	for _, path := range []string{
		directory, directory + ".evaluation-receipts", directory + ".evaluation-quarantine",
	} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("failed constructor left create-only debris at %s: %v", path, statErr)
		}
	}
	config.newBundle = nil
	retry, err := openRealtimeCUReviewCLI(t.Context(), config, "", func(name string) (string, bool) {
		return "gemini-key-fixture-long-enough", name == "TEST_GEMINI_KEY"
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := retry.close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRealtimeCUReviewCLIRejectsUnpinnedProviderAndOverlappingPaths(t *testing.T) {
	parent := t.TempDir()
	tests := []struct {
		name   string
		config realtimeCUReviewCLIConfig
	}{
		{
			name: "provider",
			config: realtimeCUReviewCLIConfig{
				Directory: filepath.Join(parent, "provider-review"), Provider: "google.latest",
				APIKeyEnvironment: "TEST_GEMINI_KEY",
			},
		},
		{
			name: "overlap",
			config: realtimeCUReviewCLIConfig{
				Directory: filepath.Join(parent, "overlap-review"), Provider: gemini.RegistrationName,
				APIKeyEnvironment: "TEST_GEMINI_KEY",
				SourceReceiptPath: filepath.Join(parent, "overlap-review", "receipt.json"),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if resources, err := openRealtimeCUReviewCLI(
				context.Background(), test.config, "", func(string) (string, bool) {
					return "gemini-key-fixture-long-enough", true
				},
			); err == nil || resources != nil {
				t.Fatalf("openRealtimeCUReviewCLI() resources=%+v error=%v", resources, err)
			}
		})
	}
}

func TestOpenRealtimeCUReviewCLINoDirectoryLeavesEvidenceUnconfigured(t *testing.T) {
	resources, err := openRealtimeCUReviewCLI(context.Background(), realtimeCUReviewCLIConfig{
		Provider: gemini.RegistrationName, APIKeyEnvironment: "GEMINI_API_KEY", Concurrency: 4,
	}, "", os.LookupEnv)
	if err != nil || resources != nil {
		t.Fatalf("unconfigured review resources=%+v error=%v", resources, err)
	}
}

func TestRealtimeCUReviewVerificationReportsDiagnosticAsIncomplete(t *testing.T) {
	var output bytes.Buffer
	options := realtimecu.ReviewBundleVerificationOptions{
		Directory: "/fixture/review", ReceiptPath: "/fixture/review.receipt.json",
		SourceReceiptPath:          "/fixture/review.source-receipt.json",
		EvaluationReceiptDirectory: "/fixture/review.evaluation-receipts",
	}
	verified := realtimecu.ReviewBundleVerification{
		Receipt: realtimecu.ReviewBundleReceipt{ManifestSHA256: strings.Repeat("a", 64)},
		SourceReceipt: realtimecu.ReviewSourceReceipt{
			ReceiptSHA256: strings.Repeat("b", 64),
		},
		Manifest: realtimecu.ReviewManifest{
			Expected: 16, Attempts: []realtimecu.ReviewAttempt{{Case: "static-control/pixel"}},
		},
		EvaluationReceipts: map[string]review.EvaluationBundleReceipt{
			"static-control/pixel": {},
		},
		EvidenceComplete: false,
	}
	err := reportRealtimeCUReviewVerification(&output, options, verified)
	if err != nil || !strings.Contains(output.String(), "1/16") ||
		!strings.Contains(output.String(), "DIAGNOSTIC POPULATION INCOMPLETE") ||
		strings.Contains(output.String(), "population-complete") ||
		strings.Contains(output.String(), "accepted") ||
		strings.Contains(output.String(), "release-ready") {
		t.Fatalf("diagnostic verification output=%q error=%v", output.String(), err)
	}
}

func TestRealtimeCUReviewVerificationReportsPopulationWithoutBehavioralAcceptance(t *testing.T) {
	var output bytes.Buffer
	descriptor := gemini.Descriptor()
	attempts := make([]realtimecu.ReviewAttempt, 16)
	receipts := make(map[string]review.EvaluationBundleReceipt, 16)
	for index := range attempts {
		caseID := fmt.Sprintf("fixture-case-%02d", index+1)
		attempts[index] = realtimecu.ReviewAttempt{Case: caseID, Reviewer: &descriptor}
		receipts[caseID] = review.EvaluationBundleReceipt{}
	}
	verified := realtimecu.ReviewBundleVerification{
		Receipt: realtimecu.ReviewBundleReceipt{ManifestSHA256: strings.Repeat("a", 64)},
		SourceReceipt: realtimecu.ReviewSourceReceipt{
			ReceiptSHA256: strings.Repeat("b", 64),
		},
		Manifest:           realtimecu.ReviewManifest{Expected: 16, Attempts: attempts},
		EvaluationReceipts: receipts,
		EvidenceComplete:   true,
	}
	err := reportRealtimeCUReviewVerification(&output, realtimecu.ReviewBundleVerificationOptions{
		Directory: "/fixture/review", ReceiptPath: "/fixture/review.receipt.json",
		SourceReceiptPath: "/fixture/review.source-receipt.json",
	}, verified)
	text := output.String()
	if err != nil || !strings.Contains(text, "population-complete") ||
		!strings.Contains(text, "behavioral acceptance not evaluated") ||
		strings.Contains(text, "release-ready") || strings.Contains(text, "accepted") {
		t.Fatalf("population verification output=%q error=%v", text, err)
	}
}

func TestRunBenchDispatchesCredentialFreeRealtimeCUReviewVerifier(t *testing.T) {
	var output bytes.Buffer
	err := runBench([]string{"verify-realtime-cu-review"}, &output)
	if err == nil || !strings.Contains(err.Error(), "requires -review-dir") {
		t.Fatalf("verify-realtime-cu-review dispatch error=%v output=%q", err, output.String())
	}
	output.Reset()
	err = runBench([]string{"anchor-realtime-cu-review"}, &output)
	if err == nil || !strings.Contains(err.Error(), "requires -review-dir") {
		t.Fatalf("anchor-realtime-cu-review dispatch error=%v output=%q", err, output.String())
	}
}

func TestOpenRealtimeCUReviewCLIPreflightFailureLeavesNoCreateOnlyDebris(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "missing-key-review")
	resources, err := openRealtimeCUReviewCLI(context.Background(), realtimeCUReviewCLIConfig{
		Directory: directory, Provider: gemini.RegistrationName,
		APIKeyEnvironment: "MISSING_REVIEW_KEY",
	}, "", func(string) (string, bool) { return "", false })
	if err == nil || resources != nil {
		t.Fatalf("missing-key resources=%+v error=%v", resources, err)
	}
	for _, path := range []string{
		directory, directory + ".source-receipt.json",
		directory + ".evaluation-receipts", directory + ".evaluation-quarantine",
	} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("failed preflight left create-only debris at %s: %v", path, statErr)
		}
	}
}

func TestCreateRealtimeCUExternalReviewDirectoriesRejectsSymlinkedAncestor(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(linkedParent, "receipts")
	if err := createRealtimeCUExternalReviewDirectories(path); err == nil {
		t.Fatal("external review directory accepted a symlinked ancestor")
	}
	if _, err := os.Lstat(filepath.Join(realParent, "receipts")); !os.IsNotExist(err) {
		t.Fatalf("rejected creation mutated the symlink target: %v", err)
	}
}

func TestOpenRealtimeCUReviewCLIResumeRecoversSealedResultWithoutBrowser(t *testing.T) {
	parent := t.TempDir()
	t.Cleanup(func() { makeRealtimeCUReviewFixtureWritable(parent) })
	directory := filepath.Join(parent, "review")
	sourceReceiptPath := filepath.Join(parent, "source-receipt.json")
	evaluationReceiptDirectory := filepath.Join(parent, "evaluation-receipts")
	evaluationQuarantineDirectory := filepath.Join(parent, "evaluation-quarantine")
	if err := createRealtimeCUExternalReviewDirectories(
		evaluationReceiptDirectory, evaluationQuarantineDirectory,
	); err != nil {
		t.Fatal(err)
	}
	reviewerLease := openFailingRealtimeCUReviewer(t)
	bundle, err := realtimecu.NewReviewBundle(realtimecu.ReviewBundleOptions{
		Directory:    directory,
		VideoFactory: realtimeCUFFmpegReviewFactory{options: reviewffmpeg.Options{}},
		Reviewer:     reviewerLease,
		SourceAnchor: realtimecu.FileReviewSourceReceiptAnchor{Path: sourceReceiptPath},
		EvaluationStores: realtimecu.FileReviewEvaluationReceiptStoreFactory{
			Directory: evaluationReceiptDirectory, QuarantineRoot: evaluationQuarantineDirectory,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := realtimecu.Case{Task: realtimecu.Suite()[0], Grounding: realtimecu.GroundingPixel}
	endpointDigest := sha256.Sum256([]byte("ws://hermetic.invalid/v1/realtime"))
	specification := realtimecu.EvidenceAttempt{
		Suite: realtimecu.SuiteName, Case: item.ID(), Trial: 1, Task: item.Task,
		Grounding: item.Grounding,
		Origin: realtimecu.EvidenceRunOrigin{
			Kind: realtimecu.EvidenceOriginHermetic, Live: false,
			Transport:      bench.TransportWebSocket,
			EndpointSHA256: "sha256:" + hex.EncodeToString(endpointDigest[:]),
		},
	}
	attempt, err := bundle.BeginAttempt(t.Context(), specification)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureAudio(bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: make([]int16, 2_400),
	}); err != nil {
		t.Fatal(err)
	}
	frame := image.NewRGBA(image.Rect(0, 0, 64, 49))
	for y := range 49 {
		for x := range 64 {
			frame.SetRGBA(x, y, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureVideo(bench.SessionVideoCapture{
		Source: "screen", Width: 64, Height: 49, MediaType: "image/png",
		WireTimestamp: 1, EpisodeAtMS: 0, Data: encoded.Bytes(),
	}); err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{
		ID: item.ID(), Completed: true, Passed: true,
		Metrics: map[string]float64{
			"task_success_rate": 1, "correct_action_rate": 1, "deadline_miss_count": 0,
		},
		Notes: map[string]string{"page_result": "fixture deterministic result"},
	}
	if err := attempt.Complete(t.Context(), realtimecu.EvidenceCompletion{
		Attempt: specification, Outcome: outcome,
		Transcript: bench.Transcript{PlaybackMS: 100, Moments: []bench.Moment{
			{Kind: bench.MomentReady, AtMS: 0},
			{Kind: bench.MomentVideoFrame, Source: "screen", AtMS: 0},
		}},
		Page: realtimecu.PageResult{
			Complete: true, Success: true, Reason: "fixture deterministic result", CompletedAtMS: 50,
		},
	}); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: realtimecu.SuiteName, Cell: realtimecu.ReferenceCell(),
		Provenance: bench.Provenance{
			Revision: "fixture-review-revision", ExecutableSHA256: strings.Repeat("a", 64),
			StartedAt: time.Unix(1, 0).UTC().Format(time.RFC3339), Modified: false,
		},
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil {
		t.Fatal("source FinishSuite unexpectedly completed its advisory review")
	}
	if err := bundle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reviewerLease.Close(); err != nil {
		t.Fatal(err)
	}
	resources, err := openRealtimeCUReviewCLI(t.Context(), realtimeCUReviewCLIConfig{
		Directory: directory, Resume: true, Provider: gemini.RegistrationName,
		APIKeyEnvironment: "TEST_GEMINI_KEY", Concurrency: 4,
		SourceReceiptPath:             sourceReceiptPath,
		EvaluationReceiptDirectory:    evaluationReceiptDirectory,
		EvaluationQuarantineDirectory: evaluationQuarantineDirectory,
	}, "", func(name string) (string, bool) {
		return "gemini-key-fixture-long-enough", name == "TEST_GEMINI_KEY"
	})
	if err != nil {
		t.Fatal(err)
	}
	if resources.resumedResult == nil || !reflect.DeepEqual(*resources.resumedResult, result) {
		t.Fatalf("resumed deterministic result = %+v", resources.resumedResult)
	}
	if err := resources.close(); err != nil {
		t.Fatal(err)
	}
}

type failingRealtimeCUReviewer struct {
	mu      sync.Mutex
	claimed bool
	closed  bool
}

var failingRealtimeCUReviewerImplementation = []byte("openrealtime Realtime-CU resume reviewer fixture v1")
var failingRealtimeCUReviewerConfiguration = []byte(`{"failure":"before evaluation publication"}`)

func failingRealtimeCUReviewerCapabilities() review.ProviderCapabilities {
	return review.ProviderCapabilities{
		MediaTypes:        []string{"audio/wav", "video/mp4"},
		MaximumMediaCount: 4, MaximumMediaBytes: 128 << 20,
	}
}

func failingRealtimeCUReviewerDescriptor() review.ProviderDescriptor {
	capabilitiesSHA, err := failingRealtimeCUReviewerCapabilities().SHA256()
	if err != nil {
		panic(err)
	}
	return review.ProviderDescriptor{
		Provider: "fixture", Model: "realtime-cu-resume-fixture", API: "fixture.review",
		APIRevision: "v1",
		Implementation: review.ContentIdentity{
			Version: "fixture.realtime-cu-resume-review.v1",
			SHA256:  realtimeCUTestDigest(failingRealtimeCUReviewerImplementation),
		},
		ConfigurationSHA256: realtimeCUTestDigest(failingRealtimeCUReviewerConfiguration),
		CapabilitiesSHA256:  capabilitiesSHA,
	}
}

func (*failingRealtimeCUReviewer) Descriptor() review.ProviderDescriptor {
	return failingRealtimeCUReviewerDescriptor()
}
func (*failingRealtimeCUReviewer) Capabilities() review.ProviderCapabilities {
	return failingRealtimeCUReviewerCapabilities()
}
func (*failingRealtimeCUReviewer) Implementation() []byte {
	return append([]byte(nil), failingRealtimeCUReviewerImplementation...)
}
func (*failingRealtimeCUReviewer) Configuration() []byte {
	return append([]byte(nil), failingRealtimeCUReviewerConfiguration...)
}
func (provider *failingRealtimeCUReviewer) Claim() error {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.claimed {
		return errors.New("fixture reviewer already claimed")
	}
	provider.claimed = true
	return nil
}
func (*failingRealtimeCUReviewer) Review(
	context.Context, review.PreparedRequest,
) (review.ProviderResponse, error) {
	return review.ProviderResponse{}, errors.New("fixture reviewer outage")
}
func (*failingRealtimeCUReviewer) VerifyResponse(
	context.Context, review.PreparedRequest, review.ProviderResponse,
) error {
	return errors.New("fixture reviewer emitted no response")
}
func (provider *failingRealtimeCUReviewer) Close() error {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if !provider.claimed || provider.closed {
		return errors.New("fixture reviewer close lifecycle is invalid")
	}
	provider.closed = true
	return nil
}

func openFailingRealtimeCUReviewer(t *testing.T) *review.ProviderLease {
	t.Helper()
	registration := review.Registration{
		Name: "realtime-cu-resume-fixture", Descriptor: failingRealtimeCUReviewerDescriptor(),
		Capabilities:   failingRealtimeCUReviewerCapabilities(),
		Implementation: append([]byte(nil), failingRealtimeCUReviewerImplementation...),
		Configuration:  append([]byte(nil), failingRealtimeCUReviewerConfiguration...),
		Factory: func(context.Context) (review.Provider, error) {
			return &failingRealtimeCUReviewer{}, nil
		},
	}
	registry, err := review.NewRegistry([]review.Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(t.Context(), registration.Name)
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func realtimeCUTestDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func makeRealtimeCUReviewFixtureWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err == nil {
			if entry.IsDir() {
				_ = os.Chmod(path, 0o700)
			} else {
				_ = os.Chmod(path, 0o600)
			}
		}
		return nil
	})
}
