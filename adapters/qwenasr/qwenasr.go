// Package qwenasr adapts the stateful Qwen3-ASR streaming service to the
// stable OpenRealtime perception interface.
//
// The adapter targets the official qwen-asr start/chunk/finish HTTP contract.
// A client instance represents exactly one utterance and serializes all calls;
// create a new instance for every concurrently recognized stream.
package qwenasr

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
	"sync"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/internal/httpclient"
	"github.com/bojieli/OpenRealtime/pcm"
)

const (
	// DefaultBaseURL is the local endpoint used by the qwen-asr streaming
	// service. It is intentionally separate from the local language model port.
	DefaultBaseURL = "http://127.0.0.1:8001"
	// DefaultModel records the initial local ASR condition in descriptors and
	// benchmark artifacts. Model selection itself belongs to the server process.
	DefaultModel = "Qwen/Qwen3-ASR-0.6B"

	serverSampleRate = uint32(16_000)
	defaultTimeout   = 30 * time.Second
	defaultMaxFrame  = 4 << 20
	maxResponseBody  = 1 << 20
)

// Config configures one Qwen3-ASR utterance session. Headers are copied at
// construction. BearerToken and headers are never included in descriptors or
// errors.
type Config struct {
	BaseURL        string
	Model          string
	BearerToken    string
	Headers        http.Header
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	MaxFrameBytes  int
}

// Adapter owns the remote streaming state for one utterance.
type Adapter struct {
	config     Config
	descriptor v1.Descriptor

	mu               sync.Mutex
	sessionID        string
	resampler        *pcm.Resampler
	inputRate        uint32
	haveFrame        bool
	nextFrameIndex   uint64
	nextSourceSample uint64
	lastText         string
	lastEmittedText  string
	language         string
	revisionID       uint64
	finalized        bool
	terminalErr      error
}

// New validates configuration and returns a fresh single-utterance adapter.
func New(config Config) (*Adapter, error) {
	if config.BaseURL == "" {
		config.BaseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("Qwen3-ASR base URL must be absolute")
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	if config.Model == "" {
		config.Model = DefaultModel
	}
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		return nil, errors.New("Qwen3-ASR model name is required")
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("Qwen3-ASR request timeout cannot be negative")
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultTimeout
	}
	if config.MaxFrameBytes < 0 {
		return nil, errors.New("Qwen3-ASR maximum frame size cannot be negative")
	}
	if config.MaxFrameBytes == 0 {
		config.MaxFrameBytes = defaultMaxFrame
	}
	if config.HTTPClient == nil {
		config.HTTPClient = httpclient.Shared()
	}
	config.Headers = config.Headers.Clone()
	config.BearerToken = strings.TrimSpace(config.BearerToken)

	descriptor := v1.Descriptor{
		Name:    "qwen3-asr-streaming/" + config.Model,
		Version: "qwen-asr-0.0.6",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true,
			v1.CapabilityRevisions:      true,
			v1.CapabilityCancellation:   true,
		},
	}
	return &Adapter{config: config, descriptor: descriptor}, nil
}

// Descriptor implements api/v1.PerceptionProvider.
func (adapter *Adapter) Descriptor() v1.Descriptor {
	descriptor := adapter.descriptor
	descriptor.Capabilities = cloneCapabilities(descriptor.Capabilities)
	return descriptor
}

// Language returns the most recent server-reported language. Language is
// provider metadata rather than part of the stable perception revision.
func (adapter *Adapter) Language() string {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.language
}

