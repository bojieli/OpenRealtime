package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	defaultIntentSettlementTrackedIntents = 64
	maximumIntentSettlementTrackedIntents = 4096
	defaultIntentSettlementCancellations  = 256
	maximumIntentSettlementCancellations  = 4096
	maximumIntentSettlementCausalParents  = 64
)

var (
	intentSettlementProbeType    = element.Event(element.Named("policy.IntentSettlementProbe"))
	intentDispositionType        = element.Event(element.Named("policy.IntentDisposition"))
	intentSettlementResetType    = element.Interrupt(element.Named("policy.IntentSettlementAddress"))
	intentSettlementCancelType   = element.Interrupt(element.Named("policy.IntentSettlementCancellation"))
	intentSettlementDecisionType = element.Event(element.Named("policy.IntentSettlementDecision"))
	intentSettlementAckType      = element.Event(element.Named("policy.IntentSettlementAcknowledgement"))
	intentSettlementStateType    = element.State(element.Named("policy.IntentSettlementState"))
	intentSettlementOutcomeType  = element.Event(element.Named("policy.IntentSettlementOutcome"))
)

func IntentSettlementProbeType() element.Type { return intentSettlementProbeType.Clone() }
func IntentDispositionType() element.Type     { return intentDispositionType.Clone() }
func IntentSettlementResetType() element.Type { return intentSettlementResetType.Clone() }
func IntentSettlementCancelType() element.Type {
	return intentSettlementCancelType.Clone()
}
func IntentSettlementDecisionType() element.Type {
	return intentSettlementDecisionType.Clone()
}
func IntentSettlementAcknowledgementType() element.Type { return intentSettlementAckType.Clone() }
func IntentSettlementStateType() element.Type           { return intentSettlementStateType.Clone() }
func IntentSettlementOutcomeType() element.Type         { return intentSettlementOutcomeType.Clone() }

// IntentSettlementDescriptor gates post-effect observation evidence until an
// independently connected, deployment-selected detector classifies the exact
// resulting state. It does not infer task completion from a successful tool
// call, visual quiet, or the absence of another model proposal.
func IntentSettlementDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "policy.IntentSettlement",
		Revision:      1,
		Ports: []element.Port{
			{Name: "evidence", Direction: element.Input, Type: temporalEvidenceAdmittedType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "disposition", Direction: element.Input, Type: intentDispositionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "ack", Direction: element.Input, Type: intentSettlementAckType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "reset", Direction: element.Input, Type: intentSettlementResetType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "cancel", Direction: element.Input, Type: intentSettlementCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "admitted", Direction: element.Output, Type: temporalEvidenceAdmittedType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "probe", Direction: element.Output, Type: intentSettlementProbeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "terminal", Direction: element.Output, Type: intentSettlementDecisionType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "state", Direction: element.Output, Type: intentSettlementStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "outcome", Direction: element.Output, Type: intentSettlementOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers:   []string{"evidence", "disposition", "ack"},
			Interrupts: []string{"reset", "cancel"},
			Outcomes: []string{
				"admitted", "probe", "terminal", "state", "outcome",
			},
			MaxConcurrency: 1,
			BreaksCycles:   true,
		},
		StateSchema:  "schema://openrealtime/policy/intent-settlement-state/v1",
		ConfigSchema: "schema://openrealtime/policy/intent-settlement-config/v1",
		Dependencies: []element.Dependency{
			{Name: stateelements.TrajectoryStoreService},
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
		},
		Effects: []element.Effect{{Name: "policy.intent-settlement.memory", Reversible: true}},
	}
}

// IntentDetectorIdentity pins the exact replaceable producer authorized to
// classify settlement probes. Reference identifies the graph/application
// registration, while Revision and ConfigurationDigest bind its behavior.
// Credentials and provider-specific settings remain outside the payload.
type IntentDetectorIdentity struct {
	Reference           string `json:"reference"`
	Revision            string `json:"revision"`
	ConfigurationDigest string `json:"configuration_digest"`
}

// IntentSettlementConfig independently pins both the upstream temporal
// admission contract and the observation sources on which task-settlement
// classification is meaningful. CandidateSources is generic: a computer-use
// graph may select a screen observer while another graph may select a device
// or application-state observer.
type IntentSettlementConfig struct {
	ExpectedAdmission TemporalEvidenceAdmissionConfig `json:"expected_admission"`
	CandidateSources  []TemporalEvidenceRequirement   `json:"candidate_sources"`
	Detector          IntentDetectorIdentity          `json:"detector"`
	MaxTrackedIntents int                             `json:"max_tracked_intents,omitempty"`
	CancelMemory      int                             `json:"cancel_memory,omitempty"`
}

