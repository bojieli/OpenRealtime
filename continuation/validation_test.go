package continuation_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// ValidateDescriptor, ValidateInvocation, and ValidateEvent are the boundary a
// provider's claims cross before anything downstream treats them as authority:
// the descriptor says what a model is allowed to do, the invocation says what
// tools it may even be shown, and the event says what it actually emitted. All
// three were exercised only on their accepting paths, so every refusal in them
// could be deleted without a test noticing. These cover the refusals.

func validDescriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "test-model", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
}

func TestValidateDescriptorRefusesUnattributedPhaselessAndOverreachingModels(t *testing.T) {
	if err := continuation.ValidateDescriptor(validDescriptor()); err != nil {
		t.Fatalf("well-formed descriptor = %v, want accepted", err)
	}
	for _, test := range []struct {
		name string
		edit func(*continuation.Descriptor)
		want string
	}{
		{"no provider", func(d *continuation.Descriptor) { d.Provider = "  " }, "provider and model are required"},
		{"no model", func(d *continuation.Descriptor) { d.Model = "" }, "provider and model are required"},
		{
			name: "phase is neither fast nor slow",
			edit: func(d *continuation.Descriptor) { d.Phase = trajectory.PhaseRuntime },
			want: "phase must be fast or slow",
		},
		{
			name: "reasoning effort is not one of the canonical levels",
			edit: func(d *continuation.Descriptor) { d.Effort = continuation.Effort("exhaustive") },
			want: "unsupported reasoning effort",
		},
		{
			name: "native state type is whitespace",
			edit: func(d *continuation.Descriptor) { d.NativeStateType = "   " },
			want: "cannot be whitespace",
		},
		{
			name: "tool authority is not a declared level",
			edit: func(d *continuation.Descriptor) { d.ToolAuthority = continuation.ToolAuthority("admin") },
			want: "unsupported tool authority",
		},
		{
			// The legacy boolean cannot be used to widen a narrower typed
			// authority; if it could, an old configuration would silently
			// promote a proposing model to an executing one.
			name: "legacy executable_tools contradicts the typed authority",
			edit: func(d *continuation.Descriptor) { d.ExecutableTools = true },
			want: "conflicts with non-executing tool authority",
		},
		{
			name: "speech authority is not a declared level",
			edit: func(d *continuation.Descriptor) {
				d.SpeechAuthority = continuation.SpeechAuthority("shout")
			},
			want: "unsupported speech authority",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor := validDescriptor()
			test.edit(&descriptor)
			err := continuation.ValidateDescriptor(descriptor)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("descriptor error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func validInvocation() continuation.Invocation {
	return continuation.Invocation{
		Instruction: "answer briefly", SourceRevision: 1,
		Capabilities: []continuation.Capability{{Name: "vision", Description: "sees frames"}},
		Tools: []continuation.ToolDefinition{{
			Name: "lookup", Description: "look something up",
			Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	}
}

func TestValidateInvocationRefusesMalformedAndUnauthorizedToolDeclarations(t *testing.T) {
	if err := continuation.ValidateInvocation(validInvocation(), validDescriptor()); err != nil {
		t.Fatalf("well-formed invocation = %v, want accepted", err)
	}
	for _, test := range []struct {
		name       string
		edit       func(*continuation.Invocation)
		descriptor continuation.Descriptor
		want       string
	}{
		{
			name: "no instruction",
			edit: func(value *continuation.Invocation) { value.Instruction = "   " },
			want: "instruction is required",
		},
		{
			name: "negative output bound",
			edit: func(value *continuation.Invocation) { value.MaxOutputTokens = -1 },
			want: "cannot be negative",
		},
		{
			name: "capability without a description",
			edit: func(value *continuation.Invocation) { value.Capabilities[0].Description = "" },
			want: "requires name and description",
		},
		{
			name: "duplicate capability",
			edit: func(value *continuation.Invocation) {
				value.Capabilities = append(value.Capabilities, value.Capabilities[0])
			},
			want: "duplicate capability",
		},
		{
			name: "tool without a description",
			edit: func(value *continuation.Invocation) { value.Tools[0].Description = "  " },
			want: "requires name and description",
		},
		{
			name: "duplicate tool",
			edit: func(value *continuation.Invocation) {
				value.Tools = append(value.Tools, value.Tools[0])
			},
			want: "duplicate tool",
		},
		{
			name: "tool parameters are not valid JSON",
			edit: func(value *continuation.Invocation) {
				value.Tools[0].Parameters = json.RawMessage(`{"type":`)
			},
			want: "must be valid JSON",
		},
		{
			name: "tool parameters are valid JSON but not an object",
			edit: func(value *continuation.Invocation) {
				value.Tools[0].Parameters = json.RawMessage(`["type"]`)
			},
			want: "must be one JSON object",
		},
		{
			// A model with no tool authority must not even be shown the
			// catalogue: the refusal belongs before the request, not after a
			// proposal comes back.
			name:       "tools offered to a model with no tool authority",
			edit:       func(*continuation.Invocation) {},
			descriptor: continuation.Descriptor{},
			want:       "no authority to receive tool definitions",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			invocation := validInvocation()
			test.edit(&invocation)
			descriptor := test.descriptor
			if descriptor.Provider == "" && test.name != "tools offered to a model with no tool authority" {
				descriptor = validDescriptor()
			}
			err := continuation.ValidateInvocation(invocation, descriptor)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invocation error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

func TestValidateEventRefusesAmbiguousAndPartialStreamEvents(t *testing.T) {
	call := trajectory.ToolCall{
		CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":"x"}`),
	}
	for _, kind := range []continuation.EventKind{
		continuation.EventReasoningDelta, continuation.EventAssistantDelta,
	} {
		if err := continuation.ValidateEvent(continuation.Event{Kind: kind, Text: "x"}); err != nil {
			t.Fatalf("%s with text = %v, want accepted", kind, err)
		}
	}
	if err := continuation.ValidateEvent(continuation.Event{
		Kind: continuation.EventToolCall, ToolCall: &call,
	}); err != nil {
		t.Fatalf("tool call event = %v, want accepted", err)
	}
	for _, test := range []struct {
		name  string
		event continuation.Event
		want  string
	}{
		{
			name:  "assistant delta carries no text",
			event: continuation.Event{Kind: continuation.EventAssistantDelta},
			want:  "requires text and no tool call",
		},
		{
			name: "assistant delta smuggles a tool call",
			event: continuation.Event{
				Kind: continuation.EventAssistantDelta, Text: "x", ToolCall: &call,
			},
			want: "requires text and no tool call",
		},
		{
			name:  "reasoning delta carries no text",
			event: continuation.Event{Kind: continuation.EventReasoningDelta},
			want:  "requires text and no tool call",
		},
		{
			name:  "tool call event carries no call",
			event: continuation.Event{Kind: continuation.EventToolCall},
			want:  "exactly one tool call",
		},
		{
			name: "tool call event also carries text",
			event: continuation.Event{
				Kind: continuation.EventToolCall, Text: "x", ToolCall: &call,
			},
			want: "exactly one tool call",
		},
		{
			name:  "unknown event kind",
			event: continuation.Event{Kind: continuation.EventKind("speech_delta"), Text: "x"},
			want:  "unknown continuation event kind",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := continuation.ValidateEvent(test.event)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("event error = %v, want one containing %q", err, test.want)
			}
		})
	}
}

// An emitted tool call is the one place a provider's output becomes a request
// to act, so a malformed one has to be refused at the stream boundary rather
// than by whatever tries to dispatch it later.
func TestEmittedToolCallsAreRefusedAtTheStreamBoundary(t *testing.T) {
	for _, test := range []struct {
		name string
		call trajectory.ToolCall
		want string
	}{
		{
			name: "no call ID",
			call: trajectory.ToolCall{Name: "lookup", Arguments: json.RawMessage(`{}`)},
			want: "ID and name are required",
		},
		{
			name: "no name",
			call: trajectory.ToolCall{CallID: "call-1", Arguments: json.RawMessage(`{}`)},
			want: "ID and name are required",
		},
		{
			name: "no arguments at all",
			call: trajectory.ToolCall{CallID: "call-1", Name: "lookup"},
			want: "must be valid JSON",
		},
		{
			name: "truncated arguments",
			call: trajectory.ToolCall{
				CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"q":`),
			},
			want: "must be valid JSON",
		},
		{
			name: "arguments are valid JSON but not an object",
			call: trajectory.ToolCall{
				CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`"q"`),
			},
			want: "must be one JSON object",
		},
		{
			name: "arguments are a JSON null",
			call: trajectory.ToolCall{
				CallID: "call-1", Name: "lookup", Arguments: json.RawMessage(`null`),
			},
			want: "must be one JSON object",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := continuation.ValidateEvent(continuation.Event{
				Kind: continuation.EventToolCall, ToolCall: &test.call,
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("emitted tool call error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
