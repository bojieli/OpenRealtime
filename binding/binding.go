// Package binding is the seam between the runtime and a voice stack.
//
// A binding is not a pipeline. It is a declaration of which subsystems the
// model owns, and the runtime supplies the rest. That is the whole idea: a
// cascade owns nothing, so the engine supplies perception, cognition, action,
// and the floor; a full-duplex model owns almost everything, so the engine
// supplies the background reasoner and the policies around it.
//
// One column never varies. Slow cognition is always the engine's, because no
// foreground model provides it - which is exactly the thing this project adds
// to whatever stack it is given.
package binding

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Owner names who provides a subsystem.
type Owner string

const (
	// OwnerEngine: OpenRealtime provides it.
	OwnerEngine Owner = "engine"
	// OwnerModel: the binding's own model provides it.
	OwnerModel Owner = "model"
	// OwnerRemote: a remote service provides it.
	OwnerRemote Owner = "remote"
)

// Ownership is the declaration itself.
type Ownership struct {
	Perception    Owner `json:"perception"`
	FastCognition Owner `json:"fast_cognition"`
	SlowCognition Owner `json:"slow_cognition"`
	Action        Owner `json:"action"`
	// Floor is the one interaction policy a model can take over. Every other
	// interaction policy stays with the engine even when the model owns the
	// floor, so ownership is not all-or-nothing.
	Floor Owner `json:"floor"`
}

// Validate rejects a declaration that contradicts the architecture.
func (ownership Ownership) Validate() error {
	for name, owner := range map[string]Owner{
		"perception": ownership.Perception, "fast_cognition": ownership.FastCognition,
		"slow_cognition": ownership.SlowCognition, "action": ownership.Action, "floor": ownership.Floor,
	} {
		switch owner {
		case OwnerEngine, OwnerModel, OwnerRemote:
		default:
			return fmt.Errorf("%s ownership must be engine, model, or remote, got %q", name, owner)
		}
	}
	if ownership.SlowCognition != OwnerEngine {
		return errors.New("slow cognition is always the engine's: no foreground model provides it")
	}
	return nil
}

// Settings is the client-visible session configuration a binding must honour.
type Settings struct {
	Instruction string                `json:"instruction,omitempty"`
	Tools       []action.ToolSpec     `json:"tools,omitempty"`
	Voice       string                `json:"voice,omitempty"`
	Modalities  []string              `json:"modalities,omitempty"`
	Gate        perception.GateConfig `json:"gate"`
	// Observers names the observer set for this session. Empty selects the
	// binding's documented default set.
	Observers []string `json:"observers,omitempty"`
}

// TranscriptEvent reports what an observer committed about the user.
type TranscriptEvent struct {
	ItemID      string  `json:"item_id"`
	Text        string  `json:"text"`
	Final       bool    `json:"final"`
	DurationSec float64 `json:"duration_seconds,omitempty"`
}

// ActivityEvent reports the acoustic gate's view of the user.
type ActivityEvent struct {
	Started      bool   `json:"started"`
	Stopped      bool   `json:"stopped"`
	ItemID       string `json:"item_id"`
	AudioStartMS int    `json:"audio_start_ms,omitempty"`
	AudioEndMS   int    `json:"audio_end_ms,omitempty"`
}

// ToolCallEvent hands authoritative calls to whoever executes them.
type ToolCallEvent struct {
	InvocationID string                `json:"invocation_id"`
	Calls        []trajectory.ToolCall `json:"calls"`
	Usage        *continuation.Usage   `json:"usage,omitempty"`
}

// ErrorEvent reports a runtime failure that the client should see.
type ErrorEvent struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Truncation reports client-side playback truncation: the client stopped
// playing an item after a known amount of audio.
type Truncation struct {
	ItemID     string `json:"item_id"`
	AudioEndMS int    `json:"audio_end_ms"`
}

// Sink is how a binding's outputs reach the world.
//
// It is deliberately not the Realtime wire format. A binding produces
// transcripts, paced audio, tool calls, and observations; rendering those as
// protocol events is the gateway's job, and a benchmark harness or a test
// implements the same interface without a socket in sight.
type Sink interface {
	// Activity reports the acoustic gate's view of the user.
	Activity(context.Context, ActivityEvent) error
	// Transcript reports committed user speech.
	Transcript(context.Context, TranscriptEvent) error
	// Observation reports what an observer perceived, so a client can display
	// and audit it. It is optional and purely outbound.
	Observation(context.Context, perception.Observation) error
	// SpeechBegin, SpeechText, SpeechAudio, and SpeechEnd carry one spoken
	// turn. Text is delivered separately from the announcement because a
	// binding that streams - a model producing text and audio together -
	// does not know what it is about to say when the turn opens.
	SpeechBegin(context.Context, action.Utterance) error
	SpeechText(context.Context, action.Utterance, string) error
	SpeechAudio(context.Context, action.Utterance, action.Frame) error
	SpeechEnd(context.Context, action.Utterance, action.Outcome) error
	// ToolCalls hands authoritative calls to the client for execution.
	ToolCalls(context.Context, ToolCallEvent) error
	// Failed reports an error the client should see.
	Failed(context.Context, ErrorEvent)
}

