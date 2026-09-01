package binding_test

import (
	"strings"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestSessionAdapterProfileIsCanonicalFingerprintBoundToExactGraphBoundaries(t *testing.T) {
	plan, _ := nativeVideoPlan(t)
	profile := nativeVideoProfile(t, plan)
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := profile.ValidateGraph(plan.Graph()); err != nil {
		t.Fatal(err)
	}
	if profile.Contract != graphbinding.SessionAdapterContract() ||
		profile.BoundaryMapDigest == "" || profile.ProjectionDigest == "" || profile.Fingerprint == "" {
		t.Fatalf("frozen adapter profile = %+v", profile)
	}

	clone := profile.Clone()
	clone.Capabilities.Observers = append(clone.Capabilities.Observers, "screen")
	if err := clone.Validate(); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("mutated projection validation = %v", err)
	}
	clone = profile.Clone()
	clone.Boundaries[0].Boundary = "references"
	if err := clone.Validate(); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("mutated boundary validation = %v", err)
	}
}

func TestSessionAdapterProfileRefusesCapabilitiesWithoutTheirBoundaryOperations(t *testing.T) {
	plan, _ := nativeVideoPlan(t)
	graph := plan.Graph()
	tests := []struct {
		name         string
		capabilities legacy.Capabilities
		want         string
	}{
		{name: "video", capabilities: legacy.Capabilities{Video: true}, want: string(graphbinding.AdapterInputVideo)},
		{name: "computer use", capabilities: legacy.Capabilities{ComputerUse: true}, want: string(graphbinding.AdapterOutputToolCalls)},
		{name: "observations", capabilities: legacy.Capabilities{Observations: true}, want: string(graphbinding.AdapterOutputObservation)},
		{name: "manual turns", capabilities: legacy.Capabilities{ManualTurns: true}, want: string(graphbinding.AdapterInputCommitAudio)},
		{name: "selectable voice", capabilities: legacy.Capabilities{Voice: legacy.VoiceControl{Selectable: true}}, want: string(graphbinding.AdapterInputUpdate)},
		{name: "audio input", capabilities: legacy.Capabilities{Stack: legacy.StackCapabilities{AudioInput: true}}, want: string(graphbinding.AdapterInputAudio)},
		{name: "audio output", capabilities: legacy.Capabilities{Stack: legacy.StackCapabilities{AudioOutput: true}}, want: string(graphbinding.AdapterOutputSpeechAudio)},
		{name: "transcription", capabilities: legacy.Capabilities{Stack: legacy.StackCapabilities{Transcription: true}}, want: string(graphbinding.AdapterOutputTranscript)},
		{name: "text injection", capabilities: legacy.Capabilities{Stack: legacy.StackCapabilities{TextInjection: true}}, want: string(graphbinding.AdapterInputText)},
		{name: "visual input", capabilities: legacy.Capabilities{Stack: legacy.StackCapabilities{VisualInput: true}}, want: string(graphbinding.AdapterInputVideo)},
		{name: "named observers", capabilities: legacy.Capabilities{Observers: []string{"screen"}}, want: "without the observations capability"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
				FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
				Name:          "openrealtime.graph.invalid-capability", Revision: 1,
				GraphFingerprint: graph.Fingerprint, Ownership: nativeOwnership(),
				Capabilities: testCase.capabilities,
			})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("unmapped %s capability error = %v", testCase.name, err)
			}
		})
	}
}

func TestSessionAdapterProfileRequiresBothManualTurnOperations(t *testing.T) {
	plan, _ := nativeVideoPlan(t)
	graph := plan.Graph()
	inputType, err := nativeBoundaryType(graph, "frame", ir.InputBoundary)
	if err != nil {
		t.Fatal(err)
	}
	_, err = graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
		FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
		Name:          "openrealtime.graph.incomplete-manual-turns", Revision: 1,
		GraphFingerprint: graph.Fingerprint, Ownership: nativeOwnership(),
		Capabilities: legacy.Capabilities{ManualTurns: true},
		Boundaries: []graphbinding.AdapterBoundary{{
			Operation: graphbinding.AdapterInputCommitAudio, Boundary: "frame",
			Direction: ir.InputBoundary, Type: inputType,
		}},
	})
	if err == nil || !strings.Contains(err.Error(), string(graphbinding.AdapterInputCreateResponse)) {
		t.Fatalf("manual turns without response creation error = %v", err)
	}
}

func TestSessionAdapterProfileRefusesBoundaryDriftEvenWhenProfileIsSelfConsistent(t *testing.T) {
	plan, _ := nativeVideoPlan(t)
	graph := plan.Graph()
	frameType, err := nativeBoundaryType(graph, "frame", ir.InputBoundary)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
		FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
		Name:          "openrealtime.graph.absent-boundary", Revision: 1,
		GraphFingerprint: graph.Fingerprint, Ownership: nativeOwnership(),
		Boundaries: []graphbinding.AdapterBoundary{{
			Operation: graphbinding.AdapterInputVideo, Boundary: "absent",
			Direction: ir.InputBoundary, Type: frameType,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := profile.ValidateGraph(graph); err == nil || !strings.Contains(err.Error(), "absent graph boundary") {
		t.Fatalf("absent graph boundary validation = %v", err)
	}
}
