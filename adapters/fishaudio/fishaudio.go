// Package fishaudio adapts the self-hosted Fish Speech HTTP server to the
// stable OpenRealtime streaming speech interface.
//
// Fish selects the base model when its server starts. The request therefore
// contains synthesis controls and optional voice references, but deliberately
// does not invent a per-request model field.
package fishaudio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/pcm"
)

const (
	// DefaultEndpoint is the native Fish Speech TTS endpoint.
	DefaultEndpoint = "http://127.0.0.1:8080/v1/tts"
	// DefaultModel records the server-side checkpoint expected by the initial
	// study. The native request has no model selector.
	DefaultModel = "fishaudio/s2-pro"

	defaultServerRate = uint32(44_100)
	defaultOutputRate = uint32(24_000)
	defaultTimeout    = 2 * time.Minute
	defaultReadBuffer = 8 << 10
	defaultMaxAudio   = 128 << 20
	maxErrorBody      = 1 << 20
)

// Reference supplies an in-context voice example. Audio contains an encoded
// audio file (for example WAV), not raw PCM. ReferenceID takes precedence when
// both forms are configured, matching Fish Speech's native behavior.
type Reference struct {
	Audio []byte `json:"audio"`
	Text  string `json:"text"`
}

// Config configures a Fish Speech client. ServerSampleRateHz is used for the
// raw streaming contract employed by the official client; if a server emits a
// WAV header, its declared rate is validated and used instead.
type Config struct {
	Endpoint             string
	Model                string
	BearerToken          string
	Headers              http.Header
	HTTPClient           *http.Client
	RequestTimeout       time.Duration
	ServerSampleRateHz   uint32
	OutputSampleRateHz   uint32
	ReferenceID          string
	References           []Reference
	UseMemoryCache       bool
	DisableNormalization bool
	Seed                 *int64
	ChunkLength          int
	MaxNewTokens         int
	TopP                 float64
	RepetitionPenalty    float64
	Temperature          float64
	ReadBufferBytes      int
	MaxAudioBytes        int64
}

// Adapter is safe for concurrent calls. Fish server-side scheduling determines
// actual concurrency; each Stream call owns independent request state.
type Adapter struct {
	config     Config
	descriptor v1.Descriptor
}

