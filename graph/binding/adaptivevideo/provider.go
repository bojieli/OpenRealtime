// Package adaptivevideo adapts the stable Realtime video and observation
// operations to the typed boundaries exported by a graph-native adaptive
// video composition.
package adaptivevideo

import (
	"context"
	"errors"
	"fmt"
	"slices"

	legacy "github.com/bojieli/OpenRealtime/binding"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphassembly "github.com/bojieli/OpenRealtime/graph/assembly"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

// ProviderRegistration is one symbolic visual-provider reference and the
// exact live descriptor its factory must return. Factory is deliberately not
// called while the dependency is created or while its mount overlay is
// assembled; the VisualObserver element owns provider acquisition.
type ProviderRegistration struct {
	Reference  string
	Descriptor perceptionelements.VisualProviderDescriptor
	Factory    perceptionelements.VisualProviderFactory
}

// ProviderDependency is an immutable contribution to both graph discovery
// and per-session mount assembly. The assembly entry contains identity only;
// each MountDependencyFactory call creates an independent registry service so
// one prepared plan can mount concurrent sessions without sharing providers.
type ProviderDependency struct {
	artifact      inspect.ArtifactIdentity
	registrations []ProviderRegistration
}

// NewProviderDependency validates and snapshots a visual-provider registry
// contribution without constructing any provider or acquiring resources.
func NewProviderDependency(
	artifact inspect.ArtifactIdentity,
	registrations []ProviderRegistration,
) (ProviderDependency, error) {
	if err := artifact.Validate(); err != nil {
		return ProviderDependency{}, fmt.Errorf("adaptive video provider dependency artifact: %w", err)
	}
	if len(registrations) == 0 {
		return ProviderDependency{}, errors.New("adaptive video provider dependency requires at least one registration")
	}
	frozen := slices.Clone(registrations)
	registry := perceptionelements.NewVisualProviderRegistry()
	for _, registration := range frozen {
		if err := registry.Register(
			registration.Reference, registration.Descriptor, registration.Factory,
		); err != nil {
			return ProviderDependency{}, fmt.Errorf("adaptive video provider dependency: %w", err)
		}
	}
	return ProviderDependency{artifact: artifact, registrations: frozen}, nil
}

// AssemblyDependency returns the resource-free, mount-scoped catalog entry
// that must be present before graph config discovery and plan creation.
func (dependency ProviderDependency) AssemblyDependency() graphassembly.Dependency {
	return graphassembly.Dependency{
		Name:     perceptionelements.VisualProviderRegistryService,
		Artifact: dependency.artifact,
		Scope:    graphconfig.DependencyScopeMount,
	}
}

// MountDependencyFactory returns the exact session overlay selected by
// AssemblyDependency. Calling the factory creates only a registry of provider
// factories; provider construction remains behind element Factory.Mount.
func (dependency ProviderDependency) MountDependencyFactory() graphbinding.MountDependencyFactory {
	artifact := dependency.artifact
	registrations := slices.Clone(dependency.registrations)
	return func(
		ctx context.Context, _ legacy.Options,
	) ([]graphruntime.PreparedMountDependency, error) {
		if ctx == nil {
			return nil, errors.New("create adaptive video mount dependency: nil context")
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		registry := perceptionelements.NewVisualProviderRegistry()
		for _, registration := range registrations {
			if err := registry.Register(
				registration.Reference, registration.Descriptor, registration.Factory,
			); err != nil {
				return nil, fmt.Errorf("create adaptive video mount dependency: %w", err)
			}
		}
		return []graphruntime.PreparedMountDependency{{
			Name:     perceptionelements.VisualProviderRegistryService,
			Artifact: artifact,
			Service:  registry,
		}}, nil
	}
}
