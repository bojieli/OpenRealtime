// Package continuation defines provider-neutral fast and slow model
// continuations over one canonical trajectory.
//
// The package is experimental and intentionally separate from stable api/v1.
// It models ordinary streamed reasoning, assistant content, and tool calls; it
// does not introduce a fast/slow advice protocol or alter the OpenAI Realtime
// wire format.
package continuation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// PendingRepairInstruction is injected by provider adapters only while the
// canonical trajectory contains an unresolved typed audible-repair obligation.
// It is runtime policy, not text-authored workflow state.
const PendingRepairInstruction = "Runtime repair obligation: assistant audio from an earlier branch was heard before newer canonical evidence invalidated it. Explicitly correct the audible claim before continuing; do not pretend it was never said."

const PendingRepairPrompt = "Apply the pending audible-repair obligation now."

// ObservationContent renders typed ASR supersession into provider-visible
// context without exposing revision counters or inferring control from text.
func ObservationContent(item trajectory.Item) string {
	if item.Event != nil && item.Event.SupersedesRevision != 0 {
		return "Updated user speech revision; replace the earlier partial observation with this text:\n" + item.Content
	}
	return item.Content
}

// ErrPreempted marks a cooperative resource preemption. Runtimes may resume a
// continuation from the interrupted canonical prefix at the next safe point.
var ErrPreempted = errors.New("continuation preempted at resource safe point")

// ErrStalePrefix means a continuation completed after another event had
// already advanced the canonical trajectory. Its output was not committed and
// must be recomputed from the new prefix.
var ErrStalePrefix = errors.New("continuation computed from a stale trajectory prefix")

// Effort is a provider-neutral reasoning-effort request. Adapters must reject
// unsupported values rather than silently selecting a different effort.
type Effort string

const (
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
)

// ToolAuthority separates knowledge of a tool schema from permission to cause
// a side effect. A proposing continuation may emit a structured candidate
// call, but the runtime records it as non-executable working state. Only an
// executing continuation can append an authoritative tool call.
type ToolAuthority string

const (
	ToolAuthorityNone    ToolAuthority = "none"
	ToolAuthorityPropose ToolAuthority = "propose"
	ToolAuthorityExecute ToolAuthority = "execute"
)

// Descriptor identifies one configured continuation provider.
type Descriptor struct {
	Provider         string           `json:"provider"`
	Model            string           `json:"model"`
	Phase            trajectory.Phase `json:"phase"`
	Effort           Effort           `json:"effort"`
	Streaming        bool             `json:"streaming"`
	NativeStateType  string           `json:"native_state_type,omitempty"`
	RetainsToolCalls bool             `json:"retains_tool_calls"`
	ToolAuthority    ToolAuthority    `json:"tool_authority,omitempty"`
	// ExecutableTools is retained in report JSON for compatibility with the
	// first experimental evidence schema. New code must use ToolAuthority.
	ExecutableTools bool `json:"executable_tools"`
}

// EffectiveToolAuthority normalizes descriptors written before ToolAuthority
// existed. The legacy boolean can only mean execute; it can never grant the
// newer proposal-only authority.
func (descriptor Descriptor) EffectiveToolAuthority() ToolAuthority {
	if descriptor.ToolAuthority != "" {
		return descriptor.ToolAuthority
	}
	if descriptor.ExecutableTools {
		return ToolAuthorityExecute
	}
	return ToolAuthorityNone
}

// Capability describes what the overall agent can do. Fast models receive the
// same capability knowledge and may receive the same tool schemas under
// proposal-only authority; only the slow phase may execute them.
type Capability struct {
	Name                 string `json:"name"`
	Description          string `json:"description"`
	Available            bool   `json:"available"`
	ExecutionPhase       string `json:"execution_phase,omitempty"`
	ConfirmationRequired bool   `json:"confirmation_required,omitempty"`
}

// ToolDefinition is a provider-neutral JSON Schema function definition.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Invocation configures one fast or slow continuation. Instruction is supplied
// as a runtime system instruction and also recorded in the canonical trajectory.
type Invocation struct {
	Instruction     string           `json:"instruction"`
	SourceRevision  uint64           `json:"source_revision,omitempty"`
	Capabilities    []Capability     `json:"capabilities,omitempty"`
	Tools           []ToolDefinition `json:"tools,omitempty"`
	MaxOutputTokens int              `json:"max_output_tokens,omitempty"`
}

// Request is the immutable provider input assembled by Runner.
type Request struct {
	InvocationID string              `json:"invocation_id"`
	Descriptor   Descriptor          `json:"descriptor"`
	Trajectory   trajectory.Snapshot `json:"trajectory"`
	Invocation   Invocation          `json:"invocation"`
}

// EventKind identifies streamed provider output.
type EventKind string

const (
	EventReasoningDelta EventKind = "reasoning_delta"
	EventAssistantDelta EventKind = "assistant_delta"
	EventToolCall       EventKind = "tool_call"
)

// Event is one ordered provider output event. ToolCall is complete; adapters
// assemble provider-specific argument deltas before emitting it.
type Event struct {
	Kind     EventKind            `json:"kind"`
	Text     string               `json:"text,omitempty"`
	ToolCall *trajectory.ToolCall `json:"tool_call,omitempty"`
}

