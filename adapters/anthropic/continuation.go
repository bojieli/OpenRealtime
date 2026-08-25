// Package anthropic implements canonical-trajectory continuation through the
// Anthropic Messages API.
//
// Anthropic is the one major provider whose language API is not a dialect of
// Chat Completions, so it gets an adapter rather than a profile. Three of its
// differences are load-bearing here and are handled explicitly rather than
// discovered as a 400 in production:
//
//   - A turn must end with a user message. Assistant prefill is rejected on
//     current models, so a continuation whose trajectory ends in model output
//     has to be given something to answer.
//   - Every tool_use block must be answered by a tool_result in the very next
//     user message. A trajectory can hold a call whose result never arrived;
//     the protocol cannot, so such a call is retold as text.
//   - Thinking blocks carry a signature that must be replayed byte for byte.
//     That is what the retained provider state is for.
//
// The same wire format is served by Anthropic-compatible endpoints, so the
// base URL is configurable and the adapter does not assume it is talking to
// Anthropic itself.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	// DefaultBaseURL is the Anthropic API root.
	DefaultBaseURL = "https://api.anthropic.com/v1"
	// DefaultVersion is the API version header value. It is a date rather
	// than a semantic version and has been stable across every model release
	// this adapter targets.
	DefaultVersion = "2023-06-01"
	// ProviderStateType identifies a retained assistant content array,
	// including thinking blocks and their signatures, wrapped with the model
	// that produced it.
	ProviderStateType = "anthropic.messages.assistant-content.v1"

	defaultMaxTokens = 1_024
	maxErrorBody     = 64 << 10
	maxSSEEvent      = 16 << 20
)

// Thinking selects how extended thinking is requested.
//
// The right value depends on the model generation, not on the deployment's
// preference, and getting it wrong is a hard error rather than a degraded
// answer: current models reject a token budget, and models that predate
// adaptive thinking reject the adaptive form. So it is declared.
type Thinking string

const (
	// ThinkingAdaptive lets the model decide how much to think. It is the
	// mode for Claude 4.6 and newer.
	ThinkingAdaptive Thinking = "adaptive"
	// ThinkingDisabled explicitly turns thinking off, which is what a voice
	// provider wants: the fast phase answers, it does not deliberate.
	ThinkingDisabled Thinking = "disabled"
	// ThinkingBudget requests a fixed token budget, which is how models older
	// than Claude 4.6 spell extended thinking.
	ThinkingBudget Thinking = "budget"
	// ThinkingOmitted sends no thinking field at all.
	ThinkingOmitted Thinking = "omitted"
)

