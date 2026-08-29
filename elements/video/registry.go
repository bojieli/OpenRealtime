package video

import (
	"errors"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func Descriptors() []element.Descriptor {
	return []element.Descriptor{FrameIngressDescriptor(), AdaptiveObservationDescriptor()}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register video descriptors: nil catalog")
	}
	for _, descriptor := range Descriptors() {
		if err := catalog.Register(descriptor); err != nil {
			return err
		}
	}
	return nil
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if registry == nil {
		return errors.New("register video factories: nil registry")
	}
	registrations := []struct {
		factory  element.Factory
		artifact inspect.ArtifactIdentity
	}{
		{frameIngressFactory{}, inspect.ArtifactIdentity{ID: frameIngressRuntimeID, Revision: implementationRev}},
		{adaptiveObservationFactory{}, inspect.ArtifactIdentity{ID: policyRuntimeID, Revision: implementationRev}},
	}
	for _, registration := range registrations {
		if err := registry.RegisterArtifact("", registration.artifact, registration.factory); err != nil {
			return err
		}
	}
	return nil
}

func reportLiveResolution(reporter element.ResolutionReporter, runtimeID string) error {
	artifact := liveidentity.Artifact{ID: runtimeID, Revision: implementationRev}
	return liveidentity.Report(reporter, artifact, []element.CapabilityResolution{})
}
