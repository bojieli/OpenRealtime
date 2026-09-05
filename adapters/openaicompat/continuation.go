// Package openaicompat implements canonical-trajectory continuation through
// an OpenAI-compatible Chat Completions endpoint, including local vLLM.
//
// This package is a model-provider adapter. It does not change or extend the
// OpenAI Realtime client/server wire protocol used elsewhere in OpenRealtime.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/internal/httpclient"
	"github.com/bojieli/OpenRealtime/internal/sse"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	// DefaultBaseURL is the conventional local vLLM API location.
	DefaultBaseURL = "http://127.0.0.1:8000/v1"
	// ProviderStateType identifies a retained assistant Chat Completions message
	// wrapped with the provider and model that produced it.
	ProviderStateType = "openai-compatible.chat-completion.assistant-message.v1"

	defaultMaxTokens = 1_024
	maxErrorBody     = 64 << 10
	maxSSEEvent      = 16 << 20
)

// ThinkingMode controls the Qwen-style enable_thinking chat-template option.
// Auto sends no provider extension and is appropriate for generic endpoints.
type ThinkingMode string

const (
	ThinkingAuto     ThinkingMode = "auto"
	ThinkingEnabled  ThinkingMode = "enabled"
	ThinkingDisabled ThinkingMode = "disabled"
)

// ReasoningDeltaField declares the exact nonstandard streaming field used by
// an endpoint for reasoning text. No response-content heuristics are applied.
type ReasoningDeltaField string

const (
	ReasoningContentField ReasoningDeltaField = "reasoning_content"
	ReasoningField        ReasoningDeltaField = "reasoning"
)

