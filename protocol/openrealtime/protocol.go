// Package openrealtime implements the OpenRealtime Protocol, version 1.
//
// The OpenAI Realtime API is large and mature, so the additions here are few,
// orthogonal, and strictly compatible. Five principles hold:
//
//  1. Strict superset. Every valid Realtime GA session is a valid OpenRealtime
//     session. A voice-only client written against the base API works here
//     with no changes and no awareness that the extension exists.
//  2. Namespaced. New events are prefixed "openrealtime."; new fields live
//     under an "openrealtime" key inside existing objects. No existing event
//     changes shape and no existing field changes meaning.
//  3. Negotiated, never assumed. Extensions activate only after the client
//     declares support and the server confirms. Absent negotiation, behaviour
//     is exactly the base protocol - which makes backward compatibility a
//     property of the negotiation rather than of client discipline.
//  4. Minimal. Three events and two object extensions, on a protocol with 66
//     wire names.
//  5. Actions are not new protocol. Computer use rides existing function
//     calling; the result of an action is the next screen, and the screen
//     already arrives through the video stream.
//
// The protocol has no abbreviation. The wire namespace is "openrealtime.", the
// project is OpenRealtime, and the protocol is the OpenRealtime Protocol - one
// name to learn rather than a name plus an acronym.
package openrealtime

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// Version is the protocol version this implementation speaks. It lives inside
// the "openrealtime" key, so it carries the number and nothing else:
// repeating the name inside a field already named for it would be noise.
const Version = 1

// Feature is a negotiable capability.
type Feature string

const (
	// FeatureVideoInput enables the two video events.
	FeatureVideoInput Feature = "video.input"
	// FeatureObservations enables the outbound observation event.
	FeatureObservations Feature = "observations"
	// FeatureComputerUse enables the computer.* tool namespace. It adds no
	// events at all; it is negotiated so a client can discover whether the
	// server will honour the namespace before declaring the tools.
	FeatureComputerUse Feature = "computer_use"
	// FeatureClientEffects enables server-authorized client/host-owned tool
	// execution. Negotiation alone grants nothing: every emitted call also
	// carries a short-lived receipt bound to its exact session, call,
	// declaration, arguments, and target.
	FeatureClientEffects Feature = "client.effects"
)

// DebugCategory names one subsystem in the optional developer trace stream.
//
// Debugging is deliberately configured separately from Features. The three
// portable v1 capabilities describe application behaviour; the debug stream
// describes this implementation and is useful precisely because it can name
// implementation details such as its acoustic gate and cognition phases.
type DebugCategory string

const (
	DebugVAD       DebugCategory = "vad"
	DebugASR       DebugCategory = "asr"
	DebugVideo     DebugCategory = "video"
	DebugCognition DebugCategory = "cognition"
	DebugPolicy    DebugCategory = "policy"
	DebugTTS       DebugCategory = "tts"
	DebugTool      DebugCategory = "tool"
	DebugSession   DebugCategory = "session"
	DebugError     DebugCategory = "error"
	// DebugGraph carries element-graph execution traces. Graph-native
	// deployments were already producing these and naming them "graph", which
	// no client could select, so every one of them was dropped by the category
	// filter on the way out. A category a client cannot ask for is an emitter
	// that does not exist.
	DebugGraph DebugCategory = "graph"
)

// DebugCategories is every category this implementation may emit.
func DebugCategories() []DebugCategory {
	return []DebugCategory{
		DebugVAD, DebugASR, DebugVideo, DebugCognition, DebugPolicy,
		DebugTTS, DebugTool, DebugSession, DebugError, DebugGraph,
	}
}

// DebugRequest opts a developer client into timestamped implementation
// events. Payloads are separate because transcripts, tool arguments, and tool
// results may contain secrets; a client has to ask for them explicitly.
type DebugRequest struct {
	Enabled         bool            `json:"enabled"`
	Categories      []DebugCategory `json:"categories,omitempty"`
	IncludePayloads bool            `json:"include_payloads,omitempty"`
}

