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

const DefaultGeminiLiveModel = "gemini-3.1-flash-live-preview"

type GeminiConfig struct {
	APIKey          string
	Model           string
	Endpoint        string
	Instructions    string
	Transcription   bool
	ThinkingLevel   string
	ChunkDuration   time.Duration
	TailDuration    time.Duration
	ConnectAttempts int
	RetryBaseDelay  time.Duration
}

type GeminiAdapter struct {
	config GeminiConfig
}

func NewGeminiAdapter(config GeminiConfig) (*GeminiAdapter, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("GEMINI_API_KEY is required")
	}
	if config.Model == "" {
		config.Model = DefaultGeminiLiveModel
	}
	if config.Endpoint == "" {
		config.Endpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"
	}
	if config.ThinkingLevel == "" {
		config.ThinkingLevel = "minimal"
	}
	if config.ChunkDuration == 0 {
		// Full-Duplex-Bench's pinned Gemini 3.1 runner sends 1,024
		// samples at 16 kHz per frame.
		config.ChunkDuration = 64 * time.Millisecond
	}
	if config.TailDuration == 0 {
		config.TailDuration = 5 * time.Second
	}
	if config.ConnectAttempts == 0 {
		config.ConnectAttempts = 4
	}
	if config.RetryBaseDelay == 0 {
		config.RetryBaseDelay = 250 * time.Millisecond
	}
	if config.ChunkDuration < 20*time.Millisecond || config.ChunkDuration > time.Second {
		return nil, errors.New("Gemini chunk duration must be between 20ms and 1s")
	}
	if config.ConnectAttempts < 1 || config.ConnectAttempts > 10 || config.RetryBaseDelay < 0 {
		return nil, errors.New("Gemini connection retry configuration is invalid")
	}
	return &GeminiAdapter{config: config}, nil
}

func (adapter *GeminiAdapter) Descriptor() Descriptor {
	return Descriptor{
		Provider: "google", Model: adapter.config.Model, Transport: "websocket-v1beta",
		Architecture: "native-audio-to-audio", Profile: "fdb-v1.5-minimal-multisession-v1",
		InputSampleRate: 16_000, OutputSampleRate: 24_000,
	}
}

type geminiEnvelope struct {
	SetupComplete json.RawMessage `json:"setupComplete"`
	ServerContent *struct {
		Interrupted        bool `json:"interrupted"`
		TurnComplete       bool `json:"turnComplete"`
		GenerationComplete bool `json:"generationComplete"`
		InputTranscription *struct {
			Text string `json:"text"`
		} `json:"inputTranscription"`
		OutputTranscription *struct {
			Text string `json:"text"`
		} `json:"outputTranscription"`
		ModelTurn *struct {
			Parts []struct {
				InlineData *struct {
					Data     string `json:"data"`
					MIMEType string `json:"mimeType"`
				} `json:"inlineData"`
			} `json:"parts"`
		} `json:"modelTurn"`
	} `json:"serverContent"`
	UsageMetadata json.RawMessage `json:"usageMetadata"`
	GoAway        json.RawMessage `json:"goAway"`
	Error         *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error"`
}

func (adapter *GeminiAdapter) Run(ctx context.Context, input Audio) (SessionResult, error) {
	providerInput, err := Resample(input, 16_000)
	if err != nil {
		return SessionResult{}, fmt.Errorf("prepare Gemini input: %w", err)
	}
	descriptor := adapter.Descriptor()
	startedAt := time.Now().UTC()
	recorder := newWireRecorder(time.Now())
	state := &geminiRunState{}
	frames := splitPaddedPCM(providerInput.PCM16, int(adapter.config.ChunkDuration*time.Duration(providerInput.SampleRateHz)/time.Second)*2)
	if len(frames) == 0 {
		return SessionResult{}, errors.New("Gemini input contains no audio frames")
	}

	var setupDuration time.Duration
	connectionCount := 0
	for next := 0; next < len(frames); {
		start := next
		outcome, sessionErr := adapter.runSession(ctx, recorder, state, frames, start)
		if sessionErr != nil {
			return SessionResult{}, sessionErr
		}
		connectionCount++
		setupDuration += outcome.SetupDuration
		next = outcome.NextFrame
		// The upstream benchmark advances one frame if a session ended before
		// its sender could make progress, preventing an infinite reconnect loop.
		if next <= start {
			next = start + 1
		}
	}

	state.mu.Lock()
	resultChunks := append([]OutputChunk(nil), state.chunks...)
	resultTranscript := state.transcript.String()
	resultFirstAudio := state.firstAudio
	streamOrigin := state.streamOrigin
	state.mu.Unlock()
	if streamOrigin.IsZero() {
		return SessionResult{}, errors.New("Gemini stream never became ready")
	}
	var outputBytes int
	for _, chunk := range resultChunks {
		outputBytes += len(chunk.PCM16)
	}
	events := recorder.snapshot()
	return SessionResult{
		Descriptor: descriptor, StartedAt: startedAt, ConnectionSetupMS: milliseconds(setupDuration),
		ConnectionCount: connectionCount, InputDurationMS: milliseconds(providerInput.Duration()),
		ElapsedMS: milliseconds(time.Since(streamOrigin)), FirstAudioMS: resultFirstAudio,
		OutputAudioMS:    milliseconds(time.Duration(outputBytes/2) * time.Second / 24_000),
		OutputTranscript: resultTranscript, Usage: aggregateGeminiUsage(events, connectionCount), Chunks: resultChunks, Events: events,
	}, nil
}