// Config configures one immutable local or hosted Chat Completions profile.
// APIKey may be empty for a trusted local endpoint. Provider should identify
// the serving stack, for example "vllm", rather than pretending to be OpenAI.
type Config struct {
	APIKey        string
	Model         string
	BaseURL       string
	Provider      string
	Phase         trajectory.Phase
	Effort        continuation.Effort
	ToolAuthority continuation.ToolAuthority
	// SpeechAuthority declares whether this provider's output may be voiced.
	// An unset value means voice; the fast/slow arrangement is what sets a
	// provider silent, and it does so explicitly.
	SpeechAuthority continuation.SpeechAuthority
	// Vision declares that the served model accepts images. It defaults to
	// false because an OpenAI-compatible endpoint serves whatever it was
	// pointed at, and handing an image to a text-only model fails the turn
	// outright rather than degrading it.
	Vision bool
	// AllowTools is a compatibility alias for ToolAuthorityExecute.
	AllowTools              bool
	ThinkingMode            ThinkingMode
	ReasoningDeltaField     ReasoningDeltaField
	DisableReasoningCapture bool
	// ReasoningControl selects the field this endpoint reads the reasoning
	// switch from. Empty preserves the historical behaviour: nothing is sent
	// under ThinkingAuto, and the vLLM chat-template form otherwise.
	ReasoningControl ReasoningControl
	// EffortNames maps portable effort onto this endpoint's vocabulary. Nil
	// selects the identity mapping. An effort with no entry is rejected at
	// construction rather than replaced with a nearby one.
	EffortNames map[continuation.Effort]string
	// DisabledEffort is the effort name that means "do not reason", for
	// endpoints that spell the off switch as a level. Empty omits the field.
	DisabledEffort string
	// MaxTokensField names the output-limit field. Empty selects max_tokens.
	MaxTokensField MaxTokensField
	// Headers are extra request headers, such as the attribution headers
	// OpenRouter asks callers to send.
	Headers map[string]string
	// ExtraBody adds provider-specific top-level request fields. It cannot
	// overwrite a field this adapter owns.
	ExtraBody      map[string]json.RawMessage
	Temperature    *float64
	Seed           *int64
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

// Adapter streams Chat Completions and preserves same-provider assistant state
// without treating state from a different model as native continuation.
type Adapter struct {
	config     Config
	descriptor continuation.Descriptor
}

// New validates config and creates an adapter. Reasoning deltas are captured
// internally by default and can be disabled explicitly.
func New(config Config) (*Adapter, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		return nil, errors.New("OpenAI-compatible model is required")
	}
	if config.BaseURL == "" {
		config.BaseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("OpenAI-compatible base URL must be absolute")
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	if config.Provider == "" {
		config.Provider = "openai-compatible"
	}
	config.Provider = strings.TrimSpace(config.Provider)
	if config.Provider == "" {
		return nil, errors.New("OpenAI-compatible provider name is required")
	}
	if config.Phase == "" {
		config.Phase = trajectory.PhaseFast
	}
	if config.Effort == "" {
		config.Effort = continuation.EffortMinimal
	}
	if config.ToolAuthority == "" {
		if config.AllowTools {
			config.ToolAuthority = continuation.ToolAuthorityExecute
		} else {
			config.ToolAuthority = continuation.ToolAuthorityNone
		}
	}
	if config.AllowTools && config.ToolAuthority != continuation.ToolAuthorityExecute {
		return nil, errors.New("allow_tools conflicts with non-executing tool authority")
	}
	if config.ThinkingMode == "" {
		config.ThinkingMode = ThinkingAuto
	}
	switch config.ThinkingMode {
	case ThinkingAuto, ThinkingEnabled, ThinkingDisabled:
	default:
		return nil, fmt.Errorf("unsupported thinking mode %q", config.ThinkingMode)
	}
	if config.ReasoningDeltaField == "" {
		config.ReasoningDeltaField = ReasoningContentField
	}
	switch config.ReasoningDeltaField {
	case ReasoningContentField, ReasoningField:
	default:
		return nil, fmt.Errorf("unsupported reasoning delta field %q", config.ReasoningDeltaField)
	}
	if err := validateDialect(&config); err != nil {
		return nil, err
	}
	if config.Temperature != nil && (*config.Temperature < 0 || *config.Temperature > 2) {
		return nil, errors.New("temperature must be between 0 and 2")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = httpclient.Shared()
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("OpenAI-compatible request timeout cannot be negative")
	}
	temperature := ""
	if config.Temperature != nil {
		temperature = strconv.FormatFloat(*config.Temperature, 'g', -1, 64)
	}
	descriptor := continuation.Descriptor{
		Provider: config.Provider, Model: config.Model, Phase: config.Phase,
		Effort: config.Effort, Streaming: true, NativeStateType: ProviderStateType,
		RetainsToolCalls: true, ToolAuthority: config.ToolAuthority,
		SpeechAuthority: config.SpeechAuthority, Vision: config.Vision,
		SamplingTemperature: temperature,
		ExecutableTools:     config.ToolAuthority == continuation.ToolAuthorityExecute,
	}
	if err := continuation.ValidateDescriptor(descriptor); err != nil {
		return nil, err
	}
	return &Adapter{config: config, descriptor: descriptor}, nil
}

// Descriptor returns the immutable provider configuration.
func (adapter *Adapter) Descriptor() continuation.Descriptor { return adapter.descriptor }

type chatRequest struct {
	Model         string            `json:"model"`
	Messages      []chatMessage     `json:"messages"`
	Stream        bool              `json:"stream"`
	StreamOptions chatStreamOptions `json:"stream_options"`
	Tools         []chatTool        `json:"tools,omitempty"`
	ToolChoice    string            `json:"tool_choice,omitempty"`
	Temperature   *float64          `json:"temperature,omitempty"`
	Seed          *int64            `json:"seed,omitempty"`

	// maxTokens is serialised under maxTokensField, which differs between an
	// endpoint that predates OpenAI's reasoning models and one that does not.
	maxTokens      int
	maxTokensField MaxTokensField
	// extra carries the reasoning switch and any profile extensions. They are
	// merged last so a profile is visible in the encoded request exactly as it
	// was configured.
	extra map[string]json.RawMessage
}

// MarshalJSON renders the request with the profile's field names applied.
func (request chatRequest) MarshalJSON() ([]byte, error) {
	type plain chatRequest
	encoded, err := json.Marshal(plain(request))
	if err != nil {
		return nil, err
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &object); err != nil {
		return nil, err
	}
	limit, err := json.Marshal(request.maxTokens)
	if err != nil {
		return nil, err
	}
	field := request.maxTokensField
	if field == "" {
		field = MaxTokensLegacy
	}
	object[string(field)] = limit
	for name, value := range request.extra {
		object[name] = value
	}
	return json.Marshal(object)
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content and Parts are alternatives: a message carries either plain text
	// or the structured content array that images require. Exactly one is
	// serialised, because a server given both has to guess.
	Content          string         `json:"content,omitempty"`
	Parts            []contentPart  `json:"-"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

// MarshalJSON emits the structured form when there are parts.
func (message chatMessage) MarshalJSON() ([]byte, error) {
	type plain chatMessage
	if len(message.Parts) == 0 {
		return json.Marshal(plain(message))
	}
	structured := struct {
		Role             string         `json:"role"`
		Content          []contentPart  `json:"content"`
		ReasoningContent string         `json:"reasoning_content,omitempty"`
		ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
		ToolCallID       string         `json:"tool_call_id,omitempty"`
	}{
		Role: message.Role, Content: message.Parts,
		ReasoningContent: message.ReasoningContent,
		ToolCalls:        message.ToolCalls, ToolCallID: message.ToolCallID,
	}
	return json.Marshal(structured)
}

// contentPart is one element of a structured message body.
type contentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *contentImage `json:"image_url,omitempty"`
}

type contentImage struct {
	URL string `json:"url"`
}

// attachMedia resolves an observation's handles into image parts.
//
// A handle that no longer resolves is skipped rather than failing the
// continuation: retention is bounded on purpose, and a model that gets the
// narration without the image is in exactly the state this design expects
// once the window has passed.
func attachMedia(
	item trajectory.Item, media continuation.MediaResolver, vision bool, selected map[string]struct{},
) []contentPart {
	if !vision || media == nil || item.Observation == nil || len(item.Observation.Media) == 0 {
		return nil
	}
	var parts []contentPart
	for _, reference := range item.Observation.Media {
		if _, ok := selected[reference.Handle]; !ok {
			continue
		}
		resolved, err := media(reference.Handle)
		if err != nil || len(resolved.Bytes) == 0 {
			continue
		}
		mimeType := resolved.MIMEType
		if mimeType == "" {
			mimeType = reference.MIMEType
		}
		parts = append(parts, contentPart{
			Type: "image_url",
			ImageURL: &contentImage{
				URL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(resolved.Bytes),
			},
		})
	}
	return parts
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

type chatToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
	// ExtraContent carries whatever a provider hangs off a tool call that is
	// not part of the shape everyone shares. It is kept verbatim and sent back
	// verbatim, because a field this adapter does not understand is exactly
	// the field it must not drop.
	//
	// Gemini puts a thought_signature here when it is thinking, and refuses
	// the next request without it: "Function call is missing a
	// thought_signature in functionCall parts", HTTP 400, which took the
	// recorded-menu scenario from passing to erroring the moment the voice was
	// given a reasoning budget. Decoding into a struct and re-encoding is
	// lossy by construction, and this is the loss that showed.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type providerState struct {
	Provider string      `json:"provider"`
	Model    string      `json:"model"`
	Message  chatMessage `json:"message"`
}

type streamEnvelope struct {
	Choices []struct {
		Index        int             `json:"index"`
		Delta        json.RawMessage `json:"delta"`
		FinishReason *string         `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens       int64 `json:"prompt_tokens"`
		CompletionTokens   int64 `json:"completion_tokens"`
		TotalTokens        int64 `json:"total_tokens"`
		PromptTokenDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details,omitempty"`
		CompletionTokenDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details,omitempty"`
	} `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error,omitempty"`
}

