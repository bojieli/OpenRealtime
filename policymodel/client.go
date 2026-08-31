// Package policymodel serves the small, constrained decisions the interaction
// plane makes about a live conversation.
//
// Three constraints define what a policy model is allowed to be, and all three
// are enforced here rather than asked for in a prompt:
//
//  1. Enumerated output, never free generation. The client sends the permitted
//     answers and rejects anything that is not one of them. A policy model
//     cannot become a third cognition provider because it has no way to say
//     anything that is not on the list.
//  2. No tool authority, ever. This client cannot send tool definitions and
//     cannot parse tool calls, so a policy model sits outside the
//     proposal/execute split by construction.
//  3. Decision-time information only. What a decision may see is assembled by
//     the caller and passed as a value; this package has no handle on the
//     trajectory or on perception, so it cannot reach for hindsight.
//
// Sizing is measured rather than assumed. The starting point is a
// state-of-the-art model in the ~3B class; whether that is adequate for these
// judgements is a question the measurement program answers before the choice
// is fixed.
package policymodel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/openaicompat"
	"github.com/bojieli/OpenRealtime/admission"
	"github.com/bojieli/OpenRealtime/interaction"
)

// DefaultBaseURL is the conventional local OpenAI-compatible location.
const DefaultBaseURL = "http://127.0.0.1:8000/v1"

// Config configures the client.
type Config struct {
	BaseURL string
	Model   string
	APIKey  string
	// Timeout bounds one decision. It is short by default: a decision about a
	// live conversation that arrives late is worthless, and waiting for it is
	// worse than falling back to the rule.
	Timeout time.Duration
	// GuidedChoice asks a current vLLM-compatible server to constrain decoding
	// to the enumerated options through its structured_outputs.choice request
	// contract. The client's own validation remains authoritative even when a
	// provider returns malformed output.
	GuidedChoice bool
	// Governor admits decisions against a shared compute budget. A policy
	// model competes for the same GPU as everything else, so it belongs under
	// the same governor rather than beside it.
	Governor *admission.Governor
	// Class is the admission class. It defaults to interactive: above
	// speculative preparation, below the foreground continuation.
	Class      admission.Class
	HTTPClient *http.Client
	// Reasoning is how this endpoint is told not to think.
	//
	// A policy model's contract is one enumerated choice from a short prompt,
	// so reasoning is definitionally not part of it - and a reasoning model
	// asked anyway spends the whole budget on the reasoning. Qwen3 answers
	// "<think>" and stops, having chosen nothing, and constrained decoding
	// does not help because the thinking block precedes the constraint.
	//
	// The switch is spelled differently by every vendor, which is why this is
	// declared rather than assumed. An instruct model needs no switch at all
	// and should be given ReasoningControlNone; a model whose thinking cannot
	// be turned off should not be used here.
	Reasoning openaicompat.ReasoningControl
}

// Client is one policy-model endpoint.
type Client struct {
	config Config
	http   *http.Client

	decisions atomic.Uint64
	refusals  atomic.Uint64
	timeouts  atomic.Uint64
	elapsedNS atomic.Uint64
}

// New validates the configuration.
func New(config Config) (*Client, error) {
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("a policy model requires a model identity")
	}
	if strings.TrimSpace(config.BaseURL) == "" {
		config.BaseURL = DefaultBaseURL
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	if config.Timeout <= 0 {
		config.Timeout = 250 * time.Millisecond
	}
	if config.Class == 0 {
		config.Class = admission.ClassInteractive
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: config.Timeout}
	}
	return &Client{config: config, http: client}, nil
}

// Name identifies the model in reports.
func (client *Client) Name() string { return client.config.Model }

