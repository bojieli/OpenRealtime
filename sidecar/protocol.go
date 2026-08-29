// Package sidecar defines the process boundary between the engine and a model
// that is not written in Go.
//
// Many audio-model inference stacks live in Python. Letting that Python into
// the build, the test path, or the analysis path would make every one of them
// slower and more fragile, so it stays on the other side of a process
// boundary with a documented, versioned protocol between. The conformance
// suite is the contract: a sidecar that passes it works, whatever it is
// written in, and a sidecar that does not is broken before anyone runs a
// model.
//
// # Framing
//
// Each message is one JSON header line terminated by a newline, optionally
// followed by exactly PayloadBytes of binary payload:
//
//	{"type":"audio","payload_bytes":960}\n<960 bytes of PCM16>
//
// Audio is the only thing large enough to matter and it is raw rather than
// base64, because a third more bytes and an encode/decode pass on every frame
// is a real cost on the hot path. Everything else is ordinary JSON, so a
// sidecar can be debugged by reading the stream.
package sidecar

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
)

// Version is the frozen, legacy protocol version and remains the default.
// VersionInteraction adds typed interaction-act handoff. VersionMultimodal
// adds direct image frames and live tool-catalog updates. VersionElementGraph
// carries arbitrary descriptor-checked element ports rather than assuming an
// audio binding. Selecting a later version is explicit in Config so existing
// sidecars are never reinterpreted merely because the engine learned a new
// frame.
const Version = 1

const (
	VersionInteraction  = 2
	VersionMultimodal   = 3
	VersionElementGraph = 4
	LatestVersion       = VersionElementGraph
)

// MessageType names one frame.
type MessageType string

// Engine to sidecar.
const (
	// TypeHello opens a session and carries its configuration. It is always
	// the first message, and a sidecar must answer with Ready or Error.
	TypeHello MessageType = "hello"
	// TypeAudio carries input PCM16 at the negotiated rate.
	TypeAudio MessageType = "audio"
	// TypeImage carries one encoded JPEG or PNG frame. It is protocol v3: the
	// model sees pixels directly rather than a narration standing in for them.
	TypeImage MessageType = "image"
	// TypeToolsUpdate replaces the live function catalog after session.update.
	// A Realtime binding starts before the client's tool declaration arrives,
	// so a hello-only catalog would always be stale in production.
	TypeToolsUpdate MessageType = "tools_update"
	// TypeText injects text into the model's context without asking it to
	// respond. It is how a background reasoner's answer reaches a model that
	// owns its own voice.
	TypeText MessageType = "text"
	// TypeCommit closes the input audio buffer: the client has declared its
	// turn over rather than waiting for silence. A sidecar whose model owns
	// its own floor decides for itself what to do about that; one that does
	// not can ignore it, because the engine has already ended the turn.
	TypeCommit MessageType = "commit"
	// TypeRespond asks the model to produce a turn now.
	TypeRespond MessageType = "respond"
	// TypeInterrupt stops generation at the model's next safe point.
	TypeInterrupt MessageType = "interrupt"
	// TypeInteractionAct carries a policy decision rather than collapsing it
	// into respond or interrupt. It is available in protocol v2 when the
	// sidecar declares CapabilityInteractionActs.
	TypeInteractionAct MessageType = "interaction_act"
	// TypeToolResult returns the outcome of a call the model requested.
	TypeToolResult MessageType = "tool_result"
	// TypeBye ends the session cleanly.
	TypeBye MessageType = "bye"
	// TypeElementFrame carries one envelope on one descriptor-declared port.
	// It is symmetric: the engine sends frames only to input ports and the
	// sidecar sends frames only from output ports. Trigger, interrupt, timing,
	// terminal, and state semantics remain in the port's element.Type rather
	// than being collapsed into transport-specific message kinds.
	TypeElementFrame MessageType = "element_frame"
)

// Sidecar to engine.
const (
	// TypeReady answers Hello and declares what this sidecar can do.
	TypeReady MessageType = "ready"
	// TypeSpeechStarted and TypeSpeechStopped report model-native voice
	// activity. A sidecar that has no native detector never sends them, and
	// the engine keeps the floor.
	TypeSpeechStarted MessageType = "speech_started"
	TypeSpeechStopped MessageType = "speech_stopped"
	// TypeTranscript reports what the model heard.
	TypeTranscript MessageType = "transcript"
	// TypeTextDelta and TypeTextDone report what the model is saying, as text.
	TypeTextDelta MessageType = "text_delta"
	TypeTextDone  MessageType = "text_done"
	// TypeOutputAudio carries output PCM16 at the declared rate.
	TypeOutputAudio MessageType = "output_audio"
	// TypeTurnDone marks the end of one model turn.
	TypeTurnDone MessageType = "turn_done"
	// TypeToolCall reports a call the model wants made. A sidecar that cannot
	// call tools never sends it.
	TypeToolCall MessageType = "tool_call"
	// TypeError reports a failure. It does not end the session unless Fatal.
	TypeError MessageType = "error"
	// TypeLog carries a diagnostic line, so a sidecar's own logging does not
	// have to fight for stderr with the process that started it.
	TypeLog MessageType = "log"
)

