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
	// Model is the compatibility model identifier reported to clients.
	Model string
	// TranscriptionModel is reported in the session object so a client can see
	// what produced its transcripts.
	TranscriptionModel string
	// MaxAudioFrameBytes bounds one inbound audio frame.
	MaxAudioFrameBytes int
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
	return mux
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
	connection.SetReadLimit(int64(server.config.MaxAudioFrameBytes) * 2)
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
