// Package speechsocket speaks openrealtime-incremental-speech/1, the
// same-context synthesis WebSocket served by the duplex-plan TTS services
// (tools/duplexmodels/common.py documents the wire contract).
//
// Two layers are exported. Context is the incremental synthesis session the
// streaming plan asks for: open a context, append text while audio is already
// being produced, flush without ending, end, or cancel - with accepted-text
// and generated-audio positions kept separately. Adapter is an ordinary
// api/v1 StreamingSpeechProvider built on it, one context per plan, so any
// such service can replace a complete-text synthesiser without further
// runtime changes.
//
// A service states how it consumes text in context.ready (incremental_text,
// input_granularity). The adapter reports that declaration in its descriptor
// rather than assuming every model conditions on a growing prefix.
package speechsocket

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/internal/speechstream"
)

const (
	// DefaultURL is the CosyVoice service of the duplex deployment profiles.
	DefaultURL = "ws://127.0.0.1:9120/v1/tts/stream"
	// DefaultModel names the service's model in descriptors; the server
	// chooses the model at start-up.
	DefaultModel = "Fun-CosyVoice3-0.5B"
	// Protocol is the wire contract this package speaks.
	Protocol = "openrealtime-incremental-speech/1"

	defaultOutputRate  = uint32(24_000)
	defaultDialTimeout = 10 * time.Second
	defaultIdle        = 30 * time.Second
	readLimit          = 16 << 20
)

// CapabilityIncrementalText is reported when the service declares that it
// conditions on text appended while a generation is running.
const CapabilityIncrementalText v1.Capability = "incremental_text_input"

// Capabilities is what a service declares in context.ready.
type Capabilities struct {
	IncrementalText  bool   `json:"incremental_text"`
	NonterminalFlush bool   `json:"nonterminal_flush"`
	InputGranularity string `json:"input_granularity"`
}

// Config configures a speech socket client.
type Config struct {
	URL          string
	Model        string
	Voice        string
	BearerToken  string
	Header       http.Header
	HTTPClient   *http.Client
	OutputRateHz uint32
	DialTimeout  time.Duration
	// IdleTimeout bounds the wait for the next server event once a context
	// has been ended; a service that stops answering fails the utterance
	// rather than holding it forever.
	IdleTimeout time.Duration
}

// Chunk is one piece of generated audio at the service's rate.
type Chunk struct {
	Sequence int
	PCM16LE  []byte
}

// Context is one incremental synthesis context on its own socket.
type Context struct {
	id           string
	connection   *websocket.Conn
	sampleRate   uint32
	model        string
	capabilities Capabilities

	audio     chan Chunk
	done      chan struct{}
	accepted  atomic.Int64
	sent      atomic.Int64
	lastEvent atomic.Int64
	idle      time.Duration
	ended     atomic.Bool
	stalled   atomic.Bool
	stopped   chan struct{}

	mu        sync.Mutex
	finishErr error
	cancelled bool
	closeOnce sync.Once
}

var contextCounter atomic.Uint64