// Message is one frame. Payload is carried out of band and is not part of the
// JSON; it is populated by the reader and consumed by the writer.
type Message struct {
	Type         MessageType `json:"type"`
	PayloadBytes int         `json:"payload_bytes,omitempty"`

	// Hello and Ready.
	Version      int      `json:"version,omitempty"`
	Model        string   `json:"model,omitempty"`
	SampleRate   int      `json:"sample_rate,omitempty"`
	OutputRate   int      `json:"output_rate,omitempty"`
	Instructions string   `json:"instructions,omitempty"`
	Voice        string   `json:"voice,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Tools        []Tool   `json:"tools,omitempty"`

	// Generic element handshake and frames (protocol v4). The full descriptor
	// is confirmed at readiness so a package declaration cannot silently drift
	// from the process that actually started. SelectedPorts is the graph's
	// active observation/action surface, not everything the implementation is
	// theoretically capable of supporting.
	ElementDescriptor    *element.Descriptor     `json:"element_descriptor,omitempty"`
	ElementConfig        json.RawMessage         `json:"element_config,omitempty"`
	SelectedPorts        []PortSelection         `json:"selected_ports,omitempty"`
	RequiredCapabilities []CapabilityRequirement `json:"required_capabilities,omitempty"`
	RuntimeArtifact      ArtifactIdentity        `json:"runtime_artifact,omitempty"`
	ResolvedCapabilities []CapabilityIdentity    `json:"resolved_capabilities,omitempty"`
	Port                 string                  `json:"port,omitempty"`
	Envelope             *WireEnvelope           `json:"envelope,omitempty"`
	// Selected ownership (protocol v2). Capabilities say what the stack can
	// do; these fields tell a sidecar which available provider is active.
	InteractionOwner string `json:"interaction_owner,omitempty"`
	FloorOwner       string `json:"floor_owner,omitempty"`

	// Transcript, text, and log.
	Text  string `json:"text,omitempty"`
	Final bool   `json:"final,omitempty"`
	Level string `json:"level,omitempty"`

	// Text injection.
	Role string `json:"role,omitempty"`

	// Tool call and result.
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`

	// Errors.
	Code  string `json:"code,omitempty"`
	Fatal bool   `json:"fatal,omitempty"`

	// Timing.
	TimestampMS int64  `json:"timestamp_ms,omitempty"`
	Source      string `json:"source,omitempty"`
	MIMEType    string `json:"mime_type,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`

	// Typed interaction plan (protocol v2).
	Act         string  `json:"act,omitempty"`
	Policy      string  `json:"policy,omitempty"`
	EvidenceRef string  `json:"evidence_ref,omitempty"`
	Floor       string  `json:"floor,omitempty"`
	DeadlineMS  int64   `json:"deadline_ms,omitempty"`
	Confidence  float64 `json:"confidence,omitempty"`
	Abstained   bool    `json:"abstained,omitempty"`

	// Payload is the binary body. It is never marshalled into the header.
	Payload []byte `json:"-"`
}

// Tool is a function definition handed to a sidecar.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// Capability names something a sidecar can do. The engine reads these to
// decide what it must supply itself, which is what makes a partial
// implementation useful rather than broken.
type Capability string

const (
	// CapabilityNativeVAD means the model detects voice activity itself. A
	// sidecar without it gets an engine-owned floor.
	CapabilityNativeVAD Capability = "native_vad"
	// CapabilityTranscript means the model reports what it heard.
	CapabilityTranscript Capability = "transcript"
	// CapabilityTextInjection means text can be added to the model's context
	// between turns. Without it, a background reasoner cannot reach the model
	// at all, and the binding must fall back to an explicit hand-off.
	CapabilityTextInjection Capability = "text_injection"
	// CapabilityVisualInput means protocol-v3 image frames reach the model as
	// pixels. A narrated screen does not satisfy this capability.
	CapabilityVisualInput Capability = "visual_input"
	// CapabilityTools means the model can request function calls.
	CapabilityTools Capability = "tools"
	// CapabilityBargeIn means the model handles overlap itself.
	CapabilityBargeIn Capability = "barge_in"
	// CapabilityFullDuplex means the model can listen and speak at once. Floor
	// and interaction ownership are selected independently by the binding.
	CapabilityFullDuplex Capability = "full_duplex"
	// CapabilityNativeInteraction means the model chooses conversational acts
	// from its own multimodal state. It is independent of full duplex and of
	// native VAD: those are capabilities a model often has alongside it, not
	// synonyms for it.
	CapabilityNativeInteraction Capability = "native_interaction"
	// CapabilityInteractionActs means the sidecar accepts protocol-v2 typed
	// plans from an external interaction controller.
	CapabilityInteractionActs Capability = "interaction_acts"
)

