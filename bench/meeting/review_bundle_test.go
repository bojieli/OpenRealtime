package meeting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	revieweval "github.com/bojieli/OpenRealtime/bench/review"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	reviewffmpeg "github.com/bojieli/OpenRealtime/bench/review/media/ffmpeg"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

type fixtureMeetingReviewer struct {
	mu              sync.Mutex
	requests        []string
	tokens          map[*fixtureMeetingReviewToken]string
	failCase        string
	failErr         error
	mutate          bool
	claimed         atomic.Bool
	closed          atomic.Bool
	reviewReady     chan struct{}
	reviewReadyOnce sync.Once
	reviewBlock     <-chan struct{}
	findingAtMS     *int64
	summary         string
	onReview        func()
}

type fixtureMeetingReviewToken struct{}

type realMeetingVideoFactory struct {
	calls   atomic.Int32
	options reviewffmpeg.Options
}

type forbiddenMeetingVideoFactory struct{ calls atomic.Int32 }

type countingMeetingVideoFactory struct {
	encoder  *countingMeetingEncoder
	attestor *countingMeetingAttestor
	err      error
}

type countingMeetingEncoder struct {
	validDescriptor bool
	claimErr        error
	claims          atomic.Int32
	closes          atomic.Int32
}

type countingMeetingAttestor struct {
	validDescriptor bool
	claimErr        error
	claims          atomic.Int32
	closes          atomic.Int32
}

var countingMeetingPluginImplementation = []byte("meeting counting video plug-in v1")
var countingMeetingPluginConfiguration = []byte(`{"mode":"lifecycle-fixture"}`)

func (factory *countingMeetingVideoFactory) NewReviewVideo(
	context.Context, EvidenceAttempt,
) (reviewmedia.Encoder, reviewmedia.Attestor, error) {
	return factory.encoder, factory.attestor, factory.err
}

func (plugin *countingMeetingEncoder) Descriptor() reviewmedia.EncoderDescriptor {
	name := "meeting-counting-encoder"
	if plugin == nil || !plugin.validDescriptor {
		name = ""
	}
	return reviewmedia.EncoderDescriptor{
		Name: name, Version: "v1",
		Implementation: revieweval.ContentIdentity{
			Version: "meeting-counting-encoder.v1",
			SHA256:  reviewDigest(countingMeetingPluginImplementation),
		},
		ConfigurationSHA256: reviewDigest(countingMeetingPluginConfiguration),
	}
}

func (*countingMeetingEncoder) Implementation() []byte {
	return slices.Clone(countingMeetingPluginImplementation)
}

func (*countingMeetingEncoder) Configuration() []byte {
	return slices.Clone(countingMeetingPluginConfiguration)
}

func (plugin *countingMeetingEncoder) Claim(context.Context) error {
	plugin.claims.Add(1)
	return plugin.claimErr
}

func (*countingMeetingEncoder) Encode(context.Context, reviewmedia.EncodeRequest) error {
	return errors.New("counting encoder must not encode in a lifecycle test")
}

func (plugin *countingMeetingEncoder) Close() error {
	plugin.closes.Add(1)
	return nil
}

func (plugin *countingMeetingAttestor) Descriptor() reviewmedia.AttestorDescriptor {
	name := "meeting-counting-attestor"
	if plugin == nil || !plugin.validDescriptor {
		name = ""
	}
	return reviewmedia.AttestorDescriptor{
		Name: name, Version: "v1", Capability: reviewmedia.FullDecodeAttestationCapability,
		Implementation: revieweval.ContentIdentity{
			Version: "meeting-counting-attestor.v1",
			SHA256:  reviewDigest(countingMeetingPluginImplementation),
		},
		ConfigurationSHA256: reviewDigest(countingMeetingPluginConfiguration),
	}
}

func (*countingMeetingAttestor) Implementation() []byte {
	return slices.Clone(countingMeetingPluginImplementation)
}

func (*countingMeetingAttestor) Configuration() []byte {
	return slices.Clone(countingMeetingPluginConfiguration)
}

func (plugin *countingMeetingAttestor) Claim(context.Context) error {
	plugin.claims.Add(1)
	return plugin.claimErr
}

func (*countingMeetingAttestor) Attest(
	context.Context, reviewmedia.AttestationRequest,
) (reviewmedia.Attestation, error) {
	return reviewmedia.Attestation{}, errors.New("counting attestor must not attest in a lifecycle test")
}

func (plugin *countingMeetingAttestor) Close() error {
	plugin.closes.Add(1)
	return nil
}

func (factory *forbiddenMeetingVideoFactory) NewReviewVideo(
	context.Context, EvidenceAttempt,
) (reviewmedia.Encoder, reviewmedia.Attestor, error) {
	factory.calls.Add(1)
	return nil, nil, errors.New("foreign task reached the video factory")
}

func (factory *realMeetingVideoFactory) NewReviewVideo(
	ctx context.Context, _ EvidenceAttempt,
) (reviewmedia.Encoder, reviewmedia.Attestor, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	factory.calls.Add(1)
	encoder, err := reviewffmpeg.NewEncoder(ctx, factory.options)
	if err != nil {
		return nil, nil, err
	}
	attestor, err := reviewffmpeg.NewAttestor(ctx, factory.options)
	if err != nil {
		_ = encoder.Close()
		return nil, nil, err
	}
	return encoder, attestor, nil
}

var fixtureMeetingReviewerImplementation = []byte("openrealtime meeting reviewer fixture v1")
var fixtureMeetingReviewerConfiguration = []byte(`{"mode":"hermetic"}`)

func fixtureMeetingReviewerCapabilities() revieweval.ProviderCapabilities {
	return revieweval.ProviderCapabilities{
		MediaTypes:        []string{"audio/wav", "video/mp4"},
		MaximumMediaCount: 8, MaximumMediaBytes: 128 << 20,
	}
}

func fixtureMeetingReviewerDescriptor() revieweval.ProviderDescriptor {
	capabilitiesSHA, err := fixtureMeetingReviewerCapabilities().SHA256()
	if err != nil {
		panic(err)
	}
	return revieweval.ProviderDescriptor{
		Provider: "fixture", Model: "meeting-review-fixture", API: "fixture.review",
		APIRevision: "v1",
		Implementation: revieweval.ContentIdentity{
			Version: "fixture.meeting-review.v1", SHA256: reviewDigest(fixtureMeetingReviewerImplementation),
		},
		ConfigurationSHA256: reviewDigest(fixtureMeetingReviewerConfiguration),
		CapabilitiesSHA256:  capabilitiesSHA,
	}
}

func (reviewer *fixtureMeetingReviewer) Descriptor() revieweval.ProviderDescriptor {
	return fixtureMeetingReviewerDescriptor()
}

func (reviewer *fixtureMeetingReviewer) Capabilities() revieweval.ProviderCapabilities {
	return fixtureMeetingReviewerCapabilities()
}

func (*fixtureMeetingReviewer) Implementation() []byte {
	return slices.Clone(fixtureMeetingReviewerImplementation)
}

func (*fixtureMeetingReviewer) Configuration() []byte {
	return slices.Clone(fixtureMeetingReviewerConfiguration)
}

func (reviewer *fixtureMeetingReviewer) Claim() error {
	if !reviewer.claimed.CompareAndSwap(false, true) {
		return errors.New("fixture meeting reviewer was already claimed")
	}
	return nil
}

func (reviewer *fixtureMeetingReviewer) Review(
	ctx context.Context, request revieweval.PreparedRequest,
) (revieweval.ProviderResponse, error) {
	reviewer.mu.Lock()
	reviewer.requests = append(reviewer.requests, request.Case)
	failCase, failErr, onReview := reviewer.failCase, reviewer.failErr, reviewer.onReview
	reviewer.mu.Unlock()
	if onReview != nil {
		onReview()
	}
	if request.Case == failCase {
		return revieweval.ProviderResponse{}, failErr
	}
	if request.Suite != SuiteName || request.Trial != 1 || len(request.Media) == 0 ||
		request.Media[0].Kind != "audio" || request.Media[0].MediaType != "audio/wav" {
		return revieweval.ProviderResponse{}, errors.New("fixture received a drifted review request")
	}
	payload := request.Media[0].Bytes
	if len(payload) < 44 || !bytes.Equal(payload[:4], []byte("RIFF")) {
		return revieweval.ProviderResponse{}, errors.New("fixture could not play the retained WAV")
	}
	var deterministic meetingReviewContext
	if err := json.Unmarshal(request.Context, &deterministic); err != nil {
		return revieweval.ProviderResponse{}, err
	}
	observed := "fail"
	if deterministic.Outcome.Passed {
		observed = "pass"
	}
	significantProblems := "[]"
	if reviewer.findingAtMS != nil {
		significantProblems = fmt.Sprintf(
			`[{"category":"timing","start_ms":%d,"evidence":"Observed outside the retained timeline.","impact":"The timestamp is not reviewable."}]`,
			*reviewer.findingAtMS,
		)
	}
	summary := reviewer.summary
	if summary == "" {
		summary = "The retained stereo audio and deterministic trace are internally consistent."
	}
	output := json.RawMessage(fmt.Sprintf(`{
		"media_usable":true,
		"observed_outcome":%q,
		"agrees_with_deterministic":true,
		"confidence":0.9,
		"summary":%q,
		"significant_problems":%s,
		"minor_observations":[],
		"limitations":[]
	}`, observed, summary, significantProblems))
	token := &fixtureMeetingReviewToken{}
	reviewer.mu.Lock()
	if reviewer.tokens == nil {
		reviewer.tokens = make(map[*fixtureMeetingReviewToken]string)
	}
	reviewer.tokens[token] = request.RequestFingerprint
	reviewer.mu.Unlock()
	if reviewer.reviewReady != nil {
		reviewer.reviewReadyOnce.Do(func() { close(reviewer.reviewReady) })
	}
	if reviewer.reviewBlock != nil {
		select {
		case <-reviewer.reviewBlock:
		case <-ctx.Done():
			return revieweval.ProviderResponse{}, context.Cause(ctx)
		}
	}
	if reviewer.mutate {
		request.Context[0] = 'x'
		request.Media[0].Path = "mutated.wav"
		request.Media[0].Bytes[0] = 'x'
	}
	return revieweval.ProviderResponse{
		Raw: []byte(`{"fixture":"meeting-review"}`), Output: output,
		ReportedModel: fixtureMeetingReviewerDescriptor().Model,
		RequestID:     "fixture-" + request.Case, RequestIDState: revieweval.ProviderRequestIDValue,
		Request: []byte(`{"store":false}`), VerificationEvidence: token,
	}, nil
}

func (reviewer *fixtureMeetingReviewer) VerifyResponse(
	ctx context.Context, request revieweval.PreparedRequest, response revieweval.ProviderResponse,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, ok := response.VerificationEvidence.(*fixtureMeetingReviewToken)
	if !ok || token == nil {
		return errors.New("fixture meeting review has no private verification token")
	}
	reviewer.mu.Lock()
	want, exists := reviewer.tokens[token]
	delete(reviewer.tokens, token)
	reviewer.mu.Unlock()
	if !exists || want != request.RequestFingerprint {
		return errors.New("fixture meeting review verification token was replayed or drifted")
	}
	return nil
}

func (reviewer *fixtureMeetingReviewer) Close() error {
	if !reviewer.closed.CompareAndSwap(false, true) {
		return errors.New("fixture meeting reviewer was closed twice")
	}
	return nil
}

func openFixtureMeetingReviewer(
	t testing.TB, reviewer *fixtureMeetingReviewer,
) *revieweval.ProviderLease {
	t.Helper()
	lease := newFixtureMeetingReviewerLease(t, reviewer)
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close fixture meeting reviewer: %v", err)
		}
	})
	return lease
}

