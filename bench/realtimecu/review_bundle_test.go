package realtimecu

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	revieweval "github.com/bojieli/OpenRealtime/bench/review"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
)

type fixtureReviewVideoFactory struct {
	calls atomic.Int32
}

type fixtureCUReviewer struct {
	mu           sync.Mutex
	tokens       map[*fixtureCUReviewToken]string
	requests     []revieweval.PreparedRequest
	claimed      atomic.Bool
	closed       atomic.Bool
	next         atomic.Uint64
	active       atomic.Int32
	maximum      atomic.Int32
	delay        time.Duration
	finding      *int64
	failCase     string
	failed       atomic.Bool
	calls        atomic.Int32
	beforeReview func() error
}

type fixtureCUReviewToken struct{ sequence uint64 }

type failAfterSourceReceiptPublishAnchor struct {
	FileReviewSourceReceiptAnchor
	failed atomic.Bool
}

type failAfterSourceReceiptPublishLease struct {
	ReviewSourceReceiptLease
	failed *atomic.Bool
}

type failOnceEvaluationPublicationCloseFactory struct {
	base   FileReviewEvaluationReceiptStoreFactory
	failed atomic.Bool
}

type failOnceEvaluationPublicationCloseStore struct {
	revieweval.EvaluationBundleReceiptStore
	failed *atomic.Bool
}

type failOnceEvaluationPublicationCloseLease struct {
	revieweval.EvaluationBundleReceiptLease
	failed *atomic.Bool
}

func (factory *failOnceEvaluationPublicationCloseFactory) Store(
	caseID, finalDirectory string,
) (revieweval.EvaluationBundleReceiptStore, error) {
	store, err := factory.base.Store(caseID, finalDirectory)
	if err != nil {
		return nil, err
	}
	return &failOnceEvaluationPublicationCloseStore{
		EvaluationBundleReceiptStore: store, failed: &factory.failed,
	}, nil
}

func (factory *failOnceEvaluationPublicationCloseFactory) Quarantine(
	caseID, finalDirectory string,
) (string, error) {
	return factory.base.Quarantine(caseID, finalDirectory)
}

func (store *failOnceEvaluationPublicationCloseStore) Acquire(
	ctx context.Context,
) (revieweval.EvaluationBundleReceiptLease, error) {
	lease, err := store.EvaluationBundleReceiptStore.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	return &failOnceEvaluationPublicationCloseLease{
		EvaluationBundleReceiptLease: lease, failed: store.failed,
	}, nil
}

func (lease *failOnceEvaluationPublicationCloseLease) Close() error {
	closeErr := lease.EvaluationBundleReceiptLease.Close()
	if lease.failed.CompareAndSwap(false, true) {
		return errors.Join(closeErr, errors.New("fixture evaluation publication close failed"))
	}
	return closeErr
}

func (anchor *failAfterSourceReceiptPublishAnchor) Acquire(
	ctx context.Context, publicationID string,
) (ReviewSourceReceiptLease, error) {
	lease, err := anchor.FileReviewSourceReceiptAnchor.Acquire(ctx, publicationID)
	if err != nil {
		return nil, err
	}
	return &failAfterSourceReceiptPublishLease{
		ReviewSourceReceiptLease: lease, failed: &anchor.failed,
	}, nil
}

func (lease *failAfterSourceReceiptPublishLease) Publish(
	ctx context.Context, receipt ReviewSourceReceipt,
) error {
	if err := lease.ReviewSourceReceiptLease.Publish(ctx, receipt); err != nil {
		return err
	}
	if lease.failed.CompareAndSwap(false, true) {
		return errors.New("fixture process stopped after durable source receipt")
	}
	return nil
}

var fixtureCUReviewerImplementation = []byte("openrealtime realtime-cu reviewer fixture v1")
var fixtureCUReviewerConfiguration = []byte(`{"mode":"hermetic"}`)

func TestCanonicalReportabilityPreservesOmittedEmptyWireForm(t *testing.T) {
	t.Parallel()

	for _, source := range [][]string{nil, {}, {"", "  "}} {
		if got := canonicalReportability(source); got != nil {
			t.Fatalf("canonical empty reportability = %#v, want nil", got)
		}
	}

	got := canonicalReportability([]string{" beta ", "alpha", "beta", ""})
	want := []string{"alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical reportability = %#v, want %#v", got, want)
	}
}

func fixtureCUReviewerCapabilities() revieweval.ProviderCapabilities {
	return revieweval.ProviderCapabilities{
		MediaTypes:        []string{"audio/wav", "video/mp4"},
		MaximumMediaCount: 4, MaximumMediaBytes: 128 << 20,
	}
}

func fixtureCUReviewerDescriptor() revieweval.ProviderDescriptor {
	capabilitiesSHA, err := fixtureCUReviewerCapabilities().SHA256()
	if err != nil {
		panic(err)
	}
	return revieweval.ProviderDescriptor{
		Provider: "fixture", Model: "realtime-cu-review-fixture", API: "fixture.review",
		APIRevision: "v1",
		Implementation: revieweval.ContentIdentity{
			Version: "fixture.realtime-cu-review.v1",
			SHA256:  reviewDigest(fixtureCUReviewerImplementation),
		},
		ConfigurationSHA256: reviewDigest(fixtureCUReviewerConfiguration),
		CapabilitiesSHA256:  capabilitiesSHA,
	}
}

func (*fixtureCUReviewer) Descriptor() revieweval.ProviderDescriptor {
	return fixtureCUReviewerDescriptor()
}

func (*fixtureCUReviewer) Capabilities() revieweval.ProviderCapabilities {
	return fixtureCUReviewerCapabilities()
}

func (*fixtureCUReviewer) Implementation() []byte {
	return slices.Clone(fixtureCUReviewerImplementation)
}

func (*fixtureCUReviewer) Configuration() []byte {
	return slices.Clone(fixtureCUReviewerConfiguration)
}

func (reviewer *fixtureCUReviewer) Claim() error {
	if !reviewer.claimed.CompareAndSwap(false, true) {
		return errors.New("fixture realtime-cu reviewer was already claimed")
	}
	return nil
}

func (reviewer *fixtureCUReviewer) Review(
	ctx context.Context, request revieweval.PreparedRequest,
) (revieweval.ProviderResponse, error) {
	reviewer.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return revieweval.ProviderResponse{}, err
	}
	if reviewer.beforeReview != nil {
		if err := reviewer.beforeReview(); err != nil {
			return revieweval.ProviderResponse{}, err
		}
	}
	if request.Suite != SuiteName || request.Trial != 1 || len(request.Media) < 2 ||
		request.Media[0].Kind != "audio" || request.Media[0].MediaType != "audio/wav" {
		return revieweval.ProviderResponse{}, errors.New("fixture reviewer received a drifted request")
	}
	var deterministic realtimeCUReviewContext
	if err := json.Unmarshal(request.Context, &deterministic); err != nil {
		return revieweval.ProviderResponse{}, err
	}
	if deterministic.ResultSHA256 == "" || deterministic.Cell.Name == "" ||
		deterministic.Provenance.FinishedAt == "" ||
		deterministic.Case != request.Case || deterministic.Outcome.ID != request.Case {
		return revieweval.ProviderResponse{}, errors.New("fixture reviewer context lacks final run identity")
	}
	if request.Case == reviewer.failCase && reviewer.failed.CompareAndSwap(false, true) {
		return revieweval.ProviderResponse{}, errors.New("fixture reviewer transient outage")
	}
	observed := "fail"
	if deterministic.Outcome.Passed {
		observed = "pass"
	}
	problems := "[]"
	if reviewer.finding != nil {
		problems = fmt.Sprintf(
			`[{"category":"timing","start_ms":%d,"evidence":"The reviewer observed the event at %d ms, beyond the playable timeline.","impact":"The timestamp cannot be reviewed."}]`,
			*reviewer.finding, *reviewer.finding,
		)
	}
	output := json.RawMessage(fmt.Sprintf(`{
		"media_usable":true,
		"observed_outcome":%q,
		"agrees_with_deterministic":true,
		"confidence":0.9,
		"summary":"The synchronized audiovisual evidence and deterministic trace are consistent.",
		"significant_problems":%s,
		"minor_observations":[],
		"limitations":[]
	}`, observed, problems))
	token := &fixtureCUReviewToken{sequence: reviewer.next.Add(1)}
	reviewer.mu.Lock()
	if reviewer.tokens == nil {
		reviewer.tokens = make(map[*fixtureCUReviewToken]string)
	}
	reviewer.tokens[token] = request.RequestFingerprint
	reviewer.requests = append(reviewer.requests, request)
	reviewer.mu.Unlock()
	active := reviewer.active.Add(1)
	for {
		maximum := reviewer.maximum.Load()
		if active <= maximum || reviewer.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer reviewer.active.Add(-1)
	if reviewer.delay > 0 {
		select {
		case <-time.After(reviewer.delay):
		case <-ctx.Done():
			return revieweval.ProviderResponse{}, context.Cause(ctx)
		}
	}
	return revieweval.ProviderResponse{
		Raw: []byte(`{"fixture":"realtime-cu-review"}`), Output: output,
		ReportedModel: fixtureCUReviewerDescriptor().Model,
		RequestID:     "fixture-" + request.Case, RequestIDState: revieweval.ProviderRequestIDValue,
		Request: []byte(`{"store":false}`), VerificationEvidence: token,
	}, nil
}

func (reviewer *fixtureCUReviewer) VerifyResponse(
	ctx context.Context, request revieweval.PreparedRequest, response revieweval.ProviderResponse,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	token, ok := response.VerificationEvidence.(*fixtureCUReviewToken)
	if !ok || token == nil {
		return errors.New("fixture realtime-cu review has no private verification token")
	}
	reviewer.mu.Lock()
	want, exists := reviewer.tokens[token]
	delete(reviewer.tokens, token)
	reviewer.mu.Unlock()
	if !exists || want != request.RequestFingerprint {
		return errors.New("fixture realtime-cu review token replayed or drifted")
	}
	return nil
}

func (reviewer *fixtureCUReviewer) Close() error {
	if !reviewer.closed.CompareAndSwap(false, true) {
		return errors.New("fixture realtime-cu reviewer was closed twice")
	}
	return nil
}

func openFixtureCUReviewer(
	t testing.TB, reviewer *fixtureCUReviewer,
) *revieweval.ProviderLease {
	t.Helper()
	lease := newFixtureCUReviewerLease(t, reviewer)
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close fixture realtime-cu reviewer: %v", err)
		}
	})
	return lease
}

func newFixtureCUReviewerLease(
	t testing.TB, reviewer *fixtureCUReviewer,
) *revieweval.ProviderLease {
	t.Helper()
	registration := revieweval.Registration{
		Name: "realtime-cu-fixture", Descriptor: fixtureCUReviewerDescriptor(),
		Capabilities:   fixtureCUReviewerCapabilities(),
		Implementation: slices.Clone(fixtureCUReviewerImplementation),
		Configuration:  slices.Clone(fixtureCUReviewerConfiguration),
		Factory:        func(context.Context) (revieweval.Provider, error) { return reviewer, nil },
	}
	registry, err := revieweval.NewRegistry([]revieweval.Registration{registration})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(context.Background(), "realtime-cu-fixture")
	if err != nil {
		t.Fatal(err)
	}
	return lease
}

func (factory *fixtureReviewVideoFactory) NewReviewVideo(
	ctx context.Context, _ EvidenceAttempt,
) (reviewmedia.Encoder, reviewmedia.Attestor, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	factory.calls.Add(1)
	return newCUFixtureEncoder(), newCUFixtureAttestor(), nil
}

type cuFixtureEncoder struct {
	claimed atomic.Bool
	closed  atomic.Bool
}

func newCUFixtureEncoder() *cuFixtureEncoder { return &cuFixtureEncoder{} }

func (*cuFixtureEncoder) Descriptor() reviewmedia.EncoderDescriptor {
	implementation := []byte("realtime-cu fixture encoder implementation")
	configuration := []byte(`{"codec":"fixture-h264-aac"}`)
	return reviewmedia.EncoderDescriptor{
		Name: "realtime_cu_fixture_encoder", Version: "fixture_1",
		Implementation: revieweval.ContentIdentity{
			Version: "openrealtime.realtime-cu-fixture-encoder.v1",
			SHA256:  reviewDigest(implementation),
		},
		ConfigurationSHA256: reviewDigest(configuration),
	}
}

