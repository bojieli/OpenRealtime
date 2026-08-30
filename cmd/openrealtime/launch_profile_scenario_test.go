package main

import (
	"bytes"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

func TestProductionScenarioToolsUseTheClientOwnedDeclaration(t *testing.T) {
	got, err := productionScenarioToolDeclarations()
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]scenario.FunctionToolDeclaration)
	for _, item := range scenario.Suite() {
		for _, tool := range item.Tools {
			declaration, err := tool.FunctionDeclaration()
			if err != nil {
				t.Fatal(err)
			}
			want[tool.Name] = declaration
		}
	}
	if len(got) != len(want) {
		t.Fatalf("production tool declarations = %d, want %d", len(got), len(want))
	}
	for _, declaration := range got {
		canonical, found := want[declaration.Name]
		if !found {
			t.Fatalf("production profile added undeclared tool %q", declaration.Name)
		}
		if declaration.Description != canonical.Description ||
			!bytes.Equal(declaration.Parameters, canonical.Parameters) ||
			declaration.Confirm != legacyaction.ConfirmNever {
			t.Fatalf("production declaration for %q drifted: %+v", declaration.Name, declaration)
		}
	}
}

func TestProductionScenarioInspectionCapabilityCoversOneAttempt(t *testing.T) {
	got := time.Duration(defaultScenarioProfileOptions().inspectionTokenTTL) * time.Millisecond
	if got != 5*time.Minute {
		t.Fatalf("scenario inspection capability lifetime = %s, want 5m", got)
	}
	if got <= 2*time.Minute+5*time.Second {
		t.Fatalf("scenario inspection capability lifetime %s does not cover the client deadline", got)
	}
}
