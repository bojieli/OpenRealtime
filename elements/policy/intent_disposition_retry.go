package policy

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	intentDispositionRetryRuntimeID       = "builtin://openrealtime/elements/policy.IntentDispositionRetry"
	intentDispositionRetryRuntimeRevision = "implementation:1"

	// IntentDispositionRetrySchedulerService lets deterministic simulations
	// drive the same graph-visible retry deadlines as production. A mount that
	// does not provide it receives a lifecycle-owned system scheduler.
	IntentDispositionRetrySchedulerService = "policy.intent-disposition-retry.scheduler"

	defaultIntentDispositionRetryInitialDelayMS = 100
	defaultIntentDispositionRetryBackoffFactor  = 2
	defaultIntentDispositionRetryMaxDelayMS     = 1_000
	defaultIntentDispositionRetryMaxRetries     = 3
	defaultIntentDispositionRetryMaxElapsedMS   = 5_000
	defaultIntentDispositionRetryMaxPending     = 64
	defaultIntentDispositionRetryTerminalMemory = 512
	defaultIntentDispositionRetryCancelMemory   = 256

	maximumIntentDispositionRetryDelayMS   = 600_000
	maximumIntentDispositionRetryBackoff   = 16
	maximumIntentDispositionRetryRetries   = 64
	maximumIntentDispositionRetryElapsedMS = 3_600_000
	maximumIntentDispositionRetryPending   = 4_096
	maximumIntentDispositionRetryTerminal  = 65_536
	maximumIntentDispositionRetryCancel    = 4_096
)

var (
	intentDispositionRetryExhaustedType = element.Event(
		element.Named("policy.IntentDispositionRetryExhausted"),
	)
	intentDispositionRetryStateType = element.State(
		element.Named("policy.IntentDispositionRetryState"),
	)
	intentDispositionRetryOutcomeType = element.Event(
		element.Named("policy.IntentDispositionRetryOutcome"),
	)
)

func IntentDispositionRetryExhaustedType() element.Type {
	return intentDispositionRetryExhaustedType.Clone()
}

func IntentDispositionRetryStateType() element.Type {
	return intentDispositionRetryStateType.Clone()
}

func IntentDispositionRetryOutcomeType() element.Type {
	return intentDispositionRetryOutcomeType.Clone()
}

// IntentDispositionRetryDescriptor is an explicit graph-owned retry policy for
// an indeterminate settlement classification. It forwards the first immutable
// probe immediately and can replay only that exact envelope after a bounded,
// inspectable delay. It never classifies evidence and never changes the probe.
// A developer can omit or replace this node without changing either the
// settlement gate or the disposition producer.
func IntentDispositionRetryDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "policy.IntentDispositionRetry",
		Revision:      1,
		Ports: []element.Port{
			{Name: "probe", Direction: element.Input, Type: intentSettlementProbeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "disposition", Direction: element.Input, Type: intentDispositionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "cancel", Direction: element.Input, Type: intentSettlementCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "reset", Direction: element.Input, Type: intentSettlementResetType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "attempt", Direction: element.Output, Type: intentSettlementProbeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "forwarded_cancel", Direction: element.Output, Type: intentSettlementCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "forwarded_reset", Direction: element.Output, Type: intentSettlementResetType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "exhausted", Direction: element.Output, Type: intentDispositionRetryExhaustedType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "state", Direction: element.Output, Type: intentDispositionRetryStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "outcome", Direction: element.Output, Type: intentDispositionRetryOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers:   []string{"probe", "disposition"},
			Interrupts: []string{"cancel", "reset"},
			Outcomes: []string{
				"attempt", "forwarded_cancel", "forwarded_reset", "exhausted", "state", "outcome",
			},
			MaxConcurrency: 1,
			BreaksCycles:   true,
		},
		StateSchema:  "schema://openrealtime/policy/intent-disposition-retry-state/v1",
		ConfigSchema: "schema://openrealtime/policy/intent-disposition-retry-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
			{Name: IntentDispositionRetrySchedulerService, Optional: true},
		},
		Effects: []element.Effect{{Name: "policy.intent-disposition-retry.timer", Reversible: true}},
	}
}

