package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/management"
	managementserver "github.com/bojieli/OpenRealtime/management/server"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// InspectionTokenHeader is the compatibility header negotiated by the
// OpenRealtime session protocol. Canonical management clients use
// management.CapabilityHeader with the same opaque capability.
const InspectionTokenHeader = openrealtime.InspectionTokenHeader

const defaultInspectionTokenTTL = time.Hour

var errInspectionTraceUnavailable = errors.New("runtime has no enabled trace recorder")

// liveInspectionSource is deliberately optional. A legacy runtime that only
// exposes Status cannot be promoted into graph evidence by the gateway.
type liveInspectionSource interface {
	Live() inspect.Live
}

type recordedTraceSource interface {
	RecordedTrace() (inspect.LiveTrace, error)
}

// managedSessionRuntime adapts the deliberately optional recording surface to
// the complete management contract. Live inspection remains available when
// recording is disabled, while trace and delta reads fail closed.
type managedSessionRuntime struct {
	live  liveInspectionSource
	trace recordedTraceSource
}

func (runtime managedSessionRuntime) Live() inspect.Live { return runtime.live.Live() }

func (runtime managedSessionRuntime) RecordedTrace() (inspect.LiveTrace, error) {
	if runtime.trace == nil {
		return inspect.LiveTrace{}, errInspectionTraceUnavailable
	}
	return runtime.trace.RecordedTrace()
}

// SessionInspectionPlane is the payload-free session authority shared by
// realtime negotiation and the canonical management API. A composable server
// profile publishes this value together with its exact Authorizer and
// SessionInspection projections; standalone gateway construction uses the
// same value behind its compatibility handler.
type SessionInspectionPlane struct {
	capabilities *management.CapabilityRegistry
	sessions     *management.SessionRegistry
	ttl          time.Duration
}

// NewSessionInspectionPlane validates the capability lifetime and creates the
// in-memory registries without mounting routes, starting workers, or acquiring
// external resources.
func NewSessionInspectionPlane(ttl time.Duration) (*SessionInspectionPlane, error) {
	if ttl == 0 {
		ttl = defaultInspectionTokenTTL
	}
	if ttl < 0 || ttl > management.MaximumCapabilityTTL {
		return nil, fmt.Errorf("inspection token TTL must be in 0..%s", management.MaximumCapabilityTTL)
	}
	return &SessionInspectionPlane{
		capabilities: management.NewCapabilityRegistry(),
		sessions:     management.NewSessionRegistry(),
		ttl:          ttl,
	}, nil
}

// Validate rejects a zero, partially constructed, or drifted plane before a
// gateway or plugin publishes it.
func (plane *SessionInspectionPlane) Validate() error {
	if plane == nil || plane.capabilities == nil || plane.sessions == nil ||
		plane.ttl <= 0 || plane.ttl > management.MaximumCapabilityTTL {
		return errors.New("session-inspection plane is incomplete")
	}
	return nil
}

// Authorizer returns the exact narrow-capability projection consumed by a
// management session-API plugin.
func (plane *SessionInspectionPlane) Authorizer() management.Authorizer {
	if plane == nil {
		return nil
	}
	return plane.capabilities
}

// Sessions returns the exact payload-free inspection projection consumed by
// a management session-API plugin.
func (plane *SessionInspectionPlane) Sessions() management.SessionInspection {
	if plane == nil {
		return nil
	}
	return plane.sessions
}

// gatewayManagement owns the canonical handler selected for a gateway and,
// only for the compatibility constructor, its private route realm. A compiled
// server profile injects the plane and handler and therefore leaves realm nil.
type gatewayManagement struct {
	plane   *SessionInspectionPlane
	handler http.Handler
	realm   *pluginruntime.Mounted
}

func newGatewayManagement(ttl time.Duration) (*gatewayManagement, error) {
	plane, err := NewSessionInspectionPlane(ttl)
	if err != nil {
		return nil, err
	}
	bundle, err := managementserver.NewBundle(managementserver.BundleConfig{
		Authorizer: plane.Authorizer(),
		Sessions:   plane.Sessions(),
	})
	if err != nil {
		return nil, fmt.Errorf("create gateway management bundle: %w", err)
	}
	realm, err := bundle.Mount(context.Background())
	if err != nil {
		return nil, fmt.Errorf("mount gateway management bundle: %w", err)
	}
	handler, err := managementserver.HTTPHandler(realm, "http")
	if err != nil {
		_ = realm.Close(context.Background())
		return nil, fmt.Errorf("export gateway management handler: %w", err)
	}
	return &gatewayManagement{
		plane: plane, handler: handler, realm: realm,
	}, nil
}

