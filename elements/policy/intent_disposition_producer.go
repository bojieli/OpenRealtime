package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	intentDispositionProducerRuntimeID       = "builtin://openrealtime/elements/policy.IntentDispositionProducer"
	intentDispositionProducerRuntimeRevision = "implementation:1"

	defaultIntentDispositionEvidenceBytes = 64 << 10
	defaultIntentDispositionMediaBytes    = 16 << 20
	defaultIntentDispositionMediaItems    = 1
	defaultIntentDispositionPending       = 64
	defaultIntentDispositionTerminal      = 512
	defaultIntentDispositionCancelMemory  = 256

	maximumIntentDispositionEvidenceBytes = 1 << 20
	maximumIntentDispositionMediaBytes    = 64 << 20
	maximumIntentDispositionMediaItems    = 16
	maximumIntentDispositionPending       = 4096
	maximumIntentDispositionTerminal      = 65536
	maximumIntentDispositionCancelMemory  = 4096
)

var (
	intentDispositionProducerStateType = element.State(
		element.Named("policy.IntentDispositionProducerState"),
	)
	intentDispositionProducerOutcomeType = element.Event(
		element.Named("policy.IntentDispositionProducerOutcome"),
	)
	intentDispositionProducerResolutionType = element.State(
		element.Named("policy.IntentDispositionProducerResolution"),
	)
)

func IntentDispositionProducerStateType() element.Type {
	return intentDispositionProducerStateType.Clone()
}

func IntentDispositionProducerOutcomeType() element.Type {
	return intentDispositionProducerOutcomeType.Clone()
}

func IntentDispositionProducerResolutionType() element.Type {
	return intentDispositionProducerResolutionType.Clone()
}

