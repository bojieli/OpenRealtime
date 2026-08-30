// Package interaction contains graph-native composition policy between model
// preparation and externally visible action. It deliberately knows no
// privileged fast/slow roles: topology and explicit selection inputs decide
// which prepared run may advance toward speech.
package interaction

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	"github.com/bojieli/OpenRealtime/elements/speech"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

var (
	preparedTextType        = cognitionelements.PreparedTextType()
	modelCancelType         = cognitionelements.CancelType()
	modelOutcomeType        = cognitionelements.OutcomeType()
	textSegmentType         = speech.TextSegmentType()
	speechCancelType        = speech.CancelType()
	timeoutType             = element.Event(element.Named("interaction.Timeout"))
	selectionType           = element.Trigger(element.Named("interaction.SpeechSelection"))
	segmentationOutcomeType = element.Event(
		element.Named("interaction.SegmentationOutcome"),
	)
	arbitrationOutcomeType = element.Event(
		element.Named("interaction.ArbitrationOutcome"),
	)
	appendType = element.Request(
		element.Named("trajectory.Append"), element.Named("flow.RequestID"),
	)
	commitType = element.Reply(
		element.Named("trajectory.Commit"), element.Named("flow.RequestID"),
	)
	rejectionType = element.Reply(
		element.Named("trajectory.Rejection"), element.Named("flow.RequestID"),
	)
	modelCommitOutcomeType = element.Event(element.Named("interaction.ModelCommitOutcome"))
)

func PreparedTextType() element.Type        { return preparedTextType.Clone() }
func ModelCancelType() element.Type         { return modelCancelType.Clone() }
func ModelOutcomeType() element.Type        { return modelOutcomeType.Clone() }
func TextSegmentType() element.Type         { return textSegmentType.Clone() }
func SpeechCancelType() element.Type        { return speechCancelType.Clone() }
func TimeoutType() element.Type             { return timeoutType.Clone() }
func SelectionType() element.Type           { return selectionType.Clone() }
func SegmentationOutcomeType() element.Type { return segmentationOutcomeType.Clone() }
func ArbitrationOutcomeType() element.Type  { return arbitrationOutcomeType.Clone() }
func ModelCommitOutcomeType() element.Type  { return modelCommitOutcomeType.Clone() }

// SegmentPreparedTextDescriptor turns one non-interleaved prepared run into
// complete safe TTS requests. Cancellation is translated in both directions:
// to the model run and to each already-emitted speech utterance.
func SegmentPreparedTextDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "interaction.SegmentPreparedText", Revision: 1,
		Ports: []element.Port{
			{Name: "text", Direction: element.Input, Type: preparedTextType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "terminal", Direction: element.Input, Type: modelOutcomeType,
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 8},
			{Name: "timeout", Direction: element.Input, Type: timeoutType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "cancel", Direction: element.Input, Type: modelCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "segments", Direction: element.Output, Type: textSegmentType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "model_cancel", Direction: element.Output, Type: modelCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "speech_cancel", Direction: element.Output, Type: speechCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: segmentationOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
		},
		Reaction: element.Reaction{
			Triggers: []string{"text", "terminal", "timeout"}, Interrupts: []string{"cancel"},
			Outcomes:       []string{"segments", "model_cancel", "speech_cancel", "outcome"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/interaction/segment-prepared-text-state/v1",
		ConfigSchema: "schema://openrealtime/interaction/segment-prepared-text-config/v1",
	}
}

// SpeechArbiterDescriptor is a stream-aware mux. It never chooses based on
// provider phase or labels; selection/preemption is explicit control input.
func SpeechArbiterDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "interaction.SpeechArbiter", Revision: 1,
		Ports: []element.Port{
			{Name: "text", Direction: element.Input, Type: preparedTextType,
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 32},
			{Name: "terminal", Direction: element.Input, Type: modelOutcomeType,
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 16},
			{Name: "selection", Direction: element.Input, Type: selectionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "timeout", Direction: element.Input, Type: timeoutType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "cancel", Direction: element.Input, Type: modelCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "selected", Direction: element.Output, Type: preparedTextType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "cancel_upstream", Direction: element.Output, Type: modelCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: arbitrationOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers:   []string{"text", "terminal", "selection", "timeout"},
			Interrupts: []string{"cancel"},
			Outcomes:   []string{"selected", "cancel_upstream", "outcome"}, MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/interaction/speech-arbiter-state/v1",
		ConfigSchema: "schema://openrealtime/interaction/speech-arbiter-config/v1",
	}
}

// ModelResultCommitDescriptor converts a prepared cognition result into one
// version-checked trajectory transaction. Store replies remain explicit
// feedback edges; the adapter owns no canonical state.
func ModelResultCommitDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "interaction.ModelResultCommit", Revision: 1,
		Ports: []element.Port{
			{Name: "result", Direction: element.Input, Type: cognitionelements.ResultType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "committed", Direction: element.Input, Type: commitType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "rejected", Direction: element.Input, Type: rejectionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "append", Direction: element.Output, Type: appendType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: modelCommitOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
		},
		Reaction: element.Reaction{
			Triggers: []string{"result", "committed", "rejected"},
			Outcomes: []string{"append", "outcome"}, MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/interaction/model-result-commit-state/v1",
		ConfigSchema: "schema://openrealtime/interaction/model-result-commit-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName},
		},
	}
}