func newFixtureMeetingReviewerLease(
	t testing.TB, reviewer *fixtureMeetingReviewer,
) *revieweval.ProviderLease {
	t.Helper()
	registration := revieweval.Registration{
		Name: "meeting-fixture", Descriptor: fixtureMeetingReviewerDescriptor(),
		Capabilities:   fixtureMeetingReviewerCapabilities(),
		Implementation: slices.Clone(fixtureMeetingReviewerImplementation),
		Configuration:  slices.Clone(fixtureMeetingReviewerConfiguration),
		Factory:        func(context.Context) (revieweval.Provider, error) { return reviewer, nil },
	}
	registry, err := revieweval.NewRegistry([]revieweval.Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(context.Background(), "meeting-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func completeFixtureMeetingReviewAttempts(
	t testing.TB, bundle *ReviewBundle,
) bench.Result {
	t.Helper()
	result := bench.Result{Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks()}
	for index, task := range Suite() {
		specification := fixtureMeetingEvidenceAttempt(task, result)
		attempt, err := bundle.BeginAttempt(context.Background(), specification)
		if err != nil {
			t.Fatal(err)
		}
		if err := attempt.CaptureAudio(fixtureMeetingAudio(index)); err != nil {
			t.Fatal(err)
		}
		outcome := bench.TaskOutcome{
			ID: task.ID, Completed: true, Passed: true,
			Metrics: map[string]float64{"task_success_rate": 1},
			Notes:   map[string]string{"fixture": "hermetic"},
		}
		if err := attempt.Complete(context.Background(), EvidenceCompletion{
			Attempt: specification, Outcome: outcome,
		}); err != nil {
			t.Fatal(err)
		}
		result.Tasks = append(result.Tasks, outcome)
	}
	result.Finish()
	return result
}

func fixtureMeetingEvidenceAttempt(task Task, result bench.Result) EvidenceAttempt {
	return EvidenceAttempt{
		Suite: SuiteName, Case: task.ID, Trial: 1, Task: task,
		Cell: result.Cell, Provenance: result.Provenance,
		Origin: fixtureMeetingOrigin(), ExecutionRequirement: result.Cell.Execution,
	}
}

func TestMeetingReviewBundleRetainsAllFourPlayableAudioAndCaseReviews(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "meeting-review")
	reviewer := &fixtureMeetingReviewer{mutate: true}
	lease := openFixtureMeetingReviewer(t, reviewer)
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
		SensitiveValues: []string{"declared-secret-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)

	result := bench.Result{Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks()}
	for index, task := range Suite() {
		specification := fixtureMeetingEvidenceAttempt(task, result)
		evidence, err := bundle.BeginAttempt(context.Background(), specification)
		if err != nil {
			t.Fatalf("begin %s: %v", task.ID, err)
		}
		if err := evidence.CaptureAudio(fixtureMeetingAudio(index)); err != nil {
			t.Fatalf("capture %s: %v", task.ID, err)
		}
		passed := index != 1
		outcome := bench.TaskOutcome{
			ID: task.ID, Completed: true, Passed: passed,
			Metrics: map[string]float64{"task_success_rate": boolFloat(passed)},
			Notes:   map[string]string{"review_note": "The declared value was sanitized."},
		}
		transcript := bench.Transcript{Moments: []bench.Moment{
			{AtMS: 1, Kind: bench.MomentTranscript, Text: "Please continue with declared-secret-value."},
			{AtMS: 2, Kind: bench.MomentAgentText, Text: "Understood."},
		}}
		if err := evidence.Complete(context.Background(), EvidenceCompletion{
			Attempt: specification, Outcome: outcome, Transcript: transcript,
		}); err != nil {
			t.Fatalf("complete %s: %v", task.ID, err)
		}
		result.Tasks = append(result.Tasks, outcome)
	}
	result.Finish()
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatal(err)
	}

	manifest := readMeetingReviewManifest(t, directory)
	if manifest.Format != ReviewBundleFormat || manifest.FormatVersion != ReviewBundleFormatVersion ||
		!manifest.Complete || manifest.Reportable || len(manifest.Attempts) != ExpectedTasks() ||
		len(manifest.Missing) != 0 || manifest.Result.Path != "source/result.json" ||
		manifest.Provenance != result.Provenance || !reflect.DeepEqual(manifest.Cell, result.Cell) {
		t.Fatalf("meeting review manifest = %+v", manifest)
	}
	receipt, err := bundle.Receipt()
	if err != nil || receipt.Directory != directory {
		t.Fatalf("meeting review receipt = %+v, %v", receipt, err)
	}
	verified, err := VerifyReviewBundle(receipt.Directory, receipt.ManifestSHA256)
	if err != nil || !reflect.DeepEqual(verified, manifest) {
		t.Fatalf("verify meeting review bundle = %+v, %v", verified, err)
	}
	if manifest.Attempts[1].Deterministic.Passed ||
		manifest.Attempts[1].Assessment == nil ||
		manifest.Attempts[1].Assessment.ObservedOutcome != "fail" {
		t.Fatalf("failed deterministic case was not preserved: %+v", manifest.Attempts[1])
	}
	for _, attempt := range manifest.Attempts {
		if attempt.ReviewStatus != "complete" || attempt.SecondaryReview == nil ||
			attempt.EvaluationManifest == nil || attempt.EvaluationReceipt == nil ||
			len(attempt.ReviewArtifacts) < 12 || len(attempt.MediaArtifacts) < 2 ||
			attempt.VideoStatus != "audio-only-no-video-factory" ||
			len(attempt.Media) != 1 || attempt.Media[0].Kind != "audio" ||
			attempt.Media[0].MediaType != "audio/wav" || attempt.Media[0].SizeBytes <= 44 {
			t.Fatalf("review attempt = %+v", attempt)
		}
		payload, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(attempt.Media[0].Path)))
		if err != nil || !bytes.Equal(payload[:4], []byte("RIFF")) {
			t.Fatalf("retained audio %s is not playable: %v", attempt.Media[0].Path, err)
		}
		contextPayload, err := os.ReadFile(filepath.Join(directory, filepath.FromSlash(attempt.Context.Path)))
		if err != nil || bytes.Contains(contextPayload, []byte("declared-secret-value")) ||
			!bytes.Contains(contextPayload, []byte("[REDACTED]")) {
			t.Fatalf("context %s redaction/read = %v", attempt.Context.Path, err)
		}
	}
	if manifest.ReviewEvidenceScope == "" || manifest.IndependentRemoteAttestation ||
		manifest.ReviewAuthenticityCaveat == "" {
		t.Fatalf("meeting review provenance caveat = %+v", manifest)
	}
	reviewer.mu.Lock()
	requests := append([]string(nil), reviewer.requests...)
	reviewer.mu.Unlock()
	if len(requests) != ExpectedTasks() {
		t.Fatalf("reviewer calls = %d, want %d", len(requests), ExpectedTasks())
	}
	report, err := os.ReadFile(filepath.Join(directory, "REVIEW.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range Suite() {
		if !bytes.Contains(report, []byte(task.ID)) {
			t.Errorf("REVIEW.md omits %s", task.ID)
		}
	}
	if !bytes.Contains(report, []byte("— PASS")) || !bytes.Contains(report, []byte("— FAIL")) {
		t.Fatalf("REVIEW.md omits case-level pass/fail states:\n%s", report)
	}
}

func TestMeetingReviewBundleRealFFmpegFullDecodeIntegration(t *testing.T) {
	if os.Getenv("OPENREALTIME_RUN_FFMPEG_INTEGRATION") != "1" {
		t.Skip("set OPENREALTIME_RUN_FFMPEG_INTEGRATION=1 to run the real FFmpeg/bubblewrap full-decode gate")
	}
	directory := filepath.Join(t.TempDir(), "video-review")
	factory := &realMeetingVideoFactory{}
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease, VideoFactory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	task := Suite()[0]
	result := bench.Result{Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks()}
	specification := fixtureMeetingEvidenceAttempt(task, result)
	attempt, err := bundle.BeginAttempt(context.Background(), specification)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureAudio(bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: make([]int16, 240),
	}); err != nil {
		t.Fatal(err)
	}
	frame := image.NewRGBA(image.Rect(0, 0, 2, 2))
	frame.Set(0, 0, color.RGBA{R: 0x44, G: 0x88, B: 0xcc, A: 0xff})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, frame); err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureVideo(bench.SessionVideoCapture{
		Source: "screen", Width: 2, Height: 2, MediaType: "image/png",
		WireTimestamp: 1, EpisodeAtMS: 100, Data: encoded.Bytes(),
	}); err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{ID: task.ID, Completed: true, Passed: true}
	if err := attempt.Complete(context.Background(), EvidenceCompletion{
		Attempt: specification, Outcome: outcome,
	}); err != nil {
		t.Fatal(err)
	}
	result.Tasks = []bench.TaskOutcome{outcome}
	result.Finish()
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("partial video review suite unexpectedly committed as complete")
	}
	manifest := readMeetingReviewManifest(t, directory)
	if factory.calls.Load() != 1 || len(manifest.Attempts) != 1 ||
		manifest.Attempts[0].ReviewStatus != "complete" || len(manifest.Attempts[0].Media) != 2 ||
		manifest.Attempts[0].VideoStatus != "retained-playable-video" ||
		manifest.Attempts[0].Media[1].Kind != "video" ||
		manifest.Attempts[0].Media[1].MediaType != "video/mp4" {
		t.Fatalf("caller-supplied video review attempt = %+v", manifest.Attempts)
	}
	videoPath := filepath.Join(directory, filepath.FromSlash(manifest.Attempts[0].Media[1].Path))
	payload, err := os.ReadFile(videoPath)
	if err != nil || !bytes.Contains(payload, []byte("ftyp")) || !bytes.Contains(payload, []byte("moov")) {
		t.Fatalf("retained playable video = %d bytes, %v", len(payload), err)
	}
	mediaDirectory := filepath.Dir(videoPath)
	verified, err := reviewmedia.VerifyBundle(mediaDirectory, manifest.Attempts[0].MediaManifest.SHA256)
	if err != nil || verified.AttemptEndUS != 101_000 || verified.Attestor == nil ||
		verified.Attestor.Descriptor.Capability != reviewmedia.FullDecodeAttestationCapability {
		t.Fatalf("real full-decode receipt = %+v, %v", verified, err)
	}
	wav, err := os.ReadFile(filepath.Join(mediaDirectory, "audio.stereo.wav"))
	if err != nil || len(wav) != 44+2424*4 {
		t.Fatalf("post-playback quiet WAV bytes = %d, %v", len(wav), err)
	}
	for offset := 44 + 240*4; offset < len(wav); offset++ {
		if wav[offset] != 0 {
			t.Fatalf("post-playback quiet contains a nonzero byte at %d", offset)
		}
	}
}

