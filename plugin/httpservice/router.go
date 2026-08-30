// Package httpservice provides realm-neutral, scoped HTTP routing mechanics
// for plugin compositions. It deliberately knows no product routes, tokens,
// or payload schemas; those remain behavior owned by server or presentation
// plugins.
package httpservice

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

// Route is one standard-library HTTP mux pattern and handler. Patterns carry
// their method (for example, "GET /client/v1/manifest") so method admission
// is owned by the registering plugin instead of a shared catch-all.
type Route struct {
	Pattern string
	Handler http.Handler
}

// Registry is the typed route extension point. Register atomically publishes
// all routes or none and returns an idempotent disposer scoped to their owner.
type Registry interface {
	Register(owner string, routes []Route) (dispose func(), err error)
}

// RegistryHandler is the complete in-memory router mechanic. Applications
// normally consume it through plugin services; the constructor is exposed for
// focused conformance and benchmarks of route publication itself.
type RegistryHandler interface {
	Registry
	http.Handler
}

type registeredRoute struct {
	owner   string
	handler http.Handler
}

type router struct {
	mu     sync.Mutex
	routes map[string]registeredRoute
	live   atomic.Value // handlerSnapshot
}

type handlerSnapshot struct{ handler http.Handler }

func newRouter() *router {
	result := &router{routes: make(map[string]registeredRoute)}
	result.live.Store(handlerSnapshot{handler: http.NotFoundHandler()})
	return result
}

// NewRouter returns an empty atomic router.
func NewRouter() RegistryHandler { return newRouter() }

func (router *router) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	router.live.Load().(handlerSnapshot).handler.ServeHTTP(writer, request)
}

func (router *router) Register(owner string, routes []Route) (func(), error) {
	if owner == "" || owner != strings.TrimSpace(owner) {
		return nil, errors.New("HTTP route registration requires a canonical owner")
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("HTTP route owner %s registered no routes", owner)
	}
	candidate := make(map[string]registeredRoute)
	router.mu.Lock()
	defer router.mu.Unlock()
	for pattern, route := range router.routes {
		candidate[pattern] = route
	}
	owned := make([]string, 0, len(routes))
	for _, route := range routes {
		if route.Pattern == "" || route.Pattern != strings.TrimSpace(route.Pattern) || nilHandler(route.Handler) {
			return nil, fmt.Errorf("HTTP route owner %s supplied an invalid route", owner)
		}
		if current, duplicate := candidate[route.Pattern]; duplicate {
			return nil, fmt.Errorf("HTTP route %q is already owned by %s", route.Pattern, current.owner)
		}
		candidate[route.Pattern] = registeredRoute{owner: owner, handler: route.Handler}
		owned = append(owned, route.Pattern)
	}
	handler, err := buildMux(candidate)
	if err != nil {
		return nil, fmt.Errorf("register HTTP routes for %s: %w", owner, err)
	}
	router.routes = candidate
	router.live.Store(handlerSnapshot{handler: handler})
	slices.Sort(owned)
	var once sync.Once
	return func() {
		once.Do(func() {
			router.mu.Lock()
			defer router.mu.Unlock()
			candidate := make(map[string]registeredRoute, len(router.routes))
			for pattern, route := range router.routes {
				candidate[pattern] = route
			}
			for _, pattern := range owned {
				if route, found := candidate[pattern]; found && route.owner == owner {
					delete(candidate, pattern)
				}
			}
			handler, err := buildMux(candidate)
			if err != nil {
				panic("previously valid plugin HTTP routes became invalid: " + err.Error())
			}
			router.routes = candidate
			router.live.Store(handlerSnapshot{handler: handler})
		})
	}, nil
}

