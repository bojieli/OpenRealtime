package server

import (
	"context"
	"errors"
	"fmt"

	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	"github.com/bojieli/OpenRealtime/management"
	managementserver "github.com/bojieli/OpenRealtime/management/server"
	"github.com/bojieli/OpenRealtime/plugin"
)

// GraphBundleConfig composes one exact graph-native session provider with the
// descriptor-locked Realtime server realm. Graph owns topology, values,
// deployment, adapter, and executable-element selection. Server owns only the
// stable protocol, management, observability, and listener-independent route
// profile.
//
// Server.Provider must be empty. Accepting both a graph launch and an injected
// provider would create two competing sources of session authority.
type GraphBundleConfig struct {
	Graph  graphlaunch.Config
	Server BundleConfig
}

// GraphBundle is the immutable boundary between graph preparation and server
// composition. Constructing it acquires no graph resource and opens no
// listener. The graph plan, its single-entry immutable discovery catalog, and
// the server bundle retain independent fingerprints so inspection can attest
// each layer without collapsing their identities.
type GraphBundle struct {
	GraphPlan    *graphconfig.Plan
	Evidence     graphevidence.Document
	GraphCatalog graphcatalog.Document
	ServerBundle *Bundle
	Readiness    []graphlaunch.ReadinessCheck

	operatorCatalog   *management.Catalog
	operatorAuthoring *management.AuthoringEngine
}

// NewGraphBundle prepares the exact graph-native provider and compiles the
// server plugin realm around it. There is no legacy binding fallback: any
// graph, adapter, dependency, or server-profile mismatch fails before Mount.
func NewGraphBundle(ctx context.Context, config GraphBundleConfig) (*GraphBundle, error) {
	if ctx == nil {
		return nil, errors.New("compose graph server bundle: nil context")
	}
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if !nilServerInterface(config.Server.Provider) {
		return nil, errors.New(
			"compose graph server bundle: server provider must be supplied only by graph launch",
		)
	}
	launched, err := graphlaunch.New(ctx, config.Graph)
	if err != nil {
		return nil, fmt.Errorf("compose graph server bundle: %w", err)
	}
	catalog, err := graphcatalog.Freeze([]graphcatalog.Entry{launched.CatalogEntry})
	if err != nil {
		return nil, fmt.Errorf("compose graph server bundle catalog: %w", err)
	}
	config.Server.Provider = launched.Binding
	serverBundle, err := NewBundle(config.Server)
	if err != nil {
		return nil, fmt.Errorf("compose graph server bundle: %w", err)
	}
	result := &GraphBundle{
		GraphPlan: launched.Plan, Evidence: launched.Evidence, GraphCatalog: catalog,
		ServerBundle: serverBundle,
		Readiness:    launched.Readiness,
	}
	if err := result.prepareOperatorServices(launched); err != nil {
		return nil, fmt.Errorf("compose graph server operator services: %w", err)
	}
	return result, nil
}

func (bundle *GraphBundle) prepareOperatorServices(launched graphlaunch.Result) error {
	if bundle == nil || bundle.GraphPlan == nil || bundle.ServerBundle == nil {
		return errors.New("incomplete graph bundle")
	}
	elements := resolve.NewCatalog()
	for _, descriptor := range launched.ElementDescriptors() {
		if err := elements.Register(descriptor); err != nil {
			return fmt.Errorf("register element descriptor: %w", err)
		}
	}
	plugins := plugin.NewCatalog()
	for _, registration := range bundle.ServerBundle.registrations {
		if _, err := plugins.Register(registration.factory.Descriptor()); err != nil {
			return fmt.Errorf("register server plugin descriptor: %w", err)
		}
	}
	catalog, err := management.NewCatalog(elements, plugins)
	if err != nil {
		return err
	}
	if err := catalog.RegisterGraph(bundle.GraphPlan.Graph(), bundle.GraphPlan.ValuesSchema()); err != nil {
		return err
	}
	authoring, err := management.NewAuthoringEngine(management.AuthoringOptions{Catalog: elements})
	if err != nil {
		return err
	}
	bundle.operatorCatalog = catalog
	bundle.operatorAuthoring = authoring
	return nil
}

// OperatorGrants returns the least-privilege process-lifetime authority for
// this exact graph's immutable catalog and the payload-only authoring API. It
// intentionally excludes session inspection, source I/O, and reconciliation.
func (bundle *GraphBundle) OperatorGrants() ([]management.Grant, error) {
	if bundle == nil || bundle.GraphPlan == nil || bundle.operatorCatalog == nil ||
		bundle.operatorAuthoring == nil {
		return nil, errors.New("derive graph server operator grants: incomplete graph bundle")
	}
	fingerprint := bundle.GraphPlan.Graph().Fingerprint
	return []management.Grant{
		{Operation: management.ReadGraph, Resource: fingerprint},
		{Operation: management.ReadDescriptor, Resource: "*"},
		{Operation: management.ReadSchema, Resource: fingerprint},
		{Operation: management.AnalyzeDocument, Resource: "authoring"},
		{Operation: management.RenameDocument, Resource: "authoring"},
		{Operation: management.RemoveDocumentEdge, Resource: "authoring"},
		{Operation: management.CreateDocumentEdge, Resource: "authoring"},
		{Operation: management.CompileDocument, Resource: "authoring"},
		{Operation: management.RenderGraph, Resource: "authoring"},
	}, nil
}

// OperatorAPIConfig binds the graph bundle's exact static and in-memory
// authoring services to a caller-owned authority. Mounting remains explicit so
// a headless profile that omits operator authority exposes no operator routes.
func (bundle *GraphBundle) OperatorAPIConfig(
	authorizer management.Authorizer,
) (managementserver.OperatorAPIConfig, error) {
	if bundle == nil || bundle.operatorCatalog == nil || bundle.operatorAuthoring == nil {
		return managementserver.OperatorAPIConfig{},
			errors.New("configure graph server operator API: incomplete graph bundle")
	}
	if authorizer == nil {
		return managementserver.OperatorAPIConfig{},
			errors.New("configure graph server operator API: nil authorizer")
	}
	return managementserver.OperatorAPIConfig{
		Authorizer: authorizer, StaticCatalog: bundle.operatorCatalog,
		Authoring: bundle.operatorAuthoring,
	}, nil
}
