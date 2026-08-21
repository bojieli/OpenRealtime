package dynacu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/realtimeclient"
)

// Options configures the gate.
type Options struct {
	Endpoint string
	Token    string
	Model    string
	Cell     bench.Cell
	// FrameInterval is how often a frame is sent. The server gates them; this
	// is a client that does not decide which ones matter.
	FrameInterval time.Duration
	// Timeout bounds the whole attempt.
	Timeout time.Duration
	// Instruction overrides what the user asks for.
	Instruction string
	Progress    func(string)
}

// Outcome is what the gate observed.
type Outcome struct {
	// Negotiated reports whether the server accepted video and computer use.
	Negotiated bool `json:"negotiated"`
	// Observed reports whether an observation carrying the dialog's text came
	// back. Without it, nothing downstream means anything: the agent would be
	// acting on a screen it never saw.
	Observed bool `json:"observed"`
	// ObservedText is what the observer actually said.
	ObservedText string `json:"observed_text,omitempty"`
	// Acted reports whether a computer-use call arrived.
	Acted bool `json:"acted"`
	// Grounded reports whether the click landed on the right control.
	Grounded bool `json:"grounded"`
	// MissDistance is how far a wrong click was from the target's centre, in
	// pixels. It separates "aimed at the wrong thing" from "aimed at the right
	// thing and missed", which have different causes.
	MissDistance float64 `json:"miss_distance_px,omitempty"`
	Clicks       []Click `json:"clicks,omitempty"`
	FinalState   State   `json:"final_state"`
	Failure      string  `json:"failure,omitempty"`
}

// Passed reports whether the gate holds.
func (outcome Outcome) Passed() bool {
	return outcome.Negotiated && outcome.Observed && outcome.Acted && outcome.Grounded
}

// Run drives the gate.
//
// It is a client and nothing more: it sends frames, answers the actions the
// agent requests, and looks at what happened. Every decision about which
// frames matter, what to narrate, and what to click is the server's.
func Run(ctx context.Context, options Options) (Outcome, error) {
	if options.FrameInterval <= 0 {
		options.FrameInterval = 350 * time.Millisecond
	}
	if options.Timeout <= 0 {
		options.Timeout = 90 * time.Second
	}
	if strings.TrimSpace(options.Instruction) == "" {
		options.Instruction = Instruction
	}
	scene := NewScene()
	if err := scene.Validate(); err != nil {
		return Outcome{}, err
	}

	timed, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	client, err := realtimeclient.Dial(timed, realtimeclient.Config{
		URL: options.Endpoint, Token: options.Token, Model: options.Model,
	})
	if err != nil {
		return Outcome{}, err
	}
	defer client.Close()

	watcher := &watcher{scene: scene, done: make(chan struct{})}
	go watcher.read(timed, client, options)

	if err := client.Send(timed, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"instructions": "You are a computer-use agent. Look at the screen and do what the user asks. " +
				"Use the computer.* tools. Click the control that performs the requested action.",
			"tools": computerTools(),
			"openrealtime": openrealtime.Request{
				Version: openrealtime.Version,
				Supports: []openrealtime.Feature{
					openrealtime.FeatureVideoInput,
					openrealtime.FeatureObservations,
					openrealtime.FeatureComputerUse,
				},
			},
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			},
		},
	}); err != nil {
		return Outcome{}, err
	}
	if err := watcher.awaitNegotiation(timed); err != nil {
		return watcher.outcome(), err
	}

	if err := client.Send(timed, map[string]any{
		"type": openrealtime.EventVideoSourceUpdate, "source": "screen",
		"state": "active", "width": scene.Width, "height": scene.Height,
	}); err != nil {
		return watcher.outcome(), err
	}
	// The user asks out loud, as a conversation item rather than as audio:
	// this gate is about seeing and acting, and putting a speech recogniser in
	// the way would make a failure ambiguous.
	if err := client.Send(timed, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": options.Instruction}},
		},
	}); err != nil {
		return watcher.outcome(), err
	}
	if err := client.Send(timed, map[string]any{"type": "response.create"}); err != nil {
		return watcher.outcome(), err
	}

	ticker := time.NewTicker(options.FrameInterval)
	defer ticker.Stop()
	for {
		frame, err := scene.Frame()
		if err != nil {
			return watcher.outcome(), err
		}
		if err := client.Send(timed, map[string]any{
			"type": openrealtime.EventVideoFrameAppend, "source": "screen",
			"frame":        base64.StdEncoding.EncodeToString(frame),
			"timestamp_ms": time.Now().UnixMilli(),
		}); err != nil {
			return watcher.outcome(), err
		}
		select {
		case <-watcher.done:
			return watcher.outcome(), nil
		case <-timed.Done():
			outcome := watcher.outcome()
			if outcome.Failure == "" {
				outcome.Failure = "the agent did not act before the timeout"
			}
			return outcome, nil
		case <-ticker.C:
		}
	}
}

