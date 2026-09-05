// Package realtimeclient is a Realtime protocol client.
//
// It exists twice over: the upstream binding needs it to put a remote model
// behind OpenRealtime's background reasoner, and the measurement program needs
// it to drive any Realtime-compatible endpoint - including this one - through
// the same wire the field uses. A client written against the protocol is also
// the only honest way to test that the protocol is what we say it is.
package realtimeclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/coder/websocket"
)

// Config configures a connection.
type Config struct {
	// URL is the WebSocket endpoint, ws:// or wss://.
	URL string
	// Token is the bearer credential. Empty sends no Authorization header.
	Token string
	// Model is appended as a query parameter when the URL has none.
	Model string
	// Header carries additional headers, such as a provider's beta opt-in.
	Header http.Header
	// ValidateWire checks inbound server events against the pinned schema.
	// It is off by default here: a client that refuses to talk to a slightly
	// non-conformant server is less useful than one that reports the problem,
	// and the conformance suite is where strictness belongs.
	ValidateWire bool
	// ReadLimit bounds one inbound message. Zero selects 8 MiB, which is
	// generous enough for a large audio delta.
	ReadLimit int64
	// DialTimeout bounds connection establishment.
	DialTimeout time.Duration
	// WriteTimeout bounds one send on an established connection. Zero selects
	// the shipped default; a negative value removes the bound.
	//
	// Send takes a context, so a caller can always bound its own call - but
	// the callers that matter here hold a session-scoped socket and pass the
	// session's context, which ends when the conversation does. A peer that
	// stops reading then blocks the send with nothing left to end it, and
	// because Send holds the write lock, every later send blocks behind the
	// first. The upstream binding forwards a caller's audio through this, so
	// what stops is the conversation, with no error anywhere to say why.
	//
	// Thirty seconds rather than a cadence: this client carries whole Realtime
	// events to a Realtime server rather than frames to one provider, and the
	// bound only has to be past any stall a congested link produces and short
	// of forever. A caller whose traffic has a tighter cadence sets its own.
	WriteTimeout time.Duration
	// LifetimeContext optionally owns the established connection's receive
	// loop independently from the Dial call's setup context. Most callers leave
	// it nil. Measurement drivers use it when a task deadline must stop behavior
	// before closing a session-scoped inspection capability needed for terminal
	// evidence collection.
	LifetimeContext context.Context
	// EventAliases renames inbound server events onto the names this project
	// reads, which are OpenAI's current ones.
	//
	// Several endpoints implement the Realtime protocol as it stood before
	// OpenAI renamed response.audio.delta to response.output_audio.delta at
	// general availability. That is the entire difference for some of them, so
	// it is a rename table rather than a second client: a vendor whose only
	// deviation is a spelling should not cost an adapter.
	//
	// It renames the delivered event type and leaves the raw bytes untouched,
	// because the raw bytes are what the endpoint actually sent and a caller
	// decoding them should see the truth.
	EventAliases map[string]string
}

// Event is one decoded server event, with its raw bytes retained so a caller
// can decode fields this package does not model.
type Event struct {
	Type string
	Raw  []byte
}

// Decode unmarshals the raw event into value.
func (event Event) Decode(value any) error { return json.Unmarshal(event.Raw, value) }

// Field returns one top-level field as a string, which is enough for the
// handful of routing decisions a caller usually needs.
func (event Event) Field(name string) string {
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(event.Raw, &decoded); err != nil {
		return ""
	}
	raw, exists := decoded[name]
	if !exists {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return string(raw)
	}
	return text
}

// Client is one connection to a Realtime endpoint.
type Client struct {
	connection *websocket.Conn
	config     Config
	validator  *protocol.Validator

	events chan Event
	closed atomic.Bool

	writeMu sync.Mutex
	readErr atomic.Pointer[error]
	done    chan struct{}
	once    sync.Once
}

// Dial connects and starts reading.
func Dial(ctx context.Context, config Config) (*Client, error) {
	if strings.TrimSpace(config.URL) == "" {
		return nil, errors.New("a Realtime client requires a URL")
	}
	if config.ReadLimit <= 0 {
		config.ReadLimit = 8 << 20
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = 30 * time.Second
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = 30 * time.Second
	}
	url := config.URL
	if strings.TrimSpace(config.Model) != "" && !strings.Contains(url, "model=") {
		separator := "?"
		if strings.Contains(url, "?") {
			separator = "&"
		}
		url += separator + "model=" + config.Model
	}
	header := http.Header{}
	for name, values := range config.Header {
		for _, value := range values {
			header.Add(name, value)
		}
	}
	if strings.TrimSpace(config.Token) != "" {
		header.Set("Authorization", "Bearer "+config.Token)
	}
	dialContext, cancel := context.WithTimeout(ctx, config.DialTimeout)
	defer cancel()
	connection, _, err := websocket.Dial(dialContext, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, fmt.Errorf("dial Realtime endpoint: %w", err)
	}
	connection.SetReadLimit(config.ReadLimit)
	config.EventAliases = maps.Clone(config.EventAliases)
	client := &Client{
		connection: connection, config: config, validator: protocol.NewValidator(),
		events: make(chan Event, 256), done: make(chan struct{}),
	}
	readContext := ctx
	if config.LifetimeContext != nil {
		readContext = config.LifetimeContext
	}
	go client.read(readContext)
	return client, nil
}

// Events yields decoded server events until the connection closes.
func (client *Client) Events() <-chan Event { return client.events }

// Err reports why reading stopped, or nil for a clean close.
func (client *Client) Err() error {
	if pointer := client.readErr.Load(); pointer != nil {
		return *pointer
	}
	return nil
}

// Send writes one client event.
func (client *Client) Send(ctx context.Context, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	if client.closed.Load() {
		return errors.New("Realtime client is closed")
	}
	if client.config.WriteTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, client.config.WriteTimeout)
		defer cancel()
	}
	if err := client.connection.Write(ctx, websocket.MessageText, encoded); err != nil {
		return fmt.Errorf("send to the Realtime endpoint: %w", err)
	}
	return nil
}

// Close ends the connection.
func (client *Client) Close() error {
	if client.closed.Swap(true) {
		return nil
	}
	err := client.connection.Close(websocket.StatusNormalClosure, "client closed")
	client.finish()
	return err
}

func (client *Client) finish() {
	client.once.Do(func() { close(client.done) })
}

func (client *Client) read(ctx context.Context) {
	defer close(client.events)
	defer client.finish()
	for {
		messageType, input, err := client.connection.Read(ctx)
		if err != nil {
			if !client.closed.Load() {
				client.readErr.Store(&err)
			}
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(input, &envelope); err != nil {
			continue
		}
		if renamed, aliased := client.config.EventAliases[envelope.Type]; aliased {
			envelope.Type = renamed
		}
		if client.config.ValidateWire && !strings.HasPrefix(envelope.Type, "openrealtime.") {
			message, decodeErr := protocol.Decode(input)
			if decodeErr == nil {
				if validateErr := client.validator.Validate(
					protocol.ProfileRealtime, protocol.DirectionServer, message,
				); validateErr != nil {
					client.readErr.Store(&validateErr)
					return
				}
			}
		}
		select {
		case client.events <- Event{Type: envelope.Type, Raw: append([]byte(nil), input...)}:
		case <-ctx.Done():
			return
		}
	}
}

// Wait blocks until the connection has finished reading.
func (client *Client) Wait() { <-client.done }
