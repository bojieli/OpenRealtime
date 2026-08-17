package livebench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const DefaultGPT4oRealtimeModel = "gpt-4o-realtime-preview-2025-06-03"

type OpenAIConfig struct {
	APIKey        string
	Model         string
	Endpoint      string
	Voice         string
	Instructions  string
	ChunkDuration time.Duration
	TailDuration  time.Duration
}

type OpenAIAdapter struct {
	config OpenAIConfig
}

func NewOpenAIAdapter(config OpenAIConfig) (*OpenAIAdapter, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("OPENAI_API_KEY is required")
	}
	if config.Model == "" {
		config.Model = DefaultGPT4oRealtimeModel
	}
	if config.Endpoint == "" {
		config.Endpoint = "wss://api.openai.com/v1/realtime"
	}
	if config.Voice == "" {
		config.Voice = "alloy"
	}
	if config.Instructions == "" {
		config.Instructions = "You are a helpful spoken-dialogue assistant. Respond naturally and concisely."
	}
	if config.ChunkDuration == 0 {
		config.ChunkDuration = 100 * time.Millisecond
	}
	if config.TailDuration == 0 {
		config.TailDuration = 5 * time.Second
	}
	if config.ChunkDuration < 20*time.Millisecond || config.ChunkDuration > time.Second {
		return nil, errors.New("OpenAI chunk duration must be between 20ms and 1s")
	}
	return &OpenAIAdapter{config: config}, nil
}

func (adapter *OpenAIAdapter) Descriptor() Descriptor {
	return Descriptor{
		Provider: "openai", Model: adapter.config.Model, Transport: "websocket",
		Architecture: "native-audio-to-audio", Profile: "fdb-v1.5-server-vad-v1",
		InputSampleRate: 24_000, OutputSampleRate: 24_000,
	}
}

type openAIEnvelope struct {
	Type       string `json:"type"`
	EventID    string `json:"event_id"`
	Delta      string `json:"delta"`
	Transcript string `json:"transcript"`
	Error      *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Response *struct {
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			InputDetails struct {
				AudioTokens int64 `json:"audio_tokens"`
			} `json:"input_token_details"`
			OutputDetails struct {
				AudioTokens int64 `json:"audio_tokens"`
			} `json:"output_token_details"`
		} `json:"usage"`
	} `json:"response"`
}

