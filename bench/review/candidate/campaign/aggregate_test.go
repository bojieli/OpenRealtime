package campaign

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type aggregateFixture struct {
	campaign campaignFixture
	provider *campaignProvider
	result   Result
	options  AggregateOptions
}

func newAggregateFixture(t testing.TB, cases ...string) aggregateFixture {
	t.Helper()
	campaignFixture := newCampaignFixture(t, cases...)
	provider := &campaignProvider{assessment: []byte(`{
  "media_usable": true,
  "observed_outcome": "pass",
  "agrees_with_deterministic": true,
  "confidence": 0.9,
  "summary": "The exact recording is usable for review.",
  "significant_problems": [{
    "category": "latency",
    "start_ms": 1,
    "end_ms": 2,
    "evidence": "A short pause is audible.",
    "impact": "The response feels slightly delayed."
  }],
  "minor_observations": [],
  "limitations": []
}`)}
	campaignFixture.options.Reviewer = fixtureLease(t, provider)
	result, err := Run(context.Background(), campaignFixture.options)
	if err != nil {
		t.Fatal(err)
	}
	return aggregateFixture{
		campaign: campaignFixture, provider: provider, result: result,
		options: AggregateOptions{
			Directory:           filepath.Join(campaignFixture.root, "aggregate"),
			ReceiptPath:         filepath.Join(campaignFixture.root, "aggregate.receipt.json"),
			SourceReceiptPath:   campaignFixture.sourceReceipt,
			QuarantineDirectory: filepath.Join(campaignFixture.root, "aggregate-quarantine"),
			SensitiveValues:     campaignFixture.options.SensitiveValues,
		},
	}
}

