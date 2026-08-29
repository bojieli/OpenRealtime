// Package speech implements graph-native speech synthesis and playback
// elements. Synthesis and playback are intentionally separate: generating
// audio is reversible computation, while handing a paced frame to a sink is
// an irreversible external effect.
package speech

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	TTSProviderRegistryService   = "speech.tts.providers"
	PlaybackSinkRegistryService  = "speech.playback.sinks"
	IrreversibilityLedgerService = "action.irreversibility.ledger"
	PlaybackSchedulerService     = "speech.playback.scheduler"
)

var (
	textSegmentType = element.Trigger(element.Named("speech.TextSegment"))
	cancelType      = element.Interrupt(element.Named("speech.UtteranceID"))
	audioType       = element.Segmented(
		element.Named("Prepared", element.Named("speech.AudioFrame")),
		element.Named("speech.UtteranceID"),
	)
	transitionType = element.Revisions(
		element.Named("speech.Transition"), element.Named("speech.UtteranceID"),
	)
	synthesisOutcomeType   = element.Event(element.Named("speech.SynthesisOutcome"))
	playbackOutcomeType    = element.Event(element.Named("speech.PlaybackOutcome"))
	providerResolutionType = element.State(element.Named("speech.ProviderResolution"))
	sinkResolutionType     = element.State(element.Named("speech.SinkResolution"))
)

// Public type helpers let graph boundaries construct exact envelopes without
// exporting mutable package-level Type values.
func TextSegmentType() element.Type        { return textSegmentType.Clone() }
func CancelType() element.Type             { return cancelType.Clone() }
func AudioType() element.Type              { return audioType.Clone() }
func TransitionType() element.Type         { return transitionType.Clone() }
func SynthesisOutcomeType() element.Type   { return synthesisOutcomeType.Clone() }
func PlaybackOutcomeType() element.Type    { return playbackOutcomeType.Clone() }
func ProviderResolutionType() element.Type { return providerResolutionType.Clone() }
func SinkResolutionType() element.Type     { return sinkResolutionType.Clone() }

// TextSegment is one complete, safe-to-speak unit. Segmentation policy lives
// upstream and can therefore be replaced without changing either TTS or
// playback. Phase is provenance only; no phase is privileged or forbidden.
type TextSegment struct {
	ID               string           `json:"id"`
	Text             string           `json:"text"`
	Producer         string           `json:"producer,omitempty"`
	Phase            trajectory.Phase `json:"phase,omitempty"`
	SpeechAuthority  string           `json:"speech_authority,omitempty"`
	SourceRevision   uint64           `json:"source_revision,omitempty"`
	AssistantItemIDs []string         `json:"assistant_item_ids,omitempty"`
	Continuer        bool             `json:"continuer,omitempty"`
	SpokeOver        bool             `json:"spoke_over,omitempty"`
}

func (segment TextSegment) validate(maxTextBytes int) error {
	if strings.TrimSpace(segment.ID) == "" {
		return errors.New("text segment requires an ID")
	}
	if strings.TrimSpace(segment.Text) == "" {
		return errors.New("text segment requires non-empty text")
	}
	if len(segment.Text) > maxTextBytes {
		return fmt.Errorf("text segment exceeds %d bytes", maxTextBytes)
	}
	return nil
}

func (segment TextSegment) utterance() action.Utterance {
	return action.Utterance{
		ID: segment.ID, Text: strings.TrimSpace(segment.Text),
		Phase:            segment.Phase,
		SourceRevision:   segment.SourceRevision,
		AssistantItemIDs: slices.Clone(segment.AssistantItemIDs),
		Continuer:        segment.Continuer, SpokeOver: segment.SpokeOver,
	}
}