// Validate rejects a malformed frame before it reaches either side.
//
// The checks are deliberately about structure rather than about semantics: a
// sidecar author debugging their implementation should get "audio needs a
// payload" rather than a silent hang three seconds later.
func (message Message) Validate() error {
	if strings.TrimSpace(string(message.Type)) == "" {
		return errors.New("a sidecar message requires a type")
	}
	if message.PayloadBytes < 0 {
		return errors.New("payload length cannot be negative")
	}
	if message.PayloadBytes != len(message.Payload) && len(message.Payload) > 0 {
		return fmt.Errorf("payload declares %d bytes and carries %d", message.PayloadBytes, len(message.Payload))
	}
	switch message.Type {
	case TypeHello:
		if message.Version == VersionElementGraph {
			return validateElementHello(message)
		}
		if message.Version <= 0 || message.SampleRate <= 0 {
			return errors.New("hello requires a version and an input sample rate")
		}
		if message.Version >= VersionInteraction {
			for field, owner := range map[string]string{
				"interaction_owner": message.InteractionOwner, "floor_owner": message.FloorOwner,
			} {
				if owner != "engine" && owner != "model" && owner != "remote" {
					return fmt.Errorf("v2 hello %s must be engine, model, or remote", field)
				}
			}
		}
	case TypeReady:
		if message.Version == VersionElementGraph {
			return validateElementReady(message)
		}
		if message.Version <= 0 || message.OutputRate <= 0 {
			return errors.New("ready requires a version and an output sample rate")
		}
		if strings.TrimSpace(message.Model) == "" {
			return errors.New("ready requires the model identity, so evidence can name what produced it")
		}
	case TypeAudio, TypeOutputAudio:
		if len(message.Payload) == 0 {
			return fmt.Errorf("%s requires a PCM16 payload", message.Type)
		}
		if len(message.Payload)%2 != 0 {
			return fmt.Errorf("%s payload must contain whole PCM16 samples", message.Type)
		}
	case TypeImage:
		if len(message.Payload) == 0 || strings.TrimSpace(message.Source) == "" ||
			message.Width <= 0 || message.Height <= 0 {
			return errors.New("image requires encoded bytes, a source, and positive geometry")
		}
		if message.MIMEType != "image/jpeg" && message.MIMEType != "image/png" {
			return fmt.Errorf("image MIME type must be image/jpeg or image/png, got %q", message.MIMEType)
		}
	case TypeText:
		if strings.TrimSpace(message.Text) == "" {
			return errors.New("text injection requires text")
		}
	case TypeTranscript, TypeTextDelta:
		if strings.TrimSpace(message.Text) == "" {
			return fmt.Errorf("%s requires text", message.Type)
		}
	case TypeToolCall:
		if strings.TrimSpace(message.CallID) == "" || strings.TrimSpace(message.Name) == "" {
			return errors.New("a tool call requires a call ID and a name")
		}
		if len(message.Arguments) == 0 || !json.Valid(message.Arguments) {
			return errors.New("tool call arguments must be valid JSON")
		}
	case TypeToolResult:
		if strings.TrimSpace(message.CallID) == "" {
			return errors.New("a tool result requires a call ID")
		}
		hasOutput := len(message.Output) > 0
		hasError := strings.TrimSpace(message.Error) != ""
		if hasOutput == hasError {
			return errors.New("a tool result requires exactly one of output or error")
		}
	case TypeInteractionAct:
		validActs := map[string]string{
			"listen": "unchanged", "speak-through": "preserve", "answer": "take",
			"interrupt": "take", "act-silently": "unchanged",
			"keep-speaking": "unchanged", "stop-speaking": "yield",
		}
		floor, valid := validActs[strings.TrimSpace(message.Act)]
		if !valid {
			return fmt.Errorf("interaction act %q is not in the protocol vocabulary", message.Act)
		}
		if message.Floor != floor {
			return fmt.Errorf("interaction act %q requires floor %q, got %q", message.Act, floor, message.Floor)
		}
		if strings.TrimSpace(message.Policy) == "" || strings.TrimSpace(message.EvidenceRef) == "" {
			return errors.New("an interaction act requires policy and evidence identities")
		}
		if message.DeadlineMS <= 0 {
			return errors.New("an interaction act requires a positive deadline")
		}
		if message.Confidence < 0 || message.Confidence > 1 {
			return errors.New("interaction confidence must be between zero and one")
		}
	case TypeError:
		if strings.TrimSpace(message.Message()) == "" {
			return errors.New("an error requires a message")
		}
	case TypeElementFrame:
		return validateElementFrame(message)
	case TypeCommit, TypeRespond, TypeInterrupt, TypeBye, TypeSpeechStarted, TypeSpeechStopped,
		TypeTextDone, TypeTurnDone, TypeLog, TypeToolsUpdate:
	default:
		return fmt.Errorf("unknown sidecar message type %q", message.Type)
	}
	return nil
}

// Message returns an error frame's human-readable text.
func (message Message) Message() string {
	if message.Text != "" {
		return message.Text
	}
	return message.Error
}

// Has reports whether a ready frame declared a capability.
func (message Message) Has(capability Capability) bool {
	for _, declared := range message.Capabilities {
		if declared == string(capability) {
			return true
		}
	}
	return false
}
