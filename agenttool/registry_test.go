package agenttool

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func testDefinition(readOnly, confirmation bool) Definition {
	return Definition{
		Tool:       continuation.ToolDefinition{Name: "lookup", Description: "Look up a value.", Parameters: json.RawMessage(`{"type":"object","properties":{"key":{"type":"string"}},"additionalProperties":false}`)},
		Capability: continuation.Capability{Name: "lookup", Description: "Look up a value.", Available: true, ExecutionPhase: "slow", ConfirmationRequired: confirmation},
		ReadOnly:   readOnly, ConfirmationRequired: confirmation,
	}
}

func TestRegistryExecutesReadOnlyCallIdempotently(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(nil)
	var calls atomic.Int64
	if err := registry.Register(testDefinition(true, false), func(_ context.Context, call trajectory.ToolCall) (any, error) {
		calls.Add(1)
		return map[string]any{"arguments": json.RawMessage(call.Arguments), "call_id": call.CallID}, nil
	}); err != nil {
		t.Fatal(err)
	}
	call := trajectory.ToolCall{CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}
	first := registry.Execute(context.Background(), call)
	second := registry.Execute(context.Background(), call)
	if first.Error != "" || second.Error != "" || string(first.Output) != string(second.Output) || calls.Load() != 1 {
		t.Fatalf("unexpected idempotency: first=%#v second=%#v calls=%d", first, second, calls.Load())
	}
	if len(registry.Tools()) != 1 || len(registry.Capabilities()) != 1 {
		t.Fatal("registry did not expose stable definitions")
	}
}

func TestRegistryRequiresAuthorityForConsequentialCall(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(nil)
	if err := registry.Register(testDefinition(false, true), func(context.Context, trajectory.ToolCall) (any, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), trajectory.ToolCall{CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{}`)})
	if result.Error == "" {
		t.Fatal("expected authority failure")
	}
}

func TestRegistryRejectsCallIDReuse(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(nil)
	if err := registry.Register(testDefinition(true, false), func(context.Context, trajectory.ToolCall) (any, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	_ = registry.Execute(context.Background(), trajectory.ToolCall{CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"x":1}`)})
	result := registry.Execute(context.Background(), trajectory.ToolCall{CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"x":2}`)})
	if result.Error == "" {
		t.Fatal("expected conflicting call ID failure")
	}
}

func TestRegistryCoalescesConcurrentDuplicateCalls(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(nil)
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	if err := registry.Register(testDefinition(true, false), func(_ context.Context, call trajectory.ToolCall) (any, error) {
		if call.CallID != "call-concurrent" {
			t.Errorf("handler received call ID %q", call.CallID)
		}
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return map[string]any{"ok": true}, nil
	}); err != nil {
		t.Fatal(err)
	}

	call := trajectory.ToolCall{CallID: "call-concurrent", Name: "lookup", Arguments: json.RawMessage(`{"key":"x"}`)}
	const concurrency = 16
	results := make(chan trajectory.ToolResult, concurrency)
	var group sync.WaitGroup
	group.Add(concurrency)
	for range concurrency {
		go func() {
			defer group.Done()
			results <- registry.Execute(context.Background(), call)
		}()
	}
	<-started
	close(release)
	group.Wait()
	close(results)
	for result := range results {
		if result.Error != "" || string(result.Output) != `{"ok":true}` {
			t.Fatalf("unexpected concurrent result: %#v", result)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler executed %d times, want 1", got)
	}
}

func TestRegistryValidatesArgumentsBeforeExecution(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(nil)
	var calls atomic.Int64
	if err := registry.Register(testDefinition(true, false), func(context.Context, trajectory.ToolCall) (any, error) {
		calls.Add(1)
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	result := registry.Execute(context.Background(), trajectory.ToolCall{
		CallID: "call-invalid", Name: "lookup", Arguments: json.RawMessage(`{"key":42}`),
	})
	if !strings.Contains(result.Error, "invalid tool arguments") {
		t.Fatalf("unexpected schema validation result: %#v", result)
	}
	if calls.Load() != 0 {
		t.Fatal("handler ran for invalid arguments")
	}
}

func TestRegistryRecoversHandlerPanic(t *testing.T) {
	t.Parallel()
	registry := NewRegistry(nil)
	if err := registry.Register(testDefinition(true, false), func(context.Context, trajectory.ToolCall) (any, error) {
		panic("boom")
	}); err != nil {
		t.Fatal(err)
	}
	call := trajectory.ToolCall{CallID: "call-panic", Name: "lookup", Arguments: json.RawMessage(`{}`)}
	first := registry.Execute(context.Background(), call)
	second := registry.Execute(context.Background(), call)
	if !strings.Contains(first.Error, "panicked") || first.Error != second.Error {
		t.Fatalf("panic was not retained safely: first=%#v second=%#v", first, second)
	}
}