func TestPublishAggregateProducesCaseByCaseMediaReviewAndRecovers(t *testing.T) {
	fixture := newAggregateFixture(t, "case-c", "case-a", "case-b")
	calls := fixture.provider.calls.Load()
	bundle, err := PublishAggregate(t.Context(), fixture.options, fixture.result)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.provider.calls.Load() != calls {
		t.Fatal("aggregate publication invoked the advisory provider")
	}
	if bundle.Manifest.EvaluationCount != 3 || !bundle.Manifest.Complete ||
		bundle.Receipt.EvaluationCount != 3 || bundle.Manifest.Source != fixture.result.Source ||
		bundle.Manifest.Provider != fixture.result.Provider {
		t.Fatalf("aggregate bundle = %+v", bundle)
	}
	for _, evaluation := range bundle.Manifest.Evaluations {
		if len(evaluation.Media) != 1 || evaluation.Media[0].MediaType != "audio/wav" ||
			evaluation.Media[0].SHA256 == "" || evaluation.Media[0].SizeBytes <= 0 {
			t.Fatalf("aggregate evaluation media = %+v", evaluation.Media)
		}
		if _, err := os.Stat(evaluation.Media[0].Path); err != nil {
			t.Fatalf("retained media %s: %v", evaluation.Media[0].Path, err)
		}
	}
	reviewPayload, err := os.ReadFile(filepath.Join(fixture.options.Directory, aggregateReviewName))
	if err != nil {
		t.Fatal(err)
	}
	reviewText := string(reviewPayload)
	for _, required := range []string{
		"# Candidate case-by-case media review", "Deterministic: 2 pass, 1 fail",
		"Advisory agreement: 3/3", "case-a", "case-b", "case-c",
		"media 1 (audio/wav)", "Evaluation receipt: [open]", "[1–2 ms] latency",
		fixture.result.Source.ManifestSHA256,
	} {
		if !strings.Contains(reviewText, required) {
			t.Fatalf("REVIEW.md is missing %q:\n%s", required, reviewText)
		}
	}

	verified, err := VerifyAggregate(t.Context(), fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	if !sameCampaignResult(verified.Result, fixture.result) || verified.Receipt != bundle.Receipt {
		t.Fatalf("verified aggregate differs: %+v", verified)
	}
	recovered, err := PublishAggregate(t.Context(), fixture.options, fixture.result)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Receipt != bundle.Receipt || fixture.provider.calls.Load() != calls {
		t.Fatalf("recovered aggregate/provider calls = %+v/%d", recovered.Receipt, fixture.provider.calls.Load())
	}
}

func TestPublishAggregateRecoversReceiptCommittedStageAfterInterruption(t *testing.T) {
	fixture := newAggregateFixture(t, "case-a", "case-b")
	interrupted := errors.New("simulated process interruption")
	if _, err := publishAggregate(
		t.Context(), fixture.options, fixture.result,
		aggregatePublishOperations{afterReceipt: func() error { return interrupted }},
	); !errors.Is(err, interrupted) {
		t.Fatalf("publishAggregate() interruption = %v", err)
	}
	if _, err := os.Stat(fixture.options.ReceiptPath); err != nil {
		t.Fatalf("committed receipt missing: %v", err)
	}
	if _, err := os.Stat(aggregateStageDirectory(fixture.options.Directory)); err != nil {
		t.Fatalf("staged aggregate missing: %v", err)
	}
	if _, err := os.Stat(fixture.options.Directory); !os.IsNotExist(err) {
		t.Fatalf("final aggregate exists before recovery: %v", err)
	}
	bundle, err := PublishAggregate(t.Context(), fixture.options, fixture.result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(fixture.options.Directory); err != nil {
		t.Fatalf("recovered aggregate missing: %v", err)
	}
	if _, err := os.Stat(aggregateStageDirectory(fixture.options.Directory)); !os.IsNotExist(err) {
		t.Fatalf("stage remained after recovery: %v", err)
	}
	if _, err := VerifyAggregate(t.Context(), fixture.options); err != nil || !bundle.Manifest.Complete {
		t.Fatalf("VerifyAggregate() after recovery = %v", err)
	}
}

func TestPublishAggregateQuarantinesUnreceiptedStage(t *testing.T) {
	fixture := newAggregateFixture(t, "case-a")
	stage := aggregateStageDirectory(fixture.options.Directory)
	if err := os.Mkdir(stage, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "partial"), []byte("diagnostic debris"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := PublishAggregate(t.Context(), fixture.options, fixture.result); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(fixture.options.QuarantineDirectory)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("quarantine entries = %+v, %v", entries, err)
	}
	retained := filepath.Join(fixture.options.QuarantineDirectory, entries[0].Name(), "partial")
	if payload, err := os.ReadFile(retained); err != nil || string(payload) != "diagnostic debris" {
		t.Fatalf("quarantined debris = %q, %v", payload, err)
	}
}

func TestPublishAggregateRejectsWrongRecoveredResult(t *testing.T) {
	fixture := newAggregateFixture(t, "case-a")
	if _, err := PublishAggregate(t.Context(), fixture.options, fixture.result); err != nil {
		t.Fatal(err)
	}
	wrong := cloneResult(fixture.result)
	wrong.Evaluations[0].Assessment.Summary = "different advisory result"
	if _, err := PublishAggregate(t.Context(), fixture.options, wrong); err == nil ||
		!strings.Contains(err.Error(), "differs from requested result") {
		t.Fatalf("PublishAggregate() wrong recovery = %v", err)
	}
}

func TestPublishAggregateRefusesPartialCampaignWithoutPublishingMarker(t *testing.T) {
	campaignFixture := newCampaignFixture(t, "case-a", "case-b")
	provider := &campaignProvider{failCalls: map[int32]error{1: errors.New("provider unavailable")}}
	campaignFixture.options.Concurrency = 1
	campaignFixture.options.Reviewer = fixtureLease(t, provider)
	partial, runErr := Run(t.Context(), campaignFixture.options)
	if runErr == nil {
		t.Fatal("partial campaign unexpectedly succeeded")
	}
	options := AggregateOptions{
		Directory:           filepath.Join(campaignFixture.root, "aggregate"),
		ReceiptPath:         filepath.Join(campaignFixture.root, "aggregate.receipt.json"),
		SourceReceiptPath:   campaignFixture.sourceReceipt,
		QuarantineDirectory: filepath.Join(campaignFixture.root, "aggregate-quarantine"),
		SensitiveValues:     campaignFixture.options.SensitiveValues,
	}
	if _, err := PublishAggregate(t.Context(), options, partial); err == nil {
		t.Fatal("partial campaign aggregate unexpectedly succeeded")
	}
	for _, path := range []string{options.Directory, options.ReceiptPath, options.QuarantineDirectory} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("partial campaign created %s: %v", path, err)
		}
	}
}