// IntentSettlementResultIdentity names the exact successful canonical tool
// result that precedes a candidate observation. Prefix binds its complete
// payload, so this identity need not copy result bytes into the control plane.
type IntentSettlementResultIdentity struct {
	TrajectoryItemID string `json:"trajectory_item_id"`
	StoreVersion     uint64 `json:"store_version"`
	InvocationID     string `json:"invocation_id"`
	CallID           string `json:"call_id"`
	Tool             string `json:"tool"`
}

// IntentSettlementProbe is content-free, deterministic classification work.
// Evidence carries the complete typed admission (still only identities) so a
// producer can independently reopen and verify it before resolving the exact
// canonical observation through its separately declared data/service path.
// A disposition must return this probe byte-for-byte.
type IntentSettlementProbe struct {
	ProbeID            string                         `json:"probe_id"`
	Issuer             string                         `json:"issuer"`
	Sequence           uint64                         `json:"sequence"`
	SessionID          string                         `json:"session_id"`
	Evidence           AdmittedTemporalEvidence       `json:"evidence"`
	DurableIntent      TemporalEvidenceItemIdentity   `json:"durable_intent"`
	TriggerObservation TemporalEvidenceItemIdentity   `json:"trigger_observation"`
	Result             IntentSettlementResultIdentity `json:"result"`
	Prefix             trajectory.PrefixIdentity      `json:"prefix"`
	Detector           IntentDetectorIdentity         `json:"detector"`
	IssuedNS           uint64                         `json:"issued_ns"`
}

type IntentDispositionKind string

const (
	IntentDispositionContinue      IntentDispositionKind = "continue"
	IntentDispositionSucceeded     IntentDispositionKind = "succeeded"
	IntentDispositionFailed        IntentDispositionKind = "failed"
	IntentDispositionIndeterminate IntentDispositionKind = "indeterminate"
)

// IntentDisposition is the only input that can make an intent terminal. The
// exact echoed Probe prevents a typed but stale or cross-result classification
// from settling different work. Decision times are retained for replay and
// must be positive, ordered, and no earlier than the probe.
type IntentDisposition struct {
	Probe              IntentSettlementProbe  `json:"probe"`
	Detector           IntentDetectorIdentity `json:"detector"`
	Kind               IntentDispositionKind  `json:"kind"`
	DecisionStartedNS  uint64                 `json:"decision_started_ns"`
	DecisionFinishedNS uint64                 `json:"decision_finished_ns"`
}

// IntentSettlementDecision is the gate's authoritative terminal output. It
// carries the exact held admission and accepted disposition so a downstream
// activation policy can independently re-attest the canonical prefix and
// clear the one matching effect. Inspection outcomes are not used as control.
type IntentSettlementDecision struct {
	TerminalID          string                        `json:"terminal_id"`
	Kind                IntentSettlementDecisionKind  `json:"kind"`
	SessionID           string                        `json:"session_id"`
	Evidence            AdmittedTemporalEvidence      `json:"evidence"`
	Probe               IntentSettlementProbe         `json:"probe"`
	Disposition         *IntentDisposition            `json:"disposition,omitempty"`
	Cancellation        *IntentSettlementCancellation `json:"cancellation,omitempty"`
	Reset               *IntentSettlementAddress      `json:"reset,omitempty"`
	SupersedingIntent   *TemporalEvidenceItemIdentity `json:"superseding_intent,omitempty"`
	InvocationID        string                        `json:"invocation_id"`
	StateRevisionBefore uint64                        `json:"state_revision_before"`
	StateRevisionAfter  uint64                        `json:"state_revision_after"`
	FinishedNS          uint64                        `json:"finished_ns"`
}

type IntentSettlementDecisionKind string

const (
	IntentSettlementDecisionSucceeded  IntentSettlementDecisionKind = "succeeded"
	IntentSettlementDecisionFailed     IntentSettlementDecisionKind = "failed"
	IntentSettlementDecisionSuperseded IntentSettlementDecisionKind = "superseded"
	IntentSettlementDecisionReset      IntentSettlementDecisionKind = "reset"
	IntentSettlementDecisionCanceled   IntentSettlementDecisionKind = "canceled"
)

// IntentSettlementAcknowledgement is emitted by the downstream activation
// only after it independently verifies Decision and atomically clears the
// matching active effect. Echoing the complete decision prevents an ack for a
// different result, intent, or terminal kind from releasing the gate.
type IntentSettlementAcknowledgement struct {
	Decision       IntentSettlementDecision `json:"decision"`
	GenerationID   string                   `json:"generation_id"`
	AcknowledgedNS uint64                   `json:"acknowledged_ns"`
}