func TestMeetingRunHermeticGraphNativeAllFourWithReviewBundle(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "graph-native-run")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	requirement, graph := fixtureMeetingGraphRequirement(t)
	harness := newHermeticMeetingHarness(t, graph)
	cell := ReferenceCell()
	cell.Execution = requirement
	result, err := Run(context.Background(), Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Cell: cell,
		AnalysisDelay: 10 * time.Millisecond, Timeout: time.Second,
		Evidence: bundle, dependencies: harness.dependencies(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Summary.Complete || result.Summary.Passed != ExpectedTasks() ||
		len(result.Tasks) != ExpectedTasks() {
		t.Fatalf("hermetic four-case result = %+v", result.Summary)
	}
	for _, outcome := range result.Tasks {
		if !outcome.Passed || outcome.Execution == nil ||
			outcome.Execution.Kind != bench.ExecutionGraphNative || outcome.Execution.Scope != outcome.ID {
			t.Fatalf("graph-native outcome = %+v", outcome)
		}
	}
	manifest := readMeetingReviewManifest(t, directory)
	coreErr := result.Reportable()
	if !manifest.Complete || manifest.Reportable ||
		manifest.CoreReportable != (coreErr == nil) ||
		len(manifest.Attempts) != ExpectedTasks() {
		t.Fatalf("graph-native review manifest = %+v", manifest)
	}
	if coreErr == nil && manifest.CoreReportability != "" ||
		coreErr != nil && manifest.CoreReportability != coreErr.Error() {
		t.Fatalf("graph-native core reportability = %q, want %v",
			manifest.CoreReportability, coreErr)
	}
	for _, attempt := range manifest.Attempts {
		if attempt.ExecutionStatus != "graph-native-attested" || attempt.Reportable || attempt.RunOrigin.Live ||
			attempt.Assessment == nil || attempt.ReviewStatus != "complete" {
			t.Fatalf("graph-native review attempt = %+v", attempt)
		}
	}
	receipts := bundle.EvaluationReceipts()
	if len(receipts) != len(manifest.Attempts) {
		t.Fatalf("graph-native evaluation receipts = %d, want %d",
			len(receipts), len(manifest.Attempts))
	}
	for index, attempt := range manifest.Attempts {
		mediaDirectory := filepath.Join(directory, filepath.FromSlash(filepath.Dir(attempt.MediaManifest.Path)))
		mediaManifest, err := reviewmedia.VerifyBundle(mediaDirectory, attempt.MediaManifest.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		maximumMS, err := meetingFindingTimestampMaximumMS(mediaManifest.AttemptEndUS)
		if err != nil {
			t.Fatal(err)
		}
		opened, err := revieweval.VerifyEvaluationBundle(
			context.Background(), revieweval.EvaluationBundleOptions{Directory: receipts[index].Directory},
			receipts[index],
		)
		if err != nil || opened.Record.FindingTimestampMaximumMS != maximumMS {
			t.Fatalf("graph-native review timeline case=%s maximum=%d want=%d error=%v",
				attempt.Case, opened.Record.FindingTimestampMaximumMS, maximumMS, err)
		}
	}
}

func TestMeetingRunHermeticGraphNativeAllFourRealFFmpegReviewEndToEnd(t *testing.T) {
	if os.Getenv("OPENREALTIME_RUN_MEETING_REVIEW_E2E") != "1" {
		t.Skip("set OPENREALTIME_RUN_MEETING_REVIEW_E2E=1 to run all four graph-native cases through real FFmpeg review")
	}
	parent := t.TempDir()
	directory := filepath.Join(parent, "graph-native-real-media")
	sourceReceiptPath := filepath.Join(parent, "source-receipt.json")
	evaluationReceiptDirectory := filepath.Join(parent, "evaluation-receipts")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	factory := &realMeetingVideoFactory{}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease, VideoFactory: factory,
		SourceReceiptPath:          sourceReceiptPath,
		EvaluationReceiptDirectory: evaluationReceiptDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	requirement, graph := fixtureMeetingGraphRequirement(t)
	harness := newHermeticMeetingHarness(t, graph)
	harness.failCase = "spoken-navigation-correction"
	cell := ReferenceCell()
	cell.Execution = requirement
	result, err := Run(context.Background(), Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Cell: cell,
		AnalysisDelay: 10 * time.Millisecond, Timeout: 2 * time.Second,
		Evidence: bundle, dependencies: harness.dependencies(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Summary.Complete || result.Summary.Passed != ExpectedTasks()-1 ||
		len(result.Tasks) != ExpectedTasks() || factory.calls.Load() != int32(ExpectedTasks()) {
		t.Fatalf("combined graph-native result=%+v tasks=%d factories=%d",
			result.Summary, len(result.Tasks), factory.calls.Load())
	}
	for _, outcome := range result.Tasks {
		if !outcome.Completed || outcome.Execution == nil ||
			outcome.Execution.Kind != bench.ExecutionGraphNative || outcome.Execution.Scope != outcome.ID {
			t.Fatalf("combined graph-native outcome = %+v", outcome)
		}
	}
	receipt, err := bundle.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := VerifyReviewBundle(receipt.Directory, receipt.ManifestSHA256)
	if err != nil || !manifest.Complete || manifest.Reportable ||
		len(manifest.Attempts) != ExpectedTasks() || len(manifest.Missing) != 0 {
		t.Fatalf("combined review manifest = %+v, %v", manifest, err)
	}
	for _, attempt := range manifest.Attempts {
		if attempt.ExecutionStatus != "graph-native-attested" || attempt.Reportable ||
			attempt.VideoStatus != "retained-playable-video" || len(attempt.Media) != 2 ||
			attempt.Media[0].Kind != "audio" || attempt.Media[0].MediaType != "audio/wav" ||
			attempt.Media[1].Kind != "video" || attempt.Media[1].MediaType != "video/mp4" {
			t.Fatalf("combined review attempt = %+v", attempt)
		}
		mediaDirectory := filepath.Join(directory, filepath.FromSlash(filepath.Dir(attempt.MediaManifest.Path)))
		mediaManifest, err := reviewmedia.VerifyBundle(mediaDirectory, attempt.MediaManifest.SHA256)
		if err != nil || mediaManifest.Attestor == nil ||
			mediaManifest.Attestor.Descriptor.Capability != reviewmedia.FullDecodeAttestationCapability ||
			len(mediaManifest.Video) != 1 || mediaManifest.Video[0].PlayableSpec == nil {
			t.Fatalf("combined full-decode media for %s = %+v, %v", attempt.Case, mediaManifest, err)
		}
	}

	sourceReceipt, err := bundle.SourceReceipt()
	if err != nil {
		t.Fatal(err)
	}
	externalSource, err := ReadReviewSourceReceipt(context.Background(), sourceReceiptPath)
	if err != nil || !samePortableMeetingSourceReceipt(externalSource, sourceReceipt) {
		t.Fatalf("external source receipt = %+v, %v", externalSource, err)
	}
	openedSource, err := VerifyMeetingReviewSource(
		context.Background(), externalSource.Directory, externalSource,
	)
	if err != nil || !openedSource.Manifest.Complete ||
		len(openedSource.Manifest.Attempts) != ExpectedTasks() {
		t.Fatalf("external source population = %+v, %v", openedSource.Manifest, err)
	}

	evaluationReceipts := bundle.EvaluationReceipts()
	if len(evaluationReceipts) != ExpectedTasks() {
		t.Fatalf("evaluation receipt population = %d, want %d", len(evaluationReceipts), ExpectedTasks())
	}
	for index, evaluationReceipt := range evaluationReceipts {
		path := filepath.Join(evaluationReceiptDirectory,
			filepath.Base(evaluationReceipt.Directory)+".receipt.json")
		external, err := revieweval.ReadEvaluationBundleReceipt(context.Background(), path)
		if err != nil || !reflect.DeepEqual(external, evaluationReceipt) {
			t.Fatalf("external evaluation receipt %d = %+v, %v", index+1, external, err)
		}
		if _, err := revieweval.VerifyEvaluationBundle(
			context.Background(), revieweval.EvaluationBundleOptions{Directory: external.Directory}, external,
		); err != nil {
			t.Fatalf("verify external evaluation receipt %d: %v", index+1, err)
		}
	}
	reviewPayload, err := os.ReadFile(filepath.Join(directory, "REVIEW.md"))
	if err != nil || !bytes.Equal(reviewPayload, []byte(renderMeetingReview(manifest))) ||
		!bytes.Contains(reviewPayload, []byte("— PASS")) ||
		!bytes.Contains(reviewPayload, []byte("— FAIL")) {
		t.Fatalf("canonical pass/fail review index error=%v:\n%s", err, reviewPayload)
	}
}

func TestMeetingReviewBundlePreservesMediaButFailsClosedWhenReviewerFails(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "reviewer-failure")
	secret := "secondary-review-secret"
	task := Suite()[0]
	reviewer := &fixtureMeetingReviewer{failCase: task.ID, failErr: errors.New(secret)}
	lease := openFixtureMeetingReviewer(t, reviewer)
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease, SensitiveValues: []string{secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	fixtureResult := bench.Result{Cell: ReferenceCell()}
	specification := fixtureMeetingEvidenceAttempt(task, fixtureResult)
	attempt, err := bundle.BeginAttempt(context.Background(), specification)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureAudio(fixtureMeetingAudio(0)); err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{ID: task.ID, Completed: true, Passed: true}
	err = attempt.Complete(context.Background(), EvidenceCompletion{
		Attempt: specification, Outcome: outcome,
	})
	if err != nil {
		t.Fatalf("review failure = %v", err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks(),
		Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(context.Background(), result); err == nil ||
		strings.Contains(err.Error(), secret) {
		t.Fatalf("FinishSuite() error = %v", err)
	}
	if _, err := bundle.Receipt(); err == nil {
		t.Fatal("advisory outage unexpectedly produced a final review receipt")
	}
	sourceReceipt, err := bundle.SourceReceipt()
	if err != nil {
		t.Fatalf("deterministic source receipt after reviewer outage: %v", err)
	}
	opened, err := VerifyMeetingReviewSource(context.Background(), sourceReceipt.Directory, sourceReceipt)
	if err != nil || opened.Manifest.Complete || len(opened.Manifest.Attempts) != 1 ||
		len(opened.Manifest.Attempts[0].Media) != 1 {
		t.Fatalf("reviewer-outage source = %+v, %v", opened.Manifest, err)
	}
	if len(bundle.EvaluationReceipts()) != 0 {
		t.Fatal("failed reviewer unexpectedly produced a sealed evaluation receipt")
	}
	if err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		payload, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Contains(payload, []byte(secret)) {
			return fmt.Errorf("secret retained in %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMeetingReviewBundleSealsSourceBeforeReviewerAndRetriesOnlyMissingSiblingEvaluation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "reviewer-retry")
	failingCase := Suite()[0].ID
	reviewer := &fixtureMeetingReviewer{failCase: failingCase, failErr: errors.New("transient outage")}
	lease := openFixtureMeetingReviewer(t, reviewer)
	var bundle *ReviewBundle
	var sourceSeen atomic.Bool
	reviewer.onReview = func() {
		receipt, receiptErr := bundle.SourceReceipt()
		external, externalErr := ReadReviewSourceReceipt(
			context.Background(), directory+".source-receipt.json",
		)
		if receiptErr == nil && externalErr == nil &&
			samePortableMeetingSourceReceipt(external, receipt) {
			if _, verifyErr := VerifyMeetingReviewSource(
				context.Background(), external.Directory, external,
			); verifyErr == nil {
				sourceSeen.Store(true)
			}
		}
	}
	var err error
	bundle, err = NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("transient reviewer outage unexpectedly committed a final review bundle")
	}
	if !sourceSeen.Load() {
		t.Fatal("reviewer was invoked before an independently verifiable source receipt existed")
	}
	sourceReceipt, err := bundle.SourceReceipt()
	if err != nil {
		t.Fatal(err)
	}
	opened, err := VerifyMeetingReviewSource(context.Background(), sourceReceipt.Directory, sourceReceipt)
	if err != nil || !opened.Manifest.Complete || len(opened.Manifest.Attempts) != ExpectedTasks() {
		t.Fatalf("sealed source during outage = %+v, %v", opened.Manifest, err)
	}
	if got := len(bundle.EvaluationReceipts()); got != ExpectedTasks()-1 {
		t.Fatalf("successful sibling evaluation receipts after outage = %d, want %d", got, ExpectedTasks()-1)
	}
	reviewer.mu.Lock()
	reviewer.failCase = ""
	reviewer.failErr = nil
	reviewer.mu.Unlock()
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatalf("retry missing advisory evaluation: %v", err)
	}
	if _, err := bundle.Receipt(); err != nil {
		t.Fatalf("final review receipt after retry: %v", err)
	}
	if got := len(bundle.EvaluationReceipts()); got != ExpectedTasks() {
		t.Fatalf("evaluation receipts after retry = %d, want %d", got, ExpectedTasks())
	}
	reviewer.mu.Lock()
	requests := slices.Clone(reviewer.requests)
	reviewer.mu.Unlock()
	counts := make(map[string]int)
	for _, caseID := range requests {
		counts[caseID]++
	}
	for _, task := range Suite() {
		want := 1
		if task.ID == failingCase {
			want = 2
		}
		if counts[task.ID] != want {
			t.Fatalf("reviewer calls for %s = %d, want %d", task.ID, counts[task.ID], want)
		}
	}
}

func TestMeetingReviewBundleReviewsOnlyAttemptsAdmittedToSealedSource(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "partial-source-population")
	reviewer := &fixtureMeetingReviewer{}
	lease := openFixtureMeetingReviewer(t, reviewer)
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)

	// Corrupt only the provider-neutral projection for the second case after
	// earlier evidence has been retained. The media receipt and deterministic
	// attempt stay intact, but PrepareContext must refuse the nonexistent file.
	// This exercises the exact boundary that previously left the mutable attempt
	// in the advisory input map after omitting it from the source manifest.
	omitted := Suite()[1].ID
	bundle.mu.Lock()
	pending := bundle.pending[omitted]
	if pending.MediaReceipt.Manifest.Audio == nil {
		bundle.mu.Unlock()
		t.Fatal("fixture media has no retained audio")
	}
	pending.MediaReceipt.Manifest.Audio.Path = "missing-provider-input.wav"
	bundle.pending[omitted] = pending
	bundle.mu.Unlock()

	finishErr := bundle.FinishSuite(context.Background(), result)
	if finishErr == nil || !strings.Contains(finishErr.Error(), "retained 3 of 4 attempts") {
		t.Fatalf("diagnostic partial source finish = %v", finishErr)
	}
	sourceReceipt, err := bundle.SourceReceipt()
	if err != nil {
		t.Fatal(err)
	}
	openedSource, err := VerifyMeetingReviewSource(
		context.Background(), sourceReceipt.Directory, sourceReceipt,
	)
	if err != nil {
		t.Fatalf("verify diagnostic source: %v", err)
	}
	if openedSource.Manifest.Complete || openedSource.Manifest.Reportable ||
		len(openedSource.Manifest.Attempts) != ExpectedTasks()-1 ||
		len(openedSource.Manifest.Missing) != 1 ||
		openedSource.Manifest.Missing[0].Case != omitted ||
		!strings.Contains(openedSource.Manifest.Missing[0].Reason,
			"stage=prepare_context code=media_refused") {
		t.Fatalf("diagnostic source population = %+v", openedSource.Manifest)
	}
	for _, attempt := range openedSource.Manifest.Attempts {
		if attempt.Case == omitted {
			t.Fatal("PrepareContext-refused attempt escaped into the sealed source population")
		}
	}

	reviewer.mu.Lock()
	requests := slices.Clone(reviewer.requests)
	reviewer.mu.Unlock()
	if len(requests) != ExpectedTasks()-1 || slices.Contains(requests, omitted) {
		t.Fatalf("reviewer requests = %v, omitted case %q must not be called", requests, omitted)
	}
	if _, err := bundle.Receipt(); err == nil {
		t.Fatal("incomplete diagnostic review unexpectedly received a reportable commit receipt")
	}
	manifest := readMeetingReviewManifest(t, directory)
	if manifest.Complete || manifest.Reportable ||
		!reflect.DeepEqual(manifest.Missing, openedSource.Manifest.Missing) ||
		len(manifest.Attempts) != ExpectedTasks()-1 {
		t.Fatalf("diagnostic review population = %+v", manifest)
	}
	if retryErr := bundle.FinishSuite(context.Background(), result); retryErr == nil ||
		retryErr.Error() != finishErr.Error() {
		t.Fatalf("finished diagnostic retry = %v, want stable %v", retryErr, finishErr)
	}
	reviewer.mu.Lock()
	retryRequests := slices.Clone(reviewer.requests)
	reviewer.mu.Unlock()
	if !reflect.DeepEqual(retryRequests, requests) {
		t.Fatalf("finished diagnostic retried provider calls: before %v, after %v", requests, retryRequests)
	}
}

func TestMeetingReviewBundleRecoversReceiptAnchoredStagedSourceBeforeReviewer(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "source-promotion-recovery")
	reviewer := &fixtureMeetingReviewer{}
	lease := openFixtureMeetingReviewer(t, reviewer)
	var interrupted atomic.Bool
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
		afterSourceReceiptPublication: func() error {
			if interrupted.CompareAndSwap(false, true) {
				return errors.New("fixture process interruption after source receipt publication")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("post-source-receipt interruption unexpectedly committed the review bundle")
	}
	external, err := ReadReviewSourceReceipt(context.Background(), directory+".source-receipt.json")
	if err != nil {
		t.Fatalf("durable source receipt did not survive interruption: %v", err)
	}
	sourceDirectory := filepath.Join(directory, reviewSourceDirectory)
	if _, err := verifyMeetingReviewSourceAtMarker(
		context.Background(), sourceDirectory, external, reviewSourceManifestStage, false,
	); err != nil {
		t.Fatalf("receipt-anchored staged source did not survive interruption: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(sourceDirectory, reviewSourceManifestName)); !os.IsNotExist(err) {
		t.Fatalf("source became finally visible before recovery: %v", err)
	}
	reviewer.mu.Lock()
	preRecoveryCalls := len(reviewer.requests)
	reviewer.mu.Unlock()
	if preRecoveryCalls != 0 {
		t.Fatalf("reviewer was invoked %d times before final source promotion", preRecoveryCalls)
	}
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatalf("retry did not promote the receipt-anchored source: %v", err)
	}
	if _, err := VerifyMeetingReviewSource(context.Background(), sourceDirectory, external); err != nil {
		t.Fatalf("promoted source failed final verification: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(sourceDirectory, reviewSourceManifestStage)); !os.IsNotExist(err) {
		t.Fatalf("staged marker remained after exclusive promotion: %v", err)
	}
	reviewer.mu.Lock()
	postRecoveryCalls := len(reviewer.requests)
	reviewer.mu.Unlock()
	if postRecoveryCalls != ExpectedTasks() {
		t.Fatalf("reviewer calls after recovered source = %d, want %d", postRecoveryCalls, ExpectedTasks())
	}
}

func TestMeetingReviewBundleResumesReceiptAnchoredSourceInNewProcess(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "source-new-process-recovery")
	makeMeetingReviewTreeRemovable(t, directory)
	reviewerBefore := &fixtureMeetingReviewer{}
	leaseBefore := newFixtureMeetingReviewerLease(t, reviewerBefore)
	var interrupted atomic.Bool
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: leaseBefore,
		afterSourceReceiptPublication: func() error {
			if interrupted.CompareAndSwap(false, true) {
				return errors.New("fixture process interruption after source receipt publication")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("post-source-receipt interruption unexpectedly committed the review bundle")
	}
	if err := bundle.Close(); err != nil {
		t.Fatalf("close interrupted process bundle: %v", err)
	}
	if err := leaseBefore.Close(); err != nil {
		t.Fatalf("close interrupted process reviewer: %v", err)
	}
	reviewerBefore.mu.Lock()
	beforeCalls := len(reviewerBefore.requests)
	reviewerBefore.mu.Unlock()
	if beforeCalls != 0 {
		t.Fatalf("reviewer was invoked %d times before source recovery", beforeCalls)
	}

	reviewerAfter := &fixtureMeetingReviewer{}
	leaseAfter := newFixtureMeetingReviewerLease(t, reviewerAfter)
	resumed, recoveredResult, err := ResumeReviewBundle(
		context.Background(), ReviewBundleOptions{Directory: directory, Reviewer: leaseAfter},
	)
	if err != nil {
		t.Fatalf("resume interrupted source in a fresh owner: %v", err)
	}
	defer func() {
		if err := resumed.Close(); err != nil {
			t.Errorf("close resumed bundle: %v", err)
		}
		if err := leaseAfter.Close(); err != nil {
			t.Errorf("close resumed reviewer: %v", err)
		}
	}()
	if !reflect.DeepEqual(recoveredResult, result) {
		t.Fatalf("recovered deterministic result drifted\n got: %+v\nwant: %+v", recoveredResult, result)
	}
	if err := resumed.FinishSuite(context.Background(), recoveredResult); err != nil {
		t.Fatalf("finish fresh-process source recovery: %v", err)
	}
	reviewerAfter.mu.Lock()
	afterCalls := len(reviewerAfter.requests)
	reviewerAfter.mu.Unlock()
	if afterCalls != ExpectedTasks() {
		t.Fatalf("fresh-process reviewer calls = %d, want %d", afterCalls, ExpectedTasks())
	}
	receipt, err := resumed.Receipt()
	if err != nil {
		t.Fatalf("read fresh-process recovered receipt: %v", err)
	}
	if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err != nil {
		t.Fatalf("verify fresh-process recovered bundle: %v", err)
	}
}

func TestMeetingReviewBundleResumeRejectsReceiptFromAnotherBundle(t *testing.T) {
	parent := t.TempDir()
	createInterrupted := func(name string) string {
		directory := filepath.Join(parent, name)
		makeMeetingReviewTreeRemovable(t, directory)
		reviewer := &fixtureMeetingReviewer{}
		lease := newFixtureMeetingReviewerLease(t, reviewer)
		bundle, err := NewReviewBundle(ReviewBundleOptions{
			Directory: directory, Reviewer: lease,
			afterSourceReceiptPublication: func() error {
				return errors.New("fixture interruption before source promotion")
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		result := completeFixtureMeetingReviewAttempts(t, bundle)
		if err := bundle.FinishSuite(context.Background(), result); err == nil {
			t.Fatal("source interruption unexpectedly committed")
		}
		if err := bundle.Close(); err != nil {
			t.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		return directory
	}
	first := createInterrupted("first")
	second := createInterrupted("second")
	lease := newFixtureMeetingReviewerLease(t, &fixtureMeetingReviewer{})
	defer lease.Close()
	resumed, _, err := ResumeReviewBundle(context.Background(), ReviewBundleOptions{
		Directory: first, Reviewer: lease,
		SourceReceiptPath:             second + ".source-receipt.json",
		EvaluationReceiptDirectory:    first + ".evaluation-receipts",
		EvaluationQuarantineDirectory: first + ".evaluation-quarantine",
	})
	if resumed != nil || err == nil || !strings.Contains(err.Error(), "outside the requested bundle") {
		t.Fatalf("cross-bundle source receipt resume = bundle %v, error %v", resumed, err)
	}
}

func TestMeetingReviewBundleAdoptsPublishedEvaluationAfterPostPublicationFault(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "evaluation-adoption")
	reviewer := &fixtureMeetingReviewer{}
	lease := openFixtureMeetingReviewer(t, reviewer)
	var injected atomic.Bool
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
		afterEvaluationPublication: func(caseID string) error {
			if caseID == Suite()[0].ID && injected.CompareAndSwap(false, true) {
				return errors.New("fixture process interruption after evaluation publication")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("post-publication fault unexpectedly committed the review index")
	}
	receiptPath, err := bundle.externalEvaluationReceiptPath(1, Suite()[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	external, err := revieweval.ReadEvaluationBundleReceipt(context.Background(), receiptPath)
	if err != nil {
		t.Fatalf("published evaluation receipt did not survive the fault: %v", err)
	}
	if _, err := revieweval.VerifyEvaluationBundle(
		context.Background(), revieweval.EvaluationBundleOptions{Directory: external.Directory}, external,
	); err != nil {
		t.Fatalf("published evaluation did not survive the fault: %v", err)
	}
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatalf("retry did not adopt the published evaluation: %v", err)
	}
	reviewer.mu.Lock()
	requests := slices.Clone(reviewer.requests)
	reviewer.mu.Unlock()
	counts := make(map[string]int)
	for _, caseID := range requests {
		counts[caseID]++
	}
	for _, task := range Suite() {
		if counts[task.ID] != 1 {
			t.Fatalf("reviewer calls for %s = %d, want one durable invocation", task.ID, counts[task.ID])
		}
	}
}

func TestMeetingReviewBundleResumesPublishedEvaluationsInNewProcess(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "evaluation-new-process-recovery")
	makeMeetingReviewTreeRemovable(t, directory)
	reviewerBefore := &fixtureMeetingReviewer{}
	leaseBefore := newFixtureMeetingReviewerLease(t, reviewerBefore)
	var interrupted atomic.Bool
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: leaseBefore,
		afterEvaluationPublication: func(caseID string) error {
			if caseID == Suite()[0].ID && interrupted.CompareAndSwap(false, true) {
				return errors.New("fixture process interruption after evaluation publication")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("post-evaluation-receipt interruption unexpectedly committed the review bundle")
	}
	for _, task := range Suite() {
		ordinal, ok := meetingCaseOrdinal(task.ID)
		if !ok {
			t.Fatalf("missing ordinal for %s", task.ID)
		}
		receiptPath, pathErr := bundle.externalEvaluationReceiptPath(ordinal, task.ID)
		if pathErr != nil {
			t.Fatal(pathErr)
		}
		if _, readErr := revieweval.ReadEvaluationBundleReceipt(context.Background(), receiptPath); readErr != nil {
			t.Fatalf("evaluation receipt for %s was not durable before interruption: %v", task.ID, readErr)
		}
	}
	if err := bundle.Close(); err != nil {
		t.Fatalf("close interrupted evaluation process bundle: %v", err)
	}
	if err := leaseBefore.Close(); err != nil {
		t.Fatalf("close interrupted evaluation process reviewer: %v", err)
	}

	reviewerAfter := &fixtureMeetingReviewer{}
	leaseAfter := newFixtureMeetingReviewerLease(t, reviewerAfter)
	resumed, recoveredResult, err := ResumeReviewBundle(
		context.Background(), ReviewBundleOptions{Directory: directory, Reviewer: leaseAfter},
	)
	if err != nil {
		t.Fatalf("resume published evaluations in a fresh owner: %v", err)
	}
	defer func() {
		if err := resumed.Close(); err != nil {
			t.Errorf("close resumed bundle: %v", err)
		}
		if err := leaseAfter.Close(); err != nil {
			t.Errorf("close resumed reviewer: %v", err)
		}
	}()
	if !reflect.DeepEqual(recoveredResult, result) {
		t.Fatal("fresh-process evaluation recovery changed the deterministic result")
	}
	if err := resumed.FinishSuite(context.Background(), recoveredResult); err != nil {
		t.Fatalf("finish fresh-process evaluation recovery: %v", err)
	}
	reviewerBefore.mu.Lock()
	beforeRequests := slices.Clone(reviewerBefore.requests)
	reviewerBefore.mu.Unlock()
	if len(beforeRequests) != ExpectedTasks() {
		t.Fatalf("original reviewer calls = %d, want %d", len(beforeRequests), ExpectedTasks())
	}
	reviewerAfter.mu.Lock()
	afterRequests := slices.Clone(reviewerAfter.requests)
	reviewerAfter.mu.Unlock()
	if len(afterRequests) != 0 {
		t.Fatalf("fresh-process recovery repeated %d already receipted reviewer calls: %v", len(afterRequests), afterRequests)
	}
	receipt, err := resumed.Receipt()
	if err != nil {
		t.Fatalf("read fresh-process evaluation receipt: %v", err)
	}
	if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err != nil {
		t.Fatalf("verify fresh-process evaluation recovery: %v", err)
	}
}

func TestMeetingReviewBundleIsCreateOnlyAndRequiresReviewer(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "existing")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	if _, err := NewReviewBundle(ReviewBundleOptions{
		Directory: path, Reviewer: lease,
	}); err == nil {
		t.Fatal("existing bundle path was accepted")
	}
	if _, err := NewReviewBundle(ReviewBundleOptions{
		Directory: filepath.Join(parent, "missing-reviewer"),
	}); err == nil {
		t.Fatal("nil reviewer was accepted")
	}
}

func TestMeetingReviewBundleRejectsNoncanonicalTaskBeforeAllocatingVideoResources(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "noncanonical-task")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	factory := &forbiddenMeetingVideoFactory{}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease, VideoFactory: factory,
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	specification := fixtureMeetingEvidenceAttempt(
		Suite()[0], bench.Result{Cell: ReferenceCell()},
	)
	// This remains structurally valid and retains the canonical ID, but it is
	// not the repository-owned task that the case ID names.
	specification.Task.PageMode = "foreign-valid-page-mode"
	if err := specification.Task.Validate(); err != nil {
		t.Fatalf("mutated task is not structurally valid: %v", err)
	}
	if _, err := bundle.BeginAttempt(context.Background(), specification); err == nil ||
		!strings.Contains(err.Error(), "canonical") {
		t.Fatalf("noncanonical task admission = %v", err)
	}
	if factory.calls.Load() != 0 {
		t.Fatalf("video factory calls = %d, want zero", factory.calls.Load())
	}
	ordinal, _ := meetingCaseOrdinal(specification.Case)
	attemptDirectory := filepath.Join(directory, reviewSourceDirectory, "media",
		fmt.Sprintf("%02d-%s-trial-01", ordinal, meetingReviewSlug(specification.Case)))
	if _, err := os.Lstat(attemptDirectory); !os.IsNotExist(err) {
		t.Fatalf("rejected task allocated an attempt directory: %v", err)
	}
	bundle.mu.Lock()
	active, failures := bundle.active, len(bundle.failures)
	bundle.mu.Unlock()
	if active != 0 || failures != 0 {
		t.Fatalf("rejected task retained resources: active=%d failures=%d", active, failures)
	}
}

func TestMeetingReviewBundleClosesEveryFreshVideoPluginExactlyOnceOnBeginFailure(t *testing.T) {
	tests := []struct {
		name          string
		factoryErr    error
		encoder       *countingMeetingEncoder
		attestor      *countingMeetingAttestor
		wantEncClaims int32
		wantAttClaims int32
	}{
		{
			name: "factory error after both allocations", factoryErr: errors.New("factory fault"),
			encoder:  &countingMeetingEncoder{validDescriptor: true},
			attestor: &countingMeetingAttestor{validDescriptor: true},
		},
		{
			name: "factory error after partial allocation", factoryErr: errors.New("partial factory fault"),
			encoder: &countingMeetingEncoder{validDescriptor: true},
		},
		{
			name:     "unclaimed descriptor failure",
			encoder:  &countingMeetingEncoder{},
			attestor: &countingMeetingAttestor{validDescriptor: true},
		},
		{
			name:    "partial claim failure",
			encoder: &countingMeetingEncoder{validDescriptor: true},
			attestor: &countingMeetingAttestor{
				validDescriptor: true, claimErr: errors.New("attestor claim fault"),
			},
			wantEncClaims: 1, wantAttClaims: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "video-plugin-lifecycle")
			lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
			factory := &countingMeetingVideoFactory{
				encoder: test.encoder, attestor: test.attestor, err: test.factoryErr,
			}
			bundle, err := NewReviewBundle(ReviewBundleOptions{
				Directory: directory, Reviewer: lease, VideoFactory: factory,
			})
			if err != nil {
				t.Fatal(err)
			}
			makeMeetingReviewTreeRemovable(t, directory)
			if _, err := bundle.BeginAttempt(context.Background(), fixtureMeetingEvidenceAttempt(
				Suite()[0], bench.Result{Cell: ReferenceCell()},
			)); err == nil {
				t.Fatal("video plug-in failure unexpectedly created a recorder")
			}
			if test.encoder != nil {
				if got := test.encoder.closes.Load(); got != 1 {
					t.Fatalf("encoder close calls = %d, want 1", got)
				}
				if got := test.encoder.claims.Load(); got != test.wantEncClaims {
					t.Fatalf("encoder claim calls = %d, want %d", got, test.wantEncClaims)
				}
			}
			if test.attestor != nil {
				if got := test.attestor.closes.Load(); got != 1 {
					t.Fatalf("attestor close calls = %d, want 1", got)
				}
				if got := test.attestor.claims.Load(); got != test.wantAttClaims {
					t.Fatalf("attestor claim calls = %d, want %d", got, test.wantAttClaims)
				}
			}
			if err := bundle.Close(); err != nil {
				t.Fatalf("close failed bundle: %v", err)
			}
		})
	}
}

func TestMeetingReviewBundleConcurrentFinishIsOneIdempotentCommit(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "concurrent-finish")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := bench.Result{Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks()}
	for index, task := range Suite() {
		specification := fixtureMeetingEvidenceAttempt(task, result)
		attempt, err := bundle.BeginAttempt(context.Background(), specification)
		if err != nil {
			t.Fatal(err)
		}
		if err := attempt.CaptureAudio(fixtureMeetingAudio(index)); err != nil {
			t.Fatal(err)
		}
		outcome := bench.TaskOutcome{ID: task.ID, Completed: true, Passed: true}
		if err := attempt.Complete(context.Background(), EvidenceCompletion{
			Attempt: specification, Outcome: outcome,
		}); err != nil {
			t.Fatal(err)
		}
		result.Tasks = append(result.Tasks, outcome)
	}
	result.Finish()
	const callers = 16
	errorsSeen := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			errorsSeen <- bundle.FinishSuite(context.Background(), result)
		}()
	}
	group.Wait()
	close(errorsSeen)
	var finishErrors []error
	for err := range errorsSeen {
		if err != nil {
			finishErrors = append(finishErrors, err)
		}
	}
	manifest := readMeetingReviewManifest(t, directory)
	if len(finishErrors) != 0 {
		t.Fatalf("concurrent finish errors = %v; manifest = %+v", finishErrors, manifest)
	}
	if !manifest.Complete || len(manifest.Attempts) != ExpectedTasks() {
		t.Fatalf("concurrent-finish manifest = %+v", manifest)
	}
}

func TestMeetingReviewBundleFinishClosesAdmissionAndDrainsActiveCompletion(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "finish-drain")
	reviewer := &fixtureMeetingReviewer{}
	lease := openFixtureMeetingReviewer(t, reviewer)
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	task := Suite()[0]
	fixtureResult := bench.Result{Cell: ReferenceCell()}
	specification := fixtureMeetingEvidenceAttempt(task, fixtureResult)
	attempt, err := bundle.BeginAttempt(context.Background(), specification)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureAudio(fixtureMeetingAudio(0)); err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{ID: task.ID, Completed: true, Passed: true}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks(),
		Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	finishDone := make(chan error, 1)
	go func() { finishDone <- bundle.FinishSuite(context.Background(), result) }()
	deadline := time.Now().Add(time.Second)
	for {
		bundle.mu.Lock()
		closing := bundle.closing
		bundle.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("FinishSuite did not close attempt admission")
		}
		runtime.Gosched()
	}
	select {
	case err := <-finishDone:
		t.Fatalf("FinishSuite returned before active completion: %v", err)
	default:
	}
	reviewer.mu.Lock()
	reviewRequests := len(reviewer.requests)
	reviewer.mu.Unlock()
	if reviewRequests != 0 {
		t.Fatalf("reviewer observed %d requests before the deterministic attempt was final", reviewRequests)
	}
	second := Suite()[1]
	if _, err := bundle.BeginAttempt(context.Background(), fixtureMeetingEvidenceAttempt(
		second, bench.Result{Cell: ReferenceCell()},
	)); err == nil {
		t.Fatal("BeginAttempt was admitted after FinishSuite closed admission")
	}
	if err := attempt.Complete(context.Background(), EvidenceCompletion{
		Attempt: specification, Outcome: outcome,
	}); err != nil {
		t.Fatal(err)
	}
	if err := <-finishDone; err == nil {
		t.Fatal("one-case suite unexpectedly committed as complete")
	}
	manifest := readMeetingReviewManifest(t, directory)
	if manifest.Complete || len(manifest.Attempts) != 1 ||
		manifest.Attempts[0].ReviewStatus != "complete" {
		t.Fatalf("drained active completion = %+v", manifest)
	}
}

func TestMeetingReviewBundleFinishDrainsBlockedReviewAndCancellationCanRetry(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "blocked-review")
	block := make(chan struct{})
	ready := make(chan struct{})
	reviewer := &fixtureMeetingReviewer{reviewReady: ready, reviewBlock: block}
	lease := openFixtureMeetingReviewer(t, reviewer)
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	task := Suite()[0]
	fixtureResult := bench.Result{Cell: ReferenceCell()}
	specification := fixtureMeetingEvidenceAttempt(task, fixtureResult)
	attempt, err := bundle.BeginAttempt(context.Background(), specification)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureAudio(fixtureMeetingAudio(0)); err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{ID: task.ID, Completed: true, Passed: true}
	if err := attempt.Complete(context.Background(), EvidenceCompletion{
		Attempt: specification, Outcome: outcome,
	}); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks(),
		Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	canceled, cancel := context.WithCancel(context.Background())
	finishDone := make(chan error, 1)
	go func() { finishDone <- bundle.FinishSuite(canceled, result) }()
	<-ready
	cancel()
	if err := <-finishDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("FinishSuite while review blocked = %v, want cancellation", err)
	}
	second := Suite()[1]
	if _, err := bundle.BeginAttempt(context.Background(), EvidenceAttempt{
		Suite: SuiteName, Case: second.ID, Trial: 1, Task: second,
		Cell: ReferenceCell(), Origin: fixtureMeetingOrigin(),
	}); err == nil {
		t.Fatal("BeginAttempt was admitted after FinishSuite closed attempt admission")
	}
	reviewer.reviewReady = nil
	reviewer.reviewBlock = nil
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("incomplete retry unexpectedly committed a complete bundle")
	}
	manifest := readMeetingReviewManifest(t, directory)
	if len(manifest.Attempts) != 1 || manifest.Attempts[0].ReviewStatus != "complete" {
		t.Fatalf("drained review attempt = %+v", manifest)
	}
}

