// Package server implements descriptor-locked server-realm plugins for the
// OpenRealtime management API. It contains no view code and does not depend on
// the realtime gateway.
package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	"github.com/bojieli/OpenRealtime/plugin/httpservice"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type Route = httpservice.Route

type RouterFactory struct{ delegate *httpservice.RouterFactory }

func NewRouterFactory() *RouterFactory {
	descriptor := plugin.Descriptor{
		FormatVersion: plugin.DescriptorFormatVersion,
		Name:          "openrealtime.management.server.router", Revision: 1,
		Realm: plugin.ServerRealm, Platforms: []string{"go"},
		Provides: []plugin.Contract{management.HTTPRoutesContract, management.HTTPHandlerContract},
	}
	delegate, err := httpservice.NewRouterFactory(
		descriptor, management.HTTPRoutesContract, management.HTTPHandlerContract,
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
		return nil, errors.New("management server router factory is nil")
	}
	return routerCandidate{factory: factory}, nil
}

type routerCandidate struct{ factory *RouterFactory }

func (candidate routerCandidate) Activate(
	ctx context.Context, mount pluginruntime.MountContext,
) error {
	return candidate.factory.Mount(ctx, mount)
}

func registerRoutes(mount pluginruntime.MountContext, routes []Route) error {
	owned := make([]Route, len(routes))
	for index, route := range routes {
		owned[index] = route
		owned[index].Handler = lifecycleHTTPHandler(
			mount.Lifecycle, mount.EntryID+"-request", route.Handler,
		)
	}
	return httpservice.RegisterRoutes(mount, management.HTTPRoutesContract, owned)
}

func lifecycleHTTPHandler(
	lifecycle pluginruntime.Lifecycle, workerPrefix string, handler http.Handler,
) http.Handler {
	var nextWorker atomic.Uint64
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		done := make(chan struct{})
		workerName := fmt.Sprintf("%s-%d", workerPrefix, nextWorker.Add(1))
		if err := lifecycle.Go(workerName, func(lifecycleContext context.Context) error {
			defer close(done)
			requestContext, cancel := context.WithCancel(lifecycleContext)
			stopRequest := context.AfterFunc(request.Context(), cancel)
			stopBody := func() bool { return false }
			if request.Body != nil {
				stopBody = context.AfterFunc(requestContext, func() { _ = request.Body.Close() })
			}
			defer func() {
				stopBody()
				stopRequest()
				cancel()
			}()
			handler.ServeHTTP(writer, request.WithContext(requestContext))
			return nil
		}); err != nil {
			http.Error(writer, "the management route lifecycle is unavailable", http.StatusServiceUnavailable)
			return
		}
		<-done
	})
}

func HTTPHandler(mounted *pluginruntime.Mounted, export string) (http.Handler, error) {
	return httpservice.Handler(mounted, export, management.HTTPHandlerContract)
}

var _ pluginruntime.CandidatePreMounter = (*RouterFactory)(nil)