func (*cuFixtureEncoder) Implementation() []byte {
	return []byte("realtime-cu fixture encoder implementation")
}

func (*cuFixtureEncoder) Configuration() []byte {
	return []byte(`{"codec":"fixture-h264-aac"}`)
}

func (encoder *cuFixtureEncoder) Claim(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !encoder.claimed.CompareAndSwap(false, true) {
		return errors.New("fixture encoder was already claimed")
	}
	return nil
}

func (encoder *cuFixtureEncoder) Encode(
	ctx context.Context, request reviewmedia.EncodeRequest,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !encoder.claimed.Load() || encoder.closed.Load() {
		return errors.New("fixture encoder is not live")
	}
	payload := cuStructuralAVMP4(request.Source)
	file, err := os.OpenFile(request.OutputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (encoder *cuFixtureEncoder) Close() error {
	if !encoder.claimed.Load() || !encoder.closed.CompareAndSwap(false, true) {
		return errors.New("fixture encoder close lifecycle is invalid")
	}
	return nil
}

type cuFixtureAttestor struct {
	claimed atomic.Bool
	closed  atomic.Bool
}

func newCUFixtureAttestor() *cuFixtureAttestor { return &cuFixtureAttestor{} }

func (*cuFixtureAttestor) Descriptor() reviewmedia.AttestorDescriptor {
	implementation := []byte("realtime-cu independent fixture full decoder")
	configuration := []byte(`{"decoder":"fixture-full-decode"}`)
	return reviewmedia.AttestorDescriptor{
		Name: "realtime_cu_fixture_full_decoder", Version: "fixture_1",
		Capability: reviewmedia.FullDecodeAttestationCapability,
		Implementation: revieweval.ContentIdentity{
			Version: "openrealtime.realtime-cu-fixture-attestor.v1",
			SHA256:  reviewDigest(implementation),
		},
		ConfigurationSHA256: reviewDigest(configuration),
	}
}

func (*cuFixtureAttestor) Implementation() []byte {
	return []byte("realtime-cu independent fixture full decoder")
}

func (*cuFixtureAttestor) Configuration() []byte {
	return []byte(`{"decoder":"fixture-full-decode"}`)
}

func (attestor *cuFixtureAttestor) Claim(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !attestor.claimed.CompareAndSwap(false, true) {
		return errors.New("fixture attestor was already claimed")
	}
	return nil
}

func (attestor *cuFixtureAttestor) Attest(
	ctx context.Context, request reviewmedia.AttestationRequest,
) (reviewmedia.Attestation, error) {
	if err := ctx.Err(); err != nil {
		return reviewmedia.Attestation{}, err
	}
	if !attestor.claimed.Load() || attestor.closed.Load() {
		return reviewmedia.Attestation{}, errors.New("fixture attestor is not live")
	}
	spec := reviewmedia.PlayableSpec{
		OutputSHA256: request.ExpectedOutputSHA256,
		Container:    "mp4", VideoCodec: "h264", PixelFormat: "yuv420p",
		Width: request.Width, Height: request.Height,
		EncodedWidth: request.Width + request.Width%2, EncodedHeight: request.Height + request.Height%2,
		GeometryPolicy: reviewmedia.YUV420PPadRightBottomBlackToEvenV1,
		AudioCodec:     "aac", AudioSampleRateHz: 24_000, AudioChannels: 2,
		DurationUS: request.AttemptEndUS, AudioStartUS: 0, AudioEndUS: request.AttemptEndUS,
		VideoStartUS: request.FirstFrameUS, VideoEndUS: request.AttemptEndUS,
		VideoFrameCount:       request.ExpectedFrameCount,
		VideoFramePTSUSSHA256: request.ExpectedFramePTSUSSHA256,
	}
	report, err := json.Marshal(map[string]any{
		"decoder": "fixture", "full_decode": true,
		"output_sha256":       request.ExpectedOutputSHA256,
		"frame_pts_us_sha256": request.ExpectedFramePTSUSSHA256,
	})
	if err != nil {
		return reviewmedia.Attestation{}, err
	}
	return reviewmedia.Attestation{Spec: spec, Report: report}, nil
}

func (attestor *cuFixtureAttestor) Close() error {
	if !attestor.claimed.Load() || !attestor.closed.CompareAndSwap(false, true) {
		return errors.New("fixture attestor close lifecycle is invalid")
	}
	return nil
}

func TestReviewBundleRetainsDeterministicCaseAudioVideoAndRawEvidence(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	factory := &fixtureReviewVideoFactory{}
	bundle, err := NewReviewBundle(ReviewBundleOptions{Directory: directory, VideoFactory: factory})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (*ReviewBundle)(nil).SourceResult(); err == nil {
		t.Fatal("nil SourceResult succeeded")
	}
	if _, err := bundle.SourceResult(); err == nil {
		t.Fatal("unsealed SourceResult succeeded")
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	outcome := fixtureReviewAttempt(t, bundle, item, true)
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "retained 1 of 16") {
		t.Fatalf("FinishSuite() error = %v", err)
	}
	sealedResult, err := bundle.SourceResult()
	if err != nil || !reflect.DeepEqual(sealedResult, result) {
		t.Fatalf("SourceResult() = %+v, %v", sealedResult, err)
	}
	sealedResult.Tasks[0].Passed = !sealedResult.Tasks[0].Passed
	sealedAgain, err := bundle.SourceResult()
	if err != nil || !reflect.DeepEqual(sealedAgain, result) {
		t.Fatalf("SourceResult() did not return an isolated decode: %+v, %v", sealedAgain, err)
	}
	receipt, ok := bundle.Receipt()
	if !ok {
		t.Fatal("completed diagnostic bundle has no verified receipt")
	}
	manifest, err := VerifyReviewBundle(directory, receipt.ManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Complete || manifest.Reportable || manifest.CoreReportable ||
		len(manifest.Attempts) != 1 || len(manifest.Missing) != 15 || factory.calls.Load() != 1 {
		t.Fatalf("manifest = %+v factory calls=%d", manifest, factory.calls.Load())
	}
	attempt := manifest.Attempts[0]
	if !attempt.Deterministic.Passed || attempt.ReviewStatus != "not_configured" ||
		attempt.Reportable || len(attempt.Media) != 2 {
		t.Fatalf("attempt = %+v", attempt)
	}
	for _, path := range []string{
		attempt.Context.Path,
		attempt.Media[0].Path,
		attempt.Media[1].Path,
		filepath.ToSlash(filepath.Join(attempt.MediaBundle.Path, "frames/screen/000001.png")),
		filepath.ToSlash(filepath.Join(attempt.MediaBundle.Path, "screen.ffconcat")),
		filepath.ToSlash(filepath.Join(attempt.MediaBundle.Path, "screen.attestation.json")),
	} {
		if info, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(path))); err != nil ||
			!info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 {
			t.Fatalf("retained artifact %q info=%v error=%v", path, info, err)
		}
	}
	review, err := os.ReadFile(filepath.Join(directory, "REVIEW.md"))
	if err != nil || !bytes.Contains(review, []byte("static-control/pixel — PASS")) ||
		!bytes.Contains(review, []byte("Raw frames, timelines")) {
		t.Fatalf("REVIEW.md error=%v\n%s", err, review)
	}
	makeReviewTreeWritable(directory)
	if err := os.WriteFile(filepath.Join(directory, "result.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bundle.SourceResult(); err == nil {
		t.Fatal("SourceResult accepted a mutated sealed result")
	}
}

func TestReviewBundleRetainsAllIncompleteDeterministicRowsWithoutSummaryDrift(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	attempt, completion, _ := fixturePendingReviewAttempt(t, bundle, item, false)
	outcome := bench.TaskOutcome{
		ID: item.ID(), Completed: false, Error: "fixture session did not complete",
		Metrics: map[string]float64{}, Notes: map[string]string{},
	}
	completion.Outcome = outcome
	completion.Page = PageResult{Reason: "fixture session did not complete"}
	if err := attempt.Complete(t.Context(), completion); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if len(result.Summary.Distributions) != 0 || result.Summary.Failed != 1 {
		t.Fatalf("incomplete fixture summary=%+v", result.Summary)
	}
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "retained 1 of 16") {
		t.Fatalf("FinishSuite()=%v", err)
	}
	receipt, ok := bundle.Receipt()
	if !ok {
		t.Fatal("incomplete deterministic evidence has no diagnostic receipt")
	}
	manifest, err := VerifyReviewBundleReceipt(directory, receipt)
	if err != nil || len(manifest.Attempts) != 1 || manifest.Attempts[0].Deterministic.Completed ||
		manifest.Reportable || manifest.CoreReportable {
		t.Fatalf("incomplete deterministic manifest=%+v error=%v", manifest, err)
	}
}

func TestReviewBundleCanonicalizesHTMLBearingFailureContextForSecondaryReview(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	reviewer := &fixtureCUReviewer{}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:         openFixtureCUReviewer(t, reviewer),
		SourceAnchor:     fixtureReviewSourceAnchor(directory),
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	attempt, completion, _ := fixturePendingReviewAttempt(t, bundle, item, false)
	outcome := bench.TaskOutcome{
		ID: item.ID(), Completed: false,
		Error:   `<html><body>fixture provider failed</body></html>`,
		Metrics: map[string]float64{}, Notes: map[string]string{},
	}
	completion.Outcome = outcome
	completion.Page = PageResult{Reason: outcome.Error}
	if err := attempt.Complete(t.Context(), completion); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "retained 1 of 16") {
		t.Fatalf("FinishSuite() = %v, want only deterministic incompleteness", err)
	}
	if reviewer.calls.Load() != 1 {
		t.Fatalf("reviewer calls = %d, want 1", reviewer.calls.Load())
	}
	reviewer.mu.Lock()
	requests := slices.Clone(reviewer.requests)
	reviewer.mu.Unlock()
	if len(requests) != 1 ||
		bytes.Contains(requests[0].Context, []byte(`\u003c`)) ||
		!bytes.Contains(requests[0].Context, []byte(`<html>`)) {
		t.Fatalf("reviewer context is not exact provider-neutral canonical JSON: %q", requests[0].Context)
	}
	receipt, ok := bundle.Receipt()
	if !ok {
		t.Fatal("HTML-bearing incomplete review has no diagnostic receipt")
	}
	manifest, err := VerifyReviewBundleReceipt(directory, receipt)
	if err != nil || len(manifest.Attempts) != 1 ||
		manifest.Attempts[0].ReviewStatus != "complete" {
		t.Fatalf("HTML-bearing diagnostic manifest = %+v, error = %v", manifest, err)
	}
}

