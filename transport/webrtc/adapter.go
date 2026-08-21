// Package webrtc terminates WebRTC and speaks the OpenRealtime Protocol.
//
// It is a client of the protocol, not a second way into a session. It handles
// ICE, the jitter buffer, packet loss concealment, and RTP pacing, and then
// speaks exactly the events any other client speaks - over a real WebSocket
// connection to the protocol endpoint, not through a private path.
//
// That is deliberate and it is load-bearing. A transport that reached the
// session core directly would become a second place the protocol can drift,
// and every such place multiplies the compatibility surface until a feature
// exists on one path and not the other. The acceptance test is adversarial:
// anything this adapter can express, a plain WebSocket client must be able to
// express too.
//
// The one thing it does change is the session's audio format, and only
// because it terminates media: the client's audio travels as RTP, so what
// format the protocol connection uses for audio is the adapter's business
// rather than the client's. Every other event is forwarded byte for byte.
package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

// EventChannel is the data channel name a Realtime client expects.
const EventChannel = "oai-events"

// Config configures the adapter.
type Config struct {
	// Endpoint is the protocol endpoint this adapter connects to, as a client.
	// A URL rather than an in-process handle: an adapter with a private path
	// into the session core is the thing this design refuses.
	Endpoint string
	// Token is the credential the adapter presents to the endpoint.
	Token string
	// Model selects the endpoint's model.
	Model string
	// ICEServers configures STUN and TURN. Empty is correct for direct and
	// localhost cases, which is what in-process termination targets;
	// production scale is what an RTC provider is for.
	ICEServers []webrtc.ICEServer
	// PacketDuration is the outbound RTP packetisation interval. Zero selects
	// 20 ms, which is what every endpoint in the field expects.
	PacketDuration time.Duration
	// ConnectTimeout bounds establishing the protocol connection.
	ConnectTimeout time.Duration
	// SessionTimeout bounds one whole session. Zero means no bound.
	SessionTimeout time.Duration
	// Logf receives operational messages. Nil discards them.
	Logf func(string, ...any)
}

// Adapter accepts WebRTC offers and bridges them to the protocol.
type Adapter struct {
	config Config
	api    *webrtc.API

	sessions atomic.Int64
	failures atomic.Int64
}

// New validates the configuration and builds the media engine.
//
// The media engine registers exactly one codec: G.711 mu-law. Browsers all
// support it, it needs no encoder library and therefore no cgo, and its
// payload is byte-identical to what the protocol already carries as
// audio/pcmu - so audio crosses this adapter without being transcoded at all.
// The trade is telephony bandwidth, which is what the base protocol's own
// g711_ulaw mode accepts; a deployment that needs wideband audio should use an
// RTC provider, which is what that path is for.
func New(config Config) (*Adapter, error) {
	if strings.TrimSpace(config.Endpoint) == "" {
		return nil, errors.New("a WebRTC adapter requires a protocol endpoint")
	}
	if config.PacketDuration <= 0 {
		config.PacketDuration = 20 * time.Millisecond
	}
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 15 * time.Second
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
	engine := &webrtc.MediaEngine{}
	if err := engine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1,
		},
		PayloadType: 0,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register audio codec: %w", err)
	}
	registry := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(engine, registry); err != nil {
		return nil, fmt.Errorf("register interceptors: %w", err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(engine), webrtc.WithInterceptorRegistry(registry))
	return &Adapter{config: config, api: api}, nil
}

// Metrics is operational telemetry.
type Metrics struct {
	Sessions int64 `json:"sessions"`
	Failures int64 `json:"failures"`
}

// Metrics reports adapter counters.
func (adapter *Adapter) Metrics() Metrics {
	return Metrics{Sessions: adapter.sessions.Load(), Failures: adapter.failures.Load()}
}

// Handler serves the SDP exchange.
//
// The shape matches what Realtime clients already do: POST an offer as
// application/sdp, receive an answer as application/sdp.
func (adapter *Adapter) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/realtime", adapter.offer)
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"status": "ok", "transport": "webrtc", "codec": "audio/PCMU",
			"endpoint": adapter.config.Endpoint, "sessions": adapter.Metrics(),
		})
	})
	return mux
}

func (adapter *Adapter) offer(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil || len(body) == 0 {
		http.Error(writer, "an SDP offer is required", http.StatusBadRequest)
		return
	}
	answer, err := adapter.Start(request.Context(), string(body), request.URL.Query().Get("model"))
	if err != nil {
		adapter.failures.Add(1)
		http.Error(writer, err.Error(), http.StatusBadGateway)
		return
	}
	writer.Header().Set("Content-Type", "application/sdp")
	writer.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(writer, answer)
}

// Start establishes one session and returns the SDP answer.
func (adapter *Adapter) Start(ctx context.Context, offer, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		model = adapter.config.Model
	}
	connection, err := adapter.api.NewPeerConnection(webrtc.Configuration{ICEServers: adapter.config.ICEServers})
	if err != nil {
		return "", fmt.Errorf("create peer connection: %w", err)
	}
	session := &session{
		adapter: adapter, connection: connection, model: model,
		done: make(chan struct{}),
	}
	if err := session.prepare(); err != nil {
		_ = connection.Close()
		return "", err
	}
	if err := connection.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer, SDP: offer,
	}); err != nil {
		_ = connection.Close()
		return "", fmt.Errorf("apply offer: %w", err)
	}
	answer, err := connection.CreateAnswer(nil)
	if err != nil {
		_ = connection.Close()
		return "", fmt.Errorf("create answer: %w", err)
	}
	gathered := webrtc.GatheringCompletePromise(connection)
	if err := connection.SetLocalDescription(answer); err != nil {
		_ = connection.Close()
		return "", fmt.Errorf("apply answer: %w", err)
	}
	// Answering with a complete description avoids trickle ICE over a
	// signalling channel this exchange does not have.
	select {
	case <-gathered:
	case <-ctx.Done():
		_ = connection.Close()
		return "", ctx.Err()
	case <-time.After(adapter.config.ConnectTimeout):
		_ = connection.Close()
		return "", errors.New("ICE gathering did not complete")
	}
	if err := session.connect(context.WithoutCancel(ctx)); err != nil {
		_ = connection.Close()
		return "", err
	}
	adapter.sessions.Add(1)
	return connection.LocalDescription().SDP, nil
}