type geminiRunState struct {
	mu           sync.Mutex
	chunks       []OutputChunk
	firstAudio   *float64
	transcript   strings.Builder
	streamOrigin time.Time
}

type geminiSessionOutcome struct {
	NextFrame     int
	SetupDuration time.Duration
}

func (adapter *GeminiAdapter) runSession(
	ctx context.Context,
	recorder *wireRecorder,
	state *geminiRunState,
	frames [][]byte,
	startFrame int,
) (geminiSessionOutcome, error) {
	sessionOrigin := time.Now()
	endpoint, err := url.Parse(adapter.config.Endpoint)
	if err != nil {
		return geminiSessionOutcome{}, fmt.Errorf("parse Gemini endpoint: %w", err)
	}
	headers := make(http.Header)
	headers.Set("x-goog-api-key", adapter.config.APIKey)
	connection, err := adapter.dial(ctx, recorder, endpoint.String(), headers)
	if err != nil {
		return geminiSessionOutcome{}, err
	}
	defer connection.CloseNow()
	connection.SetReadLimit(16 << 20)

	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()
	ready := make(chan struct{})
	done := make(chan struct{})
	receiveErr := make(chan error, 1)
	var readyOnce sync.Once
	var doneOnce sync.Once
	markDone := func() { doneOnce.Do(func() { close(done) }) }

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
			if messageType != websocket.MessageText && messageType != websocket.MessageBinary {
				continue
			}
			var envelope geminiEnvelope
			if jsonErr := json.Unmarshal(data, &envelope); jsonErr != nil {
				select {
				case receiveErr <- fmt.Errorf("decode Gemini event: %w", jsonErr):
				default:
				}
				return
			}
			if envelope.SetupComplete != nil {
				recorder.add("server", "setupComplete", "", data, nil)
				readyOnce.Do(func() { close(ready) })
				continue
			}
			if envelope.Error != nil {
				recorder.add("server", "error", "", data, nil)
				select {
				case receiveErr <- fmt.Errorf("%s (%d): %s", envelope.Error.Status, envelope.Error.Code, envelope.Error.Message):
				default:
				}
				return
			}
			if envelope.ServerContent != nil && envelope.ServerContent.ModelTurn != nil {
				for _, part := range envelope.ServerContent.ModelTurn.Parts {
					if part.InlineData == nil || part.InlineData.Data == "" {
						continue
					}
					audio, decodeErr := base64.StdEncoding.DecodeString(part.InlineData.Data)
					if decodeErr != nil {
						select {
						case receiveErr <- fmt.Errorf("decode Gemini audio: %w", decodeErr):
						default:
						}
						return
					}
					state.mu.Lock()
					arrival := time.Since(state.streamOrigin)
					state.chunks = append(state.chunks, OutputChunk{Arrival: arrival, PCM16: audio})
					if state.firstAudio == nil {
						value := milliseconds(arrival)
						state.firstAudio = &value
					}
					state.mu.Unlock()
					recorder.add("server", "serverContent.audio", "", compactPayload(map[string]any{"mime_type": part.InlineData.MIMEType}), audio)
				}
			}
			recorder.add("server", geminiEventType(envelope), "", redactGeminiAudio(data), nil)
			if envelope.ServerContent != nil && envelope.ServerContent.Interrupted {
				state.mu.Lock()
				state.chunks = append(state.chunks, OutputChunk{Arrival: time.Since(state.streamOrigin), Flush: true})
				state.mu.Unlock()
				markDone()
			}
			if envelope.ServerContent != nil && envelope.ServerContent.OutputTranscription != nil {
				state.mu.Lock()
				state.transcript.WriteString(envelope.ServerContent.OutputTranscription.Text)
				state.mu.Unlock()
			}
			if envelope.ServerContent != nil && (envelope.ServerContent.TurnComplete || envelope.ServerContent.GenerationComplete) {
				markDone()
			}
		}
	}()

	generationConfig := map[string]any{
		"responseModalities": []string{"AUDIO"},
		"thinkingConfig":     map[string]any{"thinkingLevel": adapter.config.ThinkingLevel},
	}
	setupBody := map[string]any{"model": "models/" + adapter.config.Model, "generationConfig": generationConfig}
	if adapter.config.Instructions != "" {
		setupBody["systemInstruction"] = map[string]any{"parts": []map[string]string{{"text": adapter.config.Instructions}}}
	}
	if adapter.config.Transcription {
		setupBody["inputAudioTranscription"] = map[string]any{}
		setupBody["outputAudioTranscription"] = map[string]any{}
	}
	setup := map[string]any{"setup": setupBody}
	if err := writeJSON(ctx, connection, setup); err != nil {
		return geminiSessionOutcome{}, fmt.Errorf("configure Gemini session: %w", err)
	}
	recorder.add("client", "setup", "", compactPayload(setup), nil)
	setupTimer := time.NewTimer(15 * time.Second)
	defer setupTimer.Stop()
	select {
	case <-ready:
	case err := <-receiveErr:
		return geminiSessionOutcome{}, fmt.Errorf("Gemini setup: %w", err)
	case <-setupTimer.C:
		return geminiSessionOutcome{}, errors.New("timed out waiting for Gemini setupComplete")
	case <-ctx.Done():
		return geminiSessionOutcome{}, ctx.Err()
	}
	setupDuration := time.Since(sessionOrigin)
	state.mu.Lock()
	if state.streamOrigin.IsZero() {
		state.streamOrigin = time.Now()
	}
	state.mu.Unlock()

	nextFrame := startFrame
	sendOrigin := time.Now()
	for nextFrame < len(frames) {
		select {
		case <-done:
			cancelRead()
			return geminiSessionOutcome{NextFrame: nextFrame, SetupDuration: setupDuration}, nil
		case readErr := <-receiveErr:
			return geminiSessionOutcome{}, fmt.Errorf("Gemini receive: %w", readErr)
		default:
		}
		frame := frames[nextFrame]
		message := map[string]any{"realtimeInput": map[string]any{"audio": map[string]any{
			"data": base64.StdEncoding.EncodeToString(frame), "mimeType": "audio/pcm;rate=16000",
		}}}
		if err := writeJSON(ctx, connection, message); err != nil {
			return geminiSessionOutcome{}, fmt.Errorf("send Gemini audio frame %d: %w", nextFrame, err)
		}
		recorder.add("client", "realtimeInput.audio", "", nil, frame)
		nextFrame++
		deadline := sendOrigin.Add(time.Duration(nextFrame-startFrame) * adapter.config.ChunkDuration)
		if err := sleepUntilSignal(ctx, done, receiveErr, deadline); err != nil {
			if errors.Is(err, errSessionDone) {
				cancelRead()
				return geminiSessionOutcome{NextFrame: nextFrame, SetupDuration: setupDuration}, nil
			}
			return geminiSessionOutcome{}, err
		}
	}
	end := map[string]any{"realtimeInput": map[string]any{"audioStreamEnd": true}}
	if err := writeJSON(ctx, connection, end); err != nil {
		return geminiSessionOutcome{}, fmt.Errorf("end Gemini audio stream: %w", err)
	}
	recorder.add("client", "realtimeInput.audioStreamEnd", "", compactPayload(end), nil)
	tailTimer := time.NewTimer(adapter.config.TailDuration)
	defer tailTimer.Stop()
	select {
	case <-done:
	case readErr := <-receiveErr:
		return geminiSessionOutcome{}, fmt.Errorf("Gemini receive: %w", readErr)
	case <-tailTimer.C:
	case <-ctx.Done():
		return geminiSessionOutcome{}, ctx.Err()
	}
	cancelRead()
	return geminiSessionOutcome{NextFrame: nextFrame, SetupDuration: setupDuration}, nil
}