func TestReviewBundleRealChromiumOptInEndToEnd(t *testing.T) {
	if os.Getenv("OPENREALTIME_REALTIME_CU_CHROMIUM_E2E") != "1" {
		t.Skip("set OPENREALTIME_REALTIME_CU_CHROMIUM_E2E=1 to run real-Chromium evidence E2E")
	}
	for _, tool := range []string{"chromium", "ffmpeg", "ffprobe", "bwrap"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	environment, err := NewEnvironment(ctx, EnvironmentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	item := Case{Task: Suite()[4], Grounding: GroundingSetOfMark}
	episode, err := environment.Episode(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	if err := episode.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	frame, marks, err := episode.Surface().CaptureMarked(ctx)
	if err != nil {
		t.Fatal(err)
	}
	frameConfig, _, err := image.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("captured real Chromium frame: %dx%d (%d bytes)", frameConfig.Width, frameConfig.Height, len(frame))
	var violet string
	for _, mark := range marks {
		if mark.Name == "Violet" {
			violet = mark.ID
		}
	}
	if violet == "" {
		t.Fatalf("real browser did not expose the authored target: %+v", marks)
	}
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: realtimeCUFFmpegFactory{},
		Reviewer:         openFixtureCUReviewer(t, &fixtureCUReviewer{}),
		SourceAnchor:     fixtureReviewSourceAnchor(directory),
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	specification := EvidenceAttempt{
		Suite: SuiteName, Case: item.ID(), Trial: 1, Task: cloneCase(item).Task,
		Grounding: item.Grounding,
		Origin: EvidenceRunOrigin{
			Kind: EvidenceOriginHermetic, Transport: bench.TransportWebSocket,
			EndpointSHA256: endpointIdentity("ws://hermetic.invalid/v1/realtime"),
		},
	}
	attempt, err := bundle.BeginAttempt(ctx, specification)
	if err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureAudio(fixtureReviewAudio()); err != nil {
		t.Fatal(err)
	}
	if err := attempt.CaptureVideo(bench.SessionVideoCapture{
		Source: "screen", Width: frameConfig.Width, Height: frameConfig.Height, MediaType: "image/jpeg",
		WireTimestamp: 1, EpisodeAtMS: 0, Data: frame,
	}); err != nil {
		t.Fatal(err)
	}
	if err := episode.Surface().ClickElement(ctx, violet); err != nil {
		t.Fatal(err)
	}
	page, err := episode.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{
		ID: item.ID(), Completed: true, Passed: page.Success,
		Metrics: map[string]float64{"task_success_rate": truth(page.Success)},
		Notes:   map[string]string{"page_result": page.Reason},
	}
	if err := attempt.Complete(ctx, EvidenceCompletion{
		Attempt: specification, Outcome: outcome,
		Transcript: bench.Transcript{PlaybackMS: 100, Moments: []bench.Moment{
			{Kind: bench.MomentReady, AtMS: 0},
			{Kind: bench.MomentVideoFrame, Source: "screen", AtMS: 0},
		}}, Page: page,
	}); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(ctx, result); err == nil ||
		!strings.Contains(err.Error(), "retained 1 of 16") {
		t.Fatalf("FinishSuite() = %v", err)
	}
	receipt, ok := bundle.Receipt()
	if !ok {
		t.Fatal("real-Chromium evidence bundle has no verified receipt")
	}
	manifest, err := VerifyReviewBundle(directory, receipt.ManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Attempts) != 1 || manifest.Attempts[0].ReviewStatus != "complete" ||
		manifest.Attempts[0].RunOrigin.Live || manifest.Reportable {
		t.Fatalf("real-Chromium hermetic manifest = %+v", manifest)
	}
}

func TestReviewBundleRealChromiumFFmpegFullDecodeExactSixteenOptInEndToEnd(t *testing.T) {
	if os.Getenv("OPENREALTIME_REALTIME_CU_CHROMIUM_E2E") != "1" {
		t.Skip("set OPENREALTIME_REALTIME_CU_CHROMIUM_E2E=1 to run exact-16 real-Chromium/FFmpeg E2E")
	}
	for _, tool := range []string{"chromium", "ffmpeg", "ffprobe", "bwrap"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	environment, err := NewEnvironment(ctx, EnvironmentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer environment.Close()
	cases, err := Select(nil, nil)
	if err != nil || len(cases) != 16 {
		t.Fatalf("Select() cases=%d error=%v", len(cases), err)
	}
	directory := strings.TrimSpace(os.Getenv("OPENREALTIME_REALTIME_CU_CHROMIUM_E2E_RETAIN_DIRECTORY"))
	retain := directory != ""
	if !retain {
		directory = filepath.Join(t.TempDir(), "review")
		t.Cleanup(func() { makeReviewTreeWritable(directory) })
	} else if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		t.Fatal("OPENREALTIME_REALTIME_CU_CHROMIUM_E2E_RETAIN_DIRECTORY must be an absolute canonical path")
	}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: realtimeCUFFmpegFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16,
	}
	for _, item := range cases {
		episode, err := environment.Episode(ctx, item)
		if err != nil {
			t.Fatalf("%s Episode() = %v", item.ID(), err)
		}
		if err := episode.Ready(ctx); err != nil {
			t.Fatalf("%s Ready() = %v", item.ID(), err)
		}
		screen, err := episode.CaptureScreen(ctx)
		if err != nil {
			t.Fatalf("%s CaptureScreen() = %v", item.ID(), err)
		}
		screenConfig, _, err := image.DecodeConfig(bytes.NewReader(screen))
		if err != nil {
			t.Fatalf("%s screen decode = %v", item.ID(), err)
		}
		specification := EvidenceAttempt{
			Suite: SuiteName, Case: item.ID(), Trial: 1, Task: cloneCase(item).Task,
			Grounding: item.Grounding,
			Origin: EvidenceRunOrigin{
				Kind: EvidenceOriginHermetic, Live: false, Transport: bench.TransportWebSocket,
				EndpointSHA256: endpointIdentity("ws://hermetic.invalid/v1/realtime"),
			},
		}
		attempt, err := bundle.BeginAttempt(ctx, specification)
		if err != nil {
			t.Fatalf("%s BeginAttempt() = %v", item.ID(), err)
		}
		if err := attempt.CaptureAudio(fixtureReviewAudio()); err != nil {
			t.Fatalf("%s CaptureAudio() = %v", item.ID(), err)
		}
		if err := attempt.CaptureVideo(bench.SessionVideoCapture{
			Source: "screen", Width: screenConfig.Width, Height: screenConfig.Height,
			MediaType: "image/jpeg", WireTimestamp: 1, EpisodeAtMS: 0, Data: screen,
		}); err != nil {
			t.Fatalf("%s CaptureVideo(screen) = %v", item.ID(), err)
		}
		moments := []bench.Moment{
			{Kind: bench.MomentReady, AtMS: 0},
			{Kind: bench.MomentVideoFrame, Source: "screen", AtMS: 0},
		}
		if item.Task.Camera {
			camera, err := episode.CaptureCamera(ctx)
			if err != nil {
				t.Fatalf("%s CaptureCamera() = %v", item.ID(), err)
			}
			cameraConfig, _, err := image.DecodeConfig(bytes.NewReader(camera))
			if err != nil {
				t.Fatalf("%s camera decode = %v", item.ID(), err)
			}
			if err := attempt.CaptureVideo(bench.SessionVideoCapture{
				Source: "camera", Width: cameraConfig.Width, Height: cameraConfig.Height,
				MediaType: "image/jpeg", WireTimestamp: 2, EpisodeAtMS: 1, Data: camera,
			}); err != nil {
				t.Fatalf("%s CaptureVideo(camera) = %v", item.ID(), err)
			}
			moments = append(moments,
				bench.Moment{Kind: bench.MomentVideoFrame, Source: "camera", AtMS: 1})
		}
		page, err := episode.Result(ctx)
		if err != nil {
			t.Fatalf("%s Result() = %v", item.ID(), err)
		}
		outcome := bench.TaskOutcome{
			ID: item.ID(), Completed: true, Passed: false,
			Metrics: map[string]float64{
				"task_success_rate": 0, "correct_action_rate": 0, "deadline_miss_count": 1,
			},
			Notes: map[string]string{
				"grounding": string(item.Grounding), "page_result": page.Reason,
				"evidence_scope": "hermetic Chromium capture population; no Realtime model action",
			},
		}
		if err := attempt.Complete(ctx, EvidenceCompletion{
			Attempt: specification, Outcome: outcome,
			Transcript: bench.Transcript{PlaybackMS: 100, Moments: moments}, Page: page,
		}); err != nil {
			t.Fatalf("%s Complete() = %v", item.ID(), err)
		}
		result.Tasks = append(result.Tasks, outcome)
	}
	result.Finish()
	derived := result
	derived.Finish()
	if !reflect.DeepEqual(derived.Summary, result.Summary) {
		t.Fatalf("exact-16 Chromium result summary is unstable: retained=%+v derived=%+v tasks=%+v",
			result.Summary, derived.Summary, result.Tasks)
	}
	if err := bundle.FinishSuite(ctx, result); err != nil {
		t.Fatalf("exact-16 Chromium/FFmpeg FinishSuite() = %v", err)
	}
	receipt, ok := bundle.Receipt()
	if !ok {
		t.Fatal("exact-16 Chromium/FFmpeg bundle has no verified receipt")
	}
	manifest, err := VerifyReviewBundleReceipt(directory, receipt)
	if err != nil || !manifest.Complete || manifest.Reportable || len(manifest.Attempts) != 16 {
		t.Fatalf("exact-16 Chromium/FFmpeg manifest=%+v error=%v", manifest, err)
	}
	for _, indexed := range manifest.Attempts {
		if indexed.RunOrigin.Live || indexed.Reportable || indexed.ReviewStatus != "not_configured" {
			t.Fatalf("hermetic Chromium attempt was upgraded: %+v", indexed)
		}
		mediaDirectory := filepath.Join(directory, filepath.FromSlash(indexed.MediaBundle.Path))
		mediaManifest, err := reviewmedia.VerifyBundle(
			mediaDirectory, indexed.MediaBundle.ManifestSHA256,
		)
		wantVideos := 1
		if strings.HasPrefix(indexed.Case, "camera-smoke-stop/") {
			wantVideos = 2
		}
		if err != nil || len(mediaManifest.Video) != wantVideos || mediaManifest.Attestor == nil ||
			mediaManifest.Attestor.Descriptor.Capability != reviewmedia.FullDecodeAttestationCapability {
			t.Fatalf("%s media manifest=%+v error=%v", indexed.Case, mediaManifest, err)
		}
		for _, video := range mediaManifest.Video {
			if video.Playable == nil || video.PlayableSpec == nil || video.AttestationReport == nil ||
				video.PlayableSpec.Width != video.Width || video.PlayableSpec.Height != video.Height ||
				video.PlayableSpec.EncodedWidth != video.Width+video.Width%2 ||
				video.PlayableSpec.EncodedHeight != video.Height+video.Height%2 ||
				video.PlayableSpec.GeometryPolicy != reviewmedia.YUV420PPadRightBottomBlackToEvenV1 {
				t.Fatalf("%s/%s lacks exact source/encoded geometry attestation: %+v",
					indexed.Case, video.Source, video)
			}
			playable := filepath.Join(mediaDirectory, filepath.FromSlash(video.Playable.Path))
			command := exec.CommandContext(ctx, "ffmpeg",
				"-nostdin", "-hide_banner", "-loglevel", "error", "-xerror", "-err_detect", "explode",
				"-i", playable, "-map", "0:v:0", "-map", "0:a:0", "-vsync", "0", "-f", "null", "-",
			)
			if output, err := command.CombinedOutput(); err != nil || len(output) != 0 {
				t.Fatalf("independent full decode %s/%s error=%v output=%q",
					indexed.Case, video.Source, err, output)
			}
		}
	}
	if retain {
		payload, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		payload = append(payload, '\n')
		receiptPath := directory + ".receipt.json"
		file, err := os.OpenFile(receiptPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o400)
		if err != nil {
			t.Fatalf("create retained exact-16 receipt: %v", err)
		}
		if _, err := file.Write(payload); err != nil {
			_ = file.Close()
			t.Fatalf("write retained exact-16 receipt: %v", err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			t.Fatalf("sync retained exact-16 receipt: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close retained exact-16 receipt: %v", err)
		}
		t.Logf("retained hermetic capture-only exact-16 bundle=%s receipt=%s", directory, receiptPath)
	}
}

func TestReviewBundleFullSixteenHermeticReviewedCasesNeverBecomeReportable(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	var anchorOnce sync.Once
	var anchorErr error
	var anchorObserved atomic.Bool
	reviewer := &fixtureCUReviewer{
		delay: 10 * time.Millisecond,
		beforeReview: func() error {
			anchorOnce.Do(func() {
				receipt, err := ReadReviewSourceReceipt(
					t.Context(), fixtureReviewSourceReceiptPath(directory),
				)
				if err == nil {
					_, err = VerifyReviewSourceReceipt(directory, receipt)
				}
				anchorErr = err
				anchorObserved.Store(true)
			})
			return anchorErr
		},
	}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:         openFixtureCUReviewer(t, reviewer),
		SourceAnchor:     fixtureReviewSourceAnchor(directory),
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	cases, err := Select(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(), Expected: 16,
	}
	for index, item := range cases {
		result.Tasks = append(result.Tasks, fixtureReviewAttempt(t, bundle, item, index%3 != 0))
	}
	result.Finish()
	if err := result.Reportable(); err != nil {
		t.Fatalf("fixture core result should isolate bundle origin/reviewer gates: %v", err)
	}
	if err := bundle.FinishSuite(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	receipt, ok := bundle.Receipt()
	if !ok {
		t.Fatal("full bundle has no receipt")
	}
	manifest, err := VerifyReviewBundle(directory, receipt.ManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if !manifest.Complete || !manifest.CoreReportable || manifest.Reportable ||
		len(manifest.Attempts) != 16 || len(manifest.Missing) != 0 {
		t.Fatalf("full hermetic manifest = %+v", manifest)
	}
	for _, attempt := range manifest.Attempts {
		wantMedia := 2
		if strings.HasPrefix(attempt.Case, "camera-smoke-stop/") {
			wantMedia = 3
		}
		if attempt.RunOrigin.Live || attempt.Reportable || len(attempt.Media) != wantMedia ||
			attempt.ReviewStatus != "complete" || attempt.EvaluationBundle == nil ||
			attempt.Reviewer == nil || attempt.Assessment == nil ||
			attempt.IndependentRemoteAttestation || attempt.AuthenticityCaveat == "" ||
			!strings.Contains(attempt.ReportabilityNote, "production shared Realtime path") ||
			strings.Contains(attempt.ReportabilityNote, "secondary multimodal review") {
			t.Fatalf("hermetic attempt was upgraded: %+v", attempt)
		}
	}
	reviewer.mu.Lock()
	requests := slices.Clone(reviewer.requests)
	reviewer.mu.Unlock()
	if len(requests) != 16 {
		t.Fatalf("reviewer requests = %d, want 16", len(requests))
	}
	if maximum := reviewer.maximum.Load(); maximum < 2 || maximum > 4 {
		t.Fatalf("default secondary-review concurrency = %d, want 2..4", maximum)
	}
	if !anchorObserved.Load() || anchorErr != nil {
		t.Fatalf("reviewer ran before a verified external source anchor: observed=%t error=%v",
			anchorObserved.Load(), anchorErr)
	}
	wantResultSHA := manifest.Result.SHA256
	seen := make(map[string]struct{}, 16)
	for _, request := range requests {
		if _, duplicate := seen[request.AttemptID]; duplicate ||
			!strings.Contains(request.AttemptID, strings.TrimPrefix(wantResultSHA, "sha256:")) {
			t.Fatalf("review attempt ID is not unique/run-bound: %q", request.AttemptID)
		}
		seen[request.AttemptID] = struct{}{}
	}
	reviewMarkdown, err := os.ReadFile(filepath.Join(directory, "REVIEW.md"))
	if err != nil || !bytes.Contains(reviewMarkdown, []byte("does not independently attest")) ||
		!bytes.Contains(reviewMarkdown, []byte("Significant problems: 0")) ||
		!bytes.Contains(reviewMarkdown, []byte("Minor observations: 0")) ||
		!bytes.Contains(reviewMarkdown, []byte("Limitations: 0")) {
		t.Fatalf("REVIEW.md lacks authenticity caveat: %v\n%s", err, reviewMarkdown)
	}
}

func TestRenderReviewIncludesEveryReviewerFindingClassAndNeutralSourceLabel(t *testing.T) {
	directory, receipt := fixtureFinishedReviewedBundle(t)
	manifest, err := VerifyReviewBundle(directory, receipt.ManifestSHA256)
	if err != nil || len(manifest.Attempts) != 1 || manifest.Attempts[0].Assessment == nil {
		t.Fatalf("reviewed manifest=%+v error=%v", manifest, err)
	}
	start, end := int64(7), int64(19)
	manifest.Attempts[0].Assessment.SignificantProblems = []revieweval.Finding{{
		Category: "safety", StartMS: &start,
		Evidence: "A visible warning was missed.", Impact: "The action should stop.",
	}}
	manifest.Attempts[0].Assessment.MinorObservations = []revieweval.Finding{{
		Category: "latency", StartMS: &start, EndMS: &end,
		Evidence: "The pointer paused briefly.", Impact: "The interaction felt slower.",
	}}
	manifest.Attempts[0].Assessment.Limitations = []string{
		"The retained view does not include external displays.",
	}
	markdown := renderReview(manifest)
	for _, expected := range []string{
		"[Deterministic source manifest]", "Significant problems: 1", "`safety`",
		"(at 7 ms)", "Minor observations: 1", "`latency`", "(at 7–19 ms)",
		"Limitations: 1", "does not include external displays",
	} {
		if !strings.Contains(markdown, expected) {
			t.Fatalf("case review omitted %q:\n%s", expected, markdown)
		}
	}
	if strings.Contains(markdown, "Externally anchored deterministic source") {
		t.Fatalf("case review asserted an anchor state not retained by the manifest:\n%s", markdown)
	}
}

func TestReviewBundleCommitsSourceBeforeReviewerAndAdoptsVerifiedRetryReceipts(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	cases, err := Select(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reviewer := &fixtureCUReviewer{failCase: cases[5].ID()}
	reviewerLease := openFixtureCUReviewer(t, reviewer)
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:         reviewerLease,
		SourceAnchor:     fixtureReviewSourceAnchor(directory),
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(), Expected: 16,
	}
	for index, item := range cases {
		result.Tasks = append(result.Tasks, fixtureReviewAttempt(t, bundle, item, index%2 == 0))
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "transient outage") ||
		!strings.Contains(err.Error(), cases[5].ID()) {
		t.Fatalf("first FinishSuite() = %v", err)
	}
	sourceReceipt, ok := bundle.SourceReceipt()
	if !ok {
		t.Fatal("reviewer outage discarded the deterministic source receipt")
	}
	source, err := VerifyReviewSourceBundle(directory, sourceReceipt.ManifestSHA256)
	if err != nil || !source.Complete || source.Phase != ReviewPhaseSource ||
		len(source.Attempts) != 16 || source.Reportable {
		t.Fatalf("source manifest=%+v error=%v", source, err)
	}
	if _, ok := bundle.Receipt(); ok {
		t.Fatal("reviewer outage published a final review receipt")
	}
	if _, err := os.Lstat(filepath.Join(directory, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("reviewer outage published an outer marker: %v", err)
	}
	firstEvaluations := bundle.EvaluationReceipts()
	if len(firstEvaluations) != 15 || reviewer.calls.Load() != 16 {
		t.Fatalf("first review receipts=%d calls=%d, want 15/16",
			len(firstEvaluations), reviewer.calls.Load())
	}
	stores := fixtureReviewEvaluationStores(t, directory).(FileReviewEvaluationReceiptStoreFactory)
	for caseID, expected := range firstEvaluations {
		path, err := stores.path(caseID, expected.Directory)
		if err != nil {
			t.Fatal(err)
		}
		retained, err := revieweval.ReadEvaluationBundleReceipt(t.Context(), path)
		if err != nil || !sameEvaluationBundlePortableReceipt(retained, expected) {
			t.Fatalf("durable evaluation receipt %s=%+v error=%v", caseID, retained, err)
		}
	}
	// Simulate a process stopping after its caller-owned receipt became durable
	// but before atomic final promotion. Resume receives no in-memory receipt
	// map; staged publication must recover from external state alone.
	recoverCase := cases[0].ID()
	recoverFinal := firstEvaluations[recoverCase].Directory
	recoverStage := fixtureReviewEvaluationStageDirectory(recoverFinal)
	if err := os.Rename(recoverFinal, recoverStage); err != nil {
		t.Fatal(err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeReviewBundle(t.Context(), ReviewBundleResumeOptions{
		Directory: directory, SourceReceipt: sourceReceipt, Reviewer: reviewerLease,
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.FinishSuite(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	if reviewer.calls.Load() != 17 {
		t.Fatalf("review retry repeated sealed evaluations: calls=%d, want 17", reviewer.calls.Load())
	}
	if _, err := os.Lstat(recoverStage); !os.IsNotExist(err) {
		t.Fatalf("receipt-anchored evaluation stage was not promoted: %v", err)
	}
	afterSource, ok := resumed.SourceReceipt()
	if !ok || afterSource != sourceReceipt {
		t.Fatalf("source receipt changed across retry: before=%+v after=%+v", sourceReceipt, afterSource)
	}
	receipt, ok := resumed.Receipt()
	if !ok || receipt.SourceManifestSHA256 != sourceReceipt.ManifestSHA256 {
		t.Fatalf("final receipt does not bind source: %+v", receipt)
	}
	manifest, err := VerifyReviewBundle(directory, receipt.ManifestSHA256)
	if err != nil || !manifest.Complete || len(manifest.Attempts) != 16 {
		t.Fatalf("final reviewed manifest=%+v error=%v", manifest, err)
	}
}

func TestReviewBundleResumeAdoptsAnchoredReviewsWithoutProviderLease(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	cases, err := Select(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	reviewer := &fixtureCUReviewer{failCase: cases[5].ID()}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:         openFixtureCUReviewer(t, reviewer),
		SourceAnchor:     fixtureReviewSourceAnchor(directory),
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(), Expected: 16,
	}
	for _, item := range cases {
		result.Tasks = append(result.Tasks, fixtureReviewAttempt(t, bundle, item, true))
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "transient outage") ||
		!strings.Contains(err.Error(), cases[5].ID()) {
		t.Fatalf("first FinishSuite() = %v", err)
	}
	sourceReceipt, ok := bundle.SourceReceipt()
	if !ok {
		t.Fatal("reviewer outage discarded the deterministic source receipt")
	}
	evaluations := bundle.EvaluationReceipts()
	if len(evaluations) != 15 || reviewer.calls.Load() != 16 {
		t.Fatalf("first review receipts=%d calls=%d, want 15/16",
			len(evaluations), reviewer.calls.Load())
	}
	stores := fixtureReviewEvaluationStores(t, directory).(FileReviewEvaluationReceiptStoreFactory)
	for caseID, expected := range evaluations {
		path, err := stores.path(caseID, expected.Directory)
		if err != nil {
			t.Fatal(err)
		}
		retained, err := revieweval.ReadEvaluationBundleReceipt(t.Context(), path)
		if err != nil || !sameEvaluationBundlePortableReceipt(retained, expected) {
			t.Fatalf("durable evaluation receipt %s=%+v error=%v", caseID, retained, err)
		}
	}
	if err := bundle.Close(); err != nil {
		t.Fatal(err)
	}

	// The provider lease is intentionally absent: each externally anchored
	// sibling remains independently adoptable, while the one missing review is
	// left explicitly unconfigured rather than forged or silently trusted.
	resumed, err := ResumeReviewBundle(t.Context(), ReviewBundleResumeOptions{
		Directory: directory, SourceReceipt: sourceReceipt,
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := resumed.FinishSuite(t.Context(), result); err != nil {
		t.Fatal(err)
	}
	if reviewer.calls.Load() != 16 {
		t.Fatalf("receipt-only resume called provider: calls=%d, want 16", reviewer.calls.Load())
	}
	receipt, ok := resumed.Receipt()
	if !ok || receipt.SourceManifestSHA256 != sourceReceipt.ManifestSHA256 {
		t.Fatalf("receipt-only final publication does not bind source: %+v", receipt)
	}
	manifest, err := VerifyReviewBundleReceipt(directory, receipt)
	if err != nil || !manifest.Complete || manifest.Reportable || len(manifest.Attempts) != 16 {
		t.Fatalf("receipt-only final manifest=%+v error=%v", manifest, err)
	}
	statuses := map[string]int{}
	for _, attempt := range manifest.Attempts {
		statuses[attempt.ReviewStatus]++
	}
	if statuses["complete"] != 15 || statuses["not_configured"] != 1 {
		t.Fatalf("receipt-only review statuses = %+v", statuses)
	}
}

func TestReviewBundleFinishDrainsBindsExactResultAndRetriesAfterCancellation(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	attempt, completion, outcome := fixturePendingReviewAttempt(t, bundle, item, true)
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()

	finishes := make(chan error, 2)
	go func() { finishes <- bundle.FinishSuite(context.Background(), result) }()
	waitForReviewBundleClosing(t, bundle)
	if _, err := bundle.BeginAttempt(t.Context(), EvidenceAttempt{
		Suite: SuiteName, Case: (Case{Task: Suite()[1], Grounding: GroundingPixel}).ID(),
		Trial: 1, Task: cloneCase(Case{Task: Suite()[1]}).Task, Grounding: GroundingPixel,
		Origin: EvidenceRunOrigin{
			Kind: EvidenceOriginHermetic, Transport: bench.TransportWebSocket,
			EndpointSHA256: endpointIdentity("ws://hermetic.invalid/v1/realtime"),
		},
	}); err == nil || !strings.Contains(err.Error(), "closing") {
		t.Fatalf("BeginAttempt() after FinishSuite admission close = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := bundle.FinishSuite(canceled, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled FinishSuite() = %v", err)
	}
	go func() { finishes <- bundle.FinishSuite(context.Background(), result) }()
	if err := attempt.Complete(t.Context(), completion); err != nil {
		t.Fatal(err)
	}
	var canonicalError string
	for range 2 {
		select {
		case err := <-finishes:
			if err == nil || !strings.Contains(err.Error(), "retained 1 of 16") {
				t.Fatalf("concurrent FinishSuite() = %v", err)
			}
			if canonicalError == "" {
				canonicalError = err.Error()
			} else if err.Error() != canonicalError {
				t.Fatalf("concurrent finish errors differ: %q / %q", canonicalError, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent FinishSuite did not drain")
		}
	}
	if err := bundle.FinishSuite(t.Context(), result); err == nil || err.Error() != canonicalError {
		t.Fatalf("idempotent FinishSuite() = %v, want %q", err, canonicalError)
	}
	different := result
	different.Cell = cloneCell(result.Cell)
	different.Cell.Name = "different-finish-input"
	if err := bundle.FinishSuite(t.Context(), different); err == nil ||
		!strings.Contains(err.Error(), "differs from the committed input") {
		t.Fatalf("FinishSuite() accepted different concurrent input: %v", err)
	}
	receipt, ok := bundle.Receipt()
	if !ok {
		t.Fatal("drained bundle has no verified receipt")
	}
	if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err != nil {
		t.Fatal(err)
	}
}

func TestReviewBundleRetainsSourceAcrossCanceledFinalizeAndReviewer(t *testing.T) {
	t.Run("attempt-finalize", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "review")
		t.Cleanup(func() { makeReviewTreeWritable(directory) })
		bundle, err := NewReviewBundle(ReviewBundleOptions{
			Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		})
		if err != nil {
			t.Fatal(err)
		}
		item := Case{Task: Suite()[0], Grounding: GroundingPixel}
		attempt, completion, outcome := fixturePendingReviewAttempt(t, bundle, item, true)
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		if err := attempt.Complete(canceled, completion); err != nil {
			t.Fatalf("canceled Complete() did not use bounded retention context: %v", err)
		}
		result := bench.Result{
			Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
			Expected: 16, Tasks: []bench.TaskOutcome{outcome},
		}
		result.Finish()
		if err := bundle.FinishSuite(t.Context(), result); err == nil ||
			!strings.Contains(err.Error(), "retained 1 of 16") {
			t.Fatalf("partial FinishSuite() = %v", err)
		}
		receipt, ok := bundle.SourceReceipt()
		if !ok {
			t.Fatal("canceled attempt finalization lost its source receipt")
		}
		if _, err := VerifyReviewSourceBundle(directory, receipt.ManifestSHA256); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("advisory-review", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "review")
		t.Cleanup(func() { makeReviewTreeWritable(directory) })
		reviewer := &fixtureCUReviewer{delay: time.Hour}
		bundle, err := NewReviewBundle(ReviewBundleOptions{
			Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
			Reviewer: openFixtureCUReviewer(t, reviewer), ReviewConcurrency: 1,
			SourceAnchor:     fixtureReviewSourceAnchor(directory),
			EvaluationStores: fixtureReviewEvaluationStores(t, directory),
		})
		if err != nil {
			t.Fatal(err)
		}
		item := Case{Task: Suite()[0], Grounding: GroundingPixel}
		outcome := fixtureReviewAttempt(t, bundle, item, true)
		result := bench.Result{
			Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
			Expected: 16, Tasks: []bench.TaskOutcome{outcome},
		}
		result.Finish()
		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan error, 1)
		go func() { finished <- bundle.FinishSuite(ctx, result) }()
		deadline := time.After(10 * time.Second)
		for reviewer.active.Load() == 0 {
			select {
			case <-deadline:
				t.Fatal("reviewer did not start after source publication")
			case <-time.After(time.Millisecond):
			}
		}
		cancel()
		if err := <-finished; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled reviewer FinishSuite() = %v", err)
		}
		sourceReceipt, ok := bundle.SourceReceipt()
		if !ok {
			t.Fatal("canceled reviewer lost the source receipt")
		}
		if _, err := VerifyReviewSourceBundle(directory, sourceReceipt.ManifestSHA256); err != nil {
			t.Fatal(err)
		}
		reviewer.delay = 0
		if err := bundle.FinishSuite(t.Context(), result); err == nil ||
			!strings.Contains(err.Error(), "retained 1 of 16") {
			t.Fatalf("review retry FinishSuite() = %v", err)
		}
		finalReceipt, ok := bundle.Receipt()
		if !ok || finalReceipt.SourceManifestSHA256 != sourceReceipt.ManifestSHA256 {
			t.Fatalf("final receipt after cancellation = %+v", finalReceipt)
		}
	})
}

func TestReviewBundleRetriesExternalSourceAnchorBeforeCallingReviewer(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	anchorParent := filepath.Join(parent, "external-anchors")
	anchorPath := filepath.Join(anchorParent, "source.json")
	reviewer := &fixtureCUReviewer{}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:         openFixtureCUReviewer(t, reviewer),
		SourceAnchor:     FileReviewSourceReceiptAnchor{Path: anchorPath},
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	outcome := fixtureReviewAttempt(t, bundle, item, true)
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "source receipt parent") {
		t.Fatalf("unavailable source anchor FinishSuite() = %v", err)
	}
	if reviewer.calls.Load() != 0 {
		t.Fatalf("reviewer ran before external source anchor: calls=%d", reviewer.calls.Load())
	}
	if _, ok := bundle.SourceReceipt(); ok {
		t.Fatal("anchor outage exposed an unanchored deterministic source receipt")
	}
	if info, err := os.Lstat(filepath.Join(directory, reviewSourceManifestStage)); err != nil ||
		!info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("anchor outage did not retain a sealed source stage: info=%v error=%v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(directory, reviewSourceManifest)); !os.IsNotExist(err) {
		t.Fatalf("anchor outage exposed the final source marker: %v", err)
	}
	if err := os.Mkdir(anchorParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "retained 1 of 16") {
		t.Fatalf("source anchor retry FinishSuite() = %v", err)
	}
	if reviewer.calls.Load() != 1 {
		t.Fatalf("source anchor retry reviewer calls=%d, want 1", reviewer.calls.Load())
	}
	sourceReceipt, ok := bundle.SourceReceipt()
	if !ok {
		t.Fatal("source anchor retry has no deterministic source receipt")
	}
	anchored, err := ReadReviewSourceReceipt(t.Context(), anchorPath)
	if err != nil || !samePortableReviewSourceReceipt(anchored, sourceReceipt) {
		t.Fatalf("external source anchor=%+v error=%v", anchored, err)
	}
	finalReceipt, ok := bundle.Receipt()
	if !ok || finalReceipt.SourceReceiptSHA256 != sourceReceipt.ReceiptSHA256 {
		t.Fatalf("final receipt does not bind external source: %+v", finalReceipt)
	}
}

func TestReviewBundleResumeRecoversReceiptCommittedSourceStageAfterProcessStop(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	anchorPath := filepath.Join(parent, "source.receipt.json")
	anchor := &failAfterSourceReceiptPublishAnchor{
		FileReviewSourceReceiptAnchor: FileReviewSourceReceiptAnchor{Path: anchorPath},
	}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:         openFixtureCUReviewer(t, &fixtureCUReviewer{}),
		SourceAnchor:     anchor,
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	outcome := fixtureReviewAttempt(t, bundle, item, true)
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "fixture process stopped after durable source receipt") {
		t.Fatalf("interrupted source publication FinishSuite() = %v", err)
	}
	if _, ok := bundle.SourceReceipt(); ok {
		t.Fatal("ambiguous receipt publish was self-asserted as a committed source")
	}
	retainedReceipt, err := ReadReviewSourceReceipt(t.Context(), anchorPath)
	if err != nil {
		t.Fatalf("reopen externally durable source receipt: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, reviewSourceManifest)); !os.IsNotExist(err) {
		t.Fatalf("interrupted publication exposed a final source marker: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(directory, reviewSourceManifestStage)); err != nil ||
		!info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("interrupted source stage info=%v error=%v", info, err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatal(err)
	}

	resumed, err := ResumeReviewBundle(t.Context(), ReviewBundleResumeOptions{
		Directory: directory, SourceReceipt: retainedReceipt,
		SourceAnchor: FileReviewSourceReceiptAnchor{Path: anchorPath},
	})
	if err != nil {
		t.Fatalf("recover receipt-committed source stage: %v", err)
	}
	if _, err := VerifyReviewSourceReceipt(directory, retainedReceipt); err != nil {
		t.Fatalf("verify recovered final source: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, reviewSourceManifestStage)); !os.IsNotExist(err) {
		t.Fatalf("recovered source retained its staging marker: %v", err)
	}
	if err := resumed.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "retained 1 of 16") {
		t.Fatalf("FinishSuite() after source-stage recovery = %v", err)
	}
	finalReceipt, ok := resumed.Receipt()
	if !ok || finalReceipt.SourceReceiptSHA256 != retainedReceipt.ReceiptSHA256 {
		t.Fatalf("recovered final receipt=%+v", finalReceipt)
	}
}

func TestFileReviewSourceReceiptAnchorHoldsExclusiveCrashReleasedLease(t *testing.T) {
	anchor := FileReviewSourceReceiptAnchor{Path: filepath.Join(t.TempDir(), "source.json")}
	publicationID := reviewDigest([]byte("fixture-source-publication"))
	first, err := anchor.Acquire(t.Context(), publicationID)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := anchor.Acquire(t.Context(), publicationID); err == nil ||
		!strings.Contains(err.Error(), "already leased") {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("concurrent source Acquire() = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := anchor.Acquire(t.Context(), publicationID)
	if err != nil {
		t.Fatalf("reacquire released source publication: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewBundleRequiresDurableEvaluationStoreBeforeProvider(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	evaluationAnchorDirectory := filepath.Join(parent, "external-evaluation-anchors")
	evaluationQuarantineDirectory := filepath.Join(parent, "external-evaluation-quarantine")
	reviewer := &fixtureCUReviewer{}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:     openFixtureCUReviewer(t, reviewer),
		SourceAnchor: fixtureReviewSourceAnchor(directory),
		EvaluationStores: FileReviewEvaluationReceiptStoreFactory{
			Directory: evaluationAnchorDirectory, QuarantineRoot: evaluationQuarantineDirectory,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	outcome := fixtureReviewAttempt(t, bundle, item, true)
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "receipt store") {
		t.Fatalf("unavailable evaluation store FinishSuite() = %v", err)
	}
	if reviewer.calls.Load() != 0 || len(bundle.EvaluationReceipts()) != 0 {
		t.Fatalf("evaluation store outage calls=%d receipts=%d, want 0/0",
			reviewer.calls.Load(), len(bundle.EvaluationReceipts()))
	}
	if _, ok := bundle.SourceReceipt(); !ok {
		t.Fatal("evaluation anchor outage discarded deterministic source")
	}
	if err := os.Mkdir(evaluationAnchorDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(evaluationQuarantineDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "retained 1 of 16") {
		t.Fatalf("evaluation anchor retry FinishSuite() = %v", err)
	}
	if reviewer.calls.Load() != 1 {
		t.Fatalf("evaluation store retry reviewer calls=%d, want 1", reviewer.calls.Load())
	}
	receipts := bundle.EvaluationReceipts()
	anchored := receipts[item.ID()]
	storeFactory := FileReviewEvaluationReceiptStoreFactory{
		Directory: evaluationAnchorDirectory, QuarantineRoot: evaluationQuarantineDirectory,
	}
	path, err := storeFactory.path(item.ID(), anchored.Directory)
	if err != nil {
		t.Fatal(err)
	}
	retained, err := revieweval.ReadEvaluationBundleReceipt(t.Context(), path)
	if err != nil || !sameEvaluationBundlePortableReceipt(retained, anchored) {
		t.Fatalf("external evaluation receipt=%+v error=%v", retained, err)
	}
	if receipt, ok := bundle.Receipt(); !ok || receipt.SourceReceiptSHA256 == "" {
		t.Fatalf("evaluation anchor retry has no final receipt: %+v", receipt)
	}
}

func TestReviewBundleSurfacesEvaluationLeaseCloseAndFreshRetryRecoversReceipt(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	base := fixtureReviewEvaluationStores(t, directory).(FileReviewEvaluationReceiptStoreFactory)
	stores := &failOnceEvaluationPublicationCloseFactory{base: base}
	reviewer := &fixtureCUReviewer{}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:         openFixtureCUReviewer(t, reviewer),
		SourceAnchor:     fixtureReviewSourceAnchor(directory),
		EvaluationStores: stores,
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	outcome := fixtureReviewAttempt(t, bundle, item, true)
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "fixture evaluation publication close failed") {
		t.Fatalf("evaluation close failure FinishSuite() = %v", err)
	}
	if reviewer.calls.Load() != 1 || len(bundle.EvaluationReceipts()) != 1 {
		t.Fatalf("close failure calls=%d receipts=%d, want 1/1",
			reviewer.calls.Load(), len(bundle.EvaluationReceipts()))
	}
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "retained 1 of 16") {
		t.Fatalf("fresh recovery FinishSuite() = %v", err)
	}
	if reviewer.calls.Load() != 1 {
		t.Fatalf("fresh recovery repeated provider: calls=%d", reviewer.calls.Load())
	}
}

func TestReviewBundlePadsQuietAudioStrictlyPastLatestVideoSampleBoundary(t *testing.T) {
	nearlyCovered := bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: make([]int16, 2_403),
	}
	padded, endMS, err := padReviewAudioAfterVideo(nearlyCovered, 100)
	if err != nil || len(padded.RoomPCM16) != 2_424 || endMS != 101 {
		t.Fatalf("sub-millisecond final frame padding=%d/%v error=%v",
			len(padded.RoomPCM16), endMS, err)
	}
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	specification := EvidenceAttempt{
		Suite: SuiteName, Case: item.ID(), Trial: 1, Task: cloneCase(item).Task,
		Grounding: item.Grounding,
		Origin: EvidenceRunOrigin{
			Kind: EvidenceOriginHermetic, Transport: bench.TransportWebSocket,
			EndpointSHA256: endpointIdentity("ws://hermetic.invalid/v1/realtime"),
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
	if err := attempt.CaptureVideo(bench.SessionVideoCapture{
		Source: "screen", Width: 64, Height: 48, MediaType: "image/png",
		WireTimestamp: 1, EpisodeAtMS: 100,
		Data: cuPNG(t, 64, 48, color.RGBA{R: 0x20, G: 0x40, B: 0x60, A: 0xff}),
	}); err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{
		ID: item.ID(), Completed: true, Passed: true,
		Metrics: map[string]float64{"task_success_rate": 1},
	}
	if err := attempt.Complete(t.Context(), EvidenceCompletion{
		Attempt: specification, Outcome: outcome,
		Transcript: bench.Transcript{PlaybackMS: 100, Moments: []bench.Moment{
			{Kind: bench.MomentVideoFrame, Source: "screen", AtMS: 100},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "retained 1 of 16") {
		t.Fatalf("partial FinishSuite() = %v", err)
	}
	receipt, ok := bundle.SourceReceipt()
	if !ok {
		t.Fatal("padded source has no receipt")
	}
	source, err := VerifyReviewSourceBundle(directory, receipt.ManifestSHA256)
	if err != nil || len(source.Attempts) != 1 {
		t.Fatalf("source=%+v error=%v", source, err)
	}
	mediaDirectory := filepath.Join(
		directory, filepath.FromSlash(source.Attempts[0].MediaBundle.Path),
	)
	mediaManifest, err := reviewmedia.VerifyBundle(
		mediaDirectory, source.Attempts[0].MediaBundle.ManifestSHA256,
	)
	if err != nil || mediaManifest.Audio == nil || mediaManifest.AttemptEndUS != 101_000 ||
		mediaManifest.Audio.AudioSpec.Frames != 2_424 {
		t.Fatalf("padded media manifest=%+v error=%v", mediaManifest, err)
	}
}

func TestReviewBundleRejectsTranscriptExecutionDriftBeforeMediaCommit(t *testing.T) {
	for _, mode := range []string{"error", "scope"} {
		t.Run(mode, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "review")
			t.Cleanup(func() { makeReviewTreeWritable(directory) })
			bundle, err := NewReviewBundle(ReviewBundleOptions{
				Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
			})
			if err != nil {
				t.Fatal(err)
			}
			item := Case{Task: Suite()[0], Grounding: GroundingPixel}
			attempt, completion, _ := fixturePendingReviewAttempt(t, bundle, item, true)
			switch mode {
			case "error":
				completion.Transcript.ExecutionError = "attestor refused the session"
			case "scope":
				evidence := &bench.ExecutionEvidence{Scope: "another-case"}
				completion.Transcript.Execution = evidence
				copy := evidence.Clone()
				completion.Outcome.Execution = &copy
			}
			if err := attempt.Complete(t.Context(), completion); err == nil ||
				!strings.Contains(err.Error(), "execution evidence") {
				t.Fatalf("Complete() accepted %s drift: %v", mode, err)
			}
			mediaDirectory := filepath.Join(
				directory, "media", "01-static-control-pixel-trial-01",
			)
			if _, err := os.Lstat(filepath.Join(mediaDirectory, "manifest.json")); !os.IsNotExist(err) {
				t.Fatalf("execution drift published media manifest: %v", err)
			}
			if err := bundle.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReviewBundleCloseDrainsActiveAttemptAndIsConcurrentIdempotent(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	attempt, _, _ := fixturePendingReviewAttempt(t, bundle, item, true)
	closed := make(chan error, 4)
	for range 4 {
		go func() { closed <- bundle.Close() }()
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned before the active attempt drained: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := attempt.Abort(); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		select {
		case err := <-closed:
			if err != nil {
				t.Fatalf("Close() = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent Close did not drain")
		}
	}
	if _, ok := bundle.Receipt(); ok {
		t.Fatal("abandoned bundle has a receipt")
	}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(), Expected: 16,
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "closed without a suite commit") {
		t.Fatalf("FinishSuite after Close = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("abandoned bundle published a manifest: %v", err)
	}
}

func TestReviewBundleRejectsExistingDirectoryAndSensitiveResult(t *testing.T) {
	parent := t.TempDir()
	if _, err := NewReviewBundle(ReviewBundleOptions{
		Directory:    filepath.Join(parent, "unanchored-reviewer"),
		VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:     openFixtureCUReviewer(t, &fixtureCUReviewer{}),
	}); err == nil || !strings.Contains(err.Error(), "external source receipt anchor") {
		t.Fatalf("unanchored reviewer error = %v", err)
	}
	if _, err := NewReviewBundle(ReviewBundleOptions{
		Directory:    filepath.Join(parent, "unanchored-evaluation"),
		VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:     openFixtureCUReviewer(t, &fixtureCUReviewer{}),
		SourceAnchor: FileReviewSourceReceiptAnchor{
			Path: filepath.Join(parent, "unanchored-evaluation.source.json"),
		},
	}); err == nil || !strings.Contains(err.Error(), "external evaluation receipt stores") {
		t.Fatalf("unanchored evaluation error = %v", err)
	}
	for _, concurrency := range []int{-1, 17} {
		if _, err := NewReviewBundle(ReviewBundleOptions{
			Directory:    filepath.Join(parent, fmt.Sprintf("concurrency-%d", concurrency)),
			VideoFactory: &fixtureReviewVideoFactory{}, ReviewConcurrency: concurrency,
		}); err == nil || !strings.Contains(err.Error(), "concurrency") {
			t.Fatalf("ReviewConcurrency %d error = %v", concurrency, err)
		}
	}
	existing := filepath.Join(parent, "existing")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReviewBundle(ReviewBundleOptions{
		Directory: existing, VideoFactory: &fixtureReviewVideoFactory{},
	}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing directory error = %v", err)
	}
	secret := "review-secret-value"
	directory := filepath.Join(parent, "sensitive")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		SensitiveValues: []string{secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	outcome := fixtureReviewAttempt(t, bundle, item, true)
	outcome.Notes["unsafe"] = base64Raw(secret)
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "sensitive value") {
		t.Fatalf("sensitive FinishSuite() error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("sensitive bundle published a manifest: %v", err)
	}
}

func TestReviewBundleAndReceiptAnchorsRejectSymlinkedAncestors(t *testing.T) {
	parent := t.TempDir()
	realParent := filepath.Join(parent, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(parent, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	if _, err := NewReviewBundle(ReviewBundleOptions{
		Directory:    filepath.Join(linkedParent, "review"),
		VideoFactory: &fixtureReviewVideoFactory{},
	}); err == nil || !strings.Contains(err.Error(), "symlinked or invalid ancestor") {
		t.Fatalf("NewReviewBundle() symlink-ancestor error = %v", err)
	}

	directory, finalReceipt := fixtureFinishedReviewBundle(t)
	manifest, err := VerifyReviewSourceBundle(directory, finalReceipt.SourceManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	sourceReceipt, err := buildReviewSourceReceipt(
		directory, manifest, finalReceipt.SourceManifestSHA256,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteReviewSourceReceipt(
		t.Context(), filepath.Join(linkedParent, "source.receipt.json"), sourceReceipt,
	); err == nil || !strings.Contains(err.Error(), "symlinked or invalid ancestor") {
		t.Fatalf("WriteReviewSourceReceipt() symlink-ancestor error = %v", err)
	}

	evaluationDirectory := filepath.Join(directory, "reviews", reviewSlug(manifest.Attempts[0].Case))
	anchor := FileReviewEvaluationReceiptStoreFactory{Directory: linkedParent}
	if _, err := anchor.path(
		manifest.Attempts[0].Case, evaluationDirectory,
	); err == nil || !strings.Contains(err.Error(), "symlinked or invalid ancestor") {
		t.Fatalf("evaluation anchor symlink-ancestor error = %v", err)
	}
}

func TestReviewSecretGuardCanonicalizesAndRejectsEscapedEncodedAndSplitForms(t *testing.T) {
	secrets, err := canonicalReviewSecrets([]string{"abcdefgh", "abcdefghijkl"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(secrets, []string{"abcdefghijkl", "abcdefgh"}) {
		t.Fatalf("canonical secrets = %#v", secrets)
	}
	for name, fixture := range map[string][]byte{
		"literal":        []byte(`{"value":"abcdefghijkl"}`),
		"unicode_escape": []byte(`{"value":"\u0061\u0062\u0063\u0064\u0065\u0066\u0067\u0068"}`),
		"base64":         []byte(`{"value":"YWJjZGVmZ2hpamts"}`),
		"split_fields":   []byte(`{"first":"abcdef","second":"ghijkl"}`),
		"nested_json":    []byte(`{"value":"{\"secret\":\"abcdefghijkl\"}"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if !containsReviewSecret(secrets, fixture) {
				t.Fatalf("secret guard accepted %s", fixture)
			}
		})
	}
	if containsReviewSecret(secrets, []byte(`{"value":"safe public benchmark context"}`)) {
		t.Fatal("secret guard rejected benign context")
	}
}

func TestReviewBundleRejectsReviewerFindingOutsideRetainedTimeline(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	timestamp := int64(101)
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:         openFixtureCUReviewer(t, &fixtureCUReviewer{finding: &timestamp}),
		SourceAnchor:     fixtureReviewSourceAnchor(directory),
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(),
		Expected: 16, Tasks: []bench.TaskOutcome{fixtureReviewAttempt(t, bundle, item, true)},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil ||
		!strings.Contains(err.Error(), "exceeds retained media") {
		t.Fatalf("FinishSuite() timestamp error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("out-of-range review published an outer manifest: %v", err)
	}
}

func TestReviewBundleVerifierRejectsNestedSealedReviewTampering(t *testing.T) {
	directory, receipt := fixtureFinishedReviewedBundle(t)
	manifest, err := VerifyReviewBundle(directory, receipt.ManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Attempts) != 1 || manifest.Attempts[0].EvaluationBundle == nil {
		t.Fatalf("reviewed fixture manifest = %+v", manifest)
	}
	makeReviewTreeWritable(directory)
	appendReviewFile(t, filepath.Join(
		directory, filepath.FromSlash(manifest.Attempts[0].EvaluationBundle.Path), "record.json",
	), []byte(" "))
	if err := sealReviewTree(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err == nil {
		t.Fatal("VerifyReviewBundle() accepted nested sealed-review tampering")
	}
}

func TestReviewBundleResumeRecoversRejectedOuterReviewWithoutManifest(t *testing.T) {
	directory, _ := fixtureFinishedReviewedBundle(t)
	sourceReceipt, err := ReadReviewSourceReceipt(
		t.Context(), fixtureReviewSourceReceiptPath(directory),
	)
	if err != nil {
		t.Fatal(err)
	}
	makeReviewTreeWritable(directory)
	if err := os.Remove(filepath.Join(directory, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	if err := sealReviewTree(directory); err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeReviewBundle(t.Context(), ReviewBundleResumeOptions{
		Directory: directory, SourceReceipt: sourceReceipt,
		SourceAnchor: fixtureReviewSourceAnchor(directory),
	})
	if err != nil {
		t.Fatalf("recover rejected outer review: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "REVIEW.md")); !os.IsNotExist(err) {
		t.Fatalf("rejected outer review survived recovery: %v", err)
	}
	if info, err := os.Lstat(directory); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("resumable review root is not writable: info=%v error=%v", info, err)
	}
	if err := resumed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReviewBundleResumePreservesPublishedOuterReview(t *testing.T) {
	directory, _ := fixtureFinishedReviewedBundle(t)
	sourceReceipt, err := ReadReviewSourceReceipt(
		t.Context(), fixtureReviewSourceReceiptPath(directory),
	)
	if err != nil {
		t.Fatal(err)
	}
	reviewBefore, err := os.ReadFile(filepath.Join(directory, "REVIEW.md"))
	if err != nil {
		t.Fatal(err)
	}
	if resumed, err := ResumeReviewBundle(t.Context(), ReviewBundleResumeOptions{
		Directory: directory, SourceReceipt: sourceReceipt,
		SourceAnchor: fixtureReviewSourceAnchor(directory),
	}); err == nil || !strings.Contains(err.Error(), "already has an outer publication") {
		if resumed != nil {
			_ = resumed.Close()
		}
		t.Fatalf("resume published outer review error = %v", err)
	}
	reviewAfter, err := os.ReadFile(filepath.Join(directory, "REVIEW.md"))
	if err != nil || !bytes.Equal(reviewAfter, reviewBefore) {
		t.Fatalf("published outer review changed: error=%v", err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "manifest.json")); err != nil {
		t.Fatalf("published outer manifest changed: %v", err)
	}
}

func TestReviewBundleVerifierRejectsTamperingAndExtraEntries(t *testing.T) {
	for _, mode := range []string{
		"result", "context", "raw_frame", "source_manifest", "source_review",
		"review", "extra", "symlink", "hardlink",
	} {
		t.Run(mode, func(t *testing.T) {
			directory, receipt := fixtureFinishedReviewBundle(t)
			manifest, err := VerifyReviewBundle(directory, receipt.ManifestSHA256)
			if err != nil {
				t.Fatal(err)
			}
			makeReviewTreeWritable(directory)
			switch mode {
			case "result":
				appendReviewFile(t, filepath.Join(directory, "result.json"), []byte(" "))
			case "context":
				appendReviewFile(t, filepath.Join(directory, filepath.FromSlash(manifest.Attempts[0].Context.Path)), []byte(" "))
			case "raw_frame":
				appendReviewFile(t, filepath.Join(directory, filepath.FromSlash(manifest.Attempts[0].MediaBundle.Path),
					"frames", "screen", "000001.png"), []byte("x"))
			case "source_manifest":
				appendReviewFile(t, filepath.Join(directory, "source.manifest.json"), []byte(" "))
			case "source_review":
				appendReviewFile(t, filepath.Join(directory, "SOURCE_REVIEW.md"), []byte("changed"))
			case "review":
				appendReviewFile(t, filepath.Join(directory, "REVIEW.md"), []byte("changed"))
			case "extra":
				if err := os.WriteFile(filepath.Join(directory, "extra.txt"), []byte("extra"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("result.json", filepath.Join(directory, "alias")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(
					filepath.Join(directory, "result.json"), filepath.Join(directory, "alias"),
				); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "symlink" {
				if err := sealReviewTree(directory); err != nil {
					if mode == "hardlink" && strings.Contains(err.Error(), "aliased") {
						return
					}
					t.Fatal(err)
				}
			}
			if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err == nil {
				t.Fatalf("VerifyReviewBundle() accepted %s tampering", mode)
			}
		})
	}
}

func TestVerifyReviewBundleReceiptRejectsDifferentSourceAnchor(t *testing.T) {
	directory, receipt := fixtureFinishedReviewBundle(t)
	if _, err := VerifyReviewBundleReceipt(directory, receipt); err != nil {
		t.Fatal(err)
	}
	drifted := receipt
	drifted.SourceManifestSHA256 = receipt.ManifestSHA256
	if _, err := VerifyReviewBundleReceipt(directory, drifted); err == nil ||
		!strings.Contains(err.Error(), "different source") {
		t.Fatalf("VerifyReviewBundleReceipt() accepted a different source: %v", err)
	}
	drifted = receipt
	drifted.SourceReceiptSHA256 = receipt.ManifestSHA256
	if _, err := VerifyReviewBundleReceipt(directory, drifted); err == nil ||
		!strings.Contains(err.Error(), "different source identity") {
		t.Fatalf("VerifyReviewBundleReceipt() accepted a different source receipt: %v", err)
	}
}

func TestReviewSourceReceiptIsCreateOnlyPortableAndResultBound(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(), Expected: 16,
		Tasks: []bench.TaskOutcome{fixtureReviewAttempt(t, bundle, item, true)},
	}
	result.Finish()
	_ = bundle.FinishSuite(t.Context(), result)
	receipt, ok := bundle.SourceReceipt()
	if !ok {
		t.Fatal("diagnostic source has no receipt")
	}
	path := fixtureReviewSourceReceiptPath(directory)
	if err := WriteReviewSourceReceipt(t.Context(), path, receipt); err != nil {
		t.Fatal(err)
	}
	if err := WriteReviewSourceReceipt(t.Context(), path, receipt); err == nil {
		t.Fatal("source receipt writer replaced an existing anchor")
	}
	opened, err := ReadReviewSourceReceipt(t.Context(), path)
	if err != nil || !samePortableReviewSourceReceipt(opened, receipt) {
		t.Fatalf("opened source receipt=%+v error=%v", opened, err)
	}
	if _, err := VerifyReviewSourceReceipt(directory, opened); err != nil {
		t.Fatal(err)
	}
	if err := WriteReviewSourceReceipt(
		t.Context(), filepath.Join(directory, "forbidden-receipt.json"), receipt,
	); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("source receipt writer accepted an in-bundle anchor: %v", err)
	}
	forged := receipt
	forged.ResultSHA256 = reviewDigest([]byte("different deterministic result"))
	forged.ReceiptSHA256, err = reviewSourceReceiptDigest(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyReviewSourceReceipt(directory, forged); err == nil ||
		!strings.Contains(err.Error(), "differs") {
		t.Fatalf("source verifier accepted a result-substituted receipt: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadReviewSourceReceipt(t.Context(), path); err == nil {
		t.Fatal("source receipt reader accepted a writable external anchor")
	}
}

func TestReviewBundleVerifierRejectsInterphaseMutation(t *testing.T) {
	for _, phase := range []string{"after_semantics", "after_second_tree_capture"} {
		t.Run(phase, func(t *testing.T) {
			directory, receipt := fixtureFinishedReviewBundle(t)
			manifest, err := VerifyReviewBundle(directory, receipt.ManifestSHA256)
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(
				directory, filepath.FromSlash(manifest.Attempts[0].MediaBundle.Path),
				"frames", "screen", "000001.png",
			)
			mutate := func() error {
				payload, err := os.ReadFile(target)
				if err != nil {
					return err
				}
				payload[len(payload)-1] ^= 1
				if err := os.Chmod(target, 0o600); err != nil {
					return err
				}
				if err := os.WriteFile(target, payload, 0o600); err != nil {
					return err
				}
				return os.Chmod(target, 0o400)
			}
			operations := reviewBundleVerifyOperations{}
			if phase == "after_semantics" {
				operations.afterSemanticVerification = mutate
			} else {
				operations.afterSecondTreeCapture = mutate
			}
			if _, err := verifyReviewBundleWithOperations(
				directory, receipt.ManifestSHA256, operations,
			); err == nil {
				t.Fatalf("verifier accepted %s mutation", phase)
			}
		})
	}
}

func TestReviewSourceVerifierRejectsInterphaseMutationAndCloseFailure(t *testing.T) {
	for _, phase := range []string{"after_semantics", "after_second_tree_capture"} {
		t.Run(phase, func(t *testing.T) {
			directory, receipt := fixtureFinishedReviewBundle(t)
			source, err := VerifyReviewSourceBundle(directory, receipt.SourceManifestSHA256)
			if err != nil || len(source.Attempts) != 1 {
				t.Fatalf("source manifest=%+v error=%v", source, err)
			}
			target := filepath.Join(
				directory, filepath.FromSlash(source.Attempts[0].Context.Path),
			)
			mutate := func() error {
				payload, err := os.ReadFile(target)
				if err != nil {
					return err
				}
				payload[len(payload)-1] ^= 1
				if err := os.Chmod(target, 0o600); err != nil {
					return err
				}
				if err := os.WriteFile(target, payload, 0o600); err != nil {
					return err
				}
				return os.Chmod(target, 0o400)
			}
			operations := reviewBundleVerifyOperations{}
			if phase == "after_semantics" {
				operations.afterSemanticVerification = mutate
			} else {
				operations.afterSecondTreeCapture = mutate
			}
			if _, err := verifyReviewSourceBundleWithOperations(
				directory, receipt.SourceManifestSHA256, operations,
			); err == nil {
				t.Fatalf("source verifier accepted %s mutation", phase)
			}
		})
	}

	directory, receipt := fixtureFinishedReviewBundle(t)
	manifest, err := verifyReviewSourceBundleWithOperations(
		directory, receipt.SourceManifestSHA256, reviewBundleVerifyOperations{
			closeRoot: func(*os.Root) error { return errors.New("injected source close failure") },
		},
	)
	if err == nil || !reflect.DeepEqual(manifest, ReviewManifest{}) ||
		!strings.Contains(err.Error(), "close verified deterministic") {
		t.Fatalf("source close-failure manifest=%+v error=%v", manifest, err)
	}
}

func TestReviewVerifiersRejectRootSwap(t *testing.T) {
	for _, sourceOnly := range []bool{false, true} {
		name := "final"
		if sourceOnly {
			name = "source"
		}
		t.Run(name, func(t *testing.T) {
			directory, receipt := fixtureFinishedReviewBundle(t)
			moved := directory + ".moved"
			swapped := false
			t.Cleanup(func() {
				if !swapped {
					return
				}
				_ = os.Chmod(directory, 0o700)
				_ = os.Remove(directory)
				_ = os.Rename(moved, directory)
			})
			swap := func() error {
				if err := os.Rename(directory, moved); err != nil {
					return err
				}
				swapped = true
				return os.Mkdir(directory, 0o500)
			}
			operations := reviewBundleVerifyOperations{afterSemanticVerification: swap}
			var err error
			if sourceOnly {
				_, err = verifyReviewSourceBundleWithOperations(
					directory, receipt.SourceManifestSHA256, operations,
				)
			} else {
				_, err = verifyReviewBundleWithOperations(
					directory, receipt.ManifestSHA256, operations,
				)
			}
			if err == nil || !strings.Contains(err.Error(), "root identity changed") {
				t.Fatalf("%s verifier accepted root swap: %v", name, err)
			}
		})
	}
}

func TestReviewBundleSealAndInvalidationRefuseReplacementRootSideEffects(t *testing.T) {
	directory, _ := fixtureFinishedReviewBundle(t)
	makeReviewTreeWritable(directory)
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	identity, err := root.Stat(".")
	if err != nil {
		t.Fatal(err)
	}
	moved := directory + ".owned"
	if err := os.Rename(directory, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		makeReviewTreeWritable(directory)
		_ = os.RemoveAll(directory)
		_ = os.Rename(moved, directory)
	})
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	replacementManifest := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(replacementManifest, []byte("replacement\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := sealReviewTreeRoot(directory, root, identity); err == nil ||
		!strings.Contains(err.Error(), "root identity changed") {
		t.Fatalf("sealReviewTreeRoot() replacement error = %v", err)
	}
	if err := invalidateReviewManifestExpected(directory, identity); err == nil ||
		!strings.Contains(err.Error(), "different") {
		t.Fatalf("invalidateReviewManifestExpected() replacement error = %v", err)
	}
	payload, err := os.ReadFile(replacementManifest)
	info, statErr := os.Lstat(replacementManifest)
	if err != nil || statErr != nil || string(payload) != "replacement\n" ||
		info.Mode().Perm() != 0o600 {
		t.Fatalf("replacement root was mutated: payload=%q info=%v errors=%v/%v",
			payload, info, err, statErr)
	}
}

func TestReviewBundleVerifierFailsClosedOnRootCloseError(t *testing.T) {
	directory, receipt := fixtureFinishedReviewBundle(t)
	manifest, err := verifyReviewBundleWithOperations(
		directory, receipt.ManifestSHA256, reviewBundleVerifyOperations{
			closeRoot: func(*os.Root) error { return errors.New("injected close failure") },
		},
	)
	if err == nil || !reflect.DeepEqual(manifest, ReviewManifest{}) ||
		!strings.Contains(err.Error(), "close verified") {
		t.Fatalf("close-failure result manifest=%+v error=%v", manifest, err)
	}
}

func fixtureFinishedReviewBundle(t testing.TB) (string, ReviewBundleReceipt) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(), Expected: 16,
		Tasks: []bench.TaskOutcome{fixtureReviewAttempt(t, bundle, item, true)},
	}
	result.Finish()
	_ = bundle.FinishSuite(context.Background(), result)
	receipt, ok := bundle.Receipt()
	if !ok {
		t.Fatal("fixture bundle has no receipt")
	}
	return directory, receipt
}

func fixtureFinishedReviewedBundle(t testing.TB) (string, ReviewBundleReceipt) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "review")
	t.Cleanup(func() { makeReviewTreeWritable(directory) })
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, VideoFactory: &fixtureReviewVideoFactory{},
		Reviewer:         openFixtureCUReviewer(t, &fixtureCUReviewer{}),
		SourceAnchor:     fixtureReviewSourceAnchor(directory),
		EvaluationStores: fixtureReviewEvaluationStores(t, directory),
	})
	if err != nil {
		t.Fatal(err)
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	result := bench.Result{
		Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(), Expected: 16,
		Tasks: []bench.TaskOutcome{fixtureReviewAttempt(t, bundle, item, true)},
	}
	result.Finish()
	_ = bundle.FinishSuite(context.Background(), result)
	receipt, ok := bundle.Receipt()
	if !ok {
		t.Fatal("reviewed fixture bundle has no receipt")
	}
	return directory, receipt
}

func fixtureReviewAttempt(
	t testing.TB, bundle *ReviewBundle, item Case, passed bool,
) bench.TaskOutcome {
	t.Helper()
	attempt, completion, outcome := fixturePendingReviewAttempt(t, bundle, item, passed)
	if err := attempt.Complete(context.Background(), completion); err != nil {
		t.Fatal(err)
	}
	return outcome
}

func fixturePendingReviewAttempt(
	t testing.TB, bundle *ReviewBundle, item Case, passed bool,
) (AttemptEvidence, EvidenceCompletion, bench.TaskOutcome) {
	t.Helper()
	specification := EvidenceAttempt{
		Suite: SuiteName, Case: item.ID(), Trial: 1, Task: cloneCase(item).Task,
		Grounding: item.Grounding,
		Origin: EvidenceRunOrigin{
			Kind: EvidenceOriginHermetic, Live: false, Transport: bench.TransportWebSocket,
			EndpointSHA256: endpointIdentity("ws://hermetic.invalid/v1/realtime"),
		},
	}
	attempt, err := bundle.BeginAttempt(context.Background(), specification)
	if err != nil {
		t.Fatal(err)
	}
	audio := fixtureReviewAudio()
	if err := attempt.CaptureAudio(audio); err != nil {
		t.Fatal(err)
	}
	sources := []struct {
		name          string
		width, height int
		shade         color.RGBA
	}{{"screen", 64, 48, color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff}}}
	if item.Task.Camera {
		sources = append(sources, struct {
			name          string
			width, height int
			shade         color.RGBA
		}{"camera", 32, 24, color.RGBA{R: 0xc0, G: 0x40, B: 0x20, A: 0xff}})
	}
	for index, source := range sources {
		if err := attempt.CaptureVideo(bench.SessionVideoCapture{
			Source: source.name, Width: source.width, Height: source.height,
			MediaType: "image/png", WireTimestamp: int64(1000 + index), EpisodeAtMS: float64(index),
			Data: cuPNG(t, source.width, source.height, source.shade),
		}); err != nil {
			t.Fatal(err)
		}
	}
	outcome := bench.TaskOutcome{
		ID: item.ID(), Completed: true, Passed: passed,
		Metrics: map[string]float64{
			"task_success_rate": truth(passed), "correct_action_rate": truth(passed),
			"deadline_miss_count": truth(!passed),
		},
		Notes: map[string]string{
			"category": item.Task.Category, "difficulty": item.Task.Difficulty,
			"axes": axesText(item.Task.Axes), "grounding": string(item.Grounding),
			"page_result": "fixture deterministic result",
		},
	}
	completion := EvidenceCompletion{
		Attempt: specification, Outcome: outcome,
		Transcript: bench.Transcript{
			PlaybackMS: 100, Moments: []bench.Moment{
				{Kind: bench.MomentReady, AtMS: 0},
				{Kind: bench.MomentVideoFrame, Source: "screen", AtMS: 0},
			},
		},
		Page: PageResult{Complete: true, Success: passed, Reason: "fixture deterministic result", CompletedAtMS: 50},
	}
	return attempt, completion, outcome
}

func fixtureReviewAudio() bench.SessionAudioCapture {
	room := make([]int16, 2_400)
	agent := make([]int16, 1_200)
	for index := range room {
		room[index] = int16(index%200 - 100)
	}
	for index := range agent {
		agent[index] = int16(100 - index%200)
	}
	return bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: room,
		Agent: []bench.TimedAudioChunk{{AtMS: 25, PCM16: agent}},
	}
}

func fixtureReviewProvenance() bench.Provenance {
	return bench.Provenance{
		Revision: "fixture-review-revision", ExecutableSHA256: strings.Repeat("a", 64),
		StartedAt: time.Unix(1, 0).UTC().Format(time.RFC3339), Modified: false,
	}
}

func fixtureReviewSourceAnchor(directory string) ReviewSourceReceiptAnchor {
	return FileReviewSourceReceiptAnchor{Path: fixtureReviewSourceReceiptPath(directory)}
}

func fixtureReviewSourceReceiptPath(directory string) string {
	return directory + ".source-receipt.json"
}

func fixtureReviewEvaluationStores(
	t testing.TB, directory string,
) ReviewEvaluationReceiptStoreFactory {
	t.Helper()
	anchorDirectory := fixtureReviewEvaluationReceiptDirectory(directory)
	if err := os.Mkdir(anchorDirectory, 0o700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	quarantineDirectory := fixtureReviewEvaluationQuarantineDirectory(directory)
	if err := os.Mkdir(quarantineDirectory, 0o700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	return FileReviewEvaluationReceiptStoreFactory{
		Directory: anchorDirectory, QuarantineRoot: quarantineDirectory,
	}
}

func fixtureReviewEvaluationReceiptDirectory(directory string) string {
	return directory + ".evaluation-receipts"
}

func fixtureReviewEvaluationQuarantineDirectory(directory string) string {
	return directory + ".evaluation-quarantine"
}

func fixtureReviewEvaluationStageDirectory(finalDirectory string) string {
	identity := strings.TrimPrefix(
		reviewDigest([]byte(filepath.Base(finalDirectory))), "sha256:",
	)
	return filepath.Join(
		filepath.Dir(finalDirectory), ".openrealtime-evaluation-stage-"+identity[:24],
	)
}

func cuPNG(t testing.TB, width, height int, shade color.RGBA) []byte {
	t.Helper()
	frame := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			frame.SetRGBA(x, y, shade)
		}
	}
	var output bytes.Buffer
	if err := png.Encode(&output, frame); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func cuStructuralAVMP4(source string) []byte {
	fileType := append([]byte("mp42"), []byte{0, 0, 0, 0}...)
	fileType = append(fileType, []byte("mp42")...)
	movie := append(cuISOTrack("vide", "avc1"), cuISOTrack("soun", "mp4a")...)
	mediaData := append([]byte("realtime-cu-fixture-source:"), source...)
	return bytes.Join([][]byte{cuISOBox("ftyp", fileType), cuISOBox("moov", movie), cuISOBox("mdat", mediaData)}, nil)
}

func cuISOTrack(handler, codec string) []byte {
	handlerData := make([]byte, 12)
	copy(handlerData[8:12], handler)
	stsdData := make([]byte, 8)
	binary.BigEndian.PutUint32(stsdData[4:8], 1)
	stsdData = append(stsdData, cuISOBox(codec, nil)...)
	media := append(cuISOBox("hdlr", handlerData),
		cuISOBox("minf", cuISOBox("stbl", cuISOBox("stsd", stsdData)))...)
	return cuISOBox("trak", cuISOBox("mdia", media))
}

func cuISOBox(kind string, data []byte) []byte {
	result := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(result[:4], uint32(len(result)))
	copy(result[4:8], kind)
	copy(result[8:], data)
	return result
}

func appendReviewFile(t testing.TB, path string, payload []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func makeReviewTreeWritable(directory string) {
	_ = filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() {
			_ = os.Chmod(path, 0o700)
		} else if entry.Type().IsRegular() {
			_ = os.Chmod(path, 0o600)
		}
		return nil
	})
}

func waitForReviewBundleClosing(t testing.TB, bundle *ReviewBundle) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		bundle.mu.Lock()
		closing := bundle.closing
		bundle.mu.Unlock()
		if closing {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("FinishSuite did not close admission")
		case <-ticker.C:
		}
	}
}

func base64Raw(value string) string {
	return base64.RawStdEncoding.EncodeToString([]byte(value))
}

func BenchmarkReviewBundleSixteenHermeticReviewedCases(b *testing.B) {
	cases, err := Select(nil, nil)
	if err != nil || len(cases) != 16 {
		b.Fatalf("resolve sixteen cases: %v", err)
	}
	parent := b.TempDir()
	b.ReportAllocs()
	for iteration := 0; b.Loop(); iteration++ {
		directory := filepath.Join(parent, fmt.Sprintf("bundle-%06d", iteration))
		lease := newFixtureCUReviewerLease(b, &fixtureCUReviewer{})
		bundle, err := NewReviewBundle(ReviewBundleOptions{
			Directory: directory, VideoFactory: &fixtureReviewVideoFactory{}, Reviewer: lease,
			SourceAnchor:     fixtureReviewSourceAnchor(directory),
			EvaluationStores: fixtureReviewEvaluationStores(b, directory),
		})
		if err != nil {
			b.Fatal(err)
		}
		result := bench.Result{
			Suite: SuiteName, Cell: ReferenceCell(), Provenance: fixtureReviewProvenance(), Expected: 16,
		}
		for _, item := range cases {
			result.Tasks = append(result.Tasks, fixtureReviewAttempt(b, bundle, item, true))
		}
		result.Finish()
		if err := bundle.FinishSuite(context.Background(), result); err != nil {
			b.Fatal(err)
		}
		receipt, ok := bundle.Receipt()
		if !ok {
			b.Fatal("benchmark bundle has no verified receipt")
		}
		if _, err := VerifyReviewBundle(directory, receipt.ManifestSHA256); err != nil {
			b.Fatal(err)
		}
		if err := lease.Close(); err != nil {
			b.Fatal(err)
		}
		makeReviewTreeWritable(directory)
		if err := os.RemoveAll(directory); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(16, "cases/op")
}
