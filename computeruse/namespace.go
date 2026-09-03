// Package computeruse defines the standard computer-use action vocabulary.
//
// It is the most portable piece of this project's design. It depends on
// nothing except function calling, which every Realtime implementation already
// has, so it is proposed as a specification in its own right rather than
// documented as ours - another implementation can adopt the vocabulary without
// adopting anything else here.
//
// Actions add no protocol. The result of an action is the next screen, and the
// screen already arrives through the video stream, so a tool output stays text
// and the visual consequence flows back through perception exactly as it does
// for a human. No image is ever carried in a function result.
package computeruse

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Prefix is the namespace every action name carries.
const Prefix = "computer."

// Action names.
const (
	Click = "computer.click"
	// ClickNormalized uses the 0..1000 coordinate convention emitted by many
	// vision-language action models, independent of the captured frame size.
	ClickNormalized = "computer.click_normalized"
	ClickElement    = "computer.click_element"
	DoubleClick     = "computer.double_click"
	Move            = "computer.move"
	Drag            = "computer.drag"
	Type            = "computer.type"
	Key             = "computer.key"
	Scroll          = "computer.scroll"
	Screenshot      = "computer.screenshot"
	Wait            = "computer.wait"
)

// Names lists the vocabulary in specification order.
func Names() []string {
	return []string{Click, ClickNormalized, ClickElement, DoubleClick, Move, Drag, Type, Key, Scroll, Screenshot, Wait}
}

// IsAction reports whether a tool name is in the namespace.
func IsAction(name string) bool { return strings.HasPrefix(name, Prefix) }

// IsReflexAction reports whether name is an exact standard action that can
// usefully execute against the current frame. Screenshot and wait are
// observation-control operations: admitting either to a low-latency lane lets
// the model discard current evidence or deliberately sleep through a
// transient cue. They remain ordinary computer-use tools for the reasoning
// lane.
func IsReflexAction(name string) bool {
	_, standard := Lookup(name)
	return standard && name != Screenshot && name != Wait
}

// Definition is one action's schema and its default consequence declaration.
type Definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	// DefaultConfirm is what a deployment gets if it declares nothing. It is a
	// starting point a developer overrides, never an inference the runtime
	// makes about a particular call.
	DefaultConfirm action.Confirm `json:"default_confirm"`
}

// sourceProperty is repeated by every action that touches a screen: an action
// is meaningless without the coordinate space the model actually saw.
const sourceProperty = `"source":{"type":"string","description":"the declared video source this action targets"}`

// sourcePropertyFor is the same field, narrowed to the sources a target
// actually owns.
//
// Without it the model is told to name "the declared video source" and never
// told what the declared video sources are called, so it has to recover the
// name from prose - and it gets it wrong in a way that looks like a model
// failure and is a schema failure. A real run produced source "video browser",
// which is the observer name and the source name run together, taken from an
// observation that had honestly reported both. Every deployment is exposed to
// that, because every deployment names its sources something.
//
// An enum removes the guess. It is a narrowing of an existing field for a
// target whose sources are known, changes nothing on the wire, and is what
// makes "an action names a declared source" a thing the schema states rather
// than a thing the prose asks for.
func sourcePropertyFor(sources []string) string {
	encoded, err := json.Marshal(sources)
	if err != nil {
		return sourceProperty
	}
	return fmt.Sprintf(
		`"source":{"type":"string","enum":%s,"description":"the declared video source this action targets"}`,
		encoded)
}

// coordinate renders one coordinate axis. An extent of zero means that the
// vocabulary is being published without a deployment target; a positive
// extent narrows the schema to the exact coordinate space the model sees.
func coordinate(name, description string, extent int) string {
	maximum := ""
	if extent > 0 {
		maximum = fmt.Sprintf(`,"maximum":%d`, extent-1)
	}
	return fmt.Sprintf(`%q:{"type":"integer","minimum":0%s,"description":%q}`,
		name, maximum, description)
}

// Definitions returns the complete vocabulary.
//
// The schemas are strict - additionalProperties false, required fields listed -
// because an action with a misread argument is an action on the wrong thing,
// and a permissive schema turns that into a silent failure.
func Definitions() []Definition { return definitions(sourceProperty, 0, 0) }

// DefinitionsFor returns the vocabulary with the source field narrowed to the
// sources this target owns, which is what a deployment declaring tools should
// use. Definitions stays the target-free form, for publishing the schemas and
// for a caller that has no target yet.
func DefinitionsFor(target Target) ([]Definition, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	return definitions(sourcePropertyFor(target.Sources), target.Width, target.Height), nil
}

