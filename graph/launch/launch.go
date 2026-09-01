// Package launch composes exact graph configuration artifacts and a broad,
// resource-free plugin catalog into a graph-native Realtime session provider.
//
// New performs no graph mount, adapter construction, secret resolution, or
// listener creation. It creates and validates an immutable config.Plan,
// reduces the broad assembly catalog to that plan's exact contributions, and
// seals the result in binding.NativeBinding. There is deliberately no legacy
// binding fallback or implementation-name switch in this package.
package launch

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	legacy "github.com/bojieli/OpenRealtime/binding"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

const (
	maximumAdapterPlugins         = 65_536
	maximumMountDependencyPlugins = 65_536
	maximumPluginReferenceBytes   = 64 << 10
)

// AdapterBinder derives one immutable Realtime-to-graph projection for an
// already-created plan. It is a metadata-only operation: it must not mount a
// graph, construct a session adapter, resolve a secret, or acquire a resource.
type AdapterBinder func(context.Context, *graphconfig.Plan) (BoundAdapter, error)

// BoundAdapter is the exact profile/factory pair returned by a selected
// AdapterBinder. New verifies that Registration still has the reference and
// runtime artifact published by its catalog plugin.
type BoundAdapter struct {
	Profile      graphbinding.SessionAdapterProfile
	Registration graphbinding.AdapterRegistration
}

// AdapterPlugin publishes immutable runtime metadata plus the resource-free
// binder that derives its graph-fingerprint-bound profile. Keeping Bind behind
// explicit selection lets New construct the plan first without invoking every
// adapter in a broad build inventory.
type AdapterPlugin struct {
	Reference string
	Artifact  inspect.ArtifactIdentity
	Bind      AdapterBinder
}

// AdapterSelection pins both the symbolic registration and its immutable
// profile/runtime material. Reference alone is not an exact selection because
// a catalog entry could otherwise change code or its boundary projection
// without changing the selector.
type AdapterSelection struct {
	Reference       string
	RuntimeArtifact inspect.ArtifactIdentity
	ProfileName     string
	ProfileRevision uint64
}

// Selection returns an exact runtime selector with the expected public
// profile identity. The profile fingerprint itself is derived only after the
// immutable plan exists and is preserved in NativeRuntime evidence.
func (plugin AdapterPlugin) Selection(profileName string, profileRevision uint64) AdapterSelection {
	return AdapterSelection{
		Reference: plugin.Reference, RuntimeArtifact: plugin.Artifact,
		ProfileName: profileName, ProfileRevision: profileRevision,
	}
}

// MountDependencyPlugin associates one catalog-attested, mount-scoped
// dependency with the per-session service contribution that realizes it.
// Factory is called only for a session Start and its result must contain
// exactly this one Name/Artifact contribution. Runtime-owned and
// process-scoped dependencies must not have entries here.
type MountDependencyPlugin struct {
	Name     string
	Artifact inspect.ArtifactIdentity
	Factory  graphbinding.MountDependencyFactory
}

// Catalog is the complete process inventory available to this launcher.
// Assembly may be broad; New reduces it to one plan. Adapters and
// MountDependencies are slice-based so duplicate plugin contributions cannot
// be hidden by map overwrite.
type Catalog struct {
	Assembly          graphassembly.Catalog
	Adapters          []AdapterPlugin
	MountDependencies []MountDependencyPlugin
}

// Config contains the exact artifacts and selection for one graph-native
// provider. GraphMetadata is required public discovery metadata; its Profiles
// field must be empty because New derives empirical artifact identities only
// from Evidence. Evidence is a required separate manifest; an explicit empty
// Profiles list means that the graph makes no empirical claims.
// PlanOptions.Discovery and PlanOptions.SecretCatalog must be nil: New derives
// both from Catalog and SecretCatalog so there is only one source of plugin and
// secret-catalog authority.
type Config struct {
	Artifacts     graphconfig.Artifacts
	PlanOptions   graphconfig.Options
	Catalog       Catalog
	GraphMetadata graphcatalog.Metadata
	SecretCatalog *graphsecret.Document
	Evidence      graphevidence.Document
	Adapter       AdapterSelection

	Inspection      graphruntime.InspectionConfig
	ShutdownTimeout time.Duration
	TraceRecording  *graphbinding.TraceRecordingConfig
	// Readiness contains credential/static-configuration checks for only the
	// plugins selected by this exact launch. Checks are retained as lazy
	// callbacks: New validates their identities but never invokes them during
	// resource-free profile preflight.
	Readiness []ReadinessCheck
}

