// Package turnend is the client for an acoustic end-of-turn classifier served
// over HTTP - Smart Turn in the duplex deployment profiles
// (tools/duplexmodels/turn_server.py, POST /v1/endpoint/smart-turn).
//
// The request body is the most recent user audio as little-endian float32
// mono at 16 kHz, at most MaxWindow long and ending at the pause being judged;
// the response is {"probability": P(turn complete), "model": "..."}. The
// classifier never sees audio from after the pause, so the evidence is causal.
package turnend

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
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/httpclient"
)

const (
	// SampleRateHz is the rate the service reads.
	SampleRateHz = uint32(16_000)
	// MaxWindow is the most audio sent: Smart Turn reads up to eight seconds.
	MaxWindow = 8 * time.Second
	// DefaultTimeout bounds one classification. The call sits on the pause
	// decision, so a slow classifier costs the user that much waiting.
	DefaultTimeout = 300 * time.Millisecond

	maxResponse = 64 << 10
)

// Config configures the client.
type Config struct {
	URL        string
	Timeout    time.Duration
	HTTPClient *http.Client
}

// Client classifies pauses.
type Client struct {
	config Config
	name   string
}

// New validates the configuration.
func New(config Config) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errors.New("acoustic end-of-turn URL must be an absolute http(s) URL")
	}
	config.URL = parsed.String()
	if config.Timeout < 0 {
		return nil, errors.New("acoustic end-of-turn timeout cannot be negative")
	}
	if config.Timeout == 0 {
		config.Timeout = DefaultTimeout
	}
	if config.HTTPClient == nil {
		config.HTTPClient = httpclient.Shared()
	}
	name := "turnend/" + strings.Trim(parsed.Path, "/")
	return &Client{config: config, name: name}, nil
}

// Name identifies the client and route in evidence.
func (client *Client) Name() string { return client.name }

// Evaluate classifies the pause at the end of pcm16le, which must be 16 kHz
// mono PCM16. Audio beyond MaxWindow is dropped from the front.
func (client *Client) Evaluate(ctx context.Context, pcm16le []byte) (interaction.AcousticEndpoint, error) {
	pcm16le = pcm16le[:len(pcm16le)/2*2]
	if len(pcm16le) == 0 {
		return interaction.AcousticEndpoint{}, errors.New("acoustic end-of-turn window is empty")
	}
	maximum := int(MaxWindow.Seconds()*float64(SampleRateHz)) * 2
	if len(pcm16le) > maximum {
		pcm16le = pcm16le[len(pcm16le)-maximum:]
	}
	body := make([]byte, len(pcm16le)*2)
	for offset := 0; offset < len(pcm16le); offset += 2 {
		sample := int16(binary.LittleEndian.Uint16(pcm16le[offset:]))
		binary.LittleEndian.PutUint32(body[offset*2:], math.Float32bits(float32(sample)/32768))
	}
	requestContext, cancel := context.WithTimeout(ctx, client.config.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, client.config.URL, bytes.NewReader(body))
	if err != nil {
		return interaction.AcousticEndpoint{}, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	started := time.Now()
	response, err := client.config.HTTPClient.Do(request)
	if err != nil {
		return interaction.AcousticEndpoint{}, fmt.Errorf("acoustic end-of-turn request: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponse))
	if err != nil {
		return interaction.AcousticEndpoint{}, fmt.Errorf("read acoustic end-of-turn response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return interaction.AcousticEndpoint{}, fmt.Errorf("acoustic end-of-turn service returned HTTP %d: %s",
			response.StatusCode, strings.TrimSpace(string(payload)))
	}
	var decoded struct {
		Probability *float64 `json:"probability"`
		Model       string   `json:"model"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return interaction.AcousticEndpoint{}, fmt.Errorf("decode acoustic end-of-turn response: %w", err)
	}
	if decoded.Probability == nil || *decoded.Probability < 0 || *decoded.Probability > 1 ||
		math.IsNaN(*decoded.Probability) {
		return interaction.AcousticEndpoint{}, errors.New("acoustic end-of-turn response has no probability in [0,1]")
	}
	model := decoded.Model
	if model == "" {
		model = client.name
	}
	return interaction.AcousticEndpoint{
		Probability: *decoded.Probability, Model: model, Latency: time.Since(started),
		WindowMS: float64(len(pcm16le)/2) * 1000 / float64(SampleRateHz),
	}, nil
}
