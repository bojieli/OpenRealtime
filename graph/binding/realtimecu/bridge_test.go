package realtimecu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestClientBridgeRequiresGraphEmissionBeforeAcceptingResult(t *testing.T) {
	bridge, specs, err := newClientBridge(testTarget())
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.Update(legacy.Settings{Tools: []legacyaction.ToolSpec{specs[0]}}); err != nil {
		t.Fatal(err)
	}
	call := trajectory.ToolCall{
		CallID: "call-1", Name: specs[0].Name,
		Arguments: json.RawMessage(`{"source":"screen","x":10,"y":20}`),
	}
	type dispatchOutcome struct {
		result trajectory.ToolResult
		err    error
	}
	done := make(chan dispatchOutcome, 1)
	go func() {
		result, dispatchErr := bridge.Dispatch(context.Background(), call)
		done <- dispatchOutcome{result: result, err: dispatchErr}
	}()
	waitForRegisteredCall(t, bridge, call.CallID)
	result := trajectory.ToolResult{
		CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`),
	}
	if err := bridge.Complete(result); err == nil || !strings.Contains(err.Error(), "no emitted") {
		t.Fatalf("result before committed emission error = %v", err)
	}
	if err := bridge.AwaitEmission(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Complete(trajectory.ToolResult{
		CallID: call.CallID, Name: computeruse.Type, Output: json.RawMessage(`{"ok":true}`),
	}); err == nil || !strings.Contains(err.Error(), "want") {
		t.Fatalf("mismatched result error = %v", err)
	}
	if err := bridge.Complete(result); err != nil {
		t.Fatal(err)
	}
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.result.CallID != call.CallID || outcome.result.Name != call.Name ||
			string(outcome.result.Output) != `{"ok":true}` {
			t.Fatalf("dispatch outcome = %+v", outcome)
		}
	case <-time.After(time.Second):
		t.Fatal("client dispatch did not receive its terminal result")
	}
}

func TestClientBridgeFailsClosedOnDeclarationDriftAndPendingMutation(t *testing.T) {
	bridge, specs, err := newClientBridge(testTarget())
	if err != nil {
		t.Fatal(err)
	}
	drifted := cloneToolSpec(specs[0])
	drifted.Parameters = json.RawMessage(`{"type":"object"}`)
	if err := bridge.Update(legacy.Settings{Tools: []legacyaction.ToolSpec{drifted}}); err == nil ||
		!strings.Contains(err.Error(), "drifted") {
		t.Fatalf("schema drift error = %v", err)
	}
	unknown := cloneToolSpec(specs[0])
	unknown.Name = "shell.execute"
	if err := bridge.Update(legacy.Settings{Tools: []legacyaction.ToolSpec{unknown}}); err == nil ||
		!strings.Contains(err.Error(), "non-standard") {
		t.Fatalf("unknown tool error = %v", err)
	}
	if err := bridge.Update(legacy.Settings{Tools: []legacyaction.ToolSpec{specs[0]}}); err != nil {
		t.Fatal(err)
	}
	call := trajectory.ToolCall{
		CallID: "pending-call", Name: specs[0].Name,
		Arguments: json.RawMessage(`{"source":"screen","x":1,"y":2}`),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, dispatchErr := bridge.Dispatch(ctx, call)
		done <- dispatchErr
	}()
	waitForRegisteredCall(t, bridge, call.CallID)
	if err := bridge.Update(legacy.Settings{Tools: []legacyaction.ToolSpec{specs[1]}}); err == nil ||
		!strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending declaration mutation error = %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled dispatch did not stop")
	}
}

func waitForRegisteredCall(t *testing.T, bridge *clientBridge, callID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		bridge.mu.Lock()
		state := bridge.calls[callID]
		ready := state != nil && state.call != nil
		bridge.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("call %q was not registered", callID)
}

func testTarget() computeruse.Target {
	return computeruse.Target{Name: "test-browser", Sources: []string{SourceScreen}, Width: 1280, Height: 720}
}