func (adapter *GeminiAdapter) dial(ctx context.Context, recorder *wireRecorder, endpoint string, headers http.Header) (*websocket.Conn, error) {
	var lastErr error
	for attempt := 1; attempt <= adapter.config.ConnectAttempts; attempt++ {
		connection, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPHeader: headers})
		if err == nil {
			return connection, nil
		}
		lastErr = err
		status := 0
		if response != nil {
			status = response.StatusCode
			if status != http.StatusTooManyRequests && status < 500 {
				return nil, fmt.Errorf("connect Gemini Live (HTTP %d): %w", status, err)
			}
		}
		recorder.add("client", "transport.connect_retry", "", compactPayload(map[string]any{
			"attempt": attempt, "max_attempts": adapter.config.ConnectAttempts, "http_status": status,
		}), nil)
		if attempt == adapter.config.ConnectAttempts {
			break
		}
		delay := adapter.config.RetryBaseDelay * time.Duration(1<<(attempt-1))
		if sleepErr := sleepUntil(ctx, time.Now().Add(delay)); sleepErr != nil {
			return nil, sleepErr
		}
	}
	return nil, fmt.Errorf("connect Gemini Live after %d attempts: %w", adapter.config.ConnectAttempts, lastErr)
}

var errSessionDone = errors.New("Gemini session completed")

