// Package geminilive presents Google's Live API as a Realtime endpoint.
//
// BidiGenerateContent is not a dialect of the Realtime protocol. It has no
// event type field - messages are discriminated by which top-level key is
// present - no conversation items, no session updates after the handshake, and
// it wants 16 kHz audio in while the Realtime wire carries 24 kHz. So this is a
// translator rather than a profile.
//
// It exists as a translator rather than a second binding because the upstream
// binding needs exactly four methods from its connection. Meeting those with a
// package that speaks Gemini on one side and the Realtime protocol on the other
// keeps one runtime, one mirror, and one set of interaction policies, and
// leaves the translation independently testable.
//
// Three of Gemini's differences are load-bearing:
//
//   - The system instruction can only be set in the opening setup message. So
//     setup is deferred until the caller's first session.update arrives, and
//     anything sent before that is held rather than dropped.
//   - Transcription arrives incrementally with no completion event. Input text
//     is accumulated and reported when the turn completes, which is the point
//     the Realtime protocol reports it.
//   - Input audio must be 16 kHz. It is resampled here rather than at the
//     caller, because the caller is speaking a protocol that says 24 kHz.
package geminilive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/pcm"
	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/coder/websocket"
)

const (
	// DefaultURL is the Live API WebSocket endpoint.
	DefaultURL = "wss://generativelanguage.googleapis.com/ws/" +
		"google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
	// DefaultModel is a Live-capable model. Like every default model in this
	// project it is a hint: ask the API which models list bidiGenerateContent.
	DefaultModel = "gemini-2.5-flash-native-audio-latest"

	// InputSampleRateHz is what the Live API requires of input audio.
	InputSampleRateHz = uint32(16_000)
	// OutputSampleRateHz is what it produces, and happens to be what the
	// Realtime wire carries, so output is passed through unresampled.
	OutputSampleRateHz = uint32(24_000)

	defaultDialTimeout = 30 * time.Second
	// A send here is one turn of audio or one control message. Five seconds
	// is past any stall a congested link produces and far short of a wait a
	// speaking person would sit through.
	defaultWriteTimeout = 5 * time.Second
	defaultReadLimit    = int64(8 << 20)
	// pendingLimit bounds audio held while the handshake completes. It is
	// generous: the handshake is one round trip and the alternative to holding
	// is losing the opening syllable of a conversation.
	pendingLimit = 256
)

// Config configures one translated connection.
type Config struct {
	// URL is the Live endpoint. Empty selects DefaultURL.
	URL string
	// APIKey is sent as the key query parameter.
	APIKey string
	// Model is the Live model. A bare name is prefixed with "models/".
	Model string
	// Header carries additional request headers.
	Header http.Header
	// CallerSampleRateHz is the rate the caller sends audio at. Empty selects
	// the Realtime wire's 24 kHz.
	CallerSampleRateHz uint32
	// DialTimeout bounds connection establishment.
	DialTimeout time.Duration
	// WriteTimeout bounds one send on an established connection. Zero selects
	// the shipped default; a negative value removes the bound.
	//
	// The dial was bounded and the sends that follow it were not. The context
	// reaching them comes from the session and has no deadline, so a stalled
	// socket blocks a send that carries a caller's audio - and this client is
	// the foreground voice, so what stops is the conversation.
	WriteTimeout time.Duration
	// ReadLimit bounds one inbound message.
	ReadLimit int64
	// HTTPClient dials the WebSocket.
	HTTPClient *http.Client
}

// Client is a Live connection wearing the Realtime protocol.
type Client struct {
	config     Config
	connection *websocket.Conn
	model      string

	events  chan realtimeclient.Event
	closed  chan struct{}
	done    chan struct{}
	readErr error
	errMu   sync.Mutex
	once    sync.Once

	writeMu   sync.Mutex
	setupSent bool
	pending   [][]byte
	resampler *pcm.Resampler
	// turn buffers the client turns a conversation item builds up, which are
	// sent when the caller asks for a response.
	turn []map[string]any

	// inputText and outputText accumulate transcription across a turn, which
	// is how a protocol with no completion event is given one.
	transcriptMu sync.Mutex
	inputText    strings.Builder
	outputText   strings.Builder
}

// Dial opens the connection and starts translating.
func Dial(ctx context.Context, config Config) (*Client, error) {
	if config.URL == "" {
		config.URL = DefaultURL
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || (parsed.Scheme != "ws" && parsed.Scheme != "wss") {
		return nil, errors.New("Gemini Live URL must be an absolute ws or wss URL")
	}
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("Gemini Live requires an API key")
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = DefaultModel
	}
	model := config.Model
	if !strings.HasPrefix(model, "models/") {
		model = "models/" + model
	}
	if config.CallerSampleRateHz == 0 {
		config.CallerSampleRateHz = OutputSampleRateHz
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = defaultDialTimeout
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.ReadLimit <= 0 {
		config.ReadLimit = defaultReadLimit
	}
	resampler, err := pcm.NewResampler(config.CallerSampleRateHz, InputSampleRateHz)
	if err != nil {
		return nil, fmt.Errorf("configure Gemini Live resampler: %w", err)
	}

	query := parsed.Query()
	query.Set("key", config.APIKey)
	parsed.RawQuery = query.Encode()

	dialContext, cancel := context.WithTimeout(ctx, config.DialTimeout)
	defer cancel()
	connection, _, err := websocket.Dial(dialContext, parsed.String(), &websocket.DialOptions{
		HTTPHeader: config.Header.Clone(), HTTPClient: config.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("dial Gemini Live: %w", err)
	}
	connection.SetReadLimit(config.ReadLimit)
	client := &Client{
		config: config, connection: connection, model: model,
		events: make(chan realtimeclient.Event, 256),
		closed: make(chan struct{}), done: make(chan struct{}),
		resampler: resampler,
	}
	go client.read(ctx)
	return client, nil
}

// Events yields translated Realtime server events.
func (client *Client) Events() <-chan realtimeclient.Event { return client.events }

// Err reports why reading stopped, or nil for a clean close.
func (client *Client) Err() error {
	client.errMu.Lock()
	defer client.errMu.Unlock()
	return client.readErr
}

// Close ends the connection.
func (client *Client) Close() error {
	client.once.Do(func() { close(client.closed) })
	return client.connection.Close(websocket.StatusNormalClosure, "client closed")
}

// Wait blocks until reading has finished.
func (client *Client) Wait() { <-client.done }

func (client *Client) fail(err error) {
	client.errMu.Lock()
	if client.readErr == nil {
		client.readErr = err
	}
	client.errMu.Unlock()
}
