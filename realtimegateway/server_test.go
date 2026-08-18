package realtimegateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interleave"
	protocol "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/trajectory"
	"github.com/coder/websocket"
)

func TestHealthReportsConfiguredContinuationProfiles(t *testing.T) {
	t.Parallel()
	metrics := &RuntimeMetrics{}
	metrics.asrInputFrames.Add(9)
	metrics.asrProviderAdvances.Add(3)
	server, err := New(Config{
		Model: "public-model", ASRModel: "qwen-asr", ASRProviderChunk: 200 * time.Millisecond,
		PerceptionFactory: func() (v1.PerceptionProvider, error) { return &finalOnlyASR{}, nil },
		FastProvider:      &scriptedProvider{descriptor: continuation.Descriptor{Provider: "google", Model: "gemini-fast", Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal, Streaming: true, ToolAuthority: continuation.ToolAuthorityPropose}},
		SlowProvider:      &scriptedProvider{descriptor: continuation.Descriptor{Provider: "google", Model: "gemini-slow", Phase: trajectory.PhaseSlow, Effort: continuation.EffortHigh, Streaming: true, ToolAuthority: continuation.ToolAuthorityExecute, ExecutableTools: true}},
		SpeechProvider:    fakeSpeech{},
		PreparationPolicy: PreparationEndpointOnly,
		SlowContextPolicy: interleave.SlowContextContentOnly,
		RuntimeMetrics:    metrics,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/healthz", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("health status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Status string                  `json:"status"`
		Model  string                  `json:"model"`
		Fast   continuation.Descriptor `json:"fast"`
		Slow   continuation.Descriptor `json:"slow"`
		ASR    struct {
			Model              string  `json:"model"`
			ProviderChunkMS    float64 `json:"provider_chunk_ms"`
			ProviderMaxChunkMS float64 `json:"provider_max_chunk_ms"`
			Strategy           string  `json:"strategy"`
		} `json:"asr"`
		SlowContext interleave.SlowContextPolicy `json:"slow_context"`
		Preparation PreparationPolicy            `json:"preparation_policy"`
		Runtime     RuntimeMetricsSnapshot       `json:"runtime"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || body.Model != "public-model" || body.Fast.Model != "gemini-fast" ||
		body.Fast.EffectiveToolAuthority() != continuation.ToolAuthorityPropose || body.Slow.Model != "gemini-slow" ||
		body.ASR.Model != "qwen-asr" || body.ASR.ProviderChunkMS != 200 || body.SlowContext != interleave.SlowContextContentOnly ||
		body.Preparation != PreparationEndpointOnly ||
		body.ASR.ProviderMaxChunkMS != 200 || body.ASR.Strategy != "fixed" ||
		body.Runtime.ASRInputFrames != 9 || body.Runtime.ASRProviderAdvances != 3 {
		t.Fatalf("unexpected health body: %#v", body)
	}
}

func TestHealthReportsRevisionAdaptiveASRProfile(t *testing.T) {
	t.Parallel()
	server, err := New(Config{
		ASRProviderChunk: 100 * time.Millisecond, ASRProviderMaxChunk: 400 * time.Millisecond,
		PerceptionFactory: func() (v1.PerceptionProvider, error) { return &finalOnlyASR{}, nil },
		FastProvider:      &scriptedProvider{}, SlowProvider: &scriptedProvider{}, SpeechProvider: fakeSpeech{},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/healthz", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	var body struct {
		ASR struct {
			ProviderChunkMS    float64 `json:"provider_chunk_ms"`
			ProviderMaxChunkMS float64 `json:"provider_max_chunk_ms"`
			Strategy           string  `json:"strategy"`
		} `json:"asr"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.ASR.ProviderChunkMS != 100 || body.ASR.ProviderMaxChunkMS != 400 || body.ASR.Strategy != "revision-adaptive" {
		t.Fatalf("unexpected adaptive ASR health: %#v", body.ASR)
	}
}

func TestPreparationPolicyIsExplicitAndClosed(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"continuous", "ENDPOINT-ONLY"} {
		if _, err := ParsePreparationPolicy(value); err != nil {
			t.Fatalf("parse %q: %v", value, err)
		}
	}
	if _, err := ParsePreparationPolicy("smart-router"); err == nil {
		t.Fatal("undeclared preparation policy was accepted")
	}
}

func TestServerDefaultsToContinuousPreparation(t *testing.T) {
	t.Parallel()
	server, err := New(Config{
		PerceptionFactory: func() (v1.PerceptionProvider, error) { return &finalOnlyASR{}, nil },
		FastProvider:      &scriptedProvider{}, SlowProvider: &scriptedProvider{}, SpeechProvider: fakeSpeech{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.config.PreparationPolicy != PreparationContinuous {
		t.Fatalf("default preparation policy = %q", server.config.PreparationPolicy)
	}
	server.config.PreparationPolicy = PreparationEndpointOnly
	session, err := newSession(context.Background(), nil, server.config, "")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := session.newPreparation()
	if err != nil {
		t.Fatal(err)
	}
	if prepared != nil {
		t.Fatal("endpoint-only policy allocated a private continuation chain")
	}
	session.cancel(errors.New("test complete"))
}

func TestContinuousPreparationUsesPrivateProviderClass(t *testing.T) {
	t.Parallel()
	fast := &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "fake-fast", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, Streaming: true,
		},
		scripts: []providerScript{{}},
	}
	slow := &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "fake-slow", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: continuation.EffortHigh, Streaming: true,
		},
		scripts: []providerScript{{}},
	}
	server, err := New(Config{
		PerceptionFactory: func() (v1.PerceptionProvider, error) { return &finalOnlyASR{}, nil },
		FastProvider:      fast, SlowProvider: slow, SpeechProvider: fakeSpeech{},
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := newSession(context.Background(), nil, server.config, "")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := session.newPreparation()
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.Observe(context.Background(), 1, "hello"); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		runtime := server.config.RuntimeMetrics.Snapshot()
		if runtime.FastPreparation.Invocations == 1 && runtime.SlowPreparation.Invocations == 1 {
			if runtime.Fast.Invocations != 0 || runtime.Slow.Invocations != 0 {
				t.Fatalf("private preparation leaked into foreground counters: %#v", runtime)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("private preparation did not reach both stages: %#v", runtime)
		case <-time.After(time.Millisecond):
		}
	}
	closePreparedTurn(&utteranceState{preparation: prepared}, errors.New("test complete"))
	session.cancel(errors.New("test complete"))
}

type finalOnlyASR struct {
	frames       uint64
	sourceSample uint64
}

func (asr *finalOnlyASR) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "fake-asr", Version: "1", Capabilities: v1.Capabilities{
		v1.CapabilityStreamingInput: true, v1.CapabilityRevisions: true,
	}}
}

