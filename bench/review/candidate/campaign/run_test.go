package campaign

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/bench/review/candidate/sourcebundle"
)

var fixtureAssessment = json.RawMessage(`{
  "media_usable": true,
  "observed_outcome": "pass",
  "agrees_with_deterministic": true,
  "confidence": 0.9,
  "summary": "The recording is usable and matches the deterministic scorer.",
  "significant_problems": [],
  "minor_observations": [],
  "limitations": []
}`)

type campaignProvider struct {
	descriptor     review.ProviderDescriptor
	capabilities   review.ProviderCapabilities
	implementation []byte
	configuration  []byte
	claimed        atomic.Bool
	closed         atomic.Bool
	calls          atomic.Int32
	mu             sync.Mutex
	failCalls      map[int32]error
}

func (provider *campaignProvider) Descriptor() review.ProviderDescriptor {
	return provider.descriptor
}

func (provider *campaignProvider) Capabilities() review.ProviderCapabilities {
	return provider.capabilities.Clone()
}

func (provider *campaignProvider) Implementation() []byte {
	return slices.Clone(provider.implementation)
}

func (provider *campaignProvider) Configuration() []byte {
	return slices.Clone(provider.configuration)
}

func (provider *campaignProvider) Claim() error {
	if !provider.claimed.CompareAndSwap(false, true) {
		return errors.New("campaign fixture provider already claimed")
	}
	return nil
}

func (provider *campaignProvider) Review(
	ctx context.Context, prepared review.PreparedRequest,
) (review.ProviderResponse, error) {
	if err := ctx.Err(); err != nil {
		return review.ProviderResponse{}, err
	}
	call := provider.calls.Add(1)
	provider.mu.Lock()
	failure := provider.failCalls[call]
	provider.mu.Unlock()
	if failure != nil {
		return review.ProviderResponse{}, failure
	}
	request := []byte(`{"attempt":` + strconvQuote(prepared.AttemptID) + `}`)
	raw := []byte(`{"id":` + strconvQuote("request-"+prepared.Case) + `}`)
	return review.ProviderResponse{
		Raw: raw, Output: slices.Clone(fixtureAssessment), ReportedModel: provider.descriptor.Model,
		RequestID: "request-" + prepared.Case, RequestIDState: review.ProviderRequestIDValue,
		Request: request,
	}, nil
}

func (*campaignProvider) VerifyResponse(
	ctx context.Context, _ review.PreparedRequest, _ review.ProviderResponse,
) error {
	return ctx.Err()
}

func (provider *campaignProvider) Close() error {
	provider.closed.Store(true)
	return nil
}

