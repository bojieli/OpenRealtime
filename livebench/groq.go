package livebench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"
)

const (
	DefaultGroqSTTModel = "whisper-large-v3-turbo"
	DefaultGroqLLMModel = "openai/gpt-oss-120b"
	DefaultGroqTTSModel = "canopylabs/orpheus-v1-english"
	DefaultGroqVoice    = "autumn"
)

type GroqConfig struct {
	APIKey       string
	Endpoint     string
	STTModel     string
	LLMModel     string
	TTSModel     string
	Voice        string
	Instructions string
	TailDuration time.Duration
	HTTPClient   *http.Client
	VAD          VADConfig
}

type GroqAdapter struct {
	config GroqConfig
}

func NewGroqAdapter(config GroqConfig) (*GroqAdapter, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("GROQ_API_KEY is required")
	}
	if config.Endpoint == "" {
		config.Endpoint = "https://api.groq.com/openai/v1"
	}
	config.Endpoint = strings.TrimRight(config.Endpoint, "/")
	if config.STTModel == "" {
		config.STTModel = DefaultGroqSTTModel
	}
	if config.LLMModel == "" {
		config.LLMModel = DefaultGroqLLMModel
	}
	if config.TTSModel == "" {
		config.TTSModel = DefaultGroqTTSModel
	}
	if config.Voice == "" {
		config.Voice = DefaultGroqVoice
	}
	if config.Instructions == "" {
		config.Instructions = "You are a helpful spoken-dialogue assistant. Respond naturally and concisely, in no more than two sentences."
	}
	if config.TailDuration == 0 {
		config.TailDuration = 5 * time.Second
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 45 * time.Second}
	}
	if config.VAD.FrameDuration == 0 {
		config.VAD = DefaultVADConfig()
	}
	return &GroqAdapter{config: config}, nil
}

func (adapter *GroqAdapter) Descriptor() Descriptor {
	return Descriptor{
		Provider: "groq", Model: strings.Join([]string{adapter.config.STTModel, adapter.config.LLMModel, adapter.config.TTSModel}, "+"),
		Transport: "https", Architecture: "cascaded-stt-llm-tts", Profile: "fdb-v1.5-energy-vad-cascade-v2",
		InputSampleRate: 16_000, OutputSampleRate: 24_000,
	}
}

type groqMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func (adapter *GroqAdapter) Run(ctx context.Context, input Audio) (SessionResult, error) {
	providerInput, err := Resample(input, 16_000)
	if err != nil {
		return SessionResult{}, fmt.Errorf("prepare Groq input: %w", err)
	}
	descriptor := adapter.Descriptor()
	startedAt := time.Now().UTC()
	origin := time.Now()
	recorder := newWireRecorder(origin)
	segments := DetectSpeech(providerInput, adapter.config.VAD)
	streamOrigin := time.Now()
	conversation := []groqMessage{{Role: "system", Content: adapter.config.Instructions}}
	var chunks []OutputChunk
	var firstAudio *float64
	var transcripts []string
	var usage Usage

	for index, segment := range segments {
		segmentEnd := streamOrigin.Add(time.Duration(segment.EndMS * float64(time.Millisecond)))
		if err := sleepUntil(ctx, segmentEnd); err != nil {
			return SessionResult{}, err
		}
		deadline := streamOrigin.Add(providerInput.Duration() + adapter.config.TailDuration)
		var nextStart *time.Duration
		if index+1 < len(segments) {
			value := time.Duration(segments[index+1].StartMS * float64(time.Millisecond))
			nextStart = &value
			deadline = streamOrigin.Add(value)
		}
		turnCtx, cancel := context.WithDeadline(ctx, deadline)
		turnAudio := sliceAudio(providerInput, max(0.0, segment.StartMS-300), segment.EndMS+100)
		text, responseText, responseAudio, turnUsage, turnErr := adapter.processTurn(turnCtx, recorder, turnAudio, conversation)
		cancel()
		if turnErr != nil {
			if errors.Is(turnErr, context.DeadlineExceeded) || errors.Is(turnErr, context.Canceled) {
				recorder.add("internal", "turn.cancelled_by_user_activity", "", compactPayload(map[string]any{"segment": index}), nil)
				continue
			}
			return SessionResult{}, turnErr
		}
		if strings.TrimSpace(text) == "" || len(responseAudio.PCM16) == 0 {
			continue
		}
		conversation = append(conversation, groqMessage{Role: "user", Content: text}, groqMessage{Role: "assistant", Content: responseText})
		usage.InputTokens += turnUsage.InputTokens
		usage.OutputTokens += turnUsage.OutputTokens
		usage.Complete = true
		arrival := time.Since(streamOrigin)
		pcm := responseAudio.PCM16
		if nextStart != nil {
			playable := *nextStart - arrival
			if playable <= 0 {
				recorder.add("internal", "output.cancelled_before_playback", "", compactPayload(map[string]any{"segment": index}), nil)
				continue
			}
			maxBytes := int(playable*time.Duration(responseAudio.SampleRateHz)/time.Second) * 2
			if maxBytes < len(pcm) {
				pcm = pcm[:max(0, maxBytes-maxBytes%2)]
				recorder.add("internal", "output.truncated_by_user_activity", "", compactPayload(map[string]any{"segment": index, "audio_bytes": len(pcm)}), nil)
			}
		}
		if len(pcm) > 0 {
			chunks = append(chunks, OutputChunk{Arrival: arrival, PCM16: append([]byte(nil), pcm...)})
			if firstAudio == nil {
				value := milliseconds(arrival)
				firstAudio = &value
			}
			transcripts = append(transcripts, responseText)
		}
	}
	finish := streamOrigin.Add(providerInput.Duration() + adapter.config.TailDuration)
	if err := sleepUntil(ctx, finish); err != nil {
		return SessionResult{}, err
	}
	var outputBytes int
	for _, chunk := range chunks {
		outputBytes += len(chunk.PCM16)
	}
	return SessionResult{
		Descriptor: descriptor, StartedAt: startedAt, InputDurationMS: milliseconds(providerInput.Duration()),
		ConnectionCount: 0,
		ElapsedMS:       milliseconds(time.Since(streamOrigin)), FirstAudioMS: firstAudio,
		OutputAudioMS:    milliseconds(time.Duration(outputBytes/2) * time.Second / 24_000),
		OutputTranscript: strings.Join(transcripts, " "), Usage: usage, Chunks: chunks, Events: recorder.snapshot(),
	}, nil
}

func (adapter *GroqAdapter) processTurn(ctx context.Context, recorder *wireRecorder, audio Audio, conversation []groqMessage) (string, string, Audio, Usage, error) {
	transcript, err := adapter.transcribe(ctx, recorder, audio)
	if err != nil {
		return "", "", Audio{}, Usage{}, err
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return "", "", Audio{}, Usage{}, nil
	}
	messages := append([]groqMessage(nil), conversation...)
	messages = append(messages, groqMessage{Role: "user", Content: transcript})
	response, usage, err := adapter.complete(ctx, recorder, messages)
	if err != nil {
		return "", "", Audio{}, Usage{}, err
	}
	response = strings.TrimSpace(response)
	if response == "" {
		return transcript, "", Audio{}, usage, nil
	}
	audioResponse, err := adapter.synthesize(ctx, recorder, response)
	if err != nil {
		return "", "", Audio{}, Usage{}, err
	}
	return transcript, response, audioResponse, usage, nil
}