type watcher struct {
	scene *Scene

	mu          sync.Mutex
	negotiated  bool
	observed    bool
	observation string
	acted       bool
	failure     string
	once        sync.Once
	done        chan struct{}
	ready       chan struct{}
	readyOnce   sync.Once
}

func (watcher *watcher) hasObserved() bool {
	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	return watcher.observed
}

func (watcher *watcher) awaitNegotiation(ctx context.Context) error {
	deadline := time.After(20 * time.Second)
	for {
		watcher.mu.Lock()
		negotiated, failure := watcher.negotiated, watcher.failure
		watcher.mu.Unlock()
		if negotiated {
			return nil
		}
		if failure != "" {
			return errors.New(failure)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("the server did not negotiate video and computer use")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (watcher *watcher) read(ctx context.Context, client *realtimeclient.Client, options Options) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-client.Events():
			if !open {
				return
			}
			watcher.handle(ctx, client, options, event)
		}
	}
}

func (watcher *watcher) handle(
	ctx context.Context, client *realtimeclient.Client, options Options, event realtimeclient.Event,
) {
	switch event.Type {
	case "session.updated":
		var decoded struct {
			Session struct {
				OpenRealtime *openrealtime.Response `json:"openrealtime"`
			} `json:"session"`
		}
		if event.Decode(&decoded) != nil || decoded.Session.OpenRealtime == nil {
			return
		}
		video, observations := false, false
		for _, feature := range decoded.Session.OpenRealtime.Enabled {
			switch feature {
			case openrealtime.FeatureVideoInput:
				video = true
			case openrealtime.FeatureObservations:
				observations = true
			}
		}
		watcher.mu.Lock()
		watcher.negotiated = video && observations
		if !watcher.negotiated {
			watcher.failure = "the server did not enable video input and observations; " +
				"a video observer must be configured for this gate"
		}
		watcher.mu.Unlock()
	case openrealtime.EventObservationAdded:
		var decoded openrealtime.ObservationAdded
		if event.Decode(&decoded) != nil {
			return
		}
		watcher.mu.Lock()
		watcher.observation = decoded.Text
		// The observation has to be *about the screen*, not merely present. A
		// narrator that says "a window is open" has not read the dialog, and
		// an action taken after it is not grounded in anything.
		if mentionsDialog(decoded.Text) {
			watcher.observed = true
		}
		watcher.mu.Unlock()
		if options.Progress != nil {
			options.Progress("observed: " + decoded.Text)
		}
	case "response.function_call_arguments.done":
		var decoded struct {
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if event.Decode(&decoded) != nil {
			return
		}
		watcher.act(ctx, client, options, decoded.CallID, decoded.Name, decoded.Arguments)
	case "error":
		var decoded struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = event.Decode(&decoded)
		watcher.mu.Lock()
		watcher.failure = decoded.Error.Message
		watcher.mu.Unlock()
	}
}

func (watcher *watcher) act(
	ctx context.Context, client *realtimeclient.Client, options Options,
	callID, name, arguments string,
) {
	var parsed struct {
		Source string `json:"source"`
		X      int    `json:"x"`
		Y      int    `json:"y"`
	}
	_ = json.Unmarshal([]byte(arguments), &parsed)

	output := `{"status":"ok"}`
	if name == computeruse.Click || name == computeruse.DoubleClick {
		watcher.mu.Lock()
		watcher.acted = true
		watcher.mu.Unlock()
		target := watcher.scene.Click(parsed.X, parsed.Y)
		output = fmt.Sprintf(`{"clicked":%q}`, target)
		if options.Progress != nil {
			options.Progress(fmt.Sprintf("clicked (%d, %d) → %s", parsed.X, parsed.Y, target))
		}
		if watcher.scene.State() != StateDialog {
			// The scene has moved on. One more frame goes out so the agent can
			// see the consequence, then the gate is answered.
			watcher.once.Do(func() {
				go func() {
					time.Sleep(600 * time.Millisecond)
					close(watcher.done)
				}()
			})
		}
	}
	_ = client.Send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "function_call_output", "call_id": callID, "output": output,
		},
	})
	_ = client.Send(ctx, map[string]any{"type": "response.create"})
}

