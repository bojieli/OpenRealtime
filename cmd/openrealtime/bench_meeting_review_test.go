package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/meeting"
	review "github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/gemini"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
)

func TestOpenMeetingReviewCLISelectsExactGemini37FlashAndFullDecodeFFmpeg(t *testing.T) {
	parent := t.TempDir()
	resources, err := openMeetingReviewCLI(context.Background(), meetingReviewCLIConfig{
		Directory:         filepath.Join(parent, "meeting-review"),
		Provider:          gemini.RegistrationName,
		APIKeyEnvironment: "MEETING_REVIEW_KEY",
		FFmpegPath:        "/usr/bin/ffmpeg",
		FFprobePath:       "/usr/bin/ffprobe",
		BubblewrapPath:    "/usr/bin/bwrap",
	}, "deployment-token-long-enough", func(name string) (string, bool) {
		if name != "MEETING_REVIEW_KEY" {
			return "", false
		}
		return "fixture-gemini-key-at-least-sixteen", true
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resources.close()
	if resources.reviewer.Model != "gemini-3.7-flash" ||
		resources.reviewer != gemini.Descriptor() || resources.encoder.Name == "" ||
		resources.attestor.Capability != reviewmedia.FullDecodeAttestationCapability ||
		resources.bundle == nil || resources.lease == nil {
		t.Fatalf("production Meeting review composition = %+v", resources)
	}
	if resources.sourceReceiptPath != filepath.Join(parent, "meeting-review.source-receipt.json") ||
		resources.evaluationReceiptDirectory != filepath.Join(parent, "meeting-review.evaluation-receipts") ||
		resources.evaluationQuarantineDirectory != filepath.Join(parent, "meeting-review.evaluation-quarantine") {
		t.Fatalf("external review destinations = %q / %q / %q",
			resources.sourceReceiptPath, resources.evaluationReceiptDirectory,
			resources.evaluationQuarantineDirectory)
	}
}

func TestOpenMeetingReviewCLIRejectsProviderSubstitutionBeforeCreatingArtifacts(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "must-not-exist")
	_, err := openMeetingReviewCLI(context.Background(), meetingReviewCLIConfig{
		Directory: directory, Provider: "google.gemini-latest",
		APIKeyEnvironment: "GEMINI_API_KEY",
	}, "", func(string) (string, bool) { return "fixture-gemini-key-at-least-sixteen", true })
	if err == nil || !strings.Contains(err.Error(), "exact google.gemini-3.7-flash") {
		t.Fatalf("provider substitution error = %v", err)
	}
	if _, statErr := os.Lstat(directory); !os.IsNotExist(statErr) {
		t.Fatalf("rejected provider created an artifact: %v", statErr)
	}
}

func TestOpenMeetingReviewCLIRejectsShortDeploymentCredentialBeforeCreatingArtifacts(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "must-not-exist")
	sourceReceipt := filepath.Join(parent, "must-not-exist.source.json")
	evaluations := filepath.Join(parent, "must-not-exist.evaluations")
	quarantine := filepath.Join(parent, "must-not-exist.quarantine")
	_, err := openMeetingReviewCLI(context.Background(), meetingReviewCLIConfig{
		Directory: directory, Provider: gemini.RegistrationName,
		APIKeyEnvironment: "MEETING_REVIEW_KEY",
		SourceReceiptPath: sourceReceipt, EvaluationReceiptDirectory: evaluations,
		EvaluationQuarantineDirectory: quarantine,
	}, "short", func(string) (string, bool) {
		return "fixture-gemini-key-at-least-sixteen", true
	})
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Fatalf("short deployment credential error = %v", err)
	}
	for _, path := range []string{directory, sourceReceipt, evaluations, quarantine} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("rejected deployment credential created %s: %v", path, statErr)
		}
	}
}

type meetingCLIReviewToken struct{ fingerprint string }

type meetingCLIReviewProvider struct {
	descriptor     review.ProviderDescriptor
	capabilities   review.ProviderCapabilities
	implementation []byte
	configuration  []byte
	reviewErr      error
	claimed        atomic.Bool
	calls          atomic.Int32
}