func TestPublishAggregateRejectsSymlinkAndHardlinkEvaluationReceipts(t *testing.T) {
	for _, test := range []struct {
		name string
		link func(string, string) error
	}{
		{name: "symlink", link: os.Symlink},
		{name: "hardlink", link: os.Link},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAggregateFixture(t, "case-a")
			linked := filepath.Join(fixture.campaign.root, test.name+".receipt.json")
			if err := test.link(fixture.result.Evaluations[0].ReceiptPath, linked); err != nil {
				t.Fatal(err)
			}
			fixture.result.Evaluations[0].ReceiptPath = linked
			if _, err := PublishAggregate(t.Context(), fixture.options, fixture.result); err == nil {
				t.Fatalf("%s evaluation receipt was accepted", test.name)
			}
			if _, err := os.Lstat(fixture.options.Directory); !os.IsNotExist(err) {
				t.Fatalf("invalid receipt created aggregate: %v", err)
			}
		})
	}
}

func TestVerifyAggregateRejectsTransitiveTampering(t *testing.T) {
	for _, test := range []struct {
		name   string
		target func(aggregateFixture, AggregateBundle) string
	}{
		{name: "aggregate result", target: func(f aggregateFixture, _ AggregateBundle) string {
			return filepath.Join(f.options.Directory, aggregateResultName)
		}},
		{name: "reviewed media", target: func(_ aggregateFixture, b AggregateBundle) string {
			return b.Manifest.Evaluations[0].Media[0].Path
		}},
		{name: "evaluation receipt", target: func(_ aggregateFixture, b AggregateBundle) string {
			return b.Manifest.Evaluations[0].ReceiptPath
		}},
		{name: "source receipt", target: func(f aggregateFixture, _ AggregateBundle) string {
			return f.options.SourceReceiptPath
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newAggregateFixture(t, "case-a")
			bundle, err := PublishAggregate(t.Context(), fixture.options, fixture.result)
			if err != nil {
				t.Fatal(err)
			}
			path := test.target(fixture, bundle)
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := file.WriteAt([]byte{'X'}, 0)
			closeErr := file.Close()
			if writeErr != nil || closeErr != nil {
				t.Fatalf("tamper %s: %v / %v", path, writeErr, closeErr)
			}
			if _, err := VerifyAggregate(t.Context(), fixture.options); err == nil {
				t.Fatalf("transitive tampering of %s was accepted", test.name)
			}
		})
	}
}

func TestPublishAggregateConcurrentWritersNeverReplaceEvidence(t *testing.T) {
	fixture := newAggregateFixture(t, "case-a", "case-b")
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := PublishAggregate(t.Context(), fixture.options, fixture.result)
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	first, second := <-results, <-results
	if first != nil && second != nil {
		t.Fatalf("both concurrent publishers failed: %v / %v", first, second)
	}
	verified, err := VerifyAggregate(t.Context(), fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(verified.Manifest.Evaluations, canonicalAggregateEvaluations(verified.Manifest.Evaluations)) {
		t.Fatal("concurrent publication produced noncanonical evaluations")
	}
}

func BenchmarkPublishAggregateExact16(b *testing.B) {
	cases := make([]string, 16)
	for index := range cases {
		cases[index] = "case-" + benchmarkIndex(index)
	}
	b.ReportAllocs()
	for iteration := 0; iteration < b.N; iteration++ {
		b.StopTimer()
		fixture := newAggregateFixture(b, cases...)
		b.StartTimer()
		bundle, err := PublishAggregate(context.Background(), fixture.options, fixture.result)
		if err != nil {
			b.Fatal(err)
		}
		if bundle.Manifest.EvaluationCount != len(cases) {
			b.Fatalf("evaluations = %d, want %d", bundle.Manifest.EvaluationCount, len(cases))
		}
	}
}

func BenchmarkVerifyAggregateExact16(b *testing.B) {
	cases := make([]string, 16)
	for index := range cases {
		cases[index] = "case-" + benchmarkIndex(index)
	}
	fixture := newAggregateFixture(b, cases...)
	if _, err := PublishAggregate(context.Background(), fixture.options, fixture.result); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		bundle, err := VerifyAggregate(context.Background(), fixture.options)
		if err != nil {
			b.Fatal(err)
		}
		if bundle.Manifest.EvaluationCount != len(cases) {
			b.Fatalf("evaluations = %d, want %d", bundle.Manifest.EvaluationCount, len(cases))
		}
	}
}
