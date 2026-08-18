// Package openaitts adapts an OpenAI-compatible /v1/audio/speech endpoint to
// OpenRealtime's stable streaming speech interface.
//
// The adapter uses the raw PCM streaming profile implemented by engines such
// as SGLang-Omni: response_format is "pcm", stream is true, the response body
// is mono PCM16LE, and the source sample rate is declared in X-Sample-Rate.
// Model-specific request fields remain explicit extensions rather than being
// confused with the OpenAI Realtime event protocol.
package openaitts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/pcm"
)

const (
	// DefaultEndpoint is the OpenAI-compatible speech endpoint exposed by a
	// local SGLang-Omni server.
	DefaultEndpoint = "http://127.0.0.1:8080/v1/audio/speech"
	// DefaultModel is the initial accelerated TTS condition.
	DefaultModel = "fishaudio/s2-pro"

	defaultVoice       = "default"
	defaultSourceRate  = uint32(24_000)
	defaultOutputRate  = uint32(24_000)
	defaultTimeout     = 2 * time.Minute
	defaultReadBuffer  = 8 << 10
	defaultMaxAudio    = 128 << 20
	maximumErrorBody   = 1 << 20
	maximumSampleRate  = 384_000
	maximumRequestText = 1 << 20
)

// Reference supplies an in-context voice example. AudioPath may be a path,
// file URL, data URL, or remote URL accepted by the serving engine. Production
// deployments should restrict remote-media origins on the server.
type Reference struct {
	AudioPath string `json:"audio_path"`
	Text      string `json:"text"`
}

// Config configures an OpenAI-compatible streaming speech client. Pointer
// sampling fields distinguish an omitted server default from an explicit zero.
// ExtraBody supports future, provider-specific JSON fields; reserved standard
// fields cannot be overridden.
type Config struct {
	Endpoint           string
	Model              string
	Voice              string
	BearerToken        string
	Headers            http.Header
	HTTPClient         *http.Client
	RequestTimeout     time.Duration
	FallbackSampleRate uint32
	OutputSampleRateHz uint32
	References         []Reference
	Speed              *float64
	MaxNewTokens       *int
	Temperature        *float64
	TopP               *float64
	TopK               *int
	RepetitionPenalty  *float64
	InitialChunkFrames *int
	ExtraBody          map[string]json.RawMessage
	ReadBufferBytes    int
	MaxAudioBytes      int64
}

// Adapter is safe for concurrent use. Each Stream call owns its HTTP request,
// resampler, continuity counters, and terminal-chunk state.
type Adapter struct {
	config     Config
	descriptor v1.Descriptor
}

var reservedRequestFields = map[string]struct{}{
	"model": {}, "input": {}, "voice": {}, "response_format": {}, "stream": {},
	"references": {}, "speed": {}, "max_new_tokens": {}, "temperature": {},
	"top_p": {}, "top_k": {}, "repetition_penalty": {}, "initial_codec_chunk_frames": {},
}

