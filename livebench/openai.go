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
	Provider      string
	Architecture  string
	Profile       string
	Tools         []RealtimeTool
	ExecuteTool   func(context.Context, string, json.RawMessage) (json.RawMessage, error)
	// AwaitTerminalResponse makes TailDuration a hard upper bound rather than a
	// fixed collection window. The adapter returns as soon as a completed,
	// non-tool response closes the post-input trajectory. This is appropriate
	// for tool-use tasks, where a fixed tail can silently truncate reasoning or
	// function-result resumption.
	AwaitTerminalResponse bool
}

type OpenAIAdapter struct {
	config OpenAIConfig
}

func NewOpenAIAdapter(config OpenAIConfig) (*OpenAIAdapter, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("OpenAI-compatible Realtime API key is required")
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
	if config.Provider == "" {
		config.Provider = "openai"
	}
	if config.Architecture == "" {
		config.Architecture = "native-audio-to-audio"
	}
	if config.Profile == "" {
		config.Profile = "fdb-v1.5-server-vad-v1"
	}
	if config.ChunkDuration < 20*time.Millisecond || config.ChunkDuration > time.Second {
		return nil, errors.New("OpenAI chunk duration must be between 20ms and 1s")
	}
	seenTools := make(map[string]struct{}, len(config.Tools))
	for index := range config.Tools {
		tool := &config.Tools[index]
		if tool.Type == "" {
			tool.Type = "function"
		}
		tool.Name = strings.TrimSpace(tool.Name)
		tool.Description = strings.TrimSpace(tool.Description)
		if tool.Type != "function" || tool.Name == "" || tool.Description == "" {
			return nil, fmt.Errorf("Realtime tool %d requires function type, name, and description", index)
		}
		if _, exists := seenTools[tool.Name]; exists {
			return nil, fmt.Errorf("duplicate Realtime tool %q", tool.Name)
		}
		seenTools[tool.Name] = struct{}{}
		var schema map[string]json.RawMessage
		if len(tool.Parameters) == 0 || json.Unmarshal(tool.Parameters, &schema) != nil || schema == nil {
			return nil, fmt.Errorf("Realtime tool %q parameters must be a JSON object", tool.Name)
		}
	}
	if len(config.Tools) != 0 && config.ExecuteTool == nil {
		return nil, errors.New("Realtime tools require an external executor")
	}
	return &OpenAIAdapter{config: config}, nil
}

func (adapter *OpenAIAdapter) Descriptor() Descriptor {
	return Descriptor{
		Provider: adapter.config.Provider, Model: adapter.config.Model, Transport: "websocket-openai-realtime",
		Architecture: adapter.config.Architecture, Profile: adapter.config.Profile,
		InputSampleRate: 24_000, OutputSampleRate: 24_000,
	}
}

