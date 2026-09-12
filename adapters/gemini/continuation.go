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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/internal/httpclient"
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
	maxErrorBody      = 64 << 10
	maxSSEEvent       = 16 << 20
	maxHTTPAttempts   = 4
	initialRetryDelay = 250 * time.Millisecond
	maximumRetryDelay = 2 * time.Second
	// portableToolCallThoughtSignature is Gemini's documented sentinel for a
	// manually constructed function call. Native Gemini content retains its
	// provider-authenticated signature instead; only portable calls authored
	// by another provider need this compatibility marker.
	portableToolCallThoughtSignature = "skip_thought_signature_validator"
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
	// Temperature is omitted when nil. Zero gives the fast voice a stable
	// control decision without changing the slow reasoner's provider default.
	Temperature    *float64
	HTTPClient     *http.Client
	RequestTimeout time.Duration
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
	// Reject an unreadable effort here rather than sending it and letting the
	// endpoint answer with a 400 mid-conversation.
	if !config.Effort.Named() {
		if _, numeric := config.Effort.Budget(); !numeric {
			return nil, fmt.Errorf("Gemini reasoning effort must be a name or a "+
				"number of thinking tokens, got %q", config.Effort)
		}
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
		config.HTTPClient = httpclient.Shared()
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("Gemini request timeout cannot be negative")
	}
	if config.Temperature != nil && (*config.Temperature < 0 || *config.Temperature > 2) {
		return nil, errors.New("Gemini temperature must be between 0 and 2")
	}
	temperature := ""
	if config.Temperature != nil {
		temperature = strconv.FormatFloat(*config.Temperature, 'g', -1, 64)
	}
	descriptor := continuation.Descriptor{
		Provider: "google", Model: config.Model, Phase: config.Phase,
		Effort: config.Effort, Streaming: true, NativeStateType: ProviderStateType,
		RetainsToolCalls: true, ToolAuthority: config.ToolAuthority,
		SpeechAuthority:     config.SpeechAuthority,
		SamplingTemperature: temperature,
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
	ThinkingConfig geminiThinkingConfig `json:"thinkingConfig"`
	// MaxOutputTokens is omitted unless a deployment asks for a ceiling.
	// Gemini counts thinking against it, so a number chosen to keep a spoken
	// turn short is spent on thought before any speech is produced.
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
}

// geminiThinkingConfig carries a level or a budget. The API rejects both at
// once, so at most one field is ever populated.
type geminiThinkingConfig struct {
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
	ThinkingBudget  *int   `json:"thinkingBudget,omitempty"`
	IncludeThoughts bool   `json:"includeThoughts"`
}

// thinkingFor renders an effort into the one field that expresses it.
func thinkingFor(effort continuation.Effort, thoughts bool) geminiThinkingConfig {
	config := geminiThinkingConfig{IncludeThoughts: thoughts}
	if budget, numeric := effort.Budget(); numeric {
		config.ThinkingBudget = &budget
		return config
	}
	config.ThinkingLevel = strings.ToUpper(string(effort))
	return config
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
	adapter.dump(encoded)
	if adapter.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, adapter.config.RequestTimeout)
		defer cancel()
	}
	continuation.TraceRequest(adapter.Descriptor(), request.InvocationID, encoded)
	endpoint := adapter.config.Endpoint + "/models/" + url.PathEscape(adapter.config.Model) + ":streamGenerateContent?alt=sse"
	response, err := adapter.openStream(ctx, endpoint, encoded)
	if err != nil {
		return continuation.Completion{}, err
	}
	defer response.Body.Close()

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
	if completion.StopReason == "MAX_TOKENS" {
		return completion, fmt.Errorf("Gemini exhausted its output token limit before completing the response (thinking and spoken output share this limit)")
	}
	return completion, nil
}