// DecisionTimeout is the live deadline retained in architecture evidence.
func (client *Client) DecisionTimeout() time.Duration { return client.config.Timeout }

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens"`
	Temperature float64       `json:"temperature"`
	Logprobs    bool          `json:"logprobs,omitempty"`
	TopLogprobs int           `json:"top_logprobs,omitempty"`
	// StructuredOutputs is the current vLLM extension for constrained decoding.
	// Older top-level guided_choice fields are deliberately not emitted: current
	// vLLM ignores that unknown field and silently turns a constrained decision
	// into free generation.
	StructuredOutputs *structuredOutputs `json:"structured_outputs,omitempty"`
	// ReasoningEffort, EnableThinking, Thinking and ChatTemplateKwargs are the
	// spellings of one switch. Exactly one is sent, chosen by the profile.
	ReasoningEffort string         `json:"reasoning_effort,omitempty"`
	EnableThinking  *bool          `json:"enable_thinking,omitempty"`
	Thinking        map[string]any `json:"thinking,omitempty"`
	// ChatTemplateKwargs turns a reasoning model's thinking mode off.
	//
	// A policy model's whole contract is one enumerated choice from a short
	// prompt, and a model that reasons first spends its budget on the reasoning
	// - Qwen3 answers "<think>\nOkay" and stops, having chosen nothing. The
	// caller then reads a first token that matches no option, scores it as
	// unknown, and discards a decision the model was never given room to make.
	// Constrained decoding does not save it either: the thinking block is
	// emitted before the constraint applies.
	ChatTemplateKwargs map[string]any `json:"chat_template_kwargs,omitempty"`
}

