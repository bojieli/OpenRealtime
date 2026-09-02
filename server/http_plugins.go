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
	routes, err := realtimeRoutes(mount.Services)
	if err != nil {
		return err
	}
	return httpservice.RegisterRoutes(mount, HTTPRoutesContract(), routes)
}

func (factory *RealtimeRouteFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	if factory == nil {
		return nil, errors.New("pre-mount server realtime route plugin: nil factory")
	}
	if _, err := realtimeRoutes(candidate.Services); err != nil {
		return nil, err
	}
	return serverRouteCandidate{activate: factory.Mount}, nil
}

func realtimeRoutes(services pluginruntime.Services) ([]httpservice.Route, error) {
	if _, err := httpservice.LookupRegistry(services, HTTPRoutesContract()); err != nil {
		return nil, err
	}
	endpoint, err := lookupServerService[RealtimeEndpoint](services, RealtimeEndpointContract())
	if err != nil {
		return nil, err
	}
	handler := endpoint.RealtimeHandler()
	if nilServerInterface(handler) {
		return nil, errors.New("server realtime endpoint returned a nil handler")
	}
	return []httpservice.Route{{Pattern: "GET /v1/realtime", Handler: handler}}, nil
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
	routes, err := observabilityRoutes(mount.Services)
	if err != nil {
		return err
	}
	return httpservice.RegisterRoutes(mount, HTTPRoutesContract(), routes)
}

func (factory *ObservabilityRouteFactory) PreMount(
	_ context.Context, candidate pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	if factory == nil {
		return nil, errors.New("pre-mount server observability route plugin: nil factory")
	}
	if _, err := observabilityRoutes(candidate.Services); err != nil {
		return nil, err
	}
	return serverRouteCandidate{activate: factory.Mount}, nil
}

func observabilityRoutes(services pluginruntime.Services) ([]httpservice.Route, error) {
	if _, err := httpservice.LookupRegistry(services, HTTPRoutesContract()); err != nil {
		return nil, err
	}
	endpoints, err := lookupServerService[ObservabilityEndpoints](
		services, ObservabilityEndpointsContract(),
	)
	if err != nil {
		return nil, err
	}
	health, metrics := endpoints.HealthHandler(), endpoints.MetricsHandler()
	if nilServerInterface(health) || nilServerInterface(metrics) {
		return nil, errors.New("server observability endpoint returned a nil handler")
	}
	return []httpservice.Route{
		{Pattern: "GET /healthz", Handler: health},
		{Pattern: "GET /metrics", Handler: metrics},
	}, nil
}

type serverRouteCandidate struct {
	activate func(context.Context, pluginruntime.MountContext) error
}

func (candidate serverRouteCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.activate(ctx, mount)
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
	_ pluginruntime.Factory             = (*HTTPRouterFactory)(nil)
	_ pluginruntime.Factory             = (*RealtimeRouteFactory)(nil)
	_ pluginruntime.CandidatePreMounter = (*RealtimeRouteFactory)(nil)
	_ pluginruntime.Factory             = (*ObservabilityRouteFactory)(nil)
	_ pluginruntime.CandidatePreMounter = (*ObservabilityRouteFactory)(nil)
)