func (adapter *OpenAIAdapter) Run(ctx context.Context, input Audio) (SessionResult, error) {
	providerInput, err := Resample(input, 24_000)
	if err != nil {
		return SessionResult{}, fmt.Errorf("prepare OpenAI input: %w", err)
	}
	descriptor := adapter.Descriptor()
	startedAt := time.Now().UTC()
	origin := time.Now()
	recorder := newWireRecorder(origin)

	endpoint, err := url.Parse(adapter.config.Endpoint)
	if err != nil {
		return SessionResult{}, fmt.Errorf("parse OpenAI endpoint: %w", err)
	}
	query := endpoint.Query()
	query.Set("model", adapter.config.Model)
	endpoint.RawQuery = query.Encode()
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+adapter.config.APIKey)
	connection, response, err := websocket.Dial(ctx, endpoint.String(), &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		if response != nil {
			return SessionResult{}, fmt.Errorf("connect OpenAI Realtime (HTTP %d): %w", response.StatusCode, err)
		}
		return SessionResult{}, fmt.Errorf("connect OpenAI Realtime: %w", err)
	}
	defer connection.CloseNow()
	connection.SetReadLimit(16 << 20)

	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	ready := make(chan struct{})
	receiveErr := make(chan error, 1)
	var readyOnce sync.Once
	var stateMu sync.Mutex
	var chunks []OutputChunk
	var firstAudio *float64
	var transcript strings.Builder
	var usage Usage
	var streamOrigin time.Time

	go func() {
		for {
			messageType, data, readErr := connection.Read(readCtx)
			if readErr != nil {
				if readCtx.Err() == nil {
					select {
					case receiveErr <- readErr:
					default:
					}
				}
				return
			}
			if messageType != websocket.MessageText {
				continue
			}
			var envelope openAIEnvelope
			if jsonErr := json.Unmarshal(data, &envelope); jsonErr != nil {
				select {
				case receiveErr <- fmt.Errorf("decode OpenAI event: %w", jsonErr):
				default:
				}
				return
			}
			if envelope.Type == "response.output_audio.delta" {
				audio, decodeErr := base64.StdEncoding.DecodeString(envelope.Delta)
				if decodeErr != nil {
					select {
					case receiveErr <- fmt.Errorf("decode OpenAI audio: %w", decodeErr):
					default:
					}
					return
				}
				stateMu.Lock()
				arrival := time.Duration(0)
				if !streamOrigin.IsZero() {
					arrival = time.Since(streamOrigin)
				}
				chunks = append(chunks, OutputChunk{Arrival: arrival, PCM16: audio})
				if firstAudio == nil {
					value := milliseconds(arrival)
					firstAudio = &value
				}
				stateMu.Unlock()
				recorder.add("server", envelope.Type, envelope.EventID, compactPayload(map[string]any{"type": envelope.Type}), audio)
				continue
			}
			recorder.add("server", envelope.Type, envelope.EventID, data, nil)
			switch envelope.Type {
			case "session.updated":
				readyOnce.Do(func() { close(ready) })
			case "input_audio_buffer.speech_started":
				// With a WebSocket transport, server VAD cancels generation but
				// cannot clear audio already queued by the client for playback.
				// Mark the exact arrival instant so offline alignment models the
				// required immediate playback stop.
				stateMu.Lock()
				if !streamOrigin.IsZero() {
					chunks = append(chunks, OutputChunk{Arrival: time.Since(streamOrigin), Flush: true})
				}
				stateMu.Unlock()
			case "response.output_audio_transcript.delta":
				stateMu.Lock()
				transcript.WriteString(envelope.Delta)
				stateMu.Unlock()
			case "response.done":
				if envelope.Response != nil && envelope.Response.Usage != nil {
					providerUsage, _ := json.Marshal(envelope.Response.Usage)
					stateMu.Lock()
					usage.InputTokens += envelope.Response.Usage.InputTokens
					usage.OutputTokens += envelope.Response.Usage.OutputTokens
					usage.InputAudioTokens += envelope.Response.Usage.InputDetails.AudioTokens
					usage.OutputAudioTokens += envelope.Response.Usage.OutputDetails.AudioTokens
					usage.Complete = true
					usage.SessionsExpected = 1
					usage.SessionsObserved = 1
					usage.Provider = providerUsage
					stateMu.Unlock()
				}
			case "error":
				message := "unspecified OpenAI Realtime error"
				if envelope.Error != nil {
					message = strings.TrimSpace(envelope.Error.Code + ": " + envelope.Error.Message)
				}
				select {
				case receiveErr <- errors.New(message):
				default:
				}
				return
			}
		}
	}()

	update := map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime", "output_modalities": []string{"audio"}, "instructions": adapter.config.Instructions,
			"audio": map[string]any{
				"input": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 24_000},
					"turn_detection": map[string]any{
						"type": "server_vad", "threshold": 0.5, "prefix_padding_ms": 300,
						"silence_duration_ms": 500, "create_response": true, "interrupt_response": true,
					},
				},
				"output": map[string]any{
					"format": map[string]any{"type": "audio/pcm", "rate": 24_000}, "voice": adapter.config.Voice,
				},
			},
		},
	}
	if err := writeJSON(ctx, connection, update); err != nil {
		return SessionResult{}, fmt.Errorf("configure OpenAI session: %w", err)
	}
	recorder.add("client", "session.update", "", compactPayload(update), nil)

	setupTimer := time.NewTimer(15 * time.Second)
	defer setupTimer.Stop()
	select {
	case <-ready:
	case err := <-receiveErr:
		return SessionResult{}, fmt.Errorf("OpenAI setup: %w", err)
	case <-setupTimer.C:
		return SessionResult{}, errors.New("timed out waiting for OpenAI session.updated")
	case <-ctx.Done():
		return SessionResult{}, ctx.Err()
	}
	setupDuration := time.Since(origin)
	stateMu.Lock()
	streamOrigin = time.Now()
	stateMu.Unlock()

	if err := streamOpenAIAudio(ctx, connection, recorder, providerInput, adapter.config.ChunkDuration); err != nil {
		return SessionResult{}, err
	}
	if err := waitForTail(ctx, receiveErr, adapter.config.TailDuration); err != nil {
		return SessionResult{}, fmt.Errorf("OpenAI receive: %w", err)
	}
	elapsed := time.Since(streamOrigin)
	cancelRead()

	stateMu.Lock()
	resultChunks := append([]OutputChunk(nil), chunks...)
	resultTranscript := transcript.String()
	resultFirstAudio := firstAudio
	resultUsage := usage
	stateMu.Unlock()
	var outputBytes int
	for _, chunk := range resultChunks {
		outputBytes += len(chunk.PCM16)
	}
	return SessionResult{
		Descriptor: descriptor, StartedAt: startedAt, ConnectionSetupMS: milliseconds(setupDuration),
		ConnectionCount: 1,
		InputDurationMS: milliseconds(providerInput.Duration()), ElapsedMS: milliseconds(elapsed),
		FirstAudioMS: resultFirstAudio, OutputAudioMS: milliseconds(time.Duration(outputBytes/2) * time.Second / 24_000),
		OutputTranscript: resultTranscript, Usage: resultUsage, Chunks: resultChunks, Events: recorder.snapshot(),
	}, nil
}

func streamOpenAIAudio(ctx context.Context, connection *websocket.Conn, recorder *wireRecorder, audio Audio, chunkDuration time.Duration) error {
	chunkBytes := int(chunkDuration*time.Duration(audio.SampleRateHz)/time.Second) * 2
	if chunkBytes <= 0 {
		return errors.New("OpenAI audio chunk size is zero")
	}
	streamStart := time.Now()
	for offset := 0; offset < len(audio.PCM16); offset += chunkBytes {
		end := min(offset+chunkBytes, len(audio.PCM16))
		chunk := audio.PCM16[offset:end]
		message := map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(chunk)}
		if err := writeJSON(ctx, connection, message); err != nil {
			return fmt.Errorf("send OpenAI audio at byte %d: %w", offset, err)
		}
		recorder.add("client", "input_audio_buffer.append", "", compactPayload(map[string]any{"type": "input_audio_buffer.append"}), chunk)
		nextSamples := end / 2
		deadline := streamStart.Add(time.Duration(nextSamples) * time.Second / time.Duration(audio.SampleRateHz))
		if err := sleepUntil(ctx, deadline); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(ctx context.Context, connection *websocket.Conn, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return connection.Write(ctx, websocket.MessageText, data)
}

func sleepUntil(ctx context.Context, deadline time.Time) error {
	duration := time.Until(deadline)
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func waitForTail(ctx context.Context, receiveErr <-chan error, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case err := <-receiveErr:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
