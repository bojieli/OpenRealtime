// Package wordtimings adapts an OpenAI-shaped transcription endpoint that
// reports word times to the spoken.Aligner contract.
//
// The wire shape is the published one - multipart POST of an audio file,
// `response_format=verbose_json`, `timestamp_granularities[]=word`, and a
// `words` array of `{word, start, end}` in seconds - so a deployment can point
// this at a local Whisper server, a hosted transcription API, or anything else
// that already answers that request. Nothing here is specific to a model.
//
// What this is used for is narrower than transcription, and the narrowness is
// worth stating: the text of the utterance is already known exactly, because
// the runtime is the one that wrote it. Only the times are wanted. A word this
// endpoint gets wrong costs an anchor and nothing else - spoken.Reconcile keeps
// the synthesiser's wording either way - so a recogniser that is merely good
// enough is genuinely good enough here.
package wordtimings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/spoken"
)

// DefaultEndpoint is where deploy/wordtimings serves the route.
const DefaultEndpoint = "http://127.0.0.1:8003/v1/audio/transcriptions"

// DefaultModel is what that deployment loads. Model selection belongs to the
// server process; this records the condition in descriptors and evidence.
const DefaultModel = "Systran/faster-whisper-base.en"

const (
	maxErrorBody    = 1 << 16
	maxResponseBody = 1 << 22
)

// Config configures the client.
type Config struct {
	Endpoint string
	Model    string
	// ModelField names the multipart field carrying the model. Empty selects
	// "model", which is what the published route uses.
	ModelField string
	// Language narrows the recogniser when a deployment knows what its own
	// voice speaks. Empty lets the endpoint decide.
	Language       string
	APIKey         string
	AuthHeader     string
	Headers        http.Header
	ExtraFields    map[string]string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

// Adapter is safe for concurrent calls.
type Adapter struct {
	config   Config
	endpoint string
}

// New returns a client for the word-timing route.
func New(config Config) (*Adapter, error) {
	if strings.TrimSpace(config.Endpoint) == "" {
		config.Endpoint = DefaultEndpoint
	}
	if strings.TrimSpace(config.Model) == "" {
		config.Model = DefaultModel
	}
	if strings.TrimSpace(config.ModelField) == "" {
		config.ModelField = "model"
	}
	if strings.TrimSpace(config.AuthHeader) == "" {
		config.AuthHeader = "Authorization"
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = 10 * time.Second
	}
	return &Adapter{config: config, endpoint: config.Endpoint}, nil
}

// Endpoint is where this adapter sends its audio, for readiness checks and
// deployment evidence.
func (adapter *Adapter) Endpoint() string { return adapter.endpoint }

// Model names the recogniser this adapter was configured against.
func (adapter *Adapter) Model() string { return adapter.config.Model }

type timedWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
}

type verboseTranscription struct {
	Text     string      `json:"text"`
	Words    []timedWord `json:"words"`
	Segments []struct {
		Words []timedWord `json:"words"`
	} `json:"segments"`
}

// ErrNoWordTimes means the endpoint answered without the one thing it was
// asked for.
//
// It is a named error rather than an empty result because the two mean
// different things to a deployment: no words in a piece of audio is a fact
// about the audio, while an endpoint that ignores timestamp_granularities is a
// misconfiguration that will never produce a measured boundary and should say
// so once rather than degrade in silence for ever.
var ErrNoWordTimes = errors.New("the transcription endpoint returned no word timestamps")

// Words transcribes audio and returns where each word sat inside it.
func (adapter *Adapter) Words(ctx context.Context, clip spoken.Audio) ([]spoken.Word, error) {
	if len(clip.PCM16LE) == 0 {
		return nil, errors.New("word timings need audio")
	}
	if clip.SampleRateHz == 0 {
		return nil, errors.New("word timings need a sample rate")
	}
	container, err := audio.EncodeWAVMono16(clip.PCM16LE, clip.SampleRateHz)
	if err != nil {
		return nil, fmt.Errorf("encode word-timing upload: %w", err)
	}
	body, contentType, err := adapter.upload(container)
	if err != nil {
		return nil, err
	}
	timed, cancel := context.WithTimeout(ctx, adapter.config.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(timed, http.MethodPost, adapter.endpoint, body)
	if err != nil {
		return nil, err
	}
	for name, values := range adapter.config.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Content-Type", contentType)
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
		return nil, fmt.Errorf("send word-timing request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return nil, fmt.Errorf("word-timing endpoint returned HTTP %d: %s",
			response.StatusCode, strings.TrimSpace(string(message)))
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read word-timing response: %w", err)
	}
	var decoded verboseTranscription
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("decode word-timing response: %w", err)
	}
	timings := decoded.Words
	if len(timings) == 0 {
		// Some servers nest the words inside their segments. Both are the same
		// answer; refusing one of them would make the adapter work against
		// half the endpoints that already do what it needs.
		for _, segment := range decoded.Segments {
			timings = append(timings, segment.Words...)
		}
	}
	if len(timings) == 0 {
		if strings.TrimSpace(decoded.Text) == "" {
			// Silence transcribes to nothing. That is an answer about the
			// audio, not a broken endpoint, and it must not be reported as
			// one.
			return nil, nil
		}
		return nil, fmt.Errorf("%w: it transcribed %q; ask for timestamp_granularities[]=word",
			ErrNoWordTimes, truncate(decoded.Text))
	}
	return convert(timings), nil
}

func (adapter *Adapter) upload(container []byte) (io.Reader, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="utterance.wav"`)
	header.Set("Content-Type", "audio/wav")
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", fmt.Errorf("create word-timing upload: %w", err)
	}
	if _, err := part.Write(container); err != nil {
		return nil, "", fmt.Errorf("write word-timing upload: %w", err)
	}
	fields := [][2]string{
		{adapter.config.ModelField, adapter.config.Model},
		{"response_format", "verbose_json"},
		// The published spelling is an array field, and servers that accept
		// the scalar accept this too.
		{"timestamp_granularities[]", "word"},
	}
	if adapter.config.Language != "" {
		fields = append(fields, [2]string{"language", adapter.config.Language})
	}
	for name, value := range adapter.config.ExtraFields {
		fields = append(fields, [2]string{name, value})
	}
	for _, field := range fields {
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return nil, "", fmt.Errorf("write word-timing field %q: %w", field[0], err)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("close word-timing upload: %w", err)
	}
	return &body, writer.FormDataContentType(), nil
}

// convert turns seconds into the milliseconds the runtime paces in.
//
// A negative or non-finite time is dropped rather than clamped. Reconcile uses
// these as anchors, and an anchor at an impossible position drags every
// interpolated word around it to the wrong place; having one fewer anchor
// costs a word of resolution.
func convert(timings []timedWord) []spoken.Word {
	words := make([]spoken.Word, 0, len(timings))
	for _, timing := range timings {
		if strings.TrimSpace(timing.Word) == "" {
			continue
		}
		if math.IsNaN(timing.Start) || math.IsNaN(timing.End) ||
			math.IsInf(timing.Start, 0) || math.IsInf(timing.End, 0) ||
			timing.Start < 0 || timing.End < 0 {
			continue
		}
		start := uint64(math.Round(timing.Start * 1000))
		end := uint64(math.Round(timing.End * 1000))
		if end < start {
			end = start
		}
		words = append(words, spoken.Word{
			Text: strings.TrimSpace(timing.Word), StartMS: start, EndMS: end,
		})
	}
	return words
}

func truncate(text string) string {
	if len(text) > 120 {
		return text[:120] + "…"
	}
	return text
}

var _ spoken.Aligner = (*Adapter)(nil)
