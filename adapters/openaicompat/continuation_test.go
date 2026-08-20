package openaicompat

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

func TestAdapterStreamsReasoningContentAndToolCall(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing bearer authorization")
		}
		var body chatRequest
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if !body.Stream || !body.StreamOptions.IncludeUsage || body.ChatTemplateKwargs["enable_thinking"] != false {
			t.Errorf("unexpected request: %#v", body)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"checking\"},\"finish_reason\":null}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"I will check.\"},\"finish_reason\":null}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"key\\\":\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":5,\"total_tokens\":14,\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	adapter, err := New(Config{
		APIKey: "secret", Model: "qwen-test", BaseURL: server.URL + "/v1", Provider: "vllm",
		Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
		AllowTools: true, ThinkingMode: ThinkingDisabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := continuation.Request{
		InvocationID: "inv-1", Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
			ID: "user", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "look up x",
		}}},
		Invocation: continuation.Invocation{
			Instruction: "Respond immediately.", MaxOutputTokens: 32,
			Tools: []continuation.ToolDefinition{{
				Name: "lookup", Description: "Lookup a value.", Parameters: json.RawMessage(`{"type":"object"}`),
			}},
		},
	}
	var events []continuation.Event
	completion, err := adapter.Continue(context.Background(), request, func(event continuation.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Kind != continuation.EventReasoningDelta || events[0].Text != "checking" ||
		events[1].Kind != continuation.EventAssistantDelta || events[1].Text != "I will check." ||
		events[2].ToolCall == nil || string(events[2].ToolCall.Arguments) != `{"key":"x"}` {
		t.Fatalf("unexpected events: %#v", events)
	}
	if completion.StopReason != "tool_calls" || completion.Usage.ReasoningTokens != 2 || completion.Usage.TotalTokens != 14 {
		t.Fatalf("unexpected completion: %#v", completion)
	}
	if completion.ProviderStateType != ProviderStateType || !strings.Contains(string(completion.ProviderState), `"provider":"vllm"`) ||
		!strings.Contains(string(completion.ProviderState), `"reasoning_content":"checking"`) {
		t.Fatalf("provider state was not retained: %s", completion.ProviderState)
	}
}

func TestBuildRequestReusesOnlyMatchingNativeState(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		Model: "qwen-test", Provider: "vllm", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, AllowTools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(providerState{
		Provider: "vllm", Model: "qwen-test",
		Message: chatMessage{Role: "assistant", Content: "I'll check.", ReasoningContent: "Need lookup.", ToolCalls: []chatToolCall{{
			ID: "call-1", Type: "function", Function: chatFunction{Name: "lookup", Arguments: `{"key":"x"}`},
		}}},
	})
	request := continuation.Request{
		InvocationID: "inv-slow", Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Version: 4, Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "look it up"},
			{ID: "fast", Kind: trajectory.KindAssistant, InvocationID: "inv-fast", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "I'll check.", ProviderStateType: ProviderStateType, ProviderState: state},
			{ID: "call", Kind: trajectory.KindToolCall, InvocationID: "inv-fast", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, ToolCall: &trajectory.ToolCall{CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}},
			{ID: "result", Kind: trajectory.KindToolResult, InvocationID: "inv-fast", Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &trajectory.ToolResult{CallID: "call-1", Name: "lookup", Output: json.RawMessage(`{"value":7}`)}},
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
	if len(body.Messages) != 4 {
		t.Fatalf("expected system, user, retained assistant, and tool result; got %#v", body.Messages)
	}
	encoded, _ := json.Marshal(body)
	if strings.Count(string(encoded), "Need lookup.") != 1 || !strings.Contains(string(encoded), `"tool_call_id":"call-1"`) ||
		!strings.Contains(string(encoded), "capability manifest") {
		t.Fatalf("unexpected compiled request: %s", encoded)
	}
}

func TestBuildRequestDoesNotTreatAnotherModelStateAsNative(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{Model: "model-b", Provider: "vllm", Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(providerState{
		Provider: "vllm", Model: "model-a", Message: chatMessage{Role: "assistant", Content: "native-a"},
	})
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "inv-b",
		Trajectory: trajectory.Snapshot{Version: 2, Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "question"},
			{ID: "answer", Kind: trajectory.KindAssistant, InvocationID: "inv-a", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "portable-a", ProviderStateType: ProviderStateType, ProviderState: state},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "native-a") || !strings.Contains(string(encoded), "portable-a") {
		t.Fatalf("foreign model state was miscompiled: %s", encoded)
	}
}

func TestBuildRequestUsesOnlyCurrentContinuationPolicy(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		Model: "qwen-test", Provider: "vllm", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "inv-current",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "question"},
			{ID: "old-fast-policy", Kind: trajectory.KindInstruction, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "obsolete fast policy"},
			{ID: "old-slow-policy", Kind: trajectory.KindInstruction, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "obsolete slow policy"},
		}},
		Invocation: continuation.Invocation{Instruction: "current complete policy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) < 1 || body.Messages[0].Role != "system" || body.Messages[0].Content != "current complete policy" {
		t.Fatalf("unexpected system policy: %#v", body.Messages)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "obsolete") {
		t.Fatalf("historical control instruction leaked into request: %s", encoded)
	}
}

