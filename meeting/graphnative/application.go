package graphnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	// ApplicationReference is the stable host-registry key for the Meeting
	// Assistant graph application. Installed executable material is selected
	// separately by the registration's exact Artifact identity.
	ApplicationReference                          = "application.openrealtime.meeting-assistant.v1"
	ApplicationFormatVersion               uint64 = 1
	maximumApplicationOptionalDependencies        = 65_536
)

// ApplicationArtifactPaths repeats the immutable bundle member names selected
// by a launch profile without embedding their bytes. The host loads and owns
// those bytes; the profile's complete Plan identity detects content drift.
type ApplicationArtifactPaths struct {
	Topology   string `json:"topology"`
	Values     string `json:"values"`
	Lock       string `json:"lock"`
	Channels   string `json:"channels,omitempty"`
	Deployment string `json:"deployment"`
}

// ApplicationPlanSelection contains the non-secret plan choices that are not
// already fixed by the bundle bytes. Host-owned safety limits remain outside
// the serialized document and the resulting complete plan identity is checked
// by graph/launch/profile before a provider can be exposed.
type ApplicationPlanSelection struct {
	Revision             uint64   `json:"revision"`
	OptionalDependencies []string `json:"optional_dependencies,omitempty"`
}

// ApplicationConfig is the strict plugin-owned JSON object carried by a
// generic graph launch profile. Adapter metadata is deliberately repeated:
// the application exact-matches it against installed code before LaunchConfig,
// and the generic resolver exact-matches the returned selection a second time.
type ApplicationConfig struct {
	FormatVersion uint64                         `json:"format_version"`
	Artifacts     ApplicationArtifactPaths       `json:"artifacts"`
	Plan          ApplicationPlanSelection       `json:"plan"`
	Adapter       launchprofile.AdapterSelection `json:"adapter"`
}

// ApplicationHostConfig is process-private application inventory. It carries
// immutable bundle bytes, broad resource-free plugin metadata, mount-time
// factories, and exact executable identities. None of these functions or
// credential locators enters serialized profile JSON.
type ApplicationHostConfig struct {
	ApplicationArtifact inspect.ArtifactIdentity
	ProviderArtifact    inspect.ArtifactIdentity

	Artifacts     graphconfig.Artifacts
	PlanOptions   graphconfig.Options
	Plugins       graphlaunch.Catalog
	GraphMetadata graphcatalog.Metadata
	SecretCatalog *graphsecret.Document
	Evidence      graphevidence.Document
	Adapters      []AdapterPluginConfig

	Inspection      graphruntime.InspectionConfig
	ShutdownTimeout time.Duration
	TraceRecording  *graphbinding.TraceRecordingConfig
	Readiness       []graphlaunch.ReadinessCheck
}

