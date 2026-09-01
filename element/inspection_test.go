package element_test

import (
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
)

func TestInspectionDecisionVocabularyIsClosed(t *testing.T) {
	for _, kind := range element.SupportedInspectionDecisionKinds() {
		for _, operation := range element.SupportedInspectionDecisionOperations() {
			if err := (element.InspectionDecision{
				Kind: kind, Operation: operation, Crossed: true,
			}).Validate(); err != nil {
				t.Fatalf("supported inspection decision %q/%q: %v", kind, operation, err)
			}
		}
	}
	kinds := element.SupportedInspectionDecisionKinds()
	operations := element.SupportedInspectionDecisionOperations()
	kinds[0], operations[0] = "mutated", "mutated"
	if element.SupportedInspectionDecisionKinds()[0] == "mutated" ||
		element.SupportedInspectionDecisionOperations()[0] == "mutated" {
		t.Fatal("supported inspection decision vocabulary retained caller aliases")
	}
	for _, candidate := range []element.InspectionDecision{
		{Kind: "private-request-text", Operation: element.DecisionSelect},
		{Kind: element.DecisionSucceeded, Operation: "private-request-text"},
	} {
		if err := candidate.Validate(); err == nil {
			t.Fatalf("invalid inspection decision passed: %+v", candidate)
		}
	}
}

func TestInspectionCauseVocabularyIsClosed(t *testing.T) {
	want := []element.InspectionCauseKind{
		element.CauseObservation,
		element.CauseStateRevision,
		element.CausePolicy,
		element.CauseModelRun,
	}
	got := element.SupportedInspectionCauseKinds()
	if !slices.Equal(got, want) {
		t.Fatalf("inspection cause vocabulary = %v, want %v", got, want)
	}
	for _, kind := range got {
		if err := kind.Validate(); err != nil {
			t.Fatalf("supported inspection cause %q: %v", kind, err)
		}
	}
	got[0] = "mutated"
	if element.SupportedInspectionCauseKinds()[0] == "mutated" {
		t.Fatal("inspection cause vocabulary aliases caller memory")
	}
	for _, kind := range []element.InspectionCauseKind{"", "provider-name", "observation\nsecret"} {
		if err := kind.Validate(); err == nil {
			t.Fatalf("inspection cause %q unexpectedly validated", kind)
		}
	}
}
