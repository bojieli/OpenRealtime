package deepgram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/coder/websocket"
)

// Deepgram Flux is conversational recognition on /v2/listen. It is not Nova-3
// with another model name: the protocol reports turns rather than segments -
// StartOfTurn, Update, EagerEndOfTurn, TurnResumed, EndOfTurn - and replaces
// Nova-3's interim results, endpointing and Finalize with a turn state machine
// and ForceEndTurn. So it is its own listener behind the same provider
// contract, selected by a flux- model.
const (
	DefaultFluxURL        = "wss://api.deepgram.com/v2/listen"
	DefaultFluxModel      = "flux-general-en"
	FluxMultilingualModel = "flux-general-multi"

	// fluxDefaultEOTThreshold is the service's own default, stated so the
	// eager threshold can be checked against the threshold actually in force.
	fluxDefaultEOTThreshold = 0.7

	fluxNoActiveTurn = "FORCE_END_TURN_NO_ACTIVE_TURN"
)

// fluxSampleRates are the raw PCM rates Flux accepts. A rate outside them is
// refused before dialling, because the service's refusal arrives as a closed
// socket that reads like a network failure.
var fluxSampleRates = []uint32{8_000, 16_000, 24_000, 44_100, 48_000}

// IsFluxModel reports whether a Deepgram model is served by Flux.
func IsFluxModel(model string) bool {
	return strings.HasPrefix(strings.TrimSpace(model), "flux-")
}

// FluxConfig configures one Flux stream. Zero thresholds and timeout leave the
// service defaults: EndOfTurn at 0.7 confidence, no eager events, and a 5 s
// silence backstop.
type FluxConfig struct {
	URL    string
	Model  string
	APIKey string
	// EOTThreshold is the confidence at which Flux reports EndOfTurn, 0.5 to
	// 1.0. 1.0 suppresses the model's own turn ends.
	EOTThreshold float64
	// EagerEOTThreshold enables EagerEndOfTurn and TurnResumed, 0.3 to 0.9,
	// and never above the EndOfTurn threshold in force.
	EagerEOTThreshold float64
	// EOTTimeout ends a turn after this much silence whatever the confidence,
	// 500 ms to 60 s in whole milliseconds.
	EOTTimeout time.Duration
	Keyterms   []string
	// LanguageHints bias flux-general-multi towards these language codes.
	// The English model takes none.
	LanguageHints []string
	Header        http.Header
	DialTimeout   time.Duration
	DrainTimeout  time.Duration
	WriteTimeout  time.Duration
	HTTPClient    *http.Client
}

// FluxListener is one session's Flux stream, reused across utterances.
//
// An utterance here is what the engine's acoustic gate bounds, and a Flux turn
// is what the model decides; they are not the same unit. A person who pauses
// long enough for Flux but not for the gate produces two turns in one
// utterance, so ended turns are settled into the utterance's text and the
// turn in progress is its unstable tail.
type FluxListener struct {
	config     FluxConfig
	descriptor v1.Descriptor

	mu        sync.Mutex
	stream    *fluxStream
	closed    bool
	inputRate uint32
	lastWrite time.Time

	haveFrame        bool
	nextFrameIndex   uint64
	nextSourceSample uint64
	settled          string
	current          string
	turnActive       bool
	eager            bool
	endOfTurn        bool
	confidence       float64
	// forcesOutstanding counts ForceEndTurn messages not yet answered. Each
	// is answered exactly once - a manual EndOfTurn, or a no-active-turn
	// warning when a natural EndOfTurn got there first - and a drain that
	// stopped before its answer would leave it to end the next utterance's.
	forcesOutstanding int
	lastEmittedText   string
	revisionID        uint64
}

type fluxStream struct {
	connection *websocket.Conn
	events     chan fluxEvent
	readErr    chan error
	closed     chan struct{}
	readDone   sync.Once
	stopped    chan struct{}
	stopOnce   sync.Once
}

