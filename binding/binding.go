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
	// ManualTurns reports that the client turned server voice-activity
	// detection off and will declare its own turns. A binding must then stop
	// endpointing on silence and stop creating responses of its own: the
	// client owns the floor, and answering a turn it has not finished
	// declaring is the engine overruling the side it delegated to.
	ManualTurns bool `json:"manual_turns,omitempty"`
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

// ActivityEvent reports what happened to the input buffer.
//
// Started and Stopped are the acoustic gate's view of the user, which only a
// session whose floor the engine owns has to report: a client running its own
// turn detection is not being told about voice activity, it is declaring it.
// Committed is that client's declaration coming back acknowledged.
type ActivityEvent struct {
	Started      bool   `json:"started"`
	Stopped      bool   `json:"stopped"`
	Committed    bool   `json:"committed,omitempty"`
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

// TextInput is something a client typed rather than said.
//
// It is the same participant either way: a text client and a voice client are
// both the user, and the difference is the transport rather than the
// provenance.
type TextInput struct {
	ItemID string `json:"item_id"`
	Role   string `json:"role"`
	Text   string `json:"text"`
	// Images are pictures the client attached to this turn.
	//
	// They are not a video source. A source is a stream the server gates,
	// which is what the extension's video events are for; an image in a
	// message is content of the turn, shown once because the client chose to
	// show it, and gating it could discard the only thing the turn was about.
	// So it is retained and referenced from the observation rather than
	// offered to an observer.
	Images []Image `json:"images,omitempty"`
}

// Image is one picture attached to a turn.
type Image struct {
	Bytes    []byte `json:"-"`
	MIMEType string `json:"mime_type"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`
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
	// TurnBegin and TurnEnd bracket everything one rollout produced.
	//
	// They exist because a response is a turn, not an output kind. The
	// protocol's contract is one response per response.create, carrying every
	// output item the turn produced - text, audio, and function calls
	// together, indexed within it - and a client that has been told a
	// response is done stops reading. Rendering each output kind as its own
	// response would end the turn, from the client's point of view, at
	// whichever kind happened to come first.
	//
	// A turn that produces nothing usually announces nothing: the brackets are
	// declared here and the response is opened lazily by whatever first
	// crosses into the world. The exception is a turn that produced nothing
	// because something went wrong, which TurnEnd reports - a client that
	// asked for a response and is told neither what it got nor why is left
	// waiting on something that is never coming.
	TurnBegin(context.Context) error
	TurnEnd(context.Context, TurnOutcome) error
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
	// Text delivers something the client typed.
	Text(context.Context, TextInput) error
	// ToolResult accepts a client-executed function result.
	ToolResult(context.Context, trajectory.ToolResult) error
	// CommitAudio closes the input audio buffer and commits what it holds.
	//
	// It is what a client running its own turn detection sends instead of
	// waiting for silence. A binding whose floor is the engine's has nothing
	// to do here and says so; one that has taken the client's declaration
	// ends the turn where the client said it ended.
	CommitAudio(context.Context) error
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

// TurnOutcome is why a turn ended, for the cases where that is not obvious
// from what it produced.
//
// A turn that said something needs no explanation. One that said nothing needs
// one, because silence is indistinguishable from working correctly and having
// nothing to add - and the two want opposite reactions from whoever is
// watching.
type TurnOutcome struct {
	// Incomplete reports that the turn stopped short of what it was doing.
	Incomplete bool `json:"incomplete,omitempty"`
	// Reason is the protocol's own vocabulary for why, so it can be reported
	// on the wire without translation: max_output_tokens or content_filter.
	Reason string `json:"reason,omitempty"`
	// Detail is for the operator rather than the client. It names the thing to
	// change, because the interesting failures here are configuration rather
	// than faults.
	Detail string `json:"detail,omitempty"`
}

// TurnIncompleteTokens is the protocol's reason for a turn cut short by the
// output limit.
const TurnIncompleteTokens = "max_output_tokens"

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
	// Observers names the perception a session may select from. A client
	// cannot choose an observer set without knowing what the names are, and a
	// deployment's set is a configuration rather than a constant.
	Observers []string `json:"observers,omitempty"`
	// Voice reports how this binding's voice is chosen.
	Voice VoiceControl `json:"voice"`
	// ManualTurns reports whether this binding can hand the floor to the
	// client - stop endpointing on silence, stop creating responses of its
	// own, and wait to be asked.
	//
	// It is not universal. A binding whose model owns the floor cannot give
	// away what it does not hold, and a client told it took the floor from one
	// that did not is talking over an agent that never agreed to listen.
	ManualTurns bool `json:"manual_turns"`
	// MaxOutputTokens is the limit this binding puts on one spoken turn, or
	// zero where it imposes none and the model's own governs.
	//
	// A session reports it, and a turn that runs into it now says so:
	// response.done carries max_output_tokens as the reason it stopped short.
	// A session object claiming there is no maximum while one is in force
	// makes that reason unintelligible - the client is told a limit it was
	// told did not exist is what cut its turn off.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
}

// VoiceControl is how a session's voice is decided.
//
// Whether a client may choose one is a property of the binding, not of the
// protocol: a binding that synthesises with a provider configured once at
// startup has no per-session voice to give, while a binding that forwards to a
// model with several does. The gateway cannot tell which it is holding, and
// the failure from guessing is silent - a client names a voice, is told it got
// it, and hears a different one, with no event anywhere saying otherwise.
type VoiceControl struct {
	// Selectable reports whether a session may name its own voice.
	Selectable bool `json:"selectable"`
	// InForce is the voice used when a session names none. Empty means the
	// binding cannot say, which is the honest answer for one that forwards to
	// a provider whose default is the provider's own - better than naming a
	// voice nothing will use.
	InForce string `json:"in_force,omitempty"`
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