func TestMeetingReviewBundleFinalVerificationRejectsPostReviewMediaMutation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "mutated-media")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	audioPath := filepath.Join(directory, reviewSourceDirectory, "media",
		"01-open-share-present-trial-01", "audio.stereo.wav")
	payload, err := os.ReadFile(audioPath)
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)-1] ^= 0xff
	if err := os.Chmod(audioPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(audioPath, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("post-review media mutation was accepted")
	}
	if _, err := bundle.Receipt(); err == nil {
		t.Fatal("failed bundle returned an authenticity receipt")
	}
	if _, err := bundle.SourceReceipt(); err == nil {
		t.Fatal("mutated media unexpectedly produced a sealed source receipt")
	}
}

func TestSealMeetingReviewSourceRetainsRequiredEmptyNamespaces(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	for _, relative := range []string{"contexts", "media"} {
		if err := os.Mkdir(filepath.Join(directory, relative), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(directory, "media", "diagnostic.bin"), []byte("diagnostic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sealMeetingReviewSourceTreeContext(context.Background(), directory); err != nil {
		t.Fatalf("seal diagnostic source with empty context namespace: %v", err)
	}
	for _, relative := range []string{".", "contexts", "media"} {
		info, err := os.Stat(filepath.Join(directory, relative))
		if err != nil || info.Mode().Perm() != 0o500 {
			t.Fatalf("sealed source directory %q mode=%v error=%v", relative, info, err)
		}
	}

	unexpected := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(unexpected, 0o700); err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, unexpected)
	for _, relative := range []string{"contexts", "media", "unreviewed"} {
		if err := os.Mkdir(filepath.Join(unexpected, relative), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(unexpected, "media", "diagnostic.bin"), []byte("diagnostic"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sealMeetingReviewSourceTreeContext(context.Background(), unexpected); err == nil {
		t.Fatal("unexpected empty source namespace was promoted into the sealed tree")
	}
}

func TestMeetingReviewBundleRejectsFindingOutsideRetainedMediaTimeline(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "finding-timeline")
	beyond := int64(10_000)
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{findingAtMS: &beyond})
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	task := Suite()[0]
	fixtureResult := bench.Result{Cell: ReferenceCell()}
	specification := fixtureMeetingEvidenceAttempt(task, fixtureResult)
	attempt, err := bundle.BeginAttempt(context.Background(), specification)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureAudio(fixtureMeetingAudio(0)); err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{ID: task.ID, Completed: true, Passed: true}
	if err := attempt.Complete(context.Background(), EvidenceCompletion{
		Attempt: specification, Outcome: outcome,
	}); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks(),
		Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("incomplete timeline-drift bundle committed successfully")
	}
	sourceReceipt, err := bundle.SourceReceipt()
	if err != nil {
		t.Fatal(err)
	}
	opened, err := VerifyMeetingReviewSource(context.Background(), sourceReceipt.Directory, sourceReceipt)
	if err != nil || len(opened.Manifest.Attempts) != 1 {
		t.Fatalf("timeline-drift source = %+v, %v", opened.Manifest, err)
	}
	if _, err := bundle.Receipt(); err == nil || len(bundle.EvaluationReceipts()) != 0 {
		t.Fatal("out-of-range advisory finding produced a final/evaluation receipt")
	}
}

func TestMeetingReviewResponseCloneOwnsFindingTimestampPointers(t *testing.T) {
	start, end := int64(1), int64(2)
	source := ReviewResponse{
		SignificantProblems: []ReviewFinding{{StartMS: &start, EndMS: &end}},
		MinorObservations:   []ReviewFinding{}, Limitations: []string{},
	}
	clone := cloneReviewResponse(source)
	start, end = 100, 200
	if clone.SignificantProblems[0].StartMS == nil || clone.SignificantProblems[0].EndMS == nil ||
		*clone.SignificantProblems[0].StartMS != 1 || *clone.SignificantProblems[0].EndMS != 2 {
		t.Fatalf("cloned finding timestamps alias source: %+v", clone.SignificantProblems[0])
	}
}

func TestMeetingReviewBundleBindsExactTaskOutcomeAndRejectsUnknownDrift(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "result-drift")
	reviewer := &fixtureMeetingReviewer{}
	lease := openFixtureMeetingReviewer(t, reviewer)
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	result.Tasks[0].Metrics["task_success_rate"] = 0
	result.Tasks = append(result.Tasks, bench.TaskOutcome{ID: "unknown-case", Completed: true})
	result.Finish()
	if err := bundle.FinishSuite(context.Background(), result); err == nil {
		t.Fatal("drifted exact result was accepted")
	}
	reviewer.mu.Lock()
	reviewRequests := len(reviewer.requests)
	reviewer.mu.Unlock()
	if reviewRequests != 0 {
		t.Fatalf("reviewer observed %d requests for a structurally invalid final result", reviewRequests)
	}
	if _, err := bundle.SourceReceipt(); err == nil {
		t.Fatal("structurally invalid final result produced a source receipt")
	}
	if _, err := bundle.Receipt(); err == nil {
		t.Fatal("structurally invalid final result produced a review receipt")
	}
}

