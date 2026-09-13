// Package deepgram adapts Deepgram's streaming recogniser and its speech
// endpoint to the stable OpenRealtime component API.
//
// Deepgram is here rather than behind the generic transcription adapter for
// one reason: it actually streams. A batch endpoint can only be asked what a
// recording said, so a partial hypothesis costs a full re-transcription;
// Deepgram emits interim results as the audio arrives, which is what the
// stable-partial observation policy was written for.
package deepgram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/coder/websocket"
)

const (
	// DefaultListenURL is Deepgram's streaming recognition endpoint.
	DefaultListenURL = "wss://api.deepgram.com/v1/listen"
	// DefaultListenModel is the general-purpose streaming model. Deepgram
	// also ships a turn-taking model for voice agents; this adapter does its
	// own turn-taking, so the general model is the right default here.
	DefaultListenModel = "nova-3"

	defaultDialTimeout  = 15 * time.Second
	defaultWriteTimeout = 5 * time.Second
	defaultDrain        = 5 * time.Second
	// defaultKeepAlive is how long a persistent stream may go without audio
	// before the adapter says so. Deepgram closes a stream that has been
	// silent for about ten seconds; a KeepAlive every three keeps a pause in
	// the conversation from becoming a reconnect at the next word.
	defaultKeepAlive = 3 * time.Second
	readLimit        = 1 << 20
)

// ListenConfig configures recognition.
//
// The audio sample rate is deliberately absent: it is taken from the first
// frame and declared to Deepgram, so nothing is resampled on the way in. A
// recogniser that resamples before recognising has thrown away information no
// later stage can recover.
type ListenConfig struct {
	// URL is the WebSocket endpoint. Empty selects DefaultListenURL.
	URL string
	// Model is the recognition model.
	Model string
	// APIKey is the Deepgram key. It is sent as an Authorization: Token
	// header rather than in the URL, so it does not reach proxy logs.
	APIKey string
	// Language selects recognition language. Empty leaves Deepgram's service
	// default in force. The model-specific "multi" value is not automatic
	// detection for every language; for example, Nova-3 multi does not include
	// Mandarin, which requires a Chinese locale such as zh-CN.
	Language string
	// InterimResults asks for hypotheses before a segment is finished. It
	// defaults on, because the reason to choose a streaming recogniser is to
	// get text before the speaker stops.
	InterimResults *bool
	// SmartFormat applies Deepgram's punctuation and entity formatting.
	SmartFormat *bool
	// Endpointing is the silence Deepgram's own VAD requires before marking a
	// Results event speech_final. Zero leaves the service default in force.
	Endpointing time.Duration
	// VADEvents asks Deepgram to expose its speech detector. It defaults on;
	// the adapter currently consumes speech_final and retains the explicit
	// switch so deployments can make the wire behavior reproducible.
	VADEvents *bool
	// Keywords biases recognition toward expected vocabulary.
	Keywords []string
	// Keyterms biases Nova-3 toward exact words or phrases. Deepgram accepts
	// this option as a repeated singular keyterm query parameter; keeping it
	// distinct from the legacy weighted keywords option prevents a frozen
	// deployment from silently changing wire semantics.
	Keyterms []string
	// Extra adds query parameters this adapter does not model.
	Extra map[string]string
	// Header adds request headers.
	Header http.Header
	// DialTimeout bounds connection setup.
	DialTimeout time.Duration
	// DrainTimeout bounds how long Finalize waits for Deepgram to flush the
	// results it still owes after the utterance is closed.
	DrainTimeout time.Duration
	// WriteTimeout bounds one send on an established stream. Zero selects the
	// shipped default; a negative value removes the bound.
	//
	// The dial and the drain were bounded and the sends between them were not,
	// which leaves the one call on the hot path unbounded: PushFrame writes a
	// frame every cadence, holding this listener's lock, on a context that
	// comes from the session and therefore has no deadline. A stalled socket
	// there does not slow recognition down, it stops it - and the block
	// propagates back through the observer to the binding and then to the
	// session's own event loop.
	//
	// The bound is this provider's cadence rather than a shared default: a
	// frame of audio is a few kilobytes, and a streaming recogniser that has
	// not accepted one in five seconds is not keeping up with a conversation
	// whatever it does next.
	WriteTimeout time.Duration
	// Persistent keeps one WebSocket open across utterances. Finalize then
	// asks Deepgram to flush the utterance rather than close the stream, the
	// next utterance goes down the same socket, and a KeepAlive is sent while
	// the person pauses. Off, every utterance dials its own stream and
	// Finalize closes it, which is the one-utterance contract this adapter
	// started with.
	//
	// The difference is at the start of every utterance: a dial, a TLS
	// handshake and Deepgram's own warm-up sit between the first frame and
	// the first hypothesis, and on a persistent stream they have already
	// happened.
	Persistent bool
	// KeepAliveInterval is how often a silent persistent stream is kept
	// alive. Zero selects the shipped default.
	KeepAliveInterval time.Duration
	// HTTPClient dials the WebSocket. Empty uses the default client, which is
	// what a test server needs overridden.
	HTTPClient *http.Client
}