type streamDelta struct {
	Role             string                `json:"role,omitempty"`
	Content          *string               `json:"content"`
	ReasoningContent *string               `json:"reasoning_content"`
	Reasoning        *string               `json:"reasoning"`
	ToolCalls        []streamToolCallDelta `json:"tool_calls,omitempty"`
}

type streamToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
	// ExtraContent is the provider extension on this call, sent whole rather
	// than in pieces. Gemini puts a thought_signature here.
	ExtraContent json.RawMessage `json:"extra_content,omitempty"`
}

type pendingToolCall struct {
	id        strings.Builder
	typeName  strings.Builder
	name      strings.Builder
	arguments strings.Builder
	// extras is whatever the provider hung off this call that is not part of
	// the shape everyone shares. It arrives once rather than in pieces, and it
	// has to survive into the retained message: Gemini refuses the next
	// request without the thought_signature it put here.
	extras json.RawMessage
}

// Continue implements continuation.Provider using streaming Chat Completions.
func (adapter *Adapter) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	if request.Descriptor != adapter.descriptor {
		return continuation.Completion{}, errors.New("OpenAI-compatible request descriptor does not match adapter")
	}
	body, err := adapter.buildRequest(request)
	if err != nil {
		return continuation.Completion{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("encode OpenAI-compatible request: %w", err)
	}
	if adapter.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, adapter.config.RequestTimeout)
		defer cancel()
	}
	continuation.TraceRequest(adapter.Descriptor(), request.InvocationID, encoded)
	endpoint := adapter.config.BaseURL + "/chat/completions"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("create OpenAI-compatible request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	if adapter.config.APIKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+adapter.config.APIKey)
	}
	for name, value := range adapter.config.Headers {
		httpRequest.Header.Set(name, value)
	}
	response, err := adapter.config.HTTPClient.Do(httpRequest)
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("send OpenAI-compatible request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return continuation.Completion{}, fmt.Errorf("OpenAI-compatible endpoint returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}

	completion := continuation.Completion{ProviderStateType: ProviderStateType}
	var reasoning, content strings.Builder
	var thinking thinkingFilter
	pendingCalls := make(map[int]*pendingToolCall)
	err = sse.Read(response.Body, maxSSEEvent, func(data []byte) error {
		if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
			return nil
		}
		var envelope streamEnvelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			return fmt.Errorf("decode OpenAI-compatible stream event: %w", err)
		}
		if envelope.Error != nil {
			return fmt.Errorf("OpenAI-compatible %s error (%v): %s", envelope.Error.Type, envelope.Error.Code, envelope.Error.Message)
		}
		if envelope.Usage != nil {
			completion.Usage = continuation.Usage{
				InputTokens: envelope.Usage.PromptTokens, OutputTokens: envelope.Usage.CompletionTokens,
				TotalTokens: envelope.Usage.TotalTokens,
			}
			if envelope.Usage.PromptTokenDetails != nil {
				completion.Usage.CachedInputTokens = envelope.Usage.PromptTokenDetails.CachedTokens
				completion.Usage.CachedInputTokensReported = true
			}
			if envelope.Usage.CompletionTokenDetails != nil {
				completion.Usage.ReasoningTokens = envelope.Usage.CompletionTokenDetails.ReasoningTokens
			}
		}
		for _, choice := range envelope.Choices {
			if choice.Index != 0 {
				return fmt.Errorf("OpenAI-compatible endpoint streamed unexpected choice index %d", choice.Index)
			}
			if choice.FinishReason != nil {
				completion.StopReason = *choice.FinishReason
			}
			if len(choice.Delta) == 0 || bytes.Equal(choice.Delta, []byte("null")) {
				continue
			}
			var delta streamDelta
			if err := json.Unmarshal(choice.Delta, &delta); err != nil {
				return fmt.Errorf("decode OpenAI-compatible choice delta: %w", err)
			}
			if !adapter.config.DisableReasoningCapture {
				text := adapter.reasoningDelta(delta)
				if text != "" {
					reasoning.WriteString(text)
					if err := emit(continuation.Event{Kind: continuation.EventReasoningDelta, Text: text}); err != nil {
						return err
					}
				}
			}
			if delta.Content != nil && *delta.Content != "" {
				// A model that writes its reasoning into content is still
				// reasoning. Routing it to the reasoning channel is what keeps
				// the fast provider's deliberation from being spoken.
				leakedReasoning, spoken := thinking.push(*delta.Content)
				if leakedReasoning != "" && !adapter.config.DisableReasoningCapture {
					reasoning.WriteString(leakedReasoning)
					if err := emit(continuation.Event{
						Kind: continuation.EventReasoningDelta, Text: leakedReasoning,
					}); err != nil {
						return err
					}
				}
				if spoken != "" {
					content.WriteString(spoken)
					if err := emit(continuation.Event{
						Kind: continuation.EventAssistantDelta, Text: spoken,
					}); err != nil {
						return err
					}
				}
			}
			for _, callDelta := range delta.ToolCalls {
				pending := pendingCalls[callDelta.Index]
				if pending == nil {
					pending = &pendingToolCall{}
					pendingCalls[callDelta.Index] = pending
				}
				pending.id.WriteString(callDelta.ID)
				pending.typeName.WriteString(callDelta.Type)
				pending.name.WriteString(callDelta.Function.Name)
				pending.arguments.WriteString(callDelta.Function.Arguments)
				if len(callDelta.ExtraContent) > 0 {
					pending.extras = callDelta.ExtraContent
				}
			}
		}
		return nil
	})

	// An unterminated block is the common case rather than an odd one: a short
	// output budget runs out mid-deliberation and the closing tag never
	// arrives. What was held back is still reasoning, and speaking it would
	// say exactly the half of the thought that fit.
	if heldReasoning, heldContent := thinking.flush(); heldReasoning != "" || heldContent != "" {
		if heldReasoning != "" && !adapter.config.DisableReasoningCapture {
			reasoning.WriteString(heldReasoning)
			if emitErr := emit(continuation.Event{
				Kind: continuation.EventReasoningDelta, Text: heldReasoning,
			}); emitErr != nil && err == nil {
				err = emitErr
			}
		}
		if heldContent != "" {
			content.WriteString(heldContent)
			if emitErr := emit(continuation.Event{
				Kind: continuation.EventAssistantDelta, Text: heldContent,
			}); emitErr != nil && err == nil {
				err = emitErr
			}
		}
	}

	completion.ReasoningInContent = thinking.Leaked()
	message := chatMessage{Role: "assistant", Content: content.String(), ReasoningContent: reasoning.String()}
	indices := make([]int, 0, len(pendingCalls))
	for index := range pendingCalls {
		indices = append(indices, index)
	}
	slices.Sort(indices)
	for ordinal, index := range indices {
		if err != nil {
			break
		}
		pending := pendingCalls[index]
		callID := strings.TrimSpace(pending.id.String())
		if callID == "" {
			callID = request.InvocationID + "-call-" + strconv.Itoa(ordinal+1)
		}
		typeName := strings.TrimSpace(pending.typeName.String())
		if typeName == "" {
			typeName = "function"
		}
		arguments := strings.TrimSpace(pending.arguments.String())
		if arguments == "" {
			arguments = `{}`
		}
		call := chatToolCall{
			Index: index, ID: callID, Type: typeName,
			Function:     chatFunction{Name: pending.name.String(), Arguments: arguments},
			ExtraContent: pending.extras,
		}
		if emitErr := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(call.Function.Arguments),
		}}); emitErr != nil {
			err = emitErr
			break
		}
		message.ToolCalls = append(message.ToolCalls, call)
	}
	if message.Content != "" || message.ReasoningContent != "" || len(message.ToolCalls) > 0 {
		completion.ProviderState, _ = json.Marshal(providerState{
			Provider: adapter.descriptor.Provider, Model: adapter.descriptor.Model, Message: message,
		})
	} else {
		completion.ProviderStateType = ""
	}
	if err != nil {
		return completion, err
	}
	return completion, nil
}

