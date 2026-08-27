package main

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/perception"
)

func TestArchitectureSelectionResolvesStructureInsteadOfAPresetSpecies(t *testing.T) {
	options := defaultOptions()
	options.architectureRef = "omni.external-policy@4"
	options.policyModel = "policy-3b"
	resolved, definition, err := applyArchitectureDefinition(options)
	if err != nil {
		t.Fatal(err)
	}
	if definition == nil || resolved.binding != "sidecar" ||
		resolved.sidecarInteraction != string(binding.OwnerEngine) ||
		resolved.sidecarFloor != string(binding.OwnerEngine) ||
		resolved.sidecarProtocol != 2 || resolved.policies != "interaction" {
		t.Fatalf("architecture was reduced to a preset label: definition=%+v options=%+v", definition, resolved)
	}
	stack, err := parseStackCapabilities(resolved.sidecarCapabilities)
	if err != nil {
		t.Fatal(err)
	}
	if !stack.NativeInteraction || !stack.InteractionActs || !stack.ConcurrentIO {
		t.Fatalf("unselected native capabilities disappeared: %+v", stack)
	}
}

func TestArchitectureSelectionRefusesStructuralFlagDrift(t *testing.T) {
	tests := []serveOptions{
		func() serveOptions {
			options := defaultOptions()
			options.architectureRef = "omni.native-full@3"
			options.binding = "cascade"
			options.explicit = map[string]bool{"binding": true}
			return options
		}(),
		func() serveOptions {
			options := defaultOptions()
			options.architectureRef = "omni.external-policy@4"
			options.sidecarCapabilities = "audio-input,audio-output,turn-generation"
			options.explicit = map[string]bool{"sidecar-capabilities": true}
			return options
		}(),
		func() serveOptions {
			options := defaultOptions()
			options.architectureRef = "omni.native-policy@3"
			options.policies = "interaction"
			options.explicit = map[string]bool{"policy-models": true}
			return options
		}(),
	}
	for _, options := range tests {
		if _, _, err := applyArchitectureDefinition(options); err == nil {
			t.Fatalf("structural drift was accepted: %+v", options)
		}
	}
}

func TestArchitectureCatalogCommandUsesTheProductionCatalog(t *testing.T) {
	var output strings.Builder
	if err := runArchitectures([]string{"validate"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "immutable revisions") ||
		!strings.Contains(output.String(), "sha256:") {
		t.Fatalf("validate output lacks identity: %s", output.String())
	}
	output.Reset()
	if err := runArchitectures([]string{"show", "omni.external-policy@4"}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "native-interaction") ||
		!strings.Contains(output.String(), "conversation-state") ||
		!strings.Contains(output.String(), "derived from") {
		t.Fatalf("show output lost the evolving architecture: %s", output.String())
	}
	if err := runArchitectures([]string{"show", "omni.external-policy"}, &output); err == nil {
		t.Fatal("an unpinned architecture was shown as though revisions were mutable")
	}
}

func TestArchitectureSelectionDerivesControllerArbitration(t *testing.T) {
	pure := defaultOptions()
	pure.architectureRef = "cascade.text-policy-visual-speaker@2"
	pure.policyModel = "policy-3b"
	pure.speakerURL = "http://127.0.0.1:8124/embed"
	resolved, definition, err := applyArchitectureDefinition(pure)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Interaction.Control == nil || !resolved.interactionFloor {
		t.Fatalf("single text controller did not receive floor/overlap selection: %+v %+v", definition, resolved)
	}
	purePolicies, err := buildPolicies(resolved, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(purePolicies.Floor.Name(), "act:") ||
		!strings.HasPrefix(purePolicies.BargeIn.Name(), "act:") {
		t.Fatalf("single text controller did not own both act jurisdictions: %+v", purePolicies.Report())
	}

	composed := pure
	composed.architectureRef = "cascade.composed-policy-visual-speaker@1"
	resolved, definition, err = applyArchitectureDefinition(composed)
	if err != nil {
		t.Fatal(err)
	}
	if definition.Interaction.Control == nil || resolved.interactionFloor ||
		definition.Interaction.Control.Arbitration != "predicate-floor" {
		t.Fatalf("composed controllers lost predicate-floor arbitration: %+v %+v", definition, resolved)
	}
	composedPolicies, err := buildPolicies(resolved, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(composedPolicies.Floor.Name(), "act:") ||
		strings.HasPrefix(composedPolicies.BargeIn.Name(), "act:") ||
		composedPolicies.Interaction == nil {
		t.Fatalf("predicate-floor arbitration did not retain both jurisdictions: %+v", composedPolicies.Report())
	}

	composed.interactionFloor = true
	composed.explicit = map[string]bool{"interaction-floor": true}
	if _, _, err := applyArchitectureDefinition(composed); err == nil ||
		!strings.Contains(err.Error(), "interaction-floor=false") {
		t.Fatalf("an operator overrode catalog arbitration: %v", err)
	}
}

func TestArchitectureSelectionDerivesEvidenceComposition(t *testing.T) {
	baseline := defaultOptions()
	baseline.architectureRef = "cascade.text-policy@2"
	baseline.policyModel = "policy-3b"
	baseline.interactionSees = true
	baseline.explicit = map[string]bool{"interaction-sees": true}
	if _, _, err := applyArchitectureDefinition(baseline); err == nil ||
		!strings.Contains(err.Error(), "direct_visual_input=false") {
		t.Fatalf("undeclared direct vision was accepted: %v", err)
	}

	direct := defaultOptions()
	direct.architectureRef = "cascade.text-policy-direct-visual@1"
	direct.policyModel = "policy-3b"
	resolved, _, err := applyArchitectureDefinition(direct)
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.interactionSees || resolved.observers != "audio+video" ||
		resolved.components != string(perception.ComponentKeyframeNarration) {
		t.Fatalf("the catalog's direct-visual capability was not composed into the runtime: %+v", resolved)
	}

	speaker := defaultOptions()
	speaker.architectureRef = "cascade.text-policy-speaker@1"
	speaker.policyModel = "policy-3b"
	if _, _, err := applyArchitectureDefinition(speaker); err == nil ||
		!strings.Contains(err.Error(), "speaker-url") {
		t.Fatalf("speaker evidence without a producer was accepted: %v", err)
	}
	speaker.speakerURL = "http://127.0.0.1:8124/embed"
	if _, _, err := applyArchitectureDefinition(speaker); err != nil {
		t.Fatalf("speaker evidence could not be composed from the catalog: %v", err)
	}

	addressing := defaultOptions()
	addressing.architectureRef = "cascade.text-policy-addressing@1"
	addressing.policyModel = "policy-3b"
	addressing.speakerURL = "http://127.0.0.1:8124/embed"
	if _, _, err := applyArchitectureDefinition(addressing); err == nil ||
		!strings.Contains(err.Error(), "no configured runtime component") {
		t.Fatalf("desired but unavailable addressing was fabricated: %v", err)
	}
}
