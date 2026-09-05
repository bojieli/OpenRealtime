package campaign

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestAggregateReviewPreservesNotApplicableRecording(t *testing.T) {
	fixture := newCampaignOutcomeFixture(t,
		bench.TaskOutcome{ID: "pass", Completed: true, Passed: true, Applicability: bench.Applicable},
		bench.TaskOutcome{ID: "fail", Completed: true, Applicability: bench.Applicable},
		bench.TaskOutcome{ID: "not-applicable", Completed: true, Applicability: bench.NotApplicable},
	)
	provider := &campaignProvider{assessment: []byte(`{
		"media_usable":true,"observed_outcome":"unclear","agrees_with_deterministic":false,
		"confidence":0.5,"summary":"The recording remains available for review.",
		"significant_problems":[],"minor_observations":[],"limitations":["Synthetic fixture."]
	}`)}
	fixture.options.Reviewer = fixtureLease(t, provider)
	result, err := Run(t.Context(), fixture.options)
	if err != nil {
		t.Fatal(err)
	}
	options := AggregateOptions{
		Directory: filepath.Join(fixture.root, "aggregate"), ReceiptPath: filepath.Join(fixture.root, "aggregate.receipt.json"),
		SourceReceiptPath: fixture.sourceReceipt, QuarantineDirectory: filepath.Join(fixture.root, "quarantine"),
	}
	if _, err := PublishAggregate(t.Context(), options, result); err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyAggregate(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Receipt.EvaluationCount != 3 || provider.calls.Load() != 3 {
		t.Fatalf("inapplicable recording lost its review: %+v", verified.Receipt)
	}
	labels := make(map[string]string)
	for _, evaluation := range verified.Manifest.Evaluations {
		labels[evaluation.Case] = evaluation.Deterministic
	}
	if labels["not-applicable"] != "not_applicable" || labels["pass"] != "pass" || labels["fail"] != "fail" {
		t.Fatalf("wrong deterministic labels: %+v", labels)
	}
	payload, err := os.ReadFile(filepath.Join(options.Directory, aggregateReviewName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Deterministic: 1 pass, 1 fail, 0 infrastructure", "Not applicable: 1", "not_applicable"} {
		if !strings.Contains(string(payload), want) {
			t.Fatalf("review missing %q:\n%s", want, payload)
		}
	}
}