// IntentDispositionProducerDescriptor is the reference semantic/vision
// classifier for IntentSettlement probes. It is deliberately a separate node
// from both continuation and settlement: an application-authoritative node may
// replace it by producing the same IntentDisposition type, while this node
// owns an independently selected narrow Decider client and lifecycle.
func IntentDispositionProducerDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "policy.IntentDispositionProducer",
		Revision:      1,
		Ports: []element.Port{
			{Name: "probe", Direction: element.Input, Type: intentSettlementProbeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "cancel", Direction: element.Input, Type: intentSettlementCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "disposition", Direction: element.Output, Type: intentDispositionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "state", Direction: element.Output, Type: intentDispositionProducerStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "outcome", Direction: element.Output, Type: intentDispositionProducerOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "resolved", Direction: element.Output, Type: intentDispositionProducerResolutionType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{"probe"}, Interrupts: []string{"cancel"},
			Outcomes:       []string{"disposition", "state", "outcome", "resolved"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/policy/intent-disposition-producer-state/v1",
		ConfigSchema: "schema://openrealtime/policy/intent-disposition-producer-config/v1",
		Dependencies: []element.Dependency{
			{Name: stateelements.TrajectoryStoreService},
			{Name: SemanticDeciderRegistryService},
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
			{Name: cognitionelements.MediaResolverService, Optional: true},
		},
		Effects: []element.Effect{{Name: "policy.intent-disposition.client", Reversible: true}},
	}
}

// IntentDispositionProducerConfig independently pins the settlement contract
// it accepts and the exact detector registration authorized to answer it.
// DirectVisualInput makes resolving every media item on the exact trigger
// observation mandatory; disabling it is an explicit text-semantic policy.
type IntentDispositionProducerConfig struct {
	ExpectedSettlement IntentSettlementConfig `json:"expected_settlement"`
	DirectVisualInput  bool                   `json:"direct_visual_input,omitempty"`
	MaxEvidenceBytes   int                    `json:"max_evidence_bytes,omitempty"`
	MaxMediaBytes      int                    `json:"max_media_bytes,omitempty"`
	MaxMediaItems      int                    `json:"max_media_items,omitempty"`
	MaxPending         int                    `json:"max_pending,omitempty"`
	TerminalMemory     int                    `json:"terminal_memory,omitempty"`
	CancelMemory       int                    `json:"cancel_memory,omitempty"`
}

type IntentDispositionProducerOutcomeKind string

const (
	IntentDispositionProducerProduced IntentDispositionProducerOutcomeKind = "produced"
	IntentDispositionProducerRefused  IntentDispositionProducerOutcomeKind = "refused"
	IntentDispositionProducerFailed   IntentDispositionProducerOutcomeKind = "failed"
	IntentDispositionProducerCanceled IntentDispositionProducerOutcomeKind = "canceled"
	IntentDispositionProducerIgnored  IntentDispositionProducerOutcomeKind = "ignored"
)

// IntentDispositionProducerOutcome is inspection-only. Control flows solely
// through the strongly typed disposition output; failures never masquerade as
// a succeeded or failed user intent.
type IntentDispositionProducerOutcome struct {
	Kind                IntentDispositionProducerOutcomeKind `json:"kind"`
	ProbeID             string                               `json:"probe_id,omitempty"`
	DurableIntentItemID string                               `json:"durable_intent_item_id,omitempty"`
	Disposition         IntentDispositionKind                `json:"disposition,omitempty"`
	DispositionItemID   string                               `json:"disposition_item_id,omitempty"`
	Code                string                               `json:"code,omitempty"`
	Message             string                               `json:"message,omitempty"`
	DecisionStartedNS   uint64                               `json:"decision_started_ns,omitempty"`
	DecisionFinishedNS  uint64                               `json:"decision_finished_ns,omitempty"`
}

func (IntentDispositionProducerOutcome) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

type IntentDispositionProducerState struct {
	Revision            uint64 `json:"revision"`
	Pending             int    `json:"pending"`
	Active              bool   `json:"active"`
	Produced            uint64 `json:"produced"`
	Continued           uint64 `json:"continued"`
	Succeeded           uint64 `json:"succeeded"`
	FailedIntent        uint64 `json:"failed_intent"`
	Indeterminate       uint64 `json:"indeterminate"`
	ProviderFailures    uint64 `json:"provider_failures"`
	DeciderUnavailable  bool   `json:"decider_unavailable"`
	Canceled            uint64 `json:"canceled"`
	Refused             uint64 `json:"refused"`
	Ignored             uint64 `json:"ignored"`
	TerminalEntries     int    `json:"terminal_entries"`
	CancellationEntries int    `json:"cancellation_entries"`
	MaxPending          int    `json:"max_pending"`
	TerminalMemory      int    `json:"terminal_memory"`
	CancellationMemory  int    `json:"cancellation_memory"`
}

func (IntentDispositionProducerState) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

// IntentDispositionProducerResolution exposes the complete immutable decider
// deployment selected by the symbolic detector reference. The compact
// Detector value is exactly what probes and dispositions carry.
type IntentDispositionProducerResolution struct {
	Detector          IntentDetectorIdentity    `json:"detector"`
	Descriptor        SemanticDeciderDescriptor `json:"descriptor"`
	DescriptorDigest  string                    `json:"descriptor_digest"`
	RegistryRevision  uint64                    `json:"registry_revision"`
	DirectVisualInput bool                      `json:"direct_visual_input"`
}

func (IntentDispositionProducerResolution) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

var (
	_ element.InspectionCauseProvider = IntentDispositionProducerOutcome{}
	_ element.InspectionCauseProvider = IntentDispositionProducerState{}
	_ element.InspectionCauseProvider = IntentDispositionProducerResolution{}
)

func decodeIntentDispositionProducerConfig(
	source json.RawMessage,
) (IntentDispositionProducerConfig, error) {
	config := IntentDispositionProducerConfig{
		MaxEvidenceBytes: defaultIntentDispositionEvidenceBytes,
		MaxMediaBytes:    defaultIntentDispositionMediaBytes,
		MaxMediaItems:    defaultIntentDispositionMediaItems,
		MaxPending:       defaultIntentDispositionPending,
		TerminalMemory:   defaultIntentDispositionTerminal,
		CancelMemory:     defaultIntentDispositionCancelMemory,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return IntentDispositionProducerConfig{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(source, &fields); err != nil {
		return IntentDispositionProducerConfig{}, err
	}
	rawSettlement, found := fields["expected_settlement"]
	if !found || bytes.Equal(bytes.TrimSpace(rawSettlement), []byte("null")) {
		return IntentDispositionProducerConfig{}, errors.New("expected_settlement is required and cannot be null")
	}
	settlement, err := decodeIntentSettlementConfig(rawSettlement)
	if err != nil {
		return IntentDispositionProducerConfig{}, fmt.Errorf("expected_settlement: %w", err)
	}
	config.ExpectedSettlement = settlement
	for _, name := range []string{
		"max_evidence_bytes", "max_media_bytes", "max_media_items",
		"max_pending", "terminal_memory", "cancel_memory",
	} {
		if raw, present := fields[name]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return IntentDispositionProducerConfig{}, fmt.Errorf("%s cannot be null", name)
		}
	}
	if config.MaxEvidenceBytes < 1 || config.MaxEvidenceBytes > maximumIntentDispositionEvidenceBytes {
		return IntentDispositionProducerConfig{}, fmt.Errorf(
			"max_evidence_bytes must be between 1 and %d", maximumIntentDispositionEvidenceBytes)
	}
	if config.MaxMediaBytes < 1 || config.MaxMediaBytes > maximumIntentDispositionMediaBytes {
		return IntentDispositionProducerConfig{}, fmt.Errorf(
			"max_media_bytes must be between 1 and %d", maximumIntentDispositionMediaBytes)
	}
	if config.MaxMediaItems < 1 || config.MaxMediaItems > maximumIntentDispositionMediaItems {
		return IntentDispositionProducerConfig{}, fmt.Errorf(
			"max_media_items must be between 1 and %d", maximumIntentDispositionMediaItems)
	}
	if config.MaxPending < 1 || config.MaxPending > maximumIntentDispositionPending {
		return IntentDispositionProducerConfig{}, fmt.Errorf(
			"max_pending must be between 1 and %d", maximumIntentDispositionPending)
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > maximumIntentDispositionTerminal {
		return IntentDispositionProducerConfig{}, fmt.Errorf(
			"terminal_memory must be between 1 and %d", maximumIntentDispositionTerminal)
	}
	if config.CancelMemory < 1 || config.CancelMemory > maximumIntentDispositionCancelMemory {
		return IntentDispositionProducerConfig{}, fmt.Errorf(
			"cancel_memory must be between 1 and %d", maximumIntentDispositionCancelMemory)
	}
	return config, nil
}
