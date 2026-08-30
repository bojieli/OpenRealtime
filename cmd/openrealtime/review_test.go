package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	benchreview "github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
)

type scenarioEvaluationFixtureProvider struct {
	descriptor     benchreview.ProviderDescriptor
	capabilities   benchreview.ProviderCapabilities
	implementation []byte
	configuration  []byte
	claimed        atomic.Bool
	reviewCalls    atomic.Int32
	closeCalls     atomic.Int32
	fail           bool
	assessment     *benchreview.Assessment
}

func (provider *scenarioEvaluationFixtureProvider) Descriptor() benchreview.ProviderDescriptor {
	return provider.descriptor
}

func (provider *scenarioEvaluationFixtureProvider) Capabilities() benchreview.ProviderCapabilities {
	return provider.capabilities.Clone()
}

func (provider *scenarioEvaluationFixtureProvider) Implementation() []byte {
	return bytes.Clone(provider.implementation)
}

func (provider *scenarioEvaluationFixtureProvider) Configuration() []byte {
	return bytes.Clone(provider.configuration)
}

func (provider *scenarioEvaluationFixtureProvider) Claim() error {
	if !provider.claimed.CompareAndSwap(false, true) {
		return errors.New("fixture provider already claimed")
	}
	return nil
}

func (provider *scenarioEvaluationFixtureProvider) Review(
	ctx context.Context, request benchreview.PreparedRequest,
) (benchreview.ProviderResponse, error) {
	provider.reviewCalls.Add(1)
	if err := ctx.Err(); err != nil {
		return benchreview.ProviderResponse{}, err
	}
	if provider.fail {
		return benchreview.ProviderResponse{}, errors.New("fixture review unavailable")
	}
	var source graphnative.SourceReviewContext
	if err := json.Unmarshal(request.Context, &source); err != nil {
		return benchreview.ProviderResponse{}, errors.New("fixture source context is invalid")
	}
	observed := "pass"
	if source.Attempt.Behavior == graphnative.BehaviorFailed {
		observed = "fail"
	}
	result := benchreview.Assessment{
		MediaUsable: true, ObservedOutcome: observed,
		AgreesWithDeterministic: true, Confidence: 0.9,
		Summary:             "The retained fixture media is usable and matches the deterministic result.",
		SignificantProblems: []benchreview.Finding{},
		MinorObservations:   []benchreview.Finding{},
		Limitations:         []string{"Hermetic fixture evaluator; no remote-service attestation."},
	}
	if provider.assessment != nil {
		result = *provider.assessment
	}
	assessment, err := json.Marshal(result)
	if err != nil {
		return benchreview.ProviderResponse{}, err
	}
	return benchreview.ProviderResponse{
		Raw: []byte(`{"fixture":"complete"}`), Output: assessment,
		ReportedModel: provider.descriptor.Model,
		RequestID:     "fixture-request", RequestIDState: benchreview.ProviderRequestIDValue,
		Request: []byte(`{"fixture":"request"}`),
	}, nil
}

func (provider *scenarioEvaluationFixtureProvider) VerifyResponse(
	ctx context.Context, _ benchreview.PreparedRequest, _ benchreview.ProviderResponse,
) error {
	return ctx.Err()
}

func (provider *scenarioEvaluationFixtureProvider) Close() error {
	provider.closeCalls.Add(1)
	return nil
}

func scenarioEvaluationFixtureRegistry(
	t testing.TB, fail bool,
) (*benchreview.Registry, *scenarioEvaluationFixtureProvider) {
	return scenarioEvaluationFixtureRegistryNamed(t, "fixture.scenario-review", fail)
}