func (adapter *GroqAdapter) transcribe(ctx context.Context, recorder *wireRecorder, audio Audio) (string, error) {
	wav, err := EncodeWAV(audio)
	if err != nil {
		return "", err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", adapter.config.STTModel); err != nil {
		return "", err
	}
	if err := writer.WriteField("response_format", "json"); err != nil {
		return "", err
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="speech.wav"`)
	header.Set("Content-Type", "audio/wav")
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(wav); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	recorder.add("client", "groq.audio.transcriptions.request", "", compactPayload(map[string]any{"model": adapter.config.STTModel}), wav)
	started := time.Now()
	responseBody, requestID, err := adapter.request(ctx, http.MethodPost, "/audio/transcriptions", writer.FormDataContentType(), &body)
	if err != nil {
		return "", fmt.Errorf("Groq transcription: %w", err)
	}
	var response struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", fmt.Errorf("decode Groq transcription: %w", err)
	}
	recorder.add("server", "groq.audio.transcriptions.response", requestID, compactPayload(map[string]any{"text": response.Text, "latency_ms": milliseconds(time.Since(started))}), nil)
	return response.Text, nil
}

func (adapter *GroqAdapter) complete(ctx context.Context, recorder *wireRecorder, messages []groqMessage) (string, Usage, error) {
	requestBody := map[string]any{
		"model": adapter.config.LLMModel, "messages": messages,
		"temperature": 0.2, "max_completion_tokens": 120,
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return "", Usage{}, err
	}
	recorder.add("client", "groq.chat.completions.request", "", encoded, nil)
	started := time.Now()
	responseBody, requestID, err := adapter.request(ctx, http.MethodPost, "/chat/completions", "application/json", bytes.NewReader(encoded))
	if err != nil {
		return "", Usage{}, fmt.Errorf("Groq completion: %w", err)
	}
	var response struct {
		Choices []struct {
			Message groqMessage `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return "", Usage{}, fmt.Errorf("decode Groq completion: %w", err)
	}
	if len(response.Choices) == 0 {
		return "", Usage{}, errors.New("Groq completion returned no choices")
	}
	text := response.Choices[0].Message.Content
	recorder.add("server", "groq.chat.completions.response", requestID, compactPayload(map[string]any{
		"text": text, "latency_ms": milliseconds(time.Since(started)), "usage": response.Usage,
	}), nil)
	return text, Usage{InputTokens: response.Usage.PromptTokens, OutputTokens: response.Usage.CompletionTokens}, nil
}

func (adapter *GroqAdapter) synthesize(ctx context.Context, recorder *wireRecorder, text string) (Audio, error) {
	text = truncateRunes(text, 200)
	requestBody := map[string]any{
		"model": adapter.config.TTSModel, "voice": adapter.config.Voice, "input": text,
		"response_format": "wav", "sample_rate": 24_000,
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil {
		return Audio{}, err
	}
	recorder.add("client", "groq.audio.speech.request", "", encoded, nil)
	started := time.Now()
	responseBody, requestID, err := adapter.request(ctx, http.MethodPost, "/audio/speech", "application/json", bytes.NewReader(encoded))
	if err != nil {
		return Audio{}, fmt.Errorf("Groq speech: %w", err)
	}
	audio, err := ReadWAVBytes(responseBody)
	if err != nil {
		return Audio{}, fmt.Errorf("decode Groq speech WAV: %w", err)
	}
	if audio.SampleRateHz != 24_000 {
		audio, err = Resample(audio, 24_000)
		if err != nil {
			return Audio{}, err
		}
	}
	recorder.add("server", "groq.audio.speech.response", requestID, compactPayload(map[string]any{"latency_ms": milliseconds(time.Since(started))}), responseBody)
	return audio, nil
}

func (adapter *GroqAdapter) request(ctx context.Context, method, requestPath, contentType string, body io.Reader) ([]byte, string, error) {
	request, err := http.NewRequestWithContext(ctx, method, adapter.config.Endpoint+requestPath, body)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Authorization", "Bearer "+adapter.config.APIKey)
	request.Header.Set("Content-Type", contentType)
	response, err := adapter.config.HTTPClient.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, response.Header.Get("x-request-id"), err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.Header.Get("x-request-id"), fmt.Errorf("HTTP %d: %s", response.StatusCode, truncateRunes(string(data), 1_000))
	}
	return data, response.Header.Get("x-request-id"), nil
}

func sliceAudio(audio Audio, startMS, endMS float64) Audio {
	start := max(0, int(startMS*float64(audio.SampleRateHz)/1000))
	end := min(len(audio.PCM16)/2, int(endMS*float64(audio.SampleRateHz)/1000))
	if end <= start {
		return Audio{SampleRateHz: audio.SampleRateHz}
	}
	return Audio{SampleRateHz: audio.SampleRateHz, PCM16: append([]byte(nil), audio.PCM16[start*2:end*2]...)}
}

func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}
