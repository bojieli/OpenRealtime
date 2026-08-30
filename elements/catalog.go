// Package elements registers the production standard-library element
// descriptors and in-process factories. Provider and application plugins add
// their own entries without modifying this package or the graph compiler.
package elements

import (
	"sort"

	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/flow"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	videoelements "github.com/bojieli/OpenRealtime/elements/video"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if err := acousticelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := flow.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := actionelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := cognitionelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := interactionelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := ingresselements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := mediaelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := modelelements.RegisterDescriptor(catalog); err != nil {
		return err
	}
	if err := perceptionelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := policyelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := speechelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := stateelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := videoelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	return nil
}

func RegisterFactories(registry *graphruntime.Registry) error {
	registrations, err := FactoryRegistrations()
	if err != nil {
		return err
	}
	for _, registration := range registrations {
		if err := registry.RegisterFactory(registration); err != nil {
			return err
		}
	}
	return nil
}

// FactoryRegistrations returns the deterministic exact in-process profiles
// for the complete standard element library. Provider plugins may clone and
// enrich a selected profile with deployment-resolved capabilities before
// using it for config discovery and graph assembly.
func FactoryRegistrations() ([]graphruntime.FactoryRegistration, error) {
	sources := []func() ([]graphruntime.FactoryRegistration, error){
		acousticelements.FactoryRegistrations,
		flow.FactoryRegistrations,
		actionelements.FactoryRegistrations,
		cognitionelements.FactoryRegistrations,
		interactionelements.FactoryRegistrations,
		ingresselements.FactoryRegistrations,
		mediaelements.FactoryRegistrations,
		modelelements.FactoryRegistrations,
		perceptionelements.FactoryRegistrations,
		policyelements.FactoryRegistrations,
		speechelements.FactoryRegistrations,
		stateelements.FactoryRegistrations,
		videoelements.FactoryRegistrations,
	}
	var result []graphruntime.FactoryRegistration
	for _, source := range sources {
		registrations, err := source()
		if err != nil {
			return nil, err
		}
		result = append(result, registrations...)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Profile.Reference < result[right].Profile.Reference
	})
	return result, nil
}

func Catalog() (*resolve.Catalog, error) {
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptors(catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

func RuntimeRegistry() (*graphruntime.Registry, error) {
	registry := graphruntime.NewRegistry()
	if err := RegisterFactories(registry); err != nil {
		return nil, err
	}
	return registry, nil
}

// AssemblyCatalog returns the complete built-in executable catalog used by a
// graph-native production launcher: every standard in-process element factory
// plus graph/runtime's mount-scoped clock, sequence, and secret-access
// coeffects. Provider plugins compose their own dependencies and secret
// providers onto this value before Discovery and graph-scoped Select.
func AssemblyCatalog() (graphassembly.Catalog, error) {
	registrations, err := FactoryRegistrations()
	if err != nil {
		return graphassembly.Catalog{}, err
	}
	return graphassembly.Catalog{
		Implementations: registrations,
		Dependencies:    graphassembly.StandardDependencies(),
	}, nil
}

// AssemblyInventory is the deterministic build-time surface of the standard
// library. External dependencies and genuinely unknown config schemas are
// explicit plugin gaps: AssemblyCatalog does not fabricate implementations
// for them.
type AssemblyInventory struct {
	Implementations              []string `json:"implementations"`
	RuntimeDependencies          []string `json:"runtime_dependencies"`
	ExternalRequiredDependencies []string `json:"external_required_dependencies"`
	ExternalOptionalDependencies []string `json:"external_optional_dependencies"`
	ConfigSchemas                []string `json:"config_schemas"`
	UnresolvedConfigSchemas      []string `json:"unresolved_config_schemas,omitempty"`
}

// Inventory derives the complete standard descriptor/plugin boundary. It is
// safe for startup diagnostics and contains no service handles, values,
// credentials, provider locators, or other private deployment state.
func Inventory() (AssemblyInventory, error) {
	descriptors, err := Catalog()
	if err != nil {
		return AssemblyInventory{}, err
	}
	registrations, err := FactoryRegistrations()
	if err != nil {
		return AssemblyInventory{}, err
	}
	configSchemas, err := StandardConfigSchemaCatalog()
	if err != nil {
		return AssemblyInventory{}, err
	}
	result := AssemblyInventory{
		Implementations: make([]string, len(registrations)),
		ConfigSchemas:   configSchemas.References(),
	}
	for index, registration := range registrations {
		result.Implementations[index] = registration.Profile.Reference
	}
	runtimeDependencies := make(map[string]struct{})
	for _, dependency := range graphassembly.StandardDependencies() {
		runtimeDependencies[dependency.Name] = struct{}{}
		result.RuntimeDependencies = append(result.RuntimeDependencies, dependency.Name)
	}
	requiredDependencies := make(map[string]struct{})
	optionalDependencies := make(map[string]struct{})
	schemas := make(map[string]struct{})
	for _, name := range descriptors.Names() {
		descriptor, found := descriptors.Latest(name)
		if !found {
			continue
		}
		if descriptor.ConfigSchema != "" {
			schemas[descriptor.ConfigSchema] = struct{}{}
		}
		for _, dependency := range descriptor.Dependencies {
			if _, builtIn := runtimeDependencies[dependency.Name]; builtIn {
				continue
			}
			if dependency.Optional {
				optionalDependencies[dependency.Name] = struct{}{}
			} else {
				requiredDependencies[dependency.Name] = struct{}{}
			}
		}
	}
	for name := range requiredDependencies {
		delete(optionalDependencies, name)
		result.ExternalRequiredDependencies = append(result.ExternalRequiredDependencies, name)
	}
	for name := range optionalDependencies {
		result.ExternalOptionalDependencies = append(result.ExternalOptionalDependencies, name)
	}
	providedSchemas := make(map[string]struct{}, len(result.ConfigSchemas))
	for _, reference := range result.ConfigSchemas {
		providedSchemas[reference] = struct{}{}
	}
	for reference := range schemas {
		if _, provided := providedSchemas[reference]; !provided {
			result.UnresolvedConfigSchemas = append(result.UnresolvedConfigSchemas, reference)
		}
	}
	sort.Strings(result.RuntimeDependencies)
	sort.Strings(result.ExternalRequiredDependencies)
	sort.Strings(result.ExternalOptionalDependencies)
	sort.Strings(result.ConfigSchemas)
	sort.Strings(result.UnresolvedConfigSchemas)
	return result, nil
}