// Listener recognises speech over one Deepgram stream. It is safe for
// concurrent use. Not persistent, it represents exactly one utterance, like
// every perception provider here; persistent, it represents one session's
// stream and each utterance in turn.
type Listener struct {
	config     ListenConfig
	descriptor v1.Descriptor

	mu     sync.Mutex
	stream *stream
	// closed says the session is over: nothing dials again.
	closed bool
	// finalized says the one utterance a non-persistent listener represents
	// has ended.
	finalized bool
	inputRate uint32
	lastWrite time.Time

	// Everything below is one utterance's state.
	haveFrame        bool
	nextFrameIndex   uint64
	nextSourceSample uint64
	committed        string
	interim          string
	lastEmittedText  string
	confidence       float64
	revisionID       uint64
	speechEndpointed bool
}

// stream is one WebSocket and the goroutine reading it. A persistent
// listener may own several over its life - one per reconnect - and each has
// its own channels so a late message from a dead stream can never be read as
// the live one's.
type stream struct {
	connection *websocket.Conn
	// results carries revisions from the read goroutine. Nothing but reading
	// happens on that goroutine: the connection answers protocol pings only
	// from inside Read, so a handler that did work there would stall the
	// keepalive and drop a live session.
	results chan transcriptSegment
	readErr chan error
	// closed is closed by the read goroutine when it stops, which is what a
	// closing Finalize waits for.
	closed   chan struct{}
	readDone sync.Once
	// stopped is closed when the stream is abandoned. The reader can be
	// blocked handing over a segment nobody is draining, and closing the
	// connection would not wake it, so it selects on this as well.
	stopped  chan struct{}
	stopOnce sync.Once
}

// transcriptSegment is one recognised span as Deepgram reported it.
type transcriptSegment struct {
	text        string
	confidence  float64
	final       bool
	speechFinal bool
	// fromFinalize marks the results Deepgram flushed in answer to a
	// Finalize message. They end the utterance on a persistent stream the
	// way a closed socket ends it on a one-utterance stream.
	fromFinalize bool
}

// NewListener validates configuration and returns a listener with no stream
// yet; the first frame dials.
func NewListener(config ListenConfig) (*Listener, error) {
	if config.URL == "" {
		config.URL = DefaultListenURL
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("Deepgram listen URL must be absolute")
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, errors.New("Deepgram listen URL must use ws or wss")
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = DefaultListenModel
	}
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("Deepgram requires an API key")
	}
	if config.DialTimeout <= 0 {
		config.DialTimeout = defaultDialTimeout
	}
	if config.DrainTimeout <= 0 {
		config.DrainTimeout = defaultDrain
	}
	if config.WriteTimeout == 0 {
		config.WriteTimeout = defaultWriteTimeout
	}
	if config.KeepAliveInterval <= 0 {
		config.KeepAliveInterval = defaultKeepAlive
	}
	config.Header = config.Header.Clone()
	version := "deepgram-listen-1"
	if config.Persistent {
		version = "deepgram-listen-persistent-1"
	}
	return &Listener{
		config: config,
		descriptor: v1.Descriptor{
			Name:    "deepgram-listen/" + config.Model,
			Version: version,
			Capabilities: v1.Capabilities{
				v1.CapabilityStreamingInput: true,
				v1.CapabilityRevisions:      true,
				v1.CapabilityCancellation:   true,
			},
		},
	}, nil
}

// Descriptor implements api/v1.PerceptionProvider.
func (listener *Listener) Descriptor() v1.Descriptor {
	descriptor := listener.descriptor
	capabilities := make(v1.Capabilities, len(descriptor.Capabilities))
	for capability, enabled := range descriptor.Capabilities {
		capabilities[capability] = enabled
	}
	descriptor.Capabilities = capabilities
	return descriptor
}