type fluxEvent struct {
	kind       string
	transcript string
	confidence float64
	trigger    string
	code       string
}

// NewFluxListener validates the configuration without opening a socket; the
// stream is dialled by the first frame.
func NewFluxListener(config FluxConfig) (*FluxListener, error) {
	if config.URL == "" {
		config.URL = DefaultFluxURL
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("Deepgram Flux URL must be absolute")
	}
	if parsed.Scheme != "ws" && parsed.Scheme != "wss" {
		return nil, errors.New("Deepgram Flux URL must use ws or wss")
	}
	if strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/v1/listen") {
		return nil, errors.New("Deepgram Flux is served on /v2/listen, not /v1/listen")
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = DefaultFluxModel
	}
	if !IsFluxModel(config.Model) {
		return nil, fmt.Errorf("Deepgram model %q is not a Flux model", config.Model)
	}
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("Deepgram requires an API key")
	}
	if err := validateFluxTurnDetection(config); err != nil {
		return nil, err
	}
	for index, keyterm := range config.Keyterms {
		if strings.TrimSpace(keyterm) == "" {
			return nil, fmt.Errorf("Deepgram Flux keyterm %d is empty", index)
		}
	}
	if len(config.LanguageHints) > 0 && config.Model != FluxMultilingualModel {
		return nil, fmt.Errorf("Deepgram Flux language hints require %s; %s transcribes English only",
			FluxMultilingualModel, config.Model)
	}
	for index, hint := range config.LanguageHints {
		if strings.TrimSpace(hint) == "" || hint != strings.TrimSpace(hint) {
			return nil, fmt.Errorf("Deepgram Flux language hint %d is not canonical", index)
		}
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
	config.Keyterms = slices.Clone(config.Keyterms)
	config.LanguageHints = slices.Clone(config.LanguageHints)
	config.Header = config.Header.Clone()
	return &FluxListener{
		config: config,
		descriptor: v1.Descriptor{
			Name:    "deepgram-flux/" + config.Model,
			Version: "deepgram-flux-persistent-1",
			Capabilities: v1.Capabilities{
				v1.CapabilityStreamingInput: true,
				v1.CapabilityRevisions:      true,
				v1.CapabilityCancellation:   true,
			},
		},
	}, nil
}

