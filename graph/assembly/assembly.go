// Package assembly is the graph-native production boundary between an
// immutable config plan and the executable plugins selected for that plan.
//
// The package deliberately does not discover implementations, parse command
// line flags, or fall back to a legacy binding. Callers supply one exact,
// graph-scoped Assembly assembled from their plugin catalogs. Preflight
// rejects missing, excess, or ambiguous factories, services, and secret
// providers before graph/runtime is allowed to mount the first element or
// resolve the first secret.
package assembly

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/deployment"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

const (
	maximumAssemblyEntries = 65_536
	maximumAssemblyText    = 64 << 10
)

// Dependency binds one exact service implementation selected during plan
// creation. Process-scoped dependencies carry their service through
// resource-free preflight. Mount-scoped dependencies attest only immutable
// metadata here and receive their service through PreparedMountOptions.
// RuntimeOwned is retained for the built-in graph/runtime coeffects, whose
// mount-scoped values are constructed internally rather than supplied by a
// caller overlay.
type Dependency struct {
	Name         string
	Artifact     inspect.ArtifactIdentity
	Scope        graphconfig.DependencyScope
	Service      any
	RuntimeOwned bool
}

// SecretProvider binds one provider implementation to the provider name used
// by the graph-scoped secret catalog. Provider.Resolve is never called by
// Preflight.
type SecretProvider struct {
	Name     string
	Artifact inspect.ArtifactIdentity
	Provider graphsecret.Provider
}

// Assembly is the complete executable inventory for exactly one config.Plan.
// It is intentionally a slice-based contribution surface: independent plugin
// catalogs can compose entries without a central implementation switch, while
// Preflight retains enough information to detect duplicate contributions.
//
// SecretCatalog contains provider names and non-secret locators, but never
// credential values. It must be the exact graph-scoped document whose private
// fingerprint was frozen into Plan.
type Assembly struct {
	Implementations []graphruntime.FactoryRegistration
	Dependencies    []Dependency
	SecretCatalog   *graphsecret.Document
	SecretProviders []SecretProvider
}