// IntentSettlementAddress is an exact administrative/user-policy reset. Both
// the payload and envelope name the session, and the full durable-intent
// identity prevents a delayed reset from reopening a newer epoch.
type IntentSettlementAddress struct {
	SessionID     string                       `json:"session_id"`
	DurableIntent TemporalEvidenceItemIdentity `json:"durable_intent"`
	Reason        string                       `json:"reason"`
}

// IntentSettlementCancellation revokes one exact durable-intent epoch. It is
// intentionally distinct from GenerationCancel: model generation and media
// stream addresses cannot safely identify an intent, and reusing them would
// allow a session interrupt to race a newer canonical intent. A graph that
// starts with a protocol-level session cancel must explicitly translate it at
// a stateful policy boundary that knows this complete canonical identity.
type IntentSettlementCancellation struct {
	SessionID     string                       `json:"session_id"`
	DurableIntent TemporalEvidenceItemIdentity `json:"durable_intent"`
	Reason        string                       `json:"reason,omitempty"`
}

type IntentSettlementOutcomeKind string

const (
	IntentSettlementAdmitted   IntentSettlementOutcomeKind = "admitted"
	IntentSettlementHeld       IntentSettlementOutcomeKind = "held"
	IntentSettlementSuppressed IntentSettlementOutcomeKind = "suppressed"
	IntentSettlementRefused    IntentSettlementOutcomeKind = "refused"
	IntentSettlementReset      IntentSettlementOutcomeKind = "reset"
	IntentSettlementCanceled   IntentSettlementOutcomeKind = "canceled"
	IntentSettlementIgnored    IntentSettlementOutcomeKind = "ignored"
)

// IntentSettlementOutcome contains only identities and closed decisions. The
// before/after revisions make every state transition reconstructible without
// rerunning a possibly nondeterministic detector.
type IntentSettlementOutcome struct {
	Kind                     IntentSettlementOutcomeKind `json:"kind"`
	Operation                string                      `json:"operation"`
	SessionID                string                      `json:"session_id,omitempty"`
	DurableIntentItemID      string                      `json:"durable_intent_item_id,omitempty"`
	TriggerObservationItemID string                      `json:"trigger_observation_item_id,omitempty"`
	ResultItemID             string                      `json:"result_item_id,omitempty"`
	ProbeID                  string                      `json:"probe_id,omitempty"`
	Disposition              IntentDispositionKind       `json:"disposition,omitempty"`
	Code                     string                      `json:"code,omitempty"`
	Message                  string                      `json:"message,omitempty"`
	StateRevisionBefore      uint64                      `json:"state_revision_before"`
	StateRevisionAfter       uint64                      `json:"state_revision_after"`
	FinishedNS               uint64                      `json:"finished_ns"`
}

func (IntentSettlementOutcome) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

// IntentSettlementState is a bounded, content-free operational projection.
// Internal exact identities remain in bounded records and are exposed on
// probes/outcomes rather than copied into a lossy latest-state lane.
type IntentSettlementState struct {
	Revision               uint64 `json:"revision"`
	TrackedIntents         int    `json:"tracked_intents"`
	PendingIntents         int    `json:"pending_intents"`
	TerminalIntents        int    `json:"terminal_intents"`
	AcknowledgedTerminals  int    `json:"acknowledged_terminals"`
	Admitted               uint64 `json:"admitted"`
	Held                   uint64 `json:"held"`
	Suppressed             uint64 `json:"suppressed"`
	Refused                uint64 `json:"refused"`
	Indeterminate          uint64 `json:"indeterminate"`
	Resets                 uint64 `json:"resets"`
	CancellationRequests   uint64 `json:"cancellation_requests"`
	CancellationsCompleted uint64 `json:"cancellations_completed"`
	MaxTrackedIntents      int    `json:"max_tracked_intents"`
	CancellationEntries    int    `json:"cancellation_entries"`
	CancellationMemory     int    `json:"cancellation_memory"`
	Saturated              bool   `json:"saturated"`
}

func (IntentSettlementState) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

var (
	_ element.InspectionCauseProvider = IntentSettlementOutcome{}
	_ element.InspectionCauseProvider = IntentSettlementState{}
)

