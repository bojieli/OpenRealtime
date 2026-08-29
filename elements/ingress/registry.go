package ingress

import (
	"errors"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func Descriptors() []element.Descriptor { return []element.Descriptor{UserContentDescriptor()} }

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register ingress descriptors: nil catalog")
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
		return errors.New("register ingress factories: nil registry")
	}
	return registry.RegisterArtifact("", inspect.ArtifactIdentity{
		ID: userContentRuntimeID, Revision: ingressImplementationRev,
	}, userContentFactory{})
}