func (adapter *Adapter) reasoningDelta(delta streamDelta) string {
	switch adapter.config.ReasoningDeltaField {
	case ReasoningField:
		if delta.Reasoning != nil {
			return *delta.Reasoning
		}
	default:
		if delta.ReasoningContent != nil {
			return *delta.ReasoningContent
		}
	}
	return ""
}

// dumpRequests names a file every assembled request is appended to, one JSON
// object per line, when the environment sets it.
//
// Reasoning about which part of a prompt differs from one tried by hand has
// been wrong four times in this work - the scenario preamble, the holding
// paragraph, the sampling temperature, the position of the instruction - and
// each wrong answer cost a round. The messages that actually go out settle it
// in one, and an environment variable keeps the facility out of the way until
// somebody needs it.
const dumpRequests = "OPENREALTIME_DUMP_REQUESTS"

func (adapter *Adapter) dump(result chatRequest) {
	path := strings.TrimSpace(os.Getenv(dumpRequests))
	if path == "" {
		return
	}
	// Marshal the assembled provider body, not a messages-only projection.
	// Tool declarations, selection policy, sampling controls, and dialect
	// extensions materially affect model output and are required to reproduce a
	// diagnostic. Authentication remains outside chatRequest and is never
	// retained here.
	encoded, err := json.Marshal(result)
	if err != nil {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(encoded, '\n'))
}