// Runtime is one live session.
//
// Every method is safe to call concurrently: audio arrives on one goroutine
// while a tool result arrives on another, and serialising them is the event
// loop's job rather than the caller's.
type Runtime interface {
	// Update applies a client configuration change.
	Update(context.Context, Settings) error
	// Audio delivers one input audio frame.
	Audio(context.Context, perception.Frame) error
	// Video delivers one input video frame. A binding with no video observer
	// returns ErrUnsupported, which the protocol layer reports as a
	// negotiation failure rather than swallowing.
	Video(context.Context, perception.Frame) error
	// ToolResult accepts a client-executed function result.
	ToolResult(context.Context, trajectory.ToolResult) error
	// CreateResponse asks for a response now.
	CreateResponse(context.Context) error
	// Cancel cancels response generation in flight.
	Cancel(context.Context, string) error
	// Truncate reports client-side playback truncation.
	Truncate(context.Context, Truncation) error
	// Trajectory returns the canonical log, for inspection and evidence.
	Trajectory() trajectory.Snapshot
	// Status reports what this session is running.
	Status() Status
	// Close ends the session.
	Close(context.Context, error) error
}

// ErrUnsupported means a binding does not provide a capability at all.
var ErrUnsupported = errors.New("capability not supported by this binding")

// Status is what a session is running, for the health endpoint and evidence.
type Status struct {
	Binding   string             `json:"binding"`
	Ownership Ownership          `json:"ownership"`
	Policies  interaction.Report `json:"policies"`
	Observers []string           `json:"observers"`
	Fast      string             `json:"fast,omitempty"`
	Slow      string             `json:"slow,omitempty"`
	Speech    string             `json:"speech,omitempty"`
}

// Binding creates session runtimes.
type Binding interface {
	Name() string
	Ownership() Ownership
	// Capabilities reports what this binding supports, so the protocol layer
	// can negotiate honestly rather than promising and failing.
	Capabilities() Capabilities
	// Start creates a runtime for one session.
	Start(context.Context, Options) (Runtime, error)
}

// Capabilities is what a binding can do.
type Capabilities struct {
	// Video reports whether video input is supported at all.
	Video bool `json:"video"`
	// ComputerUse reports whether computer-use actions can be dispatched.
	ComputerUse bool `json:"computer_use"`
	// Observations reports whether observation events are emitted.
	Observations bool `json:"observations"`
	// FastSlow reports whether a background reasoner is available. Every
	// binding must support it - it is the differentiator - but a degraded
	// deployment may have it configured off.
	FastSlow bool `json:"fast_slow"`
}

// Options configures one session.
type Options struct {
	Sink     Sink
	Settings Settings
	// Policies overrides the binding's default interaction policy set. A zero
	// value selects the binding's documented defaults.
	Policies *interaction.Policies
	// SessionID is used to prefix generated identifiers.
	SessionID string
}

// Registry maps binding names to implementations.
//
// It exists so a deployment names a binding in configuration and so third
// parties can register their own. The names are part of the stable extension
// surface from v1.0.
type Registry struct {
	mu       sync.RWMutex
	bindings map[string]Binding
}

// NewRegistry creates an empty registry.
func NewRegistry() *Registry {
	return &Registry{bindings: make(map[string]Binding)}
}

// Register adds a binding, rejecting a duplicate name.
func (registry *Registry) Register(binding Binding) error {
	if binding == nil {
		return errors.New("registry cannot register a nil binding")
	}
	name := strings.TrimSpace(binding.Name())
	if name == "" {
		return errors.New("a binding requires a name")
	}
	if err := binding.Ownership().Validate(); err != nil {
		return fmt.Errorf("binding %q: %w", name, err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.bindings[name]; exists {
		return fmt.Errorf("duplicate binding %q", name)
	}
	registry.bindings[name] = binding
	return nil
}

// Lookup returns a registered binding.
func (registry *Registry) Lookup(name string) (Binding, error) {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	binding, exists := registry.bindings[strings.TrimSpace(name)]
	if !exists {
		return nil, fmt.Errorf("unknown binding %q (available: %s)", name, strings.Join(registry.namesLocked(), ", "))
	}
	return binding, nil
}

// Names lists registered bindings in sorted order.
func (registry *Registry) Names() []string {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return registry.namesLocked()
}

func (registry *Registry) namesLocked() []string {
	names := make([]string, 0, len(registry.bindings))
	for name := range registry.bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CloneSettings deep-copies settings so a binding cannot be mutated through
// the value a client handed it.
func CloneSettings(settings Settings) Settings {
	settings.Tools = slices.Clone(settings.Tools)
	for index := range settings.Tools {
		settings.Tools[index].Parameters = slices.Clone(settings.Tools[index].Parameters)
	}
	settings.Modalities = slices.Clone(settings.Modalities)
	settings.Observers = slices.Clone(settings.Observers)
	return settings
}