// IntentDispositionRetryConfig is deterministic by construction: delays use
// an integer factor and no hidden jitter. MaxRetries counts retries after the
// initial attempt. MaxElapsedMS bounds the retry window beginning when the
// policy accepts the original probe; the producer's initial call remains
// independently bounded by its selected semantic-decider descriptor.
type IntentDispositionRetryConfig struct {
	InitialDelayMS int `json:"initial_delay_ms,omitempty"`
	BackoffFactor  int `json:"backoff_factor,omitempty"`
	MaxDelayMS     int `json:"max_delay_ms,omitempty"`
	MaxRetries     int `json:"max_retries,omitempty"`
	MaxElapsedMS   int `json:"max_elapsed_ms,omitempty"`
	MaxPending     int `json:"max_pending,omitempty"`
	TerminalMemory int `json:"terminal_memory,omitempty"`
	CancelMemory   int `json:"cancel_memory,omitempty"`
}

type IntentDispositionRetryOutcomeKind string

const (
	IntentDispositionRetryObserved  IntentDispositionRetryOutcomeKind = "observed"
	IntentDispositionRetryScheduled IntentDispositionRetryOutcomeKind = "scheduled"
	IntentDispositionRetryEmitted   IntentDispositionRetryOutcomeKind = "retried"
	IntentDispositionRetrySettled   IntentDispositionRetryOutcomeKind = "settled"
	IntentDispositionRetryExhausted IntentDispositionRetryOutcomeKind = "exhausted"
	IntentDispositionRetryCanceled  IntentDispositionRetryOutcomeKind = "canceled"
	IntentDispositionRetryReset     IntentDispositionRetryOutcomeKind = "reset"
	IntentDispositionRetryIgnored   IntentDispositionRetryOutcomeKind = "ignored"
	IntentDispositionRetryRefused   IntentDispositionRetryOutcomeKind = "refused"
)

// IntentDispositionRetryOutcome makes trigger timing, attempts, cancellation,
// and exhaustion visible without using an inspection event as control.
type IntentDispositionRetryOutcome struct {
	Kind                IntentDispositionRetryOutcomeKind `json:"kind"`
	SessionID           string                            `json:"session_id,omitempty"`
	ProbeID             string                            `json:"probe_id,omitempty"`
	DurableIntentItemID string                            `json:"durable_intent_item_id,omitempty"`
	Disposition         IntentDispositionKind             `json:"disposition,omitempty"`
	DispositionItemID   string                            `json:"disposition_item_id,omitempty"`
	Retry               int                               `json:"retry,omitempty"`
	DelayMS             int                               `json:"delay_ms,omitempty"`
	DeadlineNS          uint64                            `json:"deadline_ns,omitempty"`
	FinishedNS          uint64                            `json:"finished_ns"`
	Code                string                            `json:"code,omitempty"`
	Message             string                            `json:"message,omitempty"`
}

func (IntentDispositionRetryOutcome) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

// IntentDispositionRetryExhaustion is a typed control-plane result a developer
// may route to a fallback, escalation, user prompt, or explicit Drop. It does
// not silently reinterpret uncertainty as success or failure.
type IntentDispositionRetryExhaustion struct {
	Probe                 IntentSettlementProbe `json:"probe"`
	Retries               int                   `json:"retries"`
	FirstObservedNS       uint64                `json:"first_observed_ns"`
	FinishedNS            uint64                `json:"finished_ns"`
	LastDispositionItemID string                `json:"last_disposition_item_id,omitempty"`
	Code                  string                `json:"code"`
}

func (IntentDispositionRetryExhaustion) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

type IntentDispositionRetryState struct {
	Revision            uint64 `json:"revision"`
	Pending             int    `json:"pending"`
	Armed               int    `json:"armed"`
	Observed            uint64 `json:"observed"`
	Indeterminate       uint64 `json:"indeterminate"`
	Scheduled           uint64 `json:"scheduled"`
	Retried             uint64 `json:"retried"`
	Settled             uint64 `json:"settled"`
	Exhausted           uint64 `json:"exhausted"`
	Canceled            uint64 `json:"canceled"`
	Reset               uint64 `json:"reset"`
	Ignored             uint64 `json:"ignored"`
	Refused             uint64 `json:"refused"`
	EarliestDeadlineNS  uint64 `json:"earliest_deadline_ns,omitempty"`
	MaxPending          int    `json:"max_pending"`
	MaxRetries          int    `json:"max_retries"`
	TerminalEntries     int    `json:"terminal_entries"`
	CancellationEntries int    `json:"cancellation_entries"`
}

func (IntentDispositionRetryState) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

var (
	_ element.InspectionCauseProvider = IntentDispositionRetryOutcome{}
	_ element.InspectionCauseProvider = IntentDispositionRetryExhaustion{}
	_ element.InspectionCauseProvider = IntentDispositionRetryState{}
)

