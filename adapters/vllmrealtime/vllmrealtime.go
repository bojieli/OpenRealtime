// Package vllmrealtime adapts vLLM's realtime transcription WebSocket
// (/v1/realtime) to the stable OpenRealtime perception interface.
//
// vLLM serves natively streaming recognisers on that route - Voxtral Mini 4B
// Realtime and Qwen3-ASR among them - by feeding audio into one running
// generation and returning text deltas as the decoder emits them. The route is
// append-only: a delta is never withdrawn, so every word it reports is already
// committed. The adapter therefore reports all text as StableText, and names
// that as a property of this serving route (decoder-policy commitment) rather
// than as a claim about the checkpoint.
//
// One adapter is one utterance and one socket. The socket opens on the first
// frame and closes after Finalize.
package vllmrealtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/pcm"
)

const (
	// DefaultURL is the realtime route of a local vLLM server. The port is the
	// one the duplex deployment profiles give Voxtral; the room's :8001 stays
	// the start/chunk/finish Qwen service.
	DefaultURL = "ws://127.0.0.1:9101/v1/realtime"
	// DefaultModel is the served model name the deployment profiles use.
	DefaultModel = "voxtral-realtime"

	serverSampleRate       = uint32(16_000)
	defaultDialTimeout     = 10 * time.Second
	defaultFinalizeTimeout = 10 * time.Second
	readLimit              = 1 << 20
)

// Config configures one realtime transcription utterance.
type Config struct {
	URL             string
	Model           string
	BearerToken     string
	Header          http.Header
	HTTPClient      *http.Client
	DialTimeout     time.Duration
	FinalizeTimeout time.Duration
}

// Adapter owns one realtime transcription socket for one utterance.
type Adapter struct {
	config     Config
	descriptor v1.Descriptor

	mu               sync.Mutex
	connection       *websocket.Conn
	events           chan event
	stop             chan struct{}
	resampler        *pcm.Resampler
	inputRate        uint32
	haveFrame        bool
	nextFrameIndex   uint64
	nextSourceSample uint64
	text             string
	lastEmittedText  string
	revisionID       uint64
	finalized        bool
	terminalErr      error
}

type event struct {
	kind  string
	delta string
	text  string
	err   string
}

