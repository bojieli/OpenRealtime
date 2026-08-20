// Package realtimegateway serves the standard OpenAI Realtime WebSocket
// protocol over OpenRealtime's canonical fast/slow trajectory runtime.
package realtimegateway

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interleave"
	"github.com/coder/websocket"
)

type PerceptionFactory func() (v1.PerceptionProvider, error)

type Config struct {
	Token               string
	Model               string
	ASRModel            string
	ASRProviderChunk    time.Duration
	ASRProviderMaxChunk time.Duration
	PerceptionFactory   PerceptionFactory
	FastProvider        continuation.Provider
	SlowProvider        continuation.Provider
	SpeechProvider      v1.StreamingSpeechProvider
	FastMaxTokens       int
	SlowMaxTokens       int
	MaxSlowInvocations  int
	SlowPreparationMin  time.Duration
	PreparationPolicy   PreparationPolicy
	ObservationPolicy   ObservationPolicy
	SlowContextPolicy   interleave.SlowContextPolicy
	MaxAudioFrameBytes  int
	MaxPendingEvents    int
	ValidateWire        bool
	RuntimeMetrics      *RuntimeMetrics
	preparationFast     continuation.Provider
	preparationSlow     continuation.Provider
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
	if config.SlowContextPolicy == "" {
		config.SlowContextPolicy = interleave.SlowContextCanonical
	}
	if config.PreparationPolicy == "" {
		config.PreparationPolicy = PreparationContinuous
	}
	if config.ObservationPolicy == "" {
		config.ObservationPolicy = ObservationEndpointOnly
	}
	if config.RuntimeMetrics == nil {
		config.RuntimeMetrics = &RuntimeMetrics{}
	}
	rawFast, rawSlow := config.FastProvider, config.SlowProvider
	config.FastProvider = &measuredContinuationProvider{
		provider: rawFast, metrics: &config.RuntimeMetrics.fast,
	}
	config.SlowProvider = &measuredContinuationProvider{
		provider: rawSlow, metrics: &config.RuntimeMetrics.slow,
	}
	config.preparationFast = &measuredContinuationProvider{
		provider: rawFast, metrics: &config.RuntimeMetrics.fastPreparation,
	}
	config.preparationSlow = &measuredContinuationProvider{
		provider: rawSlow, metrics: &config.RuntimeMetrics.slowPreparation,
	}
	config.SpeechProvider = &measuredSpeechProvider{
		provider: config.SpeechProvider, metrics: &config.RuntimeMetrics.speech,
	}
	if config.ASRProviderMaxChunk == 0 {
		config.ASRProviderMaxChunk = config.ASRProviderChunk
	}
	if _, err := interleave.ParseSlowContextPolicy(string(config.SlowContextPolicy)); err != nil {
		return nil, err
	}
	preparationPolicy, err := ParsePreparationPolicy(string(config.PreparationPolicy))
	if err != nil {
		return nil, err
	}
	config.PreparationPolicy = preparationPolicy
	observationPolicy, err := ParseObservationPolicy(string(config.ObservationPolicy))
	if err != nil {
		return nil, err
	}
	config.ObservationPolicy = observationPolicy
	return &Server{config: config}, nil
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"status": "ok", "model": server.config.Model,
			"asr": map[string]any{
				"model":                 server.config.ASRModel,
				"provider_chunk_ms":     float64(server.config.ASRProviderChunk) / float64(time.Millisecond),
				"provider_max_chunk_ms": float64(server.config.ASRProviderMaxChunk) / float64(time.Millisecond),
				"strategy": func() string {
					if server.config.ASRProviderMaxChunk > server.config.ASRProviderChunk {
						return "revision-adaptive"
					}
					return "fixed"
				}(),
			},
			"preparation_policy": server.config.PreparationPolicy,
			"observation_policy": server.config.ObservationPolicy,
			"slow_context":       server.config.SlowContextPolicy,
			"runtime":            server.config.RuntimeMetrics.Snapshot(),
			"fast":               server.config.FastProvider.Descriptor(),
			"slow":               server.config.SlowProvider.Descriptor(),
			"speech":             server.config.SpeechProvider.Descriptor(),
		})
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
	server.config.RuntimeMetrics.sessionsStarted.Add(1)
	session, err := newSession(request.Context(), connection, server.config, request.URL.Query().Get("model"))
	if err != nil {
		_ = connection.Close(websocket.StatusInternalError, "session initialization failed")
		return
	}
	if err := session.Run(); err != nil {
		_ = connection.Close(websocket.StatusInternalError, "session failed")
		return
	}
	server.config.RuntimeMetrics.sessionsCompleted.Add(1)
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
