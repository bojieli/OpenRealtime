// Package speakerid adapts the self-hosted embedding service to the voices
// contract.
//
// The service returns a unit-length vector per utterance and nothing else. It
// deliberately does not compare them: what counts as the same person is a
// policy decision, and policy belongs where the rest of the policy is.
package speakerid

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// DefaultEndpoint is where tools/speakerid serves embeddings.
const DefaultEndpoint = "http://127.0.0.1:8124/embed"

// AdapterVersion identifies the HTTP/PCM contract implemented here. The
// embedding model and its immutable weights are separate deployment pins.
const AdapterVersion = "speakerid-http-pcm16-1"

const maxErrorBody = 1 << 16

// Config configures the client.
type Config struct {
	Endpoint       string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

// Adapter is safe for concurrent calls.
type Adapter struct {
	endpoint string
	client   *http.Client
	timeout  time.Duration
}

// New returns a client for the embedding service.
func New(config Config) (*Adapter, error) {
	endpoint := config.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	timeout := config.RequestTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Adapter{endpoint: endpoint, client: client, timeout: timeout}, nil
}

type reply struct {
	Embedding []float32 `json:"embedding"`
	Reason    string    `json:"reason,omitempty"`
}

// Embed returns the speaker embedding of one stretch of audio.
//
// An utterance too short to identify comes back as no embedding rather than as
// an error: the service is reporting that it cannot tell, which is an answer.
func (adapter *Adapter) Embed(ctx context.Context, pcm []byte, rateHz uint32) ([]float32, error) {
	if len(pcm) == 0 {
		return nil, errors.New("speaker embedding needs audio")
	}
	if rateHz == 0 {
		return nil, errors.New("speaker embedding needs a sample rate")
	}
	timed, cancel := context.WithTimeout(ctx, adapter.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(timed, http.MethodPost, adapter.endpoint, bytes.NewReader(pcm))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Sample-Rate", strconv.FormatUint(uint64(rateHz), 10))
	response, err := adapter.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return nil, fmt.Errorf("speaker embedding returned %s: %s", response.Status, bytes.TrimSpace(body))
	}
	var decoded reply
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded.Embedding, nil
}
