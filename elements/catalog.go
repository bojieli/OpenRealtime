// Package elements registers the production standard-library element
// descriptors and in-process factories. Provider and application plugins add
// their own entries without modifying this package or the graph compiler.
package elements

import (
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/flow"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if err := flow.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := actionelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := cognitionelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := interactionelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := perceptionelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := speechelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	return stateelements.RegisterDescriptors(catalog)
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if err := flow.RegisterFactories(registry); err != nil {
		return err
	}
	if err := actionelements.RegisterFactories(registry); err != nil {
		return err
	}
	if err := cognitionelements.RegisterFactories(registry); err != nil {
		return err
	}
	if err := interactionelements.RegisterFactories(registry); err != nil {
		return err
	}
	if err := perceptionelements.RegisterFactories(registry); err != nil {
		return err
	}
	if err := speechelements.RegisterFactories(registry); err != nil {
		return err
	}
	return stateelements.RegisterFactories(registry)
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