type Timeout struct {
	RunID  string `json:"run_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type SelectionMode string

const (
	SelectionQueue   SelectionMode = "queue"
	SelectionPreempt SelectionMode = "preempt"
)

type SpeechSelection struct {
	RunID string        `json:"run_id,omitempty"`
	Mode  SelectionMode `json:"mode,omitempty"`
}

type OutcomeKind string

const (
	OutcomeSelected  OutcomeKind = "selected"
	OutcomeQueued    OutcomeKind = "queued"
	OutcomeCompleted OutcomeKind = "completed"
	OutcomePreempted OutcomeKind = "preempted"
	OutcomeCanceled  OutcomeKind = "canceled"
	OutcomeFailed    OutcomeKind = "failed"
	OutcomeRefused   OutcomeKind = "refused"
	OutcomeIgnored   OutcomeKind = "ignored"
)

type SegmentationOutcome struct {
	Kind          OutcomeKind `json:"kind"`
	RunID         string      `json:"run_id,omitempty"`
	Segments      int         `json:"segments,omitempty"`
	BufferedBytes int         `json:"buffered_bytes,omitempty"`
	Code          string      `json:"code,omitempty"`
	Message       string      `json:"message,omitempty"`
}

type ModelCommitKind string

const (
	ModelCommitted ModelCommitKind = "committed"
	ModelRejected  ModelCommitKind = "rejected"
	ModelRefused   ModelCommitKind = "refused"
	ModelIgnored   ModelCommitKind = "ignored"
)

// ModelCommitOutcome is the policy-visible terminal result of one prepared
// result transaction. A result is not canonical until Kind is committed.
type ModelCommitOutcome struct {
	Kind         ModelCommitKind `json:"kind"`
	RunID        string          `json:"run_id,omitempty"`
	RequestID    string          `json:"request_id,omitempty"`
	StoreVersion uint64          `json:"store_version,omitempty"`
	ItemIDs      []string        `json:"item_ids,omitempty"`
	Code         string          `json:"code,omitempty"`
	Message      string          `json:"message,omitempty"`
}

type ArbitrationOutcome struct {
	Kind          OutcomeKind `json:"kind"`
	RunID         string      `json:"run_id,omitempty"`
	ReplacedRunID string      `json:"replaced_run_id,omitempty"`
	QueuePosition int         `json:"queue_position,omitempty"`
	BufferedRuns  int         `json:"buffered_runs,omitempty"`
	BufferedBytes int         `json:"buffered_bytes,omitempty"`
	Code          string      `json:"code,omitempty"`
	Message       string      `json:"message,omitempty"`
}

type SegmentPreparedTextConfig struct {
	MinimumRunes    int `json:"minimum_runes,omitempty"`
	MaxSegmentBytes int `json:"max_segment_bytes,omitempty"`
	MaxRunBytes     int `json:"max_run_bytes,omitempty"`
	MaxSegments     int `json:"max_segments,omitempty"`
	TerminalMemory  int `json:"terminal_memory,omitempty"`
}

type SpeechArbiterConfig struct {
	MaxPendingRuns    int `json:"max_pending_runs,omitempty"`
	MaxBufferedBytes  int `json:"max_buffered_bytes,omitempty"`
	MaxBufferedDeltas int `json:"max_buffered_deltas,omitempty"`
	MaxDeltaBytes     int `json:"max_delta_bytes,omitempty"`
}

type ModelResultCommitConfig struct {
	MaxPending int `json:"max_pending,omitempty"`
}

const maximumConfigurationBound = 64 << 20

func decodeSegmentConfig(source json.RawMessage) (SegmentPreparedTextConfig, error) {
	config := SegmentPreparedTextConfig{
		MinimumRunes: 12, MaxSegmentBytes: 64 << 10,
		MaxRunBytes: 1 << 20, MaxSegments: 256, TerminalMemory: 256,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return SegmentPreparedTextConfig{}, err
	}
	if config.MinimumRunes < 1 || config.MinimumRunes > 4096 {
		return SegmentPreparedTextConfig{}, errors.New("minimum_runes must be between 1 and 4096")
	}
	if config.MaxSegmentBytes < 1 || config.MaxSegmentBytes > maximumConfigurationBound {
		return SegmentPreparedTextConfig{}, fmt.Errorf("max_segment_bytes must be between 1 and %d", maximumConfigurationBound)
	}
	if config.MaxRunBytes < config.MaxSegmentBytes || config.MaxRunBytes > maximumConfigurationBound {
		return SegmentPreparedTextConfig{}, fmt.Errorf("max_run_bytes must be between max_segment_bytes and %d", maximumConfigurationBound)
	}
	if config.MaxSegments < 1 || config.MaxSegments > 4096 {
		return SegmentPreparedTextConfig{}, errors.New("max_segments must be between 1 and 4096")
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > 4096 {
		return SegmentPreparedTextConfig{}, errors.New("terminal_memory must be between 1 and 4096")
	}
	return config, nil
}

func decodeArbiterConfig(source json.RawMessage) (SpeechArbiterConfig, error) {
	config := SpeechArbiterConfig{
		MaxPendingRuns: 16, MaxBufferedBytes: 4 << 20,
		MaxBufferedDeltas: 4096, MaxDeltaBytes: 256 << 10,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return SpeechArbiterConfig{}, err
	}
	if config.MaxPendingRuns < 1 || config.MaxPendingRuns > 4096 {
		return SpeechArbiterConfig{}, errors.New("max_pending_runs must be between 1 and 4096")
	}
	if config.MaxBufferedBytes < 1 || config.MaxBufferedBytes > maximumConfigurationBound {
		return SpeechArbiterConfig{}, fmt.Errorf("max_buffered_bytes must be between 1 and %d", maximumConfigurationBound)
	}
	if config.MaxBufferedDeltas < 1 || config.MaxBufferedDeltas > 1_000_000 {
		return SpeechArbiterConfig{}, errors.New("max_buffered_deltas must be between 1 and 1000000")
	}
	if config.MaxDeltaBytes < 1 || config.MaxDeltaBytes > config.MaxBufferedBytes {
		return SpeechArbiterConfig{}, errors.New("max_delta_bytes must be between 1 and max_buffered_bytes")
	}
	return config, nil
}

func decodeCommitConfig(source json.RawMessage) (ModelResultCommitConfig, error) {
	config := ModelResultCommitConfig{MaxPending: 32}
	if err := elementconfig.Decode(source, &config); err != nil {
		return ModelResultCommitConfig{}, err
	}
	if config.MaxPending < 1 || config.MaxPending > 4096 {
		return ModelResultCommitConfig{}, errors.New("max_pending must be between 1 and 4096")
	}
	return config, nil
}

func runAddress(runID, envelopeRunID, scope string) (string, error) {
	runID = strings.TrimSpace(runID)
	envelopeRunID = strings.TrimSpace(envelopeRunID)
	if runID != "" && envelopeRunID != "" && runID != envelopeRunID {
		return "", errors.New("payload and envelope name different run IDs")
	}
	if runID != "" {
		return runID, nil
	}
	if envelopeRunID != "" {
		return envelopeRunID, nil
	}
	if scope = strings.TrimSpace(scope); scope != "" {
		return scope, nil
	}
	return "", errors.New("run ID is required")
}

func reportInteractionResolution(
	reporter element.ResolutionReporter, descriptor element.Descriptor,
) error {
	if reporter == nil {
		return fmt.Errorf("%s has no live resolution reporter", descriptor.Name)
	}
	identity, err := descriptor.Identity()
	if err != nil {
		return fmt.Errorf("resolve %s runtime identity: %w", descriptor.Name, err)
	}
	if err := reporter.Runtime(
		"go://github.com/bojieli/OpenRealtime/elements/interaction/"+descriptor.Name,
		fmt.Sprintf("descriptor:%d", descriptor.Revision), identity.Digest,
	); err != nil {
		return err
	}
	return reporter.Capabilities(nil)
}

func Descriptors() []element.Descriptor {
	return []element.Descriptor{
		SegmentPreparedTextDescriptor(), SpeechArbiterDescriptor(), ModelResultCommitDescriptor(),
		PostCommitSilenceDescriptor(),
	}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register interaction descriptors: nil catalog")
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
		return errors.New("register interaction factories: nil registry")
	}
	registrations, err := FactoryRegistrations()
	if err != nil {
		return err
	}
	for _, registration := range registrations {
		if err := registry.RegisterFactory(registration); err != nil {
			return err
		}
	}
	return nil
}

func FactoryRegistrations() ([]graphruntime.FactoryRegistration, error) {
	entries := make([]factoryprofile.Entry, 0, len(Descriptors()))
	for _, factory := range []element.Factory{
		segmentPreparedTextFactory{}, speechArbiterFactory{}, modelResultCommitFactory{},
		postCommitSilenceFactory{},
	} {
		entries = append(entries, factoryprofile.Entry{Factory: factory})
	}
	return factoryprofile.Registrations(entries...)
}
