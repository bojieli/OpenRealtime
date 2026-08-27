package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestAdapterStreamsTextAndPreservesSignature(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("x-goog-api-key") != "secret" {
			t.Error("missing API key header")
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\",\"thoughtSignature\":\"opaque\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"cachedContentTokenCount\":2,\"candidatesTokenCount\":1,\"totalTokenCount\":4}}\n\n"))
	}))
	defer server.Close()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Endpoint: server.URL,
		Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := continuation.Request{
		InvocationID: "inv-1", Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
			ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hi",
		}}},
		Invocation: continuation.Invocation{Instruction: "Respond.", MaxOutputTokens: 32},
	}
	var text strings.Builder
	completion, err := adapter.Continue(context.Background(), request, func(event continuation.Event) error {
		text.WriteString(event.Text)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "hello" || completion.StopReason != "STOP" || completion.Usage.CachedInputTokens != 2 || !completion.Usage.CachedInputTokensReported || completion.Usage.TotalTokens != 4 {
		t.Fatalf("unexpected response: text=%q completion=%#v", text.String(), completion)
	}
	if completion.ProviderStateType != ProviderStateType || !strings.Contains(string(completion.ProviderState), "thoughtSignature") {
		t.Fatalf("thought signature not preserved: %s", completion.ProviderState)
	}
}

func TestBuildRequestProjectsAdjacentUserObservationsAsOneContent(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Version: 2, Items: []trajectory.Item{
			{ID: "fragment-1", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "I know."},
			{ID: "fragment-2", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "I need to exchange two items for my recent order."},
		}},
		Invocation: continuation.Invocation{Instruction: "Help the user."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Contents) != 1 || body.Contents[0].Role != "user" || len(body.Contents[0].Parts) != 2 {
		t.Fatalf("unexpected projected contents: %#v", body.Contents)
	}
	encoded, _ := json.Marshal(body.Contents[0])
	if strings.Index(string(encoded), "I know.") >= strings.Index(string(encoded), "I need to exchange") {
		t.Fatalf("user fragments were reordered: %s", encoded)
	}
}

func TestExplicitZeroTemperatureReachesTheWireAndDescriptor(t *testing.T) {
	t.Parallel()
	zero := 0.0
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Temperature: &zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{{
			ID: "user", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello",
		}}},
		Invocation: continuation.Invocation{Instruction: "Answer."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body.GenerationConfig.Temperature == nil || *body.GenerationConfig.Temperature != 0 {
		t.Fatalf("temperature = %v", body.GenerationConfig.Temperature)
	}
	if adapter.Descriptor().SamplingTemperature != "0" {
		t.Fatalf("descriptor temperature = %q", adapter.Descriptor().SamplingTemperature)
	}
}

func TestBuildRequestReusesNativeStateAndCompilesToolResult(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, AllowTools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	native := json.RawMessage(`{"role":"model","parts":[{"text":"I'll check.","thoughtSignature":"signed"},{"functionCall":{"id":"call-1","name":"lookup","args":{"key":"x"}}}]}`)
	request := continuation.Request{
		InvocationID: "inv-slow", Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Version: 4, Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "look it up"},
			{ID: "fast", Kind: trajectory.KindAssistant, InvocationID: "inv-fast", Producer: trajectory.Producer{Phase: trajectory.PhaseFast, Provider: "google", Model: "gemini-test"}, Content: "I'll check.", ProviderStateType: ProviderStateType, ProviderState: native},
			{ID: "call", Kind: trajectory.KindToolCall, InvocationID: "inv-fast", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, ToolCall: &trajectory.ToolCall{CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}},
			{ID: "result", Kind: trajectory.KindToolResult, InvocationID: "inv-fast", Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &trajectory.ToolResult{CallID: "call-1", Name: "lookup", Output: json.RawMessage(`{"value":7}`)}},
			{ID: "resume", Kind: trajectory.KindInstruction, InvocationID: "inv-slow", Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "Continue."},
		}},
		Invocation: continuation.Invocation{
			Instruction: "Continue.", Capabilities: []continuation.Capability{{Name: "lookup", Description: "Lookup values.", Available: true}},
			Tools: []continuation.ToolDefinition{{Name: "lookup", Description: "Lookup values.", Parameters: json.RawMessage(`{"type":"object"}`)}},
		},
	}
	body, err := adapter.buildRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Contents) != 3 {
		t.Fatalf("expected user, retained model, and result contents; got %#v", body.Contents)
	}
	if body.Contents[2].Role != "user" {
		t.Fatalf("tool result role is %q, want user", body.Contents[2].Role)
	}
	if len(body.Contents[2].Parts) != 1 {
		t.Fatalf("runtime instruction was mixed into function response: %#v", body.Contents[2].Parts)
	}
	encoded, _ := json.Marshal(body)
	if strings.Count(string(encoded), "thoughtSignature") != 1 || !strings.Contains(string(encoded), "functionResponse") ||
		!strings.Contains(string(encoded), "complete set of capabilities") || !strings.Contains(string(encoded), "parametersJsonSchema") {
		t.Fatalf("unexpected compiled request: %s", encoded)
	}
	system, _ := json.Marshal(body.SystemInstruction)
	if manifestAt, policyAt := strings.Index(string(system), "complete set of capabilities"),
		strings.LastIndex(string(system), "Continue."); manifestAt < 0 || policyAt <= manifestAt {
		t.Fatalf("current phase policy must govern the capability data: %s", system)
	}
}

