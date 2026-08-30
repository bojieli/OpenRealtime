package adaptivevideo_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	legacy "github.com/bojieli/OpenRealtime/binding"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	adaptivevideo "github.com/bojieli/OpenRealtime/graph/binding/adaptivevideo"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
)

func TestProviderDependencyIsResourceFreeAndCreatesIndependentExactMountOverlays(t *testing.T) {
	artifact := inspect.ArtifactIdentity{
		ID:       "go://openrealtime/test/adaptive-video-provider-registry",
		Revision: "build-1", Digest: "sha256:" + strings.Repeat("a", 64),
	}
	descriptor := perceptionelements.VisualProviderDescriptor{
		Name: "deterministic-visual", Revision: "model-1",
		Digest: "sha256:" + strings.Repeat("b", 64),
	}
	var acquisitions atomic.Int64
	registrations := []adaptivevideo.ProviderRegistration{{
		Reference: "visual.youtube.narrator.v1", Descriptor: descriptor,
		Factory: func() (perceptionelements.VisualProvider, error) {
			acquisitions.Add(1)
			return nil, nil
		},
	}}
	dependency, err := adaptivevideo.NewProviderDependency(artifact, registrations)
	if err != nil {
		t.Fatal(err)
	}
	registrations[0] = adaptivevideo.ProviderRegistration{}
	if acquisitions.Load() != 0 {
		t.Fatal("provider dependency construction acquired a provider")
	}
	entry := dependency.AssemblyDependency()
	if entry.Name != perceptionelements.VisualProviderRegistryService ||
		entry.Artifact != artifact || entry.Scope != graphconfig.DependencyScopeMount ||
		entry.Service != nil || entry.RuntimeOwned {
		t.Fatalf("adaptive video assembly dependency = %+v", entry)
	}

	factory := dependency.MountDependencyFactory()
	first, err := factory(context.Background(), legacy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := factory(context.Background(), legacy.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if acquisitions.Load() != 0 {
		t.Fatal("mount dependency overlay acquired a provider")
	}
	if len(first) != 1 || len(second) != 1 || first[0].Name != entry.Name ||
		first[0].Artifact != artifact || second[0].Artifact != artifact {
		t.Fatalf("mount dependency overlays = %+v %+v", first, second)
	}
	firstRegistry, firstOK := first[0].Service.(*perceptionelements.VisualProviderRegistry)
	secondRegistry, secondOK := second[0].Service.(*perceptionelements.VisualProviderRegistry)
	if !firstOK || !secondOK || firstRegistry == nil || secondRegistry == nil ||
		firstRegistry == secondRegistry {
		t.Fatalf("mount registries are not independent: %T %p, %T %p",
			first[0].Service, firstRegistry, second[0].Service, secondRegistry)
	}
}