func sleepUntilSignal(ctx context.Context, done <-chan struct{}, receiveErr <-chan error, deadline time.Time) error {
	duration := time.Until(deadline)
	if duration <= 0 {
		return nil
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-done:
		return errSessionDone
	case err := <-receiveErr:
		return fmt.Errorf("Gemini receive: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func splitPaddedPCM(pcm []byte, chunkBytes int) [][]byte {
	if chunkBytes <= 0 || len(pcm) == 0 {
		return nil
	}
	frames := make([][]byte, 0, (len(pcm)+chunkBytes-1)/chunkBytes)
	for offset := 0; offset < len(pcm); offset += chunkBytes {
		end := min(offset+chunkBytes, len(pcm))
		frame := make([]byte, chunkBytes)
		copy(frame, pcm[offset:end])
		frames = append(frames, frame)
	}
	return frames
}

type geminiUsageMetadata struct {
	PromptTokenCount    int64 `json:"promptTokenCount"`
	ResponseTokenCount  int64 `json:"responseTokenCount"`
	PromptTokensDetails []struct {
		Modality   string `json:"modality"`
		TokenCount int64  `json:"tokenCount"`
	} `json:"promptTokensDetails"`
	ResponseTokensDetails []struct {
		Modality   string `json:"modality"`
		TokenCount int64  `json:"tokenCount"`
	} `json:"responseTokensDetails"`
}

// aggregateGeminiUsage keeps the last cumulative usage update in each Live
// session, then sums sessions. Setup events are unambiguous session boundaries.
func aggregateGeminiUsage(events []WireEvent, expectedSessions int) Usage {
	usage := Usage{SessionsExpected: expectedSessions}
	var sessions []json.RawMessage
	var current json.RawMessage
	flush := func() {
		if len(current) == 0 {
			return
		}
		var metadata geminiUsageMetadata
		if err := json.Unmarshal(current, &metadata); err == nil {
			usage.InputTokens += metadata.PromptTokenCount
			usage.OutputTokens += metadata.ResponseTokenCount
			for _, detail := range metadata.PromptTokensDetails {
				if strings.EqualFold(detail.Modality, "AUDIO") {
					usage.InputAudioTokens += detail.TokenCount
				}
			}
			for _, detail := range metadata.ResponseTokensDetails {
				if strings.EqualFold(detail.Modality, "AUDIO") {
					usage.OutputAudioTokens += detail.TokenCount
				}
			}
			sessions = append(sessions, append(json.RawMessage(nil), current...))
		}
		current = nil
	}
	for _, event := range events {
		if event.Direction == "client" && event.Type == "setup" {
			flush()
		}
		if len(event.Payload) == 0 {
			continue
		}
		var envelope struct {
			UsageMetadata json.RawMessage `json:"usageMetadata"`
		}
		if err := json.Unmarshal(event.Payload, &envelope); err == nil && len(envelope.UsageMetadata) > 0 {
			current = append(current[:0], envelope.UsageMetadata...)
		}
	}
	flush()
	usage.SessionsObserved = len(sessions)
	usage.Complete = expectedSessions > 0 && len(sessions) == expectedSessions
	usage.Provider = compactPayload(map[string]any{
		"aggregation": "last cumulative update per Live session",
		"complete":    usage.Complete, "sessions_expected": expectedSessions,
		"sessions_observed": len(sessions), "sessions": sessions,
	})
	return usage
}

func geminiEventType(envelope geminiEnvelope) string {
	switch {
	case envelope.ServerContent != nil:
		return "serverContent"
	case len(envelope.UsageMetadata) > 0:
		return "usageMetadata"
	case len(envelope.GoAway) > 0:
		return "goAway"
	default:
		return "serverMessage"
	}
}

func redactGeminiAudio(data []byte) json.RawMessage {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	serverContent, ok := root["serverContent"].(map[string]any)
	if !ok {
		return data
	}
	modelTurn, ok := serverContent["modelTurn"].(map[string]any)
	if !ok {
		return data
	}
	parts, ok := modelTurn["parts"].([]any)
	if !ok {
		return data
	}
	for _, rawPart := range parts {
		part, partOK := rawPart.(map[string]any)
		if partOK {
			delete(part, "inlineData")
		}
	}
	return compactPayload(root)
}
