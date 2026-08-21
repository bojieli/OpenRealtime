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
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
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
	// Binding provides session runtimes. It is the only thing the gateway
	// needs to know about how conversation actually happens.
	Binding binding.Binding
	// Token, when set, is the bearer token required on the upgrade request.
	Token string
	// Demo, when set, is served at /demo. It is nil unless an operator asks
	// for it, because a page is not part of a realtime server's job.
	Demo http.Handler
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
	// Logger receives structured operational events. It never receives
	// conversation content: a log that leaked what was said would be a worse
	// problem than having no log.
	Logger *slog.Logger
}

// Server serves /v1/realtime and /healthz.
type Server struct {
	config Config
}

// New validates the configuration and creates a server.
func New(config Config) (*Server, error) {
	if config.Binding == nil {
		return nil, errors.New("the gateway requires a binding")
	}
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
	if config.Metrics == nil {
		config.Metrics = &Metrics{}
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	config.Token = strings.TrimSpace(config.Token)
	return &Server{config: config}, nil
}

// Handler returns the HTTP surface.
func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /metrics", server.metrics)
	mux.Handle("GET /v1/realtime", server)
	// The demo is opt-in. A production server has no business serving a page,
	// and one that appeared on every deployment would be surface nobody asked
	// for; but with it enabled, trying the system out is one command.
	if server.config.Demo != nil {
		mux.Handle("GET /demo", server.config.Demo)
		mux.Handle("GET /demo/", http.StripPrefix("/demo", server.config.Demo))
	}
	return mux
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
	if server.config.Binding.Capabilities().Video {
		legal := int64(server.config.VideoLimits.MaxTransportBytes())
		limit = max(limit, legal*frameReadHeadroom/4)
	}
	return limit
}

func (server *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"status": "ok", "model": server.config.Model,
		"binding":      server.config.Binding.Name(),
		"ownership":    server.config.Binding.Ownership(),
		"capabilities": server.config.Binding.Capabilities(),
		"protocol": map[string]any{
			"openai_realtime": "pinned",
			"openrealtime":    map[string]any{"version": openrealtime.Version, "features": openrealtime.Features()},
		},
		"sessions": server.config.Metrics.Snapshot(),
	})
}

func (server *Server) metrics(writer http.ResponseWriter, _ *http.Request) {
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
	server.config.Logger.Info("session started",
		"session", session.id, "binding", server.config.Binding.Name(), "model", session.model)
	if err := session.Run(); err != nil {
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
