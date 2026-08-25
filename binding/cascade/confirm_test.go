package cascade_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type countingDispatcher struct{ calls chan trajectory.ToolCall }

func (dispatcher countingDispatcher) Name() string { return "counting" }

func (dispatcher countingDispatcher) Dispatch(
	_ context.Context, call trajectory.ToolCall,
) (trajectory.ToolResult, error) {
	dispatcher.calls <- call
	return trajectory.ToolResult{
		CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`),
	}, nil
}

func pressTool(dispatcher action.Dispatcher, confirm action.Confirm) action.ToolSpec {
	return action.ToolSpec{
		Name: "press", Description: "press a control",
		Parameters: json.RawMessage(`{"type":"object"}`),
		Confirm:    confirm, Target: "browser", Dispatcher: dispatcher,
	}
}

func callPress(t *testing.T, config cascade.Config) (chan trajectory.ToolCall, binding.Runtime, *recordingSink) {
	t.Helper()
	calls := make(chan trajectory.ToolCall, 4)
	dispatcher := countingDispatcher{calls: calls}
	config.Fast = newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Pressing."}})
	config.Slow = newSlow([]continuation.Event{{
		Kind: continuation.EventToolCall,
		ToolCall: &trajectory.ToolCall{
			CallID: "call_1", Name: "press", Arguments: json.RawMessage(`{"source":"screen"}`),
		},
	}})
	config.Tools = []action.ToolSpec{pressTool(dispatcher, action.ConfirmPolicy)}
	runtime, sink := startSession(t, config, binding.Settings{})
	speak(t, runtime, 3)
	return calls, runtime, sink
}

// A server-side action declaring "policy" with no policy supplied reads as
// "always", and "always" with no confirmer denies. That is fail-closed and it
// is correct - but it is also the state a deployment lands in by default, so
// the test says out loud that the action does not happen.
func TestAPolicyRequirementWithNoPolicyDeniesTheAction(t *testing.T) {
	calls, runtime, _ := callPress(t, cascade.Config{})
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolResult && item.ToolResult != nil && item.ToolResult.Error != "" {
				return true
			}
		}
		return false
	}, "an unpoliced action neither ran nor recorded a refusal")
	select {
	case call := <-calls:
		t.Fatalf("an unconfirmed action reached the world: %+v", call)
	default:
	}
}

// And with the policy supplied, the same action executes. Before the policy
// was wired there was no way to reach this state at all: every clicking and
// typing action in the shipped computer-use namespace declares "policy".
func TestAPolicyRequirementWithAPolicyExecutesTheAction(t *testing.T) {
	calls, _, _ := callPress(t, cascade.Config{
		ConfirmPolicy: func(call trajectory.ToolCall) bool { return call.Name == "press" },
	})
	waitFor(t, func() bool { return len(calls) > 0 }, "a policed action never executed")
	call := <-calls
	if call.Name != "press" {
		t.Fatalf("unexpected dispatch %+v", call)
	}
}

// An "always" requirement is a different question, and a policy must not
// answer it. Only a confirmer can.
func TestAnAlwaysRequirementIsNotAnsweredByThePolicy(t *testing.T) {
	calls := make(chan trajectory.ToolCall, 4)
	dispatcher := countingDispatcher{calls: calls}
	runtime, _ := startSession(t, cascade.Config{
		Fast: newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Pressing."}}),
		Slow: newSlow([]continuation.Event{{
			Kind: continuation.EventToolCall,
			ToolCall: &trajectory.ToolCall{
				CallID: "call_1", Name: "press", Arguments: json.RawMessage(`{"source":"screen"}`),
			},
		}}),
		Tools:         []action.ToolSpec{pressTool(dispatcher, action.ConfirmAlways)},
		ConfirmPolicy: func(trajectory.ToolCall) bool { return true },
	}, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolResult && item.ToolResult != nil && item.ToolResult.Error != "" {
				return true
			}
		}
		return false
	}, "an always-confirm action neither ran nor recorded a refusal")
	select {
	case call := <-calls:
		t.Fatalf("an action requiring explicit authorization ran without it: %+v", call)
	default:
	}
}

// And a confirmer answers it, which is the path a deployment with a human in
// the loop takes.
func TestAConfirmerAnswersAnAlwaysRequirement(t *testing.T) {
	calls := make(chan trajectory.ToolCall, 4)
	dispatcher := countingDispatcher{calls: calls}
	runtime, _ := startSession(t, cascade.Config{
		Fast: newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Pressing."}}),
		Slow: newSlow([]continuation.Event{{
			Kind: continuation.EventToolCall,
			ToolCall: &trajectory.ToolCall{
				CallID: "call_1", Name: "press", Arguments: json.RawMessage(`{"source":"screen"}`),
			},
		}}),
		Tools: []action.ToolSpec{pressTool(dispatcher, action.ConfirmAlways)},
		Confirmer: action.ConfirmerFunc(func(
			_ context.Context, request action.ConfirmationRequest,
		) (bool, error) {
			return request.Call.Name == "press", nil
		}),
	}, binding.Settings{})
	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(calls) > 0 }, "a confirmed action never executed")
	_ = <-calls
}

// A client-owned implementation is still an action, not a route around the
// server's confirmation boundary. This was especially important once bounded
// client computer tools could enter the opt-in fast lane, but it applies to
// slow calls too: authority is about the call, not where its code runs.
func TestAClientExecutedToolCannotBypassConfirmation(t *testing.T) {
	audited := make(chan action.Record, 1)
	runtime, sink := startSession(t, cascade.Config{
		Fast: newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Checking."}}),
		Slow: newSlow([]continuation.Event{{
			Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
				CallID: "remote_1", Name: "remote_press", Arguments: json.RawMessage(`{}`),
			},
		}}),
		ActionAudit: func(record action.Record) { audited <- record },
	}, binding.Settings{Tools: []action.ToolSpec{{
		Name: "remote_press", Description: "press a client control",
		Parameters: json.RawMessage(`{"type":"object"}`), Confirm: action.ConfirmAlways,
		Target: "client-browser",
	}}})
	speak(t, runtime, 3)
	select {
	case record := <-audited:
		if record.Executed || record.Name != "remote_press" {
			t.Fatalf("unexpected remote refusal audit: %+v", record)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the remote confirmation refusal was not audited")
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if item.Kind == trajectory.KindToolResult && item.ToolResult != nil &&
				item.ToolResult.CallID == "remote_1" && item.ToolResult.Error != "" {
				return true
			}
		}
		return false
	}, "the remote confirmation refusal did not become a tool result")
	sink.mu.Lock()
	remoteCalls := len(sink.toolCalls)
	sink.mu.Unlock()
	if remoteCalls != 0 {
		t.Fatal("an unconfirmed client action crossed the wire")
	}
}