func (provider *meetingCLIReviewProvider) Descriptor() review.ProviderDescriptor {
	return provider.descriptor
}
func (provider *meetingCLIReviewProvider) Capabilities() review.ProviderCapabilities {
	return provider.capabilities.Clone()
}
func (provider *meetingCLIReviewProvider) Implementation() []byte {
	return slices.Clone(provider.implementation)
}
func (provider *meetingCLIReviewProvider) Configuration() []byte {
	return slices.Clone(provider.configuration)
}
func (provider *meetingCLIReviewProvider) Claim() error {
	if !provider.claimed.CompareAndSwap(false, true) {
		return errors.New("meeting CLI fixture reviewer was claimed twice")
	}
	return nil
}
func (provider *meetingCLIReviewProvider) Review(
	ctx context.Context, request review.PreparedRequest,
) (review.ProviderResponse, error) {
	if err := ctx.Err(); err != nil {
		return review.ProviderResponse{}, err
	}
	provider.calls.Add(1)
	if provider.reviewErr != nil {
		return review.ProviderResponse{}, provider.reviewErr
	}
	output, err := json.Marshal(review.Assessment{
		MediaUsable: true, ObservedOutcome: "pass", AgreesWithDeterministic: true,
		Confidence: 0.9, Summary: "Hermetic command receipt fixture.",
		SignificantProblems: []review.Finding{}, MinorObservations: []review.Finding{},
		Limitations: []string{"Hermetic fixture; no remote-service attestation."},
	})
	if err != nil {
		return review.ProviderResponse{}, err
	}
	return review.ProviderResponse{
		Raw: []byte(`{"fixture":"meeting-cli"}`), Output: output,
		ReportedModel: provider.descriptor.Model,
		RequestID:     "meeting-cli-fixture", RequestIDState: review.ProviderRequestIDValue,
		Request:              []byte(`{"fixture":"request"}`),
		VerificationEvidence: &meetingCLIReviewToken{fingerprint: request.RequestFingerprint},
	}, nil
}
func (*meetingCLIReviewProvider) VerifyResponse(
	ctx context.Context, request review.PreparedRequest, response review.ProviderResponse,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, ok := response.VerificationEvidence.(*meetingCLIReviewToken)
	if !ok || token == nil || token.fingerprint != request.RequestFingerprint {
		return errors.New("meeting CLI fixture review verification drifted")
	}
	return nil
}
func (*meetingCLIReviewProvider) Close() error { return nil }

func TestOpenMeetingReviewCLIResumesSealedSourceWithExactGemini(t *testing.T) {
	directory, sourceReceiptPath, evaluationReceiptDirectory,
		evaluationQuarantineDirectory, result := interruptedMeetingCLIReviewCampaign(t)
	resources, err := openMeetingReviewCLI(context.Background(), meetingReviewCLIConfig{
		Directory: directory, Resume: true, Provider: gemini.RegistrationName,
		APIKeyEnvironment:             "MEETING_REVIEW_KEY",
		SourceReceiptPath:             sourceReceiptPath,
		EvaluationReceiptDirectory:    evaluationReceiptDirectory,
		EvaluationQuarantineDirectory: evaluationQuarantineDirectory,
	}, "", func(name string) (string, bool) {
		if name != "MEETING_REVIEW_KEY" {
			return "", false
		}
		return "fixture-gemini-key-at-least-sixteen", true
	})
	if err != nil {
		t.Fatalf("open production Meeting resume: %v", err)
	}
	defer resources.close()
	if resources.bundle == nil || resources.lease == nil || resources.resumedResult == nil ||
		resources.reviewer != gemini.Descriptor() ||
		!meetingCLIResultsEqual(*resources.resumedResult, result) {
		t.Fatalf("production Meeting resume composition = %+v", resources)
	}
	if resources.encoder.Name != "" || resources.attestor.Capability != "" {
		t.Fatalf("resume unnecessarily opened recorder plug-ins: %+v / %+v",
			resources.encoder, resources.attestor)
	}
}