func (adapter *Adapter) buildRequest(request continuation.Request) (chatRequest, error) {
	maxTokens := request.Invocation.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	result := chatRequest{
		Model: adapter.descriptor.Model, Stream: true,
		StreamOptions: chatStreamOptions{IncludeUsage: true},
		Temperature:   adapter.config.Temperature, Seed: adapter.config.Seed,
		maxTokens: maxTokens, maxTokensField: adapter.config.MaxTokensField,
	}
	reasoning, err := adapter.reasoningFields(request.Invocation.Effort)
	if err != nil {
		return chatRequest{}, err
	}
	result.extra = reasoning
	for name, value := range adapter.config.ExtraBody {
		result.extra[name] = value
	}

	// Capability data establishes what exists; Invocation.Instruction says how
	// this phase must use it. Keep the governing policy after the manifest, and
	// an urgent repair after both. Historical instruction items are audit
	// records, not additional system prompts; replaying them grows context and
	// presents contradictory phase policies on long conversations.
	var instructions []string
	assistantVisibility := trajectory.AssistantVisibility(request.Trajectory)
	cancelledInvocations := trajectory.CancelledAssistantInvocations(request.Trajectory)
	// Content the user heard only part of is projected down to the part they
	// heard. Retained native state is the whole turn, so replaying it would put
	// the unheard half back.
	partlyHeardInvocations := trajectory.PartlyHeardAssistantInvocations(request.Trajectory)
	nativeInvocations := make(map[string]chatMessage)
	for _, item := range request.Trajectory.Items {
		if _, cancelled := cancelledInvocations[item.InvocationID]; cancelled {
			continue
		}
		if _, partly := partlyHeardInvocations[item.InvocationID]; partly {
			continue
		}
		if item.ProviderStateType != ProviderStateType || len(item.ProviderState) == 0 || item.InvocationID == "" {
			continue
		}
		if _, duplicate := nativeInvocations[item.InvocationID]; duplicate {
			continue
		}
		var state providerState
		if err := json.Unmarshal(item.ProviderState, &state); err != nil || state.Message.Role != "assistant" {
			return chatRequest{}, fmt.Errorf("decode retained OpenAI-compatible state on item %s", item.ID)
		}
		if state.Provider == adapter.descriptor.Provider && state.Model == adapter.descriptor.Model {
			nativeInvocations[item.InvocationID] = state.Message
		}
	}
	if len(request.Invocation.Capabilities) > 0 {
		encoded, err := json.Marshal(request.Invocation.Capabilities)
		if err != nil {
			return chatRequest{}, fmt.Errorf("encode capability manifest: %w", err)
		}
		instructions = append(instructions, "The following is the complete set of capabilities this agent has. Schema visibility is not execution authority: the runtime records whether a tool-channel call is executable or only a non-executable proposal, and reasoning may revise or reject a proposal. Never tell the user the agent lacks a capability that is listed here:\n"+string(encoded))
	}
	instructions = append(instructions, request.Invocation.Instruction)
	if len(trajectory.PendingRepairs(request.Trajectory)) > 0 {
		instructions = append(instructions, continuation.PendingRepairInstruction)
	}
	if len(instructions) > 0 {
		result.Messages = append(result.Messages, chatMessage{Role: "system", Content: strings.Join(instructions, "\n\n")})
	}

	consumedInvocations := make(map[string]struct{})
	promotedProposals := trajectory.PromotedToolProposalIDs(request.Trajectory)
	terminalProposals, terminalDispositions := trajectory.TerminalToolProposalIDs(request.Trajectory)
	elapsed := continuation.ElapsedNotes(request.Trajectory.Items)
	selectedMedia := continuation.LatestMediaHandles(request.Trajectory.Items)
	for _, run := range continuation.ProviderRuns(request.Trajectory.Items) {
		if run.UserObservations {
			message := compileUserObservationRun(
				run, request.Media, request.Descriptor.Vision, selectedMedia, elapsed,
			)
			result.Messages = append(result.Messages, message)
			continue
		}
		item := run.Items[0]
		if item.Kind == trajectory.KindInstruction || item.Kind == trajectory.KindAssistantState {
			continue
		}
		if item.Kind == trajectory.KindAssistant && assistantVisibility[item.ID] == trajectory.VisibilityCancelled {
			continue
		}
		if item.Kind == trajectory.KindToolProposal {
			if _, promoted := promotedProposals[item.ID]; promoted {
				// The authority chain records promotion as a separate canonical
				// tool_call so that proposal and execution authority remain
				// auditable. A chat provider must nevertheless see one assistant
				// call, not both the non-executable working state and its exact
				// executable promotion. Replaying both teaches some chat templates
				// to echo tagged tool syntax as ordinary assistant speech.
				continue
			}
			if _, terminal := terminalProposals[item.ID]; terminal {
				// The terminal fact belongs at the later disposition's ordered
				// position, not where this still-pending proposal was recorded.
				continue
			}
			// Proposal-only provider state is deliberately not retained by the
			// continuation runner. Keep this check before native-state replay as
			// defense in depth for caller-supplied snapshots. Pending control
			// intent is runtime context, never an assistant turn, but remains
			// composable and therefore retains its exact name and arguments.
			if content, valid := continuation.PendingToolProposalContent(item.ToolCall); valid {
				result.Messages = append(result.Messages, chatMessage{Role: "user", Content: content})
			}
			continue
		}
		if item.Kind == trajectory.KindToolProposalDisposition {
			if _, valid := terminalDispositions[item.ID]; valid {
				result.Messages = append(result.Messages, chatMessage{
					Role: "user", Content: continuation.TerminalToolProposalNotice,
				})
			}
			continue
		}
		modelItem := isModelOutputItem(item.Kind)
		if message, native := nativeInvocations[item.InvocationID]; modelItem && native && item.InvocationID != "" {
			if _, consumed := consumedInvocations[item.InvocationID]; !consumed {
				if continuation.ProducedSilently(item) {
					// Retained state keeps this provider's own shape, tool
					// calls included, so the hint precedes it rather than
					// rewriting it.
					result.Messages = append(result.Messages, chatMessage{
						Role: "system", Content: continuation.BackgroundResultHint,
					})
				}
				result.Messages = append(result.Messages, message)
				consumedInvocations[item.InvocationID] = struct{}{}
			}
			continue
		}
		if _, consumed := consumedInvocations[item.InvocationID]; modelItem && consumed && item.InvocationID != "" {
			continue
		}
		message, ok, err := compilePortableItem(
			item, request.Media, request.Descriptor.Vision, selectedMedia, elapsed,
		)
		if err != nil {
			return chatRequest{}, err
		}
		if ok {
			result.Messages = append(result.Messages, message)
		}
	}
	if len(trajectory.PendingRepairs(request.Trajectory)) > 0 {
		result.Messages = append(result.Messages, chatMessage{Role: "user", Content: continuation.PendingRepairPrompt})
	}
	if len(result.Messages) == 1 && result.Messages[0].Role == "system" {
		// A turn can begin before anything has been committed - an interjection
		// or a silent act fires on an utterance still in progress - and what
		// caused it is then in the instruction rather than in the log. A system
		// instruction is not an empty request; what is missing is the user turn
		// the API wants.
		if strings.TrimSpace(request.Invocation.Instruction) == "" {
			return chatRequest{}, errors.New("OpenAI-compatible continuation requires an observation, a prior model item, or an instruction")
		}
		result.Messages = append(result.Messages, chatMessage{Role: "user", Content: "Go ahead."})
	}
	if len(request.Invocation.Tools) > 0 {
		result.ToolChoice = "auto"
		for _, tool := range request.Invocation.Tools {
			result.Tools = append(result.Tools, chatTool{Type: "function", Function: chatFunction{
				Name: tool.Name, Description: tool.Description, Parameters: slices.Clone(tool.Parameters),
			}})
		}
	}
	adapter.dump(result)
	return result, nil
}