// PushFrame sends one contiguous PCM16LE frame and returns whatever Deepgram
// has said since the previous call.
func (listener *Listener) PushFrame(
	ctx context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.closed || listener.finalized {
		return nil, errors.New("Deepgram session is finalized")
	}
	if err := listener.validateFrame(frame); err != nil {
		return nil, err
	}
	if listener.stream != nil && listener.config.Persistent && !listener.haveFrame {
		// Between utterances a persistent stream may have died - the service
		// timed it out, the network dropped - and the first frame of the next
		// utterance is where that is discovered and repaired, silently. Mid-
		// utterance a dead stream is an error, because words are missing.
		if listener.stream.dead() {
			listener.shutdown()
		}
	}
	if listener.stream == nil {
		if err := listener.dial(ctx, frame.SampleRateHz); err != nil {
			return nil, err
		}
		listener.inputRate = frame.SampleRateHz
	}
	if err := listener.write(ctx, websocket.MessageBinary, frame.PCM16LE); err != nil {
		return nil, fmt.Errorf("send Deepgram audio: %w", err)
	}
	listener.haveFrame = true
	listener.nextFrameIndex = frame.Index + 1
	listener.nextSourceSample = frame.SampleOffset + uint64(len(frame.PCM16LE)/2)
	return listener.drainAvailable(), nil
}

// write sends one message with a bound on how long the socket may take it.
//
// The caller's context comes from the session and has no deadline of its own,
// so without this a stalled Deepgram socket blocks the send forever while this
// listener's lock is held.
func (listener *Listener) write(
	ctx context.Context, kind websocket.MessageType, payload []byte,
) error {
	if listener.config.WriteTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, listener.config.WriteTimeout)
		defer cancel()
	}
	listener.lastWrite = time.Now()
	return listener.stream.connection.Write(ctx, kind, payload)
}

// Finalize ends the utterance, waits for the results Deepgram still owes, and
// emits one final revision.
//
// On a one-utterance stream that means closing it. On a persistent stream it
// means asking Deepgram to flush what it has heard and reading until the
// flushed results arrive; the socket stays open and the next utterance uses
// it.
func (listener *Listener) Finalize(
	ctx context.Context, sourceSample uint64,
) (v1.PerceptionRevision, error) {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.closed || listener.finalized {
		return v1.PerceptionRevision{}, errors.New("Deepgram session is finalized")
	}
	if !listener.haveFrame || listener.stream == nil {
		return v1.PerceptionRevision{}, errors.New("Deepgram cannot finalize an empty utterance")
	}
	if sourceSample != listener.nextSourceSample {
		return v1.PerceptionRevision{}, fmt.Errorf(
			"Deepgram final source sample is %d; expected %d", sourceSample, listener.nextSourceSample)
	}
	if !listener.config.Persistent {
		if err := listener.write(ctx, websocket.MessageText, []byte(`{"type":"CloseStream"}`)); err != nil {
			return v1.PerceptionRevision{}, fmt.Errorf("close Deepgram stream: %w", err)
		}
		if err := listener.drainUntilClosed(ctx); err != nil {
			return v1.PerceptionRevision{}, err
		}
		listener.finalized = true
		text := strings.TrimSpace(listener.committed + listener.interim)
		revision := listener.revision(text, sourceSample, true)
		listener.shutdown()
		return revision, nil
	}
	if err := listener.write(ctx, websocket.MessageText, []byte(`{"type":"Finalize"}`)); err != nil {
		return v1.PerceptionRevision{}, fmt.Errorf("finalize Deepgram utterance: %w", err)
	}
	if err := listener.drainUntilFinalized(ctx); err != nil {
		// The stream failed under the utterance. What was heard before it
		// failed is still what the person said; report it, and let the next
		// utterance repair the stream. Only an utterance with no words at
		// all is a failure worth surfacing.
		listener.shutdown()
		if strings.TrimSpace(listener.committed+listener.interim) == "" {
			listener.resetUtterance()
			return v1.PerceptionRevision{}, err
		}
	}
	text := strings.TrimSpace(listener.committed + listener.interim)
	revision := listener.revision(text, sourceSample, true)
	// The final is the transcript this stream now exposes, so its
	// confidence stays readable until the next utterance's first result
	// replaces it: the language mux compares the lanes' finals by it.
	confidence := listener.confidence
	listener.resetUtterance()
	listener.confidence = confidence
	return revision, nil
}