// DebugResponse states the trace configuration actually in force.
type DebugResponse struct {
	Enabled             bool            `json:"enabled"`
	Categories          []DebugCategory `json:"categories,omitempty"`
	IncludePayloads     bool            `json:"include_payloads,omitempty"`
	TimestampResolution string          `json:"timestamp_resolution"`
	// Inspection is an ephemeral, session-bound capability for the read-only
	// management snapshot. It is returned only when session debugging was
	// explicitly negotiated and the mounted runtime actually exports live
	// graph evidence. The token belongs in a header, never a URL.
	Inspection *InspectionAccess `json:"inspection,omitempty"`
}

// InspectionAccess grants one client read-only access to the exact graph
// mount backing its current session. SessionID remains human-operable and may
// be guessable; Token is the unguessable authority bound to it.
type InspectionAccess struct {
	SessionID   string `json:"session_id"`
	Path        string `json:"path"`
	Token       string `json:"token"`
	ExpiresAtMS int64  `json:"expires_at_ms"`
}

// Features is every capability this implementation can offer.
func Features() []Feature {
	return []Feature{
		FeatureVideoInput, FeatureObservations, FeatureComputerUse, FeatureClientEffects,
	}
}

// ParseFeature validates a declared capability name.
func ParseFeature(value string) (Feature, error) {
	feature := Feature(strings.TrimSpace(value))
	if slices.Contains(Features(), feature) {
		return feature, nil
	}
	return "", fmt.Errorf("unknown openrealtime feature %q", value)
}

// Event type names. The three of them are the entire wire addition.
const (
	// EventVideoSourceUpdate declares or updates a video source. It is
	// required before frames, and it exists mainly for geometry: a click at
	// (x, y) is meaningless without the coordinate space the model saw.
	EventVideoSourceUpdate = "openrealtime.input_video_source.update"
	// EventVideoFrameAppend carries one frame.
	EventVideoFrameAppend = "openrealtime.input_video_frame.append"
	// EventObservationAdded reports what an observer perceived. Observations
	// are already committed to the trajectory; this event exists so a client
	// can display and audit them, and it is purely outbound.
	EventObservationAdded = "openrealtime.observation.added"
	// EventDebug is an opt-in developer event. It is not an application
	// capability and is never emitted unless session.openrealtime.debug was
	// explicitly enabled.
	EventDebug = "openrealtime.debug.event"
)

// Request is what a client declares inside session.update.
type Request struct {
	Version  int       `json:"version"`
	Supports []Feature `json:"supports,omitempty"`
	// Observers selects the session's perception, by observer name. Empty
	// selects the binding's documented default set.
	//
	// It lives inside the extension object that already exists rather than
	// becoming a fourth event, so the wire surface is unchanged: three events
	// and two extended objects. And it is per session rather than per
	// deployment because that is what makes the observer set a measurable
	// factor - two sessions against one server can differ in exactly this and
	// nothing else, which is the shape every cell of the matrix needs.
	Observers []string `json:"observers,omitempty"`
	// Debug opts this session into implementation traces. Nil means no trace
	// and preserves the v1 wire surface for every existing client.
	Debug *DebugRequest `json:"debug,omitempty"`
}

// Response is what the server echoes inside session.updated.
//
// A base-protocol server ignores the unknown key and never echoes it, so the
// client sees no enabled list and stays voice-only. A base-protocol client
// sends nothing and gets an ordinary session.
type Response struct {
	Version int       `json:"version"`
	Enabled []Feature `json:"enabled"`
	Video   *Limits   `json:"video,omitempty"`
	// Observers is the perception this session will actually run. It is
	// answered rather than echoed: a client that asked for an observer this
	// deployment does not have gets a working session and a list it can read,
	// exactly as it does for a capability it asked for and did not get.
	Observers []string `json:"observers,omitempty"`
	// AvailableObservers is everything this deployment could select from, so a
	// client can choose without guessing at names.
	AvailableObservers []string       `json:"available_observers,omitempty"`
	Debug              *DebugResponse `json:"debug,omitempty"`
}

