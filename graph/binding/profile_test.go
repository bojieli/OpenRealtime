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
	_, err := graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
		FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
		Name:          "openrealtime.graph.invalid-video", Revision: 1,
		GraphFingerprint: graph.Fingerprint, Ownership: nativeOwnership(),
		Capabilities: legacy.Capabilities{Video: true},
	})
	if err == nil || !strings.Contains(err.Error(), string(graphbinding.AdapterInputVideo)) {
		t.Fatalf("unmapped video capability error = %v", err)
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
