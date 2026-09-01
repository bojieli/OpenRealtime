package server

import (
	"context"
	"fmt"
	"sort"

	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

type BundleConfig struct {
	Authorizer        management.Authorizer
	StaticCatalog     management.StaticCatalog
	Sessions          management.SessionInspection
	Authoring         management.Authoring
	SourceReading     management.SourceReading
	SourcePublication management.SourcePublication
	Reconciliation    management.Reconciliation
}

// Bundle is one compiled server-realm management profile. Optional API
// families are absent—not stubbed—when their provider is not selected.
type Bundle struct {
	Profile plugin.Profile
	Lock    plugin.Lock
	Plan    plugin.Plan

	Factories map[string]pluginruntime.Factory
}

func NewBundle(config BundleConfig) (*Bundle, error) {
	if nilInterface(config.Authorizer) {
		return nil, fmt.Errorf("management server bundle requires an authorizer")
	}
	router := NewRouterFactory()
	authorizer, err := NewAuthorizerProvider(config.Authorizer)
	if err != nil {
		return nil, err
	}
	factories := map[string]pluginruntime.Factory{
		"router": router, "authorizer": authorizer,
	}
	if !nilInterface(config.StaticCatalog) {
		provider, err := NewStaticCatalogProvider(config.StaticCatalog)
		if err != nil {
			return nil, err
		}
		factories["static-source"] = provider
		factories["static-api"] = NewStaticAPIFactory()
	}
	if !nilInterface(config.Sessions) {
		provider, err := NewSessionInspectionProvider(config.Sessions)
		if err != nil {
			return nil, err
		}
		factories["session-source"] = provider
		factories["session-api"] = NewSessionAPIFactory()
	}
	if !nilInterface(config.Authoring) {
		provider, err := NewAuthoringProvider(config.Authoring)
		if err != nil {
			return nil, err
		}
		factories["authoring-source"] = provider
		factories["authoring-api"] = NewAuthoringAPIFactory()
	}
	if !nilInterface(config.SourceReading) {
		provider, err := NewSourceReadingProvider(config.SourceReading)
		if err != nil {
			return nil, err
		}
		factories["source-reading-source"] = provider
		factories["source-reading-api"] = NewSourceReadingAPIFactory()
	}
	if !nilInterface(config.SourcePublication) {
		provider, err := NewSourcePublicationProvider(config.SourcePublication)
		if err != nil {
			return nil, err
		}
		factories["source-publication-source"] = provider
		factories["source-publication-api"] = NewSourcePublicationAPIFactory()
	}
	if !nilInterface(config.Reconciliation) {
		provider, err := NewReconciliationProvider(config.Reconciliation)
		if err != nil {
			return nil, err
		}
		factories["reconciliation-source"] = provider
		factories["reconciliation-api"] = NewReconciliationAPIFactory()
	}

	ids := make([]string, 0, len(factories))
	for id := range factories {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Mechanics/providers precede their endpoints in the authored profile for
	// readability; the compiler remains the authority on dependency order.
	order := []string{
		"router", "authorizer", "static-source", "session-source", "authoring-source",
		"source-reading-source", "source-publication-source", "reconciliation-source",
		"static-api", "session-api", "authoring-api", "source-reading-api",
		"source-publication-api", "reconciliation-api",
	}
	entries := make([]plugin.ProfileEntry, 0, len(factories))
	catalog := plugin.NewCatalog()
	seen := make(map[string]struct{}, len(factories))
	for _, id := range order {
		factory, selected := factories[id]
		if !selected {
			continue
		}
		descriptor := factory.Descriptor()
		if _, err := catalog.Register(descriptor); err != nil {
			return nil, fmt.Errorf("register management plugin %s: %w", id, err)
		}
		entries = append(entries, plugin.ProfileEntry{ID: id, Plugin: descriptor.Name, Scope: "root"})
		seen[id] = struct{}{}
	}
	for _, id := range ids {
		if _, present := seen[id]; !present {
			return nil, fmt.Errorf("management bundle has no deterministic order for plugin %s", id)
		}
	}
	profile, err := plugin.FreezeProfile(plugin.Profile{
		FormatVersion: plugin.ProfileFormatVersion,
		Name:          "openrealtime.management.server", Revision: 1, Realm: plugin.ServerRealm,
		Scopes: []plugin.ProfileScope{{Path: "root"}}, Entries: entries,
		Exports: []plugin.ProfileExport{{
			Name: "http", Provider: "router", Service: management.HTTPHandlerContract.Name,
		}},
	})
	if err != nil {
		return nil, err
	}
	lock, err := plugin.ResolveProfile(profile, catalog)
	if err != nil {
		return nil, err
	}
	plan, err := plugin.Compile(profile, lock, catalog)
	if err != nil {
		return nil, err
	}
	return &Bundle{Profile: profile, Lock: lock, Plan: plan, Factories: factories}, nil
}

// Mount registers every exact bundled implementation and mounts the compiled
// profile. The returned realm owns all route registrations.
func (bundle *Bundle) Mount(ctx context.Context) (*pluginruntime.Mounted, error) {
	if bundle == nil {
		return nil, fmt.Errorf("mount management server bundle: nil bundle")
	}
	registry := pluginruntime.NewRegistry()
	for id, factory := range bundle.Factories {
		if err := registry.Register(factory.Descriptor().Name, factory); err != nil {
			return nil, fmt.Errorf("register management implementation %s: %w", id, err)
		}
	}
	return pluginruntime.Mount(ctx, pluginruntime.Config{Plan: bundle.Plan, Registry: registry})
}