func TestMeetingReviewBundleRejectsFinalRunCellProvenanceRequirementAndSummaryDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *bench.Result)
	}{
		{name: "cell", mutate: func(_ *testing.T, result *bench.Result) {
			result.Cell.Levels[bench.FactorBinding] = "drifted-binding"
		}},
		{name: "provenance", mutate: func(_ *testing.T, result *bench.Result) {
			result.Provenance.Revision = "drifted-revision"
		}},
		{name: "execution requirement", mutate: func(t *testing.T, result *bench.Result) {
			requirement, _ := fixtureMeetingGraphRequirement(t)
			result.Cell.Execution = requirement
		}},
		{name: "summary", mutate: func(_ *testing.T, result *bench.Result) {
			result.Summary.Passed--
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "drift")
			reviewer := &fixtureMeetingReviewer{}
			lease := openFixtureMeetingReviewer(t, reviewer)
			bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
			if err != nil {
				t.Fatal(err)
			}
			makeMeetingReviewTreeRemovable(t, directory)
			result := completeFixtureMeetingReviewAttempts(t, bundle)
			test.mutate(t, &result)
			if err := bundle.FinishSuite(context.Background(), result); err == nil {
				t.Fatal("drifted final run identity committed successfully")
			}
			reviewer.mu.Lock()
			requests := len(reviewer.requests)
			reviewer.mu.Unlock()
			if requests != 0 {
				t.Fatalf("reviewer observed %d requests before final run identity was admitted", requests)
			}
			if _, err := bundle.Receipt(); err == nil {
				t.Fatal("drifted final run identity produced a review receipt")
			}
		})
	}
}

func TestMeetingReviewBundleReceiptDetectsPostCommitTampering(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "tampered-bundle")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	receipt, err := bundle.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err != nil {
		t.Fatal(err)
	}
	reviewPath := filepath.Join(directory, "REVIEW.md")
	if err := os.Chmod(reviewPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reviewPath, []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err == nil {
		t.Fatal("external receipt accepted a modified review document")
	}
}