type structuredOutputs struct {
	Choice []string `json:"choice"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is a string for an ordinary decision and a list of parts when a
	// frame comes with it, which is what the OpenAI-compatible shape requires
	// and what every server that can see accepts.
	Content any `json:"content"`
}

// contentPart is one piece of a multimodal message.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// withImages renders the evidence as text plus frames.
//
// Inline data URIs rather than links, because the frame is in memory at the
// instant the decision is taken and there is nothing to serve it from - and
// because a decision that has to fetch its own evidence is not a decision
// taken in thirty milliseconds.
func withImages(evidence string, images []interaction.Image) []contentPart {
	parts := make([]contentPart, 0, len(images)+1)
	if strings.TrimSpace(evidence) != "" {
		parts = append(parts, contentPart{Type: "text", Text: evidence})
	}
	for _, image := range images {
		if len(image.Bytes) == 0 {
			continue
		}
		mime := image.MIMEType
		if strings.TrimSpace(mime) == "" {
			mime = "image/png"
		}
		parts = append(parts, contentPart{
			Type: "image_url",
			ImageURL: &imageURL{
				URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image.Bytes),
			},
		})
	}
	return parts
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Logprobs *struct {
			Content []struct {
				TopLogprobs []struct {
					Token   string  `json:"token"`
					Logprob float64 `json:"logprob"`
				} `json:"top_logprobs"`
			} `json:"content"`
		} `json:"logprobs,omitempty"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Decide answers one enumerated question.
func (client *Client) Decide(ctx context.Context, decision interaction.Decision) (interaction.Outcome, error) {
	if err := decision.Validate(); err != nil {
		return interaction.Outcome{}, err
	}
	if client.config.Governor != nil {
		lease, err := client.config.Governor.Acquire(ctx, admission.Request{
			Class: client.config.Class, Cost: 1, Preemptible: true,
			Deadline: time.Now().Add(client.config.Timeout), Label: "policy-model",
		})
		if err != nil {
			// A policy decision that cannot get compute is a policy that is
			// off for this turn, not a failure of the conversation.
			return interaction.Outcome{}, fmt.Errorf("policy model not admitted: %w", err)
		}
		defer lease.Release()
	}

	prompt := decision.Prompt + "\n\nAnswer with exactly one of: " +
		strings.Join(decision.Options, ", ") + "\nAnswer with nothing else."
	if strings.TrimSpace(decision.Evidence) != "" {
		prompt += "\n\n" + decision.Evidence
	}
	// The frame travels in the same message as the evidence it is evidence
	// for. Splitting them asks the model to associate a picture with a
	// paragraph by position.
	var content any = prompt
	if len(decision.Images) > 0 {
		content = withImages(prompt, decision.Images)
	}
	body := chatRequest{
		Model: client.config.Model, MaxTokens: 4, Temperature: 0,
		Messages:    []chatMessage{{Role: "user", Content: content}},
		Logprobs:    true,
		TopLogprobs: len(decision.Options),
	}
	switch client.config.Reasoning {
	case openaicompat.ReasoningControlTemplateKwargs:
		body.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	case openaicompat.ReasoningControlEnableThinking:
		disabled := false
		body.EnableThinking = &disabled
	case openaicompat.ReasoningControlEffort:
		body.ReasoningEffort = "none"
	case openaicompat.ReasoningControlThinkingObject:
		body.Thinking = map[string]any{"type": "disabled"}
	}
	if client.config.GuidedChoice {
		// Copy the caller-owned options so that a concurrent caller mutation
		// cannot change the constraint between validation and JSON encoding.
		body.StructuredOutputs = &structuredOutputs{Choice: append([]string(nil), decision.Options...)}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return interaction.Outcome{}, err
	}
	timed, cancel := context.WithTimeout(ctx, client.config.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(
		timed, http.MethodPost, client.config.BaseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return interaction.Outcome{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(client.config.APIKey) != "" {
		request.Header.Set("Authorization", "Bearer "+client.config.APIKey)
	}

	started := time.Now()
	response, err := client.http.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			client.timeouts.Add(1)
		}
		return interaction.Outcome{}, fmt.Errorf("policy decision: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return interaction.Outcome{}, err
	}
	elapsed := time.Since(started)
	client.elapsedNS.Add(uint64(elapsed.Nanoseconds()))
	if response.StatusCode != http.StatusOK {
		return interaction.Outcome{}, fmt.Errorf(
			"policy decision returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(payload)))
	}
	var decoded chatResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return interaction.Outcome{}, fmt.Errorf("decode policy decision: %w", err)
	}
	if decoded.Error != nil {
		return interaction.Outcome{}, errors.New(decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 {
		return interaction.Outcome{}, errors.New("policy decision returned no choices")
	}

	answer := strings.TrimSpace(strings.ToLower(decoded.Choices[0].Message.Content))
	index := match(answer, decision.Options)
	if index < 0 {
		// The model said something that is not on the list. That is a refusal,
		// not an answer to interpret: coercing it to the nearest option is how
		// an enumerated output quietly becomes free generation.
		client.refusals.Add(1)
		return interaction.Outcome{}, fmt.Errorf(
			"policy model answered %q, which is not one of %s", answer, strings.Join(decision.Options, ", "))
	}
	client.decisions.Add(1)
	confidence, measured := confidenceOf(decoded, decision.Options[index])
	return interaction.Outcome{
		Index: index, Option: decision.Options[index],
		Confidence: confidence, Measured: measured,
		ElapsedNS: uint64(elapsed.Nanoseconds()),
	}, nil
}

// match resolves an answer to one of the permitted options.
//
// Exact first, then a prefix, because a small model asked for one word often
// produces the word plus punctuation. Nothing looser: "yes" is not "finished",
// however plausible the mapping might seem.
func match(answer string, options []string) int {
	trimmed := strings.Trim(answer, " \t\n.\"'`")
	for index, option := range options {
		if strings.EqualFold(trimmed, option) {
			return index
		}
	}
	for index, option := range options {
		if strings.HasPrefix(trimmed, strings.ToLower(option)) {
			return index
		}
	}
	return -1
}

// confidenceOf reads the chosen token's probability where the server reported
// log probabilities, and reports a neutral value where it did not. A caller
// that thresholds on confidence should not be silently handed a one.
// confidenceOf reports how sure the model was, and whether that is known.
//
// It used to return 0.5 for "no idea", which reads as a number and compares
// like one: against a 0.7 threshold every unmeasured answer was silently
// treated as an unsure answer and thrown away.
func confidenceOf(response chatResponse, option string) (float64, bool) {
	logprobs := response.Choices[0].Logprobs
	if logprobs == nil || len(logprobs.Content) == 0 {
		return 0, false
	}
	best := 0.0
	for _, candidate := range logprobs.Content[0].TopLogprobs {
		if !strings.HasPrefix(strings.ToLower(option), strings.ToLower(strings.TrimSpace(candidate.Token))) {
			continue
		}
		probability := math.Exp(candidate.Logprob)
		if probability > best {
			best = probability
		}
	}
	if best <= 0 {
		return 0, false
	}
	return min(best, 1), true
}

// Metrics is operational telemetry with no conversation content in it.
type Metrics struct {
	Decisions   uint64  `json:"decisions"`
	Refusals    uint64  `json:"refusals"`
	Timeouts    uint64  `json:"timeouts"`
	MeanLatency float64 `json:"mean_latency_ms"`
}

// Metrics reports how the policy model is behaving. Refusals matter: a model
// that keeps answering off the list is a model that is too small for the job,
// which is exactly what the sizing question needs to be able to see.
func (client *Client) Metrics() Metrics {
	decisions := client.decisions.Load()
	mean := 0.0
	if total := decisions + client.refusals.Load(); total > 0 {
		mean = float64(client.elapsedNS.Load()) / float64(total) / float64(time.Millisecond)
	}
	return Metrics{
		Decisions: decisions, Refusals: client.refusals.Load(),
		Timeouts: client.timeouts.Load(), MeanLatency: mean,
	}
}

var _ interaction.Decider = (*Client)(nil)

// Generate asks for a short free-form answer rather than an enumerated one.
//
// Most decisions here are a choice among options, which is what makes them
// cheap and checkable. One is not: noticing that somebody has set a policy out
// loud has to come back with the policy, and no enumeration can contain it.
//
// It runs off the critical path, so the budget is generous where Decide's is
// four tokens - what it produces is read once per turn rather than five times
// a second.
func (client *Client) Generate(ctx context.Context, prompt, evidence string, maxTokens int) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("generation requires a prompt")
	}
	if maxTokens <= 0 {
		maxTokens = 128
	}
	// The instruction is a system message and the evidence a user one, which
	// is how the same prompt was measured. Folding both into a single user
	// message - which is right for Decide, where the answer is one of a listed
	// few - changed the answers here: every extraction came back scoped to the
	// turn, including policies that plainly govern a conversation, and a
	// turn-scoped policy expires at the next answer. The suite said 90% and
	// the runtime pinned nothing that survived.
	messages := []chatMessage{{Role: "system", Content: prompt}}
	if strings.TrimSpace(evidence) != "" {
		messages = append(messages, chatMessage{Role: "user", Content: evidence})
	} else {
		messages = append(messages, chatMessage{Role: "user", Content: prompt})
		messages = messages[1:]
	}
	body := chatRequest{
		Model: client.config.Model, MaxTokens: maxTokens, Temperature: 0,
		Messages: messages,
	}
	switch client.config.Reasoning {
	case openaicompat.ReasoningControlTemplateKwargs:
		body.ChatTemplateKwargs = map[string]any{"enable_thinking": false}
	case openaicompat.ReasoningControlEnableThinking:
		disabled := false
		body.EnableThinking = &disabled
	case openaicompat.ReasoningControlEffort:
		body.ReasoningEffort = "none"
	case openaicompat.ReasoningControlThinkingObject:
		body.Thinking = map[string]any{"type": "disabled"}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	timed, cancel := context.WithTimeout(ctx, client.config.Timeout*4)
	defer cancel()
	request, err := http.NewRequestWithContext(
		timed, http.MethodPost, client.config.BaseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(client.config.APIKey) != "" {
		request.Header.Set("Authorization", "Bearer "+client.config.APIKey)
	}
	response, err := client.http.Do(request)
	if err != nil {
		return "", fmt.Errorf("policy generation: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("policy generation returned %s", response.Status)
	}
	var decoded chatResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", err
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("policy generation returned no choices")
	}
	return strings.TrimSpace(decoded.Choices[0].Message.Content), nil
}
