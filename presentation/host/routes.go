// Package host implements presentation-host plugins. It contains no realtime
// server internals: relays and clients use only public wire endpoints.
package host

import (
	"context"
	"errors"
	"net/http"

	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/plugin/httpservice"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/presentation"
)

// Route is one standard-library HTTP mux pattern and handler. Patterns carry
// their method (for example, "GET /client/v1/manifest") so method admission is
// owned by the provider rather than a shared catch-all.
type Route = httpservice.Route

// RouteRegistry is the typed extension point consumed by host route plugins.
// Registration is atomic and returns an idempotent disposer.
type RouteRegistry = httpservice.Registry

// newRouter is retained as a package-local compatibility seam for the route
// registry's focused tests and benchmarks while the implementation lives in
// realm-neutral plugin mechanics.
func newRouter() httpservice.RegistryHandler { return httpservice.NewRouter() }

// RouterFactory supplies the shared host route registry and exported handler.
// It does not listen on a socket; listener placement is a separate plugin or
// application deployment concern.
type RouterFactory struct {
	delegate *httpservice.RouterFactory
}

func NewRouterFactory() *RouterFactory {
	descriptor := plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.presentation.host.router", Revision: 1,
		Realm: plugin.PresentationHostRealm, Platforms: []string{"go"},
		Provides: []plugin.Contract{presentation.HTTPRoutesContract, presentation.HTTPHandlerContract},
	}
	delegate, err := httpservice.NewRouterFactory(
		descriptor, presentation.HTTPRoutesContract, presentation.HTTPHandlerContract,
	)
	if err != nil {
		panic(err)
	}
	return &RouterFactory{delegate: delegate}
}

func (factory *RouterFactory) Descriptor() plugin.Descriptor { return factory.delegate.Descriptor() }

func (factory *RouterFactory) Mount(ctx context.Context, mount pluginruntime.MountContext) error {
	return factory.delegate.Mount(ctx, mount)
}

func (factory *RouterFactory) PreMount(
	_ context.Context, _ pluginruntime.CandidateContext,
) (pluginruntime.CandidateMount, error) {
	if factory == nil || factory.delegate == nil {
		return nil, errors.New("presentation host router factory is nil")
	}
	return routerCandidate{factory: factory}, nil
}

type routerCandidate struct{ factory *RouterFactory }

func (candidate routerCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

func lookupRoutes(services pluginruntime.Services) (RouteRegistry, error) {
	return httpservice.LookupRegistry(services, presentation.HTTPRoutesContract)
}

func registerRoutes(mount pluginruntime.MountContext, routes []Route) error {
	return httpservice.RegisterRoutes(mount, presentation.HTTPRoutesContract, routes)
}

// HTTPHandler returns the exact handler deliberately exported by a mounted
// presentation-host profile.
func HTTPHandler(mounted *pluginruntime.Mounted, export string) (http.Handler, error) {
	return httpservice.Handler(mounted, export, presentation.HTTPHandlerContract)
}

var _ pluginruntime.Factory = (*RouterFactory)(nil)
var _ pluginruntime.CandidatePreMounter = (*RouterFactory)(nil)
