package server

import (
	"context"
	"errors"
	"fmt"

	graphcatalog "github.com/bojieli/OpenRealtime/graph/catalog"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
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
	return &GraphBundle{
		GraphPlan: launched.Plan, Evidence: launched.Evidence, GraphCatalog: catalog,
		ServerBundle: serverBundle,
		Readiness:    launched.Readiness,
	}, nil
}