func TestBuildRequestDoesNotTreatAnotherGeminiModelStateAsNative(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "model-b", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	native := json.RawMessage(`{"role":"model","parts":[{"text":"native-a","thoughtSignature":"model-a-signature"}]}`)
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "inv-b",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "question"},
			{ID: "answer", Kind: trajectory.KindAssistant, InvocationID: "inv-a", Producer: trajectory.Producer{Phase: trajectory.PhaseFast, Provider: "google", Model: "model-a"}, Content: "portable-a", ProviderStateType: ProviderStateType, ProviderState: native},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "native-a") || strings.Contains(string(encoded), "model-a-signature") || !strings.Contains(string(encoded), "portable-a") {
		t.Fatalf("foreign Gemini model state was miscompiled: %s", encoded)
	}
}

func TestAppendGeminiContentCoalescesContinuationSegments(t *testing.T) {
	t.Parallel()
	first := json.RawMessage(`{"text":"fast"}`)
	second := json.RawMessage(`{"text":"slow"}`)
	contents := appendGeminiContent(nil, geminiContent{Role: "model", Parts: []json.RawMessage{first}})
	contents = appendGeminiContent(contents, geminiContent{Role: "model", Parts: []json.RawMessage{second}})
	contents = appendGeminiContent(contents, geminiContent{Role: "user", Parts: []json.RawMessage{json.RawMessage(`{"functionResponse":{}}`)}})
	if len(contents) != 2 || len(contents[0].Parts) != 2 || contents[1].Role != "user" {
		t.Fatalf("continuation segments were not coalesced: %#v", contents)
	}
}

func TestBuildRequestKeepsCurrentPolicyOutOfModelContents(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "slow-invocation",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "question"},
			{ID: "fast-control", Kind: trajectory.KindInstruction, InvocationID: "fast-invocation", Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "respond fast"},
			{ID: "fast-answer", Kind: trajectory.KindAssistant, InvocationID: "fast-invocation", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "working"},
			{ID: "slow-control", Kind: trajectory.KindInstruction, InvocationID: "slow-invocation", Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "continue slowly"},
		}},
		Invocation: continuation.Invocation{Instruction: "continue slowly"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Contents) != 3 || body.Contents[0].Role != "user" || body.Contents[1].Role != "model" || body.Contents[2].Role != "user" {
		t.Fatalf("unexpected continuation prefix: %#v", body.Contents)
	}
	encoded, _ := json.Marshal(body.Contents)
	if strings.Contains(string(encoded), "continue slowly") || strings.Contains(string(encoded), "respond fast") {
		t.Fatalf("internal control text leaked into model contents: %s", encoded)
	}
	system, _ := json.Marshal(body.SystemInstruction)
	if !strings.Contains(string(system), "continue slowly") {
		t.Fatalf("current phase policy missing from system instruction: %s", system)
	}
}

func TestBuildRequestExcludesAssistantCancelledBeforePlayback(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	native := json.RawMessage(`{"role":"model","parts":[{"text":"native text the user never heard","thoughtSignature":"cancelled-signature"}]}`)
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "slow-current",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user-1", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "first request"},
			{ID: "fast-answer", Kind: trajectory.KindAssistant, InvocationID: "fast-cancelled", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "portable text the user never heard", ProviderStateType: ProviderStateType, ProviderState: native},
			{ID: "cancel", Kind: trajectory.KindAssistantState, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, AssistantState: &trajectory.AssistantState{AssistantItemID: "fast-answer", Visibility: trajectory.VisibilityCancelled}},
			{ID: "user-2", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "interruption"},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue from what was actually heard."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	for _, excluded := range []string{"native text the user never heard", "cancelled-signature", "portable text the user never heard"} {
		if strings.Contains(string(encoded), excluded) {
			t.Fatalf("cancelled assistant state leaked into request: %s", encoded)
		}
	}
	if !strings.Contains(string(encoded), "first request") || !strings.Contains(string(encoded), "interruption") {
		t.Fatalf("surrounding observations were lost: %s", encoded)
	}
}

func TestBuildRequestInjectsPendingAudibleRepairObligation(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseSlow, Effort: continuation.EffortHigh})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "updated request"},
			{ID: "fast", Kind: trajectory.KindAssistant, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "old audible answer"},
			{ID: "played", Kind: trajectory.KindAssistantState, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, AssistantState: &trajectory.AssistantState{AssistantItemID: "fast", Visibility: trajectory.VisibilityPlayed, PlayedAudioMS: 80}},
			{ID: "required", Kind: trajectory.KindRepair, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Repair: &trajectory.RepairState{TargetAssistantItemID: "fast", Status: trajectory.RepairRequired, PlayedAudioMS: 80}},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body.SystemInstruction)
	if !strings.Contains(string(encoded), continuation.PendingRepairInstruction) {
		t.Fatalf("pending repair policy missing: %s", encoded)
	}
	if policyAt, repairAt := strings.Index(string(encoded), "Continue."),
		strings.Index(string(encoded), continuation.PendingRepairInstruction); policyAt < 0 || repairAt <= policyAt {
		t.Fatalf("repair obligation must follow the ordinary phase policy: %s", encoded)
	}
}