func TestBuildRequestExcludesAssistantCancelledBeforePlayback(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		Model: "qwen-test", Provider: "vllm", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(providerState{
		Provider: "vllm", Model: "qwen-test",
		Message: chatMessage{Role: "assistant", Content: "native text the user never heard", ReasoningContent: "native cancelled state"},
	})
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "slow-current",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user-1", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "first request"},
			{ID: "fast-answer", Kind: trajectory.KindAssistant, InvocationID: "fast-cancelled", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "portable text the user never heard", ProviderStateType: ProviderStateType, ProviderState: state},
			{ID: "cancel", Kind: trajectory.KindAssistantState, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, AssistantState: &trajectory.AssistantState{AssistantItemID: "fast-answer", Visibility: trajectory.VisibilityCancelled}},
			{ID: "user-2", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "interruption"},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue from what was actually heard."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	for _, excluded := range []string{"native text the user never heard", "native cancelled state", "portable text the user never heard"} {
		if strings.Contains(string(encoded), excluded) {
			t.Fatalf("cancelled assistant state leaked into request: %s", encoded)
		}
	}
	if !strings.Contains(string(encoded), "first request") || !strings.Contains(string(encoded), "interruption") {
		t.Fatalf("surrounding observations were lost: %s", encoded)
	}
}

func TestBuildRequestInjectsOnlyPendingAudibleRepairObligation(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{Model: "qwen-test", Provider: "vllm", Phase: trajectory.PhaseSlow, Effort: continuation.EffortHigh})
	if err != nil {
		t.Fatal(err)
	}
	items := []trajectory.Item{
		{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "updated request"},
		{ID: "fast", Kind: trajectory.KindAssistant, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "old audible answer"},
		{ID: "played", Kind: trajectory.KindAssistantState, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, AssistantState: &trajectory.AssistantState{AssistantItemID: "fast", Visibility: trajectory.VisibilityPlayed, PlayedAudioMS: 80}},
		{ID: "required", Kind: trajectory.KindRepair, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Repair: &trajectory.RepairState{TargetAssistantItemID: "fast", Status: trajectory.RepairRequired, PlayedAudioMS: 80}},
	}
	request := continuation.Request{Descriptor: adapter.Descriptor(), Trajectory: trajectory.Snapshot{Items: items}, Invocation: continuation.Invocation{Instruction: "Continue."}}
	body, err := adapter.buildRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Messages[0].Content, continuation.PendingRepairInstruction) {
		t.Fatalf("pending repair policy missing: %#v", body.Messages[0])
	}
	items = append(items,
		trajectory.Item{ID: "correction", Kind: trajectory.KindAssistant, Producer: trajectory.Producer{Phase: trajectory.PhaseSlow}, Content: "Correction: new answer."},
		trajectory.Item{ID: "resolved", Kind: trajectory.KindRepair, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Repair: &trajectory.RepairState{TargetAssistantItemID: "fast", Status: trajectory.RepairResolved, RepairAssistantItemID: "correction"}},
	)
	request.Trajectory.Items = items
	body, err = adapter.buildRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body.Messages[0].Content, continuation.PendingRepairInstruction) {
		t.Fatal("resolved repair remained in provider policy")
	}
}

func TestBuildRequestRendersTypedObservationSupersession(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{Model: "qwen-test", Provider: "vllm", Phase: trajectory.PhaseSlow, Effort: continuation.EffortHigh})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "first", Kind: trajectory.KindObservation, SourceRevision: 1, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "book a flight"},
			{ID: "second", Kind: trajectory.KindObservation, SourceRevision: 2, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "book a flight tomorrow", Event: &trajectory.EventMetadata{SupersedesRevision: 1}},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 || body.Messages[2].Content == "book a flight tomorrow" ||
		!strings.Contains(body.Messages[2].Content, "replace the earlier partial observation") {
		t.Fatalf("typed observation supersession was not rendered: %#v", body.Messages)
	}
}
