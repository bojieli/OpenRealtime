// Package elements registers the production standard-library element
// descriptors and in-process factories. Provider and application plugins add
// their own entries without modifying this package or the graph compiler.
package elements

import (
	"github.com/bojieli/OpenRealtime/elements/flow"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func RegisterDescriptors(catalog *resolve.Catalog) error {
	return flow.RegisterDescriptors(catalog)
}

func RegisterFactories(registry *graphruntime.Registry) error {
	return flow.RegisterFactories(registry)
}

func Catalog() (*resolve.Catalog, error) {
	catalog := resolve.NewCatalog()
	if err := RegisterDescriptors(catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

func RuntimeRegistry() (*graphruntime.Registry, error) {
	registry := graphruntime.NewRegistry()
	if err := RegisterFactories(registry); err != nil {
		return nil, err
	}
	return registry, nil
}
