// Package binding is the legacy seam between the runtime and an adapter stack.
//
// A binding is not a pipeline. It is a declaration of which subsystems the
// model owns, which capabilities are available, and the runtime supplies the
// rest. Named cascade, Omni, and duplex bindings are presets over those two
// declarations rather than mutually exclusive model species.
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
	// Interaction names who chooses conversational acts such as listen,
	// answer, interrupt, and stop-speaking. It is deliberately separate from
	// Floor: a model can detect that a turn ended while an engine policy still
	// decides whether that ending should be answered, and a model capable of
	// full duplex can still be run under an external policy as a controlled
	// comparison.
	Interaction Owner `json:"interaction"`
	// Floor names who establishes the acoustic/turn boundary. Owning it does
	// not imply owning interaction policy, and supporting full duplex does not
	// force either ownership choice.
	Floor Owner `json:"floor"`
}

// Effective fills the interaction column for a binding compiled against the
// earlier ownership shape. Legacy bindings implicitly coupled interaction to
// floor; new bindings should always set Interaction explicitly. Keeping this
// compatibility rule at the seam lets old extensions load while all current
// evidence reports the expanded vector.
func (ownership Ownership) Effective() Ownership {
	if ownership.Interaction == "" {
		ownership.Interaction = ownership.Floor
	}
	return ownership
}