// PushFrame resamples a contiguous PCM16LE frame to 16 kHz float32, advances
// the remote streaming state once, and returns a revision only when text has
// changed. Partial Qwen output remains UnstableText because the server does not
// expose a stable-prefix boundary.
func (adapter *Adapter) PushFrame(ctx context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()

	if err := adapter.ready(); err != nil {
		return nil, err
	}
	if err := adapter.validateFrame(frame); err != nil {
		return nil, err
	}
	if err := adapter.ensureSession(ctx); err != nil {
		return nil, err
	}
	if adapter.resampler == nil {
		resampler, err := pcm.NewResampler(frame.SampleRateHz, serverSampleRate)
		if err != nil {
			return nil, err
		}
		adapter.resampler = resampler
		adapter.inputRate = frame.SampleRateHz
	}

	converted, err := adapter.resampler.Push(frame.PCM16LE)
	if err != nil {
		return nil, adapter.fail(fmt.Errorf("resample Qwen3-ASR frame: %w", err))
	}
	endSample := frame.SampleOffset + uint64(len(frame.PCM16LE)/2)
	adapter.haveFrame = true
	adapter.nextFrameIndex = frame.Index + 1
	adapter.nextSourceSample = endSample
	if len(converted) == 0 {
		return nil, nil
	}

	result, err := adapter.pushChunk(ctx, pcm16ToFloat32(converted))
	if err != nil {
		return nil, adapter.fail(err)
	}
	adapter.language = result.Language
	adapter.lastText = result.Text
	if result.Text == adapter.lastEmittedText {
		return nil, nil
	}
	revision := adapter.revision(result.Text, endSample, false)
	return []v1.PerceptionRevision{revision}, nil
}

// Finalize flushes the resampler and asks Qwen to resolve the final mutable
// suffix. It always emits one final revision, even if its text is unchanged.
func (adapter *Adapter) Finalize(ctx context.Context, sourceSample uint64) (v1.PerceptionRevision, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()

	if err := adapter.ready(); err != nil {
		return v1.PerceptionRevision{}, err
	}
	if !adapter.haveFrame || adapter.resampler == nil {
		return v1.PerceptionRevision{}, errors.New("Qwen3-ASR cannot finalize an empty utterance")
	}
	if sourceSample != adapter.nextSourceSample {
		return v1.PerceptionRevision{}, fmt.Errorf("Qwen3-ASR final source sample is %d; expected %d", sourceSample, adapter.nextSourceSample)
	}
	converted, err := adapter.resampler.Finalize()
	if err != nil {
		return v1.PerceptionRevision{}, adapter.fail(fmt.Errorf("finalize Qwen3-ASR resampler: %w", err))
	}
	if len(converted) != 0 {
		result, err := adapter.pushChunk(ctx, pcm16ToFloat32(converted))
		if err != nil {
			return v1.PerceptionRevision{}, adapter.fail(err)
		}
		adapter.language = result.Language
		adapter.lastText = result.Text
	}
	result, err := adapter.finish(ctx)
	if err != nil {
		return v1.PerceptionRevision{}, adapter.fail(err)
	}
	adapter.language = result.Language
	adapter.lastText = result.Text
	adapter.finalized = true
	return adapter.revision(result.Text, sourceSample, true), nil
}

func (adapter *Adapter) ready() error {
	if adapter.terminalErr != nil {
		return fmt.Errorf("Qwen3-ASR session is unusable after an indeterminate remote update: %w", adapter.terminalErr)
	}
	if adapter.finalized {
		return errors.New("Qwen3-ASR session is finalized")
	}
	return nil
}

func (adapter *Adapter) validateFrame(frame v1.AudioFrame) error {
	if frame.SampleRateHz == 0 || len(frame.PCM16LE) == 0 || len(frame.PCM16LE)%2 != 0 {
		return errors.New("Qwen3-ASR frame requires non-empty even-length PCM16 and a sample rate")
	}
	if len(frame.PCM16LE) > adapter.config.MaxFrameBytes {
		return fmt.Errorf("Qwen3-ASR frame has %d bytes; maximum is %d", len(frame.PCM16LE), adapter.config.MaxFrameBytes)
	}
	if uint64(len(frame.PCM16LE)/2) > math.MaxUint64-frame.SampleOffset {
		return errors.New("Qwen3-ASR frame end sample overflows")
	}
	if adapter.haveFrame {
		if frame.Index != adapter.nextFrameIndex {
			return fmt.Errorf("Qwen3-ASR frame index is %d; expected %d", frame.Index, adapter.nextFrameIndex)
		}
		if frame.SampleOffset != adapter.nextSourceSample {
			return fmt.Errorf("Qwen3-ASR frame starts at sample %d; expected %d", frame.SampleOffset, adapter.nextSourceSample)
		}
		if frame.SampleRateHz != adapter.inputRate {
			return fmt.Errorf("Qwen3-ASR sample rate changed from %d to %d", adapter.inputRate, frame.SampleRateHz)
		}
	}
	return nil
}

