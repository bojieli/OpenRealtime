package openaicompat

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/continuation"
)

// ReasoningControl selects how an endpoint is told whether, and how hard, to
// reason before it answers.
//
// Every hosted provider that speaks Chat Completions has invented its own
// spelling for this, and there is no way to detect which one an endpoint wants
// without asking it and reading the error. So it is declared rather than
// probed: a wrong guess here does not degrade the answer, it fails the request
// with a 400 on the provider's side, which is the worst possible time to find
// out. The catalogue in `providers` is where each name is recorded.
type ReasoningControl string

const (
	// ReasoningControlNone says nothing about reasoning. It is right for an
	// endpoint whose model either always reasons or never does, and where an
	// unrecognised field would be rejected.
	ReasoningControlNone ReasoningControl = "none"
	// ReasoningControlEffort sends the OpenAI `reasoning_effort` field, which
	// xAI, DeepSeek, Groq, and most Western providers followed.
	ReasoningControlEffort ReasoningControl = "reasoning_effort"
	// ReasoningControlThinkingObject sends `thinking: {"type": ...}`, which
	// Zhipu's GLM endpoint and Anthropic-shaped compatibility layers use.
	ReasoningControlThinkingObject ReasoningControl = "thinking_object"
	// ReasoningControlEnableThinking sends a top-level `enable_thinking`
	// boolean, which DashScope accepts for Qwen.
	ReasoningControlEnableThinking ReasoningControl = "enable_thinking"
	// ReasoningControlTemplateKwargs sends `chat_template_kwargs`, which is
	// how vLLM and SGLang pass the switch through to a chat template.
	ReasoningControlTemplateKwargs ReasoningControl = "chat_template_kwargs"
)

// MaxTokensField names the output-token limit field.
//
// OpenAI's reasoning models reject `max_tokens` outright, and endpoints that
// predate that change reject `max_completion_tokens`. There is no field that
// works everywhere, so the choice is part of the profile.
type MaxTokensField string

const (
	MaxTokensLegacy     MaxTokensField = "max_tokens"
	MaxTokensCompletion MaxTokensField = "max_completion_tokens"
)

// defaultEffortNames is the vocabulary an endpoint is assumed to accept when a
// profile does not say otherwise.
var defaultEffortNames = map[continuation.Effort]string{
	continuation.EffortMinimal: "minimal",
	continuation.EffortLow:     "low",
	continuation.EffortMedium:  "medium",
	continuation.EffortHigh:    "high",
}

// validateDialect checks the reasoning and limit profile at construction.
//
// An effort this endpoint has no name for is refused here rather than mapped
// to a neighbouring level. Quietly answering a request for high effort with
// medium would make every measurement that names an effort a measurement of
// something else.
func validateDialect(config *Config) error {
	if config.ReasoningControl == "" {
		if config.ThinkingMode == ThinkingAuto {
			config.ReasoningControl = ReasoningControlNone
		} else {
			config.ReasoningControl = ReasoningControlTemplateKwargs
		}
	}
	switch config.ReasoningControl {
	case ReasoningControlNone, ReasoningControlEffort, ReasoningControlThinkingObject,
		ReasoningControlEnableThinking, ReasoningControlTemplateKwargs:
	default:
		return fmt.Errorf("unsupported reasoning control %q", config.ReasoningControl)
	}
	if config.MaxTokensField == "" {
		config.MaxTokensField = MaxTokensLegacy
	}
	switch config.MaxTokensField {
	case MaxTokensLegacy, MaxTokensCompletion:
	default:
		return fmt.Errorf("unsupported maximum-token field %q", config.MaxTokensField)
	}
	if config.EffortNames == nil {
		config.EffortNames = maps.Clone(defaultEffortNames)
	} else {
		config.EffortNames = maps.Clone(config.EffortNames)
	}
	if config.ReasoningControl == ReasoningControlEffort && config.ThinkingMode != ThinkingDisabled {
		if _, named := config.EffortNames[config.Effort]; !named {
			accepted := make([]string, 0, len(config.EffortNames))
			for effort := range config.EffortNames {
				accepted = append(accepted, string(effort))
			}
			sort.Strings(accepted)
			return fmt.Errorf("endpoint has no name for %q reasoning effort; it accepts %s",
				config.Effort, strings.Join(accepted, ", "))
		}
	}
	config.Headers = maps.Clone(config.Headers)
	for name := range config.Headers {
		if strings.TrimSpace(name) == "" {
			return errors.New("OpenAI-compatible extra header name cannot be empty")
		}
	}
	config.ExtraBody = maps.Clone(config.ExtraBody)
	for name, value := range config.ExtraBody {
		if strings.TrimSpace(name) != name || name == "" {
			return errors.New("OpenAI-compatible extension field name must be non-empty and trimmed")
		}
		if slices.Contains(reservedRequestFields, name) {
			return fmt.Errorf("OpenAI-compatible extension cannot override reserved field %q", name)
		}
		if !json.Valid(value) {
			return fmt.Errorf("OpenAI-compatible extension %q is not valid JSON", name)
		}
	}
	return nil
}

// reservedRequestFields are the fields this adapter owns. A profile that could
// overwrite them could, for instance, turn streaming off, and the adapter only
// knows how to read a stream.
var reservedRequestFields = []string{
	"model", "messages", "stream", "stream_options", "tools", "tool_choice",
	"max_tokens", "max_completion_tokens",
}

// reasoningFields renders this profile's reasoning switch.
//
// Nothing here inspects the model name. A profile that says a field exists is
// the only evidence used, because guessing from a model string is how an
// adapter ends up sending `reasoning_effort` to a server that has never heard
// of it.
func (adapter *Adapter) reasoningFields() (map[string]json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	thinking := adapter.config.ThinkingMode
	switch adapter.config.ReasoningControl {
	case ReasoningControlNone:
	case ReasoningControlEffort:
		name := adapter.config.EffortNames[adapter.config.Effort]
		if thinking == ThinkingDisabled {
			name = adapter.config.DisabledEffort
		}
		if name == "" {
			return fields, nil
		}
		encoded, err := json.Marshal(name)
		if err != nil {
			return nil, fmt.Errorf("encode reasoning effort: %w", err)
		}
		fields["reasoning_effort"] = encoded
	case ReasoningControlThinkingObject:
		switch thinking {
		case ThinkingEnabled:
			fields["thinking"] = json.RawMessage(`{"type":"enabled"}`)
		case ThinkingDisabled:
			fields["thinking"] = json.RawMessage(`{"type":"disabled"}`)
		}
	case ReasoningControlEnableThinking:
		switch thinking {
		case ThinkingEnabled:
			fields["enable_thinking"] = json.RawMessage(`true`)
		case ThinkingDisabled:
			fields["enable_thinking"] = json.RawMessage(`false`)
		}
	case ReasoningControlTemplateKwargs:
		switch thinking {
		case ThinkingEnabled:
			fields["chat_template_kwargs"] = json.RawMessage(`{"enable_thinking":true}`)
		case ThinkingDisabled:
			fields["chat_template_kwargs"] = json.RawMessage(`{"enable_thinking":false}`)
		}
	}
	return fields, nil
}
