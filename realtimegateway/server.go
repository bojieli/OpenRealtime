// Package realtimegateway serves the standard OpenAI Realtime WebSocket
// protocol over OpenRealtime's canonical fast/slow trajectory runtime.
package realtimegateway

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/coder/websocket"
)

type PerceptionFactory func() (v1.PerceptionProvider, error)

type Config struct {
	Token              string
	Model              string
	PerceptionFactory  PerceptionFactory
	FastProvider       continuation.Provider
	SlowProvider       continuation.Provider
	SpeechProvider     v1.StreamingSpeechProvider
	FastMaxTokens      int
	SlowMaxTokens      int
	MaxSlowInvocations int
	SlowPreparationMin time.Duration
	MaxAudioFrameBytes int
	MaxPendingEvents   int
	ValidateWire       bool
}

type Server struct{ config Config }

func New(config Config) (*Server, error) {
	config.Token = strings.TrimSpace(config.Token)
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = "openrealtime-local"
	}
	if config.PerceptionFactory == nil || config.FastProvider == nil || config.SlowProvider == nil || config.SpeechProvider == nil {
		return nil, errors.New("Realtime gateway requires ASR, fast, slow, and speech providers")
	}
	if config.FastMaxTokens <= 0 {
		config.FastMaxTokens = 32
	}
	if config.SlowMaxTokens <= 0 {
		config.SlowMaxTokens = 2_048
	}
	if config.MaxSlowInvocations <= 0 {
		config.MaxSlowInvocations = 8
	}
	if config.MaxAudioFrameBytes <= 0 {
		config.MaxAudioFrameBytes = 1 << 20
	}
	if config.MaxPendingEvents <= 0 {
		config.MaxPendingEvents = 128
	}
	if config.MaxPendingEvents < 2 {
		return nil, errors.New("Realtime gateway requires at least two pending event slots")
	}
	return &Server{config: config}, nil
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	})
	mux.Handle("GET /v1/realtime", server)
	return mux
}

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
	connection.SetReadLimit(int64(server.config.MaxAudioFrameBytes * 2))
	session, err := newSession(request.Context(), connection, server.config, request.URL.Query().Get("model"))
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, "session initialization failed")
		return
	}
	if err := session.Run(); err != nil {
		_ = connection.Close(websocket.StatusInternalError, "session failed")
		return
	}
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