// register admits only a runtime with complete, exact initial evidence. The
// owner disposer returned by SessionRegistry prevents a stale session cleanup
// from unregistering another runtime that later reused the same public ID.
func (plane *SessionInspectionPlane) register(session string, runtime any) (func(), bool, error) {
	if plane == nil || runtime == nil {
		return nil, false, nil
	}
	live, ok := runtime.(liveInspectionSource)
	if !ok {
		return nil, false, nil
	}
	if err := management.ValidateSessionSnapshot(live.Live()); err != nil {
		// Inspection is optional. Incomplete legacy evidence must make the
		// capability absent rather than failing the conversational session.
		return nil, false, nil
	}
	managed := managedSessionRuntime{live: live}
	if trace, ok := runtime.(recordedTraceSource); ok {
		managed.trace = trace
	}
	dispose, err := plane.sessions.Register(session, managed)
	if err != nil {
		return nil, false, err
	}
	return dispose, true, nil
}

func (managementPlane *gatewayManagement) register(
	session string, runtime any,
) (func(), bool, error) {
	if managementPlane == nil {
		return nil, false, nil
	}
	return managementPlane.plane.register(session, runtime)
}

func (plane *SessionInspectionPlane) issue(
	session string,
) (openrealtime.InspectionAccess, func(), error) {
	if plane == nil {
		return openrealtime.InspectionAccess{}, nil, management.ErrUnavailable
	}
	// Revalidate immediately before minting authority. Registration alone is
	// never evidence that a runtime still has an exact public snapshot.
	snapshot, err := plane.sessions.Snapshot(context.Background(), session)
	if err != nil || management.ValidateSessionSnapshot(snapshot) != nil {
		return openrealtime.InspectionAccess{}, nil, management.ErrUnavailable
	}
	access, revoke, err := plane.capabilities.IssueScoped(plane.ttl, []management.Grant{
		{Operation: management.ReadSession, Resource: session},
		{Operation: management.ReadTrace, Resource: session},
	})
	if err != nil {
		return openrealtime.InspectionAccess{}, nil, err
	}
	return openrealtime.InspectionAccess{
		SessionID: session,
		// Keep the negotiated v1 path stable. The same bearer is also valid at
		// management.APIPrefix + "/sessions/" + session + "/live".
		Path:        "/v1/realtime/sessions/" + url.PathEscape(session) + "/live",
		Token:       access.Token,
		ExpiresAtMS: access.ExpiresAt.UnixMilli(),
	}, revoke, nil
}

func (managementPlane *gatewayManagement) issue(
	session string,
) (openrealtime.InspectionAccess, func(), error) {
	if managementPlane == nil {
		return openrealtime.InspectionAccess{}, nil, management.ErrUnavailable
	}
	return managementPlane.plane.issue(session)
}

func (plane *gatewayManagement) close(ctx context.Context) error {
	if plane == nil || plane.realm == nil {
		return nil
	}
	return plane.realm.Close(ctx)
}

// inspectLive is a compatibility adapter only: deployment authentication and
// the historical header/path remain stable, then the request is delegated to
// the canonical management server.
func (server *Server) inspectLive(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	forward := request.Clone(request.Context())
	forward.Header = request.Header.Clone()
	forward.Header.Set(
		management.CapabilityHeader,
		strings.TrimSpace(request.Header.Get(InspectionTokenHeader)),
	)
	forwardURL := *request.URL
	forwardURL.Path = management.APIPrefix + "/sessions/" +
		url.PathEscape(request.PathValue("session")) + "/live"
	forwardURL.RawPath = ""
	forward.URL = &forwardURL
	server.management.handler.ServeHTTP(writer, forward)
}

func inspectionDebugEnabled(debug *openrealtime.DebugResponse) bool {
	return debug != nil && debug.Enabled &&
		(len(debug.Categories) == 0 || containsDebugCategory(debug.Categories, openrealtime.DebugSession))
}

func containsDebugCategory(categories []openrealtime.DebugCategory, want openrealtime.DebugCategory) bool {
	for _, category := range categories {
		if category == want {
			return true
		}
	}
	return false
}

func (session *session) inspectionAccess() (*openrealtime.InspectionAccess, error) {
	session.inspectionMu.Lock()
	defer session.inspectionMu.Unlock()
	if session.inspectionClosed || session.inspectionDispose == nil {
		return nil, nil
	}
	// Every explicit negotiation rotates the capability. The session retains
	// only an owner revoker, never the plaintext bearer it already returned.
	if session.inspectionRevoke != nil {
		session.inspectionRevoke()
		session.inspectionRevoke = nil
	}
	access, revoke, err := session.config.management.issue(session.id)
	if errors.Is(err, management.ErrUnavailable) || errors.Is(err, management.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	session.inspectionRevoke = revoke
	return &access, nil
}

func (session *session) disableInspection() {
	session.inspectionMu.Lock()
	defer session.inspectionMu.Unlock()
	if session.inspectionRevoke != nil {
		session.inspectionRevoke()
		session.inspectionRevoke = nil
	}
}

func (session *session) closeInspection() {
	session.inspectionMu.Lock()
	defer session.inspectionMu.Unlock()
	if session.inspectionClosed {
		return
	}
	session.inspectionClosed = true
	if session.inspectionRevoke != nil {
		session.inspectionRevoke()
		session.inspectionRevoke = nil
	}
	if session.inspectionDispose != nil {
		session.inspectionDispose()
		session.inspectionDispose = nil
	}
}