// New validates configuration and takes defensive copies of all mutable data.
func New(config Config) (*Adapter, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("OpenAI TTS endpoint must be absolute")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("OpenAI TTS endpoint must use HTTP or HTTPS")
	}
	if config.Model == "" {
		config.Model = DefaultModel
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		return nil, errors.New("OpenAI TTS model is required")
	}
	if config.Voice == "" {
		config.Voice = defaultVoice
	}
	config.Voice = strings.TrimSpace(config.Voice)
	if config.Voice == "" {
		return nil, errors.New("OpenAI TTS voice is required")
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("OpenAI TTS request timeout cannot be negative")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultTimeout
	}
	if config.FallbackSampleRate == 0 {
		config.FallbackSampleRate = defaultSourceRate
	}
	if config.OutputSampleRateHz == 0 {
		config.OutputSampleRateHz = defaultOutputRate
	}
	if err := validateSampleRate(config.FallbackSampleRate, "fallback"); err != nil {
		return nil, err
	}
	if err := validateSampleRate(config.OutputSampleRateHz, "output"); err != nil {
		return nil, err
	}
	if config.ReadBufferBytes == 0 {
		config.ReadBufferBytes = defaultReadBuffer
	}
	if config.ReadBufferBytes < 2 {
		return nil, errors.New("OpenAI TTS read buffer must contain at least two bytes")
	}
	if config.MaxAudioBytes == 0 {
		config.MaxAudioBytes = defaultMaxAudio
	}
	if config.MaxAudioBytes < 2 {
		return nil, errors.New("OpenAI TTS maximum audio size must contain at least one PCM16 sample")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	config.Headers = config.Headers.Clone()
	config.BearerToken = strings.TrimSpace(config.BearerToken)
	config.References = slices.Clone(config.References)
	for index := range config.References {
		config.References[index].AudioPath = strings.TrimSpace(config.References[index].AudioPath)
		config.References[index].Text = strings.TrimSpace(config.References[index].Text)
		if config.References[index].AudioPath == "" || config.References[index].Text == "" {
			return nil, fmt.Errorf("OpenAI TTS reference %d requires audio_path and text", index)
		}
	}
	if err := validateControls(config); err != nil {
		return nil, err
	}
	config.Speed = clonePointer(config.Speed)
	config.MaxNewTokens = clonePointer(config.MaxNewTokens)
	config.Temperature = clonePointer(config.Temperature)
	config.TopP = clonePointer(config.TopP)
	config.TopK = clonePointer(config.TopK)
	config.RepetitionPenalty = clonePointer(config.RepetitionPenalty)
	config.InitialChunkFrames = clonePointer(config.InitialChunkFrames)
	config.ExtraBody = cloneRawMessages(config.ExtraBody)
	for name, value := range config.ExtraBody {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(name) != name {
			return nil, errors.New("OpenAI TTS extension field name must be non-empty and trimmed")
		}
		if _, reserved := reservedRequestFields[name]; reserved {
			return nil, fmt.Errorf("OpenAI TTS extension cannot override reserved field %q", name)
		}
		if !json.Valid(value) {
			return nil, fmt.Errorf("OpenAI TTS extension %q is not valid JSON", name)
		}
	}

	descriptor := v1.Descriptor{
		Name:    "openai-speech-streaming/" + config.Model,
		Version: "openai-audio-speech-1",
		Capabilities: v1.Capabilities{
			v1.CapabilityCancellation:    true,
			v1.CapabilityPCM16Output:     true,
			v1.CapabilityStreamingOutput: true,
		},
	}
	return &Adapter{config: config, descriptor: descriptor}, nil
}

func validateSampleRate(rate uint32, kind string) error {
	if rate == 0 || rate > maximumSampleRate {
		return fmt.Errorf("OpenAI TTS %s sample rate must be between 1 and %d Hz", kind, maximumSampleRate)
	}
	return nil
}

func validateControls(config Config) error {
	if config.Speed != nil && (*config.Speed < 0.25 || *config.Speed > 4) {
		return errors.New("OpenAI TTS speed must be between 0.25 and 4")
	}
	if config.MaxNewTokens != nil && *config.MaxNewTokens <= 0 {
		return errors.New("OpenAI TTS maximum new tokens must be positive")
	}
	if config.Temperature != nil && *config.Temperature < 0 {
		return errors.New("OpenAI TTS temperature cannot be negative")
	}
	if config.TopP != nil && (*config.TopP <= 0 || *config.TopP > 1) {
		return errors.New("OpenAI TTS top-p must be in (0, 1]")
	}
	if config.TopK != nil && *config.TopK < 0 {
		return errors.New("OpenAI TTS top-k cannot be negative")
	}
	if config.RepetitionPenalty != nil && *config.RepetitionPenalty <= 0 {
		return errors.New("OpenAI TTS repetition penalty must be positive")
	}
	if config.InitialChunkFrames != nil && *config.InitialChunkFrames < 0 {
		return errors.New("OpenAI TTS initial codec chunk frames cannot be negative")
	}
	return nil
}

// Descriptor implements api/v1.SpeechProvider.
func (adapter *Adapter) Descriptor() v1.Descriptor {
	descriptor := adapter.descriptor
	descriptor.Capabilities = cloneCapabilities(descriptor.Capabilities)
	return descriptor
}