// Open dials the service and opens one context. The context is ready when
// Open returns: its sample rate and declared capabilities are known.
func Open(ctx context.Context, config Config) (*Context, error) {
	config, err := normalise(config)
	if err != nil {
		return nil, err
	}
	header := config.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	if config.BearerToken != "" {
		header.Set("Authorization", "Bearer "+config.BearerToken)
	}
	dialContext, cancel := context.WithTimeout(ctx, config.DialTimeout)
	defer cancel()
	connection, _, err := websocket.Dial(dialContext, config.URL, &websocket.DialOptions{
		HTTPHeader: header, HTTPClient: config.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("dial speech socket: %w", err)
	}
	connection.SetReadLimit(readLimit)
	speech := &Context{
		id:         fmt.Sprintf("ctx-%d", contextCounter.Add(1)),
		connection: connection,
		audio:      make(chan Chunk, 256),
		done:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	if err := speech.send(ctx, map[string]any{"type": "context.open", "context_id": speech.id, "voice": config.Voice}); err != nil {
		_ = connection.CloseNow()
		return nil, err
	}
	readyContext, cancelReady := context.WithTimeout(ctx, config.DialTimeout)
	defer cancelReady()
	for {
		message, err := readMessage(readyContext, connection)
		if err != nil {
			_ = connection.CloseNow()
			return nil, fmt.Errorf("await speech context: %w", err)
		}
		if message.Type == "error" {
			_ = connection.CloseNow()
			return nil, fmt.Errorf("speech socket refused the context: %s", message.Message)
		}
		if message.Type != "context.ready" || message.ContextID != speech.id {
			continue
		}
		if message.SampleRate == 0 {
			_ = connection.CloseNow()
			return nil, errors.New("speech socket declared no sample rate")
		}
		speech.sampleRate = uint32(message.SampleRate)
		speech.model = message.Model
		speech.capabilities = message.Capabilities
		break
	}
	speech.idle = config.IdleTimeout
	speech.lastEvent.Store(time.Now().UnixNano())
	go speech.read()
	return speech, nil
}

// SampleRate is the rate of every chunk this context produces.
func (speech *Context) SampleRate() uint32 { return speech.sampleRate }

// Capabilities is what the service declared for this context.
func (speech *Context) Capabilities() Capabilities { return speech.capabilities }

// Model is the model the service reported, when it did.
func (speech *Context) Model() string { return speech.model }

// Audio delivers generated audio in order. It is closed after audio.done,
// cancellation, or failure; Err then says which.
func (speech *Context) Audio() <-chan Chunk { return speech.audio }

// Accepted is the cumulative number of characters the service has accepted.
func (speech *Context) Accepted() int64 { return speech.accepted.Load() }

// Sent is the cumulative number of characters appended by this client.
func (speech *Context) Sent() int64 { return speech.sent.Load() }

// Append adds text to the running context.
func (speech *Context) Append(ctx context.Context, text string) error {
	if text == "" {
		return nil
	}
	speech.sent.Add(int64(len([]rune(text))))
	return speech.send(ctx, map[string]any{"type": "text.append", "context_id": speech.id, "text": text})
}

// Flush asks the service to synthesise what it has buffered without ending
// the context.
func (speech *Context) Flush(ctx context.Context) error {
	return speech.send(ctx, map[string]any{"type": "text.flush", "context_id": speech.id})
}

// End says no more text will be appended; audio continues until audio.done.
// From here on a service that goes silent for longer than the idle timeout
// fails the context instead of holding it open.
func (speech *Context) End(ctx context.Context) error {
	if err := speech.send(ctx, map[string]any{"type": "text.end", "context_id": speech.id}); err != nil {
		return err
	}
	if speech.ended.CompareAndSwap(false, true) {
		speech.lastEvent.Store(time.Now().UnixNano())
		go speech.watch()
	}
	return nil
}

// watch closes a context whose service stopped answering after End. A read
// deadline cannot do this: the WebSocket library closes the connection when a
// read's context expires, so the bound is enforced from outside the reader.
func (speech *Context) watch() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-speech.done:
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, speech.lastEvent.Load())) > speech.idle {
				speech.stalled.Store(true)
				speech.Close()
				return
			}
		}
	}
}

// Cancel stops the context. Audio already queued on this side is discarded:
// nothing is delivered on Audio after Cancel returns.
func (speech *Context) Cancel(ctx context.Context) error {
	speech.mu.Lock()
	speech.cancelled = true
	speech.mu.Unlock()
	err := speech.send(ctx, map[string]any{"type": "context.cancel", "context_id": speech.id})
	speech.Close()
	return err
}

// Close releases the socket.
func (speech *Context) Close() {
	speech.closeOnce.Do(func() {
		close(speech.stopped)
		_ = speech.connection.CloseNow()
	})
}

// Err reports why Audio closed: nil after audio.done, context.Canceled after
// Cancel, or the failure.
func (speech *Context) Err() error {
	<-speech.done
	speech.mu.Lock()
	defer speech.mu.Unlock()
	return speech.finishErr
}

func (speech *Context) send(ctx context.Context, message map[string]any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err := speech.connection.Write(ctx, websocket.MessageText, payload); err != nil {
		return fmt.Errorf("send %v: %w", message["type"], err)
	}
	return nil
}

func (speech *Context) finish(err error) {
	speech.mu.Lock()
	if speech.cancelled {
		err = context.Canceled
	}
	speech.finishErr = err
	speech.mu.Unlock()
	close(speech.audio)
	close(speech.done)
	speech.Close()
}

func (speech *Context) read() {
	for {
		message, err := readMessage(context.Background(), speech.connection)
		if err != nil {
			if speech.stalled.Load() {
				err = fmt.Errorf("speech socket sent nothing for %s after text.end", speech.idle)
			}
			speech.finish(fmt.Errorf("speech socket closed before audio.done: %w", err))
			return
		}
		speech.lastEvent.Store(time.Now().UnixNano())
		if message.ContextID != "" && message.ContextID != speech.id {
			continue
		}
		switch message.Type {
		case "text.accepted":
			speech.accepted.Store(message.Chars)
		case "audio":
			pcm, err := base64.StdEncoding.DecodeString(message.PCM16)
			if err != nil || len(pcm)%2 != 0 {
				speech.finish(errors.New("speech socket sent invalid PCM16"))
				return
			}
			speech.mu.Lock()
			cancelled := speech.cancelled
			speech.mu.Unlock()
			if cancelled {
				continue
			}
			select {
			case speech.audio <- Chunk{Sequence: message.Seq, PCM16LE: pcm}:
			case <-speech.stopped:
				// A closed context delivers nothing further; the reader
				// only has to see the socket fail to finish.
			}
		case "audio.done":
			speech.finish(nil)
			return
		case "context.cancelled":
			speech.finish(context.Canceled)
			return
		case "error":
			speech.finish(fmt.Errorf("speech socket error: %s", message.Message))
			return
		}
	}
}