// New validates the configuration without opening a socket.
func New(config Config) (*Adapter, error) {
	if config.URL == "" {
		config.URL = DefaultURL
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Host == "" {
		return nil, errors.New("vLLM realtime URL must be absolute")
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, errors.New("vLLM realtime URL must use ws or wss")
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.DialTimeout < 0 || config.FinalizeTimeout < 0 {
		return nil, errors.New("vLLM realtime timeouts cannot be negative")
	}
	if config.DialTimeout == 0 {
		config.DialTimeout = defaultDialTimeout
	}
	if config.FinalizeTimeout == 0 {
		config.FinalizeTimeout = defaultFinalizeTimeout
	}
	config.Header = config.Header.Clone()
	config.BearerToken = strings.TrimSpace(config.BearerToken)
	descriptor := v1.Descriptor{
		Name:    "vllm-realtime/" + config.Model,
		Version: "vllm-realtime-transcription-1",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true,
			v1.CapabilityCancellation:   true,
		},
	}
	return &Adapter{config: config, descriptor: descriptor}, nil
}

// Descriptor implements api/v1.PerceptionProvider.
func (adapter *Adapter) Descriptor() v1.Descriptor {
	descriptor := adapter.descriptor
	capabilities := make(v1.Capabilities, len(descriptor.Capabilities))
	for capability, enabled := range descriptor.Capabilities {
		capabilities[capability] = enabled
	}
	descriptor.Capabilities = capabilities
	return descriptor
}

// PushFrame resamples a frame to 16 kHz, appends it to the running generation,
// and returns a revision when the decoder has emitted new text.
func (adapter *Adapter) PushFrame(ctx context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := adapter.ready(); err != nil {
		return nil, err
	}
	if err := adapter.validateFrame(frame); err != nil {
		return nil, err
	}
	if adapter.connection == nil {
		if err := adapter.dial(ctx); err != nil {
			return nil, adapter.fail(err)
		}
	}
	if adapter.resampler == nil {
		resampler, err := pcm.NewResampler(frame.SampleRateHz, serverSampleRate)
		if err != nil {
			return nil, err
		}
		adapter.resampler = resampler
		adapter.inputRate = frame.SampleRateHz
	}
	converted, err := adapter.resampler.Push(frame.PCM16LE)
	if err != nil {
		return nil, adapter.fail(fmt.Errorf("resample vLLM realtime frame: %w", err))
	}
	endSample := frame.SampleOffset + uint64(len(frame.PCM16LE)/2)
	adapter.haveFrame = true
	adapter.nextFrameIndex = frame.Index + 1
	adapter.nextSourceSample = endSample
	if len(converted) != 0 {
		if err := adapter.append(ctx, converted); err != nil {
			return nil, adapter.fail(err)
		}
	}
	if err := adapter.drain(); err != nil {
		return nil, adapter.fail(err)
	}
	if adapter.text == adapter.lastEmittedText {
		return nil, nil
	}
	return []v1.PerceptionRevision{adapter.revision(adapter.text, endSample, false)}, nil
}

// Finalize ends the audio, waits for the decoder to finish the delayed tail,
// and returns one final revision.
func (adapter *Adapter) Finalize(ctx context.Context, sourceSample uint64) (v1.PerceptionRevision, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if err := adapter.ready(); err != nil {
		return v1.PerceptionRevision{}, err
	}
	if !adapter.haveFrame || adapter.connection == nil {
		return v1.PerceptionRevision{}, errors.New("vLLM realtime cannot finalize an empty utterance")
	}
	if sourceSample != adapter.nextSourceSample {
		return v1.PerceptionRevision{}, fmt.Errorf(
			"vLLM realtime final source sample is %d; expected %d", sourceSample, adapter.nextSourceSample)
	}
	converted, err := adapter.resampler.Finalize()
	if err != nil {
		return v1.PerceptionRevision{}, adapter.fail(fmt.Errorf("finalize vLLM realtime resampler: %w", err))
	}
	if len(converted) != 0 {
		if err := adapter.append(ctx, converted); err != nil {
			return v1.PerceptionRevision{}, adapter.fail(err)
		}
	}
	if err := adapter.send(ctx, map[string]any{"type": "input_audio_buffer.commit", "final": true}); err != nil {
		return v1.PerceptionRevision{}, adapter.fail(err)
	}
	waitContext, cancel := context.WithTimeout(ctx, adapter.config.FinalizeTimeout)
	defer cancel()
	for {
		select {
		case <-waitContext.Done():
			adapter.closeSocket()
			return v1.PerceptionRevision{}, adapter.fail(fmt.Errorf("vLLM realtime transcription did not finish: %w", waitContext.Err()))
		case current, ok := <-adapter.events:
			if !ok {
				adapter.closeSocket()
				return v1.PerceptionRevision{}, adapter.fail(errors.New("vLLM realtime socket closed before transcription.done"))
			}
			if err := adapter.apply(current); err != nil {
				adapter.closeSocket()
				return v1.PerceptionRevision{}, adapter.fail(err)
			}
			if current.kind == "transcription.done" {
				adapter.finalized = true
				adapter.closeSocket()
				return adapter.revision(adapter.text, sourceSample, true), nil
			}
		}
	}
}

// Close releases the socket of an utterance that never finalized.
func (adapter *Adapter) Close() error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	adapter.closeSocket()
	return nil
}

func (adapter *Adapter) dial(ctx context.Context) error {
	header := adapter.config.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	if adapter.config.BearerToken != "" {
		header.Set("Authorization", "Bearer "+adapter.config.BearerToken)
	}
	dialContext, cancel := context.WithTimeout(ctx, adapter.config.DialTimeout)
	defer cancel()
	connection, _, err := websocket.Dial(dialContext, adapter.config.URL, &websocket.DialOptions{
		HTTPHeader: header, HTTPClient: adapter.config.HTTPClient,
	})
	if err != nil {
		return fmt.Errorf("dial vLLM realtime: %w", err)
	}
	connection.SetReadLimit(readLimit)
	adapter.connection = connection
	adapter.events = make(chan event, 256)
	adapter.stop = make(chan struct{})
	go read(connection, adapter.events, adapter.stop)
	// The model is validated by session.update; the first commit starts the
	// one generation every later append feeds.
	if err := adapter.send(ctx, map[string]any{"type": "session.update", "model": adapter.config.Model}); err != nil {
		return err
	}
	return adapter.send(ctx, map[string]any{"type": "input_audio_buffer.commit"})
}

