// Package gateway serves the OpenRealtime Protocol over WebSocket.
//
// The protocol over WebSocket is the only entrance to a session. Everything
// else - a WebRTC adapter, a LiveKit agent participant - sits strictly above
// it and speaks it like any other client. That is not tidiness: a transport
// that reached the session core directly would become a second place the
// protocol can drift, and every such place multiplies the compatibility
// surface until a feature exists on one path and not the other.
//
// This package is a renderer. It translates protocol events into binding
// runtime calls and the runtime's output back into protocol events, and it
// decides nothing about the conversation.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	effectauthority "github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/timeline"
	"github.com/coder/websocket"
)

// Config configures the server.
// sessionSequence names sessions uniquely within a process.
//
// It lives on the server rather than the session because a session's own item
// counter starts at zero, so deriving an identity from it would name every
// session in the process the same thing.
var sessionSequence atomic.Uint64

func (config Config) nextSessionID() string {
	return fmt.Sprintf("sess_%012d", sessionSequence.Add(1))
}

type Config struct {
	// Binding provides session runtimes. Its minimal start seam deliberately
	// carries no topology or capability projection; New resolves those protocol
	// facts from an exact graph adapter profile or the retained legacy fallback.
	Binding SessionBinding
	// Token, when set, is the bearer token required on the upgrade request.
	Token string
	// Model is the compatibility model identifier reported to clients.
	Model string
	// TranscriptionModel is reported in the session object so a client can see
	// what produced its transcripts.
	TranscriptionModel string
	// MaxAudioFrameBytes bounds one inbound audio frame.
	MaxAudioFrameBytes int
	// VideoLimits are the video bounds this deployment advertises at
	// negotiation and enforces on every frame. The zero value selects the
	// shipped defaults.
	//
	// They are configuration rather than a constant because the connection's
	// read limit is derived from them: the two numbers have to move together
	// or the transport starts refusing frames the protocol just promised to
	// accept.
	VideoLimits openrealtime.Limits
	// ValidateWire checks every base-protocol event in both directions against
	// the pinned schema. It is on by default: a compatibility claim that is
	// not continuously checked is a compatibility claim that decays.
	ValidateWire bool
	// Metrics is optional operational telemetry.
	Metrics *Metrics
	// Recogniser, when set, reports the recogniser boundary for the whole
	// process. Only a binding that owns perception can supply it: on `omni`,
	// `duplex`, and `upstream` the model hears the user directly and there is
	// no recogniser to time, so it stays nil and the field is absent rather
	// than reported as zero. A zero would read as a recogniser answering
	// instantly, which is the opposite of the truth.
	Recogniser func() RecogniserSnapshot
	// Warm reports whether the models a turn waits on have answered once. It
	// is optional: unset means nothing to wait for.
	//
	// Health is what a caller asks before deciding the server is ready, and a
	// local model server answers its own health long before it answers a
	// request at speed. Reporting ok while the first turn will be degraded
	// answers a different question than the one being asked - measured, the
	// first call after start failed the control scenario and the next nine
	// passed, ten times out of ten.
	Warm func() bool
	// Logger receives structured operational events. It never receives
	// conversation content: a log that leaked what was said would be a worse
	// problem than having no log.
	Logger *slog.Logger
	// Timeline receives one line per turn event - each transcript revision,
	// each policy choice, each model request and answer, each utterance
	// synthesised and played, each background question - for every session.
	// Unlike Logger it carries conversation content, because a turn cannot be
	// read back without the words; it is written only where an operator asked
	// for it. Nil disables it.
	Timeline *timeline.Writer
	// WriteTimeout bounds one outbound WebSocket write. Zero selects the
	// shipped default; a negative value removes the bound.
	//
	// Without it a client that stops reading wedges its session permanently.
	// The socket write blocks once the peer's receive window closes, the
	// writer goroutine stops draining, the send buffer fills, the handler
	// blocks in send, and the read loop then blocks handing it the next event
	// - at which point nothing in the session can make progress and nothing
	// ends it, because the session context has no deadline of its own. The
	// binding runtime and its provider connections are held for as long as the
	// process lives.
	//
	// The default is derived rather than chosen: it has to be longer than the
	// longest stall a healthy receiver can have, and a congested mobile link
	// can hold a flow for several seconds. Thirty is comfortably past that and
	// still bounded, and a realtime conversation whose client has not accepted
	// a frame for thirty seconds is over regardless of what the socket does
	// next.
	WriteTimeout time.Duration
	// KeepaliveInterval is how long a connection may be idle before the server
	// pings it. Zero selects the shipped default; a negative value disables
	// the ping.
	//
	// The WebSocket library answers the client's pings, so a client that sends
	// them learns the server is alive. Nothing tells the server the reverse. A
	// client that disappears without a FIN - a NAT rebind, a laptop lid, a
	// dropped mobile handover - leaves a connection that is open on this side
	// only, and in a session where neither side is currently speaking there is
	// no write to discover it with. The HTTP server's IdleTimeout does not
	// apply, because the connection was hijacked at the upgrade.
	//
	// The ping is sent only after an idle interval, so it costs nothing on a
	// session that is carrying audio, and it is bounded by the same interval:
	// a peer that has not answered within one is not answering.
	KeepaliveInterval time.Duration
	// MaxSessions bounds concurrently admitted sessions. Zero is unbounded,
	// which is what an embedder gets unless it says otherwise; a launcher that
	// owns its deployment's capacity sets a number.
	//
	// Admission is the only place a limit can be applied honestly. Past this
	// point a session holds a binding runtime and its provider connections,
	// and the way an unbounded gateway fails is by exhausting the process
	// rather than by refusing anything. A refusal is a 503 the caller can
	// retry; an exhausted process takes every established session with it.
	MaxSessions int
	// InspectionTokenTTL bounds the read-only management capability issued to
	// a session that explicitly negotiates session debugging. Zero selects one
	// hour. The capability is revoked earlier when debugging is disabled or the
	// owning session ends.
	InspectionTokenTTL time.Duration
	// SessionInspection and ManagementHandler are the clean server-composition
	// seams for session observability. They must either both be absent, in which
	// case New preserves the standalone Handler compatibility surface, or both
	// be supplied by an exact server plugin profile. The handler is used only as
	// the canonical management delegate; it is never rendered into protocol
	// state or retained by a session.
	SessionInspection *SessionInspectionPlane
	ManagementHandler http.Handler
	// ClientEffectIssuer is the replaceable server authority seam for the
	// negotiated client.effects extension. Nil means the feature is not
	// offered. The issuer is never rendered into session state, graphs, logs,
	// or evidence; only its opaque per-call receipt crosses the wire.
	ClientEffectIssuer effectauthority.EffectReceiptIssuerProvider
	// ServerProfile supplies the exact mounted server-realm composition. It is
	// optional for compatibility constructors; production launchers set it
	// before exposing the handler. Health refuses an incomplete configured
	// profile instead of reporting only caller-asserted binding labels.
	ServerProfile func() pluginruntime.Live

	management      *gatewayManagement
	bindingContract sessionBindingContract
}

