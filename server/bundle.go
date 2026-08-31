package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	managementserver "github.com/bojieli/OpenRealtime/management/server"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

// BundleConfig is the complete, immutable input to one server composition.
// ProviderArtifact identifies the session provider. GatewayArtifact identifies
// the linked server implementation; each selected router, inspection,
// gateway, and route descriptor records that artifact independently in live
// evidence even when they share executable bytes.
type BundleConfig struct {
	ProfileName     string
	ProfileRevision uint64

	Provider SessionProvider
	Gateway  gateway.Config

	ProviderArtifact inspect.ArtifactIdentity
	// GatewayArtifact identifies the linked server implementation containing
	// the gateway, stable router, inspection authority, and route plugins. Each
	// selected descriptor records it independently in live realm evidence.
	GatewayArtifact inspect.ArtifactIdentity
}

// Bundle is a descriptor-locked server profile and its exact executable
// registrations. Constructing a Bundle compiles the entire dependency graph
// but acquires no provider resources and opens no listener.
type Bundle struct {
	Profile plugin.Profile
	Lock    plugin.Lock
	Plan    plugin.Plan

	registrations []bundleRegistration
	evidence      *realmEvidence
	mountMu       sync.Mutex
	mounted       bool
}

type bundleRegistration struct {
	id       string
	factory  pluginruntime.Factory
	artifact inspect.ArtifactIdentity
}

type realmEvidence struct {
	mounted atomic.Pointer[pluginruntime.Mounted]
}

func (evidence *realmEvidence) Live() pluginruntime.Live {
	if evidence == nil {
		return pluginruntime.Live{}
	}
	mounted := evidence.mounted.Load()
	if mounted == nil {
		return pluginruntime.Live{}
	}
	return mounted.Live()
}