// Preflight converts an explicit plugin assembly into the registries consumed
// by graph/runtime.PreparePlan and returns its sealed handoff. It invokes only
// resource-free metadata operations (including Factory.Descriptor); factory
// Mount and secret Provider.Resolve remain behind PreparedPlan.Mount.
func Preflight(
	ctx context.Context,
	plan *graphconfig.Plan,
	assembly Assembly,
) (*graphruntime.PreparedPlan, error) {
	if ctx == nil {
		return nil, errors.New("preflight graph assembly: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if plan == nil {
		return nil, errors.New("preflight graph assembly: nil plan")
	}

	resolution := plan.Resolution()
	requiredImplementations, err := implementationInventory(resolution)
	if err != nil {
		return nil, fmt.Errorf("preflight graph assembly: %w", err)
	}
	requiredDependencies, err := dependencyInventory(resolution)
	if err != nil {
		return nil, fmt.Errorf("preflight graph assembly: %w", err)
	}
	secretCatalog, requiredProviders, err := secretInventory(plan, assembly.SecretCatalog)
	if err != nil {
		return nil, fmt.Errorf("preflight graph assembly: %w", err)
	}

	implementationNames := make([]string, len(assembly.Implementations))
	for index, registration := range assembly.Implementations {
		implementationNames[index] = registration.Profile.Reference
	}
	if err := requireExactInventory(
		"implementation", requiredImplementations, implementationNames,
	); err != nil {
		return nil, fmt.Errorf("preflight graph assembly: %w", err)
	}

	canonicalDependencies := make([]Dependency, len(assembly.Dependencies))
	dependencyNames := make([]string, len(assembly.Dependencies))
	for index, dependency := range assembly.Dependencies {
		canonical, canonicalErr := canonicalAssemblyDependency(dependency)
		if canonicalErr != nil {
			return nil, fmt.Errorf("preflight graph assembly dependency %q: %w", dependency.Name, canonicalErr)
		}
		canonicalDependencies[index] = canonical
		dependencyNames[index] = canonical.Name
	}
	if err := requireExactInventory("dependency", requiredDependencies, dependencyNames); err != nil {
		return nil, fmt.Errorf("preflight graph assembly: %w", err)
	}
	for _, dependency := range canonicalDependencies {
		expected := requiredDependencies[dependency.Name]
		if dependency.Artifact != expected.Artifact {
			return nil, fmt.Errorf(
				"preflight graph assembly dependency %q: artifact drifted from the frozen resolution",
				dependency.Name,
			)
		}
		if dependency.Scope != expected.Scope {
			return nil, fmt.Errorf(
				"preflight graph assembly dependency %q: scope drifted from the frozen resolution",
				dependency.Name,
			)
		}
	}

	providerNames := make([]string, len(assembly.SecretProviders))
	for index, provider := range assembly.SecretProviders {
		providerNames[index] = provider.Name
	}
	if err := requireExactInventory("secret provider", requiredProviders, providerNames); err != nil {
		return nil, fmt.Errorf("preflight graph assembly: %w", err)
	}

	// Build fresh registries only after all three contribution surfaces have
	// exact cardinality. This avoids accepting a broad process-global catalog
	// whose unused entries would otherwise become ambient authority.
	factories := graphruntime.NewRegistry()
	for _, registration := range assembly.Implementations {
		if err := factories.RegisterFactory(registration); err != nil {
			return nil, fmt.Errorf("preflight graph assembly implementation %q: %w",
				registration.Profile.Reference, err)
		}
	}

	dependencies := graphruntime.NewDependencyRegistry()
	for _, dependency := range canonicalDependencies {
		switch {
		case dependency.RuntimeOwned:
			err = dependencies.RegisterRuntimeOwned(dependency.Name, dependency.Artifact)
		case dependency.Scope == graphconfig.DependencyScopeMount:
			err = dependencies.RegisterMountScoped(dependency.Name, dependency.Artifact)
		default:
			err = dependencies.Register(dependency.Name, dependency.Artifact, dependency.Service)
		}
		if err != nil {
			return nil, fmt.Errorf("preflight graph assembly dependency %q: %w", dependency.Name, err)
		}
	}

	providers := graphsecret.NewRegistry()
	for _, provider := range assembly.SecretProviders {
		if err := providers.Register(provider.Name, provider.Artifact, provider.Provider); err != nil {
			return nil, fmt.Errorf("preflight graph assembly secret provider %q: %w", provider.Name, err)
		}
	}

	prepared, err := graphruntime.PreparePlan(ctx, plan, graphruntime.PlanPreparation{
		Factories: factories, Dependencies: dependencies,
		SecretCatalog: secretCatalog, SecretProviders: providers,
	})
	if err != nil {
		return nil, fmt.Errorf("preflight graph assembly: %w", err)
	}
	return prepared, nil
}

// Launch performs exact resource-free Preflight and then mounts only the
// resulting sealed plan. There is no legacy serve/binding fallback. Callers
// that need to inspect or approve identities before acquisition should call
// Preflight and PreparedPlan.Mount separately.
func Launch(
	ctx context.Context,
	plan *graphconfig.Plan,
	assembly Assembly,
	options graphruntime.PreparedMountOptions,
) (*graphruntime.Mounted, error) {
	prepared, err := Preflight(ctx, plan, assembly)
	if err != nil {
		return nil, err
	}
	mounted, err := prepared.Mount(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("launch graph assembly: %w", err)
	}
	return mounted, nil
}

func implementationInventory(
	resolution graphconfig.Resolution,
) (map[string]struct{}, error) {
	profiles := make(map[string]graphconfig.ImplementationResolution, len(resolution.Nodes))
	for _, node := range resolution.Nodes {
		profile := node.Implementation
		if previous, found := profiles[profile.Reference]; found {
			if !reflect.DeepEqual(previous, profile) {
				return nil, fmt.Errorf(
					"frozen plan resolves implementation %q ambiguously", profile.Reference,
				)
			}
			continue
		}
		profiles[profile.Reference] = profile
	}
	result := make(map[string]struct{}, len(profiles))
	for reference := range profiles {
		result[reference] = struct{}{}
	}
	return result, nil
}

func dependencyInventory(
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

func secretInventory(
	plan *graphconfig.Plan,
	document *graphsecret.Document,
) (*graphsecret.Document, map[string]struct{}, error) {
	required := make(map[string]struct{})
	wantedFingerprint := plan.SecretCatalogFingerprint()
	if wantedFingerprint == "" {
		if document != nil {
			return nil, nil, errors.New(
				"secret catalog is excess because the frozen plan has no secret-catalog identity",
			)
		}
		return nil, required, nil
	}
	if document == nil {
		return nil, nil, errors.New("frozen plan requires its exact graph-scoped secret catalog")
	}

	// Snapshot caller-owned map state before fingerprinting or constructing a
	// provider Store. The sealed runtime never consults document again.
	copy := cloneSecretCatalog(*document)
	if err := deployment.ValidateSecretCatalog(plan.Deployment(), copy); err != nil {
		return nil, nil, fmt.Errorf("secret catalog does not exactly cover the frozen deployment: %w", err)
	}
	fingerprint, err := graphconfig.SecretCatalogFingerprint(&copy)
	if err != nil {
		return nil, nil, errors.New("secret catalog is invalid")
	}
	if fingerprint != wantedFingerprint {
		return nil, nil, errors.New("secret catalog drifted from the frozen plan")
	}
	for _, binding := range copy.Secrets {
		required[binding.Provider] = struct{}{}
	}
	return &copy, required, nil
}

func cloneSecretCatalog(source graphsecret.Document) graphsecret.Document {
	result := source
	if source.Secrets == nil {
		return result
	}
	result.Secrets = make(map[string]graphsecret.Binding, len(source.Secrets))
	for reference, binding := range source.Secrets {
		result.Secrets[reference] = binding
	}
	return result
}

func requireExactInventory[V any](
	kind string,
	required map[string]V,
	actual []string,
) error {
	if len(actual) > maximumAssemblyEntries {
		return fmt.Errorf("%s inventory has %d entries; maximum is %d",
			kind, len(actual), maximumAssemblyEntries)
	}
	seen := make(map[string]struct{}, len(actual))
	for _, name := range actual {
		if err := canonicalAssemblyName(kind, name); err != nil {
			return err
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("%s inventory is ambiguous: %q is contributed more than once", kind, name)
		}
		seen[name] = struct{}{}
	}

	missing := make([]string, 0)
	for name := range required {
		if _, found := seen[name]; !found {
			missing = append(missing, name)
		}
	}
	excess := make([]string, 0)
	for name := range seen {
		if _, wanted := required[name]; !wanted {
			excess = append(excess, name)
		}
	}
	if len(missing) == 0 && len(excess) == 0 {
		return nil
	}
	sort.Strings(missing)
	sort.Strings(excess)
	parts := make([]string, 0, 2)
	if len(missing) != 0 {
		parts = append(parts, "missing ["+strings.Join(missing, ", ")+"]")
	}
	if len(excess) != 0 {
		parts = append(parts, "excess ["+strings.Join(excess, ", ")+"]")
	}
	return fmt.Errorf("%s inventory is not exact: %s", kind, strings.Join(parts, "; "))
}

func canonicalAssemblyName(kind, value string) error {
	if value == "" || value != strings.TrimSpace(value) ||
		strings.ContainsAny(value, "\x00\r\n") || len(value) > maximumAssemblyText {
		return fmt.Errorf("%s name %q is not canonical", kind, value)
	}
	return nil
}

func canonicalAssemblyDependency(source Dependency) (Dependency, error) {
	result := source
	if err := canonicalAssemblyName("dependency", result.Name); err != nil {
		return Dependency{}, err
	}
	if err := result.Artifact.Validate(); err != nil {
		return Dependency{}, fmt.Errorf("artifact: %w", err)
	}
	if result.Scope == "" {
		switch {
		case result.RuntimeOwned:
			result.Scope = graphconfig.DependencyScopeMount
		case !nilAssemblyService(result.Service):
			result.Scope = graphconfig.DependencyScopeProcess
		default:
			return Dependency{}, errors.New("scope is required when ownership cannot be inferred")
		}
	}
	if err := result.Scope.Validate(); err != nil {
		return Dependency{}, err
	}
	if result.RuntimeOwned && !nilAssemblyService(result.Service) {
		return Dependency{}, errors.New("runtime-owned dependency cannot carry a service")
	}
	if _, standard := graphruntime.StandardDependencyArtifact(result.Name); standard && result.Scope == graphconfig.DependencyScopeMount && !result.RuntimeOwned {
		return Dependency{}, errors.New("use RegisterRuntimeOwned for runtime-owned coeffects")
	}
	switch result.Scope {
	case graphconfig.DependencyScopeProcess:
		if result.RuntimeOwned {
			return Dependency{}, errors.New("process-scoped dependency cannot be runtime-owned")
		}
		if nilAssemblyService(result.Service) {
			return Dependency{}, errors.New("process-scoped dependency has nil service")
		}
	case graphconfig.DependencyScopeMount:
		if !nilAssemblyService(result.Service) {
			return Dependency{}, errors.New("mount-scoped dependency cannot carry a preflight service")
		}
	}
	return result, nil
}

func nilAssemblyService(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