func decodeIntentDispositionRetryConfig(source json.RawMessage) (IntentDispositionRetryConfig, error) {
	config := IntentDispositionRetryConfig{
		InitialDelayMS: defaultIntentDispositionRetryInitialDelayMS,
		BackoffFactor:  defaultIntentDispositionRetryBackoffFactor,
		MaxDelayMS:     defaultIntentDispositionRetryMaxDelayMS,
		MaxRetries:     defaultIntentDispositionRetryMaxRetries,
		MaxElapsedMS:   defaultIntentDispositionRetryMaxElapsedMS,
		MaxPending:     defaultIntentDispositionRetryMaxPending,
		TerminalMemory: defaultIntentDispositionRetryTerminalMemory,
		CancelMemory:   defaultIntentDispositionRetryCancelMemory,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return IntentDispositionRetryConfig{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(source, &fields); err != nil {
		return IntentDispositionRetryConfig{}, err
	}
	for _, name := range []string{
		"initial_delay_ms", "backoff_factor", "max_delay_ms", "max_retries",
		"max_elapsed_ms", "max_pending", "terminal_memory", "cancel_memory",
	} {
		if raw, found := fields[name]; found && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return IntentDispositionRetryConfig{}, fmt.Errorf("%s cannot be null", name)
		}
	}
	if config.InitialDelayMS < 1 || config.InitialDelayMS > maximumIntentDispositionRetryDelayMS {
		return IntentDispositionRetryConfig{}, fmt.Errorf(
			"initial_delay_ms must be between 1 and %d", maximumIntentDispositionRetryDelayMS,
		)
	}
	if config.BackoffFactor < 1 || config.BackoffFactor > maximumIntentDispositionRetryBackoff {
		return IntentDispositionRetryConfig{}, fmt.Errorf(
			"backoff_factor must be between 1 and %d", maximumIntentDispositionRetryBackoff,
		)
	}
	if config.MaxDelayMS < config.InitialDelayMS || config.MaxDelayMS > maximumIntentDispositionRetryDelayMS {
		return IntentDispositionRetryConfig{}, fmt.Errorf(
			"max_delay_ms must be between initial_delay_ms and %d", maximumIntentDispositionRetryDelayMS,
		)
	}
	if config.MaxRetries < 1 || config.MaxRetries > maximumIntentDispositionRetryRetries {
		return IntentDispositionRetryConfig{}, fmt.Errorf(
			"max_retries must be between 1 and %d", maximumIntentDispositionRetryRetries,
		)
	}
	if config.MaxElapsedMS < config.InitialDelayMS || config.MaxElapsedMS > maximumIntentDispositionRetryElapsedMS {
		return IntentDispositionRetryConfig{}, fmt.Errorf(
			"max_elapsed_ms must be between initial_delay_ms and %d", maximumIntentDispositionRetryElapsedMS,
		)
	}
	if config.MaxPending < 1 || config.MaxPending > maximumIntentDispositionRetryPending {
		return IntentDispositionRetryConfig{}, fmt.Errorf(
			"max_pending must be between 1 and %d", maximumIntentDispositionRetryPending,
		)
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > maximumIntentDispositionRetryTerminal {
		return IntentDispositionRetryConfig{}, fmt.Errorf(
			"terminal_memory must be between 1 and %d", maximumIntentDispositionRetryTerminal,
		)
	}
	if config.CancelMemory < 1 || config.CancelMemory > maximumIntentDispositionRetryCancel {
		return IntentDispositionRetryConfig{}, fmt.Errorf(
			"cancel_memory must be between 1 and %d", maximumIntentDispositionRetryCancel,
		)
	}
	return config, nil
}

type intentDispositionRetryFactory struct{}

var (
	_ element.Factory         = intentDispositionRetryFactory{}
	_ element.ConfigValidator = intentDispositionRetryFactory{}
)

func (intentDispositionRetryFactory) Descriptor() element.Descriptor {
	return IntentDispositionRetryDescriptor()
}

func (intentDispositionRetryFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeIntentDispositionRetryConfig(source)
	return err
}

func intentDispositionRetryRegistration() (factoryprofile.Entry, error) {
	descriptor := IntentDispositionRetryDescriptor()
	if err := descriptor.Validate(); err != nil {
		return factoryprofile.Entry{}, err
	}
	return factoryprofile.Entry{
		Factory: intentDispositionRetryFactory{},
		Artifact: inspect.ArtifactIdentity{
			ID: intentDispositionRetryRuntimeID, Revision: intentDispositionRetryRuntimeRevision,
		},
	}, nil
}