// Validate rejects unknown owners. Which owner combinations a concrete adapter
// can realise is an adapter contract, not a restriction of this shared seam.
func (ownership Ownership) Validate() error {
	ownership = ownership.Effective()
	for name, owner := range map[string]Owner{
		"perception": ownership.Perception, "fast_cognition": ownership.FastCognition,
		"slow_cognition": ownership.SlowCognition, "action": ownership.Action,
		"interaction": ownership.Interaction, "floor": ownership.Floor,
	} {
		switch owner {
		case OwnerEngine, OwnerModel, OwnerRemote:
		default:
			return fmt.Errorf("%s ownership must be engine, model, or remote, got %q", name, owner)
		}
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

// DebugEvent is implementation evidence a developer client may inspect.
// It is intentionally not part of Sink: ordinary bindings and test harnesses
// do not have to implement debugging. A runtime emits it only when its sink
// also implements DebugSink.
type DebugEvent struct {
	Category      string         `json:"category"`
	Name          string         `json:"name"`
	Phase         string         `json:"phase,omitempty"`
	DurationMS    float64        `json:"duration_ms,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Message       string         `json:"message,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	// Payload can contain transcript text, tool arguments, or outputs and is
	// therefore withheld unless the client explicitly requests payloads.
	Payload map[string]any `json:"payload,omitempty"`
}

// DebugSink is the optional developer trace side-channel.
type DebugSink interface {
	Debug(context.Context, DebugEvent) error
}

// SpeechReservationSink is an optional extension for sinks that render a
// response envelope around asynchronous speech. SpeechReserved happens after
// the action plane accepts an utterance but before its worker begins
// synthesis. SpeechReservationCancelled releases only work discarded before
// SpeechBegin; after SpeechBegin, SpeechEnd is the matching release.
//
// A benchmark or transport without response envelopes need not implement it.
type SpeechReservationSink interface {
	SpeechReserved(context.Context, action.Utterance) error
	SpeechReservationCancelled(context.Context, action.Utterance)
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
	// OccurredNS is source time for the typed event. Protocol gateways should
	// stamp it in the same clock domain as other client media; zero lets direct
	// callers use the receiving binding's documented fallback.
	OccurredNS uint64 `json:"occurred_ns,omitempty"`
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
// Graph-native runtimes populate Graph, Binding, and Profile only; exact
// topology, capabilities, deployment, providers, and selected paths are
// authenticated through graph inspection. The remaining fields describe
// legacy binding sessions and must not be treated as graph execution evidence.
type Status struct {
	// Architecture is the immutable project-level composition selected for
	// this session. Binding remains the concrete adapter that realised it. The
	// distinction lets several evolving architectures use the same sidecar
	// runtime without losing which definition was actually launched.
	Architecture ArchitectureIdentity `json:"architecture,omitzero"`
	// Graph is the immutable executable composition mounted for this session.
	// Architecture remains the historical experiment/catalog identity; new
	// evidence keys execution and inspection on Graph.
	Graph              ArchitectureIdentity `json:"graph,omitzero"`
	Binding            string               `json:"binding"`
	Profile            string               `json:"profile,omitempty"`
	Ownership          Ownership            `json:"ownership,omitzero"`
	Stack              StackCapabilities    `json:"stack,omitzero"`
	Policies           interaction.Report   `json:"policies,omitzero"`
	Interaction        InteractionStatus    `json:"interaction,omitzero"`
	Tools              ToolStatus           `json:"tools,omitzero"`
	Observers          []string             `json:"observers,omitempty"`
	Fast               string               `json:"fast,omitempty"`
	Reflex             string               `json:"reflex,omitempty"`
	Slow               string               `json:"slow,omitempty"`
	Perception         string               `json:"perception,omitempty"`
	PerceptionRevision string               `json:"perception_revision,omitempty"`
	// SpeakerIdentity names the optional speaker-embedding adapter. The model
	// artifact behind that adapter is pinned by an experiment manifest, just as
	// it is for Perception and Speech; this live pair proves which adapter path
	// was actually present in the session.
	SpeakerIdentity         string `json:"speaker_identity,omitempty"`
	SpeakerIdentityRevision string `json:"speaker_identity_revision,omitempty"`
	VisualNarrator          string `json:"visual_narrator,omitempty"`
	Speech                  string `json:"speech,omitempty"`
	SpeechRevision          string `json:"speech_revision,omitempty"`
}

// ArchitectureIdentity pins one resolved architecture definition. ID and
// Revision are for people and lineage; Fingerprint protects against a catalog
// entry being edited in place while retaining the same apparent revision.
type ArchitectureIdentity struct {
	ID          string `json:"id,omitempty"`
	Revision    int    `json:"revision,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Empty reports that a runtime was launched through the legacy binding-only
// path rather than through a versioned architecture definition.
func (identity ArchitectureIdentity) Empty() bool {
	return strings.TrimSpace(identity.ID) == "" && identity.Revision == 0 &&
		strings.TrimSpace(identity.Fingerprint) == ""
}

// ToolStatus records authority rather than mere schema visibility. The model
// that can name a call is not necessarily allowed to execute it; authorization
// and the final dispatcher remain separate evidence fields.
type ToolStatus struct {
	Fast          string `json:"fast"`
	Slow          string `json:"slow"`
	Authorization string `json:"authorization"`
	Execution     string `json:"execution"`
}

// InteractionStatus is the live control boundary selected for this session.
// Stack says which capabilities exist and Ownership says which provider is in
// force; this records how evidence and acts actually cross the remaining seam.
// It exists separately from policy names because "model:qwen" cannot say
// whether that model saw a transcript or native audio, or whether its answer
// crossed a typed v2 boundary or was translated into a generic respond signal.
type InteractionStatus struct {
	Evidence string `json:"evidence"`
	// EvidenceCapabilities is the exact set of channels selected into the
	// interaction owner for this live session. It is deliberately separate
	// from StackCapabilities: a foreground may be capable of native audio and
	// direct vision while an external controller is selected to receive only a
	// transcript and a clock. These are active inputs, not model-family labels
	// or a list of everything some component could theoretically expose.
	EvidenceCapabilities InteractionEvidenceCapabilities `json:"evidence_capabilities"`
	Recognizer           string                          `json:"recognizer,omitempty"`
	RecognizerRevision   string                          `json:"recognizer_revision,omitempty"`
	Transport            string                          `json:"transport"`
	ProtocolVersion      int                             `json:"protocol_version,omitempty"`
	ActHandoff           string                          `json:"act_handoff"`
	DecisionTimeoutMS    int                             `json:"decision_timeout_ms,omitempty"`
	NativeSuppression    string                          `json:"native_suppression,omitempty"`
	// Control is the exact set of interaction selectors in force and the rule
	// which gives one of them authority. It closes an ambiguity left by Mode and
	// evidence alone: a text policy may own every act, or it may be composed with
	// a narrow predicate floor. Those are different treatments even though both
	// see the same transcript-and-state evidence and are owned by the engine.
	Control InteractionControl `json:"control,omitempty"`
}

// InteractionControllers is a composable vector of selected interaction
// mechanisms. It describes active selectors, not model families and not every
// selector a foreground could theoretically provide. More than one bit is
// valid only with an explicit arbitration rule in InteractionControl.
type InteractionControllers struct {
	Predicates bool `json:"predicates"`
	TextPolicy bool `json:"text_policy"`
	Native     bool `json:"native"`
	Remote     bool `json:"remote"`
}

// Names returns stable external spellings for the selected mechanisms.
func (controllers InteractionControllers) Names() []string {
	var names []string
	for _, item := range []struct {
		name    string
		enabled bool
	}{
		{"predicates", controllers.Predicates},
		{"text-policy", controllers.TextPolicy},
		{"native", controllers.Native},
		{"remote", controllers.Remote},
	} {
		if item.enabled {
			names = append(names, item.name)
		}
	}
	return names
}

// Merge returns the union of independently selected controller mechanisms.
// Arbitration is deliberately not merged: composing selectors without also
// naming who wins would create two writers for the same conversational act.
func (controllers InteractionControllers) Merge(other InteractionControllers) InteractionControllers {
	return InteractionControllers{
		Predicates: controllers.Predicates || other.Predicates,
		TextPolicy: controllers.TextPolicy || other.TextPolicy,
		Native:     controllers.Native || other.Native,
		Remote:     controllers.Remote || other.Remote,
	}
}

// InteractionControl makes composition operational rather than taxonomic.
// Selectors says which mechanisms are active. Arbitration says how their
// outputs become one authoritative act. "single" requires exactly one
// selector; "predicate-floor" gives acoustic predicates endpoint and overlap
// decisions while the text policy governs semantic, visual, quiet, and silent
// tool acts.
type InteractionControl struct {
	Selectors   InteractionControllers `json:"selectors"`
	Arbitration string                 `json:"arbitration"`
}

// InteractionEvidenceCapabilities is a composable vector of the evidence an
// interaction owner actually receives.
//
// It replaces phrases such as "transcript policy" as a complete description.
// A transcript-conditioned controller may independently receive acoustic
// activity, a silence clock, durable instructions, tool state, speaker
// identity, addressing, narrated vision, or pixels. NativeModelState records
// the opaque audio/latent state available to an in-model interaction head; it
// must not be inferred merely because a stack has NativeInteraction.
type InteractionEvidenceCapabilities struct {
	Transcript        bool `json:"transcript"`
	AcousticActivity  bool `json:"acoustic_activity"`
	SilenceClock      bool `json:"silence_clock"`
	ConversationState bool `json:"conversation_state"`
	ToolState         bool `json:"tool_state"`
	SpeakerIdentity   bool `json:"speaker_identity"`
	Addressing        bool `json:"addressing"`
	VisualDescription bool `json:"visual_description"`
	DirectVisualInput bool `json:"direct_visual_input"`
	NativeModelState  bool `json:"native_model_state"`
}

// Names returns stable external spellings for the selected evidence channels.
func (capabilities InteractionEvidenceCapabilities) Names() []string {
	var names []string
	for _, item := range []struct {
		name    string
		enabled bool
	}{
		{"transcript", capabilities.Transcript},
		{"acoustic-activity", capabilities.AcousticActivity},
		{"silence-clock", capabilities.SilenceClock},
		{"conversation-state", capabilities.ConversationState},
		{"tool-state", capabilities.ToolState},
		{"speaker-identity", capabilities.SpeakerIdentity},
		{"addressing", capabilities.Addressing},
		{"visual-description", capabilities.VisualDescription},
		{"direct-visual-input", capabilities.DirectVisualInput},
		{"native-model-state", capabilities.NativeModelState},
	} {
		if item.enabled {
			names = append(names, item.name)
		}
	}
	return names
}

// Missing returns the required evidence channels which are not selected.
func (capabilities InteractionEvidenceCapabilities) Missing(
	required InteractionEvidenceCapabilities,
) []string {
	var missing []string
	for _, name := range required.Names() {
		available := false
		switch name {
		case "transcript":
			available = capabilities.Transcript
		case "acoustic-activity":
			available = capabilities.AcousticActivity
		case "silence-clock":
			available = capabilities.SilenceClock
		case "conversation-state":
			available = capabilities.ConversationState
		case "tool-state":
			available = capabilities.ToolState
		case "speaker-identity":
			available = capabilities.SpeakerIdentity
		case "addressing":
			available = capabilities.Addressing
		case "visual-description":
			available = capabilities.VisualDescription
		case "direct-visual-input":
			available = capabilities.DirectVisualInput
		case "native-model-state":
			available = capabilities.NativeModelState
		}
		if !available {
			missing = append(missing, name)
		}
	}
	return missing
}

// Satisfies reports whether every required evidence channel is selected.
func (capabilities InteractionEvidenceCapabilities) Satisfies(
	required InteractionEvidenceCapabilities,
) bool {
	return len(capabilities.Missing(required)) == 0
}

// Merge returns the union of two independently supplied evidence channel
// sets. Architecture selection still records the exact resulting vector; the
// union is assembly machinery, not permission for an undeclared channel to
// enter a measured cell.
func (capabilities InteractionEvidenceCapabilities) Merge(
	other InteractionEvidenceCapabilities,
) InteractionEvidenceCapabilities {
	return InteractionEvidenceCapabilities{
		Transcript:        capabilities.Transcript || other.Transcript,
		AcousticActivity:  capabilities.AcousticActivity || other.AcousticActivity,
		SilenceClock:      capabilities.SilenceClock || other.SilenceClock,
		ConversationState: capabilities.ConversationState || other.ConversationState,
		ToolState:         capabilities.ToolState || other.ToolState,
		SpeakerIdentity:   capabilities.SpeakerIdentity || other.SpeakerIdentity,
		Addressing:        capabilities.Addressing || other.Addressing,
		VisualDescription: capabilities.VisualDescription || other.VisualDescription,
		DirectVisualInput: capabilities.DirectVisualInput || other.DirectVisualInput,
		NativeModelState:  capabilities.NativeModelState || other.NativeModelState,
	}
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
	// FastSlow reports whether a separately addressable fast/slow arrangement is
	// available. False is valid for a single-provider or differently composed
	// deployment.
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
	// Stack describes the independently composable capabilities of the voice
	// stack. These are things the stack can do, not declarations that it owns
	// them in this session; Ownership selects the active provider. Keeping the
	// two separate is what permits a native-interaction model to be evaluated
	// under an engine policy without pretending it stopped having the native
	// capability.
	Stack StackCapabilities `json:"stack"`
}

// StackCapabilities is a feature vector, not a model taxonomy.
//
// In particular, TurnGeneration, ConcurrentIO, and NativeInteraction are not
// mutually exclusive. A model may expose all three, and may additionally
// accept InteractionActs from an external controller. The old cascade, Omni,
// and duplex names are useful presets and benchmark labels over this vector;
// they are not closed species that runtime code should switch on.
type StackCapabilities struct {
	AudioInput        bool `json:"audio_input"`
	AudioOutput       bool `json:"audio_output"`
	VisualInput       bool `json:"visual_input"`
	Transcription     bool `json:"transcription"`
	TurnGeneration    bool `json:"turn_generation"`
	ConcurrentIO      bool `json:"concurrent_io"`
	NativeFloor       bool `json:"native_floor"`
	NativeInteraction bool `json:"native_interaction"`
	InteractionActs   bool `json:"interaction_acts"`
	TextInjection     bool `json:"text_injection"`
}

// Names returns the stable external spellings of every available capability.
func (capabilities StackCapabilities) Names() []string {
	var names []string
	for _, item := range []struct {
		name    string
		enabled bool
	}{
		{"audio-input", capabilities.AudioInput},
		{"audio-output", capabilities.AudioOutput},
		{"visual-input", capabilities.VisualInput},
		{"transcription", capabilities.Transcription},
		{"turn-generation", capabilities.TurnGeneration},
		{"concurrent-io", capabilities.ConcurrentIO},
		{"native-floor", capabilities.NativeFloor},
		{"native-interaction", capabilities.NativeInteraction},
		{"interaction-acts", capabilities.InteractionActs},
		{"text-injection", capabilities.TextInjection},
	} {
		if item.enabled {
			names = append(names, item.name)
		}
	}
	return names
}

// Missing returns the requirements this capability vector does not satisfy.
// Extra capabilities are deliberately harmless: selecting external policy
// does not require pretending the foreground lost its native interaction head.
func (capabilities StackCapabilities) Missing(required StackCapabilities) []string {
	var missing []string
	for _, name := range required.Names() {
		available := false
		switch name {
		case "audio-input":
			available = capabilities.AudioInput
		case "audio-output":
			available = capabilities.AudioOutput
		case "visual-input":
			available = capabilities.VisualInput
		case "transcription":
			available = capabilities.Transcription
		case "turn-generation":
			available = capabilities.TurnGeneration
		case "concurrent-io":
			available = capabilities.ConcurrentIO
		case "native-floor":
			available = capabilities.NativeFloor
		case "native-interaction":
			available = capabilities.NativeInteraction
		case "interaction-acts":
			available = capabilities.InteractionActs
		case "text-injection":
			available = capabilities.TextInjection
		}
		if !available {
			missing = append(missing, name)
		}
	}
	return missing
}

// Satisfies reports whether every required capability is available.
func (capabilities StackCapabilities) Satisfies(required StackCapabilities) bool {
	return len(capabilities.Missing(required)) == 0
}

// Merge returns the union of two capability declarations. Capability
// composition is monotonic: learning that a sidecar accepts text injection,
// for example, must not erase the fact that its preset supports concurrent
// audio.
func (capabilities StackCapabilities) Merge(other StackCapabilities) StackCapabilities {
	return StackCapabilities{
		AudioInput:        capabilities.AudioInput || other.AudioInput,
		AudioOutput:       capabilities.AudioOutput || other.AudioOutput,
		VisualInput:       capabilities.VisualInput || other.VisualInput,
		Transcription:     capabilities.Transcription || other.Transcription,
		TurnGeneration:    capabilities.TurnGeneration || other.TurnGeneration,
		ConcurrentIO:      capabilities.ConcurrentIO || other.ConcurrentIO,
		NativeFloor:       capabilities.NativeFloor || other.NativeFloor,
		NativeInteraction: capabilities.NativeInteraction || other.NativeInteraction,
		InteractionActs:   capabilities.InteractionActs || other.InteractionActs,
		TextInjection:     capabilities.TextInjection || other.TextInjection,
	}
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
		settings.Tools[index].ArgumentNormalizers = slices.Clone(settings.Tools[index].ArgumentNormalizers)
	}
	settings.Modalities = slices.Clone(settings.Modalities)
	settings.Observers = slices.Clone(settings.Observers)
	return settings
}