type serverMessage struct {
	Type         string       `json:"type"`
	ContextID    string       `json:"context_id"`
	SampleRate   int          `json:"sample_rate"`
	Model        string       `json:"model"`
	Capabilities Capabilities `json:"capabilities"`
	Chars        int64        `json:"chars"`
	Seq          int          `json:"seq"`
	PCM16        string       `json:"pcm16"`
	Message      string       `json:"message"`
}

func readMessage(ctx context.Context, connection *websocket.Conn) (serverMessage, error) {
	for {
		kind, payload, err := connection.Read(ctx)
		if err != nil {
			return serverMessage{}, err
		}
		if kind != websocket.MessageText {
			continue
		}
		var message serverMessage
		if err := json.Unmarshal(payload, &message); err != nil {
			continue
		}
		return message, nil
	}
}

func normalise(config Config) (Config, error) {
	if config.URL == "" {
		config.URL = DefaultURL
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Host == "" {
		return config, errors.New("speech socket URL must be absolute")
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return config, errors.New("speech socket URL must use ws or wss")
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.Voice == "" {
		config.Voice = "default"
	}
	if config.OutputRateHz == 0 {
		config.OutputRateHz = defaultOutputRate
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = defaultDialTimeout
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = defaultIdle
	}
	config.BearerToken = strings.TrimSpace(config.BearerToken)
	config.Header = config.Header.Clone()
	return config, nil
}

// Adapter is a StreamingSpeechProvider over one context per plan.
type Adapter struct {
	config     Config
	descriptor v1.Descriptor
}

// New validates the configuration without opening a socket.
func New(config Config) (*Adapter, error) {
	config, err := normalise(config)
	if err != nil {
		return nil, err
	}
	return &Adapter{config: config, descriptor: v1.Descriptor{
		Name:    "speech-socket/" + config.Model,
		Version: Protocol,
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingOutput: true,
			v1.CapabilityCancellation:    true,
		},
	}}, nil
}

// Descriptor implements api/v1.SpeechProvider.
func (adapter *Adapter) Descriptor() v1.Descriptor {
	descriptor := adapter.descriptor
	capabilities := make(v1.Capabilities, len(descriptor.Capabilities))
	for capability, enabled := range descriptor.Capabilities {
		capabilities[capability] = enabled
	}
	descriptor.Capabilities = capabilities
	return descriptor
}

// Synthesize implements api/v1.SpeechProvider by collecting the stream.
func (adapter *Adapter) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := adapter.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	return chunks, err
}

// Stream implements api/v1.StreamingSpeechProvider: open a context, append
// the plan's text, end it, and emit audio as it arrives. Cancelling ctx
// cancels the context, and nothing generated after that reaches consume.
func (adapter *Adapter) Stream(ctx context.Context, plan v1.SpeechPlan, consume func(v1.SpeechChunk) error) error {
	if consume == nil {
		return v1.ErrNilConsumer
	}
	if strings.TrimSpace(plan.Text) == "" {
		return errors.New("speech socket input is empty")
	}
	speech, err := Open(ctx, adapter.config)
	if err != nil {
		return err
	}
	defer speech.Close()
	if err := speech.Append(ctx, plan.Text); err != nil {
		return err
	}
	if err := speech.End(ctx); err != nil {
		return err
	}
	reader, writer := io.Pipe()
	go func() {
		for {
			select {
			case <-ctx.Done():
				_ = speech.Cancel(context.Background())
				writer.CloseWithError(context.Cause(ctx))
				return
			case chunk, ok := <-speech.Audio():
				if !ok {
					writer.CloseWithError(speech.Err())
					return
				}
				if _, err := writer.Write(chunk.PCM16LE); err != nil {
					_ = speech.Cancel(context.Background())
					return
				}
			}
		}
	}()
	err = speechstream.Consume(reader, speechstream.Options{
		Label: "speech socket", SourceRateHz: speech.SampleRate(), OutputRateHz: adapter.config.OutputRateHz,
	}, plan.CandidateID, consume)
	_ = reader.Close()
	if ctxErr := context.Cause(ctx); ctxErr != nil && ctx.Err() != nil {
		return ctxErr
	}
	return err
}

var _ v1.StreamingSpeechProvider = (*Adapter)(nil)