// Limits are the server's declared bounds on video input. They are stated at
// negotiation so a client can conform rather than discover them by being
// rejected.
type Limits struct {
	Format        string `json:"format"`
	FPSCap        int    `json:"fps_cap"`
	MaxDimension  int    `json:"max_dimension"`
	MaxFrameBytes int    `json:"max_frame_bytes,omitempty"`
}

// DefaultLimits are the shipped bounds: baseline JPEG, three frames a second,
// and 1280 on the long edge.
func DefaultLimits() Limits {
	return Limits{Format: "jpeg", FPSCap: 3, MaxDimension: 1280, MaxFrameBytes: 4 << 20}
}

// frameEnvelopeBytes allows for everything in a frame event that is not the
// image: the type, the source name, the timestamp, and the JSON around them.
// It is generous because being wrong in this direction costs a few kilobytes
// of headroom, and being wrong in the other closes a connection.
const frameEnvelopeBytes = 1 << 12

// MaxTransportBytes is the largest wire message a legal frame event occupies:
// the image base64-encoded, plus the envelope around it.
//
// It exists because a transport that bounds an inbound message below this
// number does not reject an oversized frame, it drops the connection - the
// bound is enforced before any of the protocol's own validation runs, so the
// client gets a closed socket where it should have got an error it could act
// on. Every transport that carries this protocol sizes its own limit from
// here rather than from a constant that has to be kept in step by hand.
func (limits Limits) MaxTransportBytes() int {
	if limits.MaxFrameBytes <= 0 {
		return 0
	}
	return base64.StdEncoding.EncodedLen(limits.MaxFrameBytes) + frameEnvelopeBytes
}

// Negotiate resolves what a session will actually run.
//
// A capability the server cannot provide is simply absent from the enabled
// list rather than being an error: a client that asked for video against a
// binding with no video observer gets a working voice session and can see that
// video is not enabled.
func Negotiate(request Request, supported []Feature) (Response, error) {
	return NegotiateWithLimits(request, supported, DefaultLimits())
}

// NegotiateWithLimits resolves a session against a deployment's own video
// bounds rather than the shipped ones.
//
// A deployment that carries larger frames than the default has to advertise
// the number it will actually accept: a client conforms to what negotiation
// told it, so a limit that lives anywhere other than the negotiated response
// is a limit the client discovers by being rejected.
func NegotiateWithLimits(request Request, supported []Feature, limits Limits) (Response, error) {
	return NegotiateSession(request, supported, limits, nil)
}

// NegotiateSession resolves capabilities and perception together.
//
// Observers are resolved the same way capabilities are: what the deployment
// has and the client asked for is enabled, what it asked for and the
// deployment does not have is absent from the answer rather than fatal. A
// client that names nothing gets the binding's default set, which is every
// observer the deployment configured.
//
// Asking for observers where there are none is fatal, not empty. A binding
// that offers no selectable perception - which is every binding whose model
// owns its own - cannot honour a selection, and answering one back would
// confirm a set that nothing will ever act on. The client would believe it
// had turned something on.
func NegotiateSession(
	request Request, supported []Feature, limits Limits, available []string,
) (Response, error) {
	if request.Version != Version {
		return Response{}, fmt.Errorf("unsupported openrealtime version %d, this server speaks %d", request.Version, Version)
	}
	enabled := make([]Feature, 0, len(request.Supports))
	for _, declared := range request.Supports {
		feature, err := ParseFeature(string(declared))
		if err != nil {
			return Response{}, err
		}
		if slices.Contains(supported, feature) && !slices.Contains(enabled, feature) {
			enabled = append(enabled, feature)
		}
	}
	response := Response{Version: Version, Enabled: enabled}
	if request.Debug != nil {
		debug, err := negotiateDebug(*request.Debug)
		if err != nil {
			return Response{}, err
		}
		response.Debug = &debug
	}
	if len(available) > 0 {
		response.AvailableObservers = slices.Clone(available)
		response.Observers = slices.Clone(available)
	}
	if len(request.Observers) > 0 {
		selected := make([]string, 0, len(request.Observers))
		for _, name := range request.Observers {
			name = strings.TrimSpace(name)
			if name == "" || slices.Contains(selected, name) {
				continue
			}
			if !slices.Contains(available, name) {
				continue
			}
			selected = append(selected, name)
		}
		if len(selected) == 0 {
			if len(available) == 0 {
				return Response{}, fmt.Errorf(
					"this binding has no selectable observers, so %q cannot be enabled",
					strings.Join(request.Observers, ", "))
			}
			return Response{}, fmt.Errorf(
				"none of the requested observers exist here (available: %s)",
				strings.Join(available, ", "))
		}
		response.Observers = selected
	}
	if slices.Contains(enabled, FeatureVideoInput) {
		declared := limits
		if declared.MaxFrameBytes <= 0 {
			declared.MaxFrameBytes = DefaultLimits().MaxFrameBytes
		}
		response.Video = &declared
	}
	return response, nil
}