func definitions(sourceProperty string, width, height int) []Definition {
	object := func(properties, required string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(
			`{"type":"object","properties":{%s},"required":[%s],"additionalProperties":false}`,
			properties, required))
	}
	return []Definition{
		{
			Name:        Click,
			Description: "Click a point on a declared video source.",
			Parameters: object(
				sourceProperty+","+
					coordinate("x", "horizontal pixel from the left edge", width)+","+
					coordinate("y", "vertical pixel from the top edge", height)+","+
					`"button":{"type":"string","enum":["left","right","middle"],"description":"mouse button, left by default"}`,
				`"source","x","y"`),
			DefaultConfirm: action.ConfirmPolicy,
		},
		{
			Name: ClickNormalized,
			Description: "Click a point on a declared video source using normalized vision-model coordinates: " +
				"0 is the left/top edge and 1000 is the right/bottom edge, independent of frame size.",
			Parameters: object(
				sourceProperty+","+
					coordinate("x", "normalized horizontal coordinate: 0 left, 1000 right", 1001)+","+
					coordinate("y", "normalized vertical coordinate: 0 top, 1000 bottom", 1001)+","+
					`"button":{"type":"string","enum":["left","right","middle"],"description":"mouse button, left by default"}`,
				`"source","x","y"`),
			DefaultConfirm: action.ConfirmPolicy,
		},
		{
			Name: ClickElement,
			Description: "Click an element by the visible set-of-mark label on a declared video source. " +
				"Use this instead of pixel coordinates only when the current frame displays numbered marks.",
			Parameters: object(
				sourceProperty+`,"element_id":{"type":"string","pattern":"^[0-9]+$","description":"red numeric mark label exactly as shown in the current frame; never use button text"}`,
				`"source","element_id"`),
			DefaultConfirm: action.ConfirmPolicy,
		},
		{
			Name:        DoubleClick,
			Description: "Double-click a point on a declared video source.",
			Parameters: object(
				sourceProperty+","+coordinate("x", "horizontal pixel from the left edge", width)+","+
					coordinate("y", "vertical pixel from the top edge", height),
				`"source","x","y"`),
			DefaultConfirm: action.ConfirmPolicy,
		},
		{
			Name:        Move,
			Description: "Move the pointer to a point on a declared video source without clicking.",
			Parameters: object(
				sourceProperty+","+coordinate("x", "horizontal pixel from the left edge", width)+","+
					coordinate("y", "vertical pixel from the top edge", height),
				`"source","x","y"`),
			DefaultConfirm: action.ConfirmNever,
		},
		{
			Name:        Drag,
			Description: "Press at one point, move to another, and release.",
			Parameters: object(
				sourceProperty+","+
					coordinate("from_x", "starting horizontal pixel", width)+","+
					coordinate("from_y", "starting vertical pixel", height)+","+
					coordinate("to_x", "ending horizontal pixel", width)+","+
					coordinate("to_y", "ending vertical pixel", height),
				`"source","from_x","from_y","to_x","to_y"`),
			DefaultConfirm: action.ConfirmPolicy,
		},
		{
			Name: Type,
			Description: "Type an exact final character sequence into whatever currently has focus. " +
				"For a dictated code or identifier, ordinary spoken words and digits stay intact, while " +
				"spoken punctuation names denote their characters; for example, bravo dash nine becomes bravo-9.",
			Parameters: object(
				sourceProperty+`,"text":{"type":"string","description":"final characters to type, not a verbatim speech transcript; encode explicitly dictated punctuation names as punctuation characters"}`,
				`"source","text"`),
			DefaultConfirm: action.ConfirmPolicy,
		},
		{
			Name:        Key,
			Description: "Press a key combination.",
			Parameters: object(
				sourceProperty+`,"keys":{"type":"array","items":{"type":"string"},"minItems":1,"description":"key names pressed together, such as [\"ctrl\",\"s\"]"}`,
				`"source","keys"`),
			DefaultConfirm: action.ConfirmPolicy,
		},
		{
			Name:        Scroll,
			Description: "Scroll at a point on a declared video source.",
			Parameters: object(
				sourceProperty+","+
					coordinate("x", "horizontal pixel from the left edge", width)+","+
					coordinate("y", "vertical pixel from the top edge", height)+","+
					`"delta_x":{"type":"integer","description":"horizontal scroll amount"},`+
					`"delta_y":{"type":"integer","description":"vertical scroll amount"}`,
				`"source","x","y"`),
			DefaultConfirm: action.ConfirmNever,
		},
		{
			Name:           Screenshot,
			Description:    "Request a fresh frame from a declared video source.",
			Parameters:     object(sourceProperty, `"source"`),
			DefaultConfirm: action.ConfirmNever,
		},
		{
			Name:        Wait,
			Description: "Wait for the screen to settle before observing again.",
			Parameters: object(
				`"duration_ms":{"type":"integer","minimum":0,"maximum":10000,"description":"how long to wait"}`,
				`"duration_ms"`),
			DefaultConfirm: action.ConfirmNever,
		},
	}
}