// openStream retries only a retryable HTTP rejection received before a Gemini
// stream begins. Once the provider accepts a request, Continue owns that one
// stream and never replays it: retrying after SSE output could duplicate text,
// tool calls, or billing while hiding an ambiguous provider outcome.
func (adapter *Adapter) openStream(ctx context.Context, endpoint string, encoded []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= maxHTTPAttempts; attempt++ {
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
		if err != nil {
			return nil, fmt.Errorf("create Gemini request: %w", err)
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Accept", "text/event-stream")
		httpRequest.Header.Set("x-goog-api-key", adapter.config.APIKey)
		response, err := adapter.config.HTTPClient.Do(httpRequest)
		if err != nil {
			return nil, fmt.Errorf("send Gemini request: %w", err)
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return response, nil
		}
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		_ = response.Body.Close()
		lastErr = fmt.Errorf("Gemini returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
		if attempt == maxHTTPAttempts || !retryableHTTPStatus(response.StatusCode) {
			return nil, lastErr
		}
		delay := retryDelay(response.Header.Get("Retry-After"), attempt, time.Now())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, fmt.Errorf("retry Gemini request after %w: %v", ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func retryableHTTPStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func retryDelay(header string, attempt int, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, maximumRetryDelay)
	}
	if deadline, err := http.ParseTime(strings.TrimSpace(header)); err == nil {
		return min(max(deadline.Sub(now), 0), maximumRetryDelay)
	}
	delay := initialRetryDelay << (attempt - 1)
	return min(delay, maximumRetryDelay)
}

// dumpRequests names a file to append every outgoing request to.
//
// Reasoning about which part of a prompt differs from one tried by hand has
// been wrong repeatedly in this work, and each wrong answer costs a round. The
// body that actually goes out settles it in one. The OpenAI-compatible adapter
// has had this for a while; the native one did not, so moving the voice onto
// it silently gave up the diagnostic that found most of these defects.
const dumpRequests = "OPENREALTIME_DUMP_REQUESTS"

func (adapter *Adapter) dump(body []byte) {
	path := strings.TrimSpace(os.Getenv(dumpRequests))
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(append([]byte(nil), body...), '\n'))
}

func (adapter *Adapter) buildRequest(request continuation.Request) (geminiRequest, error) {
	// A turn may ask for less thinking than the deployment configured,
	// because how far to think depends on the act rather than on the setting.
	// Before this the descriptor's effort was rendered unconditionally and the
	// per-turn request was silently discarded.
	effort := adapter.descriptor.Effort
	if request.Invocation.Effort != "" {
		effort = request.Invocation.Effort
	}
	result := geminiRequest{
		GenerationConfig: geminiGenerationConfig{
			ThinkingConfig:  thinkingFor(effort, adapter.config.IncludeThoughts),
			MaxOutputTokens: request.Invocation.MaxOutputTokens,
			Temperature:     adapter.config.Temperature,
		},
	}
	assistantVisibility := trajectory.AssistantVisibility(request.Trajectory)
	cancelledInvocations := trajectory.CancelledAssistantInvocations(request.Trajectory)
	// Content the user heard only part of is projected down to the part they
	// heard. Retained native state is the whole turn, so replaying it would put
	// the unheard half back.
	partlyHeardInvocations := trajectory.PartlyHeardAssistantInvocations(request.Trajectory)
	nativeInvocations := make(map[string]geminiContent)
	for _, item := range request.Trajectory.Items {
		if _, cancelled := cancelledInvocations[item.InvocationID]; cancelled {
			continue
		}
		if _, partly := partlyHeardInvocations[item.InvocationID]; partly {
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
	// Capability data establishes what exists; the current phase policy governs
	// its use and therefore follows it. An audible-repair obligation is more
	// specific still and remains last.
	var systemInstructions []string
	if len(request.Invocation.Capabilities) > 0 {
		encoded, err := json.Marshal(request.Invocation.Capabilities)
		if err != nil {
			return geminiRequest{}, fmt.Errorf("encode capability manifest: %w", err)
		}
		systemInstructions = append(systemInstructions, "The following is the complete set of capabilities this agent has. Schema visibility is not execution authority: the runtime records whether a tool-channel call is executable or only a non-executable proposal, and reasoning may revise or reject a proposal. Never tell the user the agent lacks a capability that is listed here:\n"+string(encoded))
	}
	systemInstructions = append(systemInstructions, request.Invocation.Instruction)
	if len(trajectory.PendingRepairs(request.Trajectory)) > 0 {
		systemInstructions = append(systemInstructions, continuation.PendingRepairInstruction)
	}
	if len(systemInstructions) > 0 {
		part, _ := json.Marshal(map[string]string{"text": strings.Join(systemInstructions, "\n\n")})
		result.SystemInstruction = &geminiContent{Parts: []json.RawMessage{part}}
	}

	consumedInvocations := make(map[string]struct{})
	promotedProposals := trajectory.PromotedToolProposalIDs(request.Trajectory)
	terminalProposals, terminalDispositions := trajectory.TerminalToolProposalIDs(request.Trajectory)
	elapsed := continuation.ElapsedNotes(request.Trajectory.Items)
	selectedMedia := continuation.LatestMediaHandles(request.Trajectory.Items)
	var lastSemanticKind trajectory.Kind
	for _, run := range continuation.ProviderRuns(request.Trajectory.Items) {
		if run.UserObservations {
			lastSemanticKind = trajectory.KindObservation
			result.Contents = appendGeminiContent(
				result.Contents, compileUserObservationRun(run, request.Media, selectedMedia, elapsed),
			)
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
				continue
			}
			if _, terminal := terminalProposals[item.ID]; terminal {
				continue
			}
			// Never replay proposal-only control state through a retained native
			// model turn. Its exact details stay composable as runtime/user text
			// until a later canonical disposition or promotion resolves it.
			if content, valid := continuation.PendingToolProposalContent(item.ToolCall); valid {
				part, _ := json.Marshal(map[string]string{"text": content})
				result.Contents = appendGeminiContent(result.Contents, geminiContent{
					Role: "user", Parts: []json.RawMessage{part},
				})
			}
			lastSemanticKind = item.Kind
			continue
		}
		if item.Kind == trajectory.KindToolProposalDisposition {
			if _, valid := terminalDispositions[item.ID]; valid {
				part, _ := json.Marshal(map[string]string{
					"text": continuation.TerminalToolProposalNotice,
				})
				result.Contents = appendGeminiContent(result.Contents, geminiContent{
					Role: "user", Parts: []json.RawMessage{part},
				})
			}
			lastSemanticKind = item.Kind
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
		content, ok, err := compilePortableItem(item, request.Media, selectedMedia, elapsed)
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
	if last := len(result.Contents) - 1; last >= 0 && result.Contents[last].Role == "model" {
		// This API refuses a conversation that ends on the model's own turn,
		// and a turn the runtime opens with nothing new said - a silence
		// timer coming due, a standing instruction falling due on the clock -
		// ends exactly there. Measured, every such turn failed in 150 ms with
		// a 400 and the person asking to be checked on after fifteen seconds
		// was never checked on. The missing piece is the user turn the API
		// requires, and what that turn can truthfully say is that nobody has
		// spoken since.
		part, _ := json.Marshal(map[string]string{"text": continuation.NothingSaidSincePrompt})
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

func compileUserObservationRun(
	run continuation.ProviderRun, media continuation.MediaResolver,
	selectedMedia map[string]struct{}, elapsed map[string]string,
) geminiContent {
	content := geminiContent{Role: "user"}
	for _, item := range run.Items {
		part, _ := json.Marshal(map[string]string{
			"text": continuation.ObservationContent(item, elapsed[item.ID]),
		})
		content.Parts = append(content.Parts, part)
		content.Parts = append(content.Parts, attachMedia(item, media, selectedMedia)...)
	}
	return content
}

func isModelOutputItem(kind trajectory.Kind) bool {
	return kind == trajectory.KindReasoning || kind == trajectory.KindAssistant ||
		kind == trajectory.KindToolProposal || kind == trajectory.KindToolCall
}

func compilePortableItem(
	item trajectory.Item, media continuation.MediaResolver, selectedMedia map[string]struct{},
	elapsed map[string]string,
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
		attachments = attachMedia(item, media, selectedMedia)
	case trajectory.KindReasoning:
		if !continuation.CarriesInternalState(item.Content) {
			return geminiContent{}, false, nil
		}
		role = "model"
		part["text"] = continuation.InternalStatePreamble + item.Content
	case trajectory.KindAssistant:
		if continuation.ProducedSilently(item) {
			if !continuation.HasBackgroundContent(item.Content) {
				// A wait is not a result, and the hint below would announce it
				// as one.
				return geminiContent{}, false, nil
			}
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
		content, valid := continuation.PendingToolProposalContent(item.ToolCall)
		if !valid {
			return geminiContent{}, false, nil
		}
		part["text"] = content
	case trajectory.KindToolProposalDisposition:
		// Only buildRequest has enough ordered-prefix evidence to validate it.
		return geminiContent{}, false, nil
	case trajectory.KindToolCall:
		role = "model"
		part["functionCall"] = map[string]any{
			"id": item.ToolCall.CallID, "name": item.ToolCall.Name,
			"args": json.RawMessage(item.ToolCall.Arguments),
		}
		part["thoughtSignature"] = portableToolCallThoughtSignature
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
	case trajectory.KindToolPlaceholder:
		if item.ToolPlaceholder == nil {
			return geminiContent{}, false, nil
		}
		part["functionResponse"] = map[string]any{
			"id": item.ToolPlaceholder.CallID, "name": item.ToolPlaceholder.Name,
			"response": map[string]any{
				"status": "interrupted", "executed": false, "reason": item.ToolPlaceholder.Reason,
			},
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
func attachMedia(
	item trajectory.Item, media continuation.MediaResolver, selected map[string]struct{},
) []json.RawMessage {
	if media == nil || item.Observation == nil || len(item.Observation.Media) == 0 {
		return nil
	}
	var parts []json.RawMessage
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
