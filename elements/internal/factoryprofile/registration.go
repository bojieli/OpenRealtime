// Package factoryprofile constructs the exact in-process runtime profiles
// published by standard element packages. Keeping this in one internal helper
// ensures descriptor registration, config discovery, and executable assembly
// use the same reference and artifact rules.
package factoryprofile

import (
	"errors"
	"fmt"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

const InProcessTransport = "in-process"

// Entry couples a standard element factory with its existing executable
// artifact identity. A zero Artifact deliberately preserves the identity used
// by Registry.Register: descriptor name plus descriptor digest.
type Entry struct {
	Factory  element.Factory
	Artifact inspect.ArtifactIdentity
}

// Registrations validates and snapshots exact profiles without mounting a
// factory or acquiring a provider, device, secret, or other runtime resource.
func Registrations(entries ...Entry) ([]graphruntime.FactoryRegistration, error) {
	result := make([]graphruntime.FactoryRegistration, 0, len(entries))
	for index, entry := range entries {
		if entry.Factory == nil {
			return nil, fmt.Errorf("standard factory entry %d is nil", index)
		}
		descriptor := entry.Factory.Descriptor()
		identity, err := descriptor.Identity()
		if err != nil {
			return nil, fmt.Errorf("standard factory entry %d descriptor: %w", index, err)
		}
		artifact := entry.Artifact
		if artifact == (inspect.ArtifactIdentity{}) {
			artifact = inspect.ArtifactIdentity{ID: identity.Name, Digest: identity.Digest}
		}
		if err := artifact.Validate(); err != nil {
			return nil, fmt.Errorf("standard factory %s artifact: %w", identity.Name, err)
		}
		result = append(result, graphruntime.FactoryRegistration{
			Profile: graphruntime.FactoryProfile{
				Reference:  identity.Name,
				Artifact:   artifact,
				Transports: []string{InProcessTransport},
			},
			Factory: entry.Factory,
		})
	}
	if len(result) == 0 {
		return nil, errors.New("standard factory registration set is empty")
	}
	return result, nil
}

// Register publishes a complete exact profile for every supplied entry.
func Register(registry *graphruntime.Registry, entries ...Entry) error {
	if registry == nil {
		return errors.New("register standard factories: nil registry")
	}
	registrations, err := Registrations(entries...)
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
