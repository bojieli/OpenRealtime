// Package gemini implements canonical-trajectory continuation through the
// Gemini GenerateContent streaming API.
package gemini

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
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/internal/sse"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	// DefaultModel is the stable Gemini model selected for the fast/slow study.
	DefaultModel = "gemini-3.5-flash"
	// ProviderStateType identifies a Gemini model Content object with preserved
	// thought signatures and function calls.
	ProviderStateType = "google.gemini.generate-content.model-content.v1"
	defaultEndpoint   = "https://generativelanguage.googleapis.com/v1beta"
	defaultMaxTokens  = 1_024
	maxErrorBody      = 64 << 10
	maxSSEEvent       = 16 << 20
)

// Config configures one Gemini fast or slow continuation profile.
type Config struct {
	APIKey        string
	Model         string
	Endpoint      string
	Phase         trajectory.Phase
	Effort        continuation.Effort
	ToolAuthority continuation.ToolAuthority
	// SpeechAuthority declares whether this provider's output may be voiced.
	// An unset value means voice; the fast/slow arrangement is what sets a
	// provider silent, and it does so explicitly.
	SpeechAuthority continuation.SpeechAuthority
	// AllowTools is a compatibility alias for ToolAuthorityExecute.
	AllowTools      bool
	IncludeThoughts bool
	HTTPClient      *http.Client
	RequestTimeout  time.Duration
}

// Adapter streams Gemini output and preserves provider-authenticated thought
// signatures without requiring plaintext chain-of-thought retention.
type Adapter struct {
	config     Config
	descriptor continuation.Descriptor
}

// New creates a Gemini continuation adapter.
func New(config Config) (*Adapter, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	if config.APIKey == "" {
		return nil, errors.New("Gemini API key is required")
	}
	if config.Model == "" {
		config.Model = DefaultModel
	}
	if config.Endpoint == "" {
		config.Endpoint = defaultEndpoint
	}
	parsed, err := url.Parse(config.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("Gemini endpoint must be an absolute URL")
	}
	config.Endpoint = strings.TrimRight(config.Endpoint, "/")
	if config.Phase == "" {
		config.Phase = trajectory.PhaseSlow
	}
	if config.Effort == "" {
		config.Effort = continuation.EffortMedium
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
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("Gemini request timeout cannot be negative")
	}
	descriptor := continuation.Descriptor{
		Provider: "google", Model: config.Model, Phase: config.Phase,
		Effort: config.Effort, Streaming: true, NativeStateType: ProviderStateType,
		RetainsToolCalls: true, ToolAuthority: config.ToolAuthority,
		SpeechAuthority: config.SpeechAuthority,
		// Every Gemini model this adapter targets is multimodal, so the
		// capability is a property of the provider rather than something a
		// deployment has to remember to declare.
		Vision:          true,
		ExecutableTools: config.ToolAuthority == continuation.ToolAuthorityExecute,
	}
	if err := continuation.ValidateDescriptor(descriptor); err != nil {
		return nil, err
	}
	return &Adapter{config: config, descriptor: descriptor}, nil
}

// Descriptor returns the immutable provider configuration.
func (adapter *Adapter) Descriptor() continuation.Descriptor { return adapter.descriptor }

type geminiContent struct {
	Role  string            `json:"role,omitempty"`
	Parts []json.RawMessage `json:"parts"`
}

type geminiRequest struct {
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	Contents          []geminiContent        `json:"contents"`
	GenerationConfig  geminiGenerationConfig `json:"generationConfig"`
	Tools             []geminiTool           `json:"tools,omitempty"`
}

type geminiGenerationConfig struct {
	ThinkingConfig  geminiThinkingConfig `json:"thinkingConfig"`
	MaxOutputTokens int                  `json:"maxOutputTokens"`
}

type geminiThinkingConfig struct {
	ThinkingLevel   string `json:"thinkingLevel"`
	IncludeThoughts bool   `json:"includeThoughts"`
}

type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

type geminiFunctionDeclaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
}

type geminiResponse struct {
	Candidates []struct {
		Content      geminiContent `json:"content"`
		FinishReason string        `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount        int64  `json:"promptTokenCount"`
		CachedContentTokenCount *int64 `json:"cachedContentTokenCount"`
		CandidatesTokenCount    int64  `json:"candidatesTokenCount"`
		ThoughtsTokenCount      int64  `json:"thoughtsTokenCount"`
		TotalTokenCount         int64  `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
	} `json:"error,omitempty"`
}

type geminiPart struct {
	Text         string `json:"text,omitempty"`
	Thought      bool   `json:"thought,omitempty"`
	FunctionCall *struct {
		ID   string          `json:"id,omitempty"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall,omitempty"`
}

// Continue implements continuation.Provider using SSE GenerateContent.
func (adapter *Adapter) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	if request.Descriptor != adapter.descriptor {
		return continuation.Completion{}, errors.New("Gemini request descriptor does not match adapter")
	}
	body, err := adapter.buildRequest(request)
	if err != nil {
		return continuation.Completion{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("encode Gemini request: %w", err)
	}
	if adapter.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, adapter.config.RequestTimeout)
		defer cancel()
	}
	continuation.TraceRequest(adapter.Descriptor(), request.InvocationID, encoded)
	endpoint := adapter.config.Endpoint + "/models/" + url.PathEscape(adapter.config.Model) + ":streamGenerateContent?alt=sse"
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("create Gemini request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("x-goog-api-key", adapter.config.APIKey)
	response, err := adapter.config.HTTPClient.Do(httpRequest)
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("send Gemini request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return continuation.Completion{}, fmt.Errorf("Gemini returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}

	completion := continuation.Completion{ProviderStateType: ProviderStateType}
	state := geminiContent{Role: "model"}
	callIndex := 0
	sawToolCall := false
	err = sse.Read(response.Body, maxSSEEvent, func(data []byte) error {
		if bytes.Equal(data, []byte("[DONE]")) {
			return nil
		}
		var envelope geminiResponse
		if err := json.Unmarshal(data, &envelope); err != nil {
			return fmt.Errorf("decode Gemini stream event: %w", err)
		}
		if envelope.Error != nil {
			return fmt.Errorf("Gemini %s (%d): %s", envelope.Error.Status, envelope.Error.Code, envelope.Error.Message)
		}
		completion.Usage = continuation.Usage{
			InputTokens:     envelope.UsageMetadata.PromptTokenCount,
			OutputTokens:    envelope.UsageMetadata.CandidatesTokenCount,
			ReasoningTokens: envelope.UsageMetadata.ThoughtsTokenCount,
			TotalTokens:     envelope.UsageMetadata.TotalTokenCount,
		}
		if envelope.UsageMetadata.CachedContentTokenCount != nil {
			completion.Usage.CachedInputTokens = *envelope.UsageMetadata.CachedContentTokenCount
			completion.Usage.CachedInputTokensReported = true
		}
		for _, candidate := range envelope.Candidates {
			if candidate.FinishReason != "" {
				completion.StopReason = candidate.FinishReason
			}
			for _, rawPart := range candidate.Content.Parts {
				state.Parts = append(state.Parts, append(json.RawMessage(nil), rawPart...))
				var part geminiPart
				if err := json.Unmarshal(rawPart, &part); err != nil {
					return fmt.Errorf("decode Gemini content part: %w", err)
				}
				if part.Text != "" {
					kind := continuation.EventAssistantDelta
					if part.Thought {
						kind = continuation.EventReasoningDelta
					}
					if err := emit(continuation.Event{Kind: kind, Text: part.Text}); err != nil {
						return err
					}
				}
				if part.FunctionCall != nil {
					sawToolCall = true
					callID := strings.TrimSpace(part.FunctionCall.ID)
					if callID == "" {
						callIndex++
						callID = fmt.Sprintf("%s-call-%d", request.InvocationID, callIndex)
					}
					arguments := part.FunctionCall.Args
					if len(arguments) == 0 {
						arguments = json.RawMessage(`{}`)
					}
					if err := emit(continuation.Event{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
						CallID: callID, Name: part.FunctionCall.Name, Arguments: arguments,
					}}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	})
	if len(state.Parts) > 0 && !(err != nil && sawToolCall) {
		completion.ProviderState, _ = json.Marshal(state)
	} else {
		completion.ProviderStateType = ""
	}
	if err != nil {
		return completion, err
	}
	return completion, nil
}

func (adapter *Adapter) buildRequest(request continuation.Request) (geminiRequest, error) {
	maxTokens := request.Invocation.MaxOutputTokens
	if maxTokens == 0 {
		maxTokens = defaultMaxTokens
	}
	result := geminiRequest{
		GenerationConfig: geminiGenerationConfig{
			ThinkingConfig: geminiThinkingConfig{
				ThinkingLevel:   strings.ToUpper(string(adapter.descriptor.Effort)),
				IncludeThoughts: adapter.config.IncludeThoughts,
			},
			MaxOutputTokens: maxTokens,
		},
	}
	assistantVisibility := trajectory.AssistantVisibility(request.Trajectory)
	cancelledInvocations := trajectory.CancelledAssistantInvocations(request.Trajectory)
	nativeInvocations := make(map[string]geminiContent)
	for _, item := range request.Trajectory.Items {
		if _, cancelled := cancelledInvocations[item.InvocationID]; cancelled {
			continue
		}
		if item.ProviderStateType == ProviderStateType && len(item.ProviderState) > 0 &&
			item.Producer.Provider == adapter.descriptor.Provider && item.Producer.Model == adapter.descriptor.Model {
			if _, duplicate := nativeInvocations[item.InvocationID]; duplicate {
				continue
			}
			var content geminiContent
			if err := json.Unmarshal(item.ProviderState, &content); err != nil || content.Role == "" || len(content.Parts) == 0 {
				return geminiRequest{}, fmt.Errorf("decode retained Gemini state on item %s", item.ID)
			}
			// Retained state is what this provider itself emitted, so it
			// arrives already shaped as a model turn. An answer that was never
			// spoken is not one, whichever path renders it.
			nativeInvocations[item.InvocationID] = content
		}
	}
	var systemInstructions []string
	systemInstructions = append(systemInstructions, request.Invocation.Instruction)
	if len(trajectory.PendingRepairs(request.Trajectory)) > 0 {
		systemInstructions = append(systemInstructions, continuation.PendingRepairInstruction)
	}
	if len(request.Invocation.Capabilities) > 0 {
		encoded, err := json.Marshal(request.Invocation.Capabilities)
		if err != nil {
			return geminiRequest{}, fmt.Errorf("encode capability manifest: %w", err)
		}
		systemInstructions = append(systemInstructions, "The following is the complete set of capabilities this agent has. You cannot execute any of them yourself, and you are not given them as tools; the reasoning half executes, and may revise or reject what you propose. Never tell the user the agent lacks a capability that is listed here:\n"+string(encoded))
	}
	if len(systemInstructions) > 0 {
		part, _ := json.Marshal(map[string]string{"text": strings.Join(systemInstructions, "\n\n")})
		result.SystemInstruction = &geminiContent{Parts: []json.RawMessage{part}}
	}

	consumedInvocations := make(map[string]struct{})
	elapsed := continuation.ElapsedNotes(request.Trajectory.Items)
	var lastSemanticKind trajectory.Kind
	for _, item := range request.Trajectory.Items {
		if item.Kind == trajectory.KindInstruction || item.Kind == trajectory.KindAssistantState {
			continue
		}
		if item.Kind == trajectory.KindAssistant && assistantVisibility[item.ID] == trajectory.VisibilityCancelled {
			continue
		}
		lastSemanticKind = item.Kind
		modelItem := isModelOutputItem(item.Kind)
		if content, native := nativeInvocations[item.InvocationID]; modelItem && native && item.InvocationID != "" {
			if _, consumed := consumedInvocations[item.InvocationID]; !consumed {
				if continuation.ProducedSilently(item) {
					// Retained state keeps this provider's own shape, so the
					// hint precedes it rather than rewriting it.
					hint, err := json.Marshal(map[string]string{"text": continuation.BackgroundResultHint})
					if err != nil {
						return geminiRequest{}, fmt.Errorf("encode background result hint: %w", err)
					}
					result.Contents = appendGeminiContent(result.Contents, geminiContent{
						Role: "user", Parts: []json.RawMessage{hint},
					})
				}
				result.Contents = appendGeminiContent(result.Contents, content)
				consumedInvocations[item.InvocationID] = struct{}{}
			}
			continue
		}
		if _, consumed := consumedInvocations[item.InvocationID]; modelItem && consumed && item.InvocationID != "" {
			continue
		}
		content, ok, err := compilePortableItem(item, request.Media, elapsed)
		if err != nil {
			return geminiRequest{}, err
		}
		if ok {
			result.Contents = appendGeminiContent(result.Contents, content)
		}
	}
	if len(trajectory.PendingRepairs(request.Trajectory)) > 0 {
		part, _ := json.Marshal(map[string]string{"text": continuation.PendingRepairPrompt})
		result.Contents = appendGeminiContent(result.Contents, geminiContent{
			Role: "user", Parts: []json.RawMessage{part},
		})
	}
	if adapter.descriptor.Phase == trajectory.PhaseSlow && isModelOutputItem(lastSemanticKind) &&
		len(trajectory.PendingRepairs(request.Trajectory)) == 0 {
		part, _ := json.Marshal(map[string]string{"text": "Please finish the task."})
		result.Contents = appendGeminiContent(result.Contents, geminiContent{
			Role: "user", Parts: []json.RawMessage{part},
		})
	}
	if len(result.Contents) == 0 {
		// A turn can begin before anything has been committed. An interjection
		// or a silent act fires on an utterance still in progress, so the log
		// is genuinely empty and what caused the turn is in the instruction -
		// and this refused every one of them, fifty times in one conversation,
		// with the phone menu never getting its key pressed.
		//
		// A system instruction is not an empty request. What is missing is the
		// user turn this API requires, not the context.
		if strings.TrimSpace(request.Invocation.Instruction) == "" {
			return geminiRequest{}, errors.New("Gemini continuation requires an observation, a prior model item, or an instruction")
		}
		part, _ := json.Marshal(map[string]string{"text": "Go ahead."})
		result.Contents = appendGeminiContent(result.Contents, geminiContent{
			Role: "user", Parts: []json.RawMessage{part},
		})
	}
	if len(request.Invocation.Tools) > 0 {
		declarations := make([]geminiFunctionDeclaration, 0, len(request.Invocation.Tools))
		for _, tool := range request.Invocation.Tools {
			declarations = append(declarations, geminiFunctionDeclaration{
				Name: tool.Name, Description: tool.Description,
				ParametersJSONSchema: append(json.RawMessage(nil), tool.Parameters...),
			})
		}
		result.Tools = []geminiTool{{FunctionDeclarations: declarations}}
	}
	return result, nil
}

func isModelOutputItem(kind trajectory.Kind) bool {
	return kind == trajectory.KindReasoning || kind == trajectory.KindAssistant ||
		kind == trajectory.KindToolProposal || kind == trajectory.KindToolCall
}

func compilePortableItem(
	item trajectory.Item, media continuation.MediaResolver, elapsed map[string]string,
) (geminiContent, bool, error) {
	part := make(map[string]any)
	role := "user"
	var attachments []json.RawMessage
	switch item.Kind {
	case trajectory.KindObservation:
		part["text"] = continuation.ObservationContent(item, elapsed[item.ID])
		// An observation may carry images an observer retained. Narration is
		// what survives after they are pruned, but while they exist a model
		// that can see should see them: reasoning about a screen and clicking
		// on one are different tasks, and only the second needs pixels.
		attachments = attachMedia(item, media)
	case trajectory.KindReasoning:
		role = "model"
		part["text"] = continuation.InternalStatePreamble + item.Content
	case trajectory.KindAssistant:
		if continuation.ProducedSilently(item) {
			// A provider that cannot be heard does not take turns. This
			// dialect has no system role inside the conversation, so the hint
			// rides where every other runtime hint here does, marked for what
			// it is rather than presented as something the agent said.
			part["text"] = continuation.BackgroundResultHint + item.Content
			break
		}
		role = "model"
		part["text"] = item.Content
	case trajectory.KindToolProposal:
		role = "model"
		proposal, err := json.Marshal(map[string]any{
			"non_executable_tool_proposal": map[string]any{
				"name": item.ToolCall.Name, "arguments": json.RawMessage(item.ToolCall.Arguments),
			},
		})
		if err != nil {
			return geminiContent{}, false, err
		}
		part["text"] = string(proposal)
	case trajectory.KindToolCall:
		role = "model"
		var args any
		if err := json.Unmarshal(item.ToolCall.Arguments, &args); err != nil {
			return geminiContent{}, false, fmt.Errorf("decode tool arguments on item %s: %w", item.ID, err)
		}
		part["functionCall"] = map[string]any{"id": item.ToolCall.CallID, "name": item.ToolCall.Name, "args": args}
	case trajectory.KindToolResult:
		var response any
		if item.ToolResult.Error != "" {
			response = map[string]any{"error": item.ToolResult.Error}
		} else {
			var output any
			if err := json.Unmarshal(item.ToolResult.Output, &output); err != nil {
				return geminiContent{}, false, fmt.Errorf("decode tool output on item %s: %w", item.ID, err)
			}
			// GenerateContent's manual function-calling contract uses a stable
			// result envelope even when the underlying output is itself an object.
			response = map[string]any{"result": output}
		}
		part["functionResponse"] = map[string]any{
			"id": item.ToolResult.CallID, "name": item.ToolResult.Name, "response": response,
		}
	default:
		return geminiContent{}, false, nil
	}
	raw, err := json.Marshal(part)
	if err != nil {
		return geminiContent{}, false, err
	}
	return geminiContent{Role: role, Parts: append([]json.RawMessage{raw}, attachments...)}, true, nil
}

// attachMedia resolves an observation's handles into inline parts.
//
// A handle that no longer resolves is skipped rather than failing the
// continuation: retention is bounded on purpose, and a model that gets the
// narration without the image is in exactly the state this design expects
// after the window has passed.
func attachMedia(item trajectory.Item, media continuation.MediaResolver) []json.RawMessage {
	if media == nil || item.Observation == nil || len(item.Observation.Media) == 0 {
		return nil
	}
	var parts []json.RawMessage
	for _, reference := range item.Observation.Media {
		resolved, err := media(reference.Handle)
		if err != nil || len(resolved.Bytes) == 0 {
			continue
		}
		mimeType := resolved.MIMEType
		if mimeType == "" {
			mimeType = reference.MIMEType
		}
		encoded, err := json.Marshal(map[string]any{
			"inlineData": map[string]any{
				"mimeType": mimeType,
				"data":     base64.StdEncoding.EncodeToString(resolved.Bytes),
			},
		})
		if err != nil {
			continue
		}
		parts = append(parts, encoded)
	}
	return parts
}

// appendGeminiContent preserves each part byte-for-byte while coalescing
// adjacent semantic segments from the same role. Fast and slow continuations
// are successive portions of one assistant rollout, not separate model turns;
// GenerateContent expects model and user/function-response turns to alternate.
func appendGeminiContent(contents []geminiContent, content geminiContent) []geminiContent {
	if len(contents) > 0 && contents[len(contents)-1].Role == content.Role {
		contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, content.Parts...)
		return contents
	}
	return append(contents, content)
}