// ReadinessCheck is one selected plugin's lazy readiness contract.
type ReadinessCheck struct {
	Name  string
	Check func(context.Context) error
}

// Result retains the immutable plan, bound evidence, and derived catalog entry
// for management/discovery uses, plus the prepared NativeBinding used directly
// as a server.SessionProvider.
type Result struct {
	Plan         *graphconfig.Plan
	Evidence     graphevidence.Document
	CatalogEntry graphcatalog.Entry
	Binding      *graphbinding.NativeBinding
	Readiness    []ReadinessCheck
}

// New validates and seals a graph-native session provider without acquiring a
// runtime resource. The order is intentional: the complete broad catalog,
// exact plan, bound evidence, selected assembly, adapter, and dependency
// inventory are all rejected before graphbinding can expose a provider to a
// listener.
func New(ctx context.Context, source Config) (Result, error) {
	if ctx == nil {
		return Result{}, errors.New("launch graph-native provider: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return Result{}, err
	}
	if source.PlanOptions.Discovery != nil {
		return Result{}, errors.New(
			"launch graph-native provider: plan discovery is derived from the plugin catalog",
		)
	}
	if source.PlanOptions.SecretCatalog != nil {
		return Result{}, errors.New(
			"launch graph-native provider: plan secret catalog must be supplied through SecretCatalog",
		)
	}

	config := snapshotConfig(source)
	if err := validateReadiness(config.Readiness); err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider readiness: %w", err)
	}
	discovery, err := config.Catalog.Assembly.Discovery()
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider catalog: %w", err)
	}
	adapters, err := indexAdapters(config.Catalog.Adapters)
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider catalog: %w", err)
	}
	mountDependencies, err := indexMountDependencies(
		config.Catalog.Assembly.Dependencies,
		config.Catalog.MountDependencies,
	)
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider catalog: %w", err)
	}

	options := config.PlanOptions
	options.Discovery = discovery
	options.SecretCatalog = config.SecretCatalog
	plan, err := graphconfig.Create(ctx, config.Artifacts, options)
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider plan: %w", err)
	}
	evidence, err := graphevidence.Bind(plan, config.Evidence)
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider evidence: %w", err)
	}
	if len(config.GraphMetadata.Profiles) != 0 {
		return Result{}, errors.New(
			"launch graph-native provider catalog: empirical profiles are derived from evidence",
		)
	}
	metadata := config.GraphMetadata
	metadata.Profiles = evidenceArtifacts(evidence)
	catalogEntry, err := graphcatalog.NewEntry(plan, metadata)
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider catalog: %w", err)
	}

	selectedAssembly, err := config.Catalog.Assembly.Select(plan, config.SecretCatalog)
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider assembly: %w", err)
	}
	adapter, err := bindAdapter(ctx, plan, config.Adapter, adapters)
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider adapter: %w", err)
	}
	mountFactory, err := selectMountDependencies(selectedAssembly, mountDependencies)
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider dependencies: %w", err)
	}

	// NewNative deliberately receives the exact graph-scoped catalog, not the
	// broad process inventory. Its own Select and Preflight calls remain the
	// authoritative validation of assembly/runtime contracts.
	exactCatalog := graphassembly.Catalog{
		Implementations: selectedAssembly.Implementations,
		Dependencies:    selectedAssembly.Dependencies,
		SecretProviders: selectedAssembly.SecretProviders,
	}
	native, err := graphbinding.NewNative(graphbinding.NativeConfig{
		Plan: plan, Catalog: exactCatalog,
		SecretCatalog:     selectedAssembly.SecretCatalog,
		MountDependencies: mountFactory,
		AdapterProfile:    adapter.Profile,
		Adapter:           adapter.Registration,
		Inspection:        config.Inspection,
		ShutdownTimeout:   config.ShutdownTimeout,
		TraceRecording:    config.TraceRecording,
	})
	if err != nil {
		return Result{}, fmt.Errorf("launch graph-native provider binding: %w", err)
	}
	return Result{
		Plan: plan, Evidence: evidence, CatalogEntry: catalogEntry, Binding: native,
		Readiness: slices.Clone(config.Readiness),
	}, nil
}

func evidenceArtifacts(document graphevidence.Document) []inspect.ArtifactIdentity {
	seen := make(map[inspect.ArtifactIdentity]struct{}, len(document.Profiles))
	result := make([]inspect.ArtifactIdentity, 0, len(document.Profiles))
	for _, profile := range document.Profiles {
		if _, duplicate := seen[profile.Artifact]; duplicate {
			continue
		}
		seen[profile.Artifact] = struct{}{}
		result = append(result, profile.Artifact)
	}
	return result
}

