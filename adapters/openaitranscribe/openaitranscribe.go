// Package openaitranscribe adapts a batch transcription endpoint to the
// stable OpenRealtime perception interface.
//
// One endpoint shape - multipart POST of an audio file, JSON with a `text`
// field back - covers OpenAI, Groq, Fireworks, SiliconFlow, Mistral, and every
// local whisper server that copied it, plus ElevenLabs with two field names
// changed. That is most of the recognition market for the cost of one adapter.
//
// What it cannot do is stream. A batch endpoint has no notion of a partial
// hypothesis, so this adapter is honest about it: by default it recognises
// once, at the endpoint of the utterance, and emits exactly one final
// revision. A deployment that wants earlier text sets a partial interval and
// pays for it in re-transcribed audio, because re-sending the utterance is the
// only way a batch endpoint can produce a partial at all. Those speculative
// requests run one at a time off the audio path: a slow partial is coalesced
// rather than delaying VAD, and terminal recognition replaces any older
// snapshot. For genuinely streaming recognition, use a provider that streams
// - the Deepgram adapter, or the local Qwen3-ASR service.
package openaitranscribe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/internal/httpclient"
	"github.com/bojieli/OpenRealtime/pcm"
)

const (
	// DefaultBaseURL is OpenAI's own API root.
	DefaultBaseURL = "https://api.openai.com/v1"
	// DefaultPath is the OpenAI transcription route.
	DefaultPath = "/audio/transcriptions"
	// DefaultModel is a transcription model every compatible endpoint has
	// served at some point. It is a starting point, not a recommendation.
	DefaultModel = "whisper-1"
	// DefaultSampleRateHz is what the audio is resampled to before it is
	// sent. Recognition models are trained at 16 kHz and sending more costs
	// upload time for no accuracy.
	DefaultSampleRateHz = uint32(16_000)

	defaultTimeout      = 60 * time.Second
	defaultMaxUtterance = 32 << 20
	maxResponseBody     = 1 << 20
	maxErrorBody        = 1 << 20
)

// Config configures one transcription utterance session.
type Config struct {
	BaseURL string
	// Path is the route under BaseURL. Empty selects DefaultPath.
	Path   string
	Model  string
	APIKey string
	// AuthHeader names the header the credential goes in. Empty selects
	// Authorization with a Bearer prefix.
	AuthHeader string
	// ModelField names the multipart field carrying the model. Empty selects
	// "model"; ElevenLabs calls it "model_id".
	ModelField string
	// Provider identifies the serving stack in descriptors. It should name
	// the vendor rather than pretending to be OpenAI.
	Provider string
	// Language is an optional BCP-47 hint. Empty lets the model decide.
	Language string
	// Prompt biases recognition toward expected vocabulary, where the
	// endpoint supports it.
	Prompt string
	// SampleRateHz is the rate audio is uploaded at.
	SampleRateHz uint32
	// PartialInterval asks for a partial hypothesis roughly this often, by
	// re-transcribing everything heard so far. Zero, the default, recognises
	// only at the endpoint.
	PartialInterval time.Duration
	// ExtraFields adds provider-specific multipart fields.
	ExtraFields       map[string]string
	Headers           http.Header
	HTTPClient        *http.Client
	RequestTimeout    time.Duration
	MaxUtteranceBytes int
}

// Adapter owns one utterance. Create a new one for every concurrent stream.
type Adapter struct {
	config     Config
	descriptor v1.Descriptor
	endpoint   string

	mu               sync.Mutex
	buffer           []byte
	resampler        *pcm.Resampler
	inputRate        uint32
	haveFrame        bool
	nextFrameIndex   uint64
	nextSourceSample uint64
	partialAtSamples int
	partialCount     int
	partialDone      chan partialResult
	partialCancel    context.CancelFunc
	lastEmittedText  string
	language         string
	revisionID       uint64
	finalized        bool
}

type partialResult struct {
	text         string
	language     string
	sourceSample uint64
	err          error
}

