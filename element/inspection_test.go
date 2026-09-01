package element_test

import (
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
