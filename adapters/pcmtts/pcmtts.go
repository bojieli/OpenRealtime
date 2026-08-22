// Package pcmtts adapts the speech providers that answer one HTTP POST with a
// PCM stream: Deepgram, ElevenLabs, and Cartesia.
//
// Their request shapes differ - the voice is in the path for one, in the body
// for another, and the sample rate is a query parameter, a format string, and
// a nested object respectively - but the response is the same thing in all
// three cases, and it is the response that is hard. So the request is a small
// per-vendor profile and the streaming, resampling, and terminal-chunk logic
// is shared with every other speech adapter here.
//
// A provider whose speech endpoint speaks OpenAI's /v1/audio/speech contract
// belongs in adapters/openaitts instead. This package is for the ones that do
// not.
package pcmtts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/internal/speechstream"
)

const (
	// DeepgramEndpoint is Deepgram's speech route.
	DeepgramEndpoint = "https://api.deepgram.com/v1/speak"
	// DeepgramDefaultModel is a Deepgram Aura voice. Deepgram names the voice
	// and the model together, so there is no separate voice field.
	DeepgramDefaultModel = "aura-2-thalia-en"

	// ElevenLabsEndpoint is the streaming route, with the voice in the path.
	ElevenLabsEndpoint = "https://api.elevenlabs.io/v1/text-to-speech"
	// ElevenLabsDefaultModel is the low-latency multilingual model.
	ElevenLabsDefaultModel = "eleven_flash_v2_5"

	// CartesiaEndpoint is Cartesia's raw-bytes route.
	CartesiaEndpoint = "https://api.cartesia.ai/tts/bytes"
	// CartesiaDefaultModel is Cartesia's low-latency model.
	CartesiaDefaultModel = "sonic-2"
	// CartesiaDefaultVersion is the dated API version Cartesia requires in a
	// header. It is a date rather than a number and is worth checking against
	// their documentation before a deployment.
	CartesiaDefaultVersion = "2024-11-13"

	defaultRequestRate = uint32(24_000)
	defaultOutputRate  = uint32(24_000)
	defaultTimeout     = 2 * time.Minute
	maximumErrorBody   = 1 << 20
	maximumText        = 1 << 20
	maximumSampleRate  = 384_000
)

// Config configures one speech client. The same fields mean the same thing for
// every vendor; what differs is where they end up in the request.
type Config struct {
	APIKey string
	// Model is the vendor's model or voice-model identifier.
	Model string
	// Voice selects the speaker where the vendor separates it from the model.
	// Deepgram does not, and rejects a voice.
	Voice string
	// Endpoint overrides the vendor's URL, for a proxy or a region.
	Endpoint string
	// Language is a BCP-47 hint where the vendor accepts one.
	Language string
	// RequestSampleRateHz is the rate asked of the provider. Asking for the
	// output rate directly avoids a resample; a vendor that cannot produce it
	// returns something else and the shared reader resamples.
	RequestSampleRateHz uint32
	// OutputSampleRateHz is the rate chunks are emitted at.
	OutputSampleRateHz uint32
	// APIVersion is the dated version header Cartesia requires.
	APIVersion string
	// ExtraBody adds vendor-specific top-level body fields.
	ExtraBody map[string]json.RawMessage
	// Headers adds request headers.
	Headers         http.Header
	HTTPClient      *http.Client
	RequestTimeout  time.Duration
	ReadBufferBytes int
	MaxAudioBytes   int64
}

// profile is the per-vendor half of a request.
type profile struct {
	// label names the vendor in descriptors and error messages.
	label string
	// version identifies the wire contract in the descriptor.
	version string
	// endpoint is the resolved URL, voice already substituted where the
	// vendor puts it in the path.
	endpoint string
	// query is appended to the endpoint.
	query url.Values
	// header carries the vendor's authentication and version headers.
	header http.Header
	// body is the JSON request, minus the text.
	body map[string]json.RawMessage
	// textField names the body field the utterance goes in.
	textField string
	// sourceRateHz is the rate the response is expected at when the vendor
	// does not declare one.
	sourceRateHz uint32
}

// Adapter is safe for concurrent use. Each Stream call owns its request.
type Adapter struct {
	config     Config
	profile    profile
	descriptor v1.Descriptor
}

// NewDeepgram creates a Deepgram speech client.
func NewDeepgram(config Config) (*Adapter, error) {
	config, err := prepare(config, DeepgramDefaultModel)
	if err != nil {
		return nil, err
	}
	if config.Voice != "" {
		return nil, errors.New("Deepgram names the voice in the model; set -tts-model rather than a voice")
	}
	endpoint := config.Endpoint
	if endpoint == "" {
		endpoint = DeepgramEndpoint
	}
	query := url.Values{}
	query.Set("model", config.Model)
	query.Set("encoding", "linear16")
	query.Set("sample_rate", strconv.FormatUint(uint64(config.RequestSampleRateHz), 10))
	header := http.Header{}
	header.Set("Authorization", "Token "+config.APIKey)
	return build(config, profile{
		label: "Deepgram speech", version: "deepgram-speak-1",
		endpoint: endpoint, query: query, header: header,
		body: map[string]json.RawMessage{}, textField: "text",
		sourceRateHz: config.RequestSampleRateHz,
	})
}