func (adapter *Adapter) append(ctx context.Context, pcm16 []byte) error {
	return adapter.send(ctx, map[string]any{
		"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(pcm16),
	})
}

func (adapter *Adapter) send(ctx context.Context, message map[string]any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if err := adapter.connection.Write(ctx, websocket.MessageText, payload); err != nil {
		return fmt.Errorf("send vLLM realtime %v: %w", message["type"], err)
	}
	return nil
}

// drain folds every event already received into the transcript without
// waiting for more.
func (adapter *Adapter) drain() error {
	for {
		select {
		case current, ok := <-adapter.events:
			if !ok {
				return errors.New("vLLM realtime socket closed mid-utterance")
			}
			if err := adapter.apply(current); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}

func (adapter *Adapter) apply(current event) error {
	switch current.kind {
	case "transcription.delta":
		adapter.text += current.delta
	case "transcription.done":
		// The done text is the whole generation; prefer it over the sum of
		// deltas only when it extends what was already reported, so a word
		// already committed is never withdrawn.
		if strings.HasPrefix(current.text, adapter.text) {
			adapter.text = current.text
		}
	case "error":
		return fmt.Errorf("vLLM realtime error: %s", current.err)
	}
	return nil
}

func (adapter *Adapter) revision(text string, sourceSample uint64, final bool) v1.PerceptionRevision {
	adapter.revisionID++
	delta := text
	if strings.HasPrefix(text, adapter.lastEmittedText) {
		delta = text[len(adapter.lastEmittedText):]
	}
	adapter.lastEmittedText = text
	return v1.PerceptionRevision{
		RevisionID: adapter.revisionID, SourceSample: sourceSample,
		StableText: strings.TrimSpace(text), Delta: delta, Final: final,
	}
}

func (adapter *Adapter) ready() error {
	if adapter.terminalErr != nil {
		return adapter.terminalErr
	}
	if adapter.finalized {
		return errors.New("vLLM realtime utterance is already finalized")
	}
	return nil
}

func (adapter *Adapter) fail(err error) error {
	adapter.terminalErr = err
	return err
}

func (adapter *Adapter) closeSocket() {
	if adapter.connection != nil {
		close(adapter.stop)
		_ = adapter.connection.CloseNow()
		adapter.connection = nil
	}
}

func (adapter *Adapter) validateFrame(frame v1.AudioFrame) error {
	if frame.SampleRateHz == 0 || len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 {
		return errors.New("vLLM realtime frame requires non-empty even-length PCM16 and a sample rate")
	}
	if uint64(len(frame.PCM16LE)/2) > math.MaxUint64-frame.SampleOffset {
		return errors.New("vLLM realtime frame end sample overflows")
	}
	if !adapter.haveFrame {
		return nil
	}
	if frame.Index != adapter.nextFrameIndex {
		return fmt.Errorf("vLLM realtime frame index is %d; expected %d", frame.Index, adapter.nextFrameIndex)
	}
	if frame.SampleOffset != adapter.nextSourceSample {
		return fmt.Errorf("vLLM realtime frame sample offset is %d; expected %d", frame.SampleOffset, adapter.nextSourceSample)
	}
	if frame.SampleRateHz != adapter.inputRate {
		return fmt.Errorf("vLLM realtime frame sample rate is %d; the utterance began at %d", frame.SampleRateHz, adapter.inputRate)
	}
	return nil
}

// read is the only goroutine reading the socket. It decodes and forwards; the
// adapter folds events in under its own lock.
func read(connection *websocket.Conn, events chan<- event, stop <-chan struct{}) {
	defer close(events)
	for {
		kind, payload, err := connection.Read(context.Background())
		if err != nil {
			return
		}
		if kind != websocket.MessageText {
			continue
		}
		var message struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Text  string `json:"text"`
			Error string `json:"error"`
		}
		if json.Unmarshal(payload, &message) != nil {
			continue
		}
		switch message.Type {
		case "transcription.delta", "transcription.done", "error":
			select {
			case events <- event{kind: message.Type, delta: message.Delta, text: message.Text, err: message.Error}:
			case <-stop:
				return
			}
		}
	}
}

var _ v1.PerceptionProvider = (*Adapter)(nil)
