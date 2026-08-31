package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/plugin/httpservice"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

const (
	httpRouterPluginName         = "openrealtime.server.http-router"
	realtimeRoutePluginName      = "openrealtime.server.realtime-route"
	observabilityRoutePluginName = "openrealtime.server.observability-routes"
)

// HTTPRouterFactory owns the stable route registry exported to a process
// listener. It contains no product routes. Server and management route
// plugins publish into the same atomic registry under distinct exact service
// contracts, so an unmounted route cannot be revealed through a hidden mux.
type HTTPRouterFactory struct{ descriptor plugin.Descriptor }

func NewHTTPRouterFactory() *HTTPRouterFactory {
	return &HTTPRouterFactory{descriptor: plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          httpRouterPluginName,
		Revision:      1,
		Realm:         plugin.ServerRealm,
		Platforms:     []string{"go"},
		Provides: []plugin.Contract{
			HTTPRoutesContract(),
			RealtimeHTTPContract(),
			management.HTTPRoutesContract,
			management.HTTPHandlerContract,
		},
		Lifecycle: plugin.Lifecycle{DisposeTimeoutMS: 5_000},
	}}
}

func (factory *HTTPRouterFactory) Descriptor() plugin.Descriptor {
	if factory == nil {
		return plugin.Descriptor{}
	}
	return factory.descriptor.Clone()
}

func (factory *HTTPRouterFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	if factory == nil {
		return errors.New("mount server HTTP router plugin: nil factory")
	}
	router := httpservice.NewRouter()
	if err := mount.Publisher.Provide(HTTPRoutesContract(), httpservice.Registry(router)); err != nil {
		return err
	}
	if err := mount.Publisher.Provide(
		management.HTTPRoutesContract, httpservice.Registry(router),
	); err != nil {
		return err
	}
	if err := mount.Publisher.Provide(management.HTTPHandlerContract, http.Handler(router)); err != nil {
		return err
	}
	return mount.Publisher.Provide(
		RealtimeHTTPContract(), RealtimeHTTP(&composedHTTPService{handler: router}),
	)
}

type composedHTTPService struct{ handler http.Handler }

func (service *composedHTTPService) Handler() http.Handler {
	if service == nil || nilServerInterface(service.handler) {
		return http.NotFoundHandler()
	}
	return service.handler
}

// RealtimeRouteFactory owns only the public OpenAI-compatible WebSocket path.
type RealtimeRouteFactory struct{ descriptor plugin.Descriptor }

func NewRealtimeRouteFactory() *RealtimeRouteFactory {
	return &RealtimeRouteFactory{descriptor: routeDescriptor(
		realtimeRoutePluginName, RealtimeEndpointContract(),
	)}
}

func (factory *RealtimeRouteFactory) Descriptor() plugin.Descriptor {
	if factory == nil {
		return plugin.Descriptor{}
	}
	return factory.descriptor.Clone()
}

func (factory *RealtimeRouteFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	if factory == nil {
		return errors.New("mount server realtime route plugin: nil factory")
	}
	endpoint, err := lookupServerService[RealtimeEndpoint](mount.Services, RealtimeEndpointContract())
	if err != nil {
		return err
	}
	return httpservice.RegisterRoutes(mount, HTTPRoutesContract(), []httpservice.Route{{
		Pattern: "GET /v1/realtime", Handler: endpoint.RealtimeHandler(),
	}})
}

// ObservabilityRouteFactory owns health and bounded telemetry as one
// independently selectable, UI-free operational surface.
type ObservabilityRouteFactory struct{ descriptor plugin.Descriptor }

func NewObservabilityRouteFactory() *ObservabilityRouteFactory {
	return &ObservabilityRouteFactory{descriptor: routeDescriptor(
		observabilityRoutePluginName, ObservabilityEndpointsContract(),
	)}
}

func (factory *ObservabilityRouteFactory) Descriptor() plugin.Descriptor {
	if factory == nil {
		return plugin.Descriptor{}
	}
	return factory.descriptor.Clone()
}

func (factory *ObservabilityRouteFactory) Mount(
	_ context.Context, mount pluginruntime.MountContext,
) error {
	if factory == nil {
		return errors.New("mount server observability route plugin: nil factory")
	}
	endpoints, err := lookupServerService[ObservabilityEndpoints](
		mount.Services, ObservabilityEndpointsContract(),
	)
	if err != nil {
		return err
	}
	return httpservice.RegisterRoutes(mount, HTTPRoutesContract(), []httpservice.Route{
		{Pattern: "GET /healthz", Handler: endpoints.HealthHandler()},
		{Pattern: "GET /metrics", Handler: endpoints.MetricsHandler()},
	})
}

func routeDescriptor(name string, endpoint plugin.Contract) plugin.Descriptor {
	return plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          name,
		Revision:      1,
		Realm:         plugin.ServerRealm,
		Platforms:     []string{"go"},
		Requires: []plugin.Requirement{
			{Contract: HTTPRoutesContract()},
			{Contract: endpoint},
		},
		Lifecycle: plugin.Lifecycle{DisposeTimeoutMS: 5_000},
	}
}

func lookupServerService[T any](services pluginruntime.Services, want plugin.Contract) (T, error) {
	var zero T
	value, contract, _, _, found := services.Lookup(want.Name)
	if !found || contract != want {
		return zero, fmt.Errorf("required server service %s is unavailable", want.Name)
	}
	typed, ok := value.(T)
	if !ok || nilServerInterface(typed) {
		return zero, fmt.Errorf("server service %s has type %T", want.Name, value)
	}
	return typed, nil
}

var (
	_ pluginruntime.Factory = (*HTTPRouterFactory)(nil)
	_ pluginruntime.Factory = (*RealtimeRouteFactory)(nil)
	_ pluginruntime.Factory = (*ObservabilityRouteFactory)(nil)
)