// NewElevenLabs creates an ElevenLabs speech client. The voice is part of the
// path, so it is required rather than optional.
func NewElevenLabs(config Config) (*Adapter, error) {
	config, err := prepare(config, ElevenLabsDefaultModel)
	if err != nil {
		return nil, err
	}
	if config.Voice == "" {
		return nil, errors.New("ElevenLabs requires a voice identifier")
	}
	format, err := elevenLabsFormat(config.RequestSampleRateHz)
	if err != nil {
		return nil, err
	}
	root := config.Endpoint
	if root == "" {
		root = ElevenLabsEndpoint
	}
	query := url.Values{}
	query.Set("output_format", format)
	header := http.Header{}
	header.Set("xi-api-key", config.APIKey)
	body := map[string]json.RawMessage{}
	if encoded, err := json.Marshal(config.Model); err == nil {
		body["model_id"] = encoded
	}
	if config.Language != "" {
		if encoded, err := json.Marshal(config.Language); err == nil {
			body["language_code"] = encoded
		}
	}
	return build(config, profile{
		label: "ElevenLabs speech", version: "elevenlabs-tts-1",
		endpoint: strings.TrimRight(root, "/") + "/" + url.PathEscape(config.Voice) + "/stream",
		query:    query, header: header, body: body, textField: "text",
		sourceRateHz: config.RequestSampleRateHz,
	})
}

// NewCartesia creates a Cartesia speech client.
func NewCartesia(config Config) (*Adapter, error) {
	config, err := prepare(config, CartesiaDefaultModel)
	if err != nil {
		return nil, err
	}
	if config.Voice == "" {
		return nil, errors.New("Cartesia requires a voice identifier")
	}
	endpoint := config.Endpoint
	if endpoint == "" {
		endpoint = CartesiaEndpoint
	}
	version := config.APIVersion
	if version == "" {
		version = CartesiaDefaultVersion
	}
	header := http.Header{}
	header.Set("X-API-Key", config.APIKey)
	header.Set("Cartesia-Version", version)
	outputFormat, err := json.Marshal(map[string]any{
		"container": "raw", "encoding": "pcm_s16le", "sample_rate": config.RequestSampleRateHz,
	})
	if err != nil {
		return nil, err
	}
	voice, err := json.Marshal(map[string]any{"mode": "id", "id": config.Voice})
	if err != nil {
		return nil, err
	}
	model, err := json.Marshal(config.Model)
	if err != nil {
		return nil, err
	}
	body := map[string]json.RawMessage{
		"model_id": model, "voice": voice, "output_format": outputFormat,
	}
	if config.Language != "" {
		if encoded, err := json.Marshal(config.Language); err == nil {
			body["language"] = encoded
		}
	}
	return build(config, profile{
		label: "Cartesia speech", version: "cartesia-tts-1",
		endpoint: endpoint, query: url.Values{}, header: header,
		body: body, textField: "transcript", sourceRateHz: config.RequestSampleRateHz,
	})
}

// prepare applies the shared defaults and validation.
func prepare(config Config, defaultModel string) (Config, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return Config{}, errors.New("a hosted speech provider requires an API key")
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		config.Model = defaultModel
	}
	config.Voice = strings.TrimSpace(config.Voice)
	// "default" is the placeholder the server's own flag carries when no
	// voice was chosen. Treating it as a voice identifier would send a
	// vendor a voice that does not exist.
	if strings.EqualFold(config.Voice, "default") {
		config.Voice = ""
	}
	config.Endpoint = strings.TrimSpace(config.Endpoint)
	if config.Endpoint != "" {
		parsed, err := url.Parse(config.Endpoint)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return Config{}, errors.New("speech endpoint override must be absolute")
		}
	}
	if config.RequestSampleRateHz == 0 {
		config.RequestSampleRateHz = defaultRequestRate
	}
	if config.OutputSampleRateHz == 0 {
		config.OutputSampleRateHz = defaultOutputRate
	}
	if config.RequestSampleRateHz > maximumSampleRate || config.OutputSampleRateHz > maximumSampleRate {
		return Config{}, errors.New("speech sample rate is implausible")
	}
	if config.RequestTimeout < 0 {
		return Config{}, errors.New("speech request timeout cannot be negative")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultTimeout
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	config.Headers = config.Headers.Clone()
	config.ExtraBody = maps.Clone(config.ExtraBody)
	for name, value := range config.ExtraBody {
		if strings.TrimSpace(name) != name || name == "" {
			return Config{}, errors.New("speech extension field name must be non-empty and trimmed")
		}
		if !json.Valid(value) {
			return Config{}, fmt.Errorf("speech extension %q is not valid JSON", name)
		}
	}
	return config, nil
}