func (asr *finalOnlyASR) PushFrame(_ context.Context, frame v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	asr.frames++
	asr.sourceSample = frame.SampleOffset + uint64(len(frame.PCM16LE)/2)
	return nil, nil
}

func (asr *finalOnlyASR) Finalize(_ context.Context, sourceSample uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{
		RevisionID: 1, SourceSample: sourceSample, StableText: "check order 12345", Final: true,
	}, nil
}

type providerScript struct {
	events []continuation.Event
	usage  continuation.Usage
}

type scriptedProvider struct {
	descriptor continuation.Descriptor
	mu         sync.Mutex
	scripts    []providerScript
	requests   []continuation.Request
}

func (provider *scriptedProvider) Descriptor() continuation.Descriptor { return provider.descriptor }

func (provider *scriptedProvider) Continue(_ context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	provider.mu.Lock()
	index := len(provider.requests)
	provider.requests = append(provider.requests, request)
	script := provider.scripts[index]
	provider.mu.Unlock()
	for _, event := range script.events {
		if err := emit(event); err != nil {
			return continuation.Completion{}, err
		}
	}
	return continuation.Completion{StopReason: "stop", Usage: script.usage}, nil
}

func (provider *scriptedProvider) count() int {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return len(provider.requests)
}

type fakeSpeech struct{}

func (fakeSpeech) Descriptor() v1.Descriptor {
	return v1.Descriptor{Name: "fake-fish", Version: "1", Capabilities: v1.Capabilities{
		v1.CapabilityPCM16Output: true, v1.CapabilityStreamingOutput: true,
	}}
}

func (speech fakeSpeech) Stream(_ context.Context, plan v1.SpeechPlan, consume func(v1.SpeechChunk) error) error {
	audio := makePCM(2_400, 2_000)
	return consume(v1.SpeechChunk{
		ChunkID: plan.CandidateID + "-1", CandidateID: plan.CandidateID,
		SampleRateHz: 24_000, PCM16LE: audio, Final: true,
	})
}

func (speech fakeSpeech) Synthesize(ctx context.Context, plan v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	var result []v1.SpeechChunk
	err := speech.Stream(ctx, plan, func(chunk v1.SpeechChunk) error {
		result = append(result, chunk)
		return nil
	})
	return result, err
}