// Server serves /v1/realtime and /healthz.
type Server struct {
	config     Config
	management *gatewayManagement

	lifecycleMu sync.Mutex
	sessions    map[*session]struct{}
	sessionWait sync.WaitGroup
	closeOnce   sync.Once
	drained     chan struct{}
	closing     bool
	// inFlight counts connections that have been admitted and not yet
	// finished. It is the authoritative number for the capacity decision and
	// is kept here rather than on Metrics because that decision is made under
	// this mutex; Metrics carries a mirror of it for reporting only.
	inFlight int
}

var (
	errGatewayClosed = errors.New("gateway is closed")
	// errGatewaySaturated is refusal, not failure. It is separate from
	// errGatewayClosed because the two mean opposite things to a caller: one
	// says stop, the other says try again.
	errGatewaySaturated = errors.New("gateway is at session capacity")
)

const (
	defaultWriteTimeout      = 30 * time.Second
	defaultKeepaliveInterval = 20 * time.Second
)

// New validates the configuration and creates a server.
func New(config Config) (*Server, error) {
	if nilInterface(config.Binding) {
		return nil, errors.New("the gateway requires a binding")
	}
	bindingContract, err := resolveSessionBindingContract(config.Binding)
	if err != nil {
		return nil, fmt.Errorf("gateway binding contract: %w", err)
	}
	config.bindingContract = bindingContract.clone()
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = "openrealtime"
	}
	if strings.TrimSpace(config.TranscriptionModel) == "" {
		config.TranscriptionModel = "openrealtime-perception"
	}
	if config.MaxAudioFrameBytes <= 0 {
		config.MaxAudioFrameBytes = 1 << 20
	}
	defaults := openrealtime.DefaultLimits()
	if strings.TrimSpace(config.VideoLimits.Format) == "" {
		config.VideoLimits.Format = defaults.Format
	}
	if config.VideoLimits.FPSCap <= 0 {
		config.VideoLimits.FPSCap = defaults.FPSCap
	}
	if config.VideoLimits.MaxDimension <= 0 {
		config.VideoLimits.MaxDimension = defaults.MaxDimension
	}
	if config.VideoLimits.MaxFrameBytes <= 0 {
		config.VideoLimits.MaxFrameBytes = defaults.MaxFrameBytes
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.KeepaliveInterval == 0 {
		config.KeepaliveInterval = defaultKeepaliveInterval
	}
	if config.MaxSessions < 0 {
		return nil, fmt.Errorf("gateway session capacity must not be negative, got %d", config.MaxSessions)
	}
	if config.Metrics == nil {
		config.Metrics = &Metrics{}
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if nilInterface(config.ClientEffectIssuer) {
		config.ClientEffectIssuer = nil
	}
	var managementPlane *gatewayManagement
	inspectionSupplied := config.SessionInspection != nil
	managementHandlerSupplied := !nilInterface(config.ManagementHandler)
	switch {
	case !inspectionSupplied && !managementHandlerSupplied:
		var managementErr error
		managementPlane, managementErr = newGatewayManagement(config.InspectionTokenTTL)
		if managementErr != nil {
			return nil, managementErr
		}
	case !inspectionSupplied || !managementHandlerSupplied:
		return nil, errors.New("gateway session inspection and management handler must be supplied together")
	case config.InspectionTokenTTL != 0:
		return nil, errors.New("gateway inspection token TTL belongs to the supplied session-inspection plane")
	default:
		if err := config.SessionInspection.Validate(); err != nil {
			return nil, fmt.Errorf("gateway session inspection: %w", err)
		}
		managementPlane = &gatewayManagement{
			plane: config.SessionInspection, handler: config.ManagementHandler,
		}
	}
	config.management = managementPlane
	config.Token = strings.TrimSpace(config.Token)
	return &Server{
		config: config, management: managementPlane,
		sessions: make(map[*session]struct{}), drained: make(chan struct{}),
	}, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

// RealtimeHandler is the OpenAI-compatible WebSocket endpoint implementation.
// It carries no route pattern so a server profile, rather than the gateway,
// owns public route selection.
func (server *Server) RealtimeHandler() http.Handler {
	if server == nil {
		return http.NotFoundHandler()
	}
	return server
}

// HealthHandler is the payload-only readiness endpoint implementation.
func (server *Server) HealthHandler() http.Handler {
	if server == nil {
		return http.NotFoundHandler()
	}
	return http.HandlerFunc(server.health)
}

// MetricsHandler is the payload-only bounded telemetry endpoint
// implementation.
func (server *Server) MetricsHandler() http.Handler {
	if server == nil {
		return http.NotFoundHandler()
	}
	return http.HandlerFunc(server.metrics)
}

// ManagementHandler is the canonical, capability-authorized management API
// implementation. Server route plugins select its exact public resource
// families.
func (server *Server) ManagementHandler() http.Handler {
	if server == nil || server.management == nil || server.management.handler == nil {
		return http.NotFoundHandler()
	}
	return server.management.handler
}

// Close stops admission, terminates every admitted realtime session, waits for
// its runtime and inspection authority to retire, and releases the mounted
// management route realm. This makes a gateway a complete lifecycle resource:
// replacing its plugin cannot leave a session using retired dependencies.
func (server *Server) Close(ctx context.Context) error {
	if server == nil || ctx == nil {
		return errors.New("close gateway: nil server or context")
	}
	server.closeOnce.Do(func() {
		server.lifecycleMu.Lock()
		server.closing = true
		active := make([]*session, 0, len(server.sessions))
		for current := range server.sessions {
			active = append(active, current)
		}
		server.lifecycleMu.Unlock()
		for _, current := range active {
			current.cancel(errGatewayClosed)
			_ = current.connection.CloseNow()
		}
		go func() {
			server.sessionWait.Wait()
			close(server.drained)
		}()
	})
	select {
	case <-server.drained:
	case <-ctx.Done():
		return fmt.Errorf("close gateway sessions: %w", ctx.Err())
	}
	return server.management.close(ctx)
}

// readLimit bounds one inbound message at the largest thing this deployment
// can legally be sent.
//
// The limit is enforced by the WebSocket layer, below any of the protocol's
// own validation, and exceeding it closes the connection rather than
// producing an error the client can act on. So it is derived from the
// advertised bounds rather than chosen: a read limit under the frame size
// negotiation just promised would turn a legal frame into a dropped session,
// and the client would have no way to learn why.
//
// It is fixed for the life of the connection rather than raised when video is
// negotiated. Raising it on negotiation would be tighter for a voice-only
// session, but session.update is handled off the read goroutine, so a client
// that declared video and immediately sent a frame could race its own
// negotiation and lose the connection to it. Whether the binding can carry
// video at all is known before the connection is accepted, which gives the
// same precision with no window to race.
//
// The headroom above the largest legal frame is what makes an oversized one
// answerable. A client that forgot to downscale is the common mistake, and
// the useful reply is the protocol's own "frame of N bytes exceeds the M byte
// limit" - which the session can only produce for a message it was allowed to
// finish reading. A quarter over the legal maximum covers that mistake and
// still refuses anything that is not one.
const frameReadHeadroom = 5

func (server *Server) readLimit() int64 {
	limit := int64(server.config.MaxAudioFrameBytes) * 2
	if server.config.bindingContract.capabilities.Video {
		legal := int64(server.config.VideoLimits.MaxTransportBytes())
		limit = max(limit, legal*frameReadHeadroom/4)
	}
	return limit
}

// health answers what this process is running. The liveness half of that
// answer is public and the inventory half is not.
//
// The payload names the binding, the model identities, the live component
// digests, and the server-profile fingerprint - which is the right answer to
// "what is this process actually running" and the wrong thing to hand an
// anonymous caller on a deployment that has a token precisely because it is
// reachable. Splitting it is what keeps both true: the status code and the
// status word stay open, because a load balancer cannot present a credential
// and taking a healthy server out of rotation for a 401 would be a worse
// failure than the disclosure; everything else needs the same bearer token the
// Realtime endpoint needs.
//
// A deployment with no token configured is a loopback or development one, and
// it keeps the whole payload: there is no credential to present, and gating on
// one that does not exist would only mean the detail is never available.
func (server *Server) health(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	warm := server.config.Warm == nil || server.config.Warm()
	var profile *pluginruntime.Live
	if server.config.ServerProfile != nil {
		live := server.config.ServerProfile()
		profile = &live
		warm = warm && validServerProfile(live)
	}
	status := "ok"
	if !warm {
		// Serving, and not yet ready to be measured or routed to.
		status = "warming"
		writer.WriteHeader(http.StatusServiceUnavailable)
	} else {
		writer.WriteHeader(http.StatusOK)
	}
	if !server.authorized(request) {
		_ = json.NewEncoder(writer).Encode(map[string]any{"status": status})
		return
	}
	payload := map[string]any{
		"status": status, "model": server.config.Model,
		"binding":      server.config.Binding.Name(),
		"ownership":    server.config.bindingContract.ownership,
		"capabilities": cloneBindingCapabilities(server.config.bindingContract.capabilities),
		"protocol": map[string]any{
			"openai_realtime": "pinned",
			"openrealtime":    map[string]any{"version": openrealtime.Version, "features": openrealtime.Features()},
		},
		"sessions": server.config.Metrics.Snapshot(),
	}
	if profile != nil {
		payload["server_profile"] = profile
	}
	if server.config.Recogniser != nil {
		payload["recogniser"] = server.config.Recogniser()
	}
	_ = json.NewEncoder(writer).Encode(payload)
}

func validServerProfile(live pluginruntime.Live) bool {
	if live.FormatVersion != pluginruntime.LiveFormatVersion ||
		live.Realm != plugin.ServerRealm || live.State != "active" ||
		!management.CanonicalDigest(live.Fingerprint) || len(live.Entries) == 0 {
		return false
	}
	for _, entry := range live.Entries {
		if entry.State != "active" || !entry.Desired || entry.Implementation == "" ||
			entry.Runtime.Validate() != nil {
			return false
		}
	}
	for _, export := range live.Exports {
		if !export.Available || export.Revision == 0 {
			return false
		}
	}
	return true
}

// metrics reports counters, never content - and on a deployment that has a
// token, only to a caller that presents it. Session and media volume is not
// conversation, but it is still this deployment's traffic, and there is no
// liveness argument for publishing it the way there is for the health status
// word: nothing routes on /metrics.
func (server *Server) metrics(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(server.config.Metrics.Snapshot())
}

// ServeHTTP upgrades a request into a session.
func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !server.authorized(request) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := server.beginSession(); err != nil {
		if errors.Is(err, errGatewaySaturated) {
			server.config.Metrics.sessionsRejected.Add(1)
			// Retry-After makes the refusal actionable. A load balancer that
			// sees a bare 503 has to guess whether to take the instance out of
			// rotation; one that is told to come back in a second knows this
			// is capacity rather than a broken process.
			writer.Header().Set("Retry-After", "1")
		}
		http.Error(writer, err.Error(), http.StatusServiceUnavailable)
		return
	}
	var admitted *session
	defer func() { server.finishSession(admitted) }()
	connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	connection.SetReadLimit(server.readLimit())
	server.config.Metrics.sessionsStarted.Add(1)
	started := time.Now()
	session, err := newSession(request.Context(), connection, server.config, request.URL.Query().Get("model"))
	if err != nil {
		server.config.Metrics.sessionsFailed.Add(1)
		server.config.Logger.Error("session initialisation failed",
			"binding", server.config.Binding.Name(), "error", err)
		_ = connection.Close(websocket.StatusInternalError, "session initialisation failed")
		return
	}
	if !server.admitSession(session) {
		session.closeInspection()
		_ = session.runtime.Close(context.Background(), errGatewayClosed)
		session.cancel(errGatewayClosed)
		_ = connection.Close(websocket.StatusGoingAway, "gateway closed")
		return
	}
	admitted = session
	server.config.Logger.Info("session started",
		"session", session.id, "binding", server.config.Binding.Name(), "model", session.model)
	if err := session.Run(); err != nil {
		if errors.Is(context.Cause(session.ctx), errGatewayClosed) {
			server.config.Logger.Info("session stopped with gateway",
				"session", session.id, "duration", time.Since(started))
			return
		}
		server.config.Metrics.sessionsFailed.Add(1)
		server.config.Logger.Error("session failed",
			"session", session.id, "duration", time.Since(started), "error", err)
		_ = connection.Close(websocket.StatusInternalError, "session failed")
		return
	}
	server.config.Metrics.sessionsCompleted.Add(1)
	server.config.Logger.Info("session completed",
		"session", session.id, "duration", time.Since(started))
	_ = connection.Close(websocket.StatusNormalClosure, "session closed")
}

func (server *Server) beginSession() error {
	server.lifecycleMu.Lock()
	defer server.lifecycleMu.Unlock()
	if server.closing {
		return errGatewayClosed
	}
	if server.config.MaxSessions > 0 && server.inFlight >= server.config.MaxSessions {
		return errGatewaySaturated
	}
	server.inFlight++
	server.config.Metrics.sessionsInFlight.Store(int64(server.inFlight))
	server.sessionWait.Add(1)
	return nil
}

func (server *Server) admitSession(current *session) bool {
	server.lifecycleMu.Lock()
	defer server.lifecycleMu.Unlock()
	if server.closing {
		return false
	}
	server.sessions[current] = struct{}{}
	return true
}

func (server *Server) finishSession(current *session) {
	server.lifecycleMu.Lock()
	if current != nil {
		delete(server.sessions, current)
	}
	server.inFlight--
	server.config.Metrics.sessionsInFlight.Store(int64(server.inFlight))
	server.lifecycleMu.Unlock()
	server.sessionWait.Done()
}

func (server *Server) authorized(request *http.Request) bool {
	if server.config.Token == "" {
		return true
	}
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	if !strings.HasPrefix(value, "Bearer ") {
		return false
	}
	provided := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	return subtle.ConstantTimeCompare([]byte(provided), []byte(server.config.Token)) == 1
}
