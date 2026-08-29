// Package elements registers the production standard-library element
// descriptors and in-process factories. Provider and application plugins add
// their own entries without modifying this package or the graph compiler.
package elements

import (
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/flow"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if err := acousticelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
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
	if err := ingresselements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := mediaelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := modelelements.RegisterDescriptor(catalog); err != nil {
		return err
	}
	if err := perceptionelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := policyelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := speechelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	if err := stateelements.RegisterDescriptors(catalog); err != nil {
		return err
	}
	return nil
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if err := acousticelements.RegisterFactories(registry); err != nil {
		return err
	}
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
	if err := ingresselements.RegisterFactories(registry); err != nil {
		return err
	}
	if err := mediaelements.RegisterFactories(registry); err != nil {
		return err
	}
	if err := modelelements.RegisterFactory(registry); err != nil {
		return err
	}
	if err := perceptionelements.RegisterFactories(registry); err != nil {
		return err
	}
	if err := policyelements.RegisterFactories(registry); err != nil {
		return err
	}
	if err := speechelements.RegisterFactories(registry); err != nil {
		return err
	}
	if err := stateelements.RegisterFactories(registry); err != nil {
		return err
	}
	return nil
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