// Stream implements api/v1.StreamingSpeechProvider. It emits PCM16LE at the
// configured output rate and holds at most one output sample so exactly one
// non-empty terminal chunk can be marked Final.
func (adapter *Adapter) Stream(ctx context.Context, plan v1.SpeechPlan, consume func(v1.SpeechChunk) error) error {
	if consume == nil {
		return v1.ErrNilConsumer
	}
	plan.CandidateID = strings.TrimSpace(plan.CandidateID)
	plan.Text = strings.TrimSpace(plan.Text)
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("OpenAI TTS speech plan: %w", err)
	}
	if len(plan.Text) > maximumRequestText {
		return fmt.Errorf("OpenAI TTS input exceeds %d bytes", maximumRequestText)
	}

	encoded, err := adapter.encodeRequest(plan.Text)
	if err != nil {
		return err
	}
	requestContext := ctx
	if adapter.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		requestContext, cancel = context.WithTimeout(ctx, adapter.config.RequestTimeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, adapter.config.Endpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create OpenAI TTS request: %w", err)
	}
	for name, values := range adapter.config.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "audio/pcm")
	request.Header.Set("User-Agent", "OpenRealtime/openai-tts")
	if adapter.config.BearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+adapter.config.BearerToken)
	}

	response, err := adapter.config.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("send OpenAI TTS request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maximumErrorBody))
		return fmt.Errorf("OpenAI TTS returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := validatePCMContentType(response.Header.Get("Content-Type")); err != nil {
		return err
	}
	sourceRate, err := responseSampleRate(response.Header, adapter.config.FallbackSampleRate)
	if err != nil {
		return err
	}
	return adapter.consumePCM(response.Body, sourceRate, plan.CandidateID, consume)
}

func (adapter *Adapter) encodeRequest(text string) ([]byte, error) {
	request := map[string]any{
		"model": adapter.config.Model, "input": text, "voice": adapter.config.Voice,
		"response_format": "pcm", "stream": true,
	}
	if len(adapter.config.References) != 0 {
		request["references"] = adapter.config.References
	}
	addPointer(request, "speed", adapter.config.Speed)
	addPointer(request, "max_new_tokens", adapter.config.MaxNewTokens)
	addPointer(request, "temperature", adapter.config.Temperature)
	addPointer(request, "top_p", adapter.config.TopP)
	addPointer(request, "top_k", adapter.config.TopK)
	addPointer(request, "repetition_penalty", adapter.config.RepetitionPenalty)
	addPointer(request, "initial_codec_chunk_frames", adapter.config.InitialChunkFrames)
	for name, value := range adapter.config.ExtraBody {
		request[name] = value
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI TTS request: %w", err)
	}
	return encoded, nil
}

func addPointer[T any](target map[string]any, name string, value *T) {
	if value != nil {
		target[name] = *value
	}
}

func validatePCMContentType(value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return fmt.Errorf("OpenAI TTS returned invalid content type %q: %w", value, err)
	}
	if mediaType != "audio/pcm" && mediaType != "application/octet-stream" {
		return fmt.Errorf("OpenAI TTS returned content type %q; expected audio/pcm", mediaType)
	}
	return nil
}

func responseSampleRate(header http.Header, fallback uint32) (uint32, error) {
	value := strings.TrimSpace(header.Get("X-Sample-Rate"))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("OpenAI TTS returned invalid X-Sample-Rate %q", value)
	}
	rate := uint32(parsed)
	if err := validateSampleRate(rate, "source"); err != nil {
		return 0, err
	}
	return rate, nil
}

