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
	"crypto/subtle"
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
	// AudioCodec selects what the adapter sends the peer. Empty selects PCMU,
	// which every build can produce.
	AudioCodec AudioCodec
	// ClientCredential is the bearer token a caller must present to open a
	// session here. Empty accepts unauthenticated offers, which is only ever
	// right on a loopback listener: this endpoint spends the adapter's own
	// upstream credential, so anyone who can reach it unauthenticated is
	// spending the deployment's models. Callers binding a routable address
	// are required to set it.
	ClientCredential string
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
	// AllowedOrigins are the web origins permitted to POST an offer here.
	//
	// Empty is the default and means no cross-origin request is answered,
	// which is the current behaviour and the right one for a server-to-server
	// deployment. It is a list rather than a switch because this endpoint has
	// no credential of its own and starting a session is all it does: a
	// wildcard would let any page anyone visits open a session against any
	// adapter their browser can route to. The literal "*" is accepted for
	// local development and says what it is.
	//
	// It exists because the SDK's WebRTC transport only runs in a browser, and
	// a browser is always on a different origin from the adapter - the adapter
	// is a port on a server and the application is a site. Without this, an
	// unmodified official client cannot reach this endpoint at all.
	AllowedOrigins []string
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
// Two codecs are registered, and the asymmetry between them is deliberate.
//
// Inbound, a browser sends Opus at 48 kHz, and this adapter decodes it with a
// pure-Go decoder. That direction matters most: it carries the user's voice
// into speech recognition, where bandwidth is accuracy.
//
// Outbound, the adapter sends G.711 mu-law. There is no pure-Go Opus encoder,
// and the alternative - writing one, or taking a cgo dependency on libopus -
// trades a static binary and a verifiable build for a codec nobody here can
// check against a reference. Mu-law is 8 kHz telephony quality for synthesised
// speech, which is a real cost stated plainly rather than hidden: a deployment
// that needs wideband output should use an RTC provider, which is what that
// path is for.
//
// Registering both lets each direction pick what it can actually do, which is
// ordinary RTP: a sender chooses among the negotiated payload types, and
// nothing requires the two directions to agree.
func New(config Config) (*Adapter, error) {
	if strings.TrimSpace(config.Endpoint) == "" {
		return nil, errors.New("a WebRTC adapter requires a protocol endpoint")
	}
	if config.PacketDuration <= 0 {
		config.PacketDuration = 20 * time.Millisecond
	}
	resolved, err := ParseAudioCodec(string(config.AudioCodec))
	if err != nil {
		return nil, err
	}
	config.AudioCodec = resolved
	if config.ConnectTimeout <= 0 {
		config.ConnectTimeout = 15 * time.Second
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
	engine := &webrtc.MediaEngine{}
	for _, codec := range []webrtc.RTPCodecParameters{
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
				SDPFmtpLine: "minptime=10;useinbandfec=1",
			},
			PayloadType: 111,
		},
		{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1,
			},
			PayloadType: 0,
		},
	} {
		if err := engine.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, fmt.Errorf("register %s: %w", codec.MimeType, err)
		}
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
	mux.HandleFunc("POST /v1/realtime/calls", adapter.offer)
	mux.HandleFunc("OPTIONS /v1/realtime/calls", adapter.preflight)
	mux.HandleFunc("GET /healthz", func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"status": "ok", "transport": "webrtc", "codec": "audio/PCMU",
			"endpoint": adapter.config.Endpoint, "sessions": adapter.Metrics(),
		})
	})
	return mux
}

// allowedOrigin reports the value to echo for a request's Origin, or empty if
// this origin may not post here.
//
// The origin is echoed rather than answered with a wildcard, and Vary: Origin
// goes with it, because an intermediary that cached one origin's answer for
// another would hand out a permission nobody granted.
func (adapter *Adapter) allowedOrigin(origin string) string {
	if origin == "" {
		return ""
	}
	for _, allowed := range adapter.config.AllowedOrigins {
		if allowed == "*" || strings.EqualFold(strings.TrimSpace(allowed), origin) {
			return origin
		}
	}
	return ""
}

func (adapter *Adapter) writeCORS(writer http.ResponseWriter, request *http.Request) bool {
	writer.Header().Add("Vary", "Origin")
	origin := adapter.allowedOrigin(request.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	writer.Header().Set("Access-Control-Allow-Origin", origin)
	return true
}

// preflight answers the OPTIONS a browser sends before an offer.
//
// The SDP body and the bearer credential both make the offer a request no
// browser will send without asking first, so an adapter that does not answer
// this is an adapter no browser can reach from anywhere but its own origin.
func (adapter *Adapter) preflight(writer http.ResponseWriter, request *http.Request) {
	if !adapter.writeCORS(writer, request) {
		http.Error(writer, "cross-origin requests are not allowed by this adapter",
			http.StatusForbidden)
		return
	}
	writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	// The requested headers are echoed rather than enumerated. A client sends
	// its own alongside the two this endpoint needs - OpenAI's SDK adds an
	// X-OpenAI-Agents-SDK telemetry header - and a server cannot know in
	// advance what every client will identify itself with. Listing a fixed set
	// means the preflight fails for reasons that have nothing to do with what
	// the request is allowed to do, which is how this endpoint stayed
	// unreachable from a browser.
	//
	// Echoing is safe here because the decision that matters was already made:
	// this responds only for an origin the operator named, and headers cannot
	// widen what that origin is permitted to do.
	requested := request.Header.Get("Access-Control-Request-Headers")
	if strings.TrimSpace(requested) == "" {
		requested = "Content-Type, Authorization"
	}
	writer.Header().Add("Vary", "Access-Control-Request-Headers")
	writer.Header().Set("Access-Control-Allow-Headers", requested)
	writer.Header().Set("Access-Control-Max-Age", "600")
	writer.WriteHeader(http.StatusNoContent)
}

// authorized reports whether a request carries the configured bearer
// credential. The comparison is constant time so a caller cannot learn the
// credential from how long a refusal takes.
func (adapter *Adapter) authorized(request *http.Request) bool {
	if adapter.config.ClientCredential == "" {
		return true
	}
	header := request.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	presented := strings.TrimSpace(header[len(prefix):])
	return subtle.ConstantTimeCompare(
		[]byte(presented), []byte(adapter.config.ClientCredential),
	) == 1
}

func (adapter *Adapter) offer(writer http.ResponseWriter, request *http.Request) {
	// A cross-origin POST that the operator did not permit is refused rather
	// than served without the header: CORS only stops a browser from reading
	// an answer, so serving it anyway would still have started the session.
	if request.Header.Get("Origin") != "" && !adapter.writeCORS(writer, request) {
		http.Error(writer, "cross-origin requests are not allowed by this adapter",
			http.StatusForbidden)
		return
	}
	if !adapter.authorized(request) {
		writer.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(writer, "a bearer credential is required", http.StatusUnauthorized)
		return
	}
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
		done: make(chan struct{}), inbound: newReassembler(),
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