// Config configures one Anthropic fast or slow continuation profile.
type Config struct {
	APIKey  string
	Model   string
	BaseURL string
	// Version is the anthropic-version header. Empty selects DefaultVersion.
	Version string
	// Provider names the serving stack in descriptors. Empty selects
	// "anthropic". A compatible gateway should name itself.
	Provider      string
	Phase         trajectory.Phase
	Effort        continuation.Effort
	ToolAuthority continuation.ToolAuthority
	// SpeechAuthority declares whether this provider's output may be voiced.
	SpeechAuthority continuation.SpeechAuthority
	// AllowTools is a compatibility alias for ToolAuthorityExecute.
	AllowTools bool
	// Vision declares that images may be sent. Every current Claude model
	// accepts them, so this defaults to true and exists for a gateway that
	// serves something that does not.
	Vision *bool
	// Thinking selects the extended-thinking form. Empty selects adaptive.
	Thinking Thinking
	// ThinkingBudgetTokens is the budget for ThinkingBudget. It must be
	// smaller than the output limit and at least 1024.
	ThinkingBudgetTokens int
	// IncludeThoughts asks for a readable summary of the reasoning. Without
	// it, current models stream thinking blocks with empty text, which is not
	// a bug but does mean the trajectory records no reasoning.
	IncludeThoughts bool
	// EffortNames maps portable effort onto Anthropic's effort vocabulary. A
	// nil map sends no effort at all, which is correct for models that predate
	// the parameter. An effort with no entry is refused at construction.
	EffortNames map[continuation.Effort]string
	// Betas are anthropic-beta header values.
	Betas []string
	// Headers are extra request headers.
	Headers        map[string]string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

// sendsEffort reports whether a thinking mode leaves room for an effort level.
//
// Effort describes how hard to think, so pairing it with thinking turned off
// is a contradiction rather than a refinement - and current Claude models
// reject the pair outright at the higher levels.
func sendsEffort(thinking Thinking) bool {
	return thinking == ThinkingAdaptive || thinking == ThinkingBudget
}

// Adapter streams Messages API output and preserves signed thinking blocks so
// a later continuation can replay them.
type Adapter struct {
	config     Config
	descriptor continuation.Descriptor
}

// New validates configuration and creates an adapter.
func New(config Config) (*Adapter, error) {
	config.APIKey = strings.TrimSpace(config.APIKey)
	config.Model = strings.TrimSpace(config.Model)
	if config.Model == "" {
		return nil, errors.New("Anthropic model is required")
	}
	if config.BaseURL == "" {
		config.BaseURL = DefaultBaseURL
	}
	parsed, err := url.Parse(config.BaseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("Anthropic base URL must be absolute")
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	if config.Version == "" {
		config.Version = DefaultVersion
	}
	if config.Provider == "" {
		config.Provider = "anthropic"
	}
	config.Provider = strings.TrimSpace(config.Provider)
	if config.Provider == "" {
		return nil, errors.New("Anthropic provider name is required")
	}
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
	if config.Thinking == "" {
		config.Thinking = ThinkingAdaptive
	}
	switch config.Thinking {
	case ThinkingAdaptive, ThinkingDisabled, ThinkingOmitted:
	case ThinkingBudget:
		if config.ThinkingBudgetTokens < 1_024 {
			return nil, errors.New("Anthropic thinking budget must be at least 1024 tokens")
		}
	default:
		return nil, fmt.Errorf("unsupported Anthropic thinking mode %q", config.Thinking)
	}
	config.EffortNames = maps.Clone(config.EffortNames)
	// Effort is only checked when it will actually be sent. A voice provider
	// runs with thinking off and a descriptor that says minimal effort, and
	// refusing that would make the fast phase unreachable on a provider whose
	// effort vocabulary simply has no word for "none".
	if config.EffortNames != nil && sendsEffort(config.Thinking) {
		if _, named := config.EffortNames[config.Effort]; !named {
			accepted := make([]string, 0, len(config.EffortNames))
			for effort := range config.EffortNames {
				accepted = append(accepted, string(effort))
			}
			sort.Strings(accepted)
			return nil, fmt.Errorf("Anthropic profile has no name for %q reasoning effort; it accepts %s",
				config.Effort, strings.Join(accepted, ", "))
		}
	}
	config.Betas = slices.Clone(config.Betas)
	config.Headers = maps.Clone(config.Headers)
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{}
	}
	if config.RequestTimeout < 0 {
		return nil, errors.New("Anthropic request timeout cannot be negative")
	}
	vision := true
	if config.Vision != nil {
		vision = *config.Vision
	}
	descriptor := continuation.Descriptor{
		Provider: config.Provider, Model: config.Model, Phase: config.Phase,
		Effort: config.Effort, Streaming: true, NativeStateType: ProviderStateType,
		RetainsToolCalls: true, ToolAuthority: config.ToolAuthority,
		SpeechAuthority: config.SpeechAuthority, Vision: vision,
		ExecutableTools: config.ToolAuthority == continuation.ToolAuthorityExecute,
	}
	if err := continuation.ValidateDescriptor(descriptor); err != nil {
		return nil, err
	}
	return &Adapter{config: config, descriptor: descriptor}, nil
}

// Descriptor returns the immutable provider configuration.
func (adapter *Adapter) Descriptor() continuation.Descriptor { return adapter.descriptor }

type messagesRequest struct {
	Model        string            `json:"model"`
	MaxTokens    int               `json:"max_tokens"`
	Stream       bool              `json:"stream"`
	System       []systemBlock     `json:"system,omitempty"`
	Messages     []message         `json:"messages"`
	Tools        []toolDefinition  `json:"tools,omitempty"`
	Thinking     json.RawMessage   `json:"thinking,omitempty"`
	OutputConfig *outputConfig     `json:"output_config,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type outputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type systemBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type message struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type toolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Continue implements continuation.Provider over the streaming Messages API.
func (adapter *Adapter) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	if request.Descriptor != adapter.descriptor {
		return continuation.Completion{}, errors.New("Anthropic request descriptor does not match adapter")
	}
	body, err := adapter.buildRequest(request)
	if err != nil {
		return continuation.Completion{}, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("encode Anthropic request: %w", err)
	}
	if adapter.config.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, adapter.config.RequestTimeout)
		defer cancel()
	}
	continuation.TraceRequest(adapter.Descriptor(), request.InvocationID, encoded)
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, adapter.config.BaseURL+"/messages", bytes.NewReader(encoded))
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("create Anthropic request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("anthropic-version", adapter.config.Version)
	if adapter.config.APIKey != "" {
		httpRequest.Header.Set("x-api-key", adapter.config.APIKey)
	}
	if len(adapter.config.Betas) > 0 {
		httpRequest.Header.Set("anthropic-beta", strings.Join(adapter.config.Betas, ","))
	}
	for name, value := range adapter.config.Headers {
		httpRequest.Header.Set(name, value)
	}
	response, err := adapter.config.HTTPClient.Do(httpRequest)
	if err != nil {
		return continuation.Completion{}, fmt.Errorf("send Anthropic request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
		return continuation.Completion{}, fmt.Errorf(
			"Anthropic returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return adapter.consumeStream(response.Body, request, emit)
}
