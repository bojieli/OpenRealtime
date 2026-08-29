package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestBuildRequestAttachesOnlyLatestMediaPerSource(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		Model: "qwen-vl-test", Provider: "vllm", Phase: trajectory.PhaseFast, Vision: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	items := []trajectory.Item{
		visionObservationItem("screen-old", "screen", "old screen narration"),
		visionObservationItem("camera-current", "camera", "current camera narration"),
		visionObservationItem("screen-current", "screen", "current screen narration"),
	}
	resolved := make(map[string]int)
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), Trajectory: trajectory.Snapshot{Items: items},
		Invocation: continuation.Invocation{Instruction: "Act on the current frame."},
		Media: func(handle string) (continuation.Media, error) {
			resolved[handle]++
			return continuation.Media{MIMEType: "image/jpeg", Bytes: []byte(handle)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved["screen-old"] != 0 || resolved["screen-current"] != 1 || resolved["camera-current"] != 1 {
		t.Fatalf("unexpected media resolutions: %#v", resolved)
	}
	encoded, _ := json.Marshal(body)
	requestJSON := string(encoded)
	for _, narration := range []string{"old screen narration", "current camera narration", "current screen narration"} {
		if !strings.Contains(requestJSON, narration) {
			t.Errorf("historical observation text %q was lost: %s", narration, requestJSON)
		}
	}
	if strings.Contains(requestJSON, "c2NyZWVuLW9sZA==") {
		t.Fatalf("stale screen bytes were attached: %s", requestJSON)
	}
	if strings.Count(requestJSON, `"type":"image_url"`) != 2 {
		t.Fatalf("expected one current screen and camera image: %s", requestJSON)
	}
}

func TestBuildRequestProjectsAdjacentUserObservationsAsOneMessage(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		Model: "qwen-test", Provider: "vllm", Phase: trajectory.PhaseFast,
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
	if len(body.Messages) != 2 || body.Messages[0].Role != "system" || body.Messages[1].Role != "user" {
		t.Fatalf("unexpected projected messages: %#v", body.Messages)
	}
	want := "I know.\nI need to exchange two items for my recent order."
	if body.Messages[1].Content != want {
		t.Fatalf("projected user content = %q, want %q", body.Messages[1].Content, want)
	}
}

func visionObservationItem(handle, source, narration string) trajectory.Item {
	return trajectory.Item{
		ID: handle, Kind: trajectory.KindObservation, Content: narration,
		Producer: trajectory.Producer{Phase: trajectory.PhaseObserver},
		Observation: &trajectory.ObservationMeta{
			Observer: "video", Source: source, Authority: trajectory.AuthorityObserver,
			Media: []trajectory.MediaRef{{
				Handle: handle, MIMEType: "image/jpeg", Source: source,
			}},
		},
	}
}

func TestAdapterStreamsReasoningContentAndToolCall(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing bearer authorization")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if string(body["stream"]) != "true" || !strings.Contains(string(body["stream_options"]), "\"include_usage\":true") {
			t.Errorf("unexpected request: %v", body)
		}
		if string(body["chat_template_kwargs"]) != `{"enable_thinking":false}` {
			t.Errorf("thinking was not disabled: %s", body["chat_template_kwargs"])
		}
		if string(body["max_tokens"]) != "32" {
			t.Errorf("unexpected output limit: %s", body["max_tokens"])
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"checking\"},\"finish_reason\":null}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"I will check.\"},\"finish_reason\":null}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"key\\\":\"}}]},\"finish_reason\":null}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":5,\"total_tokens\":14,\"prompt_tokens_details\":{\"cached_tokens\":6},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n"))
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
	if completion.StopReason != "tool_calls" || completion.Usage.CachedInputTokens != 6 || !completion.Usage.CachedInputTokensReported || completion.Usage.ReasoningTokens != 2 || completion.Usage.TotalTokens != 14 {
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
		!strings.Contains(string(encoded), "complete set of capabilities") {
		t.Fatalf("unexpected compiled request: %s", encoded)
	}
	system := body.Messages[0].Content
	if manifestAt, policyAt := strings.Index(system, "complete set of capabilities"), strings.LastIndex(system, "Continue."); manifestAt < 0 || policyAt <= manifestAt {
		t.Fatalf("current phase policy must govern the capability data:\n%s", system)
	}
}

