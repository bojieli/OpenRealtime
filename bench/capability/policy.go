package capability

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Action is one bounded joint decision: what to do and the next words to say.
type Action struct {
	Act             string `json:"act"`
	Text            string `json:"text"`
	ReplacesPending string `json:"replaces_pending"`
}

// Acts are the joint policy's action vocabulary.
var Acts = []string{"wait", "speak", "continue", "backchannel", "yield", "revise"}

// Validate rejects actions that the grammar should already exclude, so a
// server that ignores the grammar cannot smuggle control text into speech.
func (a Action) Validate() error {
	switch a.Act {
	case "wait", "yield":
		if a.Text != "" || a.ReplacesPending != "" {
			return fmt.Errorf("%s carries text or a replacement", a.Act)
		}
	case "speak", "continue", "backchannel":
		if strings.TrimSpace(a.Text) == "" || a.ReplacesPending != "" {
			return fmt.Errorf("%s needs text and no replacement", a.Act)
		}
	case "revise":
		if strings.TrimSpace(a.Text) == "" || a.ReplacesPending == "" {
			return fmt.Errorf("revise needs text and a pending segment ID")
		}
	default:
		return fmt.Errorf("unknown act %q", a.Act)
	}
	return nil
}

// JointPrompt is the prompted debugging policy's system text. A released
// trained checkpoint uses its own grammar instead.
const JointPrompt = `You are participating in a live voice conversation.
At each update choose one action and a short next spoken continuation.
Return compact JSON in exactly this key order: act, text, replaces_pending.
For silence use {"act":"wait","text":"","replaces_pending":""}.
Wait until there is a user request; do not add an unsolicited greeting.
Actions: wait, speak, continue, backchannel, yield, revise.
wait and yield have empty text. All other actions contain only words to speak.
Use at most one short sentence per update. Do not speak control fields.
Observe new user feedback and fulfill its request in the next unspoken content.
An acknowledgement alone does not fulfill a request to change an explanation.
If there is an unanswered request and no pending speech, speak its substantive
answer now. Once a segment finishes, continue the explanation with the next
substantive sentence until the request is fulfilled. A greeting or promise to
explain is not a completed answer. Do not wait indefinitely for another user
turn when you still owe the answer. When feedback asks to skip or deepen a
topic, the next spoken sentence must carry the requested content.
Generated or pending words have not been heard. Repair heard mistakes explicitly.
While a segment is playing, wait to let it finish unless NEW user evidence
requires different content. Never revise a segment to the same text: that
restarts the audio and prevents the listener hearing it. Read the latest user
words before choosing continuation content. Revision must incorporate the
changed request, not restart an earlier answer.
You may revise a pending segment only by its supplied ID. Otherwise use an empty
replaces_pending. Do not repeat speech that has already been heard.
Execution outcomes distinguish requests from effects: invalid-replacement and
pending-conflict mean the requested text was NOT sent to speech. Do not repeat a
rejected reference. If its segment has completed, its content is immutable;
choose speak/continue for new content, including an explicit repair if needed.
No new input is admitted during this request; the next update is your next chance.`

// actionGrammar constrains decoding to the three action shapes, with JSON
// string escapes, so malformed IDs and runaway whitespace cannot be emitted.
const jsonString = `([^"\\\x00-\x1f]|\\(["\\/bfnrt]|u[0-9a-fA-F]{4}))*`

var actionGrammar = `(\{"act":"(wait|yield)","text":"","replaces_pending":""\}` +
	`|\{"act":"(speak|continue|backchannel)","text":"` + jsonString + `","replaces_pending":""\}` +
	`|\{"act":"revise","text":"` + jsonString + `","replaces_pending":"` + jsonString + `"\})`

// JointPolicy asks an OpenAI-compatible chat endpoint for one joint action.
type JointPolicy struct {
	URL       string
	Model     string
	MaxTokens int
	Seed      int
	// Prompt overrides JointPrompt for an explicitly declared treatment.
	Prompt string
	Client *http.Client
}

// Decision retains the exact request and response alongside the action.
type Decision struct {
	StartedAt time.Time       `json:"started_at"`
	Duration  time.Duration   `json:"duration_ns"`
	Request   json.RawMessage `json:"request"`
	Response  json.RawMessage `json:"response,omitempty"`
	Action    Action          `json:"action"`
	Error     string          `json:"error,omitempty"`
}

// Decide performs one immutable model request. No evidence is admitted while
// it runs; the caller's next tick is the next chance to observe.
func (p JointPolicy) Decide(ctx context.Context, instructions, observation string) (Decision, error) {
	decision := Decision{StartedAt: time.Now().UTC()}
	fail := func(err error) (Decision, error) {
		decision.Duration = time.Since(decision.StartedAt)
		decision.Error = err.Error()
		return decision, err
	}
	if p.MaxTokens <= 0 {
		return fail(fmt.Errorf("positive token budget required"))
	}
	prompt := p.Prompt
	if prompt == "" {
		prompt = JointPrompt
	}
	request, err := json.Marshal(map[string]any{
		"model": p.Model, "temperature": 0, "seed": p.Seed, "max_tokens": p.MaxTokens,
		"chat_template_kwargs": map[string]bool{"enable_thinking": false},
		"structured_outputs":   map[string]string{"regex": actionGrammar},
		"messages": []map[string]string{
			{"role": "system", "content": prompt + "\n\nSession instructions:\n" + instructions},
			{"role": "user", "content": observation},
		},
	})
	if err != nil {
		return fail(err)
	}
	decision.Request = request
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.URL, "/")+"/chat/completions", bytes.NewReader(request))
	if err != nil {
		return fail(err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := p.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fail(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail(err)
	}
	decision.Response = body
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("policy endpoint: %s", resp.Status))
	}
	var parsed struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err = json.Unmarshal(body, &parsed); err != nil || len(parsed.Choices) != 1 {
		return fail(fmt.Errorf("unexpected policy response"))
	}
	if parsed.Choices[0].FinishReason != "stop" {
		return fail(fmt.Errorf("truncated action: finish_reason %q", parsed.Choices[0].FinishReason))
	}
	if err = json.Unmarshal([]byte(parsed.Choices[0].Message.Content), &decision.Action); err != nil {
		return fail(fmt.Errorf("action is not JSON: %w", err))
	}
	if err = decision.Action.Validate(); err != nil {
		return fail(err)
	}
	decision.Duration = time.Since(decision.StartedAt)
	return decision, nil
}
