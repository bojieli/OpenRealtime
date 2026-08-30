// Package server supplies descriptor-locked server-realm plugins around the
// clean Realtime API. The application launcher starts a listener only after a
// compiled plugin profile exports an HTTP service; neither the graph runtime
// nor presentation clients own a built-in UI or privileged route.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/plugin"
)

const (
	// RealtimeHTTPExport is the conventional profile export consumed by an
	// application listener. Profiles may rename it explicitly.
	RealtimeHTTPExport = "realtime_http"
)

var (
	sessionProviderContract = serverContract(
		"openrealtime.server.session_provider", 1,
		"Start(ctx, session options) -> session runtime; immutable profile projection v1",
	)
	httpRoutesContract = serverContract(
		"openrealtime.server.http_routes", 1,
		"Register(owner, method-qualified routes) -> scoped disposer; atomic conflict rejection v1",
	)
	realtimeHTTPContract = serverContract(
		"openrealtime.server.realtime_http", 2,
		"stable composed HTTP handler exported by a descriptor-locked server route profile v2",
	)
	sessionInspectionPlaneContract = serverContract(
		"openrealtime.server.session_inspection_plane", 1,
		"session registration plus short-lived scoped inspection capability issuance v1",
	)
	realtimeEndpointContract = serverContract(
		"openrealtime.server.realtime_endpoint", 1,
		"payload-only OpenAI-compatible realtime WebSocket HTTP handler v1",
	)
	observabilityEndpointsContract = serverContract(
		"openrealtime.server.observability_endpoints", 1,
		"payload-free health and bounded telemetry HTTP handlers v1",
	)
	inspectionCompatibilityEndpointContract = serverContract(
		"openrealtime.server.inspection_compatibility_endpoint", 1,
		"deployment-authenticated historical session-live HTTP adapter v1",
	)
)

// SessionProvider is the exported server-plugin boundary that creates one
// independent realtime session. It deliberately does not expose a concrete
// graph, legacy preset, model client, or UI implementation. The current
// binding vocabulary remains the protocol compatibility projection while
// graph-native providers migrate behind this interface.
type SessionProvider interface {
	Name() string
	Ownership() binding.Ownership
	Capabilities() binding.Capabilities
	Start(context.Context, binding.Options) (binding.Runtime, error)
}

// RealtimeHTTP is the only service an application listener needs. The plugin
// realm owns the gateway lifecycle; closing the realm revokes this handler's
// management resources before the listener itself is shut down.
type RealtimeHTTP interface {
	Handler() http.Handler
}

// RealtimeEndpoint is the handler implementation consumed by the independently
// selected realtime route plugin. It deliberately carries no route pattern.
type RealtimeEndpoint interface {
	RealtimeHandler() http.Handler
}

// ObservabilityEndpoints supplies payload-only health and bounded telemetry
// handlers. Route ownership remains with the observability plugin.
type ObservabilityEndpoints interface {
	HealthHandler() http.Handler
	MetricsHandler() http.Handler
}

// InspectionCompatibilityEndpoint supplies only the historical session-live
// adapter. Canonical management routes are owned by management API plugins.
type InspectionCompatibilityEndpoint interface {
	InspectionHandler() http.Handler
}

func SessionProviderContract() plugin.Contract { return sessionProviderContract }
func HTTPRoutesContract() plugin.Contract      { return httpRoutesContract }
func RealtimeHTTPContract() plugin.Contract    { return realtimeHTTPContract }
func SessionInspectionPlaneContract() plugin.Contract {
	return sessionInspectionPlaneContract
}
func RealtimeEndpointContract() plugin.Contract { return realtimeEndpointContract }
func ObservabilityEndpointsContract() plugin.Contract {
	return observabilityEndpointsContract
}
func InspectionCompatibilityEndpointContract() plugin.Contract {
	return inspectionCompatibilityEndpointContract
}

func serverContract(name string, revision uint64, schema string) plugin.Contract {
	digest := sha256.Sum256([]byte("openrealtime/server-contract/v1\x00" + name + "\x00" + schema))
	return plugin.Contract{
		Name: name, Revision: revision,
		Digest: "sha256:" + hex.EncodeToString(digest[:]),
	}
}