func validateReadiness(checks []ReadinessCheck) error {
	if len(checks) > maximumAdapterPlugins {
		return fmt.Errorf("catalog has %d checks; maximum is %d", len(checks), maximumAdapterPlugins)
	}
	seen := make(map[string]struct{}, len(checks))
	for index, check := range checks {
		if check.Name == "" || strings.TrimSpace(check.Name) != check.Name ||
			len(check.Name) > maximumPluginReferenceBytes {
			return fmt.Errorf("check %d has a non-canonical name", index)
		}
		if check.Check == nil {
			return fmt.Errorf("check %q has a nil callback", check.Name)
		}
		if _, duplicate := seen[check.Name]; duplicate {
			return fmt.Errorf("check %q is registered more than once", check.Name)
		}
		seen[check.Name] = struct{}{}
	}
	return nil
}

func indexAdapters(source []AdapterPlugin) (map[string]AdapterPlugin, error) {
	if len(source) > maximumAdapterPlugins {
		return nil, fmt.Errorf("adapter catalog has %d entries; maximum is %d",
			len(source), maximumAdapterPlugins)
	}
	result := make(map[string]AdapterPlugin, len(source))
	for index, plugin := range source {
		// Reuse binding's registration validation with a sentinel factory. The
		// actual factory cannot exist until Bind receives the selected plan.
		if err := (graphbinding.AdapterRegistration{
			Reference: plugin.Reference, Artifact: plugin.Artifact,
			Factory: uncalledAdapterFactory,
		}).Validate(); err != nil {
			return nil, fmt.Errorf("adapter contribution %d registration: %w", index, err)
		}
		if plugin.Bind == nil {
			return nil, fmt.Errorf("adapter contribution %d has nil binder", index)
		}
		reference := plugin.Reference
		if len(reference) > maximumPluginReferenceBytes {
			return nil, fmt.Errorf("adapter contribution %d reference exceeds %d bytes",
				index, maximumPluginReferenceBytes)
		}
		if _, duplicate := result[reference]; duplicate {
			return nil, fmt.Errorf(
				"adapter catalog is ambiguous: %q is contributed more than once", reference,
			)
		}
		result[reference] = plugin
	}
	return result, nil
}

func bindAdapter(
	ctx context.Context,
	plan *graphconfig.Plan,
	selection AdapterSelection,
	adapters map[string]AdapterPlugin,
) (BoundAdapter, error) {
	plugin, found := adapters[selection.Reference]
	if !found {
		return BoundAdapter{}, fmt.Errorf(
			"adapter catalog is missing explicitly selected reference %q", selection.Reference,
		)
	}
	if plugin.Artifact != selection.RuntimeArtifact {
		return BoundAdapter{}, fmt.Errorf(
			"adapter %q runtime artifact drifted from selected identity", selection.Reference,
		)
	}
	bound, err := plugin.Bind(ctx, plan)
	if err != nil {
		return BoundAdapter{}, fmt.Errorf("adapter %q binder: %w", selection.Reference, err)
	}
	bound.Profile = bound.Profile.Clone()
	if bound.Registration.Reference != plugin.Reference ||
		bound.Registration.Artifact != plugin.Artifact {
		return BoundAdapter{}, fmt.Errorf(
			"adapter %q binder registration drifted from catalog identity", selection.Reference,
		)
	}
	if bound.Profile.Name != selection.ProfileName ||
		bound.Profile.Revision != selection.ProfileRevision {
		return BoundAdapter{}, fmt.Errorf(
			"adapter %q profile identity drifted: got %s@%d, want %s@%d",
			selection.Reference, bound.Profile.Name, bound.Profile.Revision,
			selection.ProfileName, selection.ProfileRevision,
		)
	}
	if err := bound.Profile.ValidateGraph(plan.Graph()); err != nil {
		return BoundAdapter{}, fmt.Errorf("adapter %q graph profile: %w", selection.Reference, err)
	}
	if err := bound.Registration.Validate(); err != nil {
		return BoundAdapter{}, fmt.Errorf("adapter %q bound registration: %w", selection.Reference, err)
	}
	return bound, nil
}

func uncalledAdapterFactory(
	context.Context, *graphruntime.Mounted, legacy.Options,
	graphbinding.SessionAdapterProfile,
) (graphbinding.SessionAdapter, error) {
	return nil, errors.New("adapter validation sentinel must not be called")
}