func compileUserObservationRun(
	run continuation.ProviderRun, media continuation.MediaResolver, vision bool,
	selectedMedia map[string]struct{}, elapsed map[string]string,
) chatMessage {
	message := chatMessage{Role: "user"}
	type observationParts struct {
		text  string
		media []contentPart
	}
	compiled := make([]observationParts, 0, len(run.Items))
	withMedia := false
	for _, item := range run.Items {
		parts := observationParts{
			text:  continuation.ObservationContent(item, elapsed[item.ID]),
			media: attachMedia(item, media, vision, selectedMedia),
		}
		compiled = append(compiled, parts)
		if len(parts.media) > 0 {
			withMedia = true
		}
	}
	if !withMedia {
		texts := make([]string, 0, len(compiled))
		for _, parts := range compiled {
			texts = append(texts, parts.text)
		}
		message.Content = strings.Join(texts, "\n")
		return message
	}
	for _, parts := range compiled {
		message.Parts = append(message.Parts, contentPart{
			Type: "text", Text: parts.text,
		})
		message.Parts = append(message.Parts, parts.media...)
	}
	return message
}

func isModelOutputItem(kind trajectory.Kind) bool {
	return kind == trajectory.KindReasoning || kind == trajectory.KindAssistant ||
		kind == trajectory.KindToolProposal || kind == trajectory.KindToolCall
}