func scenarioEvaluationFixtureRegistryNamed(
	t testing.TB, name string, fail bool,
) (*benchreview.Registry, *scenarioEvaluationFixtureProvider) {
	t.Helper()
	implementation := []byte("openrealtime scenario evaluation fixture implementation v1")
	configuration := []byte(`{"fixture":"scenario"}`)
	capabilities := benchreview.ProviderCapabilities{
		MediaTypes:        []string{"audio/wav", "image/jpeg", "image/png"},
		MaximumMediaCount: 3, MaximumMediaBytes: 8 << 20,
	}
	capabilitiesSHA, err := capabilities.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	descriptor := benchreview.ProviderDescriptor{
		Provider: "fixture", Model: "fixture-scenario-review-v1",
		API: "fixture.review", APIRevision: "v1",
		Implementation: benchreview.ContentIdentity{
			Version: "fixture.scenario-review.v1", SHA256: scenarioEvaluationDigest(implementation),
		},
		ConfigurationSHA256: scenarioEvaluationDigest(configuration),
		CapabilitiesSHA256:  capabilitiesSHA,
	}
	provider := &scenarioEvaluationFixtureProvider{
		descriptor: descriptor, capabilities: capabilities,
		implementation: implementation, configuration: configuration, fail: fail,
	}
	registry, err := benchreview.NewRegistry([]benchreview.Registration{{
		Name: name, Descriptor: descriptor,
		Capabilities: capabilities, Implementation: implementation, Configuration: configuration,
		Factory: func(context.Context) (benchreview.Provider, error) { return provider, nil },
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registry, provider
}

func TestScenarioEvaluationLeavesCredentialScanningToSelectedProvider(t *testing.T) {
	t.Chdir("../..")
	sourceDirectory, _, _ := publishScenarioGraphPopulationFixture(t, 1, true)
	// This value occurs in a case prompt. The command layer must not reinterpret
	// a provider-owned credential as a generic declared source secret; the real
	// Gemini plug-in independently rejects literal, split-token, media, and
	// encoding-synthesized credential material before transport.
	t.Setenv("GEMINI_API_KEY", "ordering from a waiter")
	registry, provider := scenarioEvaluationFixtureRegistryNamed(
		t, "google.gemini-3.7-flash", false,
	)
	outputDirectory := filepath.Join(t.TempDir(), "provider-owned-credential")
	if err := runScenarioEvaluation([]string{
		"-source-dir", sourceDirectory,
		"-source-receipt", sourceDirectory + ".receipt.json",
		"-out", outputDirectory,
		"-provider", "google.gemini-3.7-flash",
		"-parallel", "3",
	}, &bytes.Buffer{}, registry); err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls.Load() != 11 || provider.closeCalls.Load() != 1 {
		t.Fatalf("provider-owned credential run = review %d close %d",
			provider.reviewCalls.Load(), provider.closeCalls.Load())
	}
}

func TestScenarioEvaluationTimelineUsesSealedMediaDuration(t *testing.T) {
	requestForDuration := func(duration int64) benchreview.Request {
		contextPayload, err := json.Marshal(graphnative.SourceReviewContext{
			Format:          graphnative.SourceReviewContextFormat,
			FormatVersion:   graphnative.SourceReviewContextFormatVersion,
			MediaDurationMS: duration,
		})
		if err != nil {
			t.Fatal(err)
		}
		return benchreview.Request{Context: contextPayload}
	}
	value := func(candidate int64) *int64 { return &candidate }
	within := benchreview.Assessment{
		SignificantProblems: []benchreview.Finding{{StartMS: value(0), EndMS: value(1000)}},
		MinorObservations:   []benchreview.Finding{{StartMS: nil, EndMS: nil}},
	}
	if err := validateScenarioEvaluationTimeline(requestForDuration(1000), within); err != nil {
		t.Fatalf("inclusive duration boundary rejected: %v", err)
	}
	for _, test := range []struct {
		name       string
		request    benchreview.Request
		assessment benchreview.Assessment
		match      string
	}{
		{
			name: "maximum plus one", request: requestForDuration(1000),
			assessment: benchreview.Assessment{SignificantProblems: []benchreview.Finding{{
				StartMS: value(1001),
			}}}, match: "sealed media duration",
		},
		{
			name: "negative programmatic timestamp", request: requestForDuration(1000),
			assessment: benchreview.Assessment{MinorObservations: []benchreview.Finding{{
				EndMS: value(-1),
			}}}, match: "sealed media duration",
		},
		{
			name: "missing duration", request: requestForDuration(0),
			assessment: within, match: "media duration is invalid",
		},
		{
			name: "wrong context version",
			request: benchreview.Request{Context: json.RawMessage(
				`{"format":"openrealtime.scenario-source-review-context","format_version":1,"media_duration_ms":1000}`,
			)},
			assessment: within, match: "media duration is invalid",
		},
		{
			name: "trailing context",
			request: benchreview.Request{Context: json.RawMessage(
				`{"format":"openrealtime.scenario-source-review-context","format_version":2,"media_duration_ms":1000} {}`,
			)},
			assessment: within, match: "context is invalid",
		},
		{
			name: "duration beyond schema horizon", request: requestForDuration(86_400_001),
			assessment: within, match: "media duration is invalid",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateScenarioEvaluationTimeline(test.request, test.assessment); err == nil ||
				!strings.Contains(err.Error(), test.match) {
				t.Fatalf("timeline validation error = %v, want %q", err, test.match)
			}
		})
	}
}

func TestScenarioEvaluationRejectsOutOfMediaTimestampBeforeRetention(t *testing.T) {
	t.Chdir("../..")
	sourceDirectory, sourceReceipt, _ := publishScenarioGraphPopulationFixture(t, 1, false)
	registry, provider := scenarioEvaluationFixtureRegistry(t, false)
	beyond := int64(86_400_000)
	provider.assessment = &benchreview.Assessment{
		MediaUsable: true, ObservedOutcome: "pass", AgreesWithDeterministic: true,
		Confidence: 0.9, Summary: "The media appears usable.",
		SignificantProblems: []benchreview.Finding{{
			Category: "timing", StartMS: &beyond, Evidence: "A late event was reported.",
			Impact: "The reported timestamp is outside the recording.",
		}},
		MinorObservations: []benchreview.Finding{}, Limitations: []string{},
	}
	outputDirectory := filepath.Join(t.TempDir(), "invalid-timestamp")
	err := runScenarioEvaluation([]string{
		"-source-dir", sourceDirectory,
		"-source-receipt", sourceDirectory + ".receipt.json",
		"-out", outputDirectory,
		"-provider", "fixture.scenario-review",
		"-parallel", "1",
	}, &bytes.Buffer{}, registry)
	if err == nil || !strings.Contains(err.Error(), "sealed media duration") ||
		provider.reviewCalls.Load() != 1 || provider.closeCalls.Load() != 1 {
		t.Fatalf("out-of-media review = %v, calls=%d close=%d",
			err, provider.reviewCalls.Load(), provider.closeCalls.Load())
	}
	entries, readErr := os.ReadDir(outputDirectory)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("out-of-media review retained entries = %d, %v", len(entries), readErr)
	}
	if _, err := os.Lstat(outputDirectory + ".receipt.json"); !os.IsNotExist(err) {
		t.Fatalf("out-of-media review published aggregate receipt: %v", err)
	}
	if _, err := graphnative.VerifySourceBundle(
		t.Context(), graphnative.SourceBundleOptions{Directory: sourceDirectory}, sourceReceipt,
	); err != nil {
		t.Fatalf("out-of-media review changed source: %v", err)
	}
}

func TestScenarioEvaluationPublishesAndReopensAllElevenAttempts(t *testing.T) {
	t.Chdir("../..")
	sourceDirectory, sourceReceipt, _ := publishScenarioGraphPopulationFixture(t, 1, true)
	outputDirectory := filepath.Join(t.TempDir(), "scenario-evaluations")
	registry, provider := scenarioEvaluationFixtureRegistry(t, false)
	var output bytes.Buffer
	err := runScenarioEvaluation([]string{
		"-source-dir", sourceDirectory,
		"-source-receipt", sourceDirectory + ".receipt.json",
		"-out", outputDirectory,
		"-provider", "fixture.scenario-review",
		"-parallel", "3",
		"-timeout", time.Minute.String(),
	}, &output, registry)
	if err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls.Load() != 11 || provider.closeCalls.Load() != 1 {
		t.Fatalf("fixture provider calls = review %d close %d", provider.reviewCalls.Load(), provider.closeCalls.Load())
	}
	if strings.Count(output.String(), "  reviewed     ") != 11 ||
		!strings.Contains(output.String(), "evaluations  11/11") ||
		!strings.Contains(output.String(), sourceReceipt.ReceiptSHA256) {
		t.Fatalf("scenario evaluation output = %q", output.String())
	}
	manifestPayload, err := os.ReadFile(filepath.Join(outputDirectory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	index, err := decodeScenarioEvaluationIndex(manifestPayload)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Entries) != 11 || index.Entries[0].DeterministicBehavior != graphnative.BehaviorFailed ||
		index.Entries[0].Assessment.ObservedOutcome != "fail" ||
		index.Entries[1].DeterministicBehavior != graphnative.BehaviorPassed ||
		index.Entries[1].Assessment.ObservedOutcome != "pass" {
		t.Fatalf("scenario evaluation index entries = %+v", index.Entries)
	}
	receiptPayload, err := os.ReadFile(outputDirectory + ".receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeScenarioEvaluationIndexReceipt(receiptPayload)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveScenarioEvaluationOptions(scenarioEvaluationRunOptions{
		SourceDirectory: sourceDirectory, SourceReceipt: sourceDirectory + ".receipt.json",
		OutputDirectory: filepath.Join(t.TempDir(), "unused-output"),
		Provider:        "fixture.scenario-review", Parallel: 3, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved.OutputDirectory = outputDirectory
	resolved.OutputReceipt = outputDirectory + ".receipt.json"
	if err := verifyScenarioEvaluationCollection(
		t.Context(), resolved, sourceReceipt, receipt,
	); err != nil {
		t.Fatal(err)
	}
	var verifyOutput bytes.Buffer
	if err := runScenarioEvaluationVerification([]string{
		"-source-dir", sourceDirectory,
		"-source-receipt", sourceDirectory + ".receipt.json",
		"-evaluation-dir", outputDirectory,
		"-evaluation-receipt", outputDirectory + ".receipt.json",
	}, &verifyOutput); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(verifyOutput.String(), "evaluations  11/11 verified") ||
		!strings.Contains(verifyOutput.String(), receipt.ReceiptSHA256) {
		t.Fatalf("scenario verification output = %q", verifyOutput.String())
	}
	entries, err := os.ReadDir(outputDirectory)
	if err != nil || len(entries) != 24 {
		t.Fatalf("scenario evaluation root entries = %d, %v; want 24", len(entries), err)
	}
	review, err := os.ReadFile(filepath.Join(outputDirectory, "REVIEW.md"))
	if err != nil || !bytes.Contains(review, []byte("## Case —")) ||
		!bytes.Contains(review, []byte("Deterministic behavior: **failed**")) {
		t.Fatalf("scenario evaluation human review = %q, %v", review, err)
	}
	if _, err := graphnative.VerifySourceBundle(
		t.Context(), graphnative.SourceBundleOptions{Directory: sourceDirectory}, sourceReceipt,
	); err != nil {
		t.Fatalf("source changed during secondary review: %v", err)
	}
	if err := os.Remove(outputDirectory + ".receipt.json"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sourceDirectory+".receipt.json", outputDirectory+".receipt.json"); err != nil {
		t.Fatal(err)
	}
	if err := runScenarioEvaluationVerification([]string{
		"-source-dir", sourceDirectory,
		"-source-receipt", sourceDirectory + ".receipt.json",
		"-evaluation-dir", outputDirectory,
		"-evaluation-receipt", outputDirectory + ".receipt.json",
	}, &bytes.Buffer{}); err == nil {
		t.Fatal("symlinked scenario evaluation receipt unexpectedly verified")
	}
}

func TestScenarioEvaluationPublishesOneHundredSixtyFiveAttemptPopulation(t *testing.T) {
	t.Chdir("../..")
	sourceDirectory, sourceReceipt, outcome := publishScenarioGraphPopulationFixture(
		t, graphnative.MinimumReportableRepetitions, true,
	)
	if outcome.Checklist.Expected != 165 || outcome.Checklist.FailedAttempts != 1 {
		t.Fatalf("sealed source checklist = %+v", outcome.Checklist)
	}
	outputDirectory := filepath.Join(t.TempDir(), "scenario-evaluations-165")
	registry, provider := scenarioEvaluationFixtureRegistry(t, false)
	var output bytes.Buffer
	if err := runScenarioEvaluation([]string{
		"-source-dir", sourceDirectory,
		"-source-receipt", sourceDirectory + ".receipt.json",
		"-out", outputDirectory,
		"-provider", "fixture.scenario-review",
		"-parallel", "8",
		"-timeout", time.Minute.String(),
	}, &output, registry); err != nil {
		t.Fatal(err)
	}
	if provider.reviewCalls.Load() != 165 || provider.closeCalls.Load() != 1 ||
		strings.Count(output.String(), "  reviewed     ") != 165 ||
		!strings.Contains(output.String(), "evaluations  165/165") {
		t.Fatalf("165 evaluation calls = %d close=%d output=%q",
			provider.reviewCalls.Load(), provider.closeCalls.Load(), output.String())
	}
	manifestPayload, err := os.ReadFile(filepath.Join(outputDirectory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	index, err := decodeScenarioEvaluationIndex(manifestPayload)
	if err != nil {
		t.Fatal(err)
	}
	if index.Expected != 165 || len(index.Entries) != 165 ||
		index.Entries[0].DeterministicBehavior != graphnative.BehaviorFailed ||
		index.Entries[164].Ordinal != 165 {
		t.Fatalf("165 scenario evaluation index = %+v", index)
	}
	entries, err := os.ReadDir(outputDirectory)
	if err != nil || len(entries) != 332 {
		t.Fatalf("165 scenario evaluation root entries = %d, %v; want 332", len(entries), err)
	}
	receiptPayload, err := os.ReadFile(outputDirectory + ".receipt.json")
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := decodeScenarioEvaluationIndexReceipt(receiptPayload)
	if err != nil {
		t.Fatal(err)
	}
	resolved := scenarioEvaluationRunOptions{
		SourceDirectory: sourceDirectory, SourceReceipt: sourceDirectory + ".receipt.json",
		OutputDirectory: outputDirectory, OutputReceipt: outputDirectory + ".receipt.json",
		Provider: "fixture.scenario-review", Parallel: 8, Timeout: time.Minute,
	}
	if err := verifyScenarioEvaluationCollection(
		t.Context(), resolved, sourceReceipt, receipt,
	); err != nil {
		t.Fatal(err)
	}
	var allocationErr error
	allocations := testing.AllocsPerRun(1, func() {
		allocationErr = verifyScenarioEvaluationCollection(
			context.Background(), resolved, sourceReceipt, receipt,
		)
	})
	if allocationErr != nil {
		t.Fatal(allocationErr)
	}
	if allocations > 2_350_000 {
		t.Fatalf("165 evaluation verification allocations = %.0f, want <= 2350000", allocations)
	}
}

func TestScenarioEvaluationVerifiesSourceBeforeOpeningProvider(t *testing.T) {
	t.Chdir("../..")
	sourceDirectory, sourceReceipt, _ := publishScenarioGraphPopulationFixture(t, 1, false)
	verified, err := graphnative.VerifySourceBundle(
		t.Context(), graphnative.SourceBundleOptions{Directory: sourceDirectory}, sourceReceipt,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sourceDirectory, verified.Manifest.Attempts[0].Result.Path), []byte("{}"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	registry, provider := scenarioEvaluationFixtureRegistry(t, false)
	outputDirectory := filepath.Join(t.TempDir(), "must-not-exist")
	err = runScenarioEvaluation([]string{
		"-source-dir", sourceDirectory,
		"-source-receipt", sourceDirectory + ".receipt.json",
		"-out", outputDirectory,
		"-provider", "fixture.scenario-review",
	}, &bytes.Buffer{}, registry)
	if err == nil || provider.claimed.Load() || provider.reviewCalls.Load() != 0 {
		t.Fatalf("tampered source review error = %v, claimed=%t calls=%d", err, provider.claimed.Load(), provider.reviewCalls.Load())
	}
	if _, err := os.Lstat(outputDirectory); !os.IsNotExist(err) {
		t.Fatalf("tampered source created output: %v", err)
	}
}

func TestScenarioEvaluationCanceledContextDoesNotOpenProviderOrCreateOutput(t *testing.T) {
	registry, provider := scenarioEvaluationFixtureRegistry(t, false)
	root := t.TempDir()
	outputDirectory := filepath.Join(root, "must-not-exist")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := runScenarioEvaluationContext(ctx, []string{
		"-source-dir", filepath.Join(root, "source"),
		"-source-receipt", filepath.Join(root, "source.receipt.json"),
		"-out", outputDirectory,
		"-provider", "fixture.scenario-review",
	}, &bytes.Buffer{}, registry)
	if !errors.Is(err, context.Canceled) || provider.claimed.Load() || provider.reviewCalls.Load() != 0 {
		t.Fatalf("canceled scenario evaluation = %v, claimed=%t calls=%d",
			err, provider.claimed.Load(), provider.reviewCalls.Load())
	}
	if _, err := os.Lstat(outputDirectory); !os.IsNotExist(err) {
		t.Fatalf("canceled scenario evaluation created output: %v", err)
	}
}

func TestScenarioEvaluationProviderFailureLeavesSourceValidAndNoAggregateMarker(t *testing.T) {
	t.Chdir("../..")
	sourceDirectory, sourceReceipt, _ := publishScenarioGraphPopulationFixture(t, 1, false)
	registry, provider := scenarioEvaluationFixtureRegistry(t, true)
	outputDirectory := filepath.Join(t.TempDir(), "failed-evaluations")
	err := runScenarioEvaluation([]string{
		"-source-dir", sourceDirectory,
		"-source-receipt", sourceDirectory + ".receipt.json",
		"-out", outputDirectory,
		"-provider", "fixture.scenario-review",
		"-parallel", "4",
	}, &bytes.Buffer{}, registry)
	if err == nil || provider.reviewCalls.Load() == 0 || provider.closeCalls.Load() != 1 {
		t.Fatalf("provider failure = %v calls=%d close=%d", err, provider.reviewCalls.Load(), provider.closeCalls.Load())
	}
	if _, err := os.Lstat(filepath.Join(outputDirectory, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("failed evaluation published aggregate marker: %v", err)
	}
	if _, err := os.Lstat(outputDirectory + ".receipt.json"); !os.IsNotExist(err) {
		t.Fatalf("failed evaluation published aggregate receipt: %v", err)
	}
	if _, err := graphnative.VerifySourceBundle(
		t.Context(), graphnative.SourceBundleOptions{Directory: sourceDirectory}, sourceReceipt,
	); err != nil {
		t.Fatalf("provider failure changed source: %v", err)
	}
}

func BenchmarkScenarioEvaluationVerifyOneHundredSixtyFiveAttempts(b *testing.B) {
	b.Chdir("../..")
	sourceDirectory, sourceReceipt, _ := publishScenarioGraphPopulationFixture(
		b, graphnative.MinimumReportableRepetitions, true,
	)
	outputDirectory := filepath.Join(b.TempDir(), "scenario-evaluations-165")
	registry, _ := scenarioEvaluationFixtureRegistry(b, false)
	if err := runScenarioEvaluation([]string{
		"-source-dir", sourceDirectory,
		"-source-receipt", sourceDirectory + ".receipt.json",
		"-out", outputDirectory,
		"-provider", "fixture.scenario-review",
		"-parallel", "8",
		"-timeout", time.Minute.String(),
	}, &bytes.Buffer{}, registry); err != nil {
		b.Fatal(err)
	}
	receiptPayload, err := os.ReadFile(outputDirectory + ".receipt.json")
	if err != nil {
		b.Fatal(err)
	}
	receipt, err := decodeScenarioEvaluationIndexReceipt(receiptPayload)
	if err != nil {
		b.Fatal(err)
	}
	options := scenarioEvaluationRunOptions{
		SourceDirectory: sourceDirectory, SourceReceipt: sourceDirectory + ".receipt.json",
		OutputDirectory: outputDirectory, OutputReceipt: outputDirectory + ".receipt.json",
		Provider: "fixture.scenario-review", Parallel: 8, Timeout: time.Minute,
	}
	b.ReportAllocs()
	b.ReportMetric(165, "attempts/op")
	b.ResetTimer()
	for range b.N {
		if err := verifyScenarioEvaluationCollection(
			b.Context(), options, sourceReceipt, receipt,
		); err != nil {
			b.Fatal(err)
		}
	}
}
