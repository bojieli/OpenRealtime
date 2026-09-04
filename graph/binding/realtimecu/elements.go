package realtimecu

import (
	"fmt"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

// RegisterElementDescriptors contributes the Realtime-CU-specific state and
// policy contracts to a caller-owned graph catalog.
func RegisterElementDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return fmt.Errorf("register Realtime-CU element descriptors: nil catalog")
	}
	for _, descriptor := range []element.Descriptor{
		ObservationCommitDescriptor(), ActivationDescriptor(),
		CancellationCoordinatorDescriptor(),
	} {
		if err := catalog.Register(descriptor); err != nil {
			return fmt.Errorf("register Realtime-CU descriptor %s: %w", descriptor.Name, err)
		}
	}
	return nil
}

// ElementFactoryRegistrations returns exact, resource-free in-process
// registrations. Mount is still the first operation permitted to touch the
// session trajectory store.
func ElementFactoryRegistrations() ([]graphruntime.FactoryRegistration, error) {
	factories := []element.Factory{
		observationCommitFactory{}, activationFactory{}, cancellationCoordinatorFactory{},
	}
	result := make([]graphruntime.FactoryRegistration, len(factories))
	for index, factory := range factories {
		descriptor := factory.Descriptor()
		identity, err := descriptor.Identity()
		if err != nil {
			return nil, fmt.Errorf("identify Realtime-CU factory %s: %w", descriptor.Name, err)
		}
		artifact := inspect.ArtifactIdentity{ID: identity.Name, Digest: identity.Digest}
		if err := artifact.Validate(); err != nil {
			return nil, err
		}
		result[index] = graphruntime.FactoryRegistration{
			Profile: graphruntime.FactoryProfile{
				Reference: identity.Name, Artifact: artifact,
				Transports: []string{"in-process"},
			},
			Factory: factory,
		}
	}
	return result, nil
}