// NewApplicationRegistration snapshots and validates one host-installed
// Meeting Assistant application plugin. Its returned factory only decodes and
// composes resource-free launch configuration: it never resolves a secret,
// invokes a mount dependency or adapter factory, dials a model, mounts a graph,
// or opens a listener.
func NewApplicationRegistration(
	source ApplicationHostConfig,
) (launchprofile.Registration, error) {
	if err := source.ApplicationArtifact.Validate(); err != nil {
		return launchprofile.Registration{}, fmt.Errorf("meeting application artifact: %w", err)
	}
	if err := source.ProviderArtifact.Validate(); err != nil {
		return launchprofile.Registration{}, fmt.Errorf("meeting provider artifact: %w", err)
	}
	if source.PlanOptions.Catalog != nil || source.PlanOptions.Loader != nil ||
		source.PlanOptions.SchemaResolver != nil || source.PlanOptions.Discovery != nil ||
		source.PlanOptions.SecretCatalog != nil {
		return launchprofile.Registration{}, errors.New(
			"meeting application host plan options may contain only safety limits",
		)
	}
	if source.PlanOptions.Revision != 0 || len(source.PlanOptions.OptionalDependencies) != 0 {
		return launchprofile.Registration{}, errors.New(
			"meeting application revision and optional dependencies belong to profile JSON",
		)
	}

	host := cloneApplicationHost(source)
	installed := make(map[string]installedApplicationAdapter, len(host.Adapters))
	for index, config := range host.Adapters {
		_, selection, err := AdapterPlugin(config)
		if err != nil {
			return launchprofile.Registration{}, fmt.Errorf(
				"meeting application adapter %d: %w", index, err,
			)
		}
		if _, duplicate := installed[selection.Reference]; duplicate {
			return launchprofile.Registration{}, fmt.Errorf(
				"meeting application adapter reference %q is installed more than once",
				selection.Reference,
			)
		}
		installed[selection.Reference] = installedApplicationAdapter{
			selection: selection, config: cloneAdapterPluginConfig(config),
		}
	}
	if len(installed) == 0 {
		return launchprofile.Registration{}, errors.New(
			"meeting application requires at least one exact adapter plugin",
		)
	}
	expectedPaths := applicationArtifactPaths(host.Artifacts)
	registration := launchprofile.Registration{
		Reference: ApplicationReference, Artifact: host.ApplicationArtifact,
		ProviderArtifact: host.ProviderArtifact,
		Factory: func(ctx context.Context, raw json.RawMessage) (graphlaunch.Config, error) {
			if ctx == nil {
				return graphlaunch.Config{}, errors.New("resolve meeting application: nil context")
			}
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.Config{}, err
			}
			config, err := decodeApplicationConfig(raw)
			if err != nil {
				return graphlaunch.Config{}, err
			}
			if config.Artifacts != expectedPaths {
				return graphlaunch.Config{}, errors.New(
					"meeting application bundle paths drifted from the installed artifact set",
				)
			}
			adapter, found := installed[config.Adapter.Reference]
			if !found {
				return graphlaunch.Config{}, fmt.Errorf(
					"meeting application adapter registry is missing %q", config.Adapter.Reference,
				)
			}
			if !applicationAdapterMatches(config.Adapter, adapter.selection) {
				return graphlaunch.Config{}, fmt.Errorf(
					"meeting application adapter %q runtime/profile identity drifted",
					config.Adapter.Reference,
				)
			}
			if err := context.Cause(ctx); err != nil {
				return graphlaunch.Config{}, err
			}
			options := host.PlanOptions
			options.Revision = config.Plan.Revision
			options.OptionalDependencies = slices.Clone(config.Plan.OptionalDependencies)
			launch, err := LaunchConfig(ProviderConfig{
				Artifacts: cloneApplicationArtifacts(host.Artifacts), PlanOptions: options,
				Plugins:       cloneApplicationCatalog(host.Plugins),
				GraphMetadata: cloneApplicationGraphMetadata(host.GraphMetadata),
				SecretCatalog: cloneApplicationSecretCatalog(host.SecretCatalog),
				Evidence:      graphevidence.Clone(host.Evidence),
				Adapter:       adapter.config, Inspection: host.Inspection,
				ShutdownTimeout: host.ShutdownTimeout,
				TraceRecording:  cloneApplicationTraceConfig(host.TraceRecording),
				Readiness:       slices.Clone(host.Readiness),
			})
			if err != nil {
				return graphlaunch.Config{}, err
			}
			if launch.Adapter != adapter.selection {
				return graphlaunch.Config{}, errors.New(
					"meeting application launch changed the exact adapter selection",
				)
			}
			return launch, nil
		},
	}
	// Reuse the generic registry's registration validation without invoking the
	// factory. Returning the registration rather than a private registry keeps
	// independent applications composable in one host inventory.
	if _, err := launchprofile.NewRegistry([]launchprofile.Registration{registration}); err != nil {
		return launchprofile.Registration{}, err
	}
	return registration, nil
}

type installedApplicationAdapter struct {
	selection graphlaunch.AdapterSelection
	config    AdapterPluginConfig
}

func decodeApplicationConfig(source json.RawMessage) (ApplicationConfig, error) {
	var config ApplicationConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return ApplicationConfig{}, fmt.Errorf("decode meeting application configuration: %w", err)
	}
	if config.FormatVersion != ApplicationFormatVersion {
		return ApplicationConfig{}, fmt.Errorf(
			"meeting application configuration uses format %d, want %d",
			config.FormatVersion, ApplicationFormatVersion,
		)
	}
	if config.Plan.Revision == 0 {
		return ApplicationConfig{}, errors.New(
			"meeting application configuration requires a positive plan revision",
		)
	}
	if len(config.Plan.OptionalDependencies) > maximumApplicationOptionalDependencies {
		return ApplicationConfig{}, fmt.Errorf(
			"meeting application selects %d optional dependencies; maximum is %d",
			len(config.Plan.OptionalDependencies), maximumApplicationOptionalDependencies,
		)
	}
	previous := ""
	for index, dependency := range config.Plan.OptionalDependencies {
		if dependency == "" || dependency != strings.TrimSpace(dependency) ||
			strings.ContainsAny(dependency, "\x00\r\n") {
			return ApplicationConfig{}, fmt.Errorf(
				"meeting application optional dependency %d is not canonical", index,
			)
		}
		if index != 0 && dependency <= previous {
			return ApplicationConfig{}, errors.New(
				"meeting application optional dependencies must be strictly sorted and unique",
			)
		}
		previous = dependency
	}
	config.Plan.OptionalDependencies = slices.Clone(config.Plan.OptionalDependencies)
	return config, nil
}