// Cancel is an addressed interruption. An empty ID uses the envelope's
// cancellation scope; it never means "cancel every utterance" implicitly.
type Cancel struct {
	UtteranceID string `json:"utterance_id,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

type AudioFrameKind string

const (
	AudioBegin AudioFrameKind = "begin"
	AudioChunk AudioFrameKind = "chunk"
	AudioEnd   AudioFrameKind = "end"
)

// AudioFrame is the explicit framing carried by the Segmented audio port.
// Begin owns utterance metadata, Chunk owns immutable PCM, and End carries the
// synthesis terminal result. Playback never guesses completion from silence.
type AudioFrame struct {
	Kind            AudioFrameKind   `json:"kind"`
	UtteranceID     string           `json:"utterance_id"`
	Utterance       action.Utterance `json:"utterance,omitempty"`
	SpeechAuthority string           `json:"speech_authority,omitempty"`
	Chunk           v1.SpeechChunk   `json:"chunk,omitempty"`
	Terminal        SynthesisOutcome `json:"terminal,omitempty"`
}

type Stage string

const (
	StageSynthesis Stage = "synthesis"
	StagePlayback  Stage = "playback"
)

type State string

const (
	StateGenerating State = "generating"
	StateGenerated  State = "generated"
	StateQueued     State = "queued"
	StateEmitting   State = "emitting"
	StatePlayed     State = "played"
	StateCancelled  State = "cancelled"
	StateFailed     State = "failed"
	StateRefused    State = "refused"
)

// Transition makes the lifecycle visible to graph policy and inspection.
// CrossedBoundary is false for reversible cancellation and true once at least
// one audio frame may have reached the world.
type Transition struct {
	UtteranceID     string       `json:"utterance_id"`
	Stage           Stage        `json:"stage"`
	State           State        `json:"state"`
	CrossedBoundary bool         `json:"crossed_boundary"`
	PlayedNS        uint64       `json:"played_ns,omitempty"`
	Reason          string       `json:"reason,omitempty"`
	LedgerState     action.State `json:"ledger_state,omitempty"`
}

type OutcomeKind string

const (
	OutcomeSucceeded OutcomeKind = "succeeded"
	OutcomeCancelled OutcomeKind = "cancelled"
	OutcomeRefused   OutcomeKind = "refused"
	OutcomeFailed    OutcomeKind = "failed"
	OutcomeIgnored   OutcomeKind = "ignored"
)

type SynthesisOutcome struct {
	UtteranceID string      `json:"utterance_id,omitempty"`
	Kind        OutcomeKind `json:"kind"`
	Chunks      uint64      `json:"chunks,omitempty"`
	AudioBytes  uint64      `json:"audio_bytes,omitempty"`
	Code        string      `json:"code,omitempty"`
	Message     string      `json:"message,omitempty"`
}

type PlaybackOutcome struct {
	UtteranceID     string      `json:"utterance_id,omitempty"`
	Kind            OutcomeKind `json:"kind"`
	CrossedBoundary bool        `json:"crossed_boundary"`
	PlayedNS        uint64      `json:"played_ns,omitempty"`
	Code            string      `json:"code,omitempty"`
	Message         string      `json:"message,omitempty"`
}

type ProviderResolution struct {
	Reference  string          `json:"reference"`
	Descriptor v1.Descriptor   `json:"descriptor"`
	Selected   map[string]bool `json:"selected,omitempty"`
}

type SinkResolution struct {
	Reference  string          `json:"reference"`
	Descriptor v1.Descriptor   `json:"descriptor"`
	Selected   map[string]bool `json:"selected,omitempty"`
}

func TTSDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "speech.TTS", Revision: 1,
		Ports: []element.Port{
			{Name: "text", Direction: element.Input, Type: textSegmentType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "cancel", Direction: element.Input, Type: cancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "audio", Direction: element.Output, Type: audioType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "status", Direction: element.Output, Type: transitionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: synthesisOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "resolved", Direction: element.Output, Type: providerResolutionType,
				Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{"text"}, Interrupts: []string{"cancel"},
			Outcomes:       []string{"audio", "status", "outcome", "resolved"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/speech/tts-state/v1",
		ConfigSchema: "schema://openrealtime/speech/tts-config/v1",
		Dependencies: []element.Dependency{{Name: TTSProviderRegistryService}},
		Effects:      []element.Effect{{Name: "speech.tts.provider", Reversible: true}},
	}
}

func PlaybackDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "speech.Playback", Revision: 1,
		Ports: []element.Port{
			{Name: "audio", Direction: element.Input, Type: audioType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "cancel", Direction: element.Input, Type: cancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "status", Direction: element.Output, Type: transitionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: playbackOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "resolved", Direction: element.Output, Type: sinkResolutionType,
				Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{"audio"}, Interrupts: []string{"cancel"},
			Outcomes:       []string{"status", "outcome", "resolved"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/speech/playback-state/v1",
		ConfigSchema: "schema://openrealtime/speech/playback-config/v1",
		Dependencies: []element.Dependency{
			{Name: PlaybackSinkRegistryService}, {Name: IrreversibilityLedgerService},
			{Name: PlaybackSchedulerService, Optional: true},
		},
		Effects: []element.Effect{
			{Name: "speech.playback.sink", Reversible: true},
			// Playback is the commit element: Prepared audio is still
			// cancellable, and the ledger records authority immediately before
			// the first sink hand-off.
			{Name: "speech.audio.playback", External: true, Authority: "Prepared"},
		},
	}
}

func Descriptors() []element.Descriptor {
	return []element.Descriptor{TTSDescriptor(), PlaybackDescriptor()}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register speech descriptors: nil catalog")
	}
	for _, descriptor := range Descriptors() {
		if err := catalog.Register(descriptor); err != nil {
			return err
		}
	}
	return nil
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if registry == nil {
		return errors.New("register speech factories: nil registry")
	}
	for _, factory := range []element.Factory{ttsFactory{}, playbackFactory{}} {
		if err := registry.Register("", factory); err != nil {
			return err
		}
	}
	return nil
}
