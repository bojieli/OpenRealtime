package launch

import (
	"slices"

	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphsecret "github.com/bojieli/OpenRealtime/graph/secret"
)

func snapshotConfig(source Config) Config {
	result := source
	result.Artifacts = cloneArtifacts(source.Artifacts)
	result.PlanOptions.OptionalDependencies = slices.Clone(source.PlanOptions.OptionalDependencies)
	result.Catalog = cloneCatalog(source.Catalog)
	result.SecretCatalog = cloneSecretCatalog(source.SecretCatalog)
	result.Evidence = graphevidence.Clone(source.Evidence)
	result.Readiness = slices.Clone(source.Readiness)
	if source.TraceRecording != nil {
		recording := *source.TraceRecording
		result.TraceRecording = &recording
	}
	return result
}

func cloneArtifacts(source graphconfig.Artifacts) graphconfig.Artifacts {
	result := source
	result.Topology.Data = slices.Clone(source.Topology.Data)
	result.Values.Data = slices.Clone(source.Values.Data)
	result.Lock.Data = slices.Clone(source.Lock.Data)
	result.Channels.Data = slices.Clone(source.Channels.Data)
	result.Deployment.Data = slices.Clone(source.Deployment.Data)
	return result
}

func cloneCatalog(source Catalog) Catalog {
	result := source
	result.Assembly.Implementations = make(
		[]graphruntime.FactoryRegistration, len(source.Assembly.Implementations),
	)
	for index, registration := range source.Assembly.Implementations {
		result.Assembly.Implementations[index] = cloneFactoryRegistration(registration)
	}
	result.Assembly.Dependencies = slices.Clone(source.Assembly.Dependencies)
	result.Assembly.SecretProviders = slices.Clone(source.Assembly.SecretProviders)
	result.Adapters = slices.Clone(source.Adapters)
	result.MountDependencies = slices.Clone(source.MountDependencies)
	return result
}

func cloneFactoryRegistration(
	source graphruntime.FactoryRegistration,
) graphruntime.FactoryRegistration {
	result := source
	result.Profile.Placements = slices.Clone(source.Profile.Placements)
	result.Profile.Transports = slices.Clone(source.Profile.Transports)
	result.Profile.ResourceKeys = slices.Clone(source.Profile.ResourceKeys)
	result.Profile.SecretSlots = slices.Clone(source.Profile.SecretSlots)
	if source.Profile.Capabilities != nil {
		result.Profile.Capabilities = make(
			[]inspect.CapabilityIdentity, len(source.Profile.Capabilities),
		)
		for index, capability := range source.Profile.Capabilities {
			result.Profile.Capabilities[index] = capability
			if capability.Adapter != nil {
				adapter := *capability.Adapter
				result.Profile.Capabilities[index].Adapter = &adapter
			}
		}
	}
	return result
}

func cloneSecretCatalog(source *graphsecret.Document) *graphsecret.Document {
	if source == nil {
		return nil
	}
	result := *source
	if source.Secrets != nil {
		result.Secrets = make(map[string]graphsecret.Binding, len(source.Secrets))
		for reference, binding := range source.Secrets {
			result.Secrets[reference] = binding
		}
	}
	return &result
}