func fixtureLease(t testing.TB, provider *campaignProvider) *review.ProviderLease {
	t.Helper()
	provider.implementation = []byte("candidate campaign fixture implementation")
	provider.configuration = []byte(`{"mode":"hermetic"}`)
	provider.capabilities = review.ProviderCapabilities{
		MediaTypes: []string{"audio/wav"}, MaximumMediaCount: 1, MaximumMediaBytes: 64 << 20,
	}
	capabilitiesSHA, err := provider.capabilities.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	provider.descriptor = review.ProviderDescriptor{
		Provider: "fixture", Model: "fixture-multimodal", API: "fixture.interactions",
		APIRevision: "v1",
		Implementation: review.ContentIdentity{
			Version: "fixture.impl.v1", SHA256: testDigest(provider.implementation),
		},
		ConfigurationSHA256: testDigest(provider.configuration),
		CapabilitiesSHA256:  capabilitiesSHA,
	}
	registry, err := review.NewRegistry([]review.Registration{{
		Name: "fixture.multimodal", Descriptor: provider.descriptor,
		Capabilities: provider.capabilities, Implementation: provider.implementation,
		Configuration: provider.configuration,
		Factory:       func(context.Context) (review.Provider, error) { return provider, nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(context.Background(), "fixture.multimodal")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lease.Close(); err != nil {
			t.Errorf("close fixture review lease: %v", err)
		}
	})
	return lease
}

type campaignFixture struct {
	root          string
	source        string
	sourceReceipt string
	options       Options
}

func newCampaignFixture(t testing.TB, cases ...string) campaignFixture {
	t.Helper()
	root := t.TempDir()
	fixture := campaignFixture{
		root: root, source: filepath.Join(root, "source"),
		sourceReceipt: filepath.Join(root, "source.receipt.json"),
	}
	bundle, err := sourcebundle.New(sourcebundle.Options{
		Directory: fixture.source, ReceiptPath: fixture.sourceReceipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:9191/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	cell := bench.Reference()
	provenance := bench.Provenance{
		Revision: "campaign-fixture", StartedAt: "2026-08-30T00:00:00Z",
	}
	var outcomes []bench.TaskOutcome
	for index, caseID := range cases {
		specification, err := candidate.NewAttempt(
			"campaign-suite", caseID, index+1, cell, provenance, origin,
			map[string]any{"criterion": "listen to exact audio", "case": caseID},
		)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := bundle.BeginAttempt(context.Background(), specification)
		if err != nil {
			t.Fatal(err)
		}
		if err := attempt.CaptureAudio(bench.SessionAudioCapture{
			SampleRateHz: 24_000, RoomPCM16: []int16{1, 2, 3, 4},
			Agent: []bench.TimedAudioChunk{{AtMS: 0, PCM16: []int16{5, 6}}},
		}); err != nil {
			t.Fatal(err)
		}
		outcome := bench.TaskOutcome{ID: caseID, Completed: true, Passed: index%2 == 0}
		if err := attempt.Complete(context.Background(), candidate.Completion{
			Attempt: specification, Outcome: outcome,
			Transcript: bench.Transcript{PlaybackMS: 10, Moments: []bench.Moment{{
				AtMS: 1, Kind: bench.MomentAgentText, Text: "fixture",
			}}},
		}); err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, outcome)
	}
	result := bench.Result{
		Suite: "campaign-suite", Cell: cell, Provenance: provenance,
		Expected: len(outcomes), Tasks: outcomes,
	}
	result.Finish()
	if err := bundle.FinishSuite(context.Background(), result); err != nil {
		t.Fatal(err)
	}
	fixture.options = Options{
		SourceDirectory: fixture.source, SourceReceiptPath: fixture.sourceReceipt,
		EvaluationDirectory: filepath.Join(root, "evaluations"),
		ReceiptDirectory:    filepath.Join(root, "evaluation-receipts"),
		QuarantineDirectory: filepath.Join(root, "evaluation-quarantine"),
		SensitiveValues:     []string{"credential-canary-123"}, Concurrency: 3,
	}
	return fixture
}

func TestRunPublishesEveryEvaluationAndRecoversWithoutRepeatingProvider(t *testing.T) {
	fixture := newCampaignFixture(t, "case-c", "case-a", "case-b")
	provider := &campaignProvider{}
	fixture.options.Reviewer = fixtureLease(t, provider)
	result, err := Run(t.Context(), fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	if result.Expected != 3 || len(result.Evaluations) != 3 || provider.calls.Load() != 3 ||
		result.Provider != provider.descriptor {
		t.Fatalf("campaign result=%+v calls=%d", result, provider.calls.Load())
	}
	for _, evaluation := range result.Evaluations {
		if evaluation.Recovered || !evaluation.Assessment.MediaUsable ||
			evaluation.Receipt.Directory == "" || evaluation.ReceiptPath == "" {
			t.Fatalf("fresh evaluation = %+v", evaluation)
		}
		opened, err := review.VerifyEvaluationBundle(
			t.Context(), review.EvaluationBundleOptions{
				Directory:       evaluation.Receipt.Directory,
				SensitiveValues: fixture.options.SensitiveValues,
			}, evaluation.Receipt,
		)
		if err != nil || opened.Record.AttemptID != evaluation.AttemptID {
			t.Fatalf("VerifyEvaluationBundle(%s) = %+v, %v", evaluation.Case, opened, err)
		}
	}

	recovered, err := Run(t.Context(), fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 3 {
		t.Fatalf("recovery repeated provider calls: %d", provider.calls.Load())
	}
	for _, evaluation := range recovered.Evaluations {
		if !evaluation.Recovered {
			t.Fatalf("evaluation was not recovered: %+v", evaluation)
		}
	}
}

func TestRunResumesOnlyMissingEvaluationsAfterProviderFailure(t *testing.T) {
	fixture := newCampaignFixture(t, "case-a", "case-b", "case-c")
	provider := &campaignProvider{failCalls: map[int32]error{2: errors.New("fixture transport failed")}}
	fixture.options.Concurrency = 1
	fixture.options.Reviewer = fixtureLease(t, provider)
	partial, err := Run(t.Context(), fixture.options)
	if err == nil || !strings.Contains(err.Error(), "evaluate candidate review") {
		t.Fatalf("first Run() = %+v, %v", partial, err)
	}
	if provider.calls.Load() != 2 {
		t.Fatalf("provider calls before fail-fast = %d", provider.calls.Load())
	}
	provider.mu.Lock()
	provider.failCalls = nil
	provider.mu.Unlock()
	completed, err := Run(t.Context(), fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	if provider.calls.Load() != 4 {
		t.Fatalf("resume provider calls = %d, want 4 (one recovered, two fresh)", provider.calls.Load())
	}
	if !completed.Evaluations[0].Recovered || completed.Evaluations[1].Recovered ||
		completed.Evaluations[2].Recovered {
		t.Fatalf("resume states = %+v", completed.Evaluations)
	}
}

func TestRunRefusesIncompleteSourceBeforeProviderOrPublication(t *testing.T) {
	fixture := newCampaignFixture(t, "complete")
	provider := &campaignProvider{}
	fixture.options.Reviewer = fixtureLease(t, provider)
	manifest, receipt, err := sourcebundle.Verify(t.Context(), fixture.source, fixture.sourceReceipt)
	if err != nil {
		t.Fatal(err)
	}
	// A valid but incomplete source is produced independently; do not mutate the
	// already sealed fixture or fabricate a receipt.
	incompleteRoot := filepath.Join(fixture.root, "incomplete-source")
	incompleteReceipt := filepath.Join(fixture.root, "incomplete-source.receipt.json")
	bundle, err := sourcebundle.New(sourcebundle.Options{
		Directory: incompleteRoot, ReceiptPath: incompleteReceipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct through the public constructor so the test itself does not
	// depend on sourcebundle's indented wire encoding.
	entry := manifest.Attempts[0]
	specification, err := candidate.NewAttempt(
		manifest.Suite, entry.Case, entry.Trial,
		manifest.Cell, manifest.Provenance, manifest.Origin,
		map[string]any{"criterion": "incomplete"},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := bundle.BeginAttempt(t.Context(), specification)
	if err != nil {
		t.Fatal(err)
	}
	outcome := bench.TaskOutcome{ID: specification.Case, Error: "audio unavailable"}
	if err := attempt.Complete(t.Context(), candidate.Completion{
		Attempt: specification, Outcome: outcome, Transcript: bench.Transcript{},
	}); err == nil {
		t.Fatal("incomplete attempt unexpectedly completed")
	}
	result := bench.Result{
		Suite: specification.Suite, Cell: specification.Cell, Provenance: specification.Provenance,
		Expected: 1, Tasks: []bench.TaskOutcome{outcome},
	}
	result.Finish()
	if err := bundle.FinishSuite(t.Context(), result); err == nil {
		t.Fatal("incomplete source unexpectedly returned success")
	}
	fixture.options.SourceDirectory = incompleteRoot
	fixture.options.SourceReceiptPath = incompleteReceipt
	_, err = Run(t.Context(), fixture.options)
	if err == nil || !strings.Contains(err.Error(), "complete source evidence") {
		t.Fatalf("Run() incomplete source error = %v", err)
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("incomplete source reached provider %d times", provider.calls.Load())
	}
	for _, path := range []string{
		fixture.options.EvaluationDirectory, fixture.options.ReceiptDirectory,
		fixture.options.QuarantineDirectory,
	} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("incomplete source created campaign path %s: %v", path, statErr)
		}
	}
	if receipt.AttemptCount != 1 {
		t.Fatal("unrelated complete source receipt changed")
	}
}

func TestRunRejectsOverlappingOrSensitivePublicPathsBeforeCreatingRoots(t *testing.T) {
	fixture := newCampaignFixture(t, "case")
	provider := &campaignProvider{}
	fixture.options.Reviewer = fixtureLease(t, provider)
	for _, mutate := range []func(*Options){
		func(options *Options) {
			options.ReceiptDirectory = filepath.Join(options.EvaluationDirectory, "receipts")
		},
		func(options *Options) {
			options.SensitiveValues = []string{"credential-canary-123"}
			options.EvaluationDirectory = filepath.Join(fixture.root, "credential-canary-123")
		},
		func(options *Options) { options.Concurrency = maximumConcurrency + 1 },
	} {
		options := fixture.options
		mutate(&options)
		if _, err := Run(t.Context(), options); err == nil {
			t.Fatal("invalid campaign paths or concurrency were accepted")
		}
	}
	if provider.calls.Load() != 0 {
		t.Fatalf("invalid options reached provider %d times", provider.calls.Load())
	}
}

func TestRunRejectsReceiptAnchoredEvaluationForAnotherAttempt(t *testing.T) {
	fixture := newCampaignFixture(t, "case-a")
	provider := &campaignProvider{}
	fixture.options.Reviewer = fixtureLease(t, provider)
	if err := ensureCampaignDirectories(fixture.options); err != nil {
		t.Fatal(err)
	}
	source, err := sourcebundle.OpenReviewSource(
		t.Context(), fixture.source, fixture.sourceReceipt,
	)
	if err != nil {
		t.Fatal(err)
	}
	request, found, err := source.Next(t.Context(), fixture.options.SensitiveValues)
	if err != nil || !found {
		t.Fatalf("source.Next() = found %t, err %v", found, err)
	}
	if err := source.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	correctAttemptID := request.AttemptID
	request.AttemptID = "campaign-suite:another-case#1"
	request.Case = "another-case"
	name := digestName(correctAttemptID)
	publication, preparation, err := review.BeginEvaluationBundlePublication(
		t.Context(), review.EvaluationBundlePublicationConfig{
			Bundle: review.EvaluationBundleOptions{
				Directory:       filepath.Join(fixture.options.EvaluationDirectory, name),
				SensitiveValues: fixture.options.SensitiveValues,
			},
			ReceiptStore: review.FileEvaluationBundleReceiptStore{
				Path: filepath.Join(fixture.options.ReceiptDirectory, name+".receipt.json"),
			},
			QuarantineDirectory: fixture.options.QuarantineDirectory,
		},
	)
	if err != nil || preparation.Recovered != nil {
		t.Fatalf("BeginEvaluationBundlePublication() = %+v, %v", preparation, err)
	}
	evaluation, err := review.Evaluate(t.Context(), fixture.options.Reviewer, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publication.Write(t.Context(), evaluation); err != nil {
		t.Fatal(err)
	}
	if err := publication.Close(); err != nil {
		t.Fatal(err)
	}
	callsBefore := provider.calls.Load()
	if _, err := Run(t.Context(), fixture.options); err == nil ||
		!strings.Contains(err.Error(), "identity differs") {
		t.Fatalf("Run() wrong recovered evaluation error = %v", err)
	}
	if provider.calls.Load() != callsBefore {
		t.Fatal("wrong recovered evaluation caused a second provider invocation")
	}
}

func BenchmarkRunExact16CandidateAudioCampaign(b *testing.B) {
	cases := make([]string, 16)
	for index := range cases {
		cases[index] = "case-" + benchmarkIndex(index)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		fixture := newCampaignFixture(b, cases...)
		provider := &campaignProvider{}
		fixture.options.Reviewer = fixtureLease(b, provider)
		fixture.options.Concurrency = 4
		b.StartTimer()
		result, err := Run(context.Background(), fixture.options)
		if err != nil {
			b.Fatal(err)
		}
		if len(result.Evaluations) != len(cases) {
			b.Fatalf("evaluations = %d, want %d", len(result.Evaluations), len(cases))
		}
	}
}

func benchmarkIndex(value int) string {
	if value == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buffer [32]byte
	position := len(buffer)
	for value > 0 {
		position--
		buffer[position] = digits[value%10]
		value /= 10
	}
	return string(buffer[position:])
}

func testDigest(payload []byte) string {
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func strconvQuote(value string) string {
	payload, _ := json.Marshal(value)
	return string(payload)
}