func TestMeetingReviewVerifierRejectsAttemptNamespaceTraversal(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReviewManifest)
	}{
		{name: "media manifest", mutate: func(manifest *ReviewManifest) {
			manifest.Attempts[0].MediaManifest.Path = "source/media/../../evaluations/manifest.json"
		}},
		{name: "evaluation receipt", mutate: func(manifest *ReviewManifest) {
			manifest.Attempts[0].EvaluationReceipt.Directory = "evaluations/../../source"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "namespace")
			lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
			bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
			if err != nil {
				t.Fatal(err)
			}
			makeMeetingReviewTreeRemovable(t, directory)
			result := completeFixtureMeetingReviewAttempts(t, bundle)
			if err := bundle.FinishSuite(context.Background(), result); err != nil {
				t.Fatal(err)
			}
			manifest := readMeetingReviewManifest(t, directory)
			test.mutate(&manifest)
			payload := rewriteMeetingReviewJSON(t, filepath.Join(directory, "manifest.json"), manifest)
			if _, err := VerifyReviewBundle(directory, reviewDigest(payload)); err == nil ||
				!strings.Contains(err.Error(), "namespace") {
				t.Fatalf("forged %s namespace verification = %v", test.name, err)
			}
		})
	}
}

func TestMeetingReviewVerifierBindsOuterAttemptToRecomputedSourceReceipt(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "source-projection-drift")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	top := readMeetingReviewManifest(t, directory)
	sourceDirectory := filepath.Join(directory, reviewSourceDirectory)
	sourceManifestPath := filepath.Join(sourceDirectory, reviewSourceManifestName)
	sourcePayload, err := os.ReadFile(sourceManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	source, err := decodeMeetingSourceManifest(sourcePayload)
	if err != nil || len(source.Attempts) == 0 || len(source.Attempts[0].MediaArtifacts) < 2 {
		t.Fatalf("source fixture shape = %+v, %v", source, err)
	}
	// Reordering this source-only projection does not alter any artifact bytes
	// or file-set digest. A recomputed source receipt is therefore internally
	// valid, but it must not be accepted under the unchanged outer attempt.
	slices.Reverse(source.Attempts[0].MediaArtifacts)
	forgedSourcePayload, err := marshalMeetingSource(source)
	if err != nil {
		t.Fatal(err)
	}
	forgedSourceReceipt, err := buildMeetingSourceReceipt(
		sourceDirectory, forgedSourcePayload, source,
	)
	if err != nil {
		t.Fatal(err)
	}
	rewriteSealedMeetingPayload(t, sourceManifestPath, forgedSourcePayload)
	if _, err := VerifyMeetingReviewSource(
		context.Background(), sourceDirectory, forgedSourceReceipt,
	); err != nil {
		t.Fatalf("recomputed source receipt should be independently self-consistent: %v", err)
	}
	top.SourceManifest.SHA256 = forgedSourceReceipt.ManifestSHA256
	top.SourceManifest.SizeBytes = int64(len(forgedSourcePayload))
	top.SourceReceipt = forgedSourceReceipt
	top.SourceReceipt.Directory = reviewSourceDirectory
	topPayload := rewriteMeetingReviewJSON(t, filepath.Join(directory, "manifest.json"), top)
	if _, err := VerifyReviewBundle(directory, reviewDigest(topPayload)); err == nil ||
		!strings.Contains(err.Error(), "deterministic source projection") {
		t.Fatalf("recomputed source/outer projection drift verification = %v", err)
	}
}

