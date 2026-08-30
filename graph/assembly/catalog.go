package assembly

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"

	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

// Catalog is a broad, explicit set of executable plugin contributions. It is
// suitable for process startup and config discovery, but is not itself mount
// authority. Select reduces it to the exact graph-scoped Assembly frozen by a
// Plan; Preflight rejects any manually constructed Assembly that is broader.
type Catalog struct {
	Implementations []graphruntime.FactoryRegistration
	Dependencies    []Dependency
	SecretProviders []SecretProvider
}

type catalogIndex struct {
	discovery              *graphconfig.StaticDiscovery
	implementations        map[string]graphruntime.FactoryRegistration
	implementationProfiles map[string]graphconfig.ImplementationResolution
	dependencies           map[string]Dependency
	dependencyProfiles     map[string]graphconfig.DependencyResolution
	secretProviders        map[string]SecretProvider
}

// Discovery validates the whole catalog and returns a metadata-only snapshot
// for config.Create. It calls Factory.Descriptor but never Factory.Mount,
// SecretProvider.Resolve, or a dependency service.
func (catalog Catalog) Discovery() (*graphconfig.StaticDiscovery, error) {
	index, err := indexCatalog(catalog)
	if err != nil {
		return nil, fmt.Errorf("build graph assembly discovery: %w", err)
	}
	return index.discovery, nil
}

// Select validates the whole catalog and reduces it to the exact executable
// contributions named by plan. The supplied secret catalog is cloned and must
// match the private fingerprint frozen by config.Create. Unselected catalog
// entries are not copied into the returned Assembly and therefore grant no
// ambient mount authority.
func (catalog Catalog) Select(
	plan *graphconfig.Plan,
	secretCatalog *graphsecret.Document,
) (Assembly, error) {
	if plan == nil {
		return Assembly{}, errors.New("select graph assembly: nil plan")
	}
	if err := plan.Validate(); err != nil {
		return Assembly{}, fmt.Errorf("select graph assembly: invalid plan: %w", err)
	}
	index, err := indexCatalog(catalog)
	if err != nil {
		return Assembly{}, fmt.Errorf("select graph assembly: %w", err)
	}

	resolution := plan.Resolution()
	requiredImplementations, err := exactImplementationProfiles(resolution)
	if err != nil {
		return Assembly{}, fmt.Errorf("select graph assembly: %w", err)
	}
	requiredDependencies, err := exactDependencyProfiles(resolution)
	if err != nil {
		return Assembly{}, fmt.Errorf("select graph assembly: %w", err)
	}
	selectedSecretCatalog, requiredProviders, err := secretInventory(plan, secretCatalog)
	if err != nil {
		return Assembly{}, fmt.Errorf("select graph assembly: %w", err)
	}

	result := Assembly{SecretCatalog: selectedSecretCatalog}
	for _, reference := range sortedMapKeys(requiredImplementations) {
		registration, found := index.implementations[reference]
		if !found {
			return Assembly{}, fmt.Errorf("select graph assembly: implementation catalog is missing %q", reference)
		}
		if !reflect.DeepEqual(index.implementationProfiles[reference], requiredImplementations[reference]) {
			return Assembly{}, fmt.Errorf(
				"select graph assembly: implementation %q drifted from the frozen plan", reference,
			)
		}
		result.Implementations = append(result.Implementations, cloneFactoryRegistration(registration))
	}
	for _, name := range sortedMapKeys(requiredDependencies) {
		dependency, found := index.dependencies[name]
		if !found {
			return Assembly{}, fmt.Errorf("select graph assembly: dependency catalog is missing %q", name)
		}
		if index.dependencyProfiles[name] != requiredDependencies[name] {
			return Assembly{}, fmt.Errorf(
				"select graph assembly: dependency %q drifted from the frozen plan", name,
			)
		}
		result.Dependencies = append(result.Dependencies, dependency)
	}
	for _, name := range sortedSetKeys(requiredProviders) {
		provider, found := index.secretProviders[name]
		if !found {
			return Assembly{}, fmt.Errorf("select graph assembly: secret provider catalog is missing %q", name)
		}
		result.SecretProviders = append(result.SecretProviders, provider)
	}
	return result, nil
}