func decodeIntentSettlementConfig(source json.RawMessage) (IntentSettlementConfig, error) {
	config := IntentSettlementConfig{
		MaxTrackedIntents: defaultIntentSettlementTrackedIntents,
		CancelMemory:      defaultIntentSettlementCancellations,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return IntentSettlementConfig{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(source, &fields); err != nil {
		return IntentSettlementConfig{}, err
	}
	for _, name := range []string{"max_tracked_intents", "cancel_memory"} {
		if raw, found := fields[name]; found && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return IntentSettlementConfig{}, fmt.Errorf("intent settlement %s cannot be null", name)
		}
	}
	if raw, found := fields["expected_admission"]; found {
		var expectedFields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &expectedFields); err != nil {
			return IntentSettlementConfig{}, err
		}
		if _, explicit := expectedFields["source_set"]; explicit && config.ExpectedAdmission.SourceSet == "" {
			return IntentSettlementConfig{}, errors.New(
				"intent settlement expected_admission source_set cannot be explicitly empty")
		}
		if required, found := expectedFields["required"]; found &&
			bytes.Equal(bytes.TrimSpace(required), []byte("null")) {
			return IntentSettlementConfig{}, errors.New(
				"intent settlement expected_admission required cannot be null")
		}
	}
	return normalizeIntentSettlementConfig(config)
}

func normalizeIntentSettlementConfig(config IntentSettlementConfig) (IntentSettlementConfig, error) {
	if err := ValidateTemporalEvidenceAdmissionConfig(config.ExpectedAdmission); err != nil {
		return IntentSettlementConfig{}, fmt.Errorf("expected_admission: %w", err)
	}
	if config.ExpectedAdmission.Mode != TemporalEvidenceAdmissionAfterIntent {
		return IntentSettlementConfig{}, errors.New(
			"intent settlement requires after_intent temporal evidence")
	}
	for index, requirement := range config.ExpectedAdmission.Required {
		if err := validateIntentSettlementConfigIdentifier(
			fmt.Sprintf("expected admission source %d observer", index), requirement.Observer,
		); err != nil {
			return IntentSettlementConfig{}, err
		}
		if err := validateIntentSettlementConfigIdentifier(
			fmt.Sprintf("expected admission source %d source", index), requirement.Source,
		); err != nil {
			return IntentSettlementConfig{}, err
		}
	}
	if len(config.CandidateSources) == 0 || len(config.CandidateSources) > maximumTemporalEvidenceRequirements {
		return IntentSettlementConfig{}, fmt.Errorf(
			"candidate_sources must contain between 1 and %d observer/source pairs",
			maximumTemporalEvidenceRequirements)
	}
	seen := make(map[TemporalEvidenceRequirement]struct{}, len(config.CandidateSources))
	for index, source := range config.CandidateSources {
		if err := validateIntentSettlementConfigIdentifier(
			fmt.Sprintf("candidate source %d observer", index), source.Observer,
		); err != nil {
			return IntentSettlementConfig{}, err
		}
		if err := validateIntentSettlementConfigIdentifier(
			fmt.Sprintf("candidate source %d source", index), source.Source,
		); err != nil {
			return IntentSettlementConfig{}, err
		}
		if _, duplicate := seen[source]; duplicate {
			return IntentSettlementConfig{}, fmt.Errorf(
				"candidate observer/source pair %q/%q is duplicated", source.Observer, source.Source)
		}
		seen[source] = struct{}{}
	}
	if err := validateIntentSettlementConfigIdentifier("detector reference", config.Detector.Reference); err != nil {
		return IntentSettlementConfig{}, err
	}
	if err := validateIntentSettlementConfigIdentifier("detector revision", config.Detector.Revision); err != nil {
		return IntentSettlementConfig{}, err
	}
	if !validSemanticDigest(config.Detector.ConfigurationDigest) {
		return IntentSettlementConfig{}, errors.New(
			"detector configuration_digest is not canonical SHA-256")
	}
	if config.MaxTrackedIntents < 1 ||
		config.MaxTrackedIntents > maximumIntentSettlementTrackedIntents {
		return IntentSettlementConfig{}, fmt.Errorf(
			"max_tracked_intents must be between 1 and %d",
			maximumIntentSettlementTrackedIntents)
	}
	if config.CancelMemory < 1 || config.CancelMemory > maximumIntentSettlementCancellations {
		return IntentSettlementConfig{}, fmt.Errorf(
			"cancel_memory must be between 1 and %d", maximumIntentSettlementCancellations)
	}
	config.ExpectedAdmission, _ = normalizeTemporalEvidenceAdmissionConfig(config.ExpectedAdmission)
	config.ExpectedAdmission.Required = slices.Clone(config.ExpectedAdmission.Required)
	config.CandidateSources = slices.Clone(config.CandidateSources)
	return config, nil
}

// Configuration identifiers deliberately use visible ASCII. This gives JSON
// Schema and the Go factory identical character/byte length semantics while
// still covering graph, provider, source, and revision identifiers.
func validateIntentSettlementConfigIdentifier(label, value string) error {
	if err := validatePolicyIdentifier(label, value, true); err != nil {
		return err
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '!' || value[index] > '~' {
			return fmt.Errorf("%s must contain visible ASCII only", label)
		}
	}
	return nil
}
