package media

import (
	"errors"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func Descriptors() []element.Descriptor {
	return []element.Descriptor{RetainedMediaDescriptor(), ResolveAttachmentDescriptor()}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register media descriptors: nil catalog")
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
		return errors.New("register media factories: nil registry")
	}
	registrations := []struct {
		factory  element.Factory
		artifact inspect.ArtifactIdentity
	}{
		{retainedMediaFactory{}, inspect.ArtifactIdentity{ID: retainedMediaRuntimeID, Revision: mediaImplementationRevision}},
		{resolveAttachmentFactory{}, inspect.ArtifactIdentity{ID: attachmentResolverRuntimeID, Revision: mediaImplementationRevision}},
	}
	for _, registration := range registrations {
		if err := registry.RegisterArtifact("", registration.artifact, registration.factory); err != nil {
			return err
		}
	}
	return nil
}