// New validates and copies configuration.
func New(config Config) (*Adapter, error) {
	if config.Endpoint == "" {
		config.Endpoint = DefaultEndpoint
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("Fish Audio endpoint must be absolute")
	}
	if config.Model == "" {
		config.Model = DefaultModel
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		return nil, errors.New("Fish Audio model name is required")
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("Fish Audio request timeout cannot be negative")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultTimeout
	}
	if config.ServerSampleRateHz == 0 {
		config.ServerSampleRateHz = defaultServerRate
	}
	if config.OutputSampleRateHz == 0 {
		config.OutputSampleRateHz = defaultOutputRate
	}
	if config.ChunkLength == 0 {
		config.ChunkLength = 200
	}
	if config.ChunkLength < 100 || config.ChunkLength > 1_000 {
		return nil, errors.New("Fish Audio chunk length must be between 100 and 1000")
	}
	if config.MaxNewTokens == 0 {
		config.MaxNewTokens = 1_024
	}
	if config.MaxNewTokens < 0 {
		return nil, errors.New("Fish Audio maximum new tokens cannot be negative")
	}
	if config.TopP == 0 {
		config.TopP = 0.8
	}
	if config.TopP < 0.1 || config.TopP > 1 {
		return nil, errors.New("Fish Audio top-p must be between 0.1 and 1")
	}
	if config.RepetitionPenalty == 0 {
		config.RepetitionPenalty = 1.1
	}
	if config.RepetitionPenalty < 0.9 || config.RepetitionPenalty > 2 {
		return nil, errors.New("Fish Audio repetition penalty must be between 0.9 and 2")
	}
	if config.Temperature == 0 {
		config.Temperature = 0.8
	}
	if config.Temperature < 0.1 || config.Temperature > 1 {
		return nil, errors.New("Fish Audio temperature must be between 0.1 and 1")
	}
	if config.ReadBufferBytes == 0 {
		config.ReadBufferBytes = defaultReadBuffer
	}
	if config.ReadBufferBytes < 2 {
		return nil, errors.New("Fish Audio read buffer must contain at least two bytes")
	}
	if config.MaxAudioBytes == 0 {
		config.MaxAudioBytes = defaultMaxAudio
	}
	if config.MaxAudioBytes < 2 {
		return nil, errors.New("Fish Audio maximum audio size must contain at least one PCM16 sample")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	config.Headers = config.Headers.Clone()
	config.BearerToken = strings.TrimSpace(config.BearerToken)
	config.ReferenceID = strings.TrimSpace(config.ReferenceID)
	if config.Seed != nil {
		seed := *config.Seed
		config.Seed = &seed
	}
	config.References = cloneReferences(config.References)
	for index, reference := range config.References {
		if len(reference.Audio) == 0 || strings.TrimSpace(reference.Text) == "" {
			return nil, fmt.Errorf("Fish Audio reference %d requires audio and text", index)
		}
	}

	descriptor := v1.Descriptor{
		Name:    "fish-audio-streaming/" + config.Model,
		Version: "fish-speech-2.0.0",
		Capabilities: v1.Capabilities{
			v1.CapabilityCancellation:    true,
			v1.CapabilityPCM16Output:     true,
			v1.CapabilityStreamingOutput: true,
		},
	}
	return &Adapter{config: config, descriptor: descriptor}, nil
}

// Descriptor implements api/v1.SpeechProvider.
func (adapter *Adapter) Descriptor() v1.Descriptor {
	descriptor := adapter.descriptor
	descriptor.Capabilities = cloneCapabilities(descriptor.Capabilities)
	return descriptor
}

type ttsRequest struct {
	Text              string      `json:"text"`
	ChunkLength       int         `json:"chunk_length"`
	Format            string      `json:"format"`
	Latency           string      `json:"latency"`
	References        []Reference `json:"references"`
	ReferenceID       string      `json:"reference_id,omitempty"`
	Seed              *int64      `json:"seed,omitempty"`
	UseMemoryCache    string      `json:"use_memory_cache"`
	Normalize         bool        `json:"normalize"`
	Streaming         bool        `json:"streaming"`
	MaxNewTokens      int         `json:"max_new_tokens"`
	TopP              float64     `json:"top_p"`
	RepetitionPenalty float64     `json:"repetition_penalty"`
	Temperature       float64     `json:"temperature"`
}

// Stream implements api/v1.StreamingSpeechProvider. It forwards output as
// 24 kHz PCM16LE by default, suitable for the existing OpenAI Realtime media
// boundary. Only the last sample is retained to mark the terminal chunk, so
// streaming adds less than one output-sample period of buffering.
func (adapter *Adapter) Stream(ctx context.Context, plan v1.SpeechPlan, consume func(v1.SpeechChunk) error) error {
	if consume == nil {
		return v1.ErrNilConsumer
	}
	plan.CandidateID = strings.TrimSpace(plan.CandidateID)
	plan.Text = strings.TrimSpace(plan.Text)
	if plan.CandidateID == "" || plan.Text == "" {
		return errors.New("Fish Audio speech plan requires candidate ID and text")
	}
	requestBody := ttsRequest{
		Text: plan.Text, ChunkLength: adapter.config.ChunkLength,
		Format: "wav", Latency: "normal", References: cloneReferences(adapter.config.References),
		ReferenceID: adapter.config.ReferenceID, Seed: adapter.config.Seed,
		UseMemoryCache: map[bool]string{false: "off", true: "on"}[adapter.config.UseMemoryCache],
		Normalize:      !adapter.config.DisableNormalization, Streaming: true,
		MaxNewTokens: adapter.config.MaxNewTokens, TopP: adapter.config.TopP,
		RepetitionPenalty: adapter.config.RepetitionPenalty, Temperature: adapter.config.Temperature,
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("encode Fish Audio request: %w", err)
	}
	requestContext := ctx
	if adapter.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		requestContext, cancel = context.WithTimeout(ctx, adapter.config.RequestTimeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, adapter.config.Endpoint, bytes.NewReader(encoded))
	if err != nil {
		return fmt.Errorf("create Fish Audio request: %w", err)
	}
	for name, values := range adapter.config.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "audio/wav, application/octet-stream")
	request.Header.Set("User-Agent", "OpenRealtime/fish-audio")
	if adapter.config.BearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+adapter.config.BearerToken)
	}
	response, err := adapter.config.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("send Fish Audio request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return fmt.Errorf("Fish Audio returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}

	decoder := newAudioStreamDecoder(adapter.config.ServerSampleRateHz)
	buffer := make([]byte, adapter.config.ReadBufferBytes)
	var resampler *pcm.Resampler
	var oddByte []byte
	var heldSample []byte
	var outputBytes int64
	var sampleOffset uint64
	sequence := uint64(0)
	emit := func(audio []byte, final bool) error {
		if len(audio) == 0 || len(audio)%2 != 0 {
			return errors.New("Fish Audio produced an invalid PCM16 chunk")
		}
		if int64(len(audio)) > adapter.config.MaxAudioBytes-outputBytes {
			return fmt.Errorf("Fish Audio output exceeds %d bytes", adapter.config.MaxAudioBytes)
		}
		sequence++
		chunk := v1.SpeechChunk{
			ChunkID: fmt.Sprintf("%s-%06d", plan.CandidateID, sequence), CandidateID: plan.CandidateID,
			SampleOffset: sampleOffset, SampleRateHz: adapter.config.OutputSampleRateHz,
			PCM16LE: slices.Clone(audio), Final: final,
		}
		if err := consume(chunk); err != nil {
			return fmt.Errorf("consume Fish Audio chunk: %w", err)
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
	feed := func(encodedPCM []byte, sourceRate uint32) error {
		if len(encodedPCM) == 0 {
			return nil
		}
		if len(oddByte) != 0 {
			encodedPCM = append(append([]byte(nil), oddByte...), encodedPCM...)
			oddByte = nil
		}
		if len(encodedPCM)%2 != 0 {
			oddByte = []byte{encodedPCM[len(encodedPCM)-1]}
			encodedPCM = encodedPCM[:len(encodedPCM)-1]
		}
		if len(encodedPCM) == 0 {
			return nil
		}
		if resampler == nil {
			var err error
			resampler, err = pcm.NewResampler(sourceRate, adapter.config.OutputSampleRateHz)
			if err != nil {
				return err
			}
		} else if resampler.InputRate() != sourceRate {
			return fmt.Errorf("Fish Audio source sample rate changed from %d to %d", resampler.InputRate(), sourceRate)
		}
		converted, err := resampler.Push(encodedPCM)
		if err != nil {
			return fmt.Errorf("resample Fish Audio stream: %w", err)
		}
		return offer(converted)
	}

	for {
		read, readErr := response.Body.Read(buffer)
		if read > 0 {
			audio, sourceRate, err := decoder.Push(buffer[:read], false)
			if err != nil {
				return fmt.Errorf("decode Fish Audio stream: %w", err)
			}
			if err := feed(audio, sourceRate); err != nil {
				return err
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return fmt.Errorf("read Fish Audio stream: %w", readErr)
			}
			break
		}
	}
	audio, sourceRate, err := decoder.Push(nil, true)
	if err != nil {
		return fmt.Errorf("finalize Fish Audio stream decoder: %w", err)
	}
	if err := feed(audio, sourceRate); err != nil {
		return err
	}
	if len(oddByte) != 0 {
		return errors.New("Fish Audio stream ended with a partial PCM16 sample")
	}
	if resampler == nil {
		return errors.New("Fish Audio returned no audio")
	}
	terminal, err := resampler.Finalize()
	if err != nil {
		return fmt.Errorf("finalize Fish Audio resampler: %w", err)
	}
	if len(terminal) != 0 {
		combined := make([]byte, 0, len(heldSample)+len(terminal))
		combined = append(combined, heldSample...)
		combined = append(combined, terminal...)
		heldSample = combined
	}
	if len(heldSample) == 0 {
		return errors.New("Fish Audio returned no complete output sample")
	}
	return emit(heldSample, true)
}

// Synthesize collects Stream output for consumers that require the non-streaming
// stable interface. MaxAudioBytes bounds the retained result.
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

type audioStreamDecoder struct {
	mode        decoderMode
	pending     []byte
	defaultRate uint32
	rate        uint32
	fmtSeen     bool
}

type decoderMode uint8

const (
	decoderUnknown decoderMode = iota
	decoderWAVHeader
	decoderBody
)

func newAudioStreamDecoder(defaultRate uint32) *audioStreamDecoder {
	return &audioStreamDecoder{defaultRate: defaultRate}
}

// Push accepts either the raw PCM stream emitted by the official Fish client
// contract or a standards-compliant streaming WAV response. WAV data length is
// intentionally ignored because streaming servers commonly write zero there.
func (decoder *audioStreamDecoder) Push(input []byte, final bool) ([]byte, uint32, error) {
	if decoder.mode == decoderBody {
		return slices.Clone(input), decoder.rate, nil
	}
	decoder.pending = append(decoder.pending, input...)
	if decoder.mode == decoderUnknown {
		prefixLength := min(len(decoder.pending), 4)
		if !bytes.Equal(decoder.pending[:prefixLength], []byte("RIFF")[:prefixLength]) {
			decoder.mode = decoderBody
			decoder.rate = decoder.defaultRate
			output := decoder.pending
			decoder.pending = nil
			return output, decoder.rate, nil
		}
		if len(decoder.pending) < 12 {
			if final {
				return nil, 0, errors.New("truncated RIFF/WAVE header")
			}
			return nil, 0, nil
		}
		if string(decoder.pending[8:12]) != "WAVE" {
			return nil, 0, errors.New("RIFF response is not WAVE audio")
		}
		decoder.mode = decoderWAVHeader
		decoder.pending = decoder.pending[12:]
	}
	for decoder.mode == decoderWAVHeader {
		if len(decoder.pending) < 8 {
			if final {
				return nil, 0, errors.New("WAV stream ended before a data chunk")
			}
			return nil, 0, nil
		}
		chunkID := string(decoder.pending[:4])
		size := uint64(binary.LittleEndian.Uint32(decoder.pending[4:8]))
		if chunkID == "data" {
			if !decoder.fmtSeen {
				return nil, 0, errors.New("WAV data chunk precedes fmt chunk")
			}
			decoder.mode = decoderBody
			output := slices.Clone(decoder.pending[8:])
			decoder.pending = nil
			return output, decoder.rate, nil
		}
		padded := size + size%2
		if padded > uint64(math.MaxInt)-8 {
			return nil, 0, errors.New("WAV chunk is too large")
		}
		needed := 8 + int(padded)
		if len(decoder.pending) < needed {
			if final {
				return nil, 0, errors.New("truncated WAV chunk")
			}
			return nil, 0, nil
		}
		if chunkID == "fmt " {
			if size < 16 {
				return nil, 0, errors.New("WAV fmt chunk is too short")
			}
			format := decoder.pending[8 : 8+size]
			if binary.LittleEndian.Uint16(format[0:2]) != 1 || binary.LittleEndian.Uint16(format[2:4]) != 1 || binary.LittleEndian.Uint16(format[14:16]) != 16 {
				return nil, 0, errors.New("Fish Audio WAV must be mono 16-bit PCM")
			}
			decoder.rate = binary.LittleEndian.Uint32(format[4:8])
			if decoder.rate == 0 {
				return nil, 0, errors.New("Fish Audio WAV sample rate is zero")
			}
			decoder.fmtSeen = true
		}
		decoder.pending = decoder.pending[needed:]
	}
	return nil, 0, nil
}

func cloneReferences(input []Reference) []Reference {
	output := make([]Reference, len(input))
	for index, reference := range input {
		output[index] = Reference{Audio: slices.Clone(reference.Audio), Text: reference.Text}
	}
	return output
}

func cloneCapabilities(input v1.Capabilities) v1.Capabilities {
	output := make(v1.Capabilities, len(input))
	for capability, enabled := range input {
		output[capability] = enabled
	}
	return output
}

var _ v1.StreamingSpeechProvider = (*Adapter)(nil)