// EndUtterance implements api/v1.UtteranceReusable. It retires an utterance
// that will not be finalized - the speaker was cut off, the turn was
// discarded - without giving up the stream. Whatever Deepgram still holds for
// it is flushed and dropped, so it cannot surface as the opening words of the
// next utterance.
func (listener *Listener) EndUtterance() error {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if !listener.config.Persistent {
		listener.finalized = true
		listener.shutdown()
		return nil
	}
	if listener.stream != nil && listener.haveFrame && !listener.stream.dead() {
		ctx, cancel := context.WithTimeout(context.Background(), listener.config.DrainTimeout)
		defer cancel()
		if err := listener.write(ctx, websocket.MessageText, []byte(`{"type":"Finalize"}`)); err == nil {
			if err := listener.drainUntilFinalized(ctx); err != nil {
				listener.shutdown()
			}
		} else {
			listener.shutdown()
		}
	}
	listener.resetUtterance()
	return nil
}

// resetUtterance forgets one utterance and keeps the stream.
func (listener *Listener) resetUtterance() {
	listener.haveFrame = false
	listener.nextFrameIndex, listener.nextSourceSample = 0, 0
	listener.committed, listener.interim, listener.lastEmittedText = "", "", ""
	listener.confidence = 0
	listener.speechEndpointed = false
}

// Close releases the stream for a session that ends.
//
// It exists because a session can end without an endpoint - the caller hangs
// up, the process stops - and a streaming recogniser holds a socket and a
// goroutine that nothing else will reclaim. The audio observer closes the
// provider it drops for exactly this reason.
func (listener *Listener) Close() error {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	listener.closed = true
	listener.finalized = true
	if listener.stream != nil && listener.config.Persistent && !listener.stream.dead() {
		ctx, cancel := context.WithTimeout(context.Background(), listener.config.WriteTimeout)
		_ = listener.write(ctx, websocket.MessageText, []byte(`{"type":"CloseStream"}`))
		cancel()
	}
	listener.shutdown()
	return nil
}

// shutdown ends the stream and wakes a reader blocked handing over a segment.
// Closing the connection alone would not: the reader is not inside Read at
// that moment, it is waiting for a consumer that has gone away.
func (listener *Listener) shutdown() {
	current := listener.stream
	if current == nil {
		return
	}
	current.stopOnce.Do(func() { close(current.stopped) })
	_ = current.connection.Close(websocket.StatusNormalClosure, "stream complete")
	listener.stream = nil
}

// dead reports that the read goroutine has stopped, which is the only way a
// stream fails: Deepgram closed it, the network dropped it, or it reported an
// error.
func (current *stream) dead() bool {
	select {
	case <-current.closed:
		return true
	default:
		return false
	}
}

// dial opens the stream and declares the audio format the caller is sending.
func (listener *Listener) dial(ctx context.Context, sampleRateHz uint32) error {
	target, err := url.Parse(listener.config.URL)
	if err != nil {
		return fmt.Errorf("parse Deepgram listen URL: %w", err)
	}
	query := target.Query()
	query.Set("model", listener.config.Model)
	query.Set("encoding", "linear16")
	query.Set("sample_rate", strconv.FormatUint(uint64(sampleRateHz), 10))
	query.Set("channels", "1")
	query.Set("interim_results", boolText(listener.config.InterimResults, true))
	query.Set("smart_format", boolText(listener.config.SmartFormat, true))
	query.Set("vad_events", boolText(listener.config.VADEvents, true))
	if listener.config.Endpointing > 0 {
		query.Set("endpointing", strconv.FormatInt(listener.config.Endpointing.Milliseconds(), 10))
	}
	if listener.config.Language != "" {
		query.Set("language", listener.config.Language)
	}
	for _, keyword := range listener.config.Keywords {
		query.Add("keywords", keyword)
	}
	for _, keyterm := range listener.config.Keyterms {
		query.Add("keyterm", keyterm)
	}
	for name, value := range listener.config.Extra {
		query.Set(name, value)
	}
	target.RawQuery = query.Encode()

	header := listener.config.Header.Clone()
	if header == nil {
		header = http.Header{}
	}
	header.Set("Authorization", "Token "+listener.config.APIKey)
	dialContext, cancel := context.WithTimeout(ctx, listener.config.DialTimeout)
	defer cancel()
	connection, _, err := websocket.Dial(dialContext, target.String(), &websocket.DialOptions{
		HTTPHeader: header, HTTPClient: listener.config.HTTPClient,
	})
	if err != nil {
		return fmt.Errorf("dial Deepgram: %w", err)
	}
	connection.SetReadLimit(readLimit)
	current := &stream{
		connection: connection,
		results:    make(chan transcriptSegment, 64),
		readErr:    make(chan error, 1),
		closed:     make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	listener.stream = current
	listener.lastWrite = time.Now()
	go current.read()
	if listener.config.Persistent {
		go listener.keepAlive(current)
	}
	return nil
}

// keepAlive tells Deepgram the stream is still wanted while nobody is
// speaking. It runs for one stream and stops with it.
func (listener *Listener) keepAlive(current *stream) {
	ticker := time.NewTicker(listener.config.KeepAliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-current.stopped:
			return
		case <-current.closed:
			return
		case <-ticker.C:
		}
		listener.mu.Lock()
		if listener.stream != current || time.Since(listener.lastWrite) < listener.config.KeepAliveInterval {
			listener.mu.Unlock()
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), listener.config.WriteTimeout)
		_ = listener.write(ctx, websocket.MessageText, []byte(`{"type":"KeepAlive"}`))
		cancel()
		listener.mu.Unlock()
	}
}