func nilHandler(handler http.Handler) bool {
	if handler == nil {
		return true
	}
	reflected := reflect.ValueOf(handler)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func buildMux(routes map[string]registeredRoute) (handler http.Handler, err error) {
	defer func() {
		if failure := recover(); failure != nil {
			err = fmt.Errorf("invalid or conflicting HTTP mux patterns: %v", failure)
		}
	}()
	mux := http.NewServeMux()
	patterns := make([]string, 0, len(routes))
	for pattern := range routes {
		patterns = append(patterns, pattern)
	}
	slices.Sort(patterns)
	for _, pattern := range patterns {
		mux.Handle(pattern, routes[pattern].handler)
	}
	return mux, nil
}

// RouterFactory provides a route registry and handler under caller-supplied
// exact contracts. The descriptor determines the realm and immutable identity.
type RouterFactory struct {
	descriptor      plugin.Descriptor
	routesContract  plugin.Contract
	handlerContract plugin.Contract
}

// NewRouterFactory validates a complete router descriptor. It must provide
// exactly the two supplied contracts and must not require another service.
func NewRouterFactory(
	descriptor plugin.Descriptor, routesContract, handlerContract plugin.Contract,
) (*RouterFactory, error) {
	canonical, err := descriptor.Canonical()
	if err != nil {
		return nil, fmt.Errorf("HTTP router descriptor: %w", err)
	}
	if len(canonical.Provides) != 2 || !containsContract(canonical.Provides, routesContract) ||
		!containsContract(canonical.Provides, handlerContract) {
		return nil, errors.New("HTTP router descriptor must provide exactly its route and handler contracts")
	}
	if len(canonical.Requires) != 0 {
		return nil, errors.New("HTTP router descriptor cannot require another service")
	}
	return &RouterFactory{
		descriptor: canonical, routesContract: routesContract, handlerContract: handlerContract,
	}, nil
}

func containsContract(contracts []plugin.Contract, want plugin.Contract) bool {
	for _, contract := range contracts {
		if contract == want {
			return true
		}
	}
	return false
}

func (factory *RouterFactory) Descriptor() plugin.Descriptor { return factory.descriptor.Clone() }

func (factory *RouterFactory) Mount(_ context.Context, mount pluginruntime.MountContext) error {
	router := newRouter()
	if err := mount.Publisher.Provide(factory.routesContract, Registry(router)); err != nil {
		return err
	}
	return mount.Publisher.Provide(factory.handlerContract, http.Handler(router))
}

// LookupRegistry resolves one exact route-registry dependency.
func LookupRegistry(
	services pluginruntime.Services, contract plugin.Contract,
) (Registry, error) {
	value, actual, _, _, found := services.Lookup(contract.Name)
	if !found || actual != contract {
		return nil, fmt.Errorf("HTTP route registry %s is unavailable", contract.Name)
	}
	routes, ok := value.(Registry)
	if !ok || routes == nil {
		return nil, fmt.Errorf("HTTP route registry %s has the wrong Go type", contract.Name)
	}
	return routes, nil
}

// RegisterRoutes registers routes and attaches their disposer to the plugin's
// lifecycle before returning.
func RegisterRoutes(
	mount pluginruntime.MountContext, contract plugin.Contract, routes []Route,
) error {
	registry, err := LookupRegistry(mount.Services, contract)
	if err != nil {
		return err
	}
	dispose, err := registry.Register(mount.EntryID, routes)
	if err != nil {
		return err
	}
	if err := mount.Lifecycle.Defer("http-routes", func(context.Context) error {
		dispose()
		return nil
	}); err != nil {
		dispose()
		return err
	}
	return nil
}

// Handler returns an exact exported handler boundary from a mounted realm.
func Handler(
	mounted *pluginruntime.Mounted, export string, want plugin.Contract,
) (http.Handler, error) {
	if mounted == nil {
		return nil, errors.New("plugin realm is not mounted")
	}
	value, contract, _, _, err := mounted.Export(export)
	if err != nil {
		return nil, err
	}
	if contract != want {
		return nil, fmt.Errorf("plugin export %q has contract %s, want %s",
			export, contract.Name, want.Name)
	}
	handler, ok := value.(http.Handler)
	if !ok || handler == nil {
		return nil, fmt.Errorf("plugin export %q is not an HTTP handler", export)
	}
	return handler, nil
}