func negotiateDebug(request DebugRequest) (DebugResponse, error) {
	response := DebugResponse{
		Enabled: request.Enabled, IncludePayloads: request.Enabled && request.IncludePayloads,
		TimestampResolution: "milliseconds",
	}
	if !request.Enabled {
		return response, nil
	}
	if len(request.Categories) == 0 {
		response.Categories = DebugCategories()
		return response, nil
	}
	for _, declared := range request.Categories {
		category := DebugCategory(strings.TrimSpace(string(declared)))
		if !slices.Contains(DebugCategories(), category) {
			return DebugResponse{}, fmt.Errorf("unknown openrealtime debug category %q", declared)
		}
		if !slices.Contains(response.Categories, category) {
			response.Categories = append(response.Categories, category)
		}
	}
	return response, nil
}

// DebugEvent is one timestamped implementation trace entry. Attributes are
// safe operational metadata. Payload is present only when the client opted in
// to potentially sensitive content.
type DebugEvent struct {
	Type          string         `json:"type"`
	EventID       string         `json:"event_id"`
	TimestampMS   int64          `json:"timestamp_ms"`
	Category      DebugCategory  `json:"category"`
	Name          string         `json:"name"`
	Phase         string         `json:"phase,omitempty"`
	DurationMS    float64        `json:"duration_ms,omitempty"`
	CorrelationID string         `json:"correlation_id,omitempty"`
	Message       string         `json:"message,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
	Payload       map[string]any `json:"payload,omitempty"`
}

// SourceState is the lifecycle of a declared video source.
type SourceState string

const (
	SourceActive SourceState = "active"
	SourcePaused SourceState = "paused"
	SourceClosed SourceState = "closed"
)

// VideoSourceUpdate declares or updates a source.
type VideoSourceUpdate struct {
	Type string `json:"type"`
	// Source is mandatory because screen and camera are simultaneously live
	// and semantically different: you act on the screen, you observe the
	// camera. "screen", "camera", or an opaque client-declared identifier.
	Source string      `json:"source"`
	State  SourceState `json:"state"`
	Width  int         `json:"width"`
	Height int         `json:"height"`
}

// Validate rejects a declaration that could not ground a coordinate.
func (update VideoSourceUpdate) Validate() error {
	if strings.TrimSpace(update.Source) == "" {
		return errors.New("a video source declaration requires a source")
	}
	switch update.State {
	case SourceActive, SourcePaused:
		if update.Width <= 0 || update.Height <= 0 {
			return errors.New("an active video source requires positive width and height")
		}
	case SourceClosed:
	default:
		return fmt.Errorf("video source state must be active, paused, or closed, got %q", update.State)
	}
	return nil
}

// VideoFrameAppend carries one frame.
type VideoFrameAppend struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	// Frame is base64 of the encoded image.
	Frame       string `json:"frame"`
	TimestampMS int64  `json:"timestamp_ms,omitempty"`
}

// Decode validates the frame and returns its bytes.
//
// The gate that decides which frames matter runs on the server, always. That
// is not a performance trade - it is the product boundary. Selective
// perception is what this system is for, and a client that had to implement
// pixel-change gating at a specific threshold is a client nobody writes.
func (append VideoFrameAppend) Decode(limits Limits) ([]byte, error) {
	if strings.TrimSpace(append.Source) == "" {
		return nil, errors.New("a video frame requires a source")
	}
	if strings.TrimSpace(append.Frame) == "" {
		return nil, errors.New("a video frame requires image data")
	}
	payload, err := base64.StdEncoding.DecodeString(append.Frame)
	if err != nil {
		return nil, errors.New("video frame data is not valid base64")
	}
	if limits.MaxFrameBytes > 0 && len(payload) > limits.MaxFrameBytes {
		return nil, fmt.Errorf("video frame of %d bytes exceeds the %d byte limit", len(payload), limits.MaxFrameBytes)
	}
	return payload, nil
}

// ObservationAdded reports one committed observation.
type ObservationAdded struct {
	Type          string `json:"type"`
	EventID       string `json:"event_id"`
	ObservationID string `json:"observation_id"`
	// Observer names which observer produced it: "video", "audio", or an
	// opaque identifier.
	Observer string `json:"observer"`
	Source   string `json:"source,omitempty"`
	Text     string `json:"text"`
	ItemID   string `json:"item_id,omitempty"`
	// Authority is the provenance that decides what the text may do. Content
	// an observer read off the world is data forever and can never become an
	// instruction, and stating that on the wire lets a client show it.
	Authority   string `json:"authority"`
	TimestampMS int64  `json:"timestamp_ms,omitempty"`
}

// ToolExtension is the additive field on a tool definition.
//
// There is no reversibility class. Every output - speech, text, tool call,
// click - is irreversible once emitted, so the runtime does not grade them; it
// applies one commit boundary to all of them. Confirm is a developer's
// declaration about consequence, not an inference the runtime makes, and it
// applies to any tool rather than only to computer use.
type ToolExtension struct {
	Confirm string `json:"confirm,omitempty"`
	// Target names the declared context an action applies to - a browser
	// context or a virtual display, never an ambient desktop by default.
	Target string `json:"target,omitempty"`
	// Background declares that the call may start even if user speech newer
	// than the model prefix has arrived. The action plane still applies normal
	// authority and confirmation checks.
	Background bool `json:"background,omitempty"`
	// ClientEffect commits a host-owned declaration to this ordinary function
	// tool. It is inert unless client.effects was explicitly negotiated, and it
	// never carries authority: only the server's emitted call extension does.
	ClientEffect *ClientEffectDeclaration `json:"client_effect,omitempty"`
}

// ClientEffectDeclaration is the additive extension on a function tool. Its
// digest is computed by the independently mounted effect host and commits the
// schema, consequence policy, target, channel, and permission resource.
type ClientEffectDeclaration struct {
	Version           int    `json:"version"`
	DeclarationDigest string `json:"declaration_digest"`
}

// Validate rejects a declaration the server could not bind exactly.
func (declaration ClientEffectDeclaration) Validate() error {
	if declaration.Version != Version {
		return fmt.Errorf("client-effect declaration version must be %d", Version)
	}
	if !validSHA256Digest(declaration.DeclarationDigest) {
		return errors.New("client-effect declaration requires a canonical sha256 digest")
	}
	return nil
}

// ClientEffectCall is the server-only extension on
// response.function_call_arguments.done. Authority is an opaque, bounded,
// short-lived receipt; clients forward it to the effect host unchanged.
type ClientEffectCall struct {
	Version           int    `json:"version"`
	DeclarationDigest string `json:"declaration_digest"`
	Authority         string `json:"authority"`
}

// Validate rejects a call extension that could not be forwarded as one
// canonical receipt. Cryptographic validation belongs to the host provider.
func (call ClientEffectCall) Validate() error {
	if err := (ClientEffectDeclaration{
		Version: call.Version, DeclarationDigest: call.DeclarationDigest,
	}).Validate(); err != nil {
		return err
	}
	if call.Authority == "" || call.Authority != strings.TrimSpace(call.Authority) ||
		len(call.Authority) > 4096 || !utf8.ValidString(call.Authority) {
		return errors.New("client-effect call requires one canonical bounded authority receipt")
	}
	for _, character := range call.Authority {
		if character < 0x21 || character == 0x7f {
			return errors.New("client-effect authority contains whitespace or control characters")
		}
	}
	return nil
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