func compilePortableItem(
	item trajectory.Item, media continuation.MediaResolver, vision bool, selectedMedia map[string]struct{},
	elapsed map[string]string,
) (chatMessage, bool, error) {
	switch item.Kind {
	case trajectory.KindObservation:
		message := chatMessage{Role: "user", Content: continuation.ObservationContent(item, elapsed[item.ID])}
		// An observation may carry images an observer retained. A model that
		// can see should see them while they exist: reasoning about a screen
		// and clicking on one are different tasks.
		if parts := attachMedia(item, media, vision, selectedMedia); len(parts) > 0 {
			message.Parts = append([]contentPart{{Type: "text", Text: message.Content}}, parts...)
			message.Content = ""
		}
		return message, true, nil
	case trajectory.KindReasoning:
		if !continuation.CarriesInternalState(item.Content) {
			return chatMessage{}, false, nil
		}
		return chatMessage{Role: "assistant", Content: continuation.InternalStatePreamble + item.Content}, true, nil
	case trajectory.KindAssistant:
		if continuation.ProducedSilently(item) {
			if !continuation.HasBackgroundContent(item.Content) {
				// A wait is not a result, and the hint below would announce it
				// as one.
				return chatMessage{}, false, nil
			}
			// A provider that cannot be heard does not take turns. Its result
			// is state the next spoken turn reads, and this dialect has a role
			// that says exactly that.
			return chatMessage{Role: "system", Content: continuation.BackgroundResultHint + item.Content}, true, nil
		}
		return chatMessage{Role: "assistant", Content: item.Content}, true, nil
	case trajectory.KindToolProposal:
		content, valid := continuation.PendingToolProposalContent(item.ToolCall)
		return chatMessage{Role: "user", Content: content}, valid, nil
	case trajectory.KindToolProposalDisposition:
		// A disposition is meaningful only with validated ordered-prefix
		// evidence. buildRequest handles that context before reaching here.
		return chatMessage{}, false, nil
	case trajectory.KindToolCall:
		return chatMessage{Role: "assistant", ToolCalls: []chatToolCall{{
			ID: item.ToolCall.CallID, Type: "function", Function: chatFunction{
				Name: item.ToolCall.Name, Arguments: string(item.ToolCall.Arguments),
			},
		}}}, true, nil
	case trajectory.KindToolResult:
		content := string(item.ToolResult.Output)
		if item.ToolResult.Error != "" {
			encoded, err := json.Marshal(map[string]string{"error": item.ToolResult.Error})
			if err != nil {
				return chatMessage{}, false, err
			}
			content = string(encoded)
		}
		return chatMessage{Role: "tool", ToolCallID: item.ToolResult.CallID, Content: content}, true, nil
	case trajectory.KindToolPlaceholder:
		if item.ToolPlaceholder == nil {
			return chatMessage{}, false, nil
		}
		content, err := json.Marshal(map[string]any{
			"status": "interrupted", "executed": false, "reason": item.ToolPlaceholder.Reason,
		})
		if err != nil {
			return chatMessage{}, false, err
		}
		return chatMessage{
			Role: "tool", ToolCallID: item.ToolPlaceholder.CallID, Content: string(content),
		}, true, nil
	default:
		return chatMessage{}, false, nil
	}
}