func validateFluxTurnDetection(config FluxConfig) error {
	finite := func(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
	if !finite(config.EOTThreshold) || !finite(config.EagerEOTThreshold) {
		return errors.New("Deepgram Flux thresholds must be finite")
	}
	if config.EOTThreshold != 0 && (config.EOTThreshold < 0.5 || config.EOTThreshold > 1) {
		return fmt.Errorf("Deepgram Flux eot_threshold %v is outside 0.5 to 1.0", config.EOTThreshold)
	}
	if config.EagerEOTThreshold != 0 && (config.EagerEOTThreshold < 0.3 || config.EagerEOTThreshold > 0.9) {
		return fmt.Errorf("Deepgram Flux eager_eot_threshold %v is outside 0.3 to 0.9", config.EagerEOTThreshold)
	}
	inForce := config.EOTThreshold
	if inForce == 0 {
		inForce = fluxDefaultEOTThreshold
	}
	if config.EagerEOTThreshold > inForce {
		return fmt.Errorf("Deepgram Flux eager_eot_threshold %v exceeds the eot_threshold in force, %v",
			config.EagerEOTThreshold, inForce)
	}
	if config.EOTTimeout != 0 {
		if config.EOTTimeout%time.Millisecond != 0 {
			return errors.New("Deepgram Flux eot_timeout must be whole milliseconds")
		}
		if config.EOTTimeout < 500*time.Millisecond || config.EOTTimeout > 60*time.Second {
			return fmt.Errorf("Deepgram Flux eot_timeout %v is outside 500ms to 60s", config.EOTTimeout)
		}
	}
	return nil
}

func (listener *FluxListener) Descriptor() v1.Descriptor {
	descriptor := listener.descriptor
	capabilities := make(v1.Capabilities, len(descriptor.Capabilities))
	for capability, enabled := range descriptor.Capabilities {
		capabilities[capability] = enabled
	}
	descriptor.Capabilities = capabilities
	return descriptor
}

// PushFrame sends one PCM16LE frame and returns what Flux has reported since
// the previous call.
func (listener *FluxListener) PushFrame(
	ctx context.Context, frame v1.AudioFrame,
) ([]v1.PerceptionRevision, error) {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.closed {
		return nil, errors.New("Deepgram Flux session is closed")
	}
	if err := listener.validateFrame(frame); err != nil {
		return nil, err
	}
	if listener.stream != nil && !listener.haveFrame && listener.stream.dead() {
		// Between utterances the stream may have been dropped; the next
		// utterance's first frame repairs it. Mid-utterance a dead stream is an
		// error, because words are missing.
		listener.shutdown()
	}
	if listener.stream == nil {
		if err := listener.dial(ctx, frame.SampleRateHz); err != nil {
			return nil, err
		}
		listener.inputRate = frame.SampleRateHz
	}
	if err := listener.write(ctx, websocket.MessageBinary, frame.PCM16LE); err != nil {
		return nil, fmt.Errorf("send Deepgram Flux audio: %w", err)
	}
	listener.haveFrame = true
	listener.nextFrameIndex = frame.Index + 1
	listener.nextSourceSample = frame.SampleOffset + uint64(len(frame.PCM16LE)/2)
	listener.applyAvailable()
	return listener.revisionIfChanged(), nil
}

// Finalize ends the utterance. A turn Flux has already ended is final as it
// stands; a turn still open is ended with ForceEndTurn, whose EndOfTurn
// carries the transcript decoded so far without another decode pass.
func (listener *FluxListener) Finalize(
	ctx context.Context, sourceSample uint64,
) (v1.PerceptionRevision, error) {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.closed {
		return v1.PerceptionRevision{}, errors.New("Deepgram Flux session is closed")
	}
	if !listener.haveFrame || listener.stream == nil {
		return v1.PerceptionRevision{}, errors.New("Deepgram Flux cannot finalize an empty utterance")
	}
	if sourceSample != listener.nextSourceSample {
		return v1.PerceptionRevision{}, fmt.Errorf(
			"Deepgram Flux final source sample is %d; expected %d", sourceSample, listener.nextSourceSample)
	}
	if err := listener.endTurn(ctx); err != nil {
		// What was heard before the stream failed is still what the person
		// said. Only an utterance with no words is a failure worth surfacing.
		listener.shutdown()
		if listener.text() == "" {
			listener.resetUtterance()
			return v1.PerceptionRevision{}, err
		}
	}
	text := listener.text()
	revision := listener.revision(text, sourceSample, true)
	confidence := listener.confidence
	listener.resetUtterance()
	listener.confidence = confidence
	return revision, nil
}

// EndUtterance implements api/v1.UtteranceReusable: an utterance that will not
// be finalized is ended at the service and dropped, so its words cannot open
// the next one.
func (listener *FluxListener) EndUtterance() error {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if listener.stream != nil && listener.haveFrame && !listener.stream.dead() {
		ctx, cancel := context.WithTimeout(context.Background(), listener.config.DrainTimeout)
		defer cancel()
		if err := listener.endTurn(ctx); err != nil {
			listener.shutdown()
		}
	}
	listener.resetUtterance()
	return nil
}

// Close releases the stream for a session that ends.
func (listener *FluxListener) Close() error {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	listener.closed = true
	if listener.stream != nil && !listener.stream.dead() {
		ctx, cancel := context.WithTimeout(context.Background(), listener.config.WriteTimeout)
		_ = listener.write(ctx, websocket.MessageText, []byte(`{"type":"CloseStream"}`))
		cancel()
	}
	listener.shutdown()
	return nil
}

// SpeechEndpointed reports that Flux ended the turn in progress: EndOfTurn has
// arrived and no new turn has started since. The cascade reads it the way it
// reads Nova-3's speech_final.
func (listener *FluxListener) SpeechEndpointed() bool {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	return listener.endOfTurn
}

// EagerEndOfTurn reports that Flux is moderately confident the turn is over:
// an EagerEndOfTurn has arrived and neither TurnResumed nor EndOfTurn has
// followed. Unless the turn resumes, the EndOfTurn transcript is exactly the
// transcript at this moment. It is only ever true with an eager threshold set.
func (listener *FluxListener) EagerEndOfTurn() bool {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	return listener.eager
}

// Confidence is the mean word confidence of the latest transcript.
func (listener *FluxListener) Confidence() float64 {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	return listener.confidence
}

func (listener *FluxListener) endTurn(ctx context.Context) error {
	listener.applyAvailable()
	if listener.turnActive {
		if err := listener.write(ctx, websocket.MessageText, []byte(`{"type":"ForceEndTurn"}`)); err != nil {
			return fmt.Errorf("end Deepgram Flux turn: %w", err)
		}
		listener.forcesOutstanding++
	}
	if listener.forcesOutstanding == 0 {
		return nil
	}
	return listener.drainUntilAnswered(ctx)
}

// drainUntilAnswered consumes events until every ForceEndTurn has its answer
// and no turn is open.
func (listener *FluxListener) drainUntilAnswered(ctx context.Context) error {
	current := listener.stream
	deadline := time.NewTimer(listener.config.DrainTimeout)
	defer deadline.Stop()
	for listener.forcesOutstanding > 0 || listener.turnActive {
		select {
		case event := <-current.events:
			listener.apply(event)
		case err := <-current.readErr:
			return err
		case <-current.closed:
			listener.applyAvailable()
			if listener.forcesOutstanding == 0 && !listener.turnActive {
				return nil
			}
			return errors.New("Deepgram Flux closed the stream before ending the turn")
		case <-deadline.C:
			return errors.New("Deepgram Flux did not end the turn within the drain timeout")
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (listener *FluxListener) applyAvailable() {
	if listener.stream == nil {
		return
	}
	for {
		select {
		case event := <-listener.stream.events:
			listener.apply(event)
		default:
			return
		}
	}
}

// apply folds one event into the utterance, following the Flux state machine.
func (listener *FluxListener) apply(event fluxEvent) {
	switch event.kind {
	case "Warning":
		if event.code == fluxNoActiveTurn {
			// The service has no open turn at this point in the stream, and it
			// sends in order: any EndOfTurn that crossed the force arrived first.
			listener.turnActive = false
			if listener.forcesOutstanding > 0 {
				listener.forcesOutstanding--
			}
		}
		return
	case "StartOfTurn":
		listener.turnActive, listener.eager, listener.endOfTurn = true, false, false
		listener.current = event.transcript
	case "Update":
		if listener.turnActive || event.transcript != "" {
			listener.current = event.transcript
		}
	case "EagerEndOfTurn":
		listener.current, listener.eager = event.transcript, true
	case "TurnResumed":
		listener.current, listener.eager = event.transcript, false
	case "EndOfTurn":
		if event.trigger == "manual" && listener.forcesOutstanding > 0 {
			listener.forcesOutstanding--
		}
		listener.settled = joinTranscript(listener.settled, event.transcript)
		listener.current = ""
		listener.turnActive, listener.eager, listener.endOfTurn = false, false, true
	default:
		// The event set is documented as open to additions; one this listener
		// does not know changes no turn state.
		return
	}
	if event.transcript != "" {
		listener.confidence = event.confidence
	}
}

func (listener *FluxListener) text() string {
	return joinTranscript(listener.settled, listener.current)
}

func (listener *FluxListener) revisionIfChanged() []v1.PerceptionRevision {
	text := listener.text()
	if text == listener.lastEmittedText {
		return nil
	}
	return []v1.PerceptionRevision{listener.revision(text, listener.nextSourceSample, false)}
}

// revision exposes ended turns as stable text: a turn Flux has ended is not
// revised. The turn in progress, eager or not, stays unstable, because a
// TurnResumed may still change it.
func (listener *FluxListener) revision(text string, sourceSample uint64, final bool) v1.PerceptionRevision {
	listener.revisionID++
	revision := v1.PerceptionRevision{
		RevisionID: listener.revisionID, SourceSample: sourceSample,
		Delta: textDelta(listener.lastEmittedText, text), Final: final,
	}
	if final {
		revision.StableText = text
	} else {
		revision.StableText = listener.settled
		revision.UnstableText = strings.TrimPrefix(text, listener.settled)
	}
	listener.lastEmittedText = text
	return revision
}

func (listener *FluxListener) resetUtterance() {
	listener.haveFrame = false
	listener.nextFrameIndex, listener.nextSourceSample = 0, 0
	listener.settled, listener.current, listener.lastEmittedText = "", "", ""
	listener.turnActive, listener.eager, listener.endOfTurn = false, false, false
	listener.confidence = 0
}

func (listener *FluxListener) dial(ctx context.Context, sampleRateHz uint32) error {
	if !slices.Contains(fluxSampleRates, sampleRateHz) {
		return fmt.Errorf("Deepgram Flux accepts PCM at 8000, 16000, 24000, 44100 or 48000 Hz, not %d", sampleRateHz)
	}
	target, err := url.Parse(listener.config.URL)
	if err != nil {
		return fmt.Errorf("parse Deepgram Flux URL: %w", err)
	}
	query := target.Query()
	query.Set("model", listener.config.Model)
	query.Set("encoding", "linear16")
	query.Set("sample_rate", strconv.FormatUint(uint64(sampleRateHz), 10))
	if listener.config.EOTThreshold != 0 {
		query.Set("eot_threshold", strconv.FormatFloat(listener.config.EOTThreshold, 'f', -1, 64))
	}
	if listener.config.EagerEOTThreshold != 0 {
		query.Set("eager_eot_threshold", strconv.FormatFloat(listener.config.EagerEOTThreshold, 'f', -1, 64))
	}
	if listener.config.EOTTimeout != 0 {
		query.Set("eot_timeout_ms", strconv.FormatInt(listener.config.EOTTimeout.Milliseconds(), 10))
	}
	for _, keyterm := range listener.config.Keyterms {
		query.Add("keyterm", keyterm)
	}
	for _, hint := range listener.config.LanguageHints {
		query.Add("language_hint", hint)
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
		return fmt.Errorf("dial Deepgram Flux: %w", err)
	}
	connection.SetReadLimit(readLimit)
	current := &fluxStream{
		connection: connection,
		events:     make(chan fluxEvent, 64),
		readErr:    make(chan error, 1),
		closed:     make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	listener.stream = current
	listener.forcesOutstanding = 0
	listener.lastWrite = time.Now()
	// Flux has no KeepAlive message; the service keeps an idle stream open
	// with WebSocket pings, which the reader answers by reading.
	go current.read()
	return nil
}

func (listener *FluxListener) write(ctx context.Context, kind websocket.MessageType, payload []byte) error {
	if listener.config.WriteTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, listener.config.WriteTimeout)
		defer cancel()
	}
	listener.lastWrite = time.Now()
	return listener.stream.connection.Write(ctx, kind, payload)
}

func (listener *FluxListener) shutdown() {
	if listener.stream == nil {
		return
	}
	listener.stream.stop()
	listener.stream = nil
	listener.forcesOutstanding = 0
}

func (listener *FluxListener) validateFrame(frame v1.AudioFrame) error {
	if frame.SampleRateHz == 0 || len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 {
		return errors.New("Deepgram Flux frame requires non-empty even-length PCM16 and a sample rate")
	}
	if uint64(len(frame.PCM16LE)/2) > math.MaxUint64-frame.SampleOffset {
		return errors.New("Deepgram Flux frame end sample overflows")
	}
	if !listener.haveFrame {
		if listener.stream != nil && listener.inputRate != 0 && frame.SampleRateHz != listener.inputRate {
			return fmt.Errorf("Deepgram Flux stream was opened at %d Hz; a new utterance cannot switch to %d Hz",
				listener.inputRate, frame.SampleRateHz)
		}
		return nil
	}
	if frame.Index != listener.nextFrameIndex {
		return fmt.Errorf("Deepgram Flux frame index is %d; expected %d", frame.Index, listener.nextFrameIndex)
	}
	if frame.SampleOffset != listener.nextSourceSample {
		return fmt.Errorf("Deepgram Flux frame sample offset is %d; expected %d", frame.SampleOffset, listener.nextSourceSample)
	}
	if frame.SampleRateHz != listener.inputRate {
		return fmt.Errorf("Deepgram Flux frame sample rate is %d; the stream was opened at %d",
			frame.SampleRateHz, listener.inputRate)
	}
	return nil
}

func (current *fluxStream) dead() bool {
	select {
	case <-current.closed:
		return true
	default:
		return false
	}
}

func (current *fluxStream) stop() {
	current.stopOnce.Do(func() { close(current.stopped) })
	_ = current.connection.Close(websocket.StatusNormalClosure, "")
}

// read is the only goroutine that touches the connection's reader. It decodes
// and forwards; turn state is folded in by the listener, under its lock.
func (current *fluxStream) read() {
	defer current.readDone.Do(func() { close(current.closed) })
	for {
		kind, payload, err := current.connection.Read(context.Background())
		if err != nil {
			if !isCleanClose(err) {
				select {
				case current.readErr <- fmt.Errorf("read Deepgram Flux stream: %w", err):
				default:
				}
			}
			return
		}
		if kind != websocket.MessageText {
			continue
		}
		var message struct {
			Type       string `json:"type"`
			Event      string `json:"event"`
			Transcript string `json:"transcript"`
			Words      []struct {
				Confidence fluxNumber `json:"confidence"`
			} `json:"words"`
			Trigger     string `json:"trigger"`
			Code        string `json:"code"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(payload, &message); err != nil {
			continue
		}
		var event fluxEvent
		switch message.Type {
		case "TurnInfo":
			event = fluxEvent{
				kind: message.Event, transcript: strings.TrimSpace(message.Transcript), trigger: message.Trigger,
			}
			if len(message.Words) > 0 {
				total := 0.0
				for _, word := range message.Words {
					total += float64(word.Confidence)
				}
				event.confidence = total / float64(len(message.Words))
			}
		case "Warning":
			event = fluxEvent{kind: "Warning", code: message.Code}
		case "Error":
			detail := strings.TrimSpace(strings.Trim(message.Code+": "+message.Description, ": "))
			select {
			case current.readErr <- fmt.Errorf("Deepgram Flux reported an error: %s", detail):
			default:
			}
			return
		default:
			continue
		}
		select {
		case current.events <- event:
		case <-current.stopped:
			return
		}
	}
}

// fluxNumber decodes a number Flux documents as a string-typed float but
// sends as a JSON number.
type fluxNumber float64

func (number *fluxNumber) UnmarshalJSON(data []byte) error {
	text := strings.Trim(string(data), `"`)
	if text == "" || text == "null" {
		*number = 0
		return nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	*number = fluxNumber(value)
	return nil
}

func joinTranscript(before, after string) string {
	before, after = strings.TrimSpace(before), strings.TrimSpace(after)
	switch {
	case before == "":
		return after
	case after == "":
		return before
	default:
		return before + " " + after
	}
}
