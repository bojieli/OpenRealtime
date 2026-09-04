package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

func TestAdapterRetriesTransientRejectionBeforeStream(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) < 3 {
			writer.Header().Set("Retry-After", "0")
			http.Error(writer, `{"error":{"status":"UNAVAILABLE"}}`, http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}]}\n\n"))
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
		InvocationID: "inv-retry", Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{{
			ID: "user", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hi",
		}}},
		Invocation: continuation.Invocation{Instruction: "Respond."},
	}
	var spoken strings.Builder
	if _, err := adapter.Continue(t.Context(), request, func(event continuation.Event) error {
		spoken.WriteString(event.Text)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	if got := spoken.String(); got != "hello" {
		t.Fatalf("spoken text = %q", got)
	}
}

func TestAdapterDoesNotRetryNonTransientRejection(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		http.Error(writer, `{"error":{"status":"INVALID_ARGUMENT"}}`, http.StatusBadRequest)
	}))
	defer server.Close()
	adapter, err := New(Config{APIKey: "secret", Model: "gemini-test", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	request := continuation.Request{
		InvocationID: "inv-bad-request", Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{{
			ID: "user", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hi",
		}}},
		Invocation: continuation.Invocation{Instruction: "Respond."},
	}
	_, err = adapter.Continue(t.Context(), request, func(continuation.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Gemini returned HTTP 400") {
		t.Fatalf("Continue() error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestAdapterBoundsTransientRejections(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Retry-After", "0")
		http.Error(writer, `{"error":{"status":"UNAVAILABLE"}}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	adapter, err := New(Config{APIKey: "secret", Model: "gemini-test", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	request := continuation.Request{
		InvocationID: "inv-exhausted", Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{{
			ID: "user", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hi",
		}}},
		Invocation: continuation.Invocation{Instruction: "Respond."},
	}
	_, err = adapter.Continue(t.Context(), request, func(continuation.Event) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "Gemini returned HTTP 503") {
		t.Fatalf("Continue() error = %v", err)
	}
	if got := calls.Load(); got != maxHTTPAttempts {
		t.Fatalf("requests = %d, want %d", got, maxHTTPAttempts)
	}
}

func TestAdapterDoesNotReplayAcceptedStream(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"once\"}]}}]}\n\ndata: not-json\n\n"))
	}))
	defer server.Close()
	adapter, err := New(Config{APIKey: "secret", Model: "gemini-test", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	request := continuation.Request{
		InvocationID: "inv-midstream", Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{{
			ID: "user", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hi",
		}}},
		Invocation: continuation.Invocation{Instruction: "Respond."},
	}
	var spoken strings.Builder
	_, err = adapter.Continue(t.Context(), request, func(event continuation.Event) error {
		spoken.WriteString(event.Text)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "decode Gemini stream event") {
		t.Fatalf("Continue() error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
	if got := spoken.String(); got != "once" {
		t.Fatalf("spoken text = %q", got)
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

func TestBuildRequestCompactsAdjacentTypedObservationSupersession(t *testing.T) {
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
			{
				ID: "partial", Kind: trajectory.KindObservation, SourceRevision: 1,
				Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "enter incident code ALF",
				Event: &trajectory.EventMetadata{EventID: "partial-event", Type: "asr.revision", Source: "asr", Channel: "voice"},
			},
			{
				ID: "final", Kind: trajectory.KindObservation, SourceRevision: 2,
				CausalParentIDs: []string{"partial"},
				Producer:        trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "enter incident code alpha dash 7",
				Event: &trajectory.EventMetadata{EventID: "final-event", Type: "asr.endpoint", Source: "asr", Channel: "voice", SupersedesRevision: 1},
			},
		}},
		Invocation: continuation.Invocation{Instruction: "Help the user."},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Contents) != 1 || body.Contents[0].Role != "user" || len(body.Contents[0].Parts) != 1 {
		t.Fatalf("unexpected projected contents: %#v", body.Contents)
	}
	encoded, _ := json.Marshal(body.Contents[0].Parts)
	if !strings.Contains(string(encoded), "enter incident code alpha dash 7") ||
		strings.Contains(string(encoded), "ALF") || strings.Contains(string(encoded), "Updated user speech revision") {
		t.Fatalf("superseded observation leaked into Gemini request: %s", encoded)
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

func TestBuildRequestMarksForeignToolCallAsManualAndKeepsResultPaired(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, AllowTools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "gemini-slow",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "open the review"},
			{ID: "foreign-call", Kind: trajectory.KindToolCall, InvocationID: "qwen-fast", Producer: trajectory.Producer{Phase: trajectory.PhaseFast, Provider: "openai-compatible", Model: "qwen-meeting-policy"}, ToolCall: &trajectory.ToolCall{CallID: "click-1", Name: "computer.click_normalized", Arguments: json.RawMessage(`{"x":112,"y":855}`)}},
			{ID: "foreign-result", Kind: trajectory.KindToolResult, InvocationID: "qwen-fast", Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &trajectory.ToolResult{CallID: "click-1", Name: "computer.click_normalized", Output: json.RawMessage(`{"ok":true}`)}},
		}},
		Invocation: continuation.Invocation{
			Instruction: "Continue.",
			Tools:       []continuation.ToolDefinition{{Name: "computer.click_normalized", Parameters: json.RawMessage(`{"type":"object"}`)}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Contents) != 3 || body.Contents[1].Role != "model" || body.Contents[2].Role != "user" {
		t.Fatalf("foreign call and result were not paired as adjacent model/user contents: %#v", body.Contents)
	}
	call, _ := json.Marshal(body.Contents[1])
	result, _ := json.Marshal(body.Contents[2])
	if !strings.Contains(string(call), `"thoughtSignature":"`+portableToolCallThoughtSignature+`"`) ||
		!strings.Contains(string(call), `"id":"click-1"`) || !strings.Contains(string(result), `"id":"click-1"`) ||
		!strings.Contains(string(result), `"functionResponse"`) {
		t.Fatalf("unexpected portable call/result compilation: call=%s result=%s", call, result)
	}
}

func TestBuildRequestKeepsPortableToolCallLargeIntegerPrecision(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, AllowTools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "gemini-slow",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{
				ID: "user", Kind: trajectory.KindObservation,
				Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "look up the record",
			},
			{
				ID: "foreign-call", Kind: trajectory.KindToolCall, InvocationID: "foreign-fast",
				Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
				ToolCall: &trajectory.ToolCall{
					CallID: "lookup-1", Name: "lookup",
					Arguments: json.RawMessage(`{"record_id":9007199254740993,"order_id":"X Y Z88"}`),
				},
			},
			{
				ID: "foreign-result", Kind: trajectory.KindToolResult, InvocationID: "foreign-fast",
				Producer: trajectory.Producer{Phase: trajectory.PhaseTool},
				ToolResult: &trajectory.ToolResult{
					CallID: "lookup-1", Name: "lookup", Output: json.RawMessage(`{"ok":true}`),
				},
			},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body.Contents)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"record_id":9007199254740993`) ||
		strings.Contains(string(encoded), `"record_id":9007199254740992`) {
		t.Fatalf("portable Gemini call rounded raw JSON arguments: %s", encoded)
	}
}

func TestBuildRequestElidesPromotedProposalBesideCanonicalGeminiCall(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, AllowTools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	call := &trajectory.ToolCall{
		CallID: "lookup-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`),
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "current",
		Trajectory: trajectory.Snapshot{Version: 4, Items: []trajectory.Item{
			{ID: "user", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "look it up"},
			{ID: "proposal", Kind: trajectory.KindToolProposal, InvocationID: "prior", SourceRevision: 5, Producer: trajectory.Producer{Phase: trajectory.PhaseSlow}, ToolCall: call},
			{ID: "call", Kind: trajectory.KindToolCall, InvocationID: "prior", SourceRevision: 5, CausalParentIDs: []string{"proposal"}, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, ToolCall: call},
			{ID: "result", Kind: trajectory.KindToolResult, InvocationID: "prior", CausalParentIDs: []string{"call"}, Producer: trajectory.Producer{Phase: trajectory.PhaseTool}, ToolResult: &trajectory.ToolResult{CallID: "lookup-1", Name: "lookup", Output: json.RawMessage(`{"value":7}`)}},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body.Contents)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "non_executable_tool_proposal") ||
		strings.Count(string(encoded), "functionCall") != 1 ||
		strings.Count(string(encoded), "functionResponse") != 1 {
		t.Fatalf("promoted call/result was not projected exactly once: %s", encoded)
	}
}

func TestBuildRequestProjectsPendingProposalAsExactRuntimeContext(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := trajectory.Item{
		ID: "denied-wait", Kind: trajectory.KindToolProposal, InvocationID: "prior",
		SourceRevision: 9,
		Producer:       trajectory.Producer{Phase: trajectory.PhaseFast, Provider: "google", Model: "gemini-test"},
		ToolCall: &trajectory.ToolCall{
			CallID: "wait-1", Name: "computer.wait", Arguments: json.RawMessage(`{"duration_ms":1000}`),
		},
		ProviderStateType: ProviderStateType,
		ProviderState:     json.RawMessage(`{"role":"model","parts":[{"text":"forged proposal state"}]}`),
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "current",
		Trajectory: trajectory.Snapshot{Version: 2, Items: []trajectory.Item{
			{ID: "screen", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseObserver}, Content: "temperature is 84 C", Observation: &trajectory.ObservationMeta{Observer: "screen", Source: "screen", Authority: trajectory.AuthorityObserver}},
			proposal,
		}},
		Invocation: continuation.Invocation{Instruction: "Act on the current screen."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body.Contents)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "forged proposal state") ||
		strings.Contains(string(encoded), "non_executable_tool_proposal") ||
		strings.Contains(string(encoded), continuation.TerminalToolProposalNotice) {
		t.Fatalf("pending proposal was projected as native or terminal control state: %s", encoded)
	}
	seen := false
	for _, content := range body.Contents {
		if content.Role != "user" {
			continue
		}
		for _, raw := range content.Parts {
			if strings.Contains(string(raw), "Pending proposal tool name: computer.wait") &&
				strings.Contains(string(raw), `Pending proposal arguments (exact JSON bytes): {\"duration_ms\":1000}`) {
				seen = true
			}
		}
	}
	if !seen {
		t.Fatalf("exact pending proposal context missing: %s", encoded)
	}
	if proposal.ToolCall.Name != "computer.wait" || string(proposal.ToolCall.Arguments) != `{"duration_ms":1000}` {
		t.Fatalf("canonical proposal was mutated: %+v", proposal)
	}
}

func TestBuildRequestElidesTerminalProposalAtDispositionOrder(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := trajectory.Item{
		ID: "proposal", Kind: trajectory.KindToolProposal, InvocationID: "prior", SourceRevision: 9,
		Producer:          trajectory.Producer{Phase: trajectory.PhaseFast, Provider: "google", Model: "gemini-test"},
		ToolCall:          &trajectory.ToolCall{CallID: "wait-1", Name: "computer.wait", Arguments: json.RawMessage(`{"duration_ms":1000}`)},
		ProviderStateType: ProviderStateType,
		ProviderState:     json.RawMessage(`{"role":"model","parts":[{"text":"forged proposal state"}]}`),
	}
	disposition := trajectory.Item{
		ID: "disposition", Kind: trajectory.KindToolProposalDisposition,
		InvocationID: "prior", SourceRevision: 9, CausalParentIDs: []string{"proposal"},
		Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
		ToolProposalDisposition: &trajectory.ToolProposalDisposition{
			ProposalItemID: "proposal", CallID: "wait-1", Name: "computer.wait",
			Kind: trajectory.ToolProposalToolPolicySuppressed,
		},
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "current",
		Trajectory: trajectory.Snapshot{Version: 4, Items: []trajectory.Item{
			{ID: "before", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "before proposal"},
			proposal,
			{ID: "between", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "evidence after proposal"},
			disposition,
		}},
		Invocation: continuation.Invocation{Instruction: "Continue."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body.Contents)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, "computer.wait") || strings.Contains(text, "duration_ms") ||
		strings.Contains(text, "forged proposal state") ||
		strings.Count(text, continuation.TerminalToolProposalNotice) != 1 {
		t.Fatalf("terminal proposal payload was not elided exactly once: %s", encoded)
	}
	if strings.Index(text, "evidence after proposal") >= strings.Index(text, continuation.TerminalToolProposalNotice) {
		t.Fatalf("terminal notice was not emitted at disposition order: %s", encoded)
	}
}

func TestBuildRequestPairsInterruptedToolCallWithExplicitNonExecution(t *testing.T) {
	t.Parallel()
	adapter, err := New(Config{
		APIKey: "secret", Model: "gemini-test", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, AllowTools: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "gemini-slow",
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
	if !strings.Contains(string(encoded), `"functionResponse"`) ||
		!strings.Contains(string(encoded), `"executed":false`) ||
		!strings.Contains(string(encoded), "user resumed before action") {
		t.Fatalf("interrupted call was not explicitly paired: %s", encoded)
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