func applicationAdapterMatches(
	profile launchprofile.AdapterSelection, installed graphlaunch.AdapterSelection,
) bool {
	return profile.Reference == installed.Reference &&
		profile.RuntimeArtifact == installed.RuntimeArtifact &&
		profile.ProfileName == installed.ProfileName &&
		profile.ProfileRevision == installed.ProfileRevision
}

func applicationArtifactPaths(artifacts graphconfig.Artifacts) ApplicationArtifactPaths {
	return ApplicationArtifactPaths{
		Topology: artifacts.Topology.Path, Values: artifacts.Values.Path,
		Lock: artifacts.Lock.Path, Channels: artifacts.Channels.Path,
		Deployment: artifacts.Deployment.Path,
	}
}

func cloneApplicationHost(source ApplicationHostConfig) ApplicationHostConfig {
	result := source
	result.Artifacts = cloneApplicationArtifacts(source.Artifacts)
	result.PlanOptions.OptionalDependencies = slices.Clone(source.PlanOptions.OptionalDependencies)
	result.Plugins = cloneApplicationCatalog(source.Plugins)
	result.GraphMetadata = cloneApplicationGraphMetadata(source.GraphMetadata)
	result.SecretCatalog = cloneApplicationSecretCatalog(source.SecretCatalog)
	result.Evidence = graphevidence.Clone(source.Evidence)
	result.Adapters = make([]AdapterPluginConfig, len(source.Adapters))
	for index := range source.Adapters {
		result.Adapters[index] = cloneAdapterPluginConfig(source.Adapters[index])
	}
	result.TraceRecording = cloneApplicationTraceConfig(source.TraceRecording)
	result.Readiness = slices.Clone(source.Readiness)
	return result
}

func cloneApplicationGraphMetadata(source graphcatalog.Metadata) graphcatalog.Metadata {
	result := source
	result.Tags = slices.Clone(source.Tags)
	result.Profiles = slices.Clone(source.Profiles)
	return result
}

func cloneApplicationArtifacts(source graphconfig.Artifacts) graphconfig.Artifacts {
	clone := func(artifact graphconfig.Artifact) graphconfig.Artifact {
		artifact.Data = slices.Clone(artifact.Data)
		return artifact
	}
	return graphconfig.Artifacts{
		Topology: clone(source.Topology), Values: clone(source.Values), Lock: clone(source.Lock),
		Channels: clone(source.Channels), Deployment: clone(source.Deployment),
	}
}

func cloneAdapterPluginConfig(source AdapterPluginConfig) AdapterPluginConfig {
	result := source
	result.Profile.AdditionalObservers = slices.Clone(source.Profile.AdditionalObservers)
	return result
}

func cloneApplicationCatalog(source graphlaunch.Catalog) graphlaunch.Catalog {
	result := source
	result.Assembly.Implementations = slices.Clone(source.Assembly.Implementations)
	for index := range result.Assembly.Implementations {
		profile := &result.Assembly.Implementations[index].Profile
		profile.Placements = slices.Clone(profile.Placements)
		profile.Transports = slices.Clone(profile.Transports)
		profile.ResourceKeys = slices.Clone(profile.ResourceKeys)
		profile.SecretSlots = slices.Clone(profile.SecretSlots)
		profile.Capabilities = slices.Clone(profile.Capabilities)
		for capability := range profile.Capabilities {
			if profile.Capabilities[capability].Adapter != nil {
				adapter := *profile.Capabilities[capability].Adapter
				profile.Capabilities[capability].Adapter = &adapter
			}
		}
	}
	result.Assembly.Dependencies = slices.Clone(source.Assembly.Dependencies)
	result.Assembly.SecretProviders = slices.Clone(source.Assembly.SecretProviders)
	result.Adapters = slices.Clone(source.Adapters)
	result.MountDependencies = slices.Clone(source.MountDependencies)
	return result
}

func cloneApplicationSecretCatalog(source *graphsecret.Document) *graphsecret.Document {
	if source == nil {
		return nil
	}
	result := *source
	result.Secrets = make(map[string]graphsecret.Binding, len(source.Secrets))
	for reference, binding := range source.Secrets {
		result.Secrets[reference] = binding
	}
	return &result
}

func cloneApplicationTraceConfig(
	source *graphbinding.TraceRecordingConfig,
) *graphbinding.TraceRecordingConfig {
	if source == nil {
		return nil
	}
	result := *source
	return &result
}