// NewBundle compiles the canonical clean Realtime server composition. The
// profile name and revision are deployment identity, not display metadata, and
// are therefore required rather than synthesized from mutable process state.
func NewBundle(config BundleConfig) (*Bundle, error) {
	config.ProfileName = strings.TrimSpace(config.ProfileName)
	if config.ProfileName == "" {
		return nil, errors.New("server bundle requires a profile name")
	}
	if config.ProfileRevision == 0 {
		return nil, errors.New("server bundle requires a positive profile revision")
	}
	if err := config.ProviderArtifact.Validate(); err != nil {
		return nil, fmt.Errorf("server provider runtime artifact: %w", err)
	}
	if err := config.GatewayArtifact.Validate(); err != nil {
		return nil, fmt.Errorf("server gateway runtime artifact: %w", err)
	}
	evidence := &realmEvidence{}
	// The callback closes over this bundle's private evidence cell. Mount
	// publishes the realm before returning; the application cannot obtain a
	// handler, much less start a listener, while the callback is still empty.
	config.Gateway.ServerProfile = evidence.Live
	inspectionTTL := config.Gateway.InspectionTokenTTL
	config.Gateway.InspectionTokenTTL = 0
	providerFactory, err := NewSessionProviderFactory(config.Provider)
	if err != nil {
		return nil, err
	}
	gatewayFactory, err := NewGatewayFactory(GatewayFactoryConfig{Gateway: config.Gateway})
	if err != nil {
		return nil, err
	}
	inspectionFactory, err := NewSessionInspectionPlaneFactory(inspectionTTL)
	if err != nil {
		return nil, err
	}
	routerFactory := NewHTTPRouterFactory()
	realtimeRouteFactory := NewRealtimeRouteFactory()
	observabilityFactory := NewObservabilityRouteFactory()
	sessionAPIFactory := managementserver.NewSessionAPIFactory()

	registrations := []bundleRegistration{
		{id: "http-router", factory: routerFactory, artifact: config.GatewayArtifact},
		{id: "sessions", factory: providerFactory, artifact: config.ProviderArtifact},
		{id: "inspection", factory: inspectionFactory, artifact: config.GatewayArtifact},
		{id: "gateway", factory: gatewayFactory, artifact: config.GatewayArtifact},
		{id: "realtime", factory: realtimeRouteFactory, artifact: config.GatewayArtifact},
		{id: "observability", factory: observabilityFactory, artifact: config.GatewayArtifact},
		{id: "session-api", factory: sessionAPIFactory, artifact: config.GatewayArtifact},
	}

	catalog := plugin.NewCatalog()
	for _, registration := range registrations {
		if _, err := catalog.Register(registration.factory.Descriptor()); err != nil {
			return nil, fmt.Errorf(
				"register server plugin %s: %w", registration.factory.Descriptor().Name, err,
			)
		}
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          config.ProfileName,
		Revision:      config.ProfileRevision,
		Realm:         plugin.ServerRealm,
		Scopes:        []plugin.ProfileScope{{Path: "root"}},
		Entries: []plugin.ProfileEntry{
			{ID: "http-router", Plugin: routerFactory.Descriptor().Name, Scope: "root"},
			{ID: "sessions", Plugin: providerFactory.Descriptor().Name, Scope: "root"},
			{ID: "inspection", Plugin: inspectionFactory.Descriptor().Name, Scope: "root"},
			{ID: "gateway", Plugin: gatewayFactory.Descriptor().Name, Scope: "root"},
			{ID: "realtime", Plugin: realtimeRouteFactory.Descriptor().Name, Scope: "root"},
			{ID: "observability", Plugin: observabilityFactory.Descriptor().Name, Scope: "root"},
			{ID: "session-api", Plugin: sessionAPIFactory.Descriptor().Name, Scope: "root"},
		},
		Exports: []plugin.ProfileExport{{
			Name: RealtimeHTTPExport, Provider: "http-router", Service: RealtimeHTTPContract().Name,
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("freeze server profile: %w", err)
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		return nil, fmt.Errorf("resolve server profile: %w", err)
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		return nil, fmt.Errorf("compile server profile: %w", err)
	}
	return &Bundle{
		Profile: profile, Lock: lock, Plan: plan,
		registrations: registrations, evidence: evidence,
	}, nil
}

// Realm is one mounted clean-API server profile. A process listener consumes
// Handler; operators consume Live; Close releases the profile in reverse
// dependency order. No presentation or socket implementation is hidden here.
type Realm struct {
	mounted *pluginruntime.Mounted
	service RealtimeHTTP
}

// Mount validates all registered runtime identities, mounts the complete
// profile, and resolves its declared HTTP export. A caller can safely start a
// listener only after this method succeeds.
func (bundle *Bundle) Mount(ctx context.Context) (*Realm, error) {
	if bundle == nil {
		return nil, errors.New("mount server bundle: nil bundle")
	}
	if ctx == nil {
		return nil, errors.New("mount server bundle: nil context")
	}
	bundle.mountMu.Lock()
	defer bundle.mountMu.Unlock()
	if bundle.mounted {
		return nil, errors.New("mount server bundle: bundle has already mounted a realm")
	}
	registry := pluginruntime.NewRegistry()
	for _, registration := range bundle.registrations {
		if err := registry.RegisterArtifact(
			registration.factory.Descriptor().Name, registration.artifact, registration.factory,
		); err != nil {
			return nil, fmt.Errorf("register server plugin %s: %w", registration.id, err)
		}
	}
	mounted, err := pluginruntime.Mount(ctx, pluginruntime.Config{
		Plan: bundle.Plan, Registry: registry,
	})
	if err != nil {
		return nil, fmt.Errorf("mount server profile: %w", err)
	}
	value, contract, provider, _, err := mounted.Export(RealtimeHTTPExport)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("resolve server HTTP export: %w", err),
			mounted.Close(context.Background()),
		)
	}
	service, ok := value.(RealtimeHTTP)
	if !ok || nilServerInterface(service) || contract != RealtimeHTTPContract() || provider != "http-router" {
		return nil, errors.Join(
			fmt.Errorf("server HTTP export has value %T, contract %s, provider %q",
				value, contract.Name, provider),
			mounted.Close(context.Background()),
		)
	}
	bundle.evidence.mounted.Store(mounted)
	bundle.mounted = true
	return &Realm{mounted: mounted, service: service}, nil
}

func (realm *Realm) Handler() http.Handler {
	if realm == nil || nilServerInterface(realm.service) {
		return http.NotFoundHandler()
	}
	return realm.service.Handler()
}

func (realm *Realm) Live() pluginruntime.Live {
	if realm == nil || realm.mounted == nil {
		return pluginruntime.Live{}
	}
	return realm.mounted.Live()
}

func (realm *Realm) Close(ctx context.Context) error {
	if realm == nil || realm.mounted == nil {
		return nil
	}
	return realm.mounted.Close(ctx)
}
