package realtimecu

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	ActivationReference       = "policy.RealtimeComputerUseActivation"
	ActivationConfigSchema    = "schema://openrealtime/realtime-cu/activation-config/v2"
	activationRuntimeID       = "go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/activation/v10"
	defaultTerminalMemory     = 512
	defaultCancellationMemory = 256
	maximumDispositionRetries = 8
	maximumSettlementAckTries = 3
	maximumActivationMemory   = 1_000_000
	maximumInstructionBytes   = 1 << 20
)

// ActivationDescriptor keeps the latest committed user task as durable
// proposal authority while grounding cognition in each newest screen/camera
// prefix. Exactly one generation may be unsettled: a result with no proposal
// releases the next changed frame, while a proposed effect remains closed
// until a screen observation names its exact canonical tool result. The
// selected user item must be a canonical causal ancestor of the current visual
// tail; ProposalAdmission independently re-derives and checks that authority,
// so this element cannot weaken the shared effect contract.
func ActivationDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          ActivationReference,
		Revision:      10,
		Ports: []element.Port{
			{Name: "admitted", Direction: element.Input,
				Type: policyelements.AdmittedTemporalEvidenceType(), Cardinality: element.One,
				Required: true, DefaultDepth: 32},
			{Name: "cancel", Direction: element.Input,
				Type: policyelements.GenerationCancelType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
			{Name: "result", Direction: element.Input,
				Type: interactionelements.SafeModelResultType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
			{Name: "effect_terminal", Direction: element.Input,
				Type: actionelements.PreEffectTerminalType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
			{Name: "disposition_committed", Direction: element.Input,
				Type: stateelements.CommitType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
			{Name: "disposition_rejected", Direction: element.Input,
				Type: stateelements.RejectionType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
			{Name: "settlement", Direction: element.Input,
				Type: policyelements.IntentSettlementDecisionType(), Cardinality: element.One,
				Required: false, DefaultDepth: 16},
			{Name: "trigger", Direction: element.Output,
				Type: cognitionelements.GenerateType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
			{Name: "authority", Direction: element.Output,
				Type: authority.CandidateType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
			{Name: "state", Direction: element.Output,
				Type: policyelements.GenerationStateType(), Cardinality: element.One,
				Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "outcome", Direction: element.Output,
				Type: policyelements.GenerationOutcomeType(), Cardinality: element.One,
				Required: true, DefaultDepth: 32},
			{Name: "disposition_append", Direction: element.Output,
				Type: stateelements.AppendType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
			{Name: "settlement_ack", Direction: element.Output,
				Type: policyelements.IntentSettlementAcknowledgementType(), Cardinality: element.One,
				Required: false, DefaultDepth: 16},
		},
		Reaction: element.Reaction{
			Triggers: []string{
				"admitted", "result", "effect_terminal", "disposition_committed", "disposition_rejected",
				"settlement",
			},
			Interrupts: []string{"cancel"},
			Outcomes: []string{
				"trigger", "authority", "state", "outcome", "disposition_append",
			},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/realtime-cu/durable-activation-state/v1",
		ConfigSchema: ActivationConfigSchema,
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
			{Name: stateelements.TrajectoryStoreService},
		},
		Effects: []element.Effect{{Name: "realtime-cu.durable-user-intent.memory", Reversible: true}},
	}
}

type activationFactory struct{}

func (activationFactory) Descriptor() element.Descriptor { return ActivationDescriptor() }

func (activationFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeActivationConfig(source)
	return err
}

func (activationFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeActivationConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("%s %s config: %w", ActivationReference, mount.InstanceID, err)
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("Realtime-CU activation has no runtime clock")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("Realtime-CU activation clock has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("Realtime-CU activation has no runtime sequence allocator")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("Realtime-CU activation sequence allocator has type %T", sequenceValue)
	}
	storeValue, _, found := mount.Services.Lookup(stateelements.TrajectoryStoreService)
	if !found {
		return nil, errors.New("Realtime-CU activation has no canonical trajectory store")
	}
	storeService, ok := storeValue.(*stateelements.TrajectoryStoreServiceValue)
	if !ok || storeService == nil || storeService.Store == nil {
		return nil, fmt.Errorf("Realtime-CU activation trajectory store has type %T", storeValue)
	}
	if !boundedActivationIdentifier(storeService.SessionID, true) {
		return nil, errors.New("Realtime-CU activation trajectory store has no trusted canonical session ID")
	}
	ports, err := activationPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	if err := validateActivationSettlementWiring(
		config.ExpectedSettlement != nil,
		len(ports.settlement.Lanes()), len(ports.settlementAcknowledgement.Lanes()),
	); err != nil {
		return nil, err
	}
	return &activationRunner{
		instance: mount.InstanceID, sessionID: storeService.SessionID,
		config: config, clock: clock, sequences: sequences,
		store: storeService.Store, resolution: mount.Resolution, ports: ports,
		terminal:               make(map[string]struct{}),
		canceledEffects:        make(map[string]*canceledActivationEffect),
		pendingSettlementAcks:  make(map[string]pendingSettlementAcknowledgement),
		acknowledgedSettlement: make(map[string]struct{}),
		state: policyelements.GenerationState{
			Role: config.Role, TerminalMemory: config.TerminalMemory,
			CancellationMemory: config.CancelMemory,
		},
	}, nil
}

func validateActivationSettlementWiring(
	configured bool, settlementInputs, acknowledgementOutputs int,
) error {
	if settlementInputs < 0 || acknowledgementOutputs < 0 ||
		settlementInputs > 1 || acknowledgementOutputs > 1 {
		return errors.New("Realtime-CU activation settlement ports must each have at most one lane")
	}
	if configured {
		if settlementInputs != 1 || acknowledgementOutputs != 1 {
			return errors.New(
				"Realtime-CU activation expected_settlement requires one settlement input and one settlement_ack output lane")
		}
		return nil
	}
	if settlementInputs != 0 || acknowledgementOutputs != 0 {
		return errors.New(
			"Realtime-CU activation settlement lanes require expected_settlement configuration")
	}
	return nil
}

// ActivationConfig pins cognition parameters and the temporal admission
// contract expected at the consumer boundary. ExpectedSettlement enables the
// optional terminal handshake and independently repeats the complete upstream
// gate contract: typed payloads prove shape, while this expectation prevents a
// forged or miswired producer from weakening source, candidate, or detector
// requirements. Its nested admission contract must be semantically identical
// to ExpectedAdmission, which continues to cover ordinary and released evidence.
type ActivationConfig struct {
	policyelements.GenerateOnObservationConfig
	ExpectedAdmission  policyelements.TemporalEvidenceAdmissionConfig `json:"expected_admission"`
	ExpectedSettlement *policyelements.IntentSettlementConfig         `json:"expected_settlement,omitempty"`
}

func decodeActivationConfig(source json.RawMessage) (ActivationConfig, error) {
	config := ActivationConfig{
		GenerateOnObservationConfig: policyelements.GenerateOnObservationConfig{
			MaxPending: 64, TerminalMemory: defaultTerminalMemory,
			CancelMemory: defaultCancellationMemory,
		},
	}
	if err := decodeExactJSON(source, &config); err != nil {
		return ActivationConfig{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(source, &fields); err != nil {
		return ActivationConfig{}, err
	}
	if raw, found := fields["expected_settlement"]; found &&
		bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ActivationConfig{}, errors.New("expected_settlement cannot be null")
	}
	if !canonical(config.Role) || len(config.Role) > 256 {
		return ActivationConfig{}, errors.New("activation role must be a canonical identifier")
	}
	if config.MaxPending < 1 || config.MaxPending > maximumActivationMemory ||
		config.TerminalMemory < 1 || config.TerminalMemory > maximumActivationMemory ||
		config.CancelMemory < 1 || config.CancelMemory > maximumActivationMemory {
		return ActivationConfig{}, errors.New("activation memory bounds must be between 1 and 1000000")
	}
	if config.Invocation.SourceRevision != 0 {
		return ActivationConfig{}, errors.New("activation source revision is derived from canonical user authority")
	}
	if len(config.Invocation.Instruction) > maximumInstructionBytes ||
		!utf8.ValidString(config.Invocation.Instruction) {
		return ActivationConfig{}, errors.New("activation instruction must be bounded valid UTF-8")
	}
	if config.Invocation.MaxOutputTokens > 1_000_000 {
		return ActivationConfig{}, errors.New("activation maximum output tokens exceeds 1000000")
	}
	if config.Invocation.Effort != "" {
		parsed, err := continuation.ParseEffort(string(config.Invocation.Effort))
		if err != nil || parsed != config.Invocation.Effort {
			if err != nil {
				return ActivationConfig{}, err
			}
			return ActivationConfig{}, errors.New("activation effort is not canonical")
		}
	}
	validator := continuation.Descriptor{
		Provider: "realtime-cu-policy", Model: "config-validator",
		Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
	if err := continuation.ValidateInvocation(config.Invocation, validator); err != nil {
		return ActivationConfig{}, err
	}
	if err := policyelements.ValidateTemporalEvidenceAdmissionConfig(config.ExpectedAdmission); err != nil {
		return ActivationConfig{}, fmt.Errorf("expected_admission: %w", err)
	}
	if config.ExpectedSettlement != nil {
		if err := validateActivationSettlementConfig(*config.ExpectedSettlement); err != nil {
			return ActivationConfig{}, fmt.Errorf("expected_settlement: %w", err)
		}
		if !equalActivationAdmissionContracts(
			config.ExpectedAdmission, config.ExpectedSettlement.ExpectedAdmission,
		) {
			return ActivationConfig{}, errors.New(
				"expected_settlement admission differs from expected_admission")
		}
		copy := cloneActivationSettlementConfig(*config.ExpectedSettlement)
		config.ExpectedSettlement = &copy
	}
	config.Invocation = cloneInvocation(config.Invocation)
	return config, nil
}

func validateActivationSettlementConfig(config policyelements.IntentSettlementConfig) error {
	payload, err := json.Marshal(config)
	if err != nil {
		return err
	}
	registrations, err := policyelements.FactoryRegistrations()
	if err != nil {
		return err
	}
	reference := policyelements.IntentSettlementDescriptor().Name
	for _, registration := range registrations {
		if registration.Profile.Reference != reference {
			continue
		}
		validator, ok := registration.Factory.(element.ConfigValidator)
		if !ok {
			return errors.New("intent settlement factory has no configuration validator")
		}
		return validator.ValidateConfig(payload)
	}
	return errors.New("intent settlement factory is absent from the policy registry")
}

func equalActivationAdmissionContracts(
	left, right policyelements.TemporalEvidenceAdmissionConfig,
) bool {
	canonicalize := func(
		value policyelements.TemporalEvidenceAdmissionConfig,
	) policyelements.TemporalEvidenceAdmissionConfig {
		if value.Mode == policyelements.TemporalEvidenceAdmissionAfterIntent &&
			value.SourceSet == "" {
			value.SourceSet = policyelements.TemporalEvidenceSourceSetExplicit
		}
		value.Required = slices.Clone(value.Required)
		return value
	}
	return reflect.DeepEqual(canonicalize(left), canonicalize(right))
}

func cloneActivationSettlementConfig(
	source policyelements.IntentSettlementConfig,
) policyelements.IntentSettlementConfig {
	result := source
	result.ExpectedAdmission.Required = slices.Clone(source.ExpectedAdmission.Required)
	result.CandidateSources = slices.Clone(source.CandidateSources)
	return result
}

type activationPorts struct {
	admitted, cancel, result, effectTerminal, settlement  element.InputPort
	dispositionCommitted, dispositionRejected             element.InputPort
	trigger, candidate, state, outcome, dispositionAppend element.OutputPort
	settlementAcknowledgement                             element.OutputPort
}

func activationPortsFrom(ports element.Ports) (activationPorts, error) {
	if ports == nil {
		return activationPorts{}, errors.New("Realtime-CU activation has nil ports")
	}
	result := activationPorts{}
	for _, input := range []struct {
		name string
		set  *element.InputPort
	}{
		{"admitted", &result.admitted}, {"cancel", &result.cancel},
		{"result", &result.result}, {"effect_terminal", &result.effectTerminal},
		{"disposition_committed", &result.dispositionCommitted},
		{"disposition_rejected", &result.dispositionRejected},
		{"settlement", &result.settlement},
	} {
		port, err := ports.Input(input.name)
		if err != nil {
			return activationPorts{}, err
		}
		*input.set = port
	}
	for _, output := range []struct {
		name string
		set  *element.OutputPort
	}{
		{"trigger", &result.trigger}, {"authority", &result.candidate},
		{"state", &result.state}, {"outcome", &result.outcome},
		{"disposition_append", &result.dispositionAppend},
		{"settlement_ack", &result.settlementAcknowledgement},
	} {
		port, err := ports.Output(output.name)
		if err != nil {
			return activationPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

type userIntentBasis struct {
	itemID, triggerItemID string
	sourceRevision        uint64
	identity              policyelements.TemporalEvidenceItemIdentity
}

type activeGeneration struct {
	id             string
	contextVersion uint64
	callID         string
	tool           string
	intent         policyelements.TemporalEvidenceItemIdentity
}

type pendingActivationSettlement struct {
	envelope element.Envelope
	decision policyelements.IntentSettlementDecision
}

type canceledActivationEffect struct {
	generation activeGeneration
	settlement *pendingActivationSettlement
}

type pendingSettlementAcknowledgement struct {
	envelope   element.Envelope
	decision   policyelements.IntentSettlementDecision
	generation activeGeneration
}

// deferredVisualCommit is the newest committed observation that arrived while
// cognition or a proposed effect was awaiting terminal disposition. It is
// normally replaceable screen/camera state, but can briefly retain a newer
// user intent behind an older proposal. Capacity is deliberately one so input
// cadence cannot become an unbounded cognition queue.
type deferredVisualCommit struct {
	envelope  element.Envelope
	admission policyelements.AdmittedTemporalEvidence
	commit    stateelements.ObservationCommitOutcome
}

type pendingEffectTerminal struct {
	envelope element.Envelope
	terminal actionelements.PreEffectTerminal
}

// pendingProposalDisposition is the single graph-visible compare-and-append
// transaction that must settle before activation releases the model run. The
// canonical Store remains read-only to this element; all mutation travels over
// dispositionAppend and is trusted only after the matching typed reply.
type pendingProposalDisposition struct {
	cause           element.Envelope
	terminal        actionelements.PreEffectTerminal
	requestID       string
	expectedVersion uint64
	expectedPrefix  trajectory.PrefixIdentity
	item            trajectory.Item
	retries         uint32
}

type activationContextOverride struct {
	context stateelements.CommittedContext
	version uint64
	tailID  string
}

type verifiedTemporalAdmission struct {
	prefix             []trajectory.Item
	trigger            trajectory.Item
	intent             *userIntentBasis
	intentStoreVersion uint64
}

func verifyActivationTemporalAdmission(
	snapshot trajectory.Snapshot, admission policyelements.AdmittedTemporalEvidence,
	expected policyelements.TemporalEvidenceAdmissionConfig,
) (verifiedTemporalAdmission, error) {
	if err := policyelements.VerifyAdmittedTemporalEvidence(snapshot, admission, expected); err != nil {
		return verifiedTemporalAdmission{}, err
	}
	commit := admission.TriggerCommit
	prefix := snapshot.Items[:commit.StoreVersion]
	verified := verifiedTemporalAdmission{
		prefix: prefix, trigger: prefix[commit.StoreVersion-1],
	}
	if admission.DurableIntent != nil {
		intent := prefix[admission.DurableIntent.StoreVersion-1]
		verified.intent = &userIntentBasis{
			itemID: intent.ID, triggerItemID: intent.Event.EventID,
			sourceRevision: intent.SourceRevision, identity: *admission.DurableIntent,
		}
		verified.intentStoreVersion = admission.DurableIntent.StoreVersion
	}
	return verified, nil
}

type activationRunner struct {
	instance   string
	sessionID  string
	config     ActivationConfig
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	store      *trajectory.Store
	resolution element.ResolutionReporter
	ports      activationPorts

	intent                      *userIntentBasis
	active                      *activeGeneration
	deferred                    *deferredVisualCommit
	pendingTerminal             *pendingEffectTerminal
	pendingDisposition          *pendingProposalDisposition
	pendingSettlement           *pendingActivationSettlement
	revokedSequence             uint64
	revokedStoreVersion         uint64
	terminal                    map[string]struct{}
	terminalOrder               []string
	canceledEffects             map[string]*canceledActivationEffect
	canceledEffectOrder         []string
	pendingSettlementAcks       map[string]pendingSettlementAcknowledgement
	acknowledgedSettlement      map[string]struct{}
	acknowledgedSettlementOrder []string
	state                       policyelements.GenerationState
}

type activationInput struct {
	kind     string
	envelope element.Envelope
}

func (runner *activationRunner) Run(parent context.Context) error {
	if err := reportElementRuntime(runner.resolution, activationRuntimeID,
		"implementation:10", ActivationDescriptor()); err != nil {
		return err
	}
	if err := runner.publishState(parent, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan activationInput)
	failures := make(chan error, 7)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{
		{"admitted", runner.ports.admitted}, {"cancel", runner.ports.cancel},
		{"result", runner.ports.result}, {"effect_terminal", runner.ports.effectTerminal},
		{"disposition_committed", runner.ports.dispositionCommitted},
		{"disposition_rejected", runner.ports.dispositionRejected},
		{"settlement", runner.ports.settlement},
	} {
		if source.kind == "settlement" && len(source.port.Lanes()) == 0 {
			continue
		}
		wait.Add(1)
		go receiveActivationInputs(ctx, source.kind, source.port, inputs, failures, &wait)
	}
	defer func() {
		// Retained payloads are useful only for this mounted session. Release the
		// exact visual prefix even when shutdown races an in-flight generation.
		runner.deferred = nil
		runner.pendingTerminal = nil
		runner.pendingDisposition = nil
		runner.pendingSettlement = nil
		runner.canceledEffects = nil
		runner.pendingSettlementAcks = nil
		cancel(nil)
		wait.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "admitted":
				err = runner.acceptAdmission(ctx, input.envelope)
			case "cancel":
				err = runner.acceptCancel(ctx, input.envelope)
			case "result":
				err = runner.acceptResult(ctx, input.envelope)
			case "effect_terminal":
				err = runner.acceptEffectTerminal(ctx, input.envelope)
			case "disposition_committed":
				err = runner.acceptDispositionCommit(ctx, input.envelope)
			case "disposition_rejected":
				err = runner.acceptDispositionRejection(ctx, input.envelope)
			case "settlement":
				err = runner.acceptSettlement(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown Realtime-CU activation input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *activationRunner) acceptAdmission(
	ctx context.Context, envelope element.Envelope,
) error {
	return runner.acceptAdmissionAtContext(ctx, envelope, nil)
}

// acceptAdmissionAtContext consumes one exact temporal admission. A disposition
// replay keeps the original observation as the activation cause while sampling
// the later acknowledged prefix whose actual tail is the disposition item.
// This avoids relabelling runtime state as an observation merely to satisfy a
// tail-shaped API.
func (runner *activationRunner) acceptAdmissionAtContext(
	ctx context.Context, envelope element.Envelope, override *activationContextOverride,
) error {
	admission, ok := admittedTemporalEvidencePayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, stateelements.ObservationCommitOutcome{},
			"invalid_temporal_admission", fmt.Sprintf("temporal admission payload has type %T", envelope.Payload))
	}
	commit := admission.TriggerCommit
	if !envelope.Type.Equal(policyelements.AdmittedTemporalEvidenceType()) {
		return runner.refuse(ctx, envelope, commit, "invalid_temporal_admission",
			fmt.Sprintf("temporal admission envelope has type %s", envelope.Type.String()))
	}
	if envelope.SessionID != runner.sessionID {
		return runner.refuse(ctx, envelope, commit, "session_mismatch",
			"temporal admission crossed the mounted trajectory session")
	}
	if commit.Kind != stateelements.ObservationCommitted {
		return runner.refuse(ctx, envelope, commit, "invalid_temporal_admission",
			"temporal admission does not contain a committed observation")
	}
	if !canonical(envelope.SessionID) || !canonical(commit.TrajectoryItemID) ||
		!canonical(commit.TriggerItemID) || !canonical(commit.StreamID) ||
		!canonical(commit.Context.StateItemID) || commit.SourceRevision == 0 ||
		commit.ObservationRevision == 0 ||
		commit.StoreVersion == 0 || commit.Context.Prefix.Version != commit.StoreVersion {
		return runner.refuse(ctx, envelope, commit, "invalid_temporal_admission",
			"temporal admission omits canonical session, observation, revision, or context identity")
	}
	if envelope.Sequence != 0 && envelope.Sequence <= runner.revokedSequence {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, commit, policyelements.GenerationOutcome{
			Kind: policyelements.GenerationIgnored, Role: runner.config.Role,
			StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
			ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
			Code: "intent_revoked", Message: "observation predates the latest user cancellation",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	snapshot := runner.store.Snapshot()
	attested, err := verifyActivationTemporalAdmission(
		snapshot, admission, runner.config.ExpectedAdmission,
	)
	if err != nil {
		return runner.refuse(ctx, envelope, commit, "invalid_temporal_admission", err.Error())
	}
	observationPrefix := attested.prefix
	current := attested.trigger
	effectiveCommit := commit
	contextTailID := current.ID
	prefix := observationPrefix
	if override != nil {
		if override.version == 0 || override.version != override.context.Prefix.Version ||
			!canonical(override.context.StateItemID) || !canonical(override.tailID) {
			return runner.refuse(ctx, envelope, commit, "invalid_replay_context",
				"disposition replay omits canonical version, state, or tail identity")
		}
		if err := trajectory.VerifyPrefix(snapshot, override.context.Prefix); err != nil {
			return runner.refuse(ctx, envelope, commit, "replay_context_mismatch", err.Error())
		}
		prefix = snapshot.Items[:override.version]
		if len(prefix) == 0 || prefix[len(prefix)-1].ID != override.tailID ||
			commit.StoreVersion > override.version {
			return runner.refuse(ctx, envelope, commit, "invalid_replay_context",
				"acknowledged disposition prefix does not contain the retained observation")
		}
		if !trajectoryCausalAncestor(prefix, current.ID, override.tailID) {
			return runner.refuse(ctx, envelope, commit, "replay_observation_not_causal",
				"acknowledged disposition is not causally linked to the retained observation")
		}
		effectiveCommit.StoreVersion = override.version
		effectiveCommit.Context = override.context
		contextTailID = override.tailID
	}
	authorityValue := trajectory.AuthorityOf(current)
	if admission.Mode == policyelements.TemporalEvidenceAdmissionAfterIntent {
		if attested.intent == nil || authorityValue != trajectory.AuthorityObserver {
			return runner.refuse(ctx, envelope, commit, "invalid_temporal_admission",
				"after-intent admission lacks an observer trigger and exact durable intent")
		}
		if attested.intentStoreVersion <= runner.revokedStoreVersion {
			return runner.ignore(ctx, envelope, commit, "intent_revoked",
				"temporal admission names a durable intent at or before the latest cancellation")
		}
		runner.selectIntent(*attested.intent)
	} else if authorityValue == trajectory.AuthorityUser {
		// ASR revisions are useful canonical evidence, but a revisable prefix is
		// not yet the participant's instruction. Clear an older durable intent
		// while a new utterance is provisional so visual cadence cannot reactivate
		// that older instruction as the participant is still speaking.
		if current.Event.Type != current.Event.Source+".endpoint" {
			runner.intent = nil
			runner.deferred = nil
			return runner.ignore(ctx, envelope, commit, "user_observation_not_final",
				"a provisional user observation cannot activate computer effects")
		}
		if admission.TriggerObservation.StoreVersion <= runner.revokedStoreVersion {
			return runner.ignore(ctx, envelope, commit, "intent_revoked",
				"temporal admission names a durable intent at or before the latest cancellation")
		}
		nextIntent := userIntentBasis{
			itemID: current.ID, triggerItemID: current.Event.EventID,
			sourceRevision: current.SourceRevision, identity: admission.TriggerObservation,
		}
		runner.selectIntent(nextIntent)
	} else if authorityValue != trajectory.AuthorityObserver {
		return runner.refuse(ctx, envelope, commit, "invalid_authority",
			fmt.Sprintf("current observation carries %q authority", authorityValue))
	}
	if runner.intent == nil {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, commit, policyelements.GenerationOutcome{
			Kind: policyelements.GenerationIgnored, Role: runner.config.Role,
			StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
			ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
			Code:    "no_user_intent",
			Message: "observer evidence cannot activate effects before a canonical user task",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	basis, found := trajectoryItem(trajectory.Snapshot{Version: effectiveCommit.StoreVersion, Items: prefix},
		runner.intent.itemID)
	if !found || basis.Kind != trajectory.KindObservation || basis.Event == nil ||
		trajectory.AuthorityOf(basis) != trajectory.AuthorityUser ||
		basis.SourceRevision != runner.intent.sourceRevision ||
		basis.Event.EventID != runner.intent.triggerItemID {
		return runner.refuse(ctx, envelope, commit, "stale_user_intent",
			"durable intent no longer names exact canonical user evidence")
	}
	if !trajectoryCausalAncestor(prefix, basis.ID, current.ID) {
		return runner.refuse(ctx, envelope, commit, "intent_not_causal",
			"current visual context is not a canonical descendant of the selected user task")
	}
	resultParent := ""
	if authorityValue == trajectory.AuthorityObserver {
		if current.Observation == nil {
			return runner.refuse(ctx, envelope, commit, "invalid_visual_context",
				"observer-authority context has no canonical observation metadata")
		}
		if current.Observation.Source != SourceScreen && current.Observation.Source != SourceCamera {
			return runner.refuse(ctx, envelope, commit, "invalid_visual_context",
				"observer-authority context is outside the screen/camera evidence contract")
		}
		var err error
		resultParent, err = canonicalResultParent(prefix, current)
		if err != nil {
			return runner.refuse(ctx, envelope, commit, "invalid_effect_consequence", err.Error())
		}
		if resultParent != "" && priorScreenConsequence(prefix, current.ID, resultParent) {
			return runner.ignore(ctx, envelope, commit, "effect_consequence_consumed",
				"this canonical tool result already has an earlier screen consequence")
		}
	}
	if runner.active != nil {
		switch {
		case runner.active.callID == "":
			if authorityValue == trajectory.AuthorityObserver {
				return runner.deferVisual(ctx, envelope, admission, commit)
			}
			return runner.ignore(ctx, envelope, commit, "generation_pending",
				"one exact cognition turn is still in flight")
		case resultParent == "":
			message := runner.retainDeferredVisual(envelope, admission, commit,
				"changed visual evidence is retained while the proposed effect awaits a terminal disposition",
				"a newer changed visual prefix is already retained while the proposed effect awaits disposition")
			return runner.ignore(ctx, envelope, commit, "effect_pending", message)
		default:
			resultItem, found := trajectoryItem(
				trajectory.Snapshot{Version: effectiveCommit.StoreVersion, Items: prefix}, resultParent,
			)
			if !found || resultItem.ToolResult == nil ||
				resultItem.ToolResult.CallID != runner.active.callID {
				return runner.refuse(ctx, envelope, commit, "effect_result_mismatch",
					"visual consequence does not settle the one active computer effect")
			}
			runner.active = nil
			runner.deferred = nil
			runner.pendingTerminal = nil
			if resultItem.ToolResult.Error != "" {
				// The forced post-effect screen is evidence that the failed call
				// settled, not new grounds for immediately proposing the same call.
				// Keep the durable user intent so a later independent camera/screen
				// change can retry, but consume this exact failed consequence.
				return runner.ignore(ctx, envelope, commit, "effect_failed",
					"failed computer effect is terminal until independent visual evidence arrives")
			}
		}
	}
	generationID := activationGenerationID(runner.config.Role, envelope.SessionID, effectiveCommit, basis.ID)
	if _, duplicate := runner.terminal[generationID]; duplicate {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, commit, policyelements.GenerationOutcome{
			Kind: policyelements.GenerationIgnored, GenerationID: generationID,
			Role: runner.config.Role, StreamID: commit.StreamID,
			SourceRevision: commit.SourceRevision, ContextVersion: effectiveCommit.StoreVersion,
			TriggerItemID: commit.TriggerItemID, Code: "duplicate_commit",
			Message: "observation commit already reached a terminal activation decision",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	runner.active = &activeGeneration{
		id: generationID, contextVersion: effectiveCommit.StoreVersion,
		intent: runner.intent.identity,
	}
	if err := runner.emit(ctx, envelope, admission, generationID, effectiveCommit, *runner.intent, contextTailID); err != nil {
		runner.active = nil
		return err
	}
	runner.rememberTerminal(generationID)
	runner.state.ContextVersion = effectiveCommit.StoreVersion
	runner.state.Emitted++
	if err := runner.publishOutcome(ctx, envelope, commit, policyelements.GenerationOutcome{
		Kind: policyelements.GenerationEmitted, GenerationID: generationID,
		Role: runner.config.Role, StreamID: commit.StreamID,
		SourceRevision: commit.SourceRevision, ContextVersion: effectiveCommit.StoreVersion,
		TriggerItemID: commit.TriggerItemID,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *activationRunner) acceptResult(
	ctx context.Context, envelope element.Envelope,
) error {
	result, ok := cognitionResultPayload(envelope.Payload)
	if !ok || envelope.SessionID != runner.sessionID || !canonical(envelope.RunID) ||
		result.RunID != envelope.RunID {
		return fmt.Errorf("Realtime-CU activation model result has invalid identity or payload %T", envelope.Payload)
	}
	var effect *activeGeneration
	var canceled *canceledActivationEffect
	if runner.active != nil && runner.active.id == result.RunID {
		effect = runner.active
	} else if retained := runner.canceledEffects[result.RunID]; retained != nil {
		canceled = retained
		effect = &retained.generation
	} else {
		// A result can race a user cancellation. It carries no authority by itself,
		// so an already-terminal generation is safe to discard and cannot release a
		// newer turn.
		if _, terminal := runner.terminal[result.RunID]; terminal {
			return nil
		}
		return fmt.Errorf("Realtime-CU activation received result for unknown run %q", result.RunID)
	}
	if result.ContextVersion != effect.contextVersion {
		return fmt.Errorf("Realtime-CU activation result context is %d, want %d",
			result.ContextVersion, effect.contextVersion)
	}
	if len(result.ToolProposals) > 1 {
		return fmt.Errorf("Realtime-CU activation result has %d proposals; profile allows one",
			len(result.ToolProposals))
	}
	if len(result.ToolProposals) == 0 {
		if canceled != nil {
			if canceled.settlement != nil {
				return errors.New("Realtime-CU activation retained a verified settlement for a canceled result with no proposal")
			}
			runner.forgetCanceledEffect(result.RunID)
			return nil
		}
		if runner.pendingSettlement != nil {
			return errors.New("Realtime-CU activation retained a verified settlement for a model result with no proposal")
		}
		if runner.pendingTerminal != nil {
			return errors.New("Realtime-CU activation received a pre-effect terminal for a model result with no proposal")
		}
		runner.active = nil
		deferred := runner.deferred
		runner.deferred = nil
		if err := runner.publishState(ctx, envelope); err != nil {
			return err
		}
		if deferred != nil {
			// Re-enter the complete admission path. In particular this verifies
			// the retained PrefixIdentity against the append-only store and checks
			// that its exact durable intent is still a causal ancestor. Never
			// rebuild a trigger from the store's newer tail.
			return runner.acceptAdmission(ctx, deferred.envelope)
		}
		return nil
	} else {
		callID := result.ToolProposals[0].Call.CallID
		tool := result.ToolProposals[0].Call.Name
		if !canonical(callID) || !canonical(tool) {
			return errors.New("Realtime-CU activation result proposal has a noncanonical call or tool ID")
		}
		if effect.callID != "" && (effect.callID != callID || effect.tool != tool) {
			return errors.New("Realtime-CU activation received conflicting model-result copies for one generation")
		}
		// Retain the latest changed visual prefix until the proposal reaches a
		// terminal disposition. A real result-linked consequence discards it as
		// pre-effect evidence; a pre-effect suppression replays it because no
		// external action occurred.
		effect.callID = callID
		effect.tool = tool
		if canceled != nil {
			if canceled.settlement == nil {
				return nil
			}
			pending := canceled.settlement
			return runner.applyVerifiedSettlement(
				ctx, pending.envelope, pending.decision, *effect, true,
			)
		}
		if runner.pendingTerminal != nil {
			if runner.pendingSettlement != nil {
				return errors.New("Realtime-CU activation received conflicting pre-effect and post-effect terminals")
			}
			pending := runner.pendingTerminal
			if pending.envelope.SessionID != envelope.SessionID ||
				pending.envelope.RunID != envelope.RunID || pending.terminal.CallID != callID {
				runner.pendingTerminal = nil
				return errors.New("Realtime-CU activation pre-effect terminal contradicts the model result")
			}
			return runner.applyEffectTerminal(ctx, pending.envelope, pending.terminal)
		}
		if runner.pendingSettlement != nil {
			pending := runner.pendingSettlement
			return runner.applyVerifiedSettlement(
				ctx, pending.envelope, pending.decision, *effect, false,
			)
		}
	}
	return runner.publishState(ctx, envelope)
}

// acceptSettlement is the terminal half of the settlement handshake. A
// continuation never enters here: it remains an AdmittedTemporalEvidence value
// and follows acceptAdmission, which may release a new cognition turn. Terminal
// evidence is deliberately not replayed through admission; after independent
// verification this method clears only the exact active effect and acknowledges
// that cleanup to the gate.
func (runner *activationRunner) acceptSettlement(
	ctx context.Context, envelope element.Envelope,
) error {
	decision, ok := intentSettlementDecisionPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(policyelements.IntentSettlementDecisionType()) {
		return runner.refuseSettlement(ctx, "invalid_settlement_decision",
			"settlement input has the wrong envelope type or payload")
	}
	if err := validateActivationSettlementEnvelope(envelope, decision); err != nil {
		return runner.refuseSettlement(ctx, "invalid_settlement_envelope", err.Error())
	}
	if envelope.SessionID != runner.sessionID || decision.SessionID != runner.sessionID ||
		decision.Probe.SessionID != runner.sessionID {
		return runner.refuseSettlement(ctx, "settlement_session_mismatch",
			"settlement input crossed the mounted trajectory session")
	}
	if runner.config.ExpectedSettlement == nil {
		return runner.refuseSettlement(ctx, "settlement_not_configured",
			"activation has no independently pinned settlement contract")
	}
	if err := policyelements.VerifyIntentSettlementDecision(
		runner.store.Snapshot(), decision, *runner.config.ExpectedSettlement,
	); err != nil {
		return runner.refuseSettlement(ctx, "invalid_settlement_decision", err.Error())
	}
	decision = cloneActivationSettlementDecision(decision)
	if pending, found := runner.pendingSettlementAcks[decision.TerminalID]; found {
		if !reflect.DeepEqual(pending.decision, decision) {
			return runner.refuseSettlement(ctx, "conflicting_settlement_decision",
				"a cleared effect retained a different terminal acknowledgement")
		}
		return runner.completePendingSettlementAcknowledgement(ctx, decision.TerminalID)
	}
	if _, duplicate := runner.acknowledgedSettlement[decision.TerminalID]; duplicate {
		return runner.ignoreSettlement(ctx, decision, "duplicate_settlement_decision",
			"the exact terminal decision was already acknowledged")
	}
	var effect *activeGeneration
	canceled := false
	if runner.active != nil && runner.active.id == decision.InvocationID {
		effect = runner.active
	} else if retained := runner.canceledEffects[decision.InvocationID]; retained != nil {
		effect = &retained.generation
		canceled = true
	}
	if effect == nil {
		return runner.refuseSettlement(ctx, "stale_settlement_decision",
			"settlement decision does not address an active generation")
	}
	if !canceled && (runner.pendingTerminal != nil || runner.pendingDisposition != nil) {
		return runner.refuseSettlement(ctx, "conflicting_settlement_decision",
			"the active effect is already awaiting a pre-effect terminal disposition")
	}
	if effect.id != decision.InvocationID || effect.intent != decision.Probe.DurableIntent ||
		effect.contextVersion >= decision.Probe.Result.StoreVersion {
		return runner.refuseSettlement(ctx, "settlement_effect_mismatch",
			"settlement decision does not exactly address the active generation, effect, and durable intent")
	}
	if effect.callID == "" {
		return runner.retainVerifiedSettlement(ctx, envelope, decision, canceled)
	}
	if effect.callID != decision.Probe.Result.CallID || effect.tool != decision.Probe.Result.Tool {
		return runner.refuseSettlement(ctx, "settlement_effect_mismatch",
			"settlement decision does not exactly address the active generation, effect, and durable intent")
	}
	return runner.applyVerifiedSettlement(ctx, envelope, decision, *effect, canceled)
}

func (runner *activationRunner) retainVerifiedSettlement(
	ctx context.Context, envelope element.Envelope,
	decision policyelements.IntentSettlementDecision, canceled bool,
) error {
	var retained **pendingActivationSettlement
	if canceled {
		effect := runner.canceledEffects[decision.InvocationID]
		if effect == nil {
			return runner.refuseSettlement(ctx, "stale_settlement_decision",
				"canceled settlement effect disappeared before retention")
		}
		retained = &effect.settlement
	} else {
		retained = &runner.pendingSettlement
	}
	if *retained != nil {
		if reflect.DeepEqual((*retained).decision, decision) {
			return runner.ignoreSettlement(ctx, decision, "duplicate_pending_settlement",
				"the exact settlement decision is already waiting for its model-result copy")
		}
		return runner.refuseSettlement(ctx, "conflicting_settlement_decision",
			"a different verified settlement decision is already waiting for this generation")
	}
	copy := cloneActivationSettlementDecision(decision)
	retainedEnvelope := envelope.Clone()
	retainedEnvelope.Payload = copy
	*retained = &pendingActivationSettlement{envelope: retainedEnvelope, decision: copy}
	return runner.ignoreSettlement(ctx, decision, "settlement_waiting_for_model_result",
		"the verified terminal settlement is retained until the exact model-result copy arrives")
}

func (runner *activationRunner) applyVerifiedSettlement(
	ctx context.Context, _ element.Envelope,
	decision policyelements.IntentSettlementDecision,
	generation activeGeneration, canceled bool,
) error {
	if generation.id != decision.InvocationID || generation.callID != decision.Probe.Result.CallID ||
		generation.tool != decision.Probe.Result.Tool || generation.intent != decision.Probe.DurableIntent ||
		generation.contextVersion >= decision.Probe.Result.StoreVersion {
		return runner.refuseSettlement(ctx, "settlement_effect_mismatch",
			"settlement decision no longer addresses the retained exact effect")
	}
	ackEnvelope, err := runner.makeSettlementAcknowledgement(decision)
	if err != nil {
		return runner.refuseSettlement(ctx, "invalid_settlement_acknowledgement", err.Error())
	}

	if _, exists := runner.pendingSettlementAcks[decision.TerminalID]; !exists &&
		len(runner.pendingSettlementAcks) >= runner.config.TerminalMemory {
		return runner.refuseSettlement(ctx, "settlement_ack_capacity",
			"activation cannot retain another undelivered settlement acknowledgement")
	}

	// The actor is the linearization point. All validation and acknowledgement
	// construction complete before this single mutation block. The exact ack is
	// retained before its lossless send, so an ambiguous or failed delivery can
	// be retried without reviving the already-cleared effect.
	if canceled {
		retained := runner.canceledEffects[decision.InvocationID]
		if retained == nil || retained.generation != generation {
			return runner.refuseSettlement(ctx, "stale_settlement_decision",
				"canceled settlement effect changed before acknowledgement")
		}
		runner.forgetCanceledEffect(decision.InvocationID)
	} else {
		if runner.active == nil || *runner.active != generation {
			return runner.refuseSettlement(ctx, "stale_settlement_decision",
				"active settlement effect changed before acknowledgement")
		}
		runner.active = nil
		runner.deferred = nil
		runner.pendingTerminal = nil
		runner.pendingDisposition = nil
		runner.pendingSettlement = nil
		if runner.intent != nil && runner.intent.identity == decision.Probe.DurableIntent {
			runner.intent = nil
		}
	}
	runner.state.Ignored++
	runner.pendingSettlementAcks[decision.TerminalID] = pendingSettlementAcknowledgement{
		envelope: ackEnvelope.Clone(), decision: cloneActivationSettlementDecision(decision),
		generation: generation,
	}
	return runner.completePendingSettlementAcknowledgement(ctx, decision.TerminalID)
}

func (runner *activationRunner) completePendingSettlementAcknowledgement(
	ctx context.Context, terminalID string,
) error {
	pending, found := runner.pendingSettlementAcks[terminalID]
	if !found {
		return errors.New("Realtime-CU activation has no pending settlement acknowledgement")
	}
	var deliveryErr error
	for attempt := 0; attempt < maximumSettlementAckTries; attempt++ {
		deliveryErr = runner.publishSettlementAcknowledgement(ctx, pending.envelope)
		if deliveryErr == nil {
			break
		}
		if context.Cause(ctx) != nil {
			return deliveryErr
		}
	}
	if deliveryErr != nil {
		return fmt.Errorf(
			"Realtime-CU settlement acknowledgement failed after %d bounded attempts: %w",
			maximumSettlementAckTries, deliveryErr,
		)
	}
	delete(runner.pendingSettlementAcks, terminalID)
	runner.rememberAcknowledgedSettlement(terminalID)
	decision := pending.decision
	code := "intent_" + string(decision.Kind)
	if err := runner.publishOutcome(ctx, pending.envelope, stateelements.ObservationCommitOutcome{},
		policyelements.GenerationOutcome{
			Kind: policyelements.GenerationIgnored, GenerationID: decision.InvocationID,
			Role: runner.config.Role, ContextVersion: pending.generation.contextVersion,
			TriggerItemID: decision.Probe.TriggerObservation.TriggerItemID,
			Code:          code, Message: "terminal settlement cleared the exact effect without reactivating cognition",
		}); err != nil {
		return err
	}
	return runner.publishState(ctx, pending.envelope)
}

func (runner *activationRunner) makeSettlementAcknowledgement(
	decision policyelements.IntentSettlementDecision,
) (element.Envelope, error) {
	acknowledgedNS := runner.clock.NowNS()
	if acknowledgedNS == 0 || acknowledgedNS < decision.FinishedNS {
		return element.Envelope{}, errors.New(
			"activation clock is earlier than the terminal settlement decision")
	}
	sequence, err := runner.sequences.Next(runner.instance + ".settlement-ack")
	if err != nil {
		return element.Envelope{}, err
	}
	acknowledgement := policyelements.IntentSettlementAcknowledgement{
		Decision:     cloneActivationSettlementDecision(decision),
		GenerationID: decision.InvocationID, AcknowledgedNS: acknowledgedNS,
	}
	itemID, err := activationSettlementAcknowledgementID(
		runner.instance, sequence, acknowledgement,
	)
	if err != nil {
		return element.Envelope{}, err
	}
	return element.Envelope{
		Type: policyelements.IntentSettlementAcknowledgementType(), ItemID: itemID,
		SessionID: runner.sessionID, RunID: decision.InvocationID, Sequence: sequence,
		CancellationScope: decision.InvocationID,
		CausalParents: []string{
			decision.TerminalID, decision.Probe.ProbeID,
			decision.Probe.Result.TrajectoryItemID,
		},
		Payload: acknowledgement,
	}, nil
}

func (runner *activationRunner) publishSettlementAcknowledgement(
	ctx context.Context, envelope element.Envelope,
) error {
	delivery, err := runner.ports.settlementAcknowledgement.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return fmt.Errorf("Realtime-CU settlement acknowledgement delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}
	return nil
}

func (runner *activationRunner) refuseSettlement(
	ctx context.Context, code, message string,
) error {
	runner.state.Refused++
	cause := element.Envelope{
		ItemID: "realtime-cu-settlement-invalid-input", SessionID: runner.sessionID,
	}
	if err := runner.publishOutcome(ctx, cause, stateelements.ObservationCommitOutcome{},
		policyelements.GenerationOutcome{
			Kind: policyelements.GenerationRefused, Role: runner.config.Role,
			Code: code, Message: boundedReason(message),
		}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *activationRunner) ignoreSettlement(
	ctx context.Context, decision policyelements.IntentSettlementDecision, code, message string,
) error {
	runner.state.Ignored++
	cause := element.Envelope{
		ItemID: decision.TerminalID, SessionID: runner.sessionID,
		RunID: decision.InvocationID, CancellationScope: decision.InvocationID,
		CausalParents: []string{decision.Probe.ProbeID},
	}
	if err := runner.publishOutcome(ctx, cause, stateelements.ObservationCommitOutcome{},
		policyelements.GenerationOutcome{
			Kind: policyelements.GenerationIgnored, GenerationID: decision.InvocationID,
			Role: runner.config.Role, Code: code, Message: message,
		}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *activationRunner) acceptEffectTerminal(
	ctx context.Context, envelope element.Envelope,
) error {
	terminal, ok := preEffectTerminalPayload(envelope.Payload)
	if !ok || envelope.SessionID != runner.sessionID || !canonical(envelope.RunID) ||
		!canonical(terminal.CallID) || !supportedPreEffectTerminalKind(terminal.Kind) {
		return runner.refuseEffectTerminal(ctx, envelope, terminal,
			"invalid_effect_terminal", "pre-effect terminal requires canonical session, run, call, and kind")
	}
	if runner.active == nil || runner.active.id != envelope.RunID {
		if _, known := runner.terminal[envelope.RunID]; known {
			return runner.ignoreEffectTerminal(ctx, envelope, terminal, "late_effect_terminal",
				"pre-effect terminal arrived after its generation was already released")
		}
		return runner.refuseEffectTerminal(ctx, envelope, terminal, "unknown_effect_terminal",
			"pre-effect terminal does not address the active generation")
	}
	if runner.active.callID == "" {
		if runner.pendingTerminal != nil {
			if runner.pendingTerminal.envelope.SessionID == envelope.SessionID &&
				runner.pendingTerminal.envelope.RunID == envelope.RunID &&
				runner.pendingTerminal.terminal == terminal {
				return runner.ignoreEffectTerminal(ctx, envelope, terminal, "duplicate_effect_terminal",
					"the same pre-effect terminal is already pending model-result verification")
			}
			return runner.refuseEffectTerminal(ctx, envelope, terminal, "conflicting_effect_terminal",
				"a different pre-effect terminal is already pending for this generation")
		}
		retained := envelope.Clone()
		retained.Payload = terminal
		runner.pendingTerminal = &pendingEffectTerminal{envelope: retained, terminal: terminal}
		return nil
	}
	if runner.active.callID != terminal.CallID {
		return runner.refuseEffectTerminal(ctx, envelope, terminal, "effect_terminal_call_mismatch",
			"pre-effect terminal call does not match the active model proposal")
	}
	return runner.applyEffectTerminal(ctx, envelope, terminal)
}

func (runner *activationRunner) applyEffectTerminal(
	ctx context.Context, envelope element.Envelope, terminal actionelements.PreEffectTerminal,
) error {
	active := runner.active
	if active == nil || active.id != envelope.RunID || active.callID != terminal.CallID {
		return errors.New("Realtime-CU activation cannot apply an unmatched pre-effect terminal")
	}
	if runner.pendingDisposition != nil {
		if runner.pendingDisposition.cause.SessionID == envelope.SessionID &&
			runner.pendingDisposition.cause.RunID == envelope.RunID &&
			runner.pendingDisposition.terminal == terminal {
			return runner.ignoreEffectTerminal(ctx, envelope, terminal, "duplicate_effect_terminal",
				"the proposal disposition is already awaiting canonical commit")
		}
		return runner.refuseEffectTerminal(ctx, envelope, terminal, "conflicting_effect_terminal",
			"a different proposal disposition is already awaiting canonical commit")
	}
	retained := envelope.Clone()
	retained.Payload = terminal
	runner.pendingDisposition = &pendingProposalDisposition{
		cause: retained, terminal: terminal,
	}
	runner.pendingTerminal = nil
	return runner.tryStartDisposition(ctx)
}

func (runner *activationRunner) tryStartDisposition(ctx context.Context) error {
	pending := runner.pendingDisposition
	if pending == nil || pending.requestID != "" {
		return nil
	}
	if runner.active == nil || runner.active.id != pending.cause.RunID ||
		runner.active.callID != pending.terminal.CallID {
		return errors.New("Realtime-CU activation lost the generation awaiting proposal disposition")
	}
	snapshot, prefix, err := runner.store.SnapshotWithPrefixIdentity()
	if err != nil {
		return fmt.Errorf("identify proposal-disposition prefix: %w", err)
	}
	proposal, found, err := dispositionProposal(snapshot, runner.active.id, pending.terminal.CallID)
	if err != nil {
		return err
	}
	if !found {
		// Model-result commit and the action path use independent graph lanes.
		// A terminal may reach activation first; a later store commit retries
		// this lookup without granting any authority in the meantime.
		return nil
	}
	kind, err := trajectoryDispositionKind(pending.terminal.Kind)
	if err != nil {
		return err
	}
	parents := []string{proposal.ID}
	if runner.deferred != nil {
		if err := attestDeferredObservation(
			snapshot, *runner.deferred, runner.config.ExpectedAdmission,
		); err != nil {
			if !errors.Is(err, policyelements.ErrAdmittedTemporalEvidenceSuperseded) {
				return fmt.Errorf("attest retained observation for proposal disposition: %w", err)
			}
			// A newer user observation invalidates the old retained visual, but it
			// does not undo the already-terminal proposal. Settle that proposal
			// without parenting the disposition to superseded evidence; the new
			// intent can activate only after its own fresh temporal admission.
			runner.deferred = nil
		} else {
			parents = appendUniqueString(parents, runner.deferred.commit.TrajectoryItemID)
		}
	}
	item := trajectory.Item{
		ID:              dispositionTrajectoryItemID(pending.cause.SessionID, runner.active.id, proposal.ID, kind),
		Kind:            trajectory.KindToolProposalDisposition,
		MonotonicNS:     monotonicTrajectoryNS(runner.clock.NowNS(), snapshot),
		CausalParentIDs: parents,
		SourceRevision:  proposal.SourceRevision,
		InvocationID:    proposal.InvocationID,
		Producer:        trajectory.Producer{Phase: trajectory.PhaseRuntime},
		ToolProposalDisposition: &trajectory.ToolProposalDisposition{
			ProposalItemID: proposal.ID, CallID: proposal.ToolCall.CallID,
			Name: proposal.ToolCall.Name, Kind: kind,
		},
	}
	if occupied, exists := trajectoryItem(snapshot, item.ID); exists {
		return fmt.Errorf("proposal-disposition trajectory item ID %q is occupied by %q",
			item.ID, occupied.Kind)
	}
	sequence, err := runner.sequences.Next(runner.instance + ".disposition-append")
	if err != nil {
		return err
	}
	requestID := fmt.Sprintf("%s-disposition-append-%d", runner.instance, sequence)
	pending.requestID = requestID
	pending.expectedVersion = snapshot.Version
	pending.expectedPrefix = prefix
	pending.item = cloneDispositionItem(item)
	appendEnvelope := pending.cause.Clone()
	appendEnvelope.Type = stateelements.AppendType()
	appendEnvelope.ItemID = requestID
	appendEnvelope.CausalParents = appendUniqueString(appendEnvelope.CausalParents, pending.cause.ItemID)
	appendEnvelope.CausalParents = appendUniqueString(appendEnvelope.CausalParents, proposal.ID)
	for _, parent := range parents[1:] {
		appendEnvelope.CausalParents = appendUniqueString(appendEnvelope.CausalParents, parent)
	}
	appendEnvelope.Payload = stateelements.Append{
		Compare: true, ExpectedVersion: snapshot.Version,
		Items: []trajectory.Item{cloneDispositionItem(item)},
	}
	delivery, err := runner.ports.dispositionAppend.Broadcast(ctx, appendEnvelope)
	if err != nil {
		pending.requestID = ""
		return err
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		pending.requestID = ""
		return fmt.Errorf("proposal-disposition append delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}
	return nil
}

func (runner *activationRunner) acceptDispositionCommit(
	ctx context.Context, envelope element.Envelope,
) error {
	pending := runner.pendingDisposition
	if pending == nil {
		return nil
	}
	if pending.requestID == "" {
		return runner.tryStartDisposition(ctx)
	}
	if !replyMatches(envelope, pending.requestID) {
		return nil
	}
	if envelope.SessionID != pending.cause.SessionID || envelope.RunID != pending.cause.RunID {
		return errors.New("proposal-disposition commit crossed the pending session or run")
	}
	commit, ok := stateCommitPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("proposal-disposition commit %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if err := attestDispositionCommit(runner.store.Snapshot(), pending, commit); err != nil {
		return fmt.Errorf("proposal-disposition commit %s: %w", envelope.ItemID, err)
	}
	active := runner.active
	if active == nil || active.id != pending.cause.RunID || active.callID != pending.terminal.CallID {
		return errors.New("proposal-disposition commit no longer matches the active generation")
	}
	deferred := runner.deferred
	runner.active = nil
	runner.deferred = nil
	runner.pendingTerminal = nil
	runner.pendingDisposition = nil
	runner.state.Ignored++
	runner.state.ContextVersion = commit.Version
	code, message := preEffectTerminalOutcome(pending.terminal.Kind)
	cause := pending.cause.Clone()
	for _, parent := range []string{
		pending.requestID, envelope.ItemID, pending.item.ID, commit.Context.StateItemID,
	} {
		cause.CausalParents = appendUniqueString(cause.CausalParents, parent)
	}
	if err := runner.publishOutcome(ctx, cause, stateelements.ObservationCommitOutcome{},
		policyelements.GenerationOutcome{
			Kind: policyelements.GenerationIgnored, GenerationID: active.id,
			Role: runner.config.Role, ContextVersion: active.contextVersion,
			Code: code, Message: message,
		}); err != nil {
		return err
	}
	if err := runner.publishState(ctx, cause); err != nil {
		return err
	}
	if deferred == nil {
		return nil
	}
	if deferred.commit.StoreVersion > commit.Version {
		// This observation committed after the disposition and its own context
		// therefore already contains that canonical fact with the observation as
		// the real tail.
		return runner.acceptAdmission(ctx, deferred.envelope)
	}
	return runner.acceptAdmissionAtContext(ctx, deferred.envelope, &activationContextOverride{
		context: commit.Context, version: commit.Version, tailID: pending.item.ID,
	})
}

func (runner *activationRunner) acceptDispositionRejection(
	ctx context.Context, envelope element.Envelope,
) error {
	pending := runner.pendingDisposition
	if pending == nil || pending.requestID == "" || !replyMatches(envelope, pending.requestID) {
		return nil
	}
	if envelope.SessionID != pending.cause.SessionID || envelope.RunID != pending.cause.RunID {
		return errors.New("proposal-disposition rejection crossed the pending session or run")
	}
	rejection, ok := stateRejectionPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("proposal-disposition rejection %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if rejection.ExpectedVersion != pending.expectedVersion {
		return fmt.Errorf("proposal-disposition rejection expects version %d, pending append used %d",
			rejection.ExpectedVersion, pending.expectedVersion)
	}
	requestID := pending.requestID
	pending.requestID = ""
	if rejection.Code != "version_conflict" {
		return fmt.Errorf("proposal-disposition append %s was rejected (%s): %s",
			requestID, rejection.Code, rejection.Message)
	}
	if rejection.CurrentVersion <= pending.expectedVersion {
		return fmt.Errorf("proposal-disposition version conflict moved from %d to invalid version %d",
			pending.expectedVersion, rejection.CurrentVersion)
	}
	pending.retries++
	if pending.retries > maximumDispositionRetries {
		return fmt.Errorf("proposal-disposition append exceeded %d version-conflict retries",
			maximumDispositionRetries)
	}
	// tryStartDisposition samples Store again and re-attests the exact proposal,
	// any newly retained observation, and the complete fresh prefix.
	return runner.tryStartDisposition(ctx)
}

func dispositionProposal(
	snapshot trajectory.Snapshot, invocationID, callID string,
) (trajectory.Item, bool, error) {
	var proposal trajectory.Item
	found := false
	for _, item := range snapshot.Items {
		if item.Kind != trajectory.KindToolProposal || item.ToolCall == nil ||
			item.InvocationID != invocationID || item.ToolCall.CallID != callID {
			continue
		}
		if found {
			return trajectory.Item{}, false, fmt.Errorf(
				"canonical trajectory has multiple proposals for run %q call %q", invocationID, callID)
		}
		proposal, found = item, true
	}
	if !found {
		return trajectory.Item{}, false, nil
	}
	if _, promoted := trajectory.PromotedToolProposalIDs(snapshot)[proposal.ID]; promoted {
		return trajectory.Item{}, false, fmt.Errorf("tool proposal %q was already promoted", proposal.ID)
	}
	terminal, _ := trajectory.TerminalToolProposalIDs(snapshot)
	if _, disposed := terminal[proposal.ID]; disposed {
		return trajectory.Item{}, false, fmt.Errorf("tool proposal %q already has a terminal disposition", proposal.ID)
	}
	return proposal, true, nil
}

func trajectoryDispositionKind(
	kind actionelements.PreEffectTerminalKind,
) (trajectory.ToolProposalDispositionKind, error) {
	switch kind {
	case actionelements.PreEffectRepetitionSuppressed:
		return trajectory.ToolProposalRepetitionSuppressed, nil
	case actionelements.PreEffectToolPolicySuppressed:
		return trajectory.ToolProposalToolPolicySuppressed, nil
	default:
		return "", fmt.Errorf("unsupported pre-effect terminal kind %q", kind)
	}
}

func attestDeferredObservation(
	snapshot trajectory.Snapshot, deferred deferredVisualCommit,
	expected policyelements.TemporalEvidenceAdmissionConfig,
) error {
	commit := deferred.commit
	if deferred.admission.TriggerCommit != commit {
		return errors.New("retained temporal admission changed its trigger commit")
	}
	if _, err := verifyActivationTemporalAdmission(snapshot, deferred.admission, expected); err != nil {
		return fmt.Errorf("retained temporal admission: %w", err)
	}
	if commit.StoreVersion == 0 || commit.Context.Prefix.Version != commit.StoreVersion ||
		commit.StoreVersion > snapshot.Version {
		return errors.New("retained observation has an invalid canonical version")
	}
	if err := trajectory.VerifyPrefix(snapshot, commit.Context.Prefix); err != nil {
		return err
	}
	prefix := snapshot.Items[:commit.StoreVersion]
	item, found := trajectoryItem(trajectory.Snapshot{Version: commit.StoreVersion, Items: prefix},
		commit.TrajectoryItemID)
	if !found || item.Kind != trajectory.KindObservation || item.Event == nil ||
		item.ID != prefix[len(prefix)-1].ID || item.Event.EventID != commit.TriggerItemID ||
		item.SourceRevision != commit.SourceRevision {
		return errors.New("retained observation no longer matches its exact canonical prefix")
	}
	return nil
}

func attestDispositionCommit(
	storeSnapshot trajectory.Snapshot, pending *pendingProposalDisposition, commit stateelements.Commit,
) error {
	if pending == nil || pending.requestID == "" {
		return errors.New("no proposal-disposition request is pending")
	}
	if !slices.Equal(commit.AppendedIDs, []string{pending.item.ID}) ||
		commit.Version != pending.expectedVersion+1 ||
		commit.Snapshot.Version != commit.Version || uint64(len(commit.Snapshot.Items)) != commit.Version {
		return errors.New("commit does not attest the one expected disposition append")
	}
	if commit.Context.Prefix.Version != commit.Version || !canonical(commit.Context.StateItemID) {
		return errors.New("commit omits the acknowledged disposition context")
	}
	if err := trajectory.VerifyPrefix(commit.Snapshot, pending.expectedPrefix); err != nil {
		return fmt.Errorf("pre-append prefix changed: %w", err)
	}
	if err := trajectory.VerifyPrefix(commit.Snapshot, commit.Context.Prefix); err != nil {
		return fmt.Errorf("commit prefix is invalid: %w", err)
	}
	if err := trajectory.VerifyPrefix(storeSnapshot, commit.Context.Prefix); err != nil {
		return fmt.Errorf("commit prefix is not canonical: %w", err)
	}
	committed := commit.Snapshot.Items[pending.expectedVersion]
	if !reflect.DeepEqual(committed, pending.item) {
		return errors.New("commit changed the disposition item")
	}
	terminal, evidence := trajectory.TerminalToolProposalIDs(commit.Snapshot)
	if _, ok := terminal[pending.item.ToolProposalDisposition.ProposalItemID]; !ok {
		return errors.New("commit did not establish terminal proposal evidence")
	}
	if _, ok := evidence[pending.item.ID]; !ok {
		return errors.New("commit did not validate the disposition item")
	}
	return nil
}

func cloneDispositionItem(item trajectory.Item) trajectory.Item {
	item.CausalParentIDs = slices.Clone(item.CausalParentIDs)
	if item.ToolProposalDisposition != nil {
		copy := *item.ToolProposalDisposition
		item.ToolProposalDisposition = &copy
	}
	return item
}

func monotonicTrajectoryNS(now uint64, snapshot trajectory.Snapshot) uint64 {
	if len(snapshot.Items) != 0 && now < snapshot.Items[len(snapshot.Items)-1].MonotonicNS {
		return snapshot.Items[len(snapshot.Items)-1].MonotonicNS
	}
	return now
}

func dispositionTrajectoryItemID(
	sessionID, runID, proposalID string, kind trajectory.ToolProposalDispositionKind,
) string {
	hash := sha256.New()
	for _, identity := range []string{sessionID, runID, proposalID, string(kind)} {
		_, _ = fmt.Fprintf(hash, "%d:", len(identity))
		_, _ = hash.Write([]byte(identity))
	}
	return "realtime-cu-proposal-disposition:sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func supportedPreEffectTerminalKind(kind actionelements.PreEffectTerminalKind) bool {
	switch kind {
	case actionelements.PreEffectRepetitionSuppressed,
		actionelements.PreEffectToolPolicySuppressed:
		return true
	default:
		return false
	}
}

func preEffectTerminalOutcome(kind actionelements.PreEffectTerminalKind) (string, string) {
	switch kind {
	case actionelements.PreEffectRepetitionSuppressed:
		return "effect_repetition_suppressed",
			"successful semantic effect replay was suppressed before dispatch"
	case actionelements.PreEffectToolPolicySuppressed:
		return "effect_tool_policy_suppressed",
			"the graph tool policy suppressed the proposal before dispatch"
	default:
		return "invalid_effect_terminal", "an unknown pre-effect terminal was rejected"
	}
}

func (runner *activationRunner) ignoreEffectTerminal(
	ctx context.Context, envelope element.Envelope, _ actionelements.PreEffectTerminal, code, message string,
) error {
	runner.state.Ignored++
	if err := runner.publishOutcome(ctx, envelope, stateelements.ObservationCommitOutcome{},
		policyelements.GenerationOutcome{
			Kind: policyelements.GenerationIgnored, GenerationID: envelope.RunID,
			Role: runner.config.Role, Code: code, Message: message,
		}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *activationRunner) refuseEffectTerminal(
	ctx context.Context, envelope element.Envelope, _ actionelements.PreEffectTerminal, code, message string,
) error {
	runner.state.Refused++
	if err := runner.publishOutcome(ctx, envelope, stateelements.ObservationCommitOutcome{},
		policyelements.GenerationOutcome{
			Kind: policyelements.GenerationRefused, GenerationID: envelope.RunID,
			Role: runner.config.Role, Code: code, Message: message,
		}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *activationRunner) deferVisual(
	ctx context.Context, envelope element.Envelope,
	admission policyelements.AdmittedTemporalEvidence,
	commit stateelements.ObservationCommitOutcome,
) error {
	message := runner.retainDeferredVisual(envelope, admission, commit,
		"changed visual evidence is retained until the in-flight cognition turn settles",
		"a newer changed visual prefix is already retained for the in-flight cognition turn")
	return runner.ignore(ctx, envelope, commit, "generation_deferred", message)
}

func (runner *activationRunner) retainDeferredVisual(
	envelope element.Envelope, admission policyelements.AdmittedTemporalEvidence,
	commit stateelements.ObservationCommitOutcome,
	retainedMessage, supersededMessage string,
) string {
	message := retainedMessage
	if runner.deferred == nil || commit.StoreVersion > runner.deferred.commit.StoreVersion {
		retained := envelope.Clone()
		// Envelope.Clone intentionally shares immutable payloads. Pin this value
		// copy anyway so replay cannot observe mutation through a caller-owned
		// *AdmittedTemporalEvidence.
		admission = cloneAdmittedTemporalEvidence(admission)
		retained.Payload = admission
		runner.deferred = &deferredVisualCommit{
			envelope: retained, admission: admission, commit: commit,
		}
	} else {
		message = supersededMessage
	}
	return message
}

func (runner *activationRunner) selectIntent(next userIntentBasis) {
	if runner.intent == nil || *runner.intent != next {
		// Evidence retained under an older task can never become evidence for the
		// new durable task, even when both share an earlier causal prefix.
		runner.deferred = nil
		if runner.pendingTerminal != nil && runner.pendingDisposition == nil {
			// The action path already made the older generation terminal without
			// crossing an effect boundary. A replacement intent may release it even
			// if its model-result copy is still queued; that late result is terminal.
			runner.active = nil
			runner.pendingTerminal = nil
		}
	}
	copy := next
	runner.intent = &copy
}

func (runner *activationRunner) ignore(
	ctx context.Context, envelope element.Envelope,
	commit stateelements.ObservationCommitOutcome, code, message string,
) error {
	runner.state.Ignored++
	if err := runner.publishOutcome(ctx, envelope, commit, policyelements.GenerationOutcome{
		Kind: policyelements.GenerationIgnored, Role: runner.config.Role,
		StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
		ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
		Code: code, Message: message,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *activationRunner) emit(
	ctx context.Context, cause element.Envelope,
	admission policyelements.AdmittedTemporalEvidence, generationID string,
	commit stateelements.ObservationCommitOutcome, basis userIntentBasis, contextTailID string,
) error {
	invocation := cloneInvocation(runner.config.Invocation)
	invocation.SourceRevision = basis.sourceRevision
	version := commit.StoreVersion
	generate := cognitionelements.Generate{
		Invocation: invocation, ExpectedContextVersion: &version,
		ExpectedContextItemID: commit.Context.StateItemID,
		CommittedContext:      &commit.Context,
	}
	trigger := cause.Clone()
	trigger.Type = cognitionelements.GenerateType()
	trigger.ItemID = generationID + ":trigger"
	trigger.RunID = generationID
	trigger.CancellationScope = generationID
	for _, parent := range []string{
		cause.ItemID, commit.Context.StateItemID, commit.TrajectoryItemID,
		contextTailID, commit.TriggerItemID, basis.itemID, basis.triggerItemID,
	} {
		trigger.CausalParents = appendUniqueString(trigger.CausalParents, parent)
	}
	if admission.DurableIntent != nil {
		trigger.CausalParents = appendUniqueString(
			trigger.CausalParents, admission.DurableIntent.TrajectoryItemID,
		)
	}
	for _, observation := range admission.QualifyingObservations {
		trigger.CausalParents = appendUniqueString(
			trigger.CausalParents, observation.TrajectoryItemID,
		)
	}
	trigger.Payload = generate
	candidate := trigger.Clone()
	candidate.Type = authority.CandidateType()
	candidate.ItemID = generationID + ":authority"
	candidate.Payload = authority.Candidate{
		RunID: generationID, SessionID: cause.SessionID,
		ActivationItemID: trigger.ItemID, ActivationCauseItemID: cause.ItemID,
		ObservationItemID: basis.itemID, ObservationTriggerItemID: basis.triggerItemID,
		SourceRevision: basis.sourceRevision, ContextVersion: commit.StoreVersion,
		ContextEnvelopeItemID: commit.Context.StateItemID,
		ContextTailItem:       contextTailID,
	}
	result, err := runner.ports.trigger.Broadcast(ctx, trigger)
	if err != nil {
		return err
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		return fmt.Errorf("Realtime-CU activation trigger delivered %d and dropped %d lanes",
			result.Delivered, result.Dropped)
	}
	result, err = runner.ports.candidate.Broadcast(ctx, candidate)
	if err != nil {
		return err
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		return fmt.Errorf("Realtime-CU authority candidate delivered %d and dropped %d lanes",
			result.Delivered, result.Dropped)
	}
	return nil
}

func (runner *activationRunner) acceptCancel(
	ctx context.Context, envelope element.Envelope,
) error {
	cancel, ok := generationCancelPayload(envelope.Payload)
	if !ok || envelope.SessionID != runner.sessionID ||
		(cancel.GenerationID == "" && cancel.StreamID == "") {
		return runner.refuse(ctx, envelope, stateelements.ObservationCommitOutcome{},
			"invalid_cancel", "intent cancellation requires canonical session and address")
	}
	if runner.config.ExpectedSettlement != nil && runner.active != nil &&
		runner.pendingDisposition == nil {
		if err := runner.rememberCanceledEffect(*runner.active, runner.pendingSettlement); err != nil {
			return err
		}
	}
	runner.intent = nil
	runner.deferred = nil
	runner.pendingTerminal = nil
	if runner.pendingDisposition == nil {
		runner.active = nil
		runner.pendingSettlement = nil
	}
	if envelope.Sequence > runner.revokedSequence {
		runner.revokedSequence = envelope.Sequence
	}
	if version := runner.store.Snapshot().Version; version > runner.revokedStoreVersion {
		runner.revokedStoreVersion = version
	}
	runner.state.Canceled++
	if err := runner.publishOutcome(ctx, envelope, stateelements.ObservationCommitOutcome{},
		policyelements.GenerationOutcome{
			Kind: policyelements.GenerationCanceled, GenerationID: cancel.GenerationID,
			Role: runner.config.Role, StreamID: cancel.StreamID,
			Code: "intent_revoked", Message: boundedReason(cancel.Reason),
		}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *activationRunner) refuse(
	ctx context.Context, cause element.Envelope,
	commit stateelements.ObservationCommitOutcome, code, message string,
) error {
	runner.state.Refused++
	if err := runner.publishOutcome(ctx, cause, commit, policyelements.GenerationOutcome{
		Kind: policyelements.GenerationRefused, Role: runner.config.Role,
		StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
		ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
		Code: code, Message: message,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *activationRunner) publishOutcome(
	ctx context.Context, cause element.Envelope,
	_ stateelements.ObservationCommitOutcome, outcome policyelements.GenerationOutcome,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".outcome")
	if err != nil {
		return err
	}
	outcome.FinishedNS = runner.clock.NowNS()
	envelope := cause.Clone()
	envelope.Type = policyelements.GenerationOutcomeType()
	envelope.ItemID = fmt.Sprintf("%s:realtime-cu-activation-outcome:%d", cause.ItemID, sequence)
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	result, err := runner.ports.outcome.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		return fmt.Errorf("Realtime-CU activation outcome delivered %d and dropped %d lanes",
			result.Delivered, result.Dropped)
	}
	return nil
}

func (runner *activationRunner) publishState(
	ctx context.Context, cause element.Envelope,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".state")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = policyelements.GenerationStateType()
	envelope.ItemID = fmt.Sprintf("%s:state:%d", runner.instance, sequence)
	if cause.ItemID != "" {
		envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	}
	envelope.Payload = runner.state
	_, err = runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *activationRunner) rememberTerminal(generationID string) {
	if _, found := runner.terminal[generationID]; found {
		return
	}
	runner.terminal[generationID] = struct{}{}
	runner.terminalOrder = append(runner.terminalOrder, generationID)
	for len(runner.terminalOrder) > runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		runner.terminalOrder = runner.terminalOrder[1:]
		delete(runner.terminal, oldest)
	}
}

func (runner *activationRunner) rememberAcknowledgedSettlement(terminalID string) {
	if _, found := runner.acknowledgedSettlement[terminalID]; found {
		return
	}
	runner.acknowledgedSettlement[terminalID] = struct{}{}
	runner.acknowledgedSettlementOrder = append(runner.acknowledgedSettlementOrder, terminalID)
	for len(runner.acknowledgedSettlementOrder) > runner.config.TerminalMemory {
		oldest := runner.acknowledgedSettlementOrder[0]
		runner.acknowledgedSettlementOrder = runner.acknowledgedSettlementOrder[1:]
		delete(runner.acknowledgedSettlement, oldest)
	}
}

func (runner *activationRunner) rememberCanceledEffect(
	generation activeGeneration, settlement *pendingActivationSettlement,
) error {
	if existing := runner.canceledEffects[generation.id]; existing != nil {
		if existing.generation != generation {
			return errors.New("Realtime-CU activation cancellation changed a retained effect identity")
		}
		if existing.settlement == nil && settlement != nil {
			existing.settlement = clonePendingActivationSettlement(settlement)
		}
		return nil
	}
	if len(runner.canceledEffects) >= runner.config.CancelMemory {
		return errors.New(
			"Realtime-CU activation cancellation memory is full of unacknowledged effects")
	}
	copy := generation
	runner.canceledEffects[generation.id] = &canceledActivationEffect{
		generation: copy, settlement: clonePendingActivationSettlement(settlement),
	}
	runner.canceledEffectOrder = append(runner.canceledEffectOrder, generation.id)
	return nil
}

func (runner *activationRunner) forgetCanceledEffect(generationID string) {
	if _, found := runner.canceledEffects[generationID]; !found {
		return
	}
	delete(runner.canceledEffects, generationID)
	for index, retained := range runner.canceledEffectOrder {
		if retained != generationID {
			continue
		}
		runner.canceledEffectOrder = append(
			runner.canceledEffectOrder[:index], runner.canceledEffectOrder[index+1:]...,
		)
		return
	}
}

func clonePendingActivationSettlement(
	source *pendingActivationSettlement,
) *pendingActivationSettlement {
	if source == nil {
		return nil
	}
	result := *source
	result.envelope = source.envelope.Clone()
	result.decision = cloneActivationSettlementDecision(source.decision)
	result.envelope.Payload = result.decision
	return &result
}

func receiveActivationInputs(
	ctx context.Context, kind string, input element.InputPort,
	output chan<- activationInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminalAdapterError(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- fmt.Errorf("receive Realtime-CU activation %s: %w", kind, err):
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- activationInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func admittedTemporalEvidencePayload(payload any) (policyelements.AdmittedTemporalEvidence, bool) {
	switch value := payload.(type) {
	case policyelements.AdmittedTemporalEvidence:
		return cloneAdmittedTemporalEvidence(value), true
	case *policyelements.AdmittedTemporalEvidence:
		if value != nil {
			return cloneAdmittedTemporalEvidence(*value), true
		}
	}
	return policyelements.AdmittedTemporalEvidence{}, false
}

func cloneAdmittedTemporalEvidence(
	value policyelements.AdmittedTemporalEvidence,
) policyelements.AdmittedTemporalEvidence {
	if value.DurableIntent != nil {
		copy := *value.DurableIntent
		value.DurableIntent = &copy
	}
	value.QualifyingObservations = slices.Clone(value.QualifyingObservations)
	return value
}

func generationCancelPayload(payload any) (policyelements.GenerationCancel, bool) {
	switch value := payload.(type) {
	case policyelements.GenerationCancel:
		return value, true
	case *policyelements.GenerationCancel:
		if value != nil {
			return *value, true
		}
	}
	return policyelements.GenerationCancel{}, false
}

func cognitionResultPayload(payload any) (cognitionelements.Result, bool) {
	switch value := payload.(type) {
	case cognitionelements.Result:
		return value, true
	case *cognitionelements.Result:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.Result{}, false
}

func preEffectTerminalPayload(payload any) (actionelements.PreEffectTerminal, bool) {
	switch value := payload.(type) {
	case actionelements.PreEffectTerminal:
		return value, true
	case *actionelements.PreEffectTerminal:
		if value != nil {
			return *value, true
		}
	}
	return actionelements.PreEffectTerminal{}, false
}

func intentSettlementDecisionPayload(
	payload any,
) (policyelements.IntentSettlementDecision, bool) {
	switch value := payload.(type) {
	case policyelements.IntentSettlementDecision:
		return value, true
	case *policyelements.IntentSettlementDecision:
		if value != nil {
			return *value, true
		}
	}
	return policyelements.IntentSettlementDecision{}, false
}

func validateActivationSettlementEnvelope(
	envelope element.Envelope, decision policyelements.IntentSettlementDecision,
) error {
	for _, identity := range []struct {
		value    string
		required bool
	}{
		{envelope.ItemID, true}, {envelope.SessionID, true},
		{envelope.SourceID, false}, {envelope.OpportunityID, false},
		{envelope.RunID, true}, {envelope.TraceID, false},
		{envelope.CancellationScope, true},
		{decision.TerminalID, true}, {decision.SessionID, true},
		{decision.InvocationID, true}, {decision.Probe.ProbeID, true},
		{decision.Probe.SessionID, true},
	} {
		if !boundedActivationIdentifier(identity.value, identity.required) {
			return errors.New("settlement envelope contains an invalid or oversized identity")
		}
	}
	if envelope.ItemID != decision.TerminalID || envelope.SessionID != decision.SessionID ||
		envelope.RunID != decision.InvocationID ||
		envelope.CancellationScope != decision.InvocationID {
		return errors.New("settlement envelope does not exactly address its terminal decision")
	}
	if len(envelope.CausalParents) == 0 || len(envelope.CausalParents) > 64 {
		return errors.New("settlement envelope has invalid causal lineage")
	}
	seen := make(map[string]struct{}, len(envelope.CausalParents))
	for _, parent := range envelope.CausalParents {
		if !boundedActivationIdentifier(parent, true) || parent == envelope.ItemID {
			return errors.New("settlement envelope has invalid causal lineage")
		}
		if _, duplicate := seen[parent]; duplicate {
			return errors.New("settlement envelope has duplicate causal lineage")
		}
		seen[parent] = struct{}{}
	}
	if _, found := seen[decision.Probe.ProbeID]; !found {
		return errors.New("settlement envelope does not directly name its verified probe")
	}
	return nil
}

func boundedActivationIdentifier(value string, required bool) bool {
	if value == "" {
		return !required
	}
	return len(value) <= 256 && utf8.ValidString(value) && canonical(value)
}

func cloneActivationSettlementDecision(
	source policyelements.IntentSettlementDecision,
) policyelements.IntentSettlementDecision {
	result := source
	result.Evidence = cloneAdmittedTemporalEvidence(source.Evidence)
	result.Probe = cloneActivationSettlementProbe(source.Probe)
	if source.Disposition != nil {
		copy := *source.Disposition
		copy.Probe = cloneActivationSettlementProbe(source.Disposition.Probe)
		result.Disposition = &copy
	}
	if source.Cancellation != nil {
		copy := *source.Cancellation
		result.Cancellation = &copy
	}
	if source.Reset != nil {
		copy := *source.Reset
		result.Reset = &copy
	}
	if source.SupersedingIntent != nil {
		copy := *source.SupersedingIntent
		result.SupersedingIntent = &copy
	}
	return result
}

func cloneActivationSettlementProbe(
	source policyelements.IntentSettlementProbe,
) policyelements.IntentSettlementProbe {
	result := source
	result.Evidence = cloneAdmittedTemporalEvidence(source.Evidence)
	return result
}

func activationSettlementAcknowledgementID(
	instance string, sequence uint64,
	acknowledgement policyelements.IntentSettlementAcknowledgement,
) (string, error) {
	payload, err := json.Marshal(acknowledgement)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("openrealtime.realtime-cu/settlement-ack/v1\x00"))
	_, _ = fmt.Fprintf(hash, "%d:%s:%d:", len(instance), instance, sequence)
	_, _ = hash.Write(payload)
	return "realtime-cu-settlement-ack:sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func activationGenerationID(
	role, sessionID string, commit stateelements.ObservationCommitOutcome, authorityItem string,
) string {
	hash := sha256.New()
	for _, identity := range []string{
		role, sessionID, commit.StreamID, commit.TriggerItemID,
		commit.TrajectoryItemID, authorityItem,
	} {
		_, _ = fmt.Fprintf(hash, "%d:", len(identity))
		_, _ = hash.Write([]byte(identity))
	}
	_, _ = fmt.Fprintf(hash, "%d:%d", commit.SourceRevision, commit.StoreVersion)
	return "realtime-cu-generation:sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func trajectoryCausalAncestor(items []trajectory.Item, ancestor, tail string) bool {
	byID := make(map[string]trajectory.Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	pending := []string{tail}
	seen := make(map[string]struct{}, len(items))
	for len(pending) != 0 {
		id := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if id == ancestor {
			return true
		}
		if _, visited := seen[id]; visited {
			continue
		}
		seen[id] = struct{}{}
		item, found := byID[id]
		if !found {
			continue
		}
		pending = append(pending, item.CausalParentIDs...)
	}
	return false
}

func canonicalResultParent(items []trajectory.Item, observation trajectory.Item) (string, error) {
	resultID := ""
	for _, parentID := range observation.CausalParentIDs {
		parent, found := trajectoryItem(trajectory.Snapshot{Version: uint64(len(items)), Items: items}, parentID)
		if !found || parent.Kind != trajectory.KindToolResult {
			continue
		}
		if resultID != "" {
			return "", errors.New("screen consequence names more than one canonical tool result")
		}
		resultID = parent.ID
	}
	return resultID, nil
}

func priorScreenConsequence(items []trajectory.Item, currentID, resultID string) bool {
	for _, item := range items {
		if item.ID == currentID {
			break
		}
		if item.Kind == trajectory.KindObservation && item.Observation != nil &&
			item.Observation.Source == SourceScreen && slices.Contains(item.CausalParentIDs, resultID) {
			return true
		}
	}
	return false
}

func cloneInvocation(source continuation.Invocation) continuation.Invocation {
	result := source
	result.Capabilities = slices.Clone(source.Capabilities)
	result.Tools = make([]continuation.ToolDefinition, len(source.Tools))
	for index, tool := range source.Tools {
		result.Tools[index] = tool
		result.Tools[index].Parameters = slices.Clone(tool.Parameters)
	}
	return result
}

func boundedReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "user canceled the durable computer-use intent"
	}
	if len(reason) > 1024 {
		return reason[:1024]
	}
	return reason
}

var _ element.ConfigValidator = activationFactory{}
var _ element.Factory = activationFactory{}