// Usage contains provider-reported token accounting. Zero fields are valid
// when a provider does not expose a category.
type Usage struct {
	InputTokens     int64 `json:"input_tokens,omitempty"`
	OutputTokens    int64 `json:"output_tokens,omitempty"`
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens     int64 `json:"total_tokens,omitempty"`
}

// Completion is terminal provider metadata. ProviderState is an opaque native
// model-turn representation, such as Gemini content with thought signatures.
// The matching ProviderStateType prevents cross-provider reinterpretation.
type Completion struct {
	StopReason        string          `json:"stop_reason,omitempty"`
	Usage             Usage           `json:"usage,omitempty"`
	ProviderStateType string          `json:"provider_state_type,omitempty"`
	ProviderState     json.RawMessage `json:"provider_state,omitempty"`
}

// Emit receives streamed provider events. Returning an error cancels the
// continuation at the next provider-supported boundary.
type Emit func(Event) error

// Provider streams one continuation over the supplied canonical prefix.
type Provider interface {
	Descriptor() Descriptor
	Continue(context.Context, Request, Emit) (Completion, error)
}

// ValidateDescriptor validates fields shared by provider implementations.
func ValidateDescriptor(descriptor Descriptor) error {
	if strings.TrimSpace(descriptor.Provider) == "" || strings.TrimSpace(descriptor.Model) == "" {
		return errors.New("continuation provider and model are required")
	}
	if descriptor.Phase != trajectory.PhaseFast && descriptor.Phase != trajectory.PhaseSlow {
		return errors.New("continuation phase must be fast or slow")
	}
	switch descriptor.Effort {
	case EffortMinimal, EffortLow, EffortMedium, EffortHigh:
	default:
		return fmt.Errorf("unsupported reasoning effort %q", descriptor.Effort)
	}
	if descriptor.NativeStateType != "" && strings.TrimSpace(descriptor.NativeStateType) == "" {
		return errors.New("native state type cannot be whitespace")
	}
	switch descriptor.EffectiveToolAuthority() {
	case ToolAuthorityNone, ToolAuthorityPropose, ToolAuthorityExecute:
	default:
		return fmt.Errorf("unsupported tool authority %q", descriptor.ToolAuthority)
	}
	if descriptor.ExecutableTools && descriptor.EffectiveToolAuthority() != ToolAuthorityExecute {
		return errors.New("legacy executable_tools conflicts with non-executing tool authority")
	}
	return nil
}

// ValidateInvocation checks portable capability and tool definitions before a
// request reaches a provider.
func ValidateInvocation(invocation Invocation, descriptor Descriptor) error {
	if strings.TrimSpace(invocation.Instruction) == "" {
		return errors.New("continuation instruction is required")
	}
	if invocation.MaxOutputTokens < 0 {
		return errors.New("maximum output tokens cannot be negative")
	}
	capabilities := make(map[string]struct{}, len(invocation.Capabilities))
	for index, capability := range invocation.Capabilities {
		if strings.TrimSpace(capability.Name) == "" || strings.TrimSpace(capability.Description) == "" {
			return fmt.Errorf("capability %d requires name and description", index)
		}
		if _, exists := capabilities[capability.Name]; exists {
			return fmt.Errorf("duplicate capability %q", capability.Name)
		}
		capabilities[capability.Name] = struct{}{}
	}
	tools := make(map[string]struct{}, len(invocation.Tools))
	for index, tool := range invocation.Tools {
		if strings.TrimSpace(tool.Name) == "" || strings.TrimSpace(tool.Description) == "" {
			return fmt.Errorf("tool %d requires name and description", index)
		}
		if _, exists := tools[tool.Name]; exists {
			return fmt.Errorf("duplicate tool %q", tool.Name)
		}
		tools[tool.Name] = struct{}{}
		if len(tool.Parameters) == 0 || !json.Valid(tool.Parameters) {
			return fmt.Errorf("tool %q parameters must be valid JSON", tool.Name)
		}
		var schema map[string]json.RawMessage
		if err := json.Unmarshal(tool.Parameters, &schema); err != nil || schema == nil {
			return fmt.Errorf("tool %q parameters must be one JSON object", tool.Name)
		}
	}
	if len(invocation.Tools) > 0 && descriptor.EffectiveToolAuthority() == ToolAuthorityNone {
		return errors.New("continuation provider has no authority to receive tool definitions")
	}
	return nil
}

// ValidateEvent rejects ambiguous or partial portable stream events.
func ValidateEvent(event Event) error {
	switch event.Kind {
	case EventReasoningDelta, EventAssistantDelta:
		if event.Text == "" || event.ToolCall != nil {
			return fmt.Errorf("%s requires text and no tool call", event.Kind)
		}
	case EventToolCall:
		if event.Text != "" || event.ToolCall == nil {
			return errors.New("tool_call event requires exactly one tool call")
		}
		if err := validateEmittedToolCall(*event.ToolCall); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown continuation event kind %q", event.Kind)
	}
	return nil
}

func validateEmittedToolCall(call trajectory.ToolCall) error {
	if strings.TrimSpace(call.CallID) == "" || strings.TrimSpace(call.Name) == "" {
		return errors.New("emitted tool call ID and name are required")
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return errors.New("emitted tool arguments must be valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &object); err != nil || object == nil {
		return errors.New("emitted tool arguments must be one JSON object")
	}
	return nil
}