func build(config Config, resolved profile) (*Adapter, error) {
	for name, value := range config.ExtraBody {
		if name == resolved.textField {
			return nil, fmt.Errorf("speech extension cannot override the %q field", name)
		}
		resolved.body[name] = value
	}
	return &Adapter{
		config: config, profile: resolved,
		descriptor: v1.Descriptor{
			Name:    resolved.label + "/" + config.Model,
			Version: resolved.version,
			Capabilities: v1.Capabilities{
				v1.CapabilityCancellation:    true,
				v1.CapabilityPCM16Output:     true,
				v1.CapabilityStreamingOutput: true,
			},
		},
	}, nil
}

// elevenLabsFormat maps a sample rate onto ElevenLabs' PCM format names. The
// set is closed, so an unsupported rate is refused rather than silently
// answered at a different one.
func elevenLabsFormat(rate uint32) (string, error) {
	switch rate {
	case 16_000, 22_050, 24_000, 44_100:
		return "pcm_" + strconv.FormatUint(uint64(rate), 10), nil
	default:
		return "", fmt.Errorf(
			"ElevenLabs serves PCM at 16000, 22050, 24000, or 44100 Hz, not %d", rate)
	}
}

// Descriptor implements api/v1.SpeechProvider.
func (adapter *Adapter) Descriptor() v1.Descriptor {
	descriptor := adapter.descriptor
	descriptor.Capabilities = maps.Clone(descriptor.Capabilities)
	return descriptor
}

// Stream implements api/v1.StreamingSpeechProvider.
func (adapter *Adapter) Stream(
	ctx context.Context, plan v1.SpeechPlan, consume func(v1.SpeechChunk) error,
) error {
	if consume == nil {
		return v1.ErrNilConsumer
	}
	plan.CandidateID = strings.TrimSpace(plan.CandidateID)
	plan.Text = strings.TrimSpace(plan.Text)
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("%s speech plan: %w", adapter.profile.label, err)
	}
	if len(plan.Text) > maximumText {
		return fmt.Errorf("%s input exceeds %d bytes", adapter.profile.label, maximumText)
	}
	body := maps.Clone(adapter.profile.body)
	text, err := json.Marshal(plan.Text)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", adapter.profile.label, err)
	}
	body[adapter.profile.textField] = text
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", adapter.profile.label, err)
	}

	target := adapter.profile.endpoint
	if len(adapter.profile.query) > 0 {
		separator := "?"
		if strings.Contains(target, "?") {
			separator = "&"
		}
		target += separator + adapter.profile.query.Encode()
	}
	requestContext := ctx
	if adapter.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		requestContext, cancel = context.WithTimeout(ctx, adapter.config.RequestTimeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, target, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create %s request: %w", adapter.profile.label, err)
	}
	for name, values := range adapter.profile.header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	for name, values := range adapter.config.Headers {
		for _, value := range values {
			request.Header.Set(name, value)
		}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "*/*")
	request.Header.Set("User-Agent", "OpenRealtime/pcm-tts")

	response, err := adapter.config.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("send %s request: %w", adapter.profile.label, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maximumErrorBody))
		return fmt.Errorf("%s returned HTTP %d: %s",
			adapter.profile.label, response.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := rejectNonAudio(response.Header.Get("Content-Type"), adapter.profile.label); err != nil {
		return err
	}
	return speechstream.Consume(response.Body, speechstream.Options{
		Label: adapter.profile.label, SourceRateHz: adapter.profile.sourceRateHz,
		OutputRateHz:    adapter.config.OutputSampleRateHz,
		ReadBufferBytes: adapter.config.ReadBufferBytes, MaxAudioBytes: adapter.config.MaxAudioBytes,
	}, plan.CandidateID, consume)
}

// rejectNonAudio catches the common failure where a provider answers 200 with
// a JSON error body. Treating that as PCM would synthesise the error message
// as noise and play it to the caller.
func rejectNonAudio(value, label string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	mediaType, _, err := mime.ParseMediaType(trimmed)
	if err != nil {
		return fmt.Errorf("%s returned invalid content type %q: %w", label, value, err)
	}
	if strings.HasPrefix(mediaType, "audio/") || mediaType == "application/octet-stream" {
		return nil
	}
	return fmt.Errorf("%s returned content type %q; expected audio", label, mediaType)
}

// Synthesize collects Stream output for consumers that require the stable
// non-streaming interface.
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
