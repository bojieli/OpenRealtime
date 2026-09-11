// Package noisefilter implements the bounded, pre-ASR PCM filtering contract.
// A failed or late filter never releases the original audio downstream.
package noisefilter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/internal/httpclient"
)

type Config struct {
	URL       string `json:"url"`
	TimeoutMS int    `json:"timeout_ms"`
	Model     string `json:"model,omitempty"`
}

func (config Config) ModelName() string {
	if config.Model == "" {
		return "rnnoise"
	}
	return config.Model
}

func (config Config) Validate() error {
	if config.ModelName() != "rnnoise" && config.ModelName() != "real-tse" {
		return errors.New("audio filter model must be rnnoise or real-tse")
	}
	u, err := url.Parse(config.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("noise filter requires an HTTP(S) base URL without credentials, query, or fragment")
	}
	if config.TimeoutMS < 1 || config.TimeoutMS > 50 {
		return errors.New("noise filter timeout_ms must be 1..50 (strict pre-ASR budget)")
	}
	return nil
}

// Client is session-local and called serially. Sequence numbers prevent a
// retry, reordered request, or expired sidecar state from corrupting audio.
type Client struct {
	config      Config
	http        *http.Client
	id          string
	sequence    uint64
	failed      bool
	targetPhase int
	target      *targetWorker
}

func New(config Config) (*Client, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	config.URL = strings.TrimRight(config.URL, "/")
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	// The shared pool matters more on this path than on any other. This client
	// is called several times a second per session under a deadline of tens of
	// milliseconds, and http.DefaultTransport keeps two idle connections per
	// host - so every concurrent packet past the second would redial and
	// re-handshake mid-utterance, inside the filtering deadline that decides
	// whether the frame reaches admission at all. Redirects are refused on a
	// copy rather than followed: a redirected filter request is a
	// misconfiguration, and chasing it would spend the packet's deadline.
	filterClient := *httpclient.Shared()
	filterClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client := &Client{config: config, id: hex.EncodeToString(id[:]), http: &filterClient}
	if config.ModelName() == "real-tse" {
		client.target = newTargetWorker(client)
	}
	return client, nil
}

// Process returns the same sample count. RNNoise has a 20ms streaming delay;
// real-tse advertises a 65ms bound including its FIFO and resampling.
// The caller must forward this result, never the original PCM, to admission.
// Large ingress packets are split into <=100ms processing requests, all sharing
// one delivery deadline so batching cannot multiply caller latency. Target jobs
// may finish state updates after that deadline, but their late PCM is discarded.
// There is no wait for a transcript or a complete utterance. Target extraction passes
// audio through its FIFO during initial enrollment; after activation it never
// falls back to raw audio or returns to enrollment.
func (client *Client) Process(ctx context.Context, pcm []byte, rate uint32) ([]byte, error) {
	if client.failed {
		return nil, errors.New("noise filter session failed; start a new session")
	}
	if rate != 16000 && rate != 24000 && rate != 48000 {
		return nil, errors.New("noise filter supports mono PCM16 at 16, 24, or 48 kHz")
	}
	if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > 1<<20 {
		return nil, errors.New("noise filter requires 1..524288 complete PCM16 samples")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if client.target != nil {
		return client.target.process(ctx, pcm, rate)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(client.config.TimeoutMS)*time.Millisecond)
	defer cancel()
	return client.processPacket(ctx, pcm, rate)
}

func (client *Client) processPacket(ctx context.Context, pcm []byte, rate uint32) ([]byte, error) {
	result := make([]byte, 0, len(pcm))
	chunkBytes := int(rate) / 10 * 2
	for offset := 0; offset < len(pcm); offset += chunkBytes {
		end := min(offset+chunkBytes, len(pcm))
		filtered, err := client.processChunk(ctx, pcm[offset:end], rate)
		if err != nil {
			if client.target == nil {
				client.failed = true
			}
			return nil, err
		}
		result = append(result, filtered...)
	}
	return result, nil
}

func (client *Client) processChunk(parent context.Context, pcm []byte, rate uint32) ([]byte, error) {
	ctx := parent
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.config.URL+"/v1/filter/"+client.id, bytes.NewReader(pcm))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Sample-Rate", strconv.FormatUint(uint64(rate), 10))
	request.Header.Set("X-Sequence", strconv.FormatUint(client.sequence, 10))
	response, err := client.http.Do(request)
	if err != nil {
		return nil, fmt.Errorf("pre-ASR noise filter: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pre-ASR noise filter returned HTTP %d", response.StatusCode)
	}
	delay := "20"
	if client.config.ModelName() == "real-tse" {
		delay = "65"
	}
	if response.Header.Get("X-Sequence") != request.Header.Get("X-Sequence") || response.Header.Get("X-Filter-Model") != client.config.ModelName() || response.Header.Get("X-Audio-Delay-MS") != delay {
		return nil, errors.New("noise filter response contract mismatch")
	}
	if client.config.ModelName() == "real-tse" {
		phase := map[string]int{"waiting-for-speech": 1, "collecting-reference": 2, "preparing-reference": 3, "extracting": 4}[response.Header.Get("X-Target-Voice-State")]
		if phase == 0 || phase < client.targetPhase {
			return nil, errors.New("target voice enrollment state missing or regressed")
		}
		client.targetPhase = phase
	}
	filtered, err := io.ReadAll(io.LimitReader(response.Body, int64(len(pcm)+1)))
	if err != nil {
		return nil, err
	}
	if len(filtered) != len(pcm) {
		return nil, errors.New("noise filter changed PCM sample count")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client.sequence++
	return filtered, nil
}

func (client *Client) Close() {
	if client.target != nil {
		client.target.close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(client.config.TimeoutMS)*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, client.config.URL+"/v1/filter/"+client.id, nil)
	if err == nil {
		if response, err := client.http.Do(request); err == nil {
			response.Body.Close()
		}
	}
	client.http.CloseIdleConnections()
}