func TestRunMeetingReviewResumeSkipsDeterministicRunner(t *testing.T) {
	directory, sourceReceiptPath, evaluationReceiptDirectory,
		evaluationQuarantineDirectory, result := interruptedMeetingCLIReviewCampaign(t)
	lease, _ := openMeetingCLIReviewFixtureLease(t, nil)
	bundle, recovered, err := meeting.ResumeReviewBundle(context.Background(), meeting.ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
		SourceReceiptPath:             sourceReceiptPath,
		EvaluationReceiptDirectory:    evaluationReceiptDirectory,
		EvaluationQuarantineDirectory: evaluationQuarantineDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	resources := &meetingReviewCLIResources{
		bundle: bundle, resumedResult: &recovered, lease: lease,
		sourceReceiptPath:             sourceReceiptPath,
		evaluationReceiptDirectory:    evaluationReceiptDirectory,
		evaluationQuarantineDirectory: evaluationQuarantineDirectory,
	}
	var runnerCalls atomic.Int32
	dependencies := meetingCommandDependencies{
		openReview: func(
			ctx context.Context, config meetingReviewCLIConfig, _ string,
			_ func(string) (string, bool),
		) (*meetingReviewCLIResources, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if !config.Resume || config.Directory != directory ||
				config.SourceReceiptPath != sourceReceiptPath ||
				config.EvaluationReceiptDirectory != evaluationReceiptDirectory ||
				config.EvaluationQuarantineDirectory != evaluationQuarantineDirectory {
				t.Fatalf("parsed Meeting resume config = %+v", config)
			}
			return resources, nil
		},
		run: func(context.Context, meeting.Options) (bench.Result, error) {
			runnerCalls.Add(1)
			return bench.Result{}, errors.New("deterministic runner must not execute during review resume")
		},
	}
	var output bytes.Buffer
	if err := runMeetingWithDependencies([]string{
		"-review-dir", directory, "-review-resume",
		"-review-source-receipt", sourceReceiptPath,
		"-review-evaluation-receipts", evaluationReceiptDirectory,
		"-review-evaluation-quarantine", evaluationQuarantineDirectory,
	}, &output, dependencies); err != nil {
		t.Fatalf("run Meeting review resume: %v", err)
	}
	if runnerCalls.Load() != 0 {
		t.Fatalf("deterministic runner calls during resume = %d", runnerCalls.Load())
	}
	if !meetingCLIResultsEqual(recovered, result) ||
		!strings.Contains(output.String(), "meeting review bundle sealed") {
		t.Fatalf("Meeting review resume output/result drifted: %q", output.String())
	}
}

func TestMeetingReviewCLIResumeAcrossRealProcessBoundary(t *testing.T) {
	parent := t.TempDir()
	t.Cleanup(func() { makeMeetingCLIReviewTreeWritable(parent) })
	for _, mode := range []string{"produce", "resume"} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := exec.CommandContext(
			ctx, os.Args[0], "-test.run=^TestMeetingReviewCLIResumeHelperProcess$", "-test.v",
		)
		command.Env = append(os.Environ(),
			"OPENREALTIME_MEETING_RESUME_HELPER_MODE="+mode,
			"OPENREALTIME_MEETING_RESUME_HELPER_ROOT="+parent,
		)
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("Meeting review %s helper process: %v\n%s", mode, err, output)
		}
	}
	status, err := os.ReadFile(filepath.Join(parent, "resume-status.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(status) != "deterministic_runner_calls=0\nreviewer_calls=4\n" {
		t.Fatalf("fresh process status = %q", status)
	}
	manifestPath := filepath.Join(parent, "review", "manifest.json")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := meeting.VerifyReviewBundle(
		filepath.Join(parent, "review"), meetingCLIReviewDigest(manifest),
	); err != nil {
		t.Fatalf("verify real-process resumed Meeting review bundle: %v", err)
	}
}