func (adapter *Adapter) consumePCM(body io.Reader, sourceRate uint32, candidateID string, consume func(v1.SpeechChunk) error) error {
	resampler, err := pcm.NewResampler(sourceRate, adapter.config.OutputSampleRateHz)
	if err != nil {
		return fmt.Errorf("configure OpenAI TTS resampler: %w", err)
	}
	buffer := make([]byte, adapter.config.ReadBufferBytes)
	var oddByte []byte
	var heldSample []byte
	var sourceBytes int64
	var outputBytes int64
	var sampleOffset uint64
	var sequence uint64
	emit := func(audio []byte, final bool) error {
		if len(audio) == 0 || len(audio)%2 != 0 {
			return errors.New("OpenAI TTS produced an invalid PCM16 chunk")
		}
		if int64(len(audio)) > adapter.config.MaxAudioBytes-outputBytes {
			return fmt.Errorf("OpenAI TTS output exceeds %d bytes", adapter.config.MaxAudioBytes)
		}
		sequence++
		chunk := v1.SpeechChunk{
			ChunkID: fmt.Sprintf("%s-%06d", candidateID, sequence), CandidateID: candidateID,
			SampleOffset: sampleOffset, SampleRateHz: adapter.config.OutputSampleRateHz,
			PCM16LE: slices.Clone(audio), Final: final,
		}
		if err := consume(chunk); err != nil {
			return fmt.Errorf("consume OpenAI TTS chunk: %w", err)
		}
		outputBytes += int64(len(audio))
		sampleOffset += uint64(len(audio) / 2)
		return nil
	}
	offer := func(audio []byte) error {
		if len(audio) == 0 {
			return nil
		}
		combined := make([]byte, 0, len(heldSample)+len(audio))
		combined = append(combined, heldSample...)
		combined = append(combined, audio...)
		if len(combined) <= 2 {
			heldSample = combined
			return nil
		}
		cut := len(combined) - 2
		if err := emit(combined[:cut], false); err != nil {
			return err
		}
		heldSample = slices.Clone(combined[cut:])
		return nil
	}
	feed := func(input []byte) error {
		if len(input) == 0 {
			return nil
		}
		if int64(len(input)) > adapter.config.MaxAudioBytes-sourceBytes {
			return fmt.Errorf("OpenAI TTS response exceeds %d bytes", adapter.config.MaxAudioBytes)
		}
		sourceBytes += int64(len(input))
		if len(oddByte) != 0 {
			input = append(append([]byte(nil), oddByte...), input...)
			oddByte = nil
		}
		if len(input)%2 != 0 {
			oddByte = slices.Clone(input[len(input)-1:])
			input = input[:len(input)-1]
		}
		if len(input) == 0 {
			return nil
		}
		converted, err := resampler.Push(input)
		if err != nil {
			return fmt.Errorf("resample OpenAI TTS stream: %w", err)
		}
		return offer(converted)
	}
	for {
		read, readErr := body.Read(buffer)
		if read > 0 {
			if err := feed(buffer[:read]); err != nil {
				return err
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read OpenAI TTS stream: %w", readErr)
			}
			break
		}
	}
	if len(oddByte) != 0 {
		return errors.New("OpenAI TTS stream ended with a partial PCM16 sample")
	}
	if sourceBytes == 0 {
		return errors.New("OpenAI TTS returned no audio")
	}
	terminal, err := resampler.Finalize()
	if err != nil {
		return fmt.Errorf("finalize OpenAI TTS resampler: %w", err)
	}
	if len(terminal) != 0 {
		heldSample = append(heldSample, terminal...)
	}
	if len(heldSample) == 0 {
		return errors.New("OpenAI TTS returned no complete output sample")
	}
	return emit(heldSample, true)
}

// Synthesize collects Stream output for consumers that require the stable
// non-streaming interface. MaxAudioBytes bounds the retained result.
func (adapter *Adapter) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var chunks []v1.SpeechChunk
	err := adapter.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		chunk.PCM16LE = slices.Clone(chunk.PCM16LE)
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return chunks, nil
}

func cloneRawMessages(input map[string]json.RawMessage) map[string]json.RawMessage {
	if input == nil {
		return nil
	}
	output := make(map[string]json.RawMessage, len(input))
	for name, value := range input {
		output[name] = slices.Clone(value)
	}
	return output
}

func clonePointer[T any](input *T) *T {
	if input == nil {
		return nil
	}
	value := *input
	return &value
}

func cloneCapabilities(input v1.Capabilities) v1.Capabilities {
	output := make(v1.Capabilities, len(input))
	for capability, enabled := range input {
		output[capability] = enabled
	}
	return output
}

var _ v1.StreamingSpeechProvider = (*Adapter)(nil)