type openAIEnvelope struct {
	Type       string   `json:"type"`
	EventID    string   `json:"event_id"`
	Delta      string   `json:"delta"`
	Transcript string   `json:"transcript"`
	AudioEndMS *float64 `json:"audio_end_ms"`
	Error      *struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Response *struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"output"`
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

type openAIToolBatch struct {
	calls []openAIToolRequest
}

type openAIToolRequest struct {
	callID    string
	name      string
	arguments json.RawMessage
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
	var writeMu sync.Mutex
	writeEvent := func(writeCtx context.Context, value any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return writeJSON(writeCtx, connection, value)
	}

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
	var inputTranscripts []string
	var userSpeechEndMS *float64
	var userSpeechEndsMS []float64
	var toolCalls []RealtimeToolCall
	var inputComplete bool
	var speechActive bool
	var terminalEligible bool
	toolBatches := make(chan openAIToolBatch, 16)
	terminalResponse := make(chan struct{})
	var terminalOnce sync.Once
	maybeSignalTerminal := func() {
		stateMu.Lock()
		ready := inputComplete && !speechActive && terminalEligible
		stateMu.Unlock()
		if ready {
			terminalOnce.Do(func() { close(terminalResponse) })
		}
	}

	go func() {
		for {
			select {
			case <-readCtx.Done():
				return
			case batch := <-toolBatches:
				for _, call := range batch.calls {
					stateMu.Lock()
					currentOrigin := streamOrigin
					stateMu.Unlock()
					requested := time.Since(currentOrigin)
					output, executeErr := adapter.config.ExecuteTool(readCtx, call.name, call.arguments)
					if executeErr != nil {
						select {
						case receiveErr <- fmt.Errorf("execute Realtime tool %s: %w", call.name, executeErr):
						default:
						}
						return
					}
					if !json.Valid(output) {
						output, _ = json.Marshal(string(output))
					}
					completed := time.Since(currentOrigin)
					stateMu.Lock()
					toolCalls = append(toolCalls, RealtimeToolCall{
						CallID: call.callID, Name: call.name, Arguments: call.arguments, Output: output,
						RequestedMS: milliseconds(requested), CompletedMS: milliseconds(completed),
					})
					stateMu.Unlock()
					result := map[string]any{
						"type": "conversation.item.create",
						"item": map[string]any{"type": "function_call_output", "call_id": call.callID, "output": string(output)},
					}
					if err := writeEvent(readCtx, result); err != nil {
						select {
						case receiveErr <- fmt.Errorf("send Realtime tool result: %w", err):
						default:
						}
						return
					}
					recorder.add("client", "conversation.item.create", "", compactPayload(result), nil)
				}
				resume := map[string]any{"type": "response.create"}
				if err := writeEvent(readCtx, resume); err != nil {
					select {
					case receiveErr <- fmt.Errorf("resume Realtime response: %w", err):
					default:
					}
					return
				}
				recorder.add("client", "response.create", "", compactPayload(resume), nil)
			}
		}
	}()

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
			case "response.created":
				stateMu.Lock()
				terminalEligible = false
				stateMu.Unlock()
			case "input_audio_buffer.speech_started":
				// With a WebSocket transport, server VAD cancels generation but
				// cannot clear audio already queued by the client for playback.
				// Mark the exact arrival instant so offline alignment models the
				// required immediate playback stop.
				stateMu.Lock()
				speechActive = true
				terminalEligible = false
				if !streamOrigin.IsZero() {
					chunks = append(chunks, OutputChunk{Arrival: time.Since(streamOrigin), Flush: true})
				}
				stateMu.Unlock()
			case "input_audio_buffer.speech_stopped":
				stateMu.Lock()
				speechActive = false
				if envelope.AudioEndMS != nil {
					value := *envelope.AudioEndMS
					userSpeechEndMS = &value
					userSpeechEndsMS = append(userSpeechEndsMS, value)
				}
				stateMu.Unlock()
			case "conversation.item.input_audio_transcription.completed":
				if strings.TrimSpace(envelope.Transcript) != "" {
					stateMu.Lock()
					inputTranscripts = append(inputTranscripts, envelope.Transcript)
					stateMu.Unlock()
				}
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
				batch := openAIToolBatch{}
				if envelope.Response != nil && len(envelope.Response.Output) != 0 {
					for _, item := range envelope.Response.Output {
						if item.Type != "function_call" || strings.TrimSpace(item.CallID) == "" || strings.TrimSpace(item.Name) == "" {
							continue
						}
						arguments := json.RawMessage(item.Arguments)
						if !json.Valid(arguments) {
							select {
							case receiveErr <- fmt.Errorf("Realtime tool %s returned invalid arguments", item.Name):
							default:
							}
							return
						}
						batch.calls = append(batch.calls, openAIToolRequest{callID: item.CallID, name: item.Name, arguments: arguments})
					}
				}
				if len(batch.calls) != 0 {
					stateMu.Lock()
					terminalEligible = false
					stateMu.Unlock()
					select {
					case toolBatches <- batch:
					case <-readCtx.Done():
						return
					}
				} else if envelope.Response != nil && (envelope.Response.Status == "" || envelope.Response.Status == "completed") {
					stateMu.Lock()
					terminalEligible = true
					stateMu.Unlock()
					maybeSignalTerminal()
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
	if len(adapter.config.Tools) != 0 {
		session := update["session"].(map[string]any)
		session["tools"] = adapter.config.Tools
		session["tool_choice"] = "auto"
	}
	if err := writeEvent(ctx, update); err != nil {
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

	if err := streamOpenAIAudio(ctx, writeEvent, recorder, providerInput, adapter.config.ChunkDuration); err != nil {
		return SessionResult{}, err
	}
	stateMu.Lock()
	inputComplete = true
	stateMu.Unlock()
	maybeSignalTerminal()
	if err := waitForOpenAICompletion(ctx, receiveErr, terminalResponse, adapter.config.TailDuration, adapter.config.AwaitTerminalResponse); err != nil {
		return SessionResult{}, fmt.Errorf("OpenAI receive: %w", err)
	}
	elapsed := time.Since(streamOrigin)
	cancelRead()

	stateMu.Lock()
	resultChunks := append([]OutputChunk(nil), chunks...)
	resultTranscript := transcript.String()
	resultFirstAudio := firstAudio
	resultUsage := usage
	resultInputTranscripts := append([]string(nil), inputTranscripts...)
	resultUserSpeechEndMS := userSpeechEndMS
	resultUserSpeechEndsMS := append([]float64(nil), userSpeechEndsMS...)
	resultToolCalls := append([]RealtimeToolCall(nil), toolCalls...)
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
		OutputTranscript: resultTranscript, InputTranscripts: resultInputTranscripts, UserSpeechEndMS: resultUserSpeechEndMS,
		UserSpeechEndsMS: resultUserSpeechEndsMS, ToolCalls: resultToolCalls, Usage: resultUsage, Chunks: resultChunks, Events: recorder.snapshot(),
	}, nil
}

func streamOpenAIAudio(ctx context.Context, writeEvent func(context.Context, any) error, recorder *wireRecorder, audio Audio, chunkDuration time.Duration) error {
	chunkBytes := int(chunkDuration*time.Duration(audio.SampleRateHz)/time.Second) * 2
	if chunkBytes <= 0 {
		return errors.New("OpenAI audio chunk size is zero")
	}
	streamStart := time.Now()
	for offset := 0; offset < len(audio.PCM16); offset += chunkBytes {
		end := min(offset+chunkBytes, len(audio.PCM16))
		chunk := audio.PCM16[offset:end]
		message := map[string]any{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(chunk)}
		if err := writeEvent(ctx, message); err != nil {
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

func waitForOpenAICompletion(ctx context.Context, receiveErr <-chan error, terminal <-chan struct{}, duration time.Duration, awaitTerminal bool) error {
	if !awaitTerminal {
		return waitForTail(ctx, receiveErr, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-terminal:
		return nil
	case err := <-receiveErr:
		return err
	case <-timer.C:
		return fmt.Errorf("timed out after %s waiting for a terminal Realtime response", duration)
	case <-ctx.Done():
		return ctx.Err()
	}
}