func TestMeetingReviewCLIResumeHelperProcess(t *testing.T) {
	mode := os.Getenv("OPENREALTIME_MEETING_RESUME_HELPER_MODE")
	parent := os.Getenv("OPENREALTIME_MEETING_RESUME_HELPER_ROOT")
	if mode == "" || parent == "" {
		t.Skip("helper process only")
	}
	directory := filepath.Join(parent, "review")
	sourceReceiptPath := filepath.Join(parent, "source-receipt.json")
	evaluationReceiptDirectory := filepath.Join(parent, "evaluation-receipts")
	evaluationQuarantineDirectory := filepath.Join(parent, "evaluation-quarantine")
	switch mode {
	case "produce":
		lease, _ := openMeetingCLIReviewFixtureLease(
			t, errors.New("fixture provider unavailable after deterministic source publication"),
		)
		bundle, err := meeting.NewReviewBundle(meeting.ReviewBundleOptions{
			Directory: directory, Reviewer: lease,
			SourceReceiptPath:             sourceReceiptPath,
			EvaluationReceiptDirectory:    evaluationReceiptDirectory,
			EvaluationQuarantineDirectory: evaluationQuarantineDirectory,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runMeetingCLIReviewFixture(context.Background(), meeting.Options{
			Cell: meeting.ReferenceCell(), Evidence: bundle,
		}); err == nil {
			t.Fatal("producer unexpectedly committed advisory reviews")
		}
		if err := bundle.Close(); err != nil {
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	case "resume":
		lease, provider := openMeetingCLIReviewFixtureLease(t, nil)
		bundle, result, err := meeting.ResumeReviewBundle(context.Background(), meeting.ReviewBundleOptions{
			Directory: directory, Reviewer: lease,
			SourceReceiptPath:             sourceReceiptPath,
			EvaluationReceiptDirectory:    evaluationReceiptDirectory,
			EvaluationQuarantineDirectory: evaluationQuarantineDirectory,
		})
		if err != nil {
			t.Fatal(err)
		}
		resources := &meetingReviewCLIResources{
			bundle: bundle, resumedResult: &result, lease: lease,
			sourceReceiptPath:             sourceReceiptPath,
			evaluationReceiptDirectory:    evaluationReceiptDirectory,
			evaluationQuarantineDirectory: evaluationQuarantineDirectory,
		}
		var runnerCalls atomic.Int32
		var output bytes.Buffer
		err = runMeetingWithDependencies([]string{
			"-review-dir", directory, "-review-resume",
			"-review-source-receipt", sourceReceiptPath,
			"-review-evaluation-receipts", evaluationReceiptDirectory,
			"-review-evaluation-quarantine", evaluationQuarantineDirectory,
		}, &output, meetingCommandDependencies{
			openReview: func(
				context.Context, meetingReviewCLIConfig, string, func(string) (string, bool),
			) (*meetingReviewCLIResources, error) {
				return resources, nil
			},
			run: func(context.Context, meeting.Options) (bench.Result, error) {
				runnerCalls.Add(1)
				return bench.Result{}, errors.New("deterministic runner executed in helper resume")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		status := fmt.Sprintf("deterministic_runner_calls=%d\nreviewer_calls=%d\n",
			runnerCalls.Load(), provider.calls.Load())
		if err := os.WriteFile(filepath.Join(parent, "resume-status.txt"), []byte(status), 0o600); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func interruptedMeetingCLIReviewCampaign(
	t testing.TB,
) (directory, sourceReceiptPath, evaluationReceiptDirectory,
	evaluationQuarantineDirectory string, result bench.Result) {
	t.Helper()
	parent := t.TempDir()
	directory = filepath.Join(parent, "interrupted-review")
	sourceReceiptPath = filepath.Join(parent, "source-receipt.json")
	evaluationReceiptDirectory = filepath.Join(parent, "evaluation-receipts")
	evaluationQuarantineDirectory = filepath.Join(parent, "evaluation-quarantine")
	lease, _ := openMeetingCLIReviewFixtureLease(
		t, errors.New("fixture reviewer unavailable after deterministic source publication"),
	)
	bundle, err := meeting.NewReviewBundle(meeting.ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
		SourceReceiptPath:             sourceReceiptPath,
		EvaluationReceiptDirectory:    evaluationReceiptDirectory,
		EvaluationQuarantineDirectory: evaluationQuarantineDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { makeMeetingCLIReviewTreeWritable(directory) })
	result, err = runMeetingCLIReviewFixture(context.Background(), meeting.Options{
		Cell: meeting.ReferenceCell(), Evidence: bundle,
	})
	if err == nil {
		t.Fatal("fixture interruption unexpectedly committed the review campaign")
	}
	if closeErr := bundle.Close(); closeErr != nil {
		t.Fatalf("close interrupted Meeting bundle: %v", closeErr)
	}
	if closeErr := lease.Close(); closeErr != nil {
		t.Fatalf("close interrupted Meeting reviewer: %v", closeErr)
	}
	if _, err := meeting.ReadReviewSourceReceipt(context.Background(), sourceReceiptPath); err != nil {
		t.Fatalf("interrupted campaign has no durable source receipt: %v", err)
	}
	return directory, sourceReceiptPath, evaluationReceiptDirectory,
		evaluationQuarantineDirectory, result
}

func openMeetingCLIReviewFixtureLease(
	t testing.TB, reviewErr error,
) (*review.ProviderLease, *meetingCLIReviewProvider) {
	t.Helper()
	implementation := []byte("meeting CLI resume provider fixture implementation v1")
	configuration := []byte(`{"fixture":"meeting-cli-resume"}`)
	capabilities := review.ProviderCapabilities{
		MediaTypes: []string{"audio/wav"}, MaximumMediaCount: 1, MaximumMediaBytes: 8 << 20,
	}
	capabilitiesSHA256, err := capabilities.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := review.ProviderDescriptor{
		Provider: "fixture", Model: "meeting-cli-resume-v1", API: "fixture.review", APIRevision: "v1",
		Implementation: review.ContentIdentity{
			Version: "fixture.meeting-cli-resume.v1", SHA256: meetingCLIReviewDigest(implementation),
		},
		ConfigurationSHA256: meetingCLIReviewDigest(configuration), CapabilitiesSHA256: capabilitiesSHA256,
	}
	provider := &meetingCLIReviewProvider{
		descriptor: descriptor, capabilities: capabilities,
		implementation: implementation, configuration: configuration, reviewErr: reviewErr,
	}
	registry, err := review.NewRegistry([]review.Registration{{
		Name: "fixture.meeting-cli-resume", Descriptor: descriptor, Capabilities: capabilities,
		Implementation: implementation, Configuration: configuration,
		Factory: func(context.Context) (review.Provider, error) { return provider, nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(context.Background(), "fixture.meeting-cli-resume")
	if err != nil {
		t.Fatal(err)
	}
	return lease, provider
}

func TestMeetingReviewCLIReceiptPublicationEndToEnd(t *testing.T) {
	parent := t.TempDir()
	reviewDirectory := filepath.Join(parent, "review")
	sourceReceiptPath := filepath.Join(parent, "source-receipt.json")
	evaluationReceiptDirectory := filepath.Join(parent, "evaluation-receipts")
	evaluationQuarantineDirectory := filepath.Join(parent, "evaluation-quarantine")
	implementation := []byte("meeting CLI review provider fixture implementation v1")
	configuration := []byte(`{"fixture":"meeting-cli"}`)
	capabilities := review.ProviderCapabilities{
		MediaTypes: []string{"audio/wav"}, MaximumMediaCount: 1, MaximumMediaBytes: 8 << 20,
	}
	capabilitiesSHA256, err := capabilities.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := review.ProviderDescriptor{
		Provider: "fixture", Model: "meeting-cli-review-v1", API: "fixture.review", APIRevision: "v1",
		Implementation: review.ContentIdentity{
			Version: "fixture.meeting-cli-review.v1", SHA256: meetingCLIReviewDigest(implementation),
		},
		ConfigurationSHA256: meetingCLIReviewDigest(configuration), CapabilitiesSHA256: capabilitiesSHA256,
	}
	provider := &meetingCLIReviewProvider{
		descriptor: descriptor, capabilities: capabilities,
		implementation: implementation, configuration: configuration,
	}
	registry, err := review.NewRegistry([]review.Registration{{
		Name: "fixture.meeting-cli", Descriptor: descriptor, Capabilities: capabilities,
		Implementation: implementation, Configuration: configuration,
		Factory: func(context.Context) (review.Provider, error) { return provider, nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(context.Background(), "fixture.meeting-cli")
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := meeting.NewReviewBundle(meeting.ReviewBundleOptions{
		Directory: reviewDirectory, Reviewer: lease,
		SourceReceiptPath:             sourceReceiptPath,
		EvaluationReceiptDirectory:    evaluationReceiptDirectory,
		EvaluationQuarantineDirectory: evaluationQuarantineDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	resources := &meetingReviewCLIResources{
		bundle: bundle, lease: lease, sourceReceiptPath: sourceReceiptPath,
		evaluationReceiptDirectory:    evaluationReceiptDirectory,
		evaluationQuarantineDirectory: evaluationQuarantineDirectory,
	}
	t.Cleanup(func() { makeMeetingCLIReviewTreeWritable(reviewDirectory) })
	dependencies := meetingCommandDependencies{
		openReview: func(
			ctx context.Context, config meetingReviewCLIConfig, _ string,
			_ func(string) (string, bool),
		) (*meetingReviewCLIResources, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if config.Directory != reviewDirectory || config.Provider != gemini.RegistrationName ||
				config.SourceReceiptPath != sourceReceiptPath ||
				config.EvaluationReceiptDirectory != evaluationReceiptDirectory ||
				config.EvaluationQuarantineDirectory != evaluationQuarantineDirectory {
				t.Fatalf("parsed Meeting review config = %+v", config)
			}
			return resources, nil
		},
		run: func(ctx context.Context, options meeting.Options) (bench.Result, error) {
			return runMeetingCLIReviewFixture(ctx, options)
		},
	}
	var output bytes.Buffer
	if err := runMeetingWithDependencies([]string{
		"-review-dir", reviewDirectory,
		"-review-provider", gemini.RegistrationName,
		"-review-key-env", "HERMETIC_MEETING_REVIEW_KEY",
		"-review-source-receipt", sourceReceiptPath,
		"-review-evaluation-receipts", evaluationReceiptDirectory,
		"-review-evaluation-quarantine", evaluationQuarantineDirectory,
	}, &output, dependencies); err != nil {
		t.Fatal(err)
	}
	externalSource, err := meeting.ReadReviewSourceReceipt(context.Background(), sourceReceiptPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := meeting.VerifyMeetingReviewSource(
		context.Background(), externalSource.Directory, externalSource,
	); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(evaluationReceiptDirectory)
	if err != nil {
		t.Fatal(err)
	}
	receiptCount := 0
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".receipt.json") {
			continue
		}
		receiptCount++
		if _, err := review.ReadEvaluationBundleReceipt(
			context.Background(), filepath.Join(evaluationReceiptDirectory, entry.Name()),
		); err != nil {
			t.Fatalf("read external evaluation receipt %s: %v", entry.Name(), err)
		}
	}
	if receiptCount != meeting.ExpectedTasks() {
		t.Fatalf("external evaluation receipts = %d, want %d", receiptCount, meeting.ExpectedTasks())
	}
	if !strings.Contains(output.String(), "meeting source receipt retained") ||
		!strings.Contains(output.String(), "meeting review bundle sealed") {
		t.Fatalf("Meeting review CLI output = %q", output.String())
	}
}

func runMeetingCLIReviewFixture(
	ctx context.Context, options meeting.Options,
) (bench.Result, error) {
	result := bench.Result{
		Suite: meeting.SuiteName, Cell: options.Cell, Expected: meeting.ExpectedTasks(),
	}
	if options.Evidence == nil {
		return result, errors.New("Meeting command did not compose its review evidence plug-in")
	}
	for index, task := range meeting.Suite() {
		specification := meeting.EvidenceAttempt{
			Suite: meeting.SuiteName, Case: task.ID, Trial: 1, Task: task,
			Cell: result.Cell, Provenance: result.Provenance,
			Origin: meeting.EvidenceRunOrigin{
				Kind: meeting.EvidenceOriginHermetic, Live: false, Transport: bench.TransportWebSocket,
				EndpointSHA256: meetingCLIReviewDigest([]byte("ws://hermetic.invalid/v1/realtime")),
			},
			ExecutionRequirement: result.Cell.Execution,
		}
		attempt, err := options.Evidence.BeginAttempt(ctx, specification)
		if err != nil {
			return result, err
		}
		audio := make([]int16, 240+index)
		audio[0] = int16(100 + index)
		if err := attempt.CaptureAudio(bench.SessionAudioCapture{
			SampleRateHz: 24_000, RoomPCM16: audio,
		}); err != nil {
			return result, err
		}
		outcome := bench.TaskOutcome{ID: task.ID, Completed: true, Passed: true}
		if err := attempt.Complete(ctx, meeting.EvidenceCompletion{
			Attempt: specification, Outcome: outcome,
		}); err != nil {
			return result, err
		}
		result.Tasks = append(result.Tasks, outcome)
	}
	result.Finish()
	if err := options.Evidence.FinishSuite(ctx, result); err != nil {
		return result, err
	}
	return result, nil
}

func meetingCLIReviewDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func meetingCLIResultsEqual(left, right bench.Result) bool {
	leftPayload, leftErr := json.Marshal(left)
	rightPayload, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftPayload, rightPayload)
}

func makeMeetingCLIReviewTreeWritable(directory string) {
	_ = filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}