// Lookup returns one definition.
func Lookup(name string) (Definition, bool) {
	for _, definition := range Definitions() {
		if definition.Name == name {
			return definition, true
		}
	}
	return Definition{}, false
}

// Target is a declared context an action may act on.
//
// Blast radius is bounded by construction: an action names a video source, and
// a source is bound to a target that a deployment declared - a browser context
// or a virtual display, never an ambient desktop by default.
type Target struct {
	// Name identifies the context, and is what appears in a confirmation
	// prompt and in the audit record.
	Name string `json:"name"`
	// Sources are the declared video sources this target owns.
	Sources []string `json:"sources"`
	// Width and Height are the coordinate space, used to reject an action that
	// lands outside the screen the model saw.
	Width  int `json:"width"`
	Height int `json:"height"`
}

// Validate rejects an unusable target.
func (target Target) Validate() error {
	if strings.TrimSpace(target.Name) == "" {
		return errors.New("a computer-use target requires a name")
	}
	if len(target.Sources) == 0 {
		return fmt.Errorf("target %q declares no video sources", target.Name)
	}
	if target.Width <= 0 || target.Height <= 0 {
		return fmt.Errorf("target %q requires a positive coordinate space", target.Name)
	}
	return nil
}

// Owns reports whether this target owns a source.
func (target Target) Owns(source string) bool { return slices.Contains(target.Sources, source) }

// Specs renders the vocabulary as declared tools for one target.
//
// A deployment overrides the confirmation requirements it wants; these are the
// defaults, and they err toward asking. An unattended deployment that declares
// no confirmer refuses everything above `never`, which is the correct failure
// for an action nobody can authorize.
// TargetPolicy answers the "policy" confirmation requirement for a declared
// target.
//
// This is what "policy" means here, and it is worth being exact about, because
// the alternative was worse than it looked. Every clicking, typing, and
// scrolling action in the namespace declares "policy" by default; with no
// policy supplied the requirement reads as "always", and with no confirmer
// supplied "always" denies - so a deployment that turned computer use on got
// an agent that could move the pointer and take screenshots and could never
// press anything. A capability that cannot be exercised is not a safe
// capability, it is a broken one.
//
// The fence that actually bounds the blast radius is the declared target: an
// action names a video source, the target owns a fixed set of sources, and the
// dispatcher refuses anything outside them. So the policy admits an action
// that lands inside the declared context and nothing else. A deployment that
// wants a human in the loop declares "always" and supplies a confirmer, which
// is a different requirement rather than a stricter reading of this one.
func TargetPolicy(target Target) action.PolicyDecision {
	sources := slices.Clone(target.Sources)
	return func(call trajectory.ToolCall) bool {
		if !strings.HasPrefix(call.Name, "computer.") {
			return false
		}
		var arguments struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
			return false
		}
		source := strings.TrimSpace(arguments.Source)
		if source == "" {
			// Only computer.wait takes no source, and it changes nothing.
			return call.Name == Wait
		}
		return slices.Contains(sources, source)
	}
}

func Specs(target Target, dispatcher action.Dispatcher, overrides map[string]action.Confirm) ([]action.ToolSpec, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	if dispatcher == nil {
		return nil, errors.New("computer-use tools require a dispatcher")
	}
	declared, err := DefinitionsFor(target)
	if err != nil {
		return nil, err
	}
	specs := make([]action.ToolSpec, 0, len(declared))
	for _, definition := range declared {
		confirm := definition.DefaultConfirm
		if override, exists := overrides[definition.Name]; exists {
			parsed, err := action.ParseConfirm(string(override))
			if err != nil {
				return nil, fmt.Errorf("tool %q: %w", definition.Name, err)
			}
			confirm = parsed
		}
		specs = append(specs, action.ToolSpec{
			Name: definition.Name, Description: definition.Description,
			Parameters: slices.Clone(definition.Parameters), Confirm: confirm,
			Target: target.Name, Dispatcher: dispatcher,
		})
	}
	return specs, nil
}