// New validates configuration and returns a fresh single-utterance adapter.
func New(config Config) (*Adapter, error) {
	if config.BaseURL == "" {
		config.BaseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("transcription base URL must be absolute")
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	if config.Path == "" {
		config.Path = DefaultPath
	}
	if !strings.HasPrefix(config.Path, "/") {
		config.Path = "/" + config.Path
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.ModelField == "" {
		config.ModelField = "model"
	}
	if config.AuthHeader == "" {
		config.AuthHeader = "Authorization"
	}
	config.Provider = strings.TrimSpace(config.Provider)
	if config.Provider == "" {
		config.Provider = "openai-transcriptions"
	}
	if config.SampleRateHz == 0 {
		config.SampleRateHz = DefaultSampleRateHz
	}
	if config.SampleRateHz > 384_000 {
		return nil, errors.New("transcription sample rate is implausible")
	}
	if config.PartialInterval < 0 {
		return nil, errors.New("transcription partial interval cannot be negative")
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("transcription request timeout cannot be negative")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultTimeout
	}
	if config.MaxUtteranceBytes < 0 {
		return nil, errors.New("transcription utterance limit cannot be negative")
	}
	if config.MaxUtteranceBytes == 0 {
		config.MaxUtteranceBytes = defaultMaxUtterance
	}
	if config.HTTPClient == nil {
		config.HTTPClient = httpclient.Shared()
	}
	config.ExtraFields = maps.Clone(config.ExtraFields)
	config.Headers = config.Headers.Clone()
	config.APIKey = strings.TrimSpace(config.APIKey)

	adapter := &Adapter{
		config:   config,
		endpoint: config.BaseURL + config.Path,
		descriptor: v1.Descriptor{
			Name:    config.Provider + "-transcriptions/" + config.Model,
			Version: "openai-audio-transcriptions-1",
			Capabilities: v1.Capabilities{
				v1.CapabilityStreamingInput: true,
				v1.CapabilityRevisions:      config.PartialInterval > 0,
				v1.CapabilityCancellation:   true,
			},
		},
	}
	if config.PartialInterval > 0 {
		adapter.partialAtSamples = int(float64(config.SampleRateHz) * config.PartialInterval.Seconds())
		if adapter.partialAtSamples < 1 {
			adapter.partialAtSamples = 1
		}
	}
	return adapter, nil
}

// Descriptor implements api/v1.PerceptionProvider.
func (adapter *Adapter) Descriptor() v1.Descriptor {
	descriptor := adapter.descriptor
	descriptor.Capabilities = maps.Clone(descriptor.Capabilities)
	return descriptor
}

// Language returns the most recent endpoint-reported language.
func (adapter *Adapter) Language() string {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.language
}

// PushFrame buffers a contiguous PCM16LE frame and, when a partial interval is
// configured and enough new audio has arrived, re-transcribes what has been
// heard so far.
func (adapter *Adapter) PushFrame(ctx context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.finalized {
		return nil, errors.New("transcription session is finalized")
	}
	revisions, err := adapter.collectPartial()
	if err != nil {
		return nil, err
	}
	if err := adapter.validateFrame(frame); err != nil {
		return nil, err
	}
	if adapter.resampler == nil {
		resampler, err := pcm.NewResampler(frame.SampleRateHz, adapter.config.SampleRateHz)
		if err != nil {
			return nil, err
		}
		adapter.resampler = resampler
		adapter.inputRate = frame.SampleRateHz
	}
	converted, err := adapter.resampler.Push(frame.PCM16LE)
	if err != nil {
		return nil, fmt.Errorf("resample transcription frame: %w", err)
	}
	if len(adapter.buffer)+len(converted) > adapter.config.MaxUtteranceBytes {
		return nil, fmt.Errorf("transcription utterance exceeds %d bytes", adapter.config.MaxUtteranceBytes)
	}
	adapter.buffer = append(adapter.buffer, converted...)
	adapter.haveFrame = true
	adapter.nextFrameIndex = frame.Index + 1
	adapter.nextSourceSample = frame.SampleOffset + uint64(len(frame.PCM16LE)/2)

	if adapter.partialAtSamples == 0 || len(adapter.buffer) == 0 {
		return revisions, nil
	}
	// A partial costs a full re-transcription of the utterance, so its base
	// schedule is measured in buffered audio rather than wall clock. A provider
	// that keeps up is asked at the same points in every recording; a slow one
	// coalesces the decode opportunities that arrived while it was busy.
	pending := len(adapter.buffer)/2 - adapter.partialCount*adapter.partialAtSamples
	if pending < adapter.partialAtSamples || adapter.partialDone != nil {
		return revisions, nil
	}
	adapter.startPartial(ctx)
	// This snapshot covers every interval currently buffered. If the previous
	// request was slower than the configured cadence, the intermediate
	// snapshots are already obsolete; counting only one here would replay that
	// backlog one full-utterance transcription at a time.
	adapter.partialCount = len(adapter.buffer) / 2 / adapter.partialAtSamples
	return revisions, nil
}

// Finalize transcribes the whole utterance and emits one final revision.
func (adapter *Adapter) Finalize(ctx context.Context, sourceSample uint64) (v1.PerceptionRevision, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.finalized {
		return v1.PerceptionRevision{}, errors.New("transcription session is finalized")
	}
	if !adapter.haveFrame || adapter.resampler == nil {
		return v1.PerceptionRevision{}, errors.New("transcription cannot finalize an empty utterance")
	}
	if sourceSample != adapter.nextSourceSample {
		return v1.PerceptionRevision{}, fmt.Errorf(
			"transcription final source sample is %d; expected %d", sourceSample, adapter.nextSourceSample)
	}
	// A partial is speculative. Once the acoustic endpoint is known, waiting
	// for an older snapshot before asking for the terminal transcript makes the
	// endpoint pay for obsolete work and delays every downstream response.
	// Cancel it first; the final request below contains all admitted audio.
	if err := adapter.stopPartial(); err != nil {
		return v1.PerceptionRevision{}, err
	}
	converted, err := adapter.resampler.Finalize()
	if err != nil {
		return v1.PerceptionRevision{}, fmt.Errorf("finalize transcription resampler: %w", err)
	}
	adapter.buffer = append(adapter.buffer, converted...)
	if len(adapter.buffer) == 0 {
		return v1.PerceptionRevision{}, errors.New("transcription cannot finalize an empty utterance")
	}
	text, language, err := adapter.transcribe(ctx, adapter.buffer)
	if err != nil {
		return v1.PerceptionRevision{}, err
	}
	adapter.language = language
	adapter.finalized = true
	return adapter.revision(text, sourceSample, true), nil
}

// Close cancels a speculative partial when an utterance is abandoned before
// its endpoint. The adapter owns asynchronous partial requests, so it also
// owns their lifetime rather than borrowing the lifetime of one PushFrame
// call.
func (adapter *Adapter) Close() error {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	err := adapter.stopPartial()
	adapter.finalized = true
	return err
}

// startPartial re-transcribes a snapshot without holding up audio ingestion.
//
// A batch transcription endpoint cannot truly stream. When partials are
// enabled, every hypothesis is a remote transcription of the growing buffer;
// doing that synchronously makes provider latency block the acoustic gate
// itself. Audio arriving during the request must keep advancing the gate, and
// a later frame collects the result if it is still useful. At most one request
// is in flight, so a slow endpoint coalesces stale decode opportunities rather
// than building an unbounded request queue.
func (adapter *Adapter) startPartial(ctx context.Context) {
	// PushFrame is commonly called from an event handler whose context is
	// canceled as soon as that handler returns. The speculative request is
	// adapter-owned work and deliberately outlives that one call; retain its
	// values while bounding it with the adapter's request timeout and explicit
	// cancellation from Finalize or Close.
	requestContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan partialResult, 1)
	samples := slices.Clone(adapter.buffer)
	sourceSample := adapter.nextSourceSample
	adapter.partialDone, adapter.partialCancel = done, cancel
	go func() {
		text, language, err := adapter.transcribe(requestContext, samples)
		done <- partialResult{
			text: text, language: language, sourceSample: sourceSample, err: err,
		}
	}()
}

// collectPartial publishes a completed partial at the next provider advance.
// It never waits on the realtime input path.
func (adapter *Adapter) collectPartial() ([]v1.PerceptionRevision, error) {
	if adapter.partialDone == nil {
		return nil, nil
	}
	var result partialResult
	select {
	case result = <-adapter.partialDone:
	default:
		return nil, nil
	}
	adapter.partialDone = nil
	adapter.partialCancel = nil
	if result.err != nil {
		return nil, result.err
	}
	adapter.language = result.language
	if result.text == adapter.lastEmittedText {
		return nil, nil
	}
	return []v1.PerceptionRevision{adapter.revision(result.text, result.sourceSample, false)}, nil
}

// stopPartial removes work for a superseded snapshot before terminal
// recognition or cleanup. A request that already failed still reports its
// failure; cancellation initiated here is expected and is replaced by the
// final request.
func (adapter *Adapter) stopPartial() error {
	if adapter.partialDone == nil {
		return nil
	}
	adapter.partialCancel()
	result := <-adapter.partialDone
	adapter.partialDone = nil
	adapter.partialCancel = nil
	if result.err != nil && !errors.Is(result.err, context.Canceled) {
		return result.err
	}
	return nil
}

func (adapter *Adapter) validateFrame(frame v1.AudioFrame) error {
	if frame.SampleRateHz == 0 || len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 {
		return errors.New("transcription frame requires non-empty even-length PCM16 and a sample rate")
	}
	if uint64(len(frame.PCM16LE)/2) > math.MaxUint64-frame.SampleOffset {
		return errors.New("transcription frame end sample overflows")
	}
	if !adapter.haveFrame {
		return nil
	}
	if frame.Index != adapter.nextFrameIndex {
		return fmt.Errorf("transcription frame index is %d; expected %d", frame.Index, adapter.nextFrameIndex)
	}
	if frame.SampleOffset != adapter.nextSourceSample {
		return fmt.Errorf("transcription frame starts at sample %d; expected %d",
			frame.SampleOffset, adapter.nextSourceSample)
	}
	if frame.SampleRateHz != adapter.inputRate {
		return fmt.Errorf("transcription sample rate changed from %d to %d",
			adapter.inputRate, frame.SampleRateHz)
	}
	return nil
}

func (adapter *Adapter) revision(text string, sourceSample uint64, final bool) v1.PerceptionRevision {
	adapter.revisionID++
	revision := v1.PerceptionRevision{
		RevisionID: adapter.revisionID, SourceSample: sourceSample,
		Delta: textDelta(adapter.lastEmittedText, text), Final: final,
	}
	if final {
		revision.StableText = text
	} else {
		// The part this revision and the one before it agree on. A partial is
		// a whole re-transcription of the buffer, so the recogniser revises
		// its own tail constantly - re-punctuating, re-spelling a name, taking
		// a word back - while the front of the sentence stops moving almost at
		// once. That settled front is what "stable" means, and until this
		// computed it every partial was reported as entirely unstable and
		// anything downstream that asked what had been committed got nothing.
		revision.StableText, revision.UnstableText = settled(adapter.lastEmittedText, text)
	}
	adapter.lastEmittedText = text
	return revision
}

// settled splits text into the part the previous revision already agreed with
// and the part that is new or has changed.
//
// Compared as words, because a recogniser re-punctuates and re-capitalises
// what it has already given you: "a warm afternoon and I" becomes "a warm
// afternoon. And I" and back again, and compared as characters neither is a
// prefix of the other.
func settled(previous, current string) (stable, unstable string) {
	// The unit of agreement is the script's, not the format's. Chinese,
	// Japanese and Thai are written without spaces between words, so splitting
	// on whitespace returns the whole utterance as one token and it agrees
	// with the previous revision only when nothing changed at all. Stable is
	// then empty on every partial of every sentence, and a caller that falls
	// back to the whole revision when stable is empty - which is what the
	// interjection path does - acts on precisely the unsettled tail this
	// exists to hold back.
	//
	// It costs more than a missing optimisation, because a truncated syllable
	// in these scripts is usually another real word rather than a visibly
	// broken one. Measured while interpreting Mandarin, a partial ending
	// part-way through 很高兴 - "very pleased" - was transcribed 高雄, the city
	// Kaohsiung, and the voice translated that faithfully. In English the same
	// truncation gives "thir-", which is nothing, and the damage never
	// appears.
	if withoutWordSpacing(current) {
		return settledByCharacter(previous, current)
	}
	was, now := strings.Fields(previous), strings.Fields(current)
	agreed := 0
	for agreed < len(was) && agreed < len(now) &&
		strings.EqualFold(strings.Trim(was[agreed], ".,!?;:"), strings.Trim(now[agreed], ".,!?;:")) {
		agreed++
	}
	if agreed == 0 {
		return "", current
	}
	stable = strings.Join(now[:agreed], " ")
	if agreed == len(now) {
		return stable, ""
	}
	return stable, " " + strings.Join(now[agreed:], " ")
}

// withoutWordSpacing reports whether the text is in a script that does not put
// spaces between words. Tested on what is present rather than what is absent:
// a single English word has no spaces either, and splitting that by character
// would report half a word as settled, which is the opposite of the point.
func withoutWordSpacing(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Thai, r) {
			return true
		}
	}
	return false
}