func TestBuildRequestPairsInterruptedToolCallWithExplicitNonExecution(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		Model: "qwen-test", Provider: "vllm", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, AllowTools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "slow-new",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "analyze it"},
			{ID: "call", Kind: trajectory.KindToolCall, InvocationID: "slow-old", Producer: trajectory.Producer{Phase: trajectory.PhaseSlow}, ToolCall: &trajectory.ToolCall{CallID: "analysis-1", Name: "analyze", Arguments: json.RawMessage(`{}`)}},
			{ID: "placeholder", Kind: trajectory.KindToolPlaceholder, InvocationID: "slow-old", Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, ToolPlaceholder: &trajectory.ToolPlaceholder{CallID: "analysis-1", Name: "analyze", Reason: "user resumed before action"}},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue.", Tools: []continuation.ToolDefinition{{Name: "analyze", Parameters: json.RawMessage(`{"type":"object"}`)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	if !strings.Contains(string(encoded), `"role":"tool"`) ||
		!strings.Contains(string(encoded), `\"executed\":false`) ||
		!strings.Contains(string(encoded), "user resumed before action") {
		t.Fatalf("interrupted call was not explicitly paired: %s", encoded)
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
	if policyAt, repairAt := strings.Index(body.Messages[0].Content, "Continue."),
		strings.Index(body.Messages[0].Content, continuation.PendingRepairInstruction); policyAt < 0 || repairAt <= policyAt {
		t.Fatalf("repair obligation must follow the ordinary phase policy:\n%s", body.Messages[0].Content)
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
	if len(body.Messages) != 2 || body.Messages[1].Content == "book a flight tomorrow" ||
		!strings.Contains(body.Messages[1].Content, "book a flight\nUpdated user speech revision") ||
		!strings.Contains(body.Messages[1].Content, "replace the earlier partial observation") {
		t.Fatalf("typed observation supersession was not rendered: %#v", body.Messages)
	}
}

// TestBuildRequestMarksAnswersTheUserNeverHeard pins the distinction the whole
// fast/slow arrangement rests on. Slow writes an answer it is not permitted to
// speak and a fast continuation voices it; if both arrive as plain assistant
// turns, the voicing step has no referent for "the answer the reasoning
// continuation just produced" and echoes what was already said instead.
func TestBuildRequestMarksAnswersTheUserNeverHeard(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		Model: "qwen-test", Provider: "vllm", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "voice-current",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user-1", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "what should I do this weekend?"},
			{ID: "spoken", Kind: trajectory.KindAssistant, InvocationID: "inv-fast", Content: "SPOKEN-ANSWER", Producer: trajectory.Producer{
				Phase: trajectory.PhaseFast, SpeechAuthority: string(continuation.SpeechAuthorityVoice),
			}},
			{ID: "written", Kind: trajectory.KindAssistant, InvocationID: "inv-slow", Content: "WRITTEN-ANSWER", Producer: trajectory.Producer{
				Phase: trajectory.PhaseSlow, SpeechAuthority: string(continuation.SpeechAuthoritySilent),
			}},
		}},
		Invocation: continuation.Invocation{Instruction: "Answer the user now."},
	})
	if err != nil {
		t.Fatal(err)
	}
	var written, spoken, writtenRole, spokenRole string
	for _, message := range body.Messages {
		if strings.Contains(message.Content, "WRITTEN-ANSWER") {
			written, writtenRole = message.Content, message.Role
		}
		if strings.Contains(message.Content, "SPOKEN-ANSWER") {
			spoken, spokenRole = message.Content, message.Role
		}
	}
	if written == "" || spoken == "" {
		t.Fatalf("both answers must reach the provider, got %+v", body.Messages)
	}
	if writtenRole != "system" {
		t.Fatalf("a result the user never heard was rendered as a %q turn: %q", writtenRole, written)
	}
	if !strings.HasPrefix(written, continuation.BackgroundResultHint) {
		t.Fatalf("the background result reached the model unmarked: %q", written)
	}
	if spokenRole != "assistant" {
		t.Fatalf("what the user actually heard was not an assistant turn: %q", spokenRole)
	}
	if strings.Contains(spoken, continuation.BackgroundResultHint) {
		t.Fatalf("what the user actually heard was marked as background state: %q", spoken)
	}
}

// TestBuildRequestMarksUnspokenRetainedState covers the same rule on the path
// that bypasses the portable compile. A deployment whose fast and slow
// providers are the same local model reaches slow's answer as retained native
// state, and it is no more spoken for having been retained.
func TestBuildRequestMarksUnspokenRetainedState(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		Model: "qwen-test", Provider: "vllm", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, _ := json.Marshal(providerState{
		Provider: "vllm", Model: "qwen-test",
		Message: chatMessage{Role: "assistant", Content: "WRITTEN-ANSWER"},
	})
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "voice-current",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user-1", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "question"},
			{ID: "written", Kind: trajectory.KindAssistant, InvocationID: "inv-slow", Content: "WRITTEN-ANSWER",
				ProviderStateType: ProviderStateType, ProviderState: state, Producer: trajectory.Producer{
					Phase: trajectory.PhaseSlow, SpeechAuthority: string(continuation.SpeechAuthoritySilent),
				}},
		}},
		Invocation: continuation.Invocation{Instruction: "voice it"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var hinted bool
	for _, message := range body.Messages {
		if message.Role == "system" && strings.HasPrefix(message.Content, continuation.BackgroundResultHint) {
			hinted = true
		}
	}
	if !hinted {
		encoded, _ := json.Marshal(body)
		t.Fatalf("retained state kept a background result indistinguishable from speech: %s", encoded)
	}
}

// The projection is what puts the log's clock in front of the model. Testing
// the note in isolation proves only that it can be rendered; this proves it is
// actually carried, which is the half that silently breaks.
func TestBuildRequestCarriesElapsedTime(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{Model: "model-a", Provider: "vllm", Phase: trajectory.PhaseFast})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "inv-t",
		Trajectory: trajectory.Snapshot{Version: 2, Items: []trajectory.Item{
			{ID: "first", Kind: trajectory.KindObservation, MonotonicNS: 0,
				Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "are you there"},
			{ID: "second", Kind: trajectory.KindObservation, MonotonicNS: uint64(9 * time.Second),
				Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "still there"},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	if !strings.Contains(string(encoded), "9.0s later") {
		t.Fatalf("a nine second silence never reached the model: %s", encoded)
	}
	if strings.Contains(string(encoded), "later] are you there") {
		t.Fatalf("the first observation was given something to be later than: %s", encoded)
	}
}