func (watcher *watcher) outcome() Outcome {
	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	outcome := Outcome{
		Negotiated: watcher.negotiated, Observed: watcher.observed,
		ObservedText: watcher.observation, Acted: watcher.acted,
		Clicks: watcher.scene.Clicks(), FinalState: watcher.scene.State(),
		Failure: watcher.failure,
	}
	outcome.Grounded = outcome.FinalState == StateConfirmed
	if !outcome.Grounded && len(outcome.Clicks) > 0 {
		targetX, targetY := watcher.scene.ConfirmRegion().Centre()
		best := math.Inf(1)
		for _, click := range outcome.Clicks {
			distance := math.Hypot(float64(click.X-targetX), float64(click.Y-targetY))
			best = math.Min(best, distance)
		}
		outcome.MissDistance = best
	}
	return outcome
}

// mentionsDialog decides whether an observation actually read the screen.
//
// It looks for the values a person would need - the amount and who it is going
// to - rather than for the whole sentence, because a narrator is allowed to
// phrase things its own way and this is a functional check rather than a
// string comparison.
func mentionsDialog(text string) bool {
	lowered := strings.ToLower(text)
	return strings.Contains(lowered, "40") &&
		(strings.Contains(lowered, "acme") || strings.Contains(lowered, "payment"))
}

func computerTools() []json.RawMessage {
	var tools []json.RawMessage
	for _, definition := range computeruse.Definitions() {
		tool := fmt.Sprintf(
			`{"type":"function","name":%q,"description":%q,"parameters":%s}`,
			definition.Name, definition.Description, definition.Parameters)
		tools = append(tools, json.RawMessage(tool))
	}
	return tools
}

// AsResult wraps the gate as a harness result, so it can be reported and
// compared like any other cell.
func AsResult(cell bench.Cell, outcome Outcome, err error) bench.Result {
	result := bench.Result{
		Suite: "dynacu-gate", Cell: cell, Provenance: bench.Capture(), Expected: 1,
	}
	task := bench.TaskOutcome{
		ID: "confirm-payment",
		Metrics: map[string]float64{
			"negotiated": boolean(outcome.Negotiated),
			"observed":   boolean(outcome.Observed),
			"acted":      boolean(outcome.Acted),
			"grounded":   boolean(outcome.Grounded),
		},
		Notes: map[string]string{
			"observation": outcome.ObservedText,
			"final_state": string(outcome.FinalState),
		},
	}
	if outcome.MissDistance > 0 {
		task.Metrics["miss_distance_px"] = outcome.MissDistance
	}
	switch {
	case err != nil:
		task.Error = err.Error()
	case outcome.Failure != "":
		task.Error = outcome.Failure
	default:
		task.Completed = true
		task.Passed = outcome.Passed()
	}
	result.Tasks = append(result.Tasks, task)
	result.Finish()
	return result
}

func boolean(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