type startResponse struct {
	SessionID string `json:"session_id"`
	Error     string `json:"error,omitempty"`
}

type transcriptResponse struct {
	Language string `json:"language"`
	Text     string `json:"text"`
	Error    string `json:"error,omitempty"`
}

func (adapter *Adapter) ensureSession(ctx context.Context) error {
	if adapter.sessionID != "" {
		return nil
	}
	var response startResponse
	if err := adapter.do(ctx, http.MethodPost, "/api/start", "application/json", nil, &response); err != nil {
		return fmt.Errorf("start Qwen3-ASR session: %w", err)
	}
	response.SessionID = strings.TrimSpace(response.SessionID)
	if response.Error != "" {
		return fmt.Errorf("start Qwen3-ASR session: %s", response.Error)
	}
	if response.SessionID == "" {
		return errors.New("start Qwen3-ASR session: server returned an empty session ID")
	}
	adapter.sessionID = response.SessionID
	return nil
}

func (adapter *Adapter) pushChunk(ctx context.Context, audio []byte) (transcriptResponse, error) {
	path := "/api/chunk?session_id=" + url.QueryEscape(adapter.sessionID)
	var response transcriptResponse
	if err := adapter.do(ctx, http.MethodPost, path, "application/octet-stream", audio, &response); err != nil {
		return transcriptResponse{}, fmt.Errorf("push Qwen3-ASR chunk: %w", err)
	}
	if response.Error != "" {
		return transcriptResponse{}, fmt.Errorf("push Qwen3-ASR chunk: %s", response.Error)
	}
	return response, nil
}

func (adapter *Adapter) finish(ctx context.Context) (transcriptResponse, error) {
	path := "/api/finish?session_id=" + url.QueryEscape(adapter.sessionID)
	var response transcriptResponse
	if err := adapter.do(ctx, http.MethodPost, path, "application/json", nil, &response); err != nil {
		return transcriptResponse{}, fmt.Errorf("finish Qwen3-ASR session: %w", err)
	}
	if response.Error != "" {
		return transcriptResponse{}, fmt.Errorf("finish Qwen3-ASR session: %s", response.Error)
	}
	return response, nil
}

func (adapter *Adapter) do(ctx context.Context, method, path, contentType string, body []byte, output any) error {
	requestContext := ctx
	if adapter.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		requestContext, cancel = context.WithTimeout(ctx, adapter.config.RequestTimeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestContext, method, adapter.config.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	for name, values := range adapter.config.Headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "OpenRealtime/qwen3-asr")
	if adapter.config.BearerToken != "" {
		request.Header.Set("Authorization", "Bearer "+adapter.config.BearerToken)
	}
	response, err := adapter.config.HTTPClient.Do(request)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxResponseBody))
		return fmt.Errorf("server returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxResponseBody))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (adapter *Adapter) revision(text string, sourceSample uint64, final bool) v1.PerceptionRevision {
	adapter.revisionID++
	revision := v1.PerceptionRevision{
		RevisionID:   adapter.revisionID,
		SourceSample: sourceSample,
		Delta:        textDelta(adapter.lastEmittedText, text),
		Final:        final,
	}
	if final {
		revision.StableText = text
	} else {
		revision.UnstableText = text
	}
	adapter.lastEmittedText = text
	return revision
}

func (adapter *Adapter) fail(err error) error {
	adapter.terminalErr = err
	return err
}

func pcm16ToFloat32(input []byte) []byte {
	output := make([]byte, len(input)*2)
	for offset := 0; offset < len(input); offset += 2 {
		sample := int16(binary.LittleEndian.Uint16(input[offset : offset+2]))
		value := float32(sample) / 32768
		binary.LittleEndian.PutUint32(output[offset*2:offset*2+4], math.Float32bits(value))
	}
	return output
}

func textDelta(previous, current string) string {
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	return current
}

func cloneCapabilities(input v1.Capabilities) v1.Capabilities {
	output := make(v1.Capabilities, len(input))
	for capability, enabled := range input {
		output[capability] = enabled
	}
	return output
}

var _ v1.PerceptionProvider = (*Adapter)(nil)