// read is the only goroutine that touches the connection's reader.
func (current *stream) read() {
	defer current.readDone.Do(func() { close(current.closed) })
	for {
		kind, payload, err := current.connection.Read(context.Background())
		if err != nil {
			if !isCleanClose(err) {
				select {
				case current.readErr <- fmt.Errorf("read Deepgram stream: %w", err):
				default:
				}
			}
			return
		}
		if kind != websocket.MessageText {
			continue
		}
		var envelope struct {
			Type         string `json:"type"`
			IsFinal      bool   `json:"is_final"`
			SpeechFinal  bool   `json:"speech_final"`
			FromFinalize bool   `json:"from_finalize"`
			Channel      struct {
				Alternatives []struct {
					Transcript string  `json:"transcript"`
					Confidence float64 `json:"confidence"`
				} `json:"alternatives"`
			} `json:"channel"`
			Error       string `json:"error"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			continue
		}
		switch envelope.Type {
		case "Error":
			detail := envelope.Description
			if detail == "" {
				detail = envelope.Error
			}
			select {
			case current.readErr <- fmt.Errorf("Deepgram reported an error: %s", detail):
			default:
			}
			return
		case "Results":
			if len(envelope.Channel.Alternatives) == 0 {
				continue
			}
			segment := transcriptSegment{
				text:       envelope.Channel.Alternatives[0].Transcript,
				confidence: envelope.Channel.Alternatives[0].Confidence,
				final:      envelope.IsFinal, speechFinal: envelope.SpeechFinal,
				fromFinalize: envelope.FromFinalize,
			}
			// An interim result with nothing in it is Deepgram saying it has
			// not decided yet, not that the speaker said nothing. Forwarding
			// it would blank the partial transcript mid-utterance.
			if segment.text == "" && !segment.final {
				continue
			}
			select {
			case current.results <- segment:
			case <-current.stopped:
				return
			}
		}
	}
}

// drainAvailable folds every segment received so far into the transcript.
func (listener *Listener) drainAvailable() []v1.PerceptionRevision {
	changed := false
	for {
		select {
		case segment := <-listener.stream.results:
			listener.apply(segment)
			changed = true
			continue
		default:
		}
		break
	}
	if !changed {
		return nil
	}
	text := strings.TrimSpace(listener.committed + listener.interim)
	if text == listener.lastEmittedText {
		return nil
	}
	return []v1.PerceptionRevision{listener.revision(text, listener.nextSourceSample, false)}
}

// drainUntilClosed consumes results until Deepgram closes the stream.
func (listener *Listener) drainUntilClosed(ctx context.Context) error {
	current := listener.stream
	deadline := time.NewTimer(listener.config.DrainTimeout)
	defer deadline.Stop()
	for {
		select {
		case segment := <-current.results:
			listener.apply(segment)
		case err := <-current.readErr:
			return err
		case <-current.closed:
			// The reader has stopped, but buffered segments may still be
			// queued ahead of it. Take them before deciding the transcript.
			for {
				select {
				case segment := <-current.results:
					listener.apply(segment)
					continue
				default:
				}
				break
			}
			select {
			case err := <-current.readErr:
				return err
			default:
			}
			return nil
		case <-deadline.C:
			return errors.New("Deepgram did not finish the utterance within the drain timeout")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// drainUntilFinalized consumes results until the ones Deepgram flushed for
// the Finalize message have arrived. The stream stays open.
func (listener *Listener) drainUntilFinalized(ctx context.Context) error {
	current := listener.stream
	deadline := time.NewTimer(listener.config.DrainTimeout)
	defer deadline.Stop()
	for {
		select {
		case segment := <-current.results:
			listener.apply(segment)
			if segment.fromFinalize {
				return nil
			}
		case err := <-current.readErr:
			return err
		case <-current.closed:
			return errors.New("Deepgram closed the stream before flushing the utterance")
		case <-deadline.C:
			return errors.New("Deepgram did not flush the utterance within the drain timeout")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// apply folds one segment into the committed and interim transcript.
//
// Deepgram reports a finished span once with is_final, then moves on. So a
// final segment appends and clears the interim tail, and an interim segment
// replaces the tail without touching what is already settled.
func (listener *Listener) apply(segment transcriptSegment) {
	if segment.speechFinal {
		listener.speechEndpointed = true
	}
	if segment.final {
		if strings.TrimSpace(segment.text) != "" {
			listener.confidence = segment.confidence
			if listener.committed != "" {
				listener.committed += " "
			}
			listener.committed += strings.TrimSpace(segment.text)
		}
		listener.interim = ""
		return
	}
	if strings.TrimSpace(segment.text) != "" {
		listener.confidence = segment.confidence
	}
	listener.interim = " " + strings.TrimSpace(segment.text)
}

// Confidence is Deepgram's confidence in the transcript currently exposed by
// this stream. It is retained across an empty endpoint marker because that
// marker closes the preceding text; it does not replace it with a hypothesis
// that the speaker said nothing.
func (listener *Listener) Confidence() float64 {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	return listener.confidence
}

// SpeechEndpointed reports that Deepgram's own VAD marked the current
// utterance complete. It is an optional capability consumed by the
// event-aware cascade path; the existing acoustic endpoint path never asks.
func (listener *Listener) SpeechEndpointed() bool {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	return listener.speechEndpointed
}

func (listener *Listener) revision(text string, sourceSample uint64, final bool) v1.PerceptionRevision {
	listener.revisionID++
	revision := v1.PerceptionRevision{
		RevisionID: listener.revisionID, SourceSample: sourceSample,
		Delta: textDelta(listener.lastEmittedText, text), Final: final,
	}
	if final {
		revision.StableText = text
	} else {
		// Deepgram's is_final settles a segment even while the utterance as a
		// whole remains open. Expose that settled prefix as stable evidence and
		// leave only the current hypothesis in the unstable tail. Treating the
		// whole live transcript as unstable threw away information the service
		// had explicitly committed to and left stable-partial policies with
		// nothing they were permitted to read.
		revision.StableText = strings.TrimSpace(listener.committed)
		revision.UnstableText = strings.TrimPrefix(text, revision.StableText)
	}
	listener.lastEmittedText = text
	return revision
}

func (listener *Listener) validateFrame(frame v1.AudioFrame) error {
	if frame.SampleRateHz == 0 || len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 {
		return errors.New("Deepgram frame requires non-empty even-length PCM16 and a sample rate")
	}
	if uint64(len(frame.PCM16LE)/2) > math.MaxUint64-frame.SampleOffset {
		return errors.New("Deepgram frame end sample overflows")
	}
	if !listener.haveFrame {
		if listener.stream != nil && listener.inputRate != 0 && frame.SampleRateHz != listener.inputRate {
			return fmt.Errorf("Deepgram stream was opened at %d Hz; a new utterance cannot switch to %d Hz",
				listener.inputRate, frame.SampleRateHz)
		}
		return nil
	}
	if frame.Index != listener.nextFrameIndex {
		return fmt.Errorf("Deepgram frame index is %d; expected %d", frame.Index, listener.nextFrameIndex)
	}
	if frame.SampleOffset != listener.nextSourceSample {
		return fmt.Errorf("Deepgram frame sample offset is %d; expected %d", frame.SampleOffset, listener.nextSourceSample)
	}
	if frame.SampleRateHz != listener.inputRate {
		return fmt.Errorf("Deepgram frame sample rate is %d; the stream was opened at %d", frame.SampleRateHz, listener.inputRate)
	}
	return nil
}

func textDelta(previous, current string) string {
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	return current
}

func boolText(value *bool, fallback bool) string {
	if value != nil {
		fallback = *value
	}
	return strconv.FormatBool(fallback)
}

func isCleanClose(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}
