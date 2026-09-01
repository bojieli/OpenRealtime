package elements_test

import (
	"slices"
	"testing"

	"github.com/bojieli/OpenRealtime/elements"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
)

func TestStandardCatalogsConstruct(t *testing.T) {
	if _, err := elements.Catalog(); err != nil {
		t.Fatal(err)
	}
	if _, err := elements.RuntimeRegistry(); err != nil {
		t.Fatal(err)
	}
	assemblyCatalog, err := elements.AssemblyCatalog()
	if err != nil {
		t.Fatal(err)
	}
	discovery, err := assemblyCatalog.Discovery()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := discovery.Snapshot(); len(snapshot.Implementations) != 41 ||
		len(snapshot.Dependencies) != 3 {
		t.Fatalf("standard assembly discovery = %+v", snapshot)
	}
}

func TestStandardFactoryRegistrationsCoverTheDescriptorCatalogExactly(t *testing.T) {
	descriptors, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	registrations, err := elements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	names := descriptors.Names()
	if len(registrations) != 41 || len(registrations) != len(names) {
		t.Fatalf("standard inventory has %d registrations and %d descriptors", len(registrations), len(names))
	}
	for index, registration := range registrations {
		if registration.Factory == nil {
			t.Fatalf("registration %d has a nil factory", index)
		}
		descriptor := registration.Factory.Descriptor()
		identity, err := descriptor.Identity()
		if err != nil {
			t.Fatalf("registration %d descriptor: %v", index, err)
		}
		if registration.Profile.Reference != names[index] || registration.Profile.Reference != identity.Name {
			t.Fatalf("registration %d reference = %q, descriptor = %q, catalog = %q",
				index, registration.Profile.Reference, identity.Name, names[index])
		}
		if err := registration.Profile.Artifact.Validate(); err != nil {
			t.Fatalf("registration %s artifact: %v", identity.Name, err)
		}
		if !slices.Equal(registration.Profile.Transports, []string{"in-process"}) ||
			registration.Profile.Capabilities != nil {
			t.Fatalf("registration %s profile = %+v", identity.Name, registration.Profile)
		}
		if _, found := descriptors.Exact(identity); !found {
			t.Fatalf("registration %s descriptor is absent from the standard catalog", identity.Name)
		}
		if index > 0 && registrations[index-1].Profile.Reference >= registration.Profile.Reference {
			t.Fatalf("standard registrations are not unique and sorted at %q", identity.Name)
		}
	}

	discovery, err := (graphassembly.Catalog{Implementations: registrations}).Discovery()
	if err != nil {
		t.Fatalf("standard factory discovery: %v", err)
	}
	snapshot := discovery.Snapshot()
	if len(snapshot.Implementations) != len(registrations) {
		t.Fatalf("standard discovery contains %d implementations, want %d",
			len(snapshot.Implementations), len(registrations))
	}
	for index, implementation := range snapshot.Implementations {
		if implementation.Reference != names[index] ||
			implementation.Artifact != registrations[index].Profile.Artifact ||
			implementation.Contract.Name != names[index] {
			t.Fatalf("standard discovery implementation %d = %+v", index, implementation)
		}
	}
}

func TestStandardAssemblyInventoryNamesEveryExternalPluginGap(t *testing.T) {
	inventory, err := elements.Inventory()
	if err != nil {
		t.Fatal(err)
	}
	wantRequired := []string{
		"action.client-tool-result.rendezvous",
		"action.confirmation.providers",
		"action.irreversibility.ledger",
		"action.ledger.registries",
		"action.target.registries",
		"action.tool.registries",
		"action.trajectory.store",
		"cognition.continuation.providers",
		"model.external.deployments",
		"model.external.payload-codec",
		"perception.asr.providers",
		"perception.visual.providers",
		"policy.semantic.deciders",
		"speech.playback.sinks",
		"speech.tts.providers",
	}
	wantOptional := []string{
		"cognition.media.resolver",
		"interaction.post-commit-silence.scheduler",
		"perception.media.retainer",
		"speech.playback.scheduler",
		"state.trajectory.store",
	}
	wantRuntime := []string{"runtime.clock", "runtime.secrets", "runtime.sequence"}
	if !slices.Equal(inventory.ExternalRequiredDependencies, wantRequired) ||
		!slices.Equal(inventory.ExternalOptionalDependencies, wantOptional) ||
		!slices.Equal(inventory.RuntimeDependencies, wantRuntime) ||
		len(inventory.Implementations) != 41 || len(inventory.ConfigSchemas) != 31 ||
		len(inventory.UnresolvedConfigSchemas) != 0 {
		t.Fatalf("standard assembly inventory = %+v", inventory)
	}
}

func TestStandardFactoryRegistrationSnapshotsDoNotAliasCallers(t *testing.T) {
	first, err := elements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	first[0].Profile.Transports[0] = "mutated"
	if first[0].Profile.Capabilities != nil {
		first[0].Profile.Capabilities[0].Name = "mutated"
	}
	again, err := elements.FactoryRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(again[0].Profile.Transports, []string{"in-process"}) ||
		again[0].Profile.Capabilities != nil {
		t.Fatalf("standard factory registration aliases caller mutation: %+v", again[0].Profile)
	}
}