// StandardDependencies returns independent, deterministic catalog entries for
// graph/runtime's built-in mount-scoped coeffects. Catalog.Select will retain
// only the names frozen into a particular plan.
func StandardDependencies() []Dependency {
	artifacts := graphruntime.StandardDependencyArtifacts()
	names := make([]string, 0, len(artifacts))
	for name := range artifacts {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]Dependency, 0, len(names))
	for _, name := range names {
		result = append(result, Dependency{
			Name: name, Artifact: artifacts[name], Scope: graphconfig.DependencyScopeMount,
			RuntimeOwned: true,
		})
	}
	return result
}

// StandardDependency returns one built-in runtime coeffect contribution.
func StandardDependency(name string) (Dependency, bool) {
	artifact, found := graphruntime.StandardDependencyArtifact(name)
	if !found {
		return Dependency{}, false
	}
	return Dependency{
		Name: name, Artifact: artifact, Scope: graphconfig.DependencyScopeMount,
		RuntimeOwned: true,
	}, true
}

func indexCatalog(catalog Catalog) (catalogIndex, error) {
	if len(catalog.Implementations) > maximumAssemblyEntries {
		return catalogIndex{}, fmt.Errorf("implementation catalog has %d entries; maximum is %d",
			len(catalog.Implementations), maximumAssemblyEntries)
	}
	if len(catalog.Dependencies) > maximumAssemblyEntries {
		return catalogIndex{}, fmt.Errorf("dependency catalog has %d entries; maximum is %d",
			len(catalog.Dependencies), maximumAssemblyEntries)
	}
	if len(catalog.SecretProviders) > maximumAssemblyEntries {
		return catalogIndex{}, fmt.Errorf("secret provider catalog has %d entries; maximum is %d",
			len(catalog.SecretProviders), maximumAssemblyEntries)
	}

	result := catalogIndex{
		discovery:              graphconfig.NewStaticDiscovery(),
		implementations:        make(map[string]graphruntime.FactoryRegistration, len(catalog.Implementations)),
		implementationProfiles: make(map[string]graphconfig.ImplementationResolution, len(catalog.Implementations)),
		dependencies:           make(map[string]Dependency, len(catalog.Dependencies)),
		dependencyProfiles:     make(map[string]graphconfig.DependencyResolution, len(catalog.Dependencies)),
		secretProviders:        make(map[string]SecretProvider, len(catalog.SecretProviders)),
	}
	factories := graphruntime.NewRegistry()
	for _, source := range catalog.Implementations {
		registration := cloneFactoryRegistration(source)
		if err := factories.RegisterFactory(registration); err != nil {
			return catalogIndex{}, fmt.Errorf("implementation %q: %w", registration.Profile.Reference, err)
		}
		descriptor := registration.Factory.Descriptor()
		contract, err := descriptor.Identity()
		if err != nil {
			return catalogIndex{}, fmt.Errorf("implementation %q descriptor: %w",
				registration.Profile.Reference, err)
		}
		profile := graphconfig.ImplementationResolution{
			Reference:    registration.Profile.Reference,
			Contract:     contract,
			Artifact:     registration.Profile.Artifact,
			Evidence:     inspect.EvidenceRegistered,
			Placements:   slices.Clone(registration.Profile.Placements),
			Transports:   slices.Clone(registration.Profile.Transports),
			ResourceKeys: slices.Clone(registration.Profile.ResourceKeys),
			SecretSlots:  slices.Clone(registration.Profile.SecretSlots),
			Capabilities: cloneCapabilities(registration.Profile.Capabilities),
		}
		if err := result.discovery.RegisterImplementation(profile); err != nil {
			return catalogIndex{}, fmt.Errorf("implementation %q discovery: %w", profile.Reference, err)
		}
		result.implementations[profile.Reference] = registration
	}

	dependencies := graphruntime.NewDependencyRegistry()
	for _, source := range catalog.Dependencies {
		dependency, err := canonicalAssemblyDependency(source)
		if err != nil {
			return catalogIndex{}, fmt.Errorf("dependency %q: %w", source.Name, err)
		}
		switch {
		case dependency.RuntimeOwned:
			err = dependencies.RegisterRuntimeOwned(dependency.Name, dependency.Artifact)
		case dependency.Scope == graphconfig.DependencyScopeMount:
			err = dependencies.RegisterMountScoped(dependency.Name, dependency.Artifact)
		default:
			err = dependencies.Register(dependency.Name, dependency.Artifact, dependency.Service)
		}
		if err != nil {
			return catalogIndex{}, fmt.Errorf("dependency %q: %w", dependency.Name, err)
		}
		profile := graphconfig.DependencyResolution{
			Name: dependency.Name, Artifact: dependency.Artifact, Scope: dependency.Scope,
		}
		if err := result.discovery.RegisterDependency(profile); err != nil {
			return catalogIndex{}, fmt.Errorf("dependency %q discovery: %w", dependency.Name, err)
		}
		result.dependencies[dependency.Name] = dependency
	}

	providers := graphsecret.NewRegistry()
	for _, provider := range catalog.SecretProviders {
		if err := providers.Register(provider.Name, provider.Artifact, provider.Provider); err != nil {
			return catalogIndex{}, fmt.Errorf("secret provider %q: %w", provider.Name, err)
		}
		result.secretProviders[provider.Name] = provider
	}

	for _, profile := range result.discovery.Snapshot().Implementations {
		result.implementationProfiles[profile.Reference] = profile
	}
	for _, profile := range result.discovery.Snapshot().Dependencies {
		result.dependencyProfiles[profile.Name] = profile
	}
	return result, nil
}