func TestMeetingReviewSourceExternalReceiptAndFilesystemIdentityAdversaries(t *testing.T) {
	newSource := func(t *testing.T) (string, ReviewSourceReceipt) {
		t.Helper()
		directory := filepath.Join(t.TempDir(), "source-adversary")
		lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
		bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
		if err != nil {
			t.Fatal(err)
		}
		makeMeetingReviewTreeRemovable(t, directory)
		result := completeFixtureMeetingReviewAttempts(t, bundle)
		if err := bundle.FinishSuite(context.Background(), result); err != nil {
			t.Fatal(err)
		}
		receipt, err := bundle.SourceReceipt()
		if err != nil {
			t.Fatal(err)
		}
		return directory, receipt
	}

	t.Run("external create-only receipt", func(t *testing.T) {
		_, receipt := newSource(t)
		path := filepath.Join(t.TempDir(), "meeting-source-receipt.json")
		if err := WriteReviewSourceReceipt(context.Background(), path, receipt); err != nil {
			t.Fatal(err)
		}
		opened, err := ReadReviewSourceReceipt(context.Background(), path)
		if err != nil || !samePortableMeetingSourceReceipt(opened, receipt) {
			t.Fatalf("external source receipt = %+v, %v", opened, err)
		}
		if err := WriteReviewSourceReceipt(context.Background(), path, receipt); err == nil {
			t.Fatal("external source receipt destination was overwritten")
		}
	})

	t.Run("hardlink", func(t *testing.T) {
		_, receipt := newSource(t)
		if err := os.Chmod(receipt.Directory, 0o700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(receipt.Directory, "result.json")
		if err := os.Link(target, filepath.Join(receipt.Directory, "duplicate-result.json")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(receipt.Directory, 0o500); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyMeetingReviewSource(context.Background(), receipt.Directory, receipt); err == nil {
			t.Fatal("hard-linked source artifact was accepted")
		}
	})

	t.Run("external hardlink alias", func(t *testing.T) {
		_, receipt := newSource(t)
		external := filepath.Join(t.TempDir(), "external-result-alias.json")
		if err := os.Link(filepath.Join(receipt.Directory, "result.json"), external); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyMeetingReviewSource(context.Background(), receipt.Directory, receipt); err == nil {
			t.Fatal("source artifact with an out-of-tree hardlink alias was accepted")
		}
	})

	t.Run("symlink root", func(t *testing.T) {
		directory, receipt := newSource(t)
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		moved := receipt.Directory + ".moved"
		if err := os.Rename(receipt.Directory, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(moved, receipt.Directory); err != nil {
			t.Fatal(err)
		}
		if _, err := VerifyMeetingReviewSource(context.Background(), receipt.Directory, receipt); err == nil {
			t.Fatal("symlinked source root was accepted")
		}
	})

	t.Run("root swap after open", func(t *testing.T) {
		directory, receipt := newSource(t)
		ctx := context.WithValue(context.Background(), meetingSourceVerifyHookKey{}, func(stage string) {
			if stage != "opened" {
				return
			}
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(receipt.Directory, receipt.Directory+".opened"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(receipt.Directory, 0o500); err != nil {
				t.Fatal(err)
			}
		})
		if _, err := VerifyMeetingReviewSource(ctx, receipt.Directory, receipt); err == nil {
			t.Fatal("source root swap after os.Root open was accepted")
		}
	})

	t.Run("concurrent file mutation", func(t *testing.T) {
		_, receipt := newSource(t)
		audio := filepath.Join(receipt.Directory, "media", "01-open-share-present-trial-01", "audio.stereo.wav")
		ctx := context.WithValue(context.Background(), meetingSourceVerifyHookKey{}, func(stage string) {
			if stage != "before_final_snapshot" {
				return
			}
			payload, err := os.ReadFile(audio)
			if err != nil {
				t.Fatal(err)
			}
			payload[len(payload)-1] ^= 0xff
			if err := os.Chmod(audio, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(audio, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(audio, 0o400); err != nil {
				t.Fatal(err)
			}
		})
		if _, err := VerifyMeetingReviewSource(ctx, receipt.Directory, receipt); err == nil {
			t.Fatal("source mutation between verification snapshots was accepted")
		}
	})
}

func TestMeetingReviewIndexRejectsExternalHardlinkAlias(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review-external-hardlink")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	receipt, err := bundle.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external-review-alias.md")
	if err := os.Link(filepath.Join(directory, "REVIEW.md"), external); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err == nil {
		t.Fatal("review index artifact with an out-of-tree hardlink alias was accepted")
	}
}

func TestMeetingReviewAnchoredSealAndReadRejectVisibleRootSwapsWithoutTouchingOutside(t *testing.T) {
	t.Run("seal", func(t *testing.T) {
		base := t.TempDir()
		directory := filepath.Join(base, "target")
		outside := filepath.Join(base, "outside")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "inside.txt"), []byte("inside"), 0o600); err != nil {
			t.Fatal(err)
		}
		outsidePath := filepath.Join(outside, "outside.txt")
		if err := os.WriteFile(outsidePath, []byte("outside-must-stay-writable"), 0o600); err != nil {
			t.Fatal(err)
		}
		moved := directory + ".opened"
		ctx := context.WithValue(context.Background(), meetingSourceVerifyHookKey{}, func(stage string) {
			if stage != "review_seal_opened_root" {
				return
			}
			if err := os.Rename(directory, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, directory); err != nil {
				t.Fatal(err)
			}
		})
		if err := sealMeetingReviewTreeContext(ctx, directory); err == nil {
			t.Fatal("visible root swap during seal was accepted")
		}
		info, err := os.Stat(outsidePath)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("outside file mode changed during anchored seal: %v, %v", info, err)
		}
		payload, err := os.ReadFile(outsidePath)
		if err != nil || string(payload) != "outside-must-stay-writable" {
			t.Fatalf("outside file changed during anchored seal: %q, %v", payload, err)
		}
		if err := os.Remove(directory); err != nil {
			t.Fatal(err)
		}
		makeMeetingCLIPathWritableForTest(t, moved)
	})

	t.Run("read", func(t *testing.T) {
		base := t.TempDir()
		parent := filepath.Join(base, "target")
		outside := filepath.Join(base, "outside")
		if err := os.Mkdir(parent, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(outside, 0o700); err != nil {
			t.Fatal(err)
		}
		name := "artifact.json"
		insidePath := filepath.Join(parent, name)
		outsidePath := filepath.Join(outside, name)
		if err := os.WriteFile(insidePath, []byte(`{"scope":"inside"}`), 0o400); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outsidePath, []byte(`{"secret":"outside-must-not-be-read"}`), 0o400); err != nil {
			t.Fatal(err)
		}
		moved := parent + ".opened"
		ctx := context.WithValue(context.Background(), meetingSourceVerifyHookKey{}, func(stage string) {
			if stage != "review_read_opened_root" {
				return
			}
			if err := os.Rename(parent, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, parent); err != nil {
				t.Fatal(err)
			}
		})
		payload, _, err := readMeetingReviewFileContext(ctx, insidePath, true)
		if err == nil || len(payload) != 0 || bytes.Contains(payload, []byte("outside-must-not-be-read")) {
			t.Fatalf("visible root swap read = %q, %v", payload, err)
		}
		if err := os.Remove(parent); err != nil {
			t.Fatal(err)
		}
		makeMeetingCLIPathWritableForTest(t, moved)
	})
}

func makeMeetingCLIPathWritableForTest(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return os.Chmod(path, 0o600)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMeetingReviewExactTreeRejectsExtraReadOnlyMediaDescendant(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "extra-media")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	receipt, err := bundle.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	mediaDirectory := filepath.Join(directory, reviewSourceDirectory, "media",
		"01-open-share-present-trial-01")
	if err := os.Chmod(mediaDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(mediaDirectory, "post-subverify-extra.bin")
	if err := os.WriteFile(extra, []byte("unexpected"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mediaDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err == nil {
		t.Fatal("extra read-only media descendant was accepted")
	}
}

func TestMeetingReviewBundleRejectsConcurrentFinishWithDifferentExactResult(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "finish-binding")
	ready, block := make(chan struct{}), make(chan struct{})
	reviewer := &fixtureMeetingReviewer{reviewReady: ready, reviewBlock: block}
	lease := openFixtureMeetingReviewer(t, reviewer)
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	firstDone := make(chan error, 1)
	go func() { firstDone <- bundle.FinishSuite(context.Background(), result) }()
	<-ready
	different, err := cloneMeetingResult(result)
	if err != nil {
		t.Fatal(err)
	}
	different.Tasks[0].Notes["drift"] = "different exact result"
	if err := bundle.FinishSuite(context.Background(), different); err == nil ||
		!strings.Contains(err.Error(), "differs") {
		t.Fatalf("different concurrent FinishSuite() = %v", err)
	}
	close(block)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestMeetingReviewBundleCloseAbandonsOwnedResourcesIdempotently(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "abandoned")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	if err := bundle.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	if _, err := bundle.Receipt(); err == nil {
		t.Fatal("abandoned bundle returned a success receipt")
	}
	if _, err := bundle.BeginAttempt(context.Background(), fixtureMeetingEvidenceAttempt(
		Suite()[0], bench.Result{Cell: ReferenceCell()},
	)); err == nil {
		t.Fatal("abandoned bundle admitted a new attempt")
	}
	if _, err := os.Stat(filepath.Join(directory, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("abandoned bundle published a manifest: %v", err)
	}
}

func TestMeetingReviewBundleFinishAfterCloseReturnsAbortedWithoutWaiting(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "finish-after-close")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	if err := bundle.Close(); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks()}
	result.Finish()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- bundle.FinishSuite(ctx, result) }()
	select {
	case finishErr := <-done:
		if !errors.Is(finishErr, errMeetingReviewBundleAborted) {
			t.Fatalf("FinishSuite after Close = %v, want aborted", finishErr)
		}
	case <-ctx.Done():
		t.Fatal("FinishSuite waited on an unobservable drain after Close")
	}
}

func TestMeetingReviewBundleConcurrentCloseMakesFinishReturnAbortedWithoutWaiting(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "finish-concurrent-close")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	closeDetached, releaseClose := make(chan struct{}), make(chan struct{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
		beforeAbandonRootClose: func() {
			close(closeDetached)
			<-releaseClose
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	closeDone := make(chan error, 1)
	go func() { closeDone <- bundle.Close() }()
	select {
	case <-closeDetached:
	case <-time.After(time.Second):
		t.Fatal("Close did not publish its terminal transition")
	}
	result := bench.Result{Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks()}
	result.Finish()
	finishDone := make(chan error, 1)
	go func() { finishDone <- bundle.FinishSuite(context.Background(), result) }()
	close(releaseClose)
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("concurrent Close = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent Close hung")
	}
	select {
	case err := <-finishDone:
		if !errors.Is(err, errMeetingReviewBundleAborted) {
			t.Fatalf("FinishSuite concurrent with Close = %v, want aborted", err)
		}
	case <-time.After(time.Second):
		t.Fatal("FinishSuite waited after concurrent Close")
	}
}

func TestMeetingReviewMarkdownEscapesActiveContentAndVerifierRerenders(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "markdown")
	reviewer := &fixtureMeetingReviewer{summary: `<img src=x onerror=alert(1)> [load](https://invalid.example) *bold*`}
	lease := openFixtureMeetingReviewer(t, reviewer)
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, Reviewer: lease})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := completeFixtureMeetingReviewAttempts(t, bundle)
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	receipt, err := bundle.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	reviewPath := filepath.Join(directory, "REVIEW.md")
	reviewPayload, err := os.ReadFile(reviewPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(reviewPayload, []byte("<img")) ||
		bytes.Contains(reviewPayload, []byte("](https://invalid.example)")) {
		t.Fatalf("active reviewer Markdown was retained:\n%s", reviewPayload)
	}
	manifest := readMeetingReviewManifest(t, directory)
	forgedReview := []byte("# forged but self-consistent review\n")
	manifest.ReviewDocument.SHA256 = reviewDigest(forgedReview)
	manifest.ReviewDocument.SizeBytes = int64(len(forgedReview))
	manifestPayload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	manifestPayload = append(manifestPayload, '\n')
	for path, payload := range map[string][]byte{
		reviewPath: forgedReview, filepath.Join(directory, "manifest.json"): manifestPayload,
	} {
		if err := os.Chmod(path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := VerifyReviewBundle(directory, reviewDigest(manifestPayload)); err == nil {
		t.Fatal("self-consistent noncanonical human review was accepted")
	}
	if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err == nil {
		t.Fatal("original receipt accepted forged human review")
	}
}

func TestMeetingReviewSecretRedactionHandlesOverlapEscapesAndFieldSplits(t *testing.T) {
	secrets, err := canonicalReviewSecrets([]string{
		"abcdefgh", "abcdefghijkl", `quoted-"token"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	contextValue := meetingReviewContext{
		Outcome: bench.TaskOutcome{Error: `abcdefghijkl quoted-"token"`},
		Transcript: bench.Transcript{Moments: []bench.Moment{{
			Text: "abcdefgh", Arguments: `abcdefghijkl`,
		}}},
	}
	redactMeetingReviewContext(&contextValue, secrets)
	payload, err := json.Marshal(contextValue)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, []byte("abcdefghijkl")) || bytes.Contains(payload, []byte("[REDACTED]ijkl")) ||
		containsReviewSecret(secrets, payload) || !bytes.Contains(payload, []byte("[REDACTED]")) {
		t.Fatalf("overlap-safe redaction = %s", payload)
	}
	escaped := []byte(`{"value":"quoted-\"token\""}`)
	if !containsReviewSecret(secrets, escaped) {
		t.Fatal("JSON-escaped declared secret was not detected")
	}
	split := []byte(`{"left":"abcdefgh","right":"ijkl"}`)
	longOnly, err := canonicalReviewSecrets([]string{"abcdefghijkl"})
	if err != nil || !containsReviewSecret(longOnly, split) {
		t.Fatal("sole long secret split across JSON fields was not detected")
	}
	reversedKeyOrder := []byte(`{"z":"abcdefgh","a":"ijkl"}`)
	if !containsReviewSecret(longOnly, reversedKeyOrder) {
		t.Fatal("wire-ordered secret fragments were lost to object-key ordering")
	}
	for _, benign := range [][]byte{
		[]byte(`{"abcdefgh":"benign","ijkl":"values"}`),
		[]byte(`{"z":"abcdefgh","separator":0,"a":"ijkl"}`),
		[]byte(`{"outer":{"z":"abcdefgh"},"a":"ijkl"}`),
		[]byte(`{"z":"ijkl","a":"abcdefgh"}`),
	} {
		if containsReviewSecret(longOnly, benign) {
			t.Fatalf("benign JSON produced a split-secret false positive: %s", benign)
		}
	}
	if _, err := canonicalReviewSecrets([]string{"short"}); err == nil {
		t.Fatal("short declared secret was accepted")
	}
}

func TestAlignedMeetingAudioEndRoundsUpToExact24kHzBoundary(t *testing.T) {
	for _, test := range []struct {
		name    string
		capture bench.SessionAudioCapture
		want    float64
	}{
		{name: "three-frames", capture: bench.SessionAudioCapture{RoomPCM16: make([]int16, 3)}, want: 0.125},
		{name: "five-frames", capture: bench.SessionAudioCapture{RoomPCM16: make([]int16, 5)}, want: 0.25},
		{name: "agent-extends", capture: bench.SessionAudioCapture{
			RoomPCM16: []int16{1}, Agent: []bench.TimedAudioChunk{{AtMS: 1, PCM16: make([]int16, 3)}},
		}, want: 1.125},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := alignedMeetingAudioEnd(test.capture)
			if err != nil || got != test.want {
				t.Fatalf("aligned end = %.3f, %v; want %.3f", got, err, test.want)
			}
		})
	}
}

func TestMeetingAttemptEndAdvancesPastLatestVideoAndPadsPostPlaybackQuiet(t *testing.T) {
	endUS, err := meetingAttemptEndUS(10_000, 100_000, true)
	if err != nil || endUS != 101_000 {
		t.Fatalf("attempt end = %dus, %v; want 101000us", endUS, err)
	}
	capture := bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: make([]int16, 240),
	}
	capture.RoomPCM16[0] = 123
	padded, err := padMeetingAudioCapture(capture, endUS)
	if err != nil || len(padded.RoomPCM16) != 2424 {
		t.Fatalf("padded room frames = %d, %v; want 2424", len(padded.RoomPCM16), err)
	}
	for index, sample := range padded.RoomPCM16[240:] {
		if sample != 0 {
			t.Fatalf("post-playback quiet frame %d = %d", index+240, sample)
		}
	}
	if _, err := meetingAttemptEndUS(10_000, 100_125, true); err != nil {
		t.Fatalf("125us-boundary video PTS rejected: %v", err)
	}
	boundary, _ := meetingAttemptEndUS(10_000, 100_125, true)
	if boundary != 101_125 {
		t.Fatalf("boundary attempt end = %dus, want 101125us", boundary)
	}
}

func TestMeetingRequiredAVCellWithoutFullDecodedVideoIsDiagnostic(t *testing.T) {
	requirement, graph := fixtureMeetingGraphRequirement(t)
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
		Scope: Suite()[0].ID, Graph: cloneGraphEvidence(graph),
	})
	if err != nil {
		t.Fatal(err)
	}
	cell := ReferenceCell()
	cell.Execution = requirement
	outcome := bench.TaskOutcome{ID: Suite()[0].ID, Completed: true, Passed: true, Execution: &evidence}
	origin := EvidenceRunOrigin{
		Kind: EvidenceOriginProduction, Live: true, Transport: bench.TransportWebSocket,
		EndpointSHA256: meetingEndpointIdentity("wss://production.invalid/v1/realtime"),
	}
	if reportable, note := meetingAttemptReportability(origin, requirement, outcome, cell, false); reportable || !strings.Contains(note, "full-decoded screen video") {
		t.Fatalf("audio-only A/V reportability = %t, %q", reportable, note)
	}
	if reportable, note := meetingAttemptReportability(origin, requirement, outcome, cell, true); !reportable || note != "" {
		t.Fatalf("full-decoded A/V reportability = %t, %q", reportable, note)
	}
}

func TestMeetingTranscriptExecutionErrorAndScopeMustExactlyMatchOutcome(t *testing.T) {
	evidence := fixtureMeetingExecutionEvidence(t, Suite()[0].ID)
	outcome := bench.TaskOutcome{
		ID: Suite()[0].ID, Execution: &evidence, ExecutionError: "exact diagnostic",
	}
	transcript := bench.Transcript{Execution: &evidence, ExecutionError: "exact diagnostic"}
	if !meetingTranscriptExecutionMatchesOutcome(transcript, outcome) {
		t.Fatal("exact transcript execution evidence was rejected")
	}
	driftedError := transcript
	driftedError.ExecutionError = "different diagnostic"
	if meetingTranscriptExecutionMatchesOutcome(driftedError, outcome) {
		t.Fatal("drifted transcript execution error was accepted")
	}
	driftedScope := transcript
	copy := evidence
	copy.Scope = Suite()[1].ID
	driftedScope.Execution = &copy
	if meetingTranscriptExecutionMatchesOutcome(driftedScope, outcome) {
		t.Fatal("cross-case transcript execution scope was accepted")
	}
}

func TestMeetingReviewTimelineUsesExactMicrosecondBoundary(t *testing.T) {
	zero, one, two := int64(0), int64(1), int64(2)
	assessment := func(at *int64) revieweval.Assessment {
		return revieweval.Assessment{
			SignificantProblems: []revieweval.Finding{{Category: "timing", StartMS: at,
				Evidence: "fixture", Impact: "fixture"}},
			MinorObservations: []revieweval.Finding{}, Limitations: []string{},
		}
	}
	if meetingAssessmentBeyondMedia(assessment(&zero), 125) {
		t.Fatal("zero-millisecond finding exceeded a 125us recording")
	}
	if !meetingAssessmentBeyondMedia(assessment(&one), 125) {
		t.Fatal("one-millisecond finding was admitted after a 125us recording")
	}
	if meetingAssessmentBeyondMedia(assessment(&one), 1_000) {
		t.Fatal("one-millisecond finding was rejected at an exact 1ms endpoint")
	}
	if !meetingAssessmentBeyondMedia(assessment(&two), 1_999) {
		t.Fatal("two-millisecond finding was admitted after a 1.999ms recording")
	}
	for _, test := range []struct {
		endUS int64
		want  int64
	}{
		{endUS: 1_000, want: 1},
		{endUS: 1_999, want: 1},
		{endUS: 2_000, want: 2},
	} {
		got, err := meetingFindingTimestampMaximumMS(test.endUS)
		if err != nil || got != test.want {
			t.Fatalf("finding timestamp maximum for %dus = %d, %v; want %d",
				test.endUS, got, err, test.want)
		}
	}
	for _, invalid := range []int64{-1, 0, 999} {
		if _, err := meetingFindingTimestampMaximumMS(invalid); err == nil {
			t.Fatalf("finding timestamp maximum admitted %dus", invalid)
		}
	}
}

func BenchmarkMeetingReviewBundleFourCases(b *testing.B) {
	parent := b.TempDir()
	b.ReportAllocs()
	b.ReportMetric(float64(ExpectedTasks()), "cases/op")
	for iteration := range b.N {
		directory := filepath.Join(parent, fmt.Sprintf("bundle-%08d", iteration))
		lease := newFixtureMeetingReviewerLease(b, &fixtureMeetingReviewer{})
		bundle, err := NewReviewBundle(ReviewBundleOptions{
			Directory: directory, Reviewer: lease,
		})
		if err != nil {
			b.Fatal(err)
		}
		result := bench.Result{Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks()}
		for index, task := range Suite() {
			specification := fixtureMeetingEvidenceAttempt(task, result)
			attempt, err := bundle.BeginAttempt(b.Context(), specification)
			if err != nil {
				b.Fatal(err)
			}
			if err := attempt.CaptureAudio(fixtureMeetingAudio(index)); err != nil {
				b.Fatal(err)
			}
			outcome := bench.TaskOutcome{ID: task.ID, Completed: true, Passed: true}
			if err := attempt.Complete(b.Context(), EvidenceCompletion{
				Attempt: specification, Outcome: outcome,
			}); err != nil {
				b.Fatal(err)
			}
			result.Tasks = append(result.Tasks, outcome)
		}
		result.Finish()
		if err := bundle.FinishSuite(b.Context(), result); err != nil {
			b.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		makeMeetingReviewTreeWritable(directory)
		_ = os.RemoveAll(directory)
		b.StartTimer()
	}
}

func TestMeetingReviewBundleFourCaseAllocationThreshold(t *testing.T) {
	if meetingRaceEnabled {
		t.Skip("allocation budget is measured without race-detector instrumentation")
	}
	const helperEnvironment = "OPENREALTIME_MEETING_ALLOCATION_HELPER"
	switch os.Getenv(helperEnvironment) {
	case "":
		// AllocsPerRun reads process-global runtime counters. Run the measurement
		// in a fresh copy of this exact test binary so delayed cleanup exercised
		// by earlier adversarial tests cannot be charged to this workload.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0],
			"-test.run=^TestMeetingReviewBundleFourCaseAllocationThreshold$", "-test.count=1")
		command.Env = append(os.Environ(), helperEnvironment+"=1")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated four-case allocation gate: %v\n%s", err, output)
		}
		return
	case "1":
		// Continue with the isolated measurement below.
	default:
		t.Fatalf("invalid %s value", helperEnvironment)
	}
	parent := t.TempDir()
	iteration := 0
	allocations := testing.AllocsPerRun(3, func() {
		iteration++
		directory := filepath.Join(parent, fmt.Sprintf("allocation-%02d", iteration))
		lease := newFixtureMeetingReviewerLease(t, &fixtureMeetingReviewer{})
		bundle, err := NewReviewBundle(ReviewBundleOptions{
			Directory: directory, Reviewer: lease,
		})
		if err != nil {
			panic(err)
		}
		result := bench.Result{Suite: SuiteName, Cell: ReferenceCell(), Expected: ExpectedTasks()}
		for index, task := range Suite() {
			specification := fixtureMeetingEvidenceAttempt(task, result)
			attempt, err := bundle.BeginAttempt(context.Background(), specification)
			if err != nil {
				panic(err)
			}
			if err := attempt.CaptureAudio(fixtureMeetingAudio(index)); err != nil {
				panic(err)
			}
			outcome := bench.TaskOutcome{ID: task.ID, Completed: true, Passed: true}
			if err := attempt.Complete(context.Background(), EvidenceCompletion{
				Attempt: specification, Outcome: outcome,
			}); err != nil {
				panic(err)
			}
			result.Tasks = append(result.Tasks, outcome)
		}
		result.Finish()
		if err := bundle.FinishSuite(context.Background(), result); err != nil {
			panic(err)
		}
		if err := lease.Close(); err != nil {
			panic(err)
		}
		makeMeetingReviewTreeWritable(directory)
		_ = os.RemoveAll(directory)
	})
	// This includes four full shared review Evaluate+Verify calls, staged
	// publication hidden under non-reportable names, create-only external
	// receipt publication/reopen before atomic promotion, retention of every
	// provider exchange artifact, media reverification, and exact-tree sealing.
	// The crash-recoverable receipt-before-visibility contract has a stable
	// 172,567..172,569 allocation baseline on the reference toolchain. Preserve
	// about seven percent headroom while still catching a material regression in
	// this expanded security boundary.
	const maximumAllocations = 185_000
	if allocations > maximumAllocations {
		t.Fatalf("four-case review bundle allocations = %.0f, threshold %d", allocations, maximumAllocations)
	}
}

type hermeticMeetingHarness struct {
	t        testing.TB
	mu       sync.Mutex
	current  *hermeticMeetingEpisode
	graph    bench.GraphEvidence
	failCase string
}

func newHermeticMeetingHarness(t testing.TB, graph bench.GraphEvidence) *hermeticMeetingHarness {
	t.Helper()
	return &hermeticMeetingHarness{t: t, graph: graph}
}

func (harness *hermeticMeetingHarness) dependencies() *runDependencies {
	return &runDependencies{
		newEnvironment: func(context.Context, EnvironmentConfig) (meetingRunEnvironment, error) {
			return meetingRunEnvironment{
				episode: func(_ context.Context, task Task) (meetingRunEpisode, error) {
					episode := newHermeticMeetingEpisode(harness.t, task)
					harness.mu.Lock()
					harness.current = episode
					harness.mu.Unlock()
					return meetingRunEpisode{
						ready: episode.ready, started: episode.startedAt, surface: episode.surface,
						captureScreen: episode.captureScreen, result: episode.result,
					}, nil
				},
				close: func() error { return nil },
			}, nil
		},
		playSamples: harness.play,
		now: func() time.Time {
			harness.mu.Lock()
			episode := harness.current
			harness.mu.Unlock()
			if episode == nil {
				return time.Unix(0, 0)
			}
			return episode.clock.nowTime()
		},
	}
}

func (harness *hermeticMeetingHarness) play(
	ctx context.Context, config bench.SessionConfig, _ []int16,
) (bench.Transcript, error) {
	harness.mu.Lock()
	episode := harness.current
	harness.mu.Unlock()
	if episode == nil {
		return bench.Transcript{}, errors.New("hermetic meeting episode is missing")
	}
	if err := config.Ready(ctx); err != nil {
		return bench.Transcript{}, err
	}
	frame, err := config.Video[0].Capture(ctx)
	if err != nil {
		return bench.Transcript{}, err
	}
	if config.CaptureVideo != nil {
		if err := config.CaptureVideo(bench.SessionVideoCapture{
			Source: "screen", Width: 1200, Height: 700, MediaType: "image/png",
			WireTimestamp: 1, EpisodeAtMS: 1, Data: frame,
		}); err != nil {
			return bench.Transcript{}, err
		}
	}
	transcript := bench.Transcript{Moments: []bench.Moment{
		{AtMS: 1, Kind: bench.MomentReady},
		{AtMS: 1, Kind: bench.MomentVideoFrame, Source: "screen"},
	}}
	call := func(atMS float64, name string, x int) error {
		episode.clock.setMS(atMS)
		arguments := json.RawMessage(fmt.Sprintf(
			`{"source":"screen","x":%d,"y":500,"button":"left"}`, x,
		))
		_, err := config.HandleTool(ctx, bench.ToolRequest{
			CallID: fmt.Sprintf("%s-%s", episode.task.ID, name), Name: name, Arguments: arguments,
		})
		transcript.Moments = append(transcript.Moments, bench.Moment{
			AtMS: atMS, Kind: bench.MomentToolCall, Name: name, Arguments: string(arguments),
		})
		return err
	}
	click := func(atMS float64, x int) error {
		return call(atMS, computeruse.ClickNormalized, x)
	}
	switch episode.task.ID {
	case "open-share-present":
		if err := click(2_000, 100); err != nil {
			return transcript, err
		}
		if err := click(2_100, 250); err != nil {
			return transcript, err
		}
		if err := call(2_200, ToolReadLaunchReview, 0); err != nil {
			return transcript, err
		}
		transcript.Moments = append(transcript.Moments,
			bench.Moment{AtMS: 2_300, Kind: bench.MomentAgentText, Text: "The grounded value is 18.4%."},
			bench.Moment{AtMS: 2_400, Kind: bench.MomentResponseDone})
	case "follow-up-during-analysis":
		episode.clock.setMS(1_000)
		analysis := make(chan error, 1)
		go func() {
			_, err := config.HandleTool(ctx, bench.ToolRequest{
				CallID: "analysis", Name: ToolAnalyzeLaunchReview, Arguments: json.RawMessage(`{}`),
			})
			analysis <- err
		}()
		<-episode.clock.observed
		if err := click(9_000, 450); err != nil {
			return transcript, err
		}
		if err := <-analysis; err != nil {
			return transcript, err
		}
	case "visual-alert-during-presentation":
		if err := click(10_600, 650); err != nil {
			return transcript, err
		}
		transcript.Moments = append(transcript.Moments,
			bench.Moment{AtMS: 10_000, Kind: bench.MomentAgentAudio, AudioMS: 400},
			bench.Moment{AtMS: 10_700, Kind: bench.MomentAgentAudio, AudioMS: 400})
	case "spoken-navigation-correction":
		if err := click(3_000, 800); err != nil {
			return transcript, err
		}
		if harness.failCase != episode.task.ID {
			if err := click(9_000, 950); err != nil {
				return transcript, err
			}
		}
	default:
		return transcript, errors.New("unknown hermetic meeting case")
	}
	if config.CaptureAudio != nil {
		if err := config.CaptureAudio(fixtureMeetingAudio(0)); err != nil {
			return transcript, err
		}
	}
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
		Scope: episode.task.ID, Graph: cloneGraphEvidence(harness.graph),
	})
	if err != nil {
		return transcript, err
	}
	transcript.Execution = &evidence
	return transcript, nil
}

type hermeticMeetingEpisode struct {
	task    Task
	clock   *hermeticMeetingClock
	surface *hermeticMeetingSurface
	frame   []byte
}

func newHermeticMeetingEpisode(t testing.TB, task Task) *hermeticMeetingEpisode {
	t.Helper()
	clock := &hermeticMeetingClock{started: time.Unix(1, 0), observed: make(chan struct{})}
	surface := &hermeticMeetingSurface{clock: clock}
	// The retained bytes must match the same viewport geometry advertised to
	// the production SessionConfig; a tiny structural PNG is insufficient for
	// the real encoder/full-decode gate.
	imageValue := image.NewRGBA(image.Rect(0, 0, 1200, 700))
	imageValue.Set(0, 0, color.RGBA{R: 0x33, G: 0x66, B: 0x99, A: 0xff})
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, imageValue); err != nil {
		t.Fatal(err)
	}
	return &hermeticMeetingEpisode{task: task, clock: clock, surface: surface, frame: buffer.Bytes()}
}

func (episode *hermeticMeetingEpisode) ready(context.Context) error {
	episode.clock.setMS(0)
	return nil
}

func (episode *hermeticMeetingEpisode) startedAt() time.Time { return episode.clock.started }
func (episode *hermeticMeetingEpisode) captureScreen(context.Context) ([]byte, error) {
	return slices.Clone(episode.frame), nil
}
func (episode *hermeticMeetingEpisode) result(context.Context) (PageResult, error) {
	episode.surface.mu.Lock()
	defer episode.surface.mu.Unlock()
	return PageResult{Events: slices.Clone(episode.surface.events), State: "complete"}, nil
}

type hermeticMeetingClock struct {
	mu       sync.Mutex
	started  time.Time
	now      time.Time
	observed chan struct{}
	once     sync.Once
}

func (clock *hermeticMeetingClock) setMS(value float64) {
	clock.mu.Lock()
	clock.now = clock.started.Add(time.Duration(value * float64(time.Millisecond)))
	clock.mu.Unlock()
}
func (clock *hermeticMeetingClock) nowTime() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.once.Do(func() { close(clock.observed) })
	return clock.now
}

type hermeticMeetingSurface struct {
	mu     sync.Mutex
	clock  *hermeticMeetingClock
	events []PageEvent
}

func (*hermeticMeetingSurface) Name() string                               { return "hermetic-meeting-surface" }
func (*hermeticMeetingSurface) Viewport(context.Context) (int, int, error) { return 1200, 700, nil }
func (surface *hermeticMeetingSurface) Click(_ context.Context, x, _ int, _ string) error {
	name := ""
	switch {
	case x < 200:
		name = "document-opened"
	case x < 400:
		name = "screen-shared"
	case x < 650:
		name = "risks-slide"
	case x < 850:
		name = "alert-acknowledged"
	case x < 1100:
		name = "summary-slide"
	default:
		name = "overview-slide"
	}
	atMS := elapsedMS(surface.clock.started, surface.clock.nowTime())
	surface.mu.Lock()
	surface.events = append(surface.events, PageEvent{Name: name, AtMS: atMS})
	surface.mu.Unlock()
	return nil
}
func (*hermeticMeetingSurface) DoubleClick(context.Context, int, int) error      { return nil }
func (*hermeticMeetingSurface) Move(context.Context, int, int) error             { return nil }
func (*hermeticMeetingSurface) Drag(context.Context, int, int, int, int) error   { return nil }
func (*hermeticMeetingSurface) Type(context.Context, string) error               { return nil }
func (*hermeticMeetingSurface) Key(context.Context, []string) error              { return nil }
func (*hermeticMeetingSurface) Scroll(context.Context, int, int, int, int) error { return nil }
func (*hermeticMeetingSurface) Screenshot(context.Context) error                 { return nil }

func fixtureMeetingGraphRequirement(
	t testing.TB,
) (bench.ExecutionRequirement, bench.GraphEvidence) {
	t.Helper()
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	graph := bench.GraphEvidence{
		Graph: bench.GraphIdentity{
			FormatVersion: ir.FormatVersion, ID: "meeting_assistant", Revision: 1, Fingerprint: digest,
		},
		Configuration: bench.ArtifactIdentity{ID: "config://meeting-fixture", Digest: digest},
		Nodes: []bench.GraphNodeEvidence{{
			Node:           "foreground",
			Element:        element.Identity{Name: "meeting.FixtureForeground", Revision: 1, Digest: digest},
			Implementation: "fixture.meeting.foreground.v1",
			Config:         bench.ArtifactIdentity{ID: "config://meeting-fixture/foreground", Digest: digest},
			Runtime:        bench.ArtifactIdentity{ID: "runtime://meeting-fixture/foreground", Revision: "1"},
		}},
	}
	requirement := bench.ExecutionRequirement{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
		Graph: cloneGraphEvidence(graph),
	}
	if err := requirement.Validate(); err != nil {
		t.Fatal(err)
	}
	return requirement, graph
}

func cloneGraphEvidence(source bench.GraphEvidence) *bench.GraphEvidence {
	result := source
	result.Nodes = slices.Clone(source.Nodes)
	for index := range result.Nodes {
		result.Nodes[index].Capabilities = slices.Clone(source.Nodes[index].Capabilities)
	}
	result.Paths = slices.Clone(source.Paths)
	return &result
}

func fixtureMeetingAudio(index int) bench.SessionAudioCapture {
	room := make([]int16, 240+index*3)
	room[0] = 100
	return bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: room,
		Agent: []bench.TimedAudioChunk{{AtMS: 1, PCM16: []int16{200, -200, 100}}},
	}
}

func readMeetingReviewManifest(t testing.TB, directory string) ReviewManifest {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest ReviewManifest
	if err := json.Unmarshal(payload, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest
}

func makeMeetingReviewTreeRemovable(t testing.TB, directory string) {
	t.Helper()
	t.Cleanup(func() { makeMeetingReviewTreeWritable(directory) })
}

func rewriteMeetingReviewJSON(t testing.TB, path string, value any) []byte {
	t.Helper()
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, '\n')
	rewriteSealedMeetingPayload(t, path, payload)
	return payload
}

func rewriteSealedMeetingPayload(t testing.TB, path string, payload []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
}

func makeMeetingReviewTreeWritable(directory string) {
	_ = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
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

func boolFloat(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
