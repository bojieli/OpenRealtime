package runtime

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
)

func TestResolutionReporterIsCanonicalImmutableAndCannotDrift(t *testing.T) {
	mounted := &Mounted{nodeLive: map[string]inspect.NodeLive{
		"model": {
			State: "mounted",
			Resolution: &inspect.NodeResolution{
				Element: element.Identity{
					Name: "test.Model", Revision: 1,
					Digest: "sha256:" + strings.Repeat("1", 64),
				},
				Implementation: "test.Model",
				Runtime: inspect.ArtifactIdentity{
					ID: "test.Model", Digest: "sha256:" + strings.Repeat("1", 64),
				},
				RuntimeEvidence: inspect.EvidenceDeclared,
			},
		},
	}}
	reporter := nodeResolutionReporter{mounted: mounted, node: "model"}
	if err := reporter.Runtime("worker://model", "image:1", ""); err != nil {
		t.Fatal(err)
	}
	capabilities := []element.CapabilityResolution{
		{Name: "vision", ProviderID: "model://multi", ProviderRevision: "weights:1"},
		{Name: "generation", ProviderID: "model://multi", ProviderRevision: "weights:1"},
	}
	if err := reporter.Capabilities(capabilities); err != nil {
		t.Fatal(err)
	}
	capabilities[0].ProviderID = "mutated"
	resolution := mounted.nodeLive["model"].Resolution
	if resolution.RuntimeEvidence != inspect.EvidenceLive ||
		resolution.CapabilitiesEvidence != inspect.EvidenceLive ||
		resolution.Capabilities[0].Name != "generation" ||
		resolution.Capabilities[1].Provider.ID != "model://multi" {
		t.Fatalf("reported resolution = %+v", resolution)
	}
	if err := reporter.Runtime("worker://other", "image:2", ""); err == nil ||
		!strings.Contains(err.Error(), "changed") {
		t.Fatalf("live runtime drift was accepted: %v", err)
	}
	if err := reporter.Capabilities([]element.CapabilityResolution{{
		Name: "generation", ProviderID: "model://other", ProviderRevision: "weights:2",
	}}); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("live capability drift was accepted: %v", err)
	}
	if err := reporter.Capabilities([]element.CapabilityResolution{{
		Name: "generation", ProviderID: "model://latest", ProviderRevision: "weights:2",
	}}); err == nil {
		t.Fatal("incomplete provider identity was accepted")
	}
}