func indexMountDependencies(
	assemblyDependencies []graphassembly.Dependency,
	source []MountDependencyPlugin,
) (map[string]MountDependencyPlugin, error) {
	if len(source) > maximumMountDependencyPlugins {
		return nil, fmt.Errorf("mount dependency catalog has %d entries; maximum is %d",
			len(source), maximumMountDependencyPlugins)
	}
	metadata := make(map[string]graphassembly.Dependency, len(assemblyDependencies))
	for _, dependency := range assemblyDependencies {
		metadata[dependency.Name] = dependency
	}
	result := make(map[string]MountDependencyPlugin, len(source))
	for index, plugin := range source {
		dependency, found := metadata[plugin.Name]
		if !found {
			return nil, fmt.Errorf(
				"mount dependency contribution %q has no assembly metadata", plugin.Name,
			)
		}
		if dependency.Scope != graphconfig.DependencyScopeMount || dependency.RuntimeOwned {
			return nil, fmt.Errorf(
				"mount dependency contribution %q is excess for a %s dependency",
				plugin.Name, dependencyKind(dependency),
			)
		}
		if plugin.Artifact != dependency.Artifact {
			return nil, fmt.Errorf(
				"mount dependency contribution %q artifact drifted from assembly metadata",
				plugin.Name,
			)
		}
		if plugin.Factory == nil {
			return nil, fmt.Errorf("mount dependency contribution %q has nil factory", plugin.Name)
		}
		if len(plugin.Name) > maximumPluginReferenceBytes || strings.TrimSpace(plugin.Name) != plugin.Name {
			return nil, fmt.Errorf("mount dependency contribution %d has a non-canonical name", index)
		}
		if _, duplicate := result[plugin.Name]; duplicate {
			return nil, fmt.Errorf(
				"mount dependency catalog is ambiguous: %q is contributed more than once",
				plugin.Name,
			)
		}
		result[plugin.Name] = plugin
	}
	return result, nil
}

func dependencyKind(dependency graphassembly.Dependency) string {
	if dependency.RuntimeOwned {
		return "runtime-owned"
	}
	if dependency.Scope == graphconfig.DependencyScopeProcess {
		return "process-scoped"
	}
	return string(dependency.Scope)
}

func selectMountDependencies(
	selected graphassembly.Assembly,
	catalog map[string]MountDependencyPlugin,
) (graphbinding.MountDependencyFactory, error) {
	plugins := make([]MountDependencyPlugin, 0, len(selected.Dependencies))
	for _, dependency := range selected.Dependencies {
		if dependency.Scope != graphconfig.DependencyScopeMount || dependency.RuntimeOwned {
			continue
		}
		plugin, found := catalog[dependency.Name]
		if !found {
			return nil, fmt.Errorf(
				"mount dependency catalog is missing selected contribution %q", dependency.Name,
			)
		}
		if plugin.Artifact != dependency.Artifact {
			return nil, fmt.Errorf(
				"mount dependency %q drifted from selected assembly", dependency.Name,
			)
		}
		plugins = append(plugins, plugin)
	}
	if len(plugins) == 0 {
		return nil, nil
	}
	plugins = slices.Clone(plugins)
	return func(
		ctx context.Context, options legacy.Options,
	) ([]graphruntime.PreparedMountDependency, error) {
		result := make([]graphruntime.PreparedMountDependency, 0, len(plugins))
		for _, plugin := range plugins {
			contributions, err := plugin.Factory(ctx, cloneOptions(options))
			if err != nil {
				return nil, fmt.Errorf("mount dependency %q factory: %w", plugin.Name, err)
			}
			if len(contributions) != 1 {
				return nil, fmt.Errorf(
					"mount dependency %q factory contributed %d entries; want exactly one",
					plugin.Name, len(contributions),
				)
			}
			contribution := contributions[0]
			if contribution.Name != plugin.Name {
				return nil, fmt.Errorf(
					"mount dependency %q factory contributed excess name %q",
					plugin.Name, contribution.Name,
				)
			}
			if contribution.Artifact != plugin.Artifact {
				return nil, fmt.Errorf(
					"mount dependency %q factory artifact drifted from its registration",
					plugin.Name,
				)
			}
			result = append(result, contribution)
		}
		return result, nil
	}, nil
}

func cloneOptions(source legacy.Options) legacy.Options {
	result := source
	result.Settings = legacy.CloneSettings(source.Settings)
	if source.Policies != nil {
		policies := *source.Policies
		result.Policies = &policies
	}
	return result
}