func exactImplementationProfiles(
	resolution graphconfig.Resolution,
) (map[string]graphconfig.ImplementationResolution, error) {
	result := make(map[string]graphconfig.ImplementationResolution, len(resolution.Nodes))
	for _, node := range resolution.Nodes {
		profile := node.Implementation
		if previous, found := result[profile.Reference]; found {
			if !reflect.DeepEqual(previous, profile) {
				return nil, fmt.Errorf("frozen plan resolves implementation %q ambiguously", profile.Reference)
			}
			continue
		}
		result[profile.Reference] = profile
	}
	return result, nil
}

func exactDependencyProfiles(
	resolution graphconfig.Resolution,
) (map[string]graphconfig.DependencyResolution, error) {
	result := make(map[string]graphconfig.DependencyResolution, len(resolution.Dependencies))
	for _, dependency := range resolution.Dependencies {
		if _, duplicate := result[dependency.Name]; duplicate {
			return nil, fmt.Errorf("frozen plan repeats dependency %q", dependency.Name)
		}
		result[dependency.Name] = dependency
	}
	return result, nil
}

func cloneFactoryRegistration(source graphruntime.FactoryRegistration) graphruntime.FactoryRegistration {
	result := source
	result.Profile.Placements = slices.Clone(source.Profile.Placements)
	result.Profile.Transports = slices.Clone(source.Profile.Transports)
	result.Profile.ResourceKeys = slices.Clone(source.Profile.ResourceKeys)
	result.Profile.SecretSlots = slices.Clone(source.Profile.SecretSlots)
	result.Profile.Capabilities = cloneCapabilities(source.Profile.Capabilities)
	return result
}

func cloneCapabilities(source []inspect.CapabilityIdentity) []inspect.CapabilityIdentity {
	if source == nil {
		return nil
	}
	result := slices.Clone(source)
	for index := range result {
		if source[index].Adapter != nil {
			adapter := *source[index].Adapter
			result[index].Adapter = &adapter
		}
	}
	return result
}

func sortedMapKeys[V any](source map[string]V) []string {
	result := make([]string, 0, len(source))
	for key := range source {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

func sortedSetKeys(source map[string]struct{}) []string {
	return sortedMapKeys(source)
}