func TestStandardRealtimeGatewayRunsCanonicalToolResumption(t *testing.T) {
	fast := &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "fake-fast", Model: "fast", Phase: trajectory.PhaseFast,
			Effort: continuation.EffortMinimal, Streaming: true,
			ToolAuthority: continuation.ToolAuthorityPropose,
		},
		scripts: []providerScript{{events: []continuation.Event{
			{Kind: continuation.EventAssistantDelta, Text: "I'll check."},
			{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "proposal-1", Name: "get_order_status", Arguments: json.RawMessage(`{"order_id":"12345"}`),
			}},
		}, usage: continuation.Usage{InputTokens: 10, OutputTokens: 3, TotalTokens: 13}}},
	}
	slow := &scriptedProvider{
		descriptor: continuation.Descriptor{
			Provider: "fake-slow", Model: "slow", Phase: trajectory.PhaseSlow,
			Effort: continuation.EffortHigh, Streaming: true,
			ToolAuthority: continuation.ToolAuthorityExecute, ExecutableTools: true,
		},
		scripts: []providerScript{
			{events: []continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "call-1", Name: "get_order_status", Arguments: json.RawMessage(`{"order_id":"12345"}`),
			}}}, usage: continuation.Usage{InputTokens: 20, OutputTokens: 4, TotalTokens: 24}},
			{events: []continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Order 12345 arrives tomorrow."}}, usage: continuation.Usage{InputTokens: 30, OutputTokens: 6, TotalTokens: 36}},
		},
	}
	server, err := New(Config{
		Model: "openrealtime-test", PerceptionFactory: func() (v1.PerceptionProvider, error) { return &finalOnlyASR{}, nil },
		FastProvider: fast, SlowProvider: slow, SpeechProvider: fakeSpeech{},
		PreparationPolicy: PreparationEndpointOnly,
		ValidateWire:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	probe, err := newSession(context.Background(), nil, server.config, "gpt-realtime")
	if err != nil {
		t.Fatal(err)
	}
	encodedCreated, err := json.Marshal(probe.sessionEvent("session.created"))
	if err != nil {
		t.Fatal(err)
	}
	createdMessage, err := protocol.Decode(encodedCreated)
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.NewValidator().Validate(protocol.ProfileRealtime, protocol.DirectionServer, createdMessage); err != nil {
		t.Fatalf("session.created is not standard Realtime: %v\n%s", err, encodedCreated)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connection, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/realtime?model=gpt-realtime", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "test complete")
	if got := readEvent(t, ctx, connection); got["type"] != "session.created" {
		t.Fatalf("first event is %#v", got)
	}
	writeEvent(t, ctx, connection, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime", "instructions": "Use tools for order facts.",
			"output_modalities": []string{"audio"}, "tool_choice": "auto",
			"tools": []map[string]any{{
				"type": "function", "name": "get_order_status", "description": "Get an order status.",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"order_id": map[string]any{"type": "string"}}, "required": []string{"order_id"}},
			}},
			"audio": map[string]any{
				"input": map[string]any{
					"format":         map[string]any{"type": "audio/pcmu"},
					"transcription":  map[string]any{"model": "test"},
					"turn_detection": map[string]any{"type": "server_vad", "threshold": 0.5, "prefix_padding_ms": 0, "silence_duration_ms": 60},
				},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcmu"}, "voice": "alloy"},
			},
		},
	})
	if got := readEvent(t, ctx, connection); got["type"] != "session.updated" {
		t.Fatalf("session update response is %#v", got)
	}
	voice, err := encodeMuLaw(makePCM(160, 6_000))
	if err != nil {
		t.Fatal(err)
	}
	silence, err := encodeMuLaw(makePCM(160, 0))
	if err != nil {
		t.Fatal(err)
	}
	writeAudio(t, ctx, connection, voice)
	writeAudio(t, ctx, connection, voice)
	writeAudio(t, ctx, connection, silence)
	writeAudio(t, ctx, connection, silence)
	writeAudio(t, ctx, connection, silence)

	var callID string
	for callID == "" {
		message := readEvent(t, ctx, connection)
		if message["type"] == "error" {
			t.Fatalf("gateway error before tool call: %#v", message)
		}
		if message["type"] == "response.function_call_arguments.done" {
			callID, _ = message["call_id"].(string)
		}
	}
	if callID != "call-1" {
		t.Fatalf("authoritative call ID is %q", callID)
	}
	writeEvent(t, ctx, connection, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{"type": "function_call_output", "call_id": callID, "output": `{"status":"shipped"}`},
	})
	writeEvent(t, ctx, connection, map[string]any{"type": "response.create"})
	gotFinal := false
	for !gotFinal {
		message := readEvent(t, ctx, connection)
		if message["type"] == "error" {
			t.Fatalf("gateway error after tool result: %#v", message)
		}
		if message["type"] == "response.output_audio_transcript.done" && message["transcript"] == "Order 12345 arrives tomorrow." {
			gotFinal = true
		}
	}
	if fast.count() != 1 || slow.count() != 2 {
		t.Fatalf("tool resumption reran fast or lost slow continuation: fast=%d slow=%d", fast.count(), slow.count())
	}
	runtime := server.config.RuntimeMetrics.Snapshot()
	if runtime.Fast.Invocations != 1 || runtime.Slow.Invocations != 2 ||
		runtime.FastPreparation.Invocations != 0 || runtime.SlowPreparation.Invocations != 0 {
		t.Fatalf("endpoint-only provider provenance is incorrect: %#v", runtime)
	}
}

func writeAudio(t *testing.T, ctx context.Context, connection *websocket.Conn, audio []byte) {
	t.Helper()
	writeEvent(t, ctx, connection, map[string]any{
		"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(audio),
	})
}

func writeEvent(t *testing.T, ctx context.Context, connection *websocket.Conn, value any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.Write(ctx, websocket.MessageText, encoded); err != nil {
		t.Fatal(err)
	}
}

func readEvent(t *testing.T, ctx context.Context, connection *websocket.Conn) map[string]any {
	t.Helper()
	_, encoded, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
