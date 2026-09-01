package server

import (
	"context"
	"errors"
	"reflect"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type providerFactory struct {
	descriptor plugin.Descriptor
	contract   plugin.Contract
	value      any
}

func newProviderFactory(name string, contract plugin.Contract, value any) (*providerFactory, error) {
	if nilInterface(value) {
		return nil, errors.New("management service provider requires a non-nil value")
	}
	return &providerFactory{
		descriptor: plugin.Descriptor{
			FormatVersion: plugin.DescriptorFormatVersion,
			Name:          name, Revision: 1, Realm: plugin.ServerRealm, Platforms: []string{"go"},
			Provides: []plugin.Contract{contract},
		},
		contract: contract, value: value,
	}, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (factory *providerFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *providerFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	return mount.Publisher.Provide(factory.contract, factory.value)
}

func NewAuthorizerProvider(authorizer management.Authorizer) (pluginruntime.Factory, error) {
	return newProviderFactory(
		"openrealtime.management.server.authorizer-provider", management.AuthorizerContract, authorizer,
	)
}

func NewStaticCatalogProvider(catalog management.StaticCatalog) (pluginruntime.Factory, error) {
	return newProviderFactory(
		"openrealtime.management.server.static-catalog-provider", management.StaticCatalogContract, catalog,
	)
}

func NewSessionInspectionProvider(source management.SessionInspection) (pluginruntime.Factory, error) {
	return newProviderFactory(
		"openrealtime.management.server.session-inspection-provider",
		management.SessionInspectionContract, source,
	)
}

func NewAuthoringProvider(authoring management.Authoring) (pluginruntime.Factory, error) {
	return newProviderFactory(
		"openrealtime.management.server.authoring-provider", management.AuthoringContract, authoring,
	)
}

func NewSourceReadingProvider(reading management.SourceReading) (pluginruntime.Factory, error) {
	return newProviderFactory(
		"openrealtime.management.server.source-reading-provider",
		management.SourceReadingContract, reading,
	)
}

func NewSourcePublicationProvider(
	publication management.SourcePublication,
) (pluginruntime.Factory, error) {
	return newProviderFactory(
		"openrealtime.management.server.source-publication-provider",
		management.SourcePublicationContract, publication,
	)
}

func NewReconciliationProvider(reconciliation management.Reconciliation) (pluginruntime.Factory, error) {
	return newProviderFactory(
		"openrealtime.management.server.reconciliation-provider",
		management.ReconciliationContract, reconciliation,
	)
}
