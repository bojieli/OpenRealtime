// Package graphnative owns the Meeting Assistant's graph-native plugin
// contributions and its stable Realtime-to-graph adapter profile.
//
// The package contains no listener, UI, model endpoint, credential, or
// implementation-name switch. Deployments contribute exact provider and
// adapter plugins through graph/launch; the checked graph decides how those
// plugins are composed.
package graphnative

import (
	"github.com/bojieli/OpenRealtime/element"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	videograph "github.com/bojieli/OpenRealtime/elements/video"
)

const (
	GraphID = "meeting_assistant"
	// Provider references are stable graph contracts. Exact server profiles
	// contribute implementations for these references; they never rewrite the
	// topology or select an implementation by a cascade/Omni name switch.
	ForegroundDeploymentReference = "meeting.foreground"
	VisualProviderReference       = "meeting.visual"
	BackgroundProviderReference   = "meeting.background"
	// ForegroundCausalOrderingCapability is required because model.External
	// drains different input ports concurrently. A selected deployment must
	// buffer a Generate frame until every ContextInjection named by its causal
	// parents has been applied.
	ForegroundCausalOrderingCapability = "model.causal-input-order"
	ForegroundCausalOrderingContract   = "text.ContextInjection->cognition.Generate"

	ScreenForkConfigSchema = "schema://openrealtime/meeting/screen-fork-config/v1"
	BackgroundConfigSchema = "schema://openrealtime/meeting/background-injection-config/v1"

	screenForkRuntimeID = "plugin://openrealtime/meeting/screen-fork"
	backgroundRuntimeID = "plugin://openrealtime/meeting/background-injection"
	runtimeRevision     = "implementation:1"
)

var (
	backgroundOutcomeType = element.Event(element.Named("meeting.BackgroundInjectionOutcome"))
)

// ScreenForkDescriptor converts one exact Realtime/model video input into a
// direct foreground stream and an explicitly clocked adaptive-observation
// stream. The conversion is a graph element so direct vision is not hidden in
// an adapter callback and the observation path remains independently
// inspectable.
func ScreenForkDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "meeting.ScreenFork",
		Revision:      1,
		Ports: []element.Port{
			{Name: "video", Direction: element.Input, Type: modelelements.VideoInputType(),
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 2},
			{Name: "foreground", Direction: element.Output, Type: modelelements.VideoInputType(),
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 2},
			{Name: "source", Direction: element.Output, Type: videograph.SourceStartType(),
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "frame", Direction: element.Output, Type: videograph.InlineFrameType(),
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 2},
			{Name: "tick", Direction: element.Output, Type: videograph.TimingTickType(),
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 4},
			{Name: "end", Direction: element.Output, Type: videograph.SourceEndType(),
				Cardinality: element.One, Required: true, DefaultDepth: 4},
		},
		Reaction: element.Reaction{
			Triggers:       []string{"video"},
			Outcomes:       []string{"foreground", "source", "frame", "tick", "end"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/meeting/screen-fork-state/v1",
		ConfigSchema: ScreenForkConfigSchema,
	}
}

// BackgroundInjectionDescriptor turns a complete prepared slow-model text
// stream into an explicit foreground context injection followed by a response
// trigger. Partial, interrupted, replayed, and oversized streams never cross
// the injection boundary.
func BackgroundInjectionDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "meeting.BackgroundInjection",
		Revision:      1,
		Ports: []element.Port{
			{Name: "text", Direction: element.Input, Type: modelelements.PreparedTextType(),
				Cardinality: element.One, Required: true, DefaultDepth: 64},
			{Name: "injection", Direction: element.Output, Type: modelelements.TextInputType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "trigger", Direction: element.Output, Type: modelelements.GenerateType(),
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "outcome", Direction: element.Output, Type: backgroundOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
		},
		Reaction: element.Reaction{
			Triggers:       []string{"text"},
			Outcomes:       []string{"injection", "trigger", "outcome"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/meeting/background-injection-state/v1",
		ConfigSchema: BackgroundConfigSchema,
	}
}

func BackgroundOutcomeType() element.Type { return backgroundOutcomeType.Clone() }

// ContextInjection is the provider-neutral payload carried by
// text.ContextInjection. External foreground plugins may enrich how it is
// rendered, but may not reinterpret its role or source identity.
type ContextInjection struct {
	Role   string `json:"role"`
	Source string `json:"source"`
	RunID  string `json:"run_id"`
	Text   string `json:"text"`
}

type BackgroundOutcomeKind string

const (
	BackgroundInjected    BackgroundOutcomeKind = "injected"
	BackgroundInterrupted BackgroundOutcomeKind = "interrupted"
	BackgroundRejected    BackgroundOutcomeKind = "rejected"
	BackgroundIgnored     BackgroundOutcomeKind = "ignored"
)

// BackgroundInjectionOutcome is bounded audit evidence for every terminal
// background stream. It deliberately carries no generated text.
type BackgroundInjectionOutcome struct {
	Kind    BackgroundOutcomeKind `json:"kind"`
	RunID   string                `json:"run_id,omitempty"`
	Bytes   int                   `json:"bytes,omitempty"`
	Code    string                `json:"code,omitempty"`
	Message string                `json:"message,omitempty"`
}