// settledByCharacter agrees on the longest common prefix, counted in
// characters. In a script written without spaces the character is the unit a
// recogniser revises, so it is the unit agreement is measured in.
func settledByCharacter(previous, current string) (stable, unstable string) {
	was, now := []rune(previous), []rune(current)
	agreed := 0
	for agreed < len(was) && agreed < len(now) && was[agreed] == now[agreed] {
		agreed++
	}
	// The last character of the agreed run is the one still being decided:
	// a recogniser that has heard 很高 may yet write 很高兴 or 高雄, and the
	// difference is the character it has committed to least. Holding one back
	// costs a character of latency and buys not translating a word nobody said.
	if agreed > 0 && agreed < len(now) {
		agreed--
	}
	if agreed == 0 {
		return "", current
	}
	if agreed == len(now) {
		return string(now), ""
	}
	return string(now[:agreed]), string(now[agreed:])
}

func textDelta(previous, current string) string {
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	return current
}

type transcriptionResponse struct {
	Text     string `json:"text"`
	Language string `json:"language"`
}

// transcribe uploads the buffered utterance and returns the recognised text.
func (adapter *Adapter) transcribe(ctx context.Context, samples []byte) (string, string, error) {
	container, err := audio.EncodeWAVMono16(samples, adapter.config.SampleRateHz)
	if err != nil {
		return "", "", fmt.Errorf("encode transcription upload: %w", err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="utterance.wav"`)
	header.Set("Content-Type", "audio/wav")
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", "", fmt.Errorf("create transcription upload: %w", err)
	}
	if _, err := part.Write(container); err != nil {
		return "", "", fmt.Errorf("write transcription upload: %w", err)
	}
	fields := map[string]string{adapter.config.ModelField: adapter.config.Model}
	if adapter.config.Language != "" {
		fields["language"] = adapter.config.Language
	}
	if adapter.config.Prompt != "" {
		fields["prompt"] = adapter.config.Prompt
	}
	for name, value := range adapter.config.ExtraFields {
		fields[name] = value
	}
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			return "", "", fmt.Errorf("write transcription field %q: %w", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		return "", "", fmt.Errorf("close transcription upload: %w", err)
	}

	requestContext := ctx
	if adapter.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		requestContext, cancel = context.WithTimeout(ctx, adapter.config.RequestTimeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, adapter.endpoint, &body)
	if err != nil {
		return "", "", fmt.Errorf("create transcription request: %w", err)
	}
	for name, values := range adapter.config.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Accept", "application/json")
	if adapter.config.APIKey != "" {
		if strings.EqualFold(adapter.config.AuthHeader, "Authorization") {
			request.Header.Set("Authorization", "Bearer "+adapter.config.APIKey)
		} else {
			request.Header.Set(adapter.config.AuthHeader, adapter.config.APIKey)
		}
	}
	response, err := adapter.config.HTTPClient.Do(request)
	if err != nil {
		return "", "", fmt.Errorf("send transcription request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return "", "", fmt.Errorf("transcription endpoint returned HTTP %d: %s",
			response.StatusCode, strings.TrimSpace(string(message)))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
	if err != nil {
		return "", "", fmt.Errorf("read transcription response: %w", err)
	}
	var decoded transcriptionResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", "", fmt.Errorf("decode transcription response: %w", err)
	}
	return strings.TrimSpace(decoded.Text), decoded.Language, nil
}
