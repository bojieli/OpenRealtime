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
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

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
	CancellationCoordinatorReference    = "policy.RealtimeComputerUseCancellationCoordinator"
	CancellationCoordinatorConfigSchema = "schema://openrealtime/realtime-cu/session-cancellation-coordinator-config/v1"
	cancellationCoordinatorRuntimeID    = "go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/session-cancellation-coordinator/v2"

	defaultCancellationTransactions = 64
	defaultCancellationTombstones   = 256
	maximumCancellationMemory       = 4096
	maximumCancellationAckStages    = 32
	maximumCancellationReasonBytes  = 1024
	maximumCancellationInputParents = 62
	maximumCancellationPublishTries = 3
	invalidCancellationInputItemID  = "realtime-cu-cancellation-invalid-input"
)

var (
	sessionCancellationType      = element.Interrupt(element.Named("realtimecu.SessionCancellation"))
	sessionCancellationStateType = element.State(
		element.Named("realtimecu.SessionCancellationState"),
	)
	sessionCancellationOutcomeType = element.Event(
		element.Named("realtimecu.SessionCancellationOutcome"),
	)
)

func SessionCancellationType() element.Type { return sessionCancellationType.Clone() }
func SessionCancellationStateType() element.Type {
	return sessionCancellationStateType.Clone()
}
func SessionCancellationOutcomeType() element.Type {
	return sessionCancellationOutcomeType.Clone()
}

// CancellationCoordinatorDescriptor turns one protocol/session cancellation
// into an inspectable, acknowledgement-driven graph transaction. It does not
// assume delivery is completion: settlement is serialized first, activation
// returns the exact generation it actually revoked, model termination is
// observed separately from canonical result commit, and action cancellation
// is acknowledged by every graph-selected stage.
func CancellationCoordinatorDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          CancellationCoordinatorReference,
		Revision:      2,
		Ports: []element.Port{
			{Name: "request", Direction: element.Input, Type: sessionCancellationType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "settlement_outcome", Direction: element.Input,
				Type:        policyelements.IntentSettlementOutcomeType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "settlement_producer_outcome", Direction: element.Input,
				Type:        policyelements.IntentDispositionProducerOutcomeType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "activation_outcome", Direction: element.Input,
				Type:        policyelements.GenerationOutcomeType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "model_outcome", Direction: element.Input,
				Type:        cognitionelements.OutcomeType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "model_commit_outcome", Direction: element.Input,
				Type:        interactionelements.ModelCommitOutcomeType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "action_outcome", Direction: element.Input,
				Type:        actionelements.OutcomeType(),
				Cardinality: element.One, Required: true, DefaultDepth: 64},
			{Name: "settlement_cancel", Direction: element.Output,
				Type:        policyelements.IntentSettlementCancelType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "activation_cancel", Direction: element.Output,
				Type:        policyelements.GenerationCancelType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "model_cancel", Direction: element.Output,
				Type:        cognitionelements.CancelType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "action_cancel", Direction: element.Output,
				Type:        actionelements.InterruptType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "state", Direction: element.Output, Type: sessionCancellationStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "outcome", Direction: element.Output, Type: sessionCancellationOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers: []string{
				"settlement_outcome", "settlement_producer_outcome",
				"activation_outcome", "model_outcome",
				"model_commit_outcome", "action_outcome",
			},
			Interrupts: []string{"request"},
			Outcomes: []string{
				"settlement_cancel", "activation_cancel", "model_cancel",
				"action_cancel", "state", "outcome",
			},
			MaxConcurrency: 1,
			BreaksCycles:   true,
		},
		StateSchema:  "schema://openrealtime/realtime-cu/session-cancellation-state/v1",
		ConfigSchema: CancellationCoordinatorConfigSchema,
		Dependencies: []element.Dependency{
			{Name: stateelements.TrajectoryStoreService},
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
		},
		Effects: []element.Effect{{
			Name: "realtime-cu.session-cancellation.transactions", Reversible: true,
		}},
	}
}

var defaultCancellationActionAckStages = []string{
	"authorized_call_commit",
	"confirmation",
	"dispatch",
	"ledger_commit",
	"proposal_admission",
	"provenance_join",
	"tool_result_commit",
}

type CancellationCoordinatorConfig struct {
	MaxTransactions int      `json:"max_transactions,omitempty"`
	TombstoneMemory int      `json:"tombstone_memory,omitempty"`
	ActionAckStages []string `json:"action_ack_stages,omitempty"`
}

// SessionCancellation is the protocol-neutral request accepted by the graph.
// The trusted mounted service supplies the store; the repeated session ID
// prevents a boundary adapter from relabelling a request across sessions.
type SessionCancellation struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason,omitempty"`
}

type SessionCancellationOutcomeKind string

const (
	SessionCancellationAccepted        SessionCancellationOutcomeKind = "accepted"
	SessionCancellationProgress        SessionCancellationOutcomeKind = "progress"
	SessionCancellationCompleted       SessionCancellationOutcomeKind = "completed"
	SessionCancellationNoCurrentIntent SessionCancellationOutcomeKind = "no_current_intent"
	SessionCancellationRefused         SessionCancellationOutcomeKind = "refused"
	SessionCancellationIgnored         SessionCancellationOutcomeKind = "ignored"
	SessionCancellationIncomplete      SessionCancellationOutcomeKind = "incomplete"
)

// SessionCancellationOutcome is a content-free transaction projection. A
// transaction is completed only after every required typed acknowledgement;
// an unaddressable model run or already-crossed effect remains incomplete.
type SessionCancellationOutcome struct {
	Kind                SessionCancellationOutcomeKind `json:"kind"`
	Operation           string                         `json:"operation"`
	TransactionID       string                         `json:"transaction_id,omitempty"`
	RequestItemID       string                         `json:"request_item_id,omitempty"`
	SessionID           string                         `json:"session_id,omitempty"`
	DurableIntentItemID string                         `json:"durable_intent_item_id,omitempty"`
	GenerationID        string                         `json:"generation_id,omitempty"`
	ActionRunID         string                         `json:"action_run_id,omitempty"`
	ActionCallID        string                         `json:"action_call_id,omitempty"`
	ActionStage         string                         `json:"action_stage,omitempty"`
	PendingSettlement   bool                           `json:"pending_settlement,omitempty"`
	PendingProducer     bool                           `json:"pending_producer,omitempty"`
	PendingActivation   bool                           `json:"pending_activation,omitempty"`
	PendingModel        bool                           `json:"pending_model,omitempty"`
	PendingModelCommit  bool                           `json:"pending_model_commit,omitempty"`
	PendingActionAcks   int                            `json:"pending_action_acks,omitempty"`
	Code                string                         `json:"code,omitempty"`
	Message             string                         `json:"message,omitempty"`
	StateRevisionBefore uint64                         `json:"state_revision_before"`
	StateRevisionAfter  uint64                         `json:"state_revision_after"`
	FinishedNS          uint64                         `json:"finished_ns"`
}

func (SessionCancellationOutcome) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

type SessionCancellationState struct {
	Revision                 uint64 `json:"revision"`
	PendingTransactions      int    `json:"pending_transactions"`
	Tombstones               int    `json:"tombstones"`
	Requests                 uint64 `json:"requests"`
	Completed                uint64 `json:"completed"`
	NoCurrentIntent          uint64 `json:"no_current_intent"`
	Refused                  uint64 `json:"refused"`
	Incomplete               uint64 `json:"incomplete"`
	PendingSettlementAcks    int    `json:"pending_settlement_acks"`
	PendingProducerAcks      int    `json:"pending_producer_acks"`
	PendingActivationAcks    int    `json:"pending_activation_acks"`
	PendingModelAcks         int    `json:"pending_model_acks"`
	PendingModelCommitAcks   int    `json:"pending_model_commit_acks"`
	PendingActionAcks        int    `json:"pending_action_acks"`
	MaxTransactions          int    `json:"max_transactions"`
	TombstoneMemory          int    `json:"tombstone_memory"`
	ConfiguredActionAckKinds int    `json:"configured_action_ack_stages"`
	Saturated                bool   `json:"saturated"`
}

func (SessionCancellationState) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

var (
	_ element.InspectionCauseProvider = SessionCancellationOutcome{}
	_ element.InspectionCauseProvider = SessionCancellationState{}
)

type cancellationCoordinatorFactory struct{}

var (
	_ element.Factory         = cancellationCoordinatorFactory{}
	_ element.ConfigValidator = cancellationCoordinatorFactory{}
	_ element.Runnable        = (*cancellationCoordinatorRunner)(nil)
)

func (cancellationCoordinatorFactory) Descriptor() element.Descriptor {
	return CancellationCoordinatorDescriptor()
}

func (cancellationCoordinatorFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeCancellationCoordinatorConfig(source)
	return err
}

func (cancellationCoordinatorFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeCancellationCoordinatorConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("%s %s config: %w",
			CancellationCoordinatorReference, mount.InstanceID, err)
	}
	if !boundedActivationIdentifier(mount.InstanceID, true) {
		return nil, errors.New("Realtime-CU cancellation coordinator requires a canonical instance ID")
	}
	storeValue, _, found := mount.Services.Lookup(stateelements.TrajectoryStoreService)
	if !found {
		return nil, errors.New("Realtime-CU cancellation coordinator has no canonical trajectory store")
	}
	storeService, ok := storeValue.(*stateelements.TrajectoryStoreServiceValue)
	if !ok || storeService == nil || storeService.Store == nil {
		return nil, fmt.Errorf("Realtime-CU cancellation coordinator trajectory store has type %T", storeValue)
	}
	if !boundedActivationIdentifier(storeService.SessionID, true) {
		return nil, errors.New(
			"Realtime-CU cancellation coordinator trajectory store has no trusted canonical session ID")
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("Realtime-CU cancellation coordinator has no runtime clock")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("Realtime-CU cancellation coordinator clock has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("Realtime-CU cancellation coordinator has no runtime sequence allocator")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("Realtime-CU cancellation coordinator sequence allocator has type %T", sequenceValue)
	}
	ports, err := cancellationCoordinatorPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &cancellationCoordinatorRunner{
		instance: mount.InstanceID, sessionID: storeService.SessionID,
		config: config, store: storeService.Store, clock: clock, sequences: sequences,
		resolution: mount.Resolution, ports: ports,
		transactions: make(map[string]*cancellationTransaction),
		byRequest:    make(map[string]*cancellationTransaction),
		byIntent:     make(map[string]*cancellationTransaction),
		tombstones:   make(map[string]cancellationTombstone),
		modelCommits: make(map[string]element.Envelope),
		state: SessionCancellationState{
			MaxTransactions:          config.MaxTransactions,
			TombstoneMemory:          config.TombstoneMemory,
			ConfiguredActionAckKinds: len(config.ActionAckStages),
		},
	}, nil
}

func decodeCancellationCoordinatorConfig(
	source json.RawMessage,
) (CancellationCoordinatorConfig, error) {
	config := CancellationCoordinatorConfig{
		MaxTransactions: defaultCancellationTransactions,
		TombstoneMemory: defaultCancellationTombstones,
		ActionAckStages: slices.Clone(defaultCancellationActionAckStages),
	}
	if err := decodeExactJSON(source, &config); err != nil {
		return CancellationCoordinatorConfig{}, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(source, &fields); err != nil {
		return CancellationCoordinatorConfig{}, err
	}
	for _, name := range []string{"max_transactions", "tombstone_memory", "action_ack_stages"} {
		if raw, found := fields[name]; found && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return CancellationCoordinatorConfig{}, fmt.Errorf("%s cannot be null", name)
		}
	}
	if config.MaxTransactions < 1 || config.MaxTransactions > maximumCancellationMemory ||
		config.TombstoneMemory < 1 || config.TombstoneMemory > maximumCancellationMemory {
		return CancellationCoordinatorConfig{}, fmt.Errorf(
			"cancellation memory bounds must be between 1 and %d", maximumCancellationMemory)
	}
	if len(config.ActionAckStages) < 1 || len(config.ActionAckStages) > maximumCancellationAckStages {
		return CancellationCoordinatorConfig{}, fmt.Errorf(
			"action_ack_stages must contain between 1 and %d stages", maximumCancellationAckStages)
	}
	stages := slices.Clone(config.ActionAckStages)
	for _, stage := range stages {
		if !boundedActivationIdentifier(stage, true) {
			return CancellationCoordinatorConfig{}, fmt.Errorf(
				"action acknowledgement stage %q is not canonical", stage)
		}
	}
	sort.Strings(stages)
	if len(slices.Compact(slices.Clone(stages))) != len(stages) {
		return CancellationCoordinatorConfig{}, errors.New("action_ack_stages contains a duplicate")
	}
	config.ActionAckStages = stages
	return config, nil
}

type cancellationCoordinatorPorts struct {
	request, settlementOutcome, settlementProducerOutcome element.InputPort
	activationOutcome, modelOutcome                       element.InputPort
	modelCommitOutcome, actionOutcome                     element.InputPort
	settlementCancel, activationCancel, modelCancel       element.OutputPort
	actionCancel, state, outcome                          element.OutputPort
}

func cancellationCoordinatorPortsFrom(ports element.Ports) (cancellationCoordinatorPorts, error) {
	if ports == nil {
		return cancellationCoordinatorPorts{}, errors.New("Realtime-CU cancellation coordinator has nil ports")
	}
	var result cancellationCoordinatorPorts
	for _, entry := range []struct {
		name string
		set  *element.InputPort
	}{
		{"request", &result.request},
		{"settlement_outcome", &result.settlementOutcome},
		{"settlement_producer_outcome", &result.settlementProducerOutcome},
		{"activation_outcome", &result.activationOutcome},
		{"model_outcome", &result.modelOutcome},
		{"model_commit_outcome", &result.modelCommitOutcome},
		{"action_outcome", &result.actionOutcome},
	} {
		port, err := ports.Input(entry.name)
		if err != nil {
			return cancellationCoordinatorPorts{}, err
		}
		*entry.set = port
	}
	for _, entry := range []struct {
		name string
		set  *element.OutputPort
	}{
		{"settlement_cancel", &result.settlementCancel},
		{"activation_cancel", &result.activationCancel},
		{"model_cancel", &result.modelCancel},
		{"action_cancel", &result.actionCancel},
		{"state", &result.state},
		{"outcome", &result.outcome},
	} {
		port, err := ports.Output(entry.name)
		if err != nil {
			return cancellationCoordinatorPorts{}, err
		}
		*entry.set = port
	}
	return result, nil
}

type cancellationCoordinatorInput struct {
	kind     string
	envelope element.Envelope
}

type cancellationActionTarget struct {
	runID, callID string
	envelope      element.Envelope
	sent          bool
	acknowledged  map[string]string
}

type cancellationTransaction struct {
	id                    string
	request               element.Envelope
	requestDigest         string
	reason                string
	intent                policyelements.TemporalEvidenceItemIdentity
	snapshotVersion       uint64
	settlement            element.Envelope
	settlementSent        bool
	settlementSeen        bool
	settlementDone        bool
	settlementAuthorizer  element.Envelope
	producerDone          bool
	producerAuthorizer    element.Envelope
	activation            element.Envelope
	activationSent        bool
	activationDone        bool
	activationAuthorizer  element.Envelope
	generationID          string
	model                 element.Envelope
	modelSent             bool
	modelDone             bool
	modelAuthorizer       element.Envelope
	modelCommitNeeded     bool
	modelCommitDone       bool
	modelCommitAuthorizer element.Envelope
	actionsDiscovered     bool
	actions               map[string]*cancellationActionTarget
	failureCode           string
	completion            *cancellationCompletionPublication
}

// cancellationCompletionPublication is retained across transient output
// failures. The two envelopes and their projected state revision are minted
// once; a retry never reallocates an identity, republishes a part that already
// succeeded, or repeats any upstream cancellation effect.
type cancellationCompletionPublication struct {
	outcome          element.Envelope
	state            element.Envelope
	projectedState   SessionCancellationState
	kind             SessionCancellationOutcomeKind
	outcomePublished bool
	statePublished   bool
}

type cancellationTombstone struct {
	transactionID string
	requestDigest string
	kind          SessionCancellationOutcomeKind
}

type cancellationCoordinatorRunner struct {
	instance   string
	sessionID  string
	config     CancellationCoordinatorConfig
	store      *trajectory.Store
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	resolution element.ResolutionReporter
	ports      cancellationCoordinatorPorts

	transactions   map[string]*cancellationTransaction
	byRequest      map[string]*cancellationTransaction
	byIntent       map[string]*cancellationTransaction
	tombstones     map[string]cancellationTombstone
	tombstoneOrder []string
	modelCommits   map[string]element.Envelope
	modelCommitOrd []string
	state          SessionCancellationState
}

func (runner *cancellationCoordinatorRunner) Run(parent context.Context) error {
	if err := reportElementRuntime(runner.resolution, cancellationCoordinatorRuntimeID,
		"implementation:2", CancellationCoordinatorDescriptor()); err != nil {
		return err
	}
	if err := runner.publishState(parent, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan cancellationCoordinatorInput)
	failures := make(chan error, 7)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{
		{"request", runner.ports.request},
		{"settlement_outcome", runner.ports.settlementOutcome},
		{"settlement_producer_outcome", runner.ports.settlementProducerOutcome},
		{"activation_outcome", runner.ports.activationOutcome},
		{"model_outcome", runner.ports.modelOutcome},
		{"model_commit_outcome", runner.ports.modelCommitOutcome},
		{"action_outcome", runner.ports.actionOutcome},
	} {
		wait.Add(1)
		go receiveCancellationCoordinatorInput(
			ctx, source.kind, source.port, inputs, failures, &wait,
		)
	}
	defer func() {
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
			case "request":
				err = runner.acceptRequest(ctx, input.envelope)
			case "settlement_outcome":
				err = runner.acceptSettlementOutcome(ctx, input.envelope)
			case "settlement_producer_outcome":
				err = runner.acceptSettlementProducerOutcome(ctx, input.envelope)
			case "activation_outcome":
				err = runner.acceptActivationOutcome(ctx, input.envelope)
			case "model_outcome":
				err = runner.acceptModelOutcome(ctx, input.envelope)
			case "model_commit_outcome":
				err = runner.acceptModelCommitOutcome(ctx, input.envelope)
			case "action_outcome":
				err = runner.acceptActionOutcome(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown cancellation coordinator input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func receiveCancellationCoordinatorInput(
	ctx context.Context, kind string, port element.InputPort,
	inputs chan<- cancellationCoordinatorInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := port.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, graphruntime.ErrChannelClosed) {
				select {
				case failures <- fmt.Errorf("receive cancellation coordinator %s: %w", kind, err):
				case <-ctx.Done():
				}
			}
			return
		}
		select {
		case inputs <- cancellationCoordinatorInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func (runner *cancellationCoordinatorRunner) acceptRequest(
	ctx context.Context, envelope element.Envelope,
) error {
	request, ok := sessionCancellationPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(SessionCancellationType()) {
		return runner.refuse(ctx, envelope, "request", "invalid_request",
			fmt.Sprintf("session cancellation payload has type %T", envelope.Payload))
	}
	if err := validateCancellationCoordinatorEnvelope("request", envelope, true); err != nil {
		return runner.refuse(ctx, envelope, "request", "invalid_request", err.Error())
	}
	request.Reason = strings.TrimSpace(request.Reason)
	if err := validateSessionCancellationRequest(runner.sessionID, envelope, request); err != nil {
		return runner.refuse(ctx, envelope, "request", "invalid_request", err.Error())
	}
	digest := cancellationRequestDigest(envelope, request)
	if retained, found := runner.tombstones[envelope.ItemID]; found {
		if retained.requestDigest != digest {
			return runner.refuse(ctx, envelope, "request", "request_identity_conflict",
				"a terminal request item ID was replayed with different content")
		}
		return runner.transition(ctx, nil, envelope, SessionCancellationOutcome{
			Kind: SessionCancellationIgnored, Operation: "request",
			TransactionID: retained.transactionID, RequestItemID: envelope.ItemID,
			SessionID: runner.sessionID, Code: "duplicate_terminal_request",
			Message: "the exact session cancellation request is already terminal",
		})
	}
	if active := runner.byRequest[envelope.ItemID]; active != nil {
		if active.requestDigest != digest {
			return runner.refuse(ctx, envelope, "request", "request_identity_conflict",
				"an active request item ID was replayed with different content")
		}
		if !active.settlementSent {
			if err := runner.sendExact(ctx, runner.ports.settlementCancel, active.settlement); err != nil {
				return err
			}
			active.settlementSent = true
		}
		return runner.transition(ctx, active, envelope, SessionCancellationOutcome{
			Kind: SessionCancellationIgnored, Operation: "request",
			Code:    "duplicate_active_request",
			Message: "the exact session cancellation transaction is still awaiting acknowledgements",
		})
	}

	snapshot := runner.store.Snapshot()
	intent, found, err := newestFinalUserIntent(snapshot, runner.sessionID)
	if err != nil {
		return runner.refuse(ctx, envelope, "request", "invalid_canonical_intent", err.Error())
	}
	runner.state.Requests++
	if !found {
		transactionID := cancellationTransactionID(runner.sessionID, envelope.ItemID,
			policyelements.TemporalEvidenceItemIdentity{}, snapshot.Version)
		runner.state.NoCurrentIntent++
		runner.rememberTombstone(envelope.ItemID, cancellationTombstone{
			transactionID: transactionID, requestDigest: digest,
			kind: SessionCancellationNoCurrentIntent,
		})
		return runner.transition(ctx, nil, envelope, SessionCancellationOutcome{
			Kind: SessionCancellationNoCurrentIntent, Operation: "request",
			TransactionID: transactionID, RequestItemID: envelope.ItemID,
			SessionID: runner.sessionID, Code: "no_current_intent",
			Message: "the canonical trajectory has no final user-authority intent to cancel",
		})
	}
	if active := runner.byIntent[intent.TrajectoryItemID]; active != nil {
		// This is a distinct caller request, not a replay of the active one.
		// Keep its own item identity on the terminal response so an adapter waiter
		// cannot be stranded behind the first transaction's RequestItemID.
		return runner.transition(ctx, nil, envelope, SessionCancellationOutcome{
			Kind: SessionCancellationIgnored, Operation: "request",
			TransactionID: active.id, RequestItemID: envelope.ItemID,
			SessionID: runner.sessionID, DurableIntentItemID: active.intent.TrajectoryItemID,
			Code:    "intent_cancellation_in_progress",
			Message: "the newest exact durable intent already has an active cancellation transaction",
		})
	}
	if len(runner.transactions) >= runner.config.MaxTransactions {
		runner.state.Saturated = true
		return runner.refuse(ctx, envelope, "request", "transaction_capacity_exhausted",
			"the coordinator cannot evict a live cancellation transaction")
	}
	if request.Reason == "" {
		request.Reason = "client canceled response"
	}
	transaction := &cancellationTransaction{
		id:      cancellationTransactionID(runner.sessionID, envelope.ItemID, intent, snapshot.Version),
		request: envelope.Clone(), requestDigest: digest, reason: request.Reason,
		intent: intent, snapshotVersion: snapshot.Version,
		actions: make(map[string]*cancellationActionTarget),
	}
	settlement, err := runner.makeSettlementCancel(transaction)
	if err != nil {
		return runner.refuse(ctx, envelope, "request", "cannot_construct_settlement_cancel", err.Error())
	}
	transaction.settlement = settlement
	runner.transactions[transaction.id] = transaction
	runner.byRequest[envelope.ItemID] = transaction
	runner.byIntent[intent.TrajectoryItemID] = transaction
	if err := runner.sendExact(ctx, runner.ports.settlementCancel, transaction.settlement); err != nil {
		return err
	}
	transaction.settlementSent = true
	return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
		Kind: SessionCancellationAccepted, Operation: "request",
		Code:    "settlement_cancel_sent",
		Message: "the exact durable-intent cancellation is awaiting settlement acknowledgement",
	})
}

func (runner *cancellationCoordinatorRunner) acceptSettlementOutcome(
	ctx context.Context, envelope element.Envelope,
) error {
	if err := validateCancellationCoordinatorEnvelope("settlement outcome", envelope, true); err != nil {
		return runner.refuse(ctx, envelope, "settlement_outcome", "invalid_settlement_envelope", err.Error())
	}
	outcome, ok := intentSettlementOutcomePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(policyelements.IntentSettlementOutcomeType()) {
		return runner.refuse(ctx, envelope, "settlement_outcome", "invalid_settlement_outcome",
			fmt.Sprintf("settlement outcome payload has type %T", envelope.Payload))
	}
	if err := validateIntentSettlementOutcome(outcome); err != nil {
		return runner.refuse(ctx, envelope, "settlement_outcome", "invalid_settlement_outcome", err.Error())
	}
	transaction := runner.transactionForSettlementOutcome(envelope, outcome)
	if transaction == nil {
		return nil
	}
	if envelope.SessionID != runner.sessionID || outcome.SessionID != runner.sessionID ||
		outcome.DurableIntentItemID != transaction.intent.TrajectoryItemID {
		return runner.incomplete(ctx, transaction, envelope, "settlement_identity_mismatch",
			"settlement outcome crossed the exact session or durable-intent transaction")
	}
	switch outcome.Operation {
	case "cancel":
		if !slices.Contains(envelope.CausalParents, transaction.settlement.ItemID) {
			return runner.incomplete(ctx, transaction, envelope, "settlement_lineage_mismatch",
				"initial settlement acknowledgement does not directly name the cancellation request")
		}
		if outcome.Kind == policyelements.IntentSettlementRefused {
			return runner.permanentIncomplete(
				ctx, transaction, envelope, "settlement_cancel_refused", outcome.Message,
			)
		}
		switch outcome.Kind {
		case policyelements.IntentSettlementHeld,
			policyelements.IntentSettlementCanceled,
			policyelements.IntentSettlementIgnored:
		default:
			return runner.incomplete(ctx, transaction, envelope, "settlement_cancel_unacknowledged",
				fmt.Sprintf("settlement cancellation reached %s/%s", outcome.Kind, outcome.Code))
		}
		transaction.settlementSeen = true
		transaction.settlementDone = settlementCancellationTerminal(outcome)
		if transaction.settlementDone {
			transaction.settlementAuthorizer = envelope.Clone()
		}
	case "ack":
		if !transaction.settlementSeen || outcome.Kind != policyelements.IntentSettlementCanceled {
			return nil
		}
		transaction.settlementDone = true
		transaction.settlementAuthorizer = envelope.Clone()
	default:
		return nil
	}
	if err := runner.advance(ctx, transaction); err != nil {
		return err
	}
	if runner.ready(transaction) {
		return runner.complete(ctx, transaction, envelope)
	}
	return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
		Kind: SessionCancellationProgress, Operation: "settlement_ack",
		Code:    "settlement_acknowledged",
		Message: "the settlement gate acknowledged the exact durable-intent cancellation",
	})
}

func (runner *cancellationCoordinatorRunner) acceptSettlementProducerOutcome(
	ctx context.Context, envelope element.Envelope,
) error {
	if err := validateCancellationCoordinatorEnvelope(
		"settlement producer outcome", envelope, true,
	); err != nil {
		return runner.refuse(ctx, envelope, "settlement_producer_outcome",
			"invalid_settlement_producer_envelope", err.Error())
	}
	outcome, ok := intentDispositionProducerOutcomePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(policyelements.IntentDispositionProducerOutcomeType()) {
		return runner.refuse(ctx, envelope, "settlement_producer_outcome",
			"invalid_settlement_producer_outcome",
			fmt.Sprintf("settlement producer outcome payload has type %T", envelope.Payload))
	}
	if err := validateIntentDispositionProducerOutcome(outcome); err != nil {
		return runner.refuse(ctx, envelope, "settlement_producer_outcome",
			"invalid_settlement_producer_outcome", err.Error())
	}
	transaction := runner.transactionForDirectParent(envelope,
		func(candidate *cancellationTransaction) string {
			if candidate.settlementSent {
				return candidate.settlement.ItemID
			}
			return ""
		})
	if transaction == nil {
		return nil
	}
	if envelope.SessionID != runner.sessionID ||
		outcome.DurableIntentItemID != transaction.intent.TrajectoryItemID {
		return runner.incomplete(ctx, transaction, envelope, "settlement_producer_identity_mismatch",
			"settlement producer outcome crossed the exact session or durable-intent transaction")
	}
	if outcome.Kind == policyelements.IntentDispositionProducerRefused ||
		outcome.Kind == policyelements.IntentDispositionProducerFailed {
		return runner.permanentIncomplete(ctx, transaction, envelope,
			"settlement_producer_cancel_refused",
			fmt.Sprintf("settlement producer cancellation reached %s/%s: %s",
				outcome.Kind, outcome.Code, outcome.Message))
	}
	terminal := false
	switch {
	case outcome.Kind == policyelements.IntentDispositionProducerCanceled &&
		outcome.Code == "canceled":
		terminal = true
	case outcome.Kind == policyelements.IntentDispositionProducerIgnored &&
		outcome.Code == "duplicate_cancellation":
		// The producer emits duplicate_cancellation only after it has no matching
		// active Decide call. While a canceled Decide is still running it emits
		// cancellation_pending_decision instead.
		terminal = true
	case outcome.Kind == policyelements.IntentDispositionProducerIgnored &&
		outcome.Code == "cancellation_pending_decision":
		return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
			Kind: SessionCancellationProgress, Operation: "settlement_producer_ack",
			Code:    "settlement_producer_decision_pending",
			Message: "the exact settlement decision is canceled but its provider call has not quiesced",
		})
	}
	if !terminal {
		return runner.incomplete(ctx, transaction, envelope,
			"settlement_producer_cancel_unacknowledged",
			fmt.Sprintf("settlement producer cancellation reached %s/%s",
				outcome.Kind, outcome.Code))
	}
	if transaction.completion != nil {
		return runner.publishRetainedTerminal(ctx, transaction)
	}
	transaction.producerDone = true
	transaction.producerAuthorizer = envelope.Clone()
	if err := runner.advance(ctx, transaction); err != nil {
		return err
	}
	if runner.ready(transaction) {
		return runner.complete(ctx, transaction, envelope)
	}
	return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
		Kind: SessionCancellationProgress, Operation: "settlement_producer_ack",
		Code:    "settlement_producer_quiescent",
		Message: "the intent-disposition producer acknowledged quiescence for the exact durable intent",
	})
}

func (runner *cancellationCoordinatorRunner) acceptActivationOutcome(
	ctx context.Context, envelope element.Envelope,
) error {
	if err := validateCancellationCoordinatorEnvelope("activation outcome", envelope, true); err != nil {
		return runner.refuse(ctx, envelope, "activation_outcome", "invalid_activation_envelope", err.Error())
	}
	outcome, ok := generationOutcomePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(policyelements.GenerationOutcomeType()) {
		return runner.refuse(ctx, envelope, "activation_outcome", "invalid_activation_outcome",
			fmt.Sprintf("activation outcome payload has type %T", envelope.Payload))
	}
	if err := validateGenerationOutcome(outcome); err != nil {
		return runner.refuse(ctx, envelope, "activation_outcome", "invalid_activation_outcome", err.Error())
	}
	transaction := runner.transactionForDirectParent(envelope, func(candidate *cancellationTransaction) string {
		if candidate.activationSent {
			return candidate.activation.ItemID
		}
		return ""
	})
	if transaction == nil {
		return nil
	}
	if envelope.SessionID != runner.sessionID || outcome.Kind != policyelements.GenerationCanceled {
		return runner.permanentIncomplete(ctx, transaction, envelope, "activation_cancel_refused",
			fmt.Sprintf("activation cancellation reached %s/%s", outcome.Kind, outcome.Code))
	}
	if outcome.Code != "intent_revoked" && outcome.Code != "intent_not_active" &&
		outcome.Code != "intent_superseded" {
		return runner.incomplete(ctx, transaction, envelope, "activation_cancel_unacknowledged",
			fmt.Sprintf("activation cancellation reached %s/%s", outcome.Kind, outcome.Code))
	}
	if outcome.GenerationID != "" && !boundedActivationIdentifier(outcome.GenerationID, true) {
		return runner.incomplete(ctx, transaction, envelope, "invalid_generation_identity",
			"activation returned a noncanonical generation identity")
	}
	transaction.activationDone = true
	transaction.activationAuthorizer = envelope.Clone()
	transaction.generationID = outcome.GenerationID
	if err := runner.advance(ctx, transaction); err != nil {
		return err
	}
	if runner.ready(transaction) {
		return runner.complete(ctx, transaction, envelope)
	}
	return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
		Kind: SessionCancellationProgress, Operation: "activation_ack",
		GenerationID: outcome.GenerationID, Code: outcome.Code,
		Message: "activation acknowledged the exact durable-intent cancellation",
	})
}

func (runner *cancellationCoordinatorRunner) acceptModelOutcome(
	ctx context.Context, envelope element.Envelope,
) error {
	if err := validateCancellationCoordinatorEnvelope("model outcome", envelope, true); err != nil {
		return runner.refuse(ctx, envelope, "model_outcome", "invalid_model_envelope", err.Error())
	}
	outcome, ok := cognitionOutcomePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(cognitionelements.OutcomeType()) {
		return runner.refuse(ctx, envelope, "model_outcome", "invalid_model_outcome",
			fmt.Sprintf("model outcome payload has type %T", envelope.Payload))
	}
	if err := validateCognitionOutcome(outcome); err != nil {
		return runner.refuse(ctx, envelope, "model_outcome", "invalid_model_outcome", err.Error())
	}
	transaction := runner.transactionForModelOutcome(envelope, outcome.RunID)
	if transaction == nil {
		return nil
	}
	if envelope.SessionID != runner.sessionID || outcome.RunID != transaction.generationID {
		return runner.incomplete(ctx, transaction, envelope, "model_identity_mismatch",
			"model outcome crossed the exact session or generation transaction")
	}
	direct := transaction.modelSent && slices.Contains(envelope.CausalParents, transaction.model.ItemID)
	if direct && outcome.Kind == cognitionelements.OutcomeIgnored && outcome.Code == "run_not_active" {
		if runner.modelResultCommitted(transaction) {
			// The cancel raced after ModelResultCommit made this exact run's
			// instruction batch canonical. In that ordering, run_not_active is
			// positive quiescence evidence: no provider work remains, and the
			// canonical batch is the source of any pending action targets.
			transaction.modelDone = true
			transaction.modelAuthorizer = envelope.Clone()
			transaction.modelCommitNeeded = true
			if err := runner.adoptModelCommit(transaction); err != nil {
				return runner.permanentIncomplete(ctx, transaction, envelope,
					"model_commit_not_canonical", err.Error())
			}
			if err := runner.advance(ctx, transaction); err != nil {
				return err
			}
			if runner.ready(transaction) {
				return runner.complete(ctx, transaction, envelope)
			}
			return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
				Kind: SessionCancellationProgress, Operation: "model_ack",
				GenerationID: transaction.generationID, Code: "canonical_result_already_committed",
				Message: "the exact model run is inactive and its canonical result batch is committed",
			})
		}
		// Cancel can overtake the already-emitted trigger on independent graph
		// lanes, or arrive while another serial generation still owns the model.
		// An idle reply therefore proves neither cancellation nor terminal
		// generation. This is progress, not a terminal incomplete result: keep the
		// adapter waiter attached until the addressed trigger is either rejected by
		// the model's durable pre-cancel tombstone or reaches another typed terminal
		// outcome.
		return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
			Kind: SessionCancellationProgress, Operation: "model_ack",
			GenerationID: transaction.generationID, Code: "model_run_not_observed",
			Message: "model cancellation arrived before the addressed generation became observable",
		})
	}
	if !modelOutcomeTerminal(outcome) {
		return runner.incomplete(ctx, transaction, envelope, "model_cancel_unacknowledged",
			fmt.Sprintf("model cancellation reached %s/%s", outcome.Kind, outcome.Code))
	}
	transaction.modelDone = true
	transaction.modelAuthorizer = envelope.Clone()
	transaction.modelCommitNeeded = modelOutcomeHasPreparedResult(outcome) ||
		runner.modelResultCommitted(transaction)
	if !transaction.modelCommitNeeded {
		transaction.modelCommitDone = true
	} else if err := runner.adoptModelCommit(transaction); err != nil {
		return runner.permanentIncomplete(ctx, transaction, envelope,
			"model_commit_not_canonical", err.Error())
	}
	if err := runner.advance(ctx, transaction); err != nil {
		return err
	}
	if runner.ready(transaction) {
		return runner.complete(ctx, transaction, envelope)
	}
	return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
		Kind: SessionCancellationProgress, Operation: "model_ack",
		GenerationID: transaction.generationID, Code: string(outcome.Kind),
		Message: "the exact model run reached a typed terminal outcome",
	})
}

func (runner *cancellationCoordinatorRunner) acceptModelCommitOutcome(
	ctx context.Context, envelope element.Envelope,
) error {
	if err := validateCancellationCoordinatorEnvelope("model commit outcome", envelope, true); err != nil {
		return runner.refuse(ctx, envelope, "model_commit_outcome", "invalid_model_commit_envelope", err.Error())
	}
	outcome, ok := modelCommitOutcomePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(interactionelements.ModelCommitOutcomeType()) {
		return runner.refuse(ctx, envelope, "model_commit_outcome", "invalid_model_commit_outcome",
			fmt.Sprintf("model commit outcome payload has type %T", envelope.Payload))
	}
	if err := validateModelCommitOutcome(outcome); err != nil {
		return runner.refuse(ctx, envelope, "model_commit_outcome", "invalid_model_commit_outcome", err.Error())
	}
	transaction := runner.transactionForGeneration(outcome.RunID)
	if transaction == nil {
		// ModelResultCommit can become canonical before a later session-level
		// cancellation resolves this run. Retain the exact bounded acknowledgement
		// so that the future transaction can prove the already-past commit instead
		// of waiting forever for an event that will not be replayed.
		if outcome.Kind != interactionelements.ModelCommitted {
			return nil
		}
		if envelope.SessionID != runner.sessionID || envelope.RunID != outcome.RunID {
			return runner.refuse(ctx, envelope, "model_commit_outcome",
				"model_commit_identity_mismatch",
				"model commit outcome crossed the trusted mounted session or run")
		}
		if _, _, err := runner.canonicalModelCommitBatch(envelope, outcome); err != nil {
			return runner.refuse(ctx, envelope, "model_commit_outcome",
				"model_commit_not_canonical", err.Error())
		}
		if err := runner.rememberModelCommit(envelope); err != nil {
			return runner.refuse(ctx, envelope, "model_commit_outcome",
				"model_commit_identity_conflict", err.Error())
		}
		return nil
	}
	if envelope.SessionID != runner.sessionID || envelope.RunID != transaction.generationID ||
		outcome.RunID != transaction.generationID {
		return runner.incomplete(ctx, transaction, envelope, "model_commit_identity_mismatch",
			"model commit outcome crossed the exact session or generation transaction")
	}
	if outcome.Kind != interactionelements.ModelCommitted {
		return runner.incomplete(ctx, transaction, envelope, "model_result_not_committed", outcome.Message)
	}
	if err := runner.verifyModelCommitEvidence(transaction, envelope, outcome); err != nil {
		return runner.incomplete(ctx, transaction, envelope, "model_commit_not_canonical",
			err.Error())
	}
	if transaction.modelCommitDone {
		if !reflect.DeepEqual(transaction.modelCommitAuthorizer, envelope) {
			return runner.incomplete(ctx, transaction, envelope, "model_commit_identity_conflict",
				"a different acknowledgement attempted to rebind the retained model-commit authorizer")
		}
		if transaction.completion != nil {
			return runner.publishRetainedTerminal(ctx, transaction)
		}
		return nil
	}
	transaction.modelCommitDone = true
	transaction.modelCommitAuthorizer = envelope.Clone()
	if err := runner.advance(ctx, transaction); err != nil {
		return err
	}
	if runner.ready(transaction) {
		return runner.complete(ctx, transaction, envelope)
	}
	return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
		Kind: SessionCancellationProgress, Operation: "model_commit_ack",
		GenerationID: transaction.generationID, Code: "model_result_committed",
		Message: "the terminal model result is now canonical before action reconciliation",
	})
}

func (runner *cancellationCoordinatorRunner) acceptActionOutcome(
	ctx context.Context, envelope element.Envelope,
) error {
	if err := validateCancellationCoordinatorEnvelope("action outcome", envelope, true); err != nil {
		return runner.refuse(ctx, envelope, "action_outcome", "invalid_action_envelope", err.Error())
	}
	outcome, ok := actionOutcomePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(actionelements.OutcomeType()) {
		return runner.refuse(ctx, envelope, "action_outcome", "invalid_action_outcome",
			fmt.Sprintf("action outcome payload has type %T", envelope.Payload))
	}
	if err := validateActionOutcome(outcome); err != nil {
		return runner.refuse(ctx, envelope, "action_outcome", "invalid_action_outcome", err.Error())
	}
	transaction, target := runner.transactionForActionOutcome(envelope)
	if transaction == nil || target == nil {
		return nil
	}
	if envelope.SessionID != runner.sessionID || envelope.RunID != target.runID ||
		outcome.CallID != target.callID || !slices.Contains(runner.config.ActionAckStages, outcome.Stage) {
		return runner.incomplete(ctx, transaction, envelope, "action_identity_mismatch",
			"action outcome crossed the exact session, run, call, or configured stage")
	}
	if outcome.Operation != "cancel" || outcome.Crossed || !actionCancellationTerminal(outcome) {
		code := "action_cancel_unacknowledged"
		if outcome.Crossed {
			code = "action_already_crossed"
			return runner.permanentIncomplete(ctx, transaction, envelope, code,
				fmt.Sprintf("action cancellation at %s reached %s/%s", outcome.Stage, outcome.Kind, outcome.Code))
		}
		if outcome.Operation == "cancel" && actionCancellationPending(outcome) {
			return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
				Kind: SessionCancellationProgress, Operation: "action_ack",
				ActionRunID: target.runID, ActionCallID: target.callID,
				ActionStage: outcome.Stage, Code: "action_stage_pending",
				Message: fmt.Sprintf("action cancellation at %s remains pending: %s",
					outcome.Stage, outcome.Code),
			})
		}
		return runner.incomplete(ctx, transaction, envelope, code,
			fmt.Sprintf("action cancellation at %s reached %s/%s", outcome.Stage, outcome.Kind, outcome.Code))
	}
	if _, duplicate := target.acknowledged[outcome.Stage]; duplicate {
		if runner.ready(transaction) {
			return runner.complete(ctx, transaction, envelope)
		}
		return nil
	}
	target.acknowledged[outcome.Stage] = envelope.ItemID
	if runner.ready(transaction) {
		return runner.complete(ctx, transaction, envelope)
	}
	return runner.transition(ctx, transaction, envelope, SessionCancellationOutcome{
		Kind: SessionCancellationProgress, Operation: "action_ack",
		ActionRunID: target.runID, ActionCallID: target.callID,
		ActionStage: outcome.Stage, Code: "action_stage_acknowledged",
		Message: "one configured action stage acknowledged the exact call cancellation",
	})
}

func (runner *cancellationCoordinatorRunner) advance(
	ctx context.Context, transaction *cancellationTransaction,
) error {
	if transaction == nil || transaction.failureCode != "" || transaction.completion != nil ||
		!transaction.settlementDone || !transaction.producerDone {
		return nil
	}
	if !transaction.activationSent {
		if transaction.activation.ItemID == "" {
			envelope, err := runner.makeActivationCancel(transaction)
			if err != nil {
				return err
			}
			transaction.activation = envelope
		}
		if err := runner.sendExact(ctx, runner.ports.activationCancel, transaction.activation); err != nil {
			return err
		}
		transaction.activationSent = true
		return nil
	}
	if !transaction.activationDone {
		return nil
	}
	if transaction.generationID != "" && !transaction.modelSent {
		if transaction.model.ItemID == "" {
			envelope, err := runner.makeModelCancel(transaction)
			if err != nil {
				return err
			}
			transaction.model = envelope
		}
		if err := runner.sendExact(ctx, runner.ports.modelCancel, transaction.model); err != nil {
			return err
		}
		transaction.modelSent = true
		return nil
	}
	if transaction.generationID == "" {
		transaction.modelDone = true
		transaction.modelCommitDone = true
	}
	if !transaction.modelDone || transaction.modelCommitNeeded && !transaction.modelCommitDone {
		return nil
	}
	if !transaction.actionsDiscovered {
		targets, err := cancellationActionTargets(runner.store.Snapshot(), transaction.intent)
		if err != nil {
			transaction.failureCode = "action_identity_unresolved"
			return runner.incomplete(ctx, transaction, transaction.request,
				transaction.failureCode, err.Error())
		}
		for _, target := range targets {
			key := cancellationActionKey(target.runID, target.callID)
			target.acknowledged = make(map[string]string, len(runner.config.ActionAckStages))
			envelope, envelopeErr := runner.makeActionCancel(transaction, target)
			if envelopeErr != nil {
				return envelopeErr
			}
			target.envelope = envelope
			transaction.actions[key] = target
		}
		transaction.actionsDiscovered = true
	}
	keys := make([]string, 0, len(transaction.actions))
	for key := range transaction.actions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		target := transaction.actions[key]
		if target.sent {
			continue
		}
		if err := runner.sendExact(ctx, runner.ports.actionCancel, target.envelope); err != nil {
			return err
		}
		target.sent = true
	}
	return nil
}

func (runner *cancellationCoordinatorRunner) ready(
	transaction *cancellationTransaction,
) bool {
	if transaction == nil || transaction.failureCode != "" ||
		!transaction.settlementDone || transaction.settlementAuthorizer.ItemID == "" ||
		!transaction.producerDone || transaction.producerAuthorizer.ItemID == "" ||
		!transaction.activationDone || transaction.activationAuthorizer.ItemID == "" ||
		!transaction.modelDone ||
		transaction.generationID != "" && transaction.modelAuthorizer.ItemID == "" ||
		transaction.modelCommitNeeded && !transaction.modelCommitDone ||
		transaction.modelCommitNeeded && transaction.modelCommitAuthorizer.ItemID == "" ||
		!transaction.actionsDiscovered {
		return false
	}
	for _, target := range transaction.actions {
		if !target.sent || len(target.acknowledged) != len(runner.config.ActionAckStages) {
			return false
		}
		for _, stage := range runner.config.ActionAckStages {
			if target.acknowledged[stage] == "" {
				return false
			}
		}
	}
	return true
}

func (runner *cancellationCoordinatorRunner) complete(
	ctx context.Context, transaction *cancellationTransaction, cause element.Envelope,
) error {
	if transaction == nil || !runner.ready(transaction) {
		return errors.New("Realtime-CU cancellation transaction is not ready for completion")
	}
	if runner.transactions[transaction.id] != transaction ||
		runner.byRequest[transaction.request.ItemID] != transaction ||
		runner.byIntent[transaction.intent.TrajectoryItemID] != transaction {
		return errors.New("Realtime-CU cancellation transaction lost its active identity before completion")
	}
	if transaction.completion == nil {
		publication, err := runner.prepareTerminalPublication(
			transaction, cause, SessionCancellationOutcome{
				Kind: SessionCancellationCompleted, Operation: "complete",
				GenerationID: transaction.generationID,
				Code:         "all_required_acknowledgements",
				Message:      "settlement, activation, model, model commit, and action cancellation are quiescent",
			},
		)
		if err != nil {
			return err
		}
		transaction.completion = publication
	}
	return runner.publishRetainedTerminal(ctx, transaction)
}

func (runner *cancellationCoordinatorRunner) publishRetainedTerminal(
	ctx context.Context, transaction *cancellationTransaction,
) error {
	if transaction == nil || runner.transactions[transaction.id] != transaction ||
		runner.byRequest[transaction.request.ItemID] != transaction ||
		runner.byIntent[transaction.intent.TrajectoryItemID] != transaction {
		return errors.New("Realtime-CU cancellation transaction lost its active identity before terminal publication")
	}
	publication := transaction.completion
	if publication == nil {
		return errors.New("Realtime-CU cancellation transaction has no retained terminal publication")
	}
	var lastErr error
	for attempt := 0; attempt < maximumCancellationPublishTries &&
		(!publication.outcomePublished || !publication.statePublished); attempt++ {
		if !publication.outcomePublished {
			if err := runner.sendExact(ctx, runner.ports.outcome, publication.outcome); err != nil {
				lastErr = err
				if ctx.Err() != nil {
					return err
				}
				continue
			}
			publication.outcomePublished = true
		}
		if !publication.statePublished {
			if _, err := runner.ports.state.Broadcast(ctx, publication.state); err != nil {
				lastErr = err
				if ctx.Err() != nil {
					return err
				}
				continue
			}
			publication.statePublished = true
		}
	}
	if !publication.outcomePublished || !publication.statePublished {
		return fmt.Errorf("Realtime-CU cancellation terminal publication failed after %d bounded attempts: %w",
			maximumCancellationPublishTries, lastErr)
	}

	// Retirement is deliberately last. Before both publications succeed, the
	// exact transaction remains addressable by a duplicate terminal
	// acknowledgement and no completed counter or tombstone is visible.
	delete(runner.transactions, transaction.id)
	delete(runner.byRequest, transaction.request.ItemID)
	if runner.byIntent[transaction.intent.TrajectoryItemID] == transaction {
		delete(runner.byIntent, transaction.intent.TrajectoryItemID)
	}
	runner.rememberTombstone(transaction.request.ItemID, cancellationTombstone{
		transactionID: transaction.id, requestDigest: transaction.requestDigest,
		kind: publication.kind,
	})
	runner.state = publication.projectedState
	runner.state.Tombstones = len(runner.tombstones)
	return nil
}

func (runner *cancellationCoordinatorRunner) prepareTerminalPublication(
	transaction *cancellationTransaction, cause element.Envelope,
	outcome SessionCancellationOutcome,
) (*cancellationCompletionPublication, error) {
	if outcome.Kind != SessionCancellationCompleted &&
		outcome.Kind != SessionCancellationIncomplete {
		return nil, fmt.Errorf("Realtime-CU cancellation terminal kind %q is unsupported", outcome.Kind)
	}
	projected := runner.projectTerminalState(transaction, outcome.Kind)
	outcome.TransactionID = transaction.id
	outcome.RequestItemID = transaction.request.ItemID
	outcome.SessionID = runner.sessionID
	outcome.DurableIntentItemID = transaction.intent.TrajectoryItemID
	if outcome.GenerationID == "" {
		outcome.GenerationID = transaction.generationID
	}
	pending := runner.pending(transaction)
	outcome.PendingSettlement = pending.settlement
	outcome.PendingProducer = pending.producer
	outcome.PendingActivation = pending.activation
	outcome.PendingModel = pending.model
	outcome.PendingModelCommit = pending.modelCommit
	outcome.PendingActionAcks = pending.actions
	outcome.StateRevisionBefore = runner.state.Revision
	outcome.StateRevisionAfter = projected.Revision
	outcome.FinishedNS = runner.clock.NowNS()
	if outcome.FinishedNS == 0 {
		return nil, errors.New("Realtime-CU cancellation coordinator clock returned zero")
	}
	outcomeSequence, err := runner.sequences.Next(runner.instance + ".outcome")
	if err != nil {
		return nil, err
	}
	outcomeEnvelope := cause.Clone()
	outcomeEnvelope.Type = SessionCancellationOutcomeType()
	outcomeEnvelope.ItemID = fmt.Sprintf(
		"%s:cancellation-outcome:%d", runner.instance, outcomeSequence,
	)
	if cause.ItemID != "" {
		outcomeEnvelope.CausalParents = appendUniqueString(
			outcomeEnvelope.CausalParents, cause.ItemID,
		)
	}
	outcomeEnvelope.CausalParents = appendUniqueString(
		outcomeEnvelope.CausalParents, transaction.request.ItemID,
	)
	outcomeEnvelope.SessionID = runner.sessionID
	outcomeEnvelope.Payload = outcome

	stateSequence, err := runner.sequences.Next(runner.instance + ".state")
	if err != nil {
		return nil, err
	}
	stateEnvelope := element.Envelope{
		Type:      SessionCancellationStateType(),
		ItemID:    fmt.Sprintf("%s:cancellation-state:%d", runner.instance, stateSequence),
		SessionID: runner.sessionID, Sequence: stateSequence,
		TraceID:       firstCanonical(outcomeEnvelope.TraceID, transaction.id),
		CausalParents: []string{outcomeEnvelope.ItemID}, Payload: projected,
	}
	return &cancellationCompletionPublication{
		outcome: outcomeEnvelope, state: stateEnvelope, projectedState: projected,
		kind: outcome.Kind,
	}, nil
}

func (runner *cancellationCoordinatorRunner) projectTerminalState(
	terminal *cancellationTransaction, kind SessionCancellationOutcomeKind,
) SessionCancellationState {
	projected := runner.state
	projected.Revision++
	if kind == SessionCancellationCompleted {
		projected.Completed++
	} else {
		projected.Incomplete++
	}
	projected.PendingTransactions = 0
	projected.PendingSettlementAcks = 0
	projected.PendingProducerAcks = 0
	projected.PendingActivationAcks = 0
	projected.PendingModelAcks = 0
	projected.PendingModelCommitAcks = 0
	projected.PendingActionAcks = 0
	for _, transaction := range runner.transactions {
		if transaction == terminal {
			continue
		}
		projected.PendingTransactions++
		pending := runner.pending(transaction)
		if pending.settlement {
			projected.PendingSettlementAcks++
		}
		if pending.producer {
			projected.PendingProducerAcks++
		}
		if pending.activation {
			projected.PendingActivationAcks++
		}
		if pending.model {
			projected.PendingModelAcks++
		}
		if pending.modelCommit {
			projected.PendingModelCommitAcks++
		}
		projected.PendingActionAcks += pending.actions
	}
	projected.Tombstones = len(runner.tombstones)
	if _, retained := runner.tombstones[terminal.request.ItemID]; !retained &&
		projected.Tombstones < runner.config.TombstoneMemory {
		projected.Tombstones++
	}
	projected.Saturated = projected.PendingTransactions >= runner.config.MaxTransactions
	return projected
}

func (runner *cancellationCoordinatorRunner) incomplete(
	ctx context.Context, transaction *cancellationTransaction, cause element.Envelope,
	code, message string,
) error {
	runner.state.Incomplete++
	return runner.transition(ctx, transaction, cause, SessionCancellationOutcome{
		Kind: SessionCancellationIncomplete, Operation: "acknowledge",
		Code: code, Message: boundedReason(message),
	})
}

func (runner *cancellationCoordinatorRunner) permanentIncomplete(
	ctx context.Context, transaction *cancellationTransaction, cause element.Envelope,
	code, message string,
) error {
	if transaction != nil && transaction.failureCode == "" {
		transaction.failureCode = code
	}
	if transaction == nil {
		return runner.incomplete(ctx, nil, cause, code, message)
	}
	if transaction.completion == nil {
		publication, err := runner.prepareTerminalPublication(
			transaction, cause, SessionCancellationOutcome{
				Kind: SessionCancellationIncomplete, Operation: "acknowledge",
				Code: code, Message: boundedReason(message),
			},
		)
		if err != nil {
			return err
		}
		transaction.completion = publication
	}
	return runner.publishRetainedTerminal(ctx, transaction)
}

func (runner *cancellationCoordinatorRunner) refuse(
	ctx context.Context, cause element.Envelope, operation, code, message string,
) error {
	runner.state.Refused++
	projected := canonicalCancellationCoordinatorCause(cause, runner.sessionID)
	return runner.transition(ctx, nil, projected, SessionCancellationOutcome{
		Kind: SessionCancellationRefused, Operation: operation,
		RequestItemID: projected.ItemID, SessionID: runner.sessionID,
		Code: code, Message: boundedReason(message),
	})
}

func (runner *cancellationCoordinatorRunner) transition(
	ctx context.Context, transaction *cancellationTransaction, cause element.Envelope,
	outcome SessionCancellationOutcome,
) error {
	before := runner.state.Revision
	runner.state.Revision++
	runner.refreshState()
	if transaction != nil {
		outcome.TransactionID = transaction.id
		outcome.RequestItemID = transaction.request.ItemID
		outcome.SessionID = runner.sessionID
		outcome.DurableIntentItemID = transaction.intent.TrajectoryItemID
		if outcome.GenerationID == "" {
			outcome.GenerationID = transaction.generationID
		}
		pending := runner.pending(transaction)
		outcome.PendingSettlement = pending.settlement
		outcome.PendingProducer = pending.producer
		outcome.PendingActivation = pending.activation
		outcome.PendingModel = pending.model
		outcome.PendingModelCommit = pending.modelCommit
		outcome.PendingActionAcks = pending.actions
	}
	outcome.StateRevisionBefore = before
	outcome.StateRevisionAfter = runner.state.Revision
	outcome.FinishedNS = runner.clock.NowNS()
	if outcome.FinishedNS == 0 {
		return errors.New("Realtime-CU cancellation coordinator clock returned zero")
	}
	sequence, err := runner.sequences.Next(runner.instance + ".outcome")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = SessionCancellationOutcomeType()
	envelope.ItemID = fmt.Sprintf("%s:cancellation-outcome:%d", runner.instance, sequence)
	if cause.ItemID != "" {
		envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	}
	if transaction != nil {
		envelope.CausalParents = appendUniqueString(envelope.CausalParents, transaction.request.ItemID)
	}
	envelope.SessionID = runner.sessionID
	envelope.Payload = outcome
	if err := runner.sendExact(ctx, runner.ports.outcome, envelope); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

type cancellationPending struct {
	settlement, producer, activation, model, modelCommit bool
	actions                                              int
}

func (runner *cancellationCoordinatorRunner) pending(
	transaction *cancellationTransaction,
) cancellationPending {
	if transaction == nil {
		return cancellationPending{}
	}
	result := cancellationPending{
		settlement:  !transaction.settlementDone,
		producer:    !transaction.producerDone,
		activation:  transaction.settlementDone && transaction.producerDone && !transaction.activationDone,
		model:       transaction.activationDone && !transaction.modelDone,
		modelCommit: transaction.modelCommitNeeded && !transaction.modelCommitDone,
	}
	for _, target := range transaction.actions {
		result.actions += len(runner.config.ActionAckStages) - len(target.acknowledged)
	}
	return result
}

func (runner *cancellationCoordinatorRunner) refreshState() {
	runner.state.PendingTransactions = len(runner.transactions)
	runner.state.Tombstones = len(runner.tombstones)
	runner.state.PendingSettlementAcks = 0
	runner.state.PendingProducerAcks = 0
	runner.state.PendingActivationAcks = 0
	runner.state.PendingModelAcks = 0
	runner.state.PendingModelCommitAcks = 0
	runner.state.PendingActionAcks = 0
	for _, transaction := range runner.transactions {
		pending := runner.pending(transaction)
		if pending.settlement {
			runner.state.PendingSettlementAcks++
		}
		if pending.producer {
			runner.state.PendingProducerAcks++
		}
		if pending.activation {
			runner.state.PendingActivationAcks++
		}
		if pending.model {
			runner.state.PendingModelAcks++
		}
		if pending.modelCommit {
			runner.state.PendingModelCommitAcks++
		}
		runner.state.PendingActionAcks += pending.actions
	}
	runner.state.Saturated = len(runner.transactions) >= runner.config.MaxTransactions
}

func (runner *cancellationCoordinatorRunner) publishState(
	ctx context.Context, cause element.Envelope,
) error {
	runner.refreshState()
	sequence, err := runner.sequences.Next(runner.instance + ".state")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = SessionCancellationStateType()
	envelope.ItemID = fmt.Sprintf("%s:cancellation-state:%d", runner.instance, sequence)
	if cause.ItemID != "" {
		envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	}
	envelope.SessionID = runner.sessionID
	envelope.Payload = runner.state
	_, err = runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *cancellationCoordinatorRunner) sendExact(
	ctx context.Context, port element.OutputPort, envelope element.Envelope,
) error {
	result, err := port.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		return fmt.Errorf("Realtime-CU cancellation output %s delivered %d and dropped %d lanes",
			port.Name(), result.Delivered, result.Dropped)
	}
	return nil
}

func (runner *cancellationCoordinatorRunner) makeSettlementCancel(
	transaction *cancellationTransaction,
) (element.Envelope, error) {
	sequence, err := runner.sequences.Next(runner.instance + ".settlement-cancel")
	if err != nil {
		return element.Envelope{}, err
	}
	return element.Envelope{
		Type:   policyelements.IntentSettlementCancelType(),
		ItemID: transaction.id + ":settlement", SessionID: runner.sessionID,
		Sequence: sequence, TraceID: firstCanonical(transaction.request.TraceID, transaction.id),
		CancellationScope: transaction.intent.TrajectoryItemID,
		CausalParents:     []string{transaction.request.ItemID},
		Payload: policyelements.IntentSettlementCancellation{
			SessionID: runner.sessionID, DurableIntent: transaction.intent,
			Reason: transaction.reason,
		},
	}, nil
}

func (runner *cancellationCoordinatorRunner) makeActivationCancel(
	transaction *cancellationTransaction,
) (element.Envelope, error) {
	if transaction.settlementAuthorizer.ItemID == "" ||
		transaction.producerAuthorizer.ItemID == "" {
		return element.Envelope{}, errors.New(
			"activation cancellation requires exact gate and producer terminal authorizers")
	}
	sequence, err := runner.sequences.Next(runner.instance + ".activation-cancel")
	if err != nil {
		return element.Envelope{}, err
	}
	intent := transaction.intent
	return element.Envelope{
		Type:   policyelements.GenerationCancelType(),
		ItemID: transaction.id + ":activation", SessionID: runner.sessionID,
		Sequence: sequence, TraceID: firstCanonical(transaction.request.TraceID, transaction.id),
		CancellationScope: intent.TrajectoryItemID,
		CausalParents: []string{
			transaction.request.ItemID, transaction.settlement.ItemID,
			transaction.settlementAuthorizer.ItemID, transaction.producerAuthorizer.ItemID,
		},
		Payload: policyelements.GenerationCancel{
			StreamID: runner.sessionID, Reason: transaction.reason, DurableIntent: &intent,
		},
	}, nil
}

func (runner *cancellationCoordinatorRunner) makeModelCancel(
	transaction *cancellationTransaction,
) (element.Envelope, error) {
	if transaction.activationAuthorizer.ItemID == "" {
		return element.Envelope{}, errors.New(
			"model cancellation requires the exact activation acknowledgement")
	}
	sequence, err := runner.sequences.Next(runner.instance + ".model-cancel")
	if err != nil {
		return element.Envelope{}, err
	}
	return element.Envelope{
		Type: cognitionelements.CancelType(), ItemID: transaction.id + ":model",
		SessionID: runner.sessionID, RunID: transaction.generationID,
		Sequence: sequence, TraceID: firstCanonical(transaction.request.TraceID, transaction.id),
		CancellationScope: transaction.generationID,
		CausalParents: []string{
			transaction.request.ItemID, transaction.activation.ItemID,
			transaction.activationAuthorizer.ItemID,
		},
		Payload: cognitionelements.Cancel{
			RunID: transaction.generationID, Reason: transaction.reason,
		},
	}, nil
}

func (runner *cancellationCoordinatorRunner) makeActionCancel(
	transaction *cancellationTransaction, target *cancellationActionTarget,
) (element.Envelope, error) {
	if transaction.modelAuthorizer.ItemID == "" {
		return element.Envelope{}, errors.New(
			"action cancellation requires the exact terminal model acknowledgement")
	}
	if transaction.modelCommitNeeded && transaction.modelCommitAuthorizer.ItemID == "" {
		return element.Envelope{}, errors.New(
			"action cancellation requires the exact model-commit acknowledgement")
	}
	sequence, err := runner.sequences.Next(runner.instance + ".action-cancel")
	if err != nil {
		return element.Envelope{}, err
	}
	hash := sha256.Sum256([]byte(target.runID + "\x00" + target.callID))
	parents := []string{
		transaction.request.ItemID, transaction.activation.ItemID,
		transaction.modelAuthorizer.ItemID,
	}
	if transaction.model.ItemID != "" {
		parents = append(parents, transaction.model.ItemID)
	}
	if transaction.modelCommitNeeded {
		parents = append(parents, transaction.modelCommitAuthorizer.ItemID)
	}
	return element.Envelope{
		Type:      actionelements.InterruptType(),
		ItemID:    transaction.id + ":action:" + hex.EncodeToString(hash[:8]),
		SessionID: runner.sessionID, RunID: target.runID, Sequence: sequence,
		TraceID:           firstCanonical(transaction.request.TraceID, transaction.id),
		CancellationScope: target.runID, CausalParents: parents,
		Payload: actionelements.Interrupt{CallID: target.callID, Reason: transaction.reason},
	}, nil
}

func (runner *cancellationCoordinatorRunner) transactionForSettlementOutcome(
	envelope element.Envelope, outcome policyelements.IntentSettlementOutcome,
) *cancellationTransaction {
	if outcome.Operation == "cancel" {
		return runner.transactionForDirectParent(envelope, func(candidate *cancellationTransaction) string {
			if candidate.settlementSent {
				return candidate.settlement.ItemID
			}
			return ""
		})
	}
	if outcome.Operation == "ack" {
		candidate := runner.byIntent[outcome.DurableIntentItemID]
		if candidate != nil && candidate.settlementSeen {
			return candidate
		}
	}
	return nil
}

func (runner *cancellationCoordinatorRunner) transactionForDirectParent(
	envelope element.Envelope, item func(*cancellationTransaction) string,
) *cancellationTransaction {
	var matched *cancellationTransaction
	for _, candidate := range runner.transactions {
		wanted := item(candidate)
		if wanted == "" || !slices.Contains(envelope.CausalParents, wanted) {
			continue
		}
		if matched != nil {
			return nil
		}
		matched = candidate
	}
	return matched
}

func (runner *cancellationCoordinatorRunner) transactionForGeneration(
	runID string,
) *cancellationTransaction {
	if runID == "" {
		return nil
	}
	var matched *cancellationTransaction
	for _, candidate := range runner.transactions {
		if candidate.generationID != runID {
			continue
		}
		if matched != nil {
			return nil
		}
		matched = candidate
	}
	return matched
}

func (runner *cancellationCoordinatorRunner) transactionForModelOutcome(
	envelope element.Envelope, runID string,
) *cancellationTransaction {
	if direct := runner.transactionForDirectParent(envelope, func(candidate *cancellationTransaction) string {
		if candidate.modelSent {
			return candidate.model.ItemID
		}
		return ""
	}); direct != nil {
		return direct
	}
	return runner.transactionForGeneration(runID)
}

func (runner *cancellationCoordinatorRunner) transactionForActionOutcome(
	envelope element.Envelope,
) (*cancellationTransaction, *cancellationActionTarget) {
	var matchedTransaction *cancellationTransaction
	var matchedTarget *cancellationActionTarget
	for _, transaction := range runner.transactions {
		for _, target := range transaction.actions {
			if !target.sent || !slices.Contains(envelope.CausalParents, target.envelope.ItemID) {
				continue
			}
			if matchedTransaction != nil {
				return nil, nil
			}
			matchedTransaction, matchedTarget = transaction, target
		}
	}
	return matchedTransaction, matchedTarget
}

func (runner *cancellationCoordinatorRunner) modelResultCommitted(
	transaction *cancellationTransaction,
) bool {
	if transaction == nil || transaction.generationID == "" {
		return false
	}
	snapshot := runner.store.Snapshot()
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return false
	}
	for _, item := range snapshot.Items {
		// ModelResultCommit always starts one canonical batch with a runtime
		// instruction item for the exact run. Tool calls/results can reuse an
		// invocation ID, so accepting any invocation-bearing item would let an
		// unrelated action masquerade as model-result commit evidence.
		if item.InvocationID == transaction.generationID &&
			item.Kind == trajectory.KindInstruction &&
			item.Producer.Phase == trajectory.PhaseRuntime &&
			item.SourceRevision == transaction.intent.SourceRevision &&
			trajectoryCausalAncestor(snapshot.Items,
				transaction.intent.TrajectoryItemID, item.ID) {
			return true
		}
	}
	return false
}

// verifyModelCommitEvidence binds a nominal ModelCommitted outcome back to the
// exact append transaction and immutable canonical batch that produced it.
// Run identity alone is insufficient: tool and result records can legitimately
// reuse an invocation ID, and a stale or partial acknowledgement must never
// authorize downstream action cancellation.
func (runner *cancellationCoordinatorRunner) verifyModelCommitEvidence(
	transaction *cancellationTransaction, envelope element.Envelope,
	outcome interactionelements.ModelCommitOutcome,
) error {
	if transaction == nil || transaction.generationID == "" {
		return errors.New("model commit has no addressed cancellation transaction")
	}
	if outcome.RunID != transaction.generationID {
		return errors.New("model commit envelope and payload must name the exact generation")
	}
	snapshot, batch, err := runner.canonicalModelCommitBatch(envelope, outcome)
	if err != nil {
		return err
	}
	first := batch[0]
	if first.SourceRevision != transaction.intent.SourceRevision ||
		!trajectoryCausalAncestor(snapshot.Items,
			transaction.intent.TrajectoryItemID, first.ID) {
		return errors.New("model commit batch is not rooted in the exact durable intent")
	}
	return nil
}

func (runner *cancellationCoordinatorRunner) canonicalModelCommitBatch(
	envelope element.Envelope, outcome interactionelements.ModelCommitOutcome,
) (trajectory.Snapshot, []trajectory.Item, error) {
	if envelope.RunID == "" || envelope.RunID != outcome.RunID {
		return trajectory.Snapshot{}, nil,
			errors.New("model commit envelope and payload must name the same generation")
	}
	if outcome.RequestID == "" || !slices.Contains(envelope.CausalParents, outcome.RequestID) {
		return trajectory.Snapshot{}, nil,
			errors.New("model commit outcome does not directly name its append request")
	}
	if outcome.StoreVersion == 0 || len(outcome.ItemIDs) == 0 {
		return trajectory.Snapshot{}, nil,
			errors.New("model commit outcome has no positive canonical batch boundary")
	}

	snapshot := runner.store.Snapshot()
	if snapshot.Version != uint64(len(snapshot.Items)) || outcome.StoreVersion > snapshot.Version {
		return trajectory.Snapshot{}, nil,
			errors.New("model commit outcome exceeds an inconsistent canonical trajectory")
	}
	count := uint64(len(outcome.ItemIDs))
	if count > outcome.StoreVersion {
		return trajectory.Snapshot{}, nil,
			errors.New("model commit batch starts before the canonical trajectory")
	}
	start := int(outcome.StoreVersion - count)
	end := int(outcome.StoreVersion)
	batch := snapshot.Items[start:end]
	canonicalIDs := make([]string, len(batch))
	for index := range batch {
		canonicalIDs[index] = batch[index].ID
	}
	if !slices.Equal(outcome.ItemIDs, canonicalIDs) {
		return trajectory.Snapshot{}, nil,
			fmt.Errorf("model commit item IDs %v do not attest canonical batch %v",
				outcome.ItemIDs, canonicalIDs)
	}

	first := batch[0]
	if first.Kind != trajectory.KindInstruction ||
		first.Producer.Phase != trajectory.PhaseRuntime ||
		first.InvocationID != outcome.RunID {
		return trajectory.Snapshot{}, nil,
			errors.New("model commit batch does not start with the exact runtime instruction")
	}
	previous := first.ID
	for index := 1; index < len(batch); index++ {
		item := batch[index]
		if !modelResultItemKind(item.Kind) ||
			item.InvocationID != outcome.RunID ||
			item.SourceRevision != first.SourceRevision ||
			len(item.CausalParentIDs) != 1 || item.CausalParentIDs[0] != previous {
			return trajectory.Snapshot{}, nil,
				fmt.Errorf("model commit batch item %d is not an exact chained result item", index)
		}
		previous = item.ID
	}
	// ModelResultCommit appends one complete batch atomically. If the very next
	// canonical item is another chained model-result item for this run, the
	// acknowledgement truncated that batch and cannot be used as authority.
	if end < len(snapshot.Items) {
		next := snapshot.Items[end]
		if modelResultItemKind(next.Kind) &&
			next.InvocationID == outcome.RunID &&
			next.SourceRevision == first.SourceRevision &&
			len(next.CausalParentIDs) == 1 && next.CausalParentIDs[0] == previous {
			return trajectory.Snapshot{}, nil,
				errors.New("model commit outcome attests only a strict prefix of the canonical result batch")
		}
	}
	return snapshot, batch, nil
}

func modelResultItemKind(kind trajectory.Kind) bool {
	switch kind {
	case trajectory.KindReasoning, trajectory.KindAssistant, trajectory.KindToolProposal:
		return true
	default:
		return false
	}
}

func (runner *cancellationCoordinatorRunner) rememberModelCommit(
	envelope element.Envelope,
) error {
	if runner.modelCommits == nil {
		runner.modelCommits = make(map[string]element.Envelope)
	}
	if retained, found := runner.modelCommits[envelope.RunID]; found {
		if reflect.DeepEqual(retained, envelope) {
			return nil
		}
		return fmt.Errorf("model run %q has conflicting canonical commit acknowledgements",
			envelope.RunID)
	}
	runner.modelCommits[envelope.RunID] = envelope.Clone()
	runner.modelCommitOrd = append(runner.modelCommitOrd, envelope.RunID)
	for len(runner.modelCommitOrd) > runner.config.TombstoneMemory {
		oldest := runner.modelCommitOrd[0]
		runner.modelCommitOrd = runner.modelCommitOrd[1:]
		delete(runner.modelCommits, oldest)
	}
	return nil
}

func (runner *cancellationCoordinatorRunner) adoptModelCommit(
	transaction *cancellationTransaction,
) error {
	if transaction == nil || transaction.generationID == "" || transaction.modelCommitDone {
		return nil
	}
	envelope, found := runner.modelCommits[transaction.generationID]
	if !found {
		return nil
	}
	outcome, ok := modelCommitOutcomePayload(envelope.Payload)
	if !ok || outcome.Kind != interactionelements.ModelCommitted {
		return errors.New("retained model commit acknowledgement changed type or terminal kind")
	}
	if err := runner.verifyModelCommitEvidence(transaction, envelope, outcome); err != nil {
		return err
	}
	transaction.modelCommitDone = true
	transaction.modelCommitAuthorizer = envelope.Clone()
	delete(runner.modelCommits, transaction.generationID)
	runner.modelCommitOrd = slices.DeleteFunc(runner.modelCommitOrd,
		func(runID string) bool { return runID == transaction.generationID })
	return nil
}

func (runner *cancellationCoordinatorRunner) rememberTombstone(
	requestItemID string, tombstone cancellationTombstone,
) {
	if _, found := runner.tombstones[requestItemID]; found {
		runner.tombstones[requestItemID] = tombstone
		return
	}
	runner.tombstones[requestItemID] = tombstone
	runner.tombstoneOrder = append(runner.tombstoneOrder, requestItemID)
	for len(runner.tombstoneOrder) > runner.config.TombstoneMemory {
		oldest := runner.tombstoneOrder[0]
		runner.tombstoneOrder = runner.tombstoneOrder[1:]
		delete(runner.tombstones, oldest)
	}
}

func cancellationActionTargets(
	snapshot trajectory.Snapshot,
	intent policyelements.TemporalEvidenceItemIdentity,
) ([]*cancellationActionTarget, error) {
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return nil, errors.New("canonical trajectory snapshot has inconsistent version and items")
	}
	if err := policyelements.VerifyIntentSettlementCancellation(snapshot,
		policyelements.IntentSettlementCancellation{
			SessionID: "cancellation-target-resolution", DurableIntent: intent,
		}); err != nil {
		return nil, err
	}
	terminalProposals, _ := trajectory.TerminalToolProposalIDs(snapshot)
	resolved := make(map[string]struct{})
	for _, item := range snapshot.Items {
		if item.InvocationID == "" {
			continue
		}
		switch item.Kind {
		case trajectory.KindToolResult:
			if item.ToolResult != nil {
				resolved[cancellationActionKey(item.InvocationID, item.ToolResult.CallID)] = struct{}{}
			}
		case trajectory.KindToolPlaceholder:
			if item.ToolPlaceholder != nil {
				resolved[cancellationActionKey(item.InvocationID, item.ToolPlaceholder.CallID)] = struct{}{}
			}
		}
	}
	targets := make(map[string]*cancellationActionTarget)
	for _, item := range snapshot.Items {
		if item.Kind != trajectory.KindToolProposal || item.ToolCall == nil ||
			item.SourceRevision != intent.SourceRevision || item.InvocationID == "" ||
			!trajectoryCausalAncestor(snapshot.Items, intent.TrajectoryItemID, item.ID) {
			continue
		}
		if _, terminal := terminalProposals[item.ID]; terminal {
			continue
		}
		key := cancellationActionKey(item.InvocationID, item.ToolCall.CallID)
		if _, terminal := resolved[key]; terminal {
			continue
		}
		targets[key] = &cancellationActionTarget{
			runID: item.InvocationID, callID: item.ToolCall.CallID,
		}
	}
	for _, pending := range trajectory.UnresolvedToolCalls(snapshot) {
		item, found := trajectoryItem(snapshot, pending.ItemID)
		if !found || pending.SourceRevision != intent.SourceRevision ||
			pending.InvocationID == "" ||
			!trajectoryCausalAncestor(snapshot.Items, intent.TrajectoryItemID, item.ID) {
			continue
		}
		key := cancellationActionKey(pending.InvocationID, pending.Call.CallID)
		targets[key] = &cancellationActionTarget{
			runID: pending.InvocationID, callID: pending.Call.CallID,
		}
	}
	keys := make([]string, 0, len(targets))
	for key := range targets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]*cancellationActionTarget, 0, len(keys))
	for _, key := range keys {
		result = append(result, targets[key])
	}
	return result, nil
}

func cancellationActionKey(runID, callID string) string {
	return runID + "\x00" + callID
}

// newestFinalUserIntent resolves exactly one identity from one immutable
// snapshot. A malformed newest final-authority candidate is an error; the
// coordinator never falls back to an older intent merely because it is easier
// to encode.
func newestFinalUserIntent(
	snapshot trajectory.Snapshot, sessionID string,
) (policyelements.TemporalEvidenceItemIdentity, bool, error) {
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return policyelements.TemporalEvidenceItemIdentity{}, false,
			errors.New("canonical trajectory snapshot has inconsistent version and items")
	}
	for index := len(snapshot.Items) - 1; index >= 0; index-- {
		item := snapshot.Items[index]
		if item.Kind != trajectory.KindObservation ||
			trajectory.AuthorityOf(item) != trajectory.AuthorityUser ||
			item.Producer.Phase != trajectory.PhaseUser {
			continue
		}
		// The newest user-authority observation is the only candidate. If it is
		// not a timestamped endpoint, fail closed instead of silently falling back
		// to an older intent and canceling the wrong authority epoch.
		if item.Event == nil || item.Event.Type != item.Event.Source+".endpoint" ||
			item.Event.OccurredNS == 0 {
			return policyelements.TemporalEvidenceItemIdentity{}, false,
				fmt.Errorf("newest user authority %q is not a final timestamped endpoint", item.ID)
		}
		identity := policyelements.TemporalEvidenceItemIdentity{
			TrajectoryItemID: item.ID, TriggerItemID: item.Event.EventID,
			StoreVersion: uint64(index + 1), SourceRevision: item.SourceRevision,
			OccurredNS: item.Event.OccurredNS, Authority: trajectory.AuthorityUser,
			Observer: item.Event.Source, Source: item.Event.Channel,
		}
		if item.Observation != nil {
			identity.Observer = item.Observation.Observer
			identity.Source = item.Observation.Source
		}
		if err := policyelements.VerifyIntentSettlementCancellation(snapshot,
			policyelements.IntentSettlementCancellation{
				SessionID: sessionID, DurableIntent: identity,
			}); err != nil {
			return policyelements.TemporalEvidenceItemIdentity{}, false,
				fmt.Errorf("newest final user authority %q: %w", item.ID, err)
		}
		return identity, true, nil
	}
	return policyelements.TemporalEvidenceItemIdentity{}, false, nil
}

func validateSessionCancellationRequest(
	mountedSession string, envelope element.Envelope, request SessionCancellation,
) error {
	if !boundedActivationIdentifier(request.SessionID, true) {
		return errors.New("session cancellation requires canonical item and session identities")
	}
	if envelope.SessionID != mountedSession || request.SessionID != mountedSession {
		return errors.New("session cancellation crossed the trusted mounted session")
	}
	if envelope.CancellationScope != mountedSession {
		return errors.New("session cancellation scope must equal the trusted mounted session")
	}
	if envelope.Sequence == 0 {
		return errors.New("session cancellation requires a positive boundary sequence")
	}
	if len(request.Reason) > maximumCancellationReasonBytes || !utf8.ValidString(request.Reason) {
		return fmt.Errorf("session cancellation reason exceeds %d bytes or is not valid UTF-8",
			maximumCancellationReasonBytes)
	}
	return nil
}

func validateIntentSettlementOutcome(outcome policyelements.IntentSettlementOutcome) error {
	switch outcome.Kind {
	case policyelements.IntentSettlementAdmitted, policyelements.IntentSettlementHeld,
		policyelements.IntentSettlementSuppressed, policyelements.IntentSettlementRefused,
		policyelements.IntentSettlementReset, policyelements.IntentSettlementCanceled,
		policyelements.IntentSettlementIgnored:
	default:
		return fmt.Errorf("settlement outcome kind %q is not closed", outcome.Kind)
	}
	switch outcome.Operation {
	case "evidence", "disposition", "ack", "reset", "cancel":
	default:
		return fmt.Errorf("settlement outcome operation %q is not closed", outcome.Operation)
	}
	if outcome.Disposition != "" {
		switch outcome.Disposition {
		case policyelements.IntentDispositionContinue, policyelements.IntentDispositionSucceeded,
			policyelements.IntentDispositionFailed, policyelements.IntentDispositionIndeterminate:
		default:
			return fmt.Errorf("settlement disposition %q is not closed", outcome.Disposition)
		}
	}
	if err := validateCancellationPayloadIdentifiers("settlement outcome",
		cancellationPayloadIdentifier{"session ID", outcome.SessionID, true},
		cancellationPayloadIdentifier{"durable intent item ID", outcome.DurableIntentItemID, false},
		cancellationPayloadIdentifier{"trigger observation item ID", outcome.TriggerObservationItemID, false},
		cancellationPayloadIdentifier{"result item ID", outcome.ResultItemID, false},
		cancellationPayloadIdentifier{"probe ID", outcome.ProbeID, false},
		cancellationPayloadIdentifier{"code", outcome.Code, false}); err != nil {
		return err
	}
	return validateCancellationPayloadMessage("settlement outcome", outcome.Message)
}

func validateGenerationOutcome(outcome policyelements.GenerationOutcome) error {
	switch outcome.Kind {
	case policyelements.GenerationEmitted, policyelements.GenerationCanceled,
		policyelements.GenerationRefused, policyelements.GenerationIgnored:
	default:
		return fmt.Errorf("activation outcome kind %q is not closed", outcome.Kind)
	}
	if err := validateCancellationPayloadIdentifiers("activation outcome",
		cancellationPayloadIdentifier{"generation ID", outcome.GenerationID, false},
		cancellationPayloadIdentifier{"role", outcome.Role, true},
		cancellationPayloadIdentifier{"stream ID", outcome.StreamID, false},
		cancellationPayloadIdentifier{"trigger item ID", outcome.TriggerItemID, false},
		cancellationPayloadIdentifier{"code", outcome.Code, false}); err != nil {
		return err
	}
	return validateCancellationPayloadMessage("activation outcome", outcome.Message)
}

func validateCognitionOutcome(outcome cognitionelements.Outcome) error {
	switch outcome.Kind {
	case cognitionelements.OutcomeSucceeded, cognitionelements.OutcomeCanceled,
		cognitionelements.OutcomeRefused, cognitionelements.OutcomeFailed,
		cognitionelements.OutcomeIgnored:
	default:
		return fmt.Errorf("model outcome kind %q is not closed", outcome.Kind)
	}
	if outcome.Operation != "generate" && outcome.Operation != "cancel" {
		return fmt.Errorf("model outcome operation %q is not closed", outcome.Operation)
	}
	if err := validateCancellationPayloadIdentifiers("model outcome",
		cancellationPayloadIdentifier{"run ID", outcome.RunID, true},
		cancellationPayloadIdentifier{"provider reference", outcome.ProviderReference, false},
		cancellationPayloadIdentifier{"code", outcome.Code, false}); err != nil {
		return err
	}
	if outcome.StartedNS != 0 && outcome.FinishedNS != 0 && outcome.FinishedNS < outcome.StartedNS {
		return errors.New("model outcome finished before it started")
	}
	return validateCancellationPayloadMessage("model outcome", outcome.Message)
}

func validateModelCommitOutcome(outcome interactionelements.ModelCommitOutcome) error {
	switch outcome.Kind {
	case interactionelements.ModelCommitted, interactionelements.ModelRejected,
		interactionelements.ModelRefused, interactionelements.ModelIgnored:
	default:
		return fmt.Errorf("model commit outcome kind %q is not closed", outcome.Kind)
	}
	if err := validateCancellationPayloadIdentifiers("model commit outcome",
		cancellationPayloadIdentifier{"run ID", outcome.RunID, true},
		cancellationPayloadIdentifier{"request ID", outcome.RequestID, false},
		cancellationPayloadIdentifier{"code", outcome.Code, false}); err != nil {
		return err
	}
	if len(outcome.ItemIDs) > maximumCancellationInputParents {
		return fmt.Errorf("model commit item IDs exceed %d entries", maximumCancellationInputParents)
	}
	seen := make(map[string]struct{}, len(outcome.ItemIDs))
	for index, itemID := range outcome.ItemIDs {
		if !boundedActivationIdentifier(itemID, true) {
			return fmt.Errorf("model commit item ID %d is invalid or oversized", index)
		}
		if _, duplicate := seen[itemID]; duplicate {
			return fmt.Errorf("model commit item ID %d is duplicated", index)
		}
		seen[itemID] = struct{}{}
	}
	return validateCancellationPayloadMessage("model commit outcome", outcome.Message)
}

func validateIntentDispositionProducerOutcome(
	outcome policyelements.IntentDispositionProducerOutcome,
) error {
	switch outcome.Kind {
	case policyelements.IntentDispositionProducerProduced,
		policyelements.IntentDispositionProducerRefused,
		policyelements.IntentDispositionProducerFailed,
		policyelements.IntentDispositionProducerCanceled,
		policyelements.IntentDispositionProducerIgnored:
	default:
		return fmt.Errorf("settlement producer outcome kind %q is not closed", outcome.Kind)
	}
	if outcome.Disposition != "" {
		switch outcome.Disposition {
		case policyelements.IntentDispositionContinue,
			policyelements.IntentDispositionSucceeded,
			policyelements.IntentDispositionFailed,
			policyelements.IntentDispositionIndeterminate:
		default:
			return fmt.Errorf("settlement producer disposition %q is not closed", outcome.Disposition)
		}
	}
	if err := validateCancellationPayloadIdentifiers("settlement producer outcome",
		cancellationPayloadIdentifier{"probe ID", outcome.ProbeID, false},
		cancellationPayloadIdentifier{"durable intent item ID", outcome.DurableIntentItemID, false},
		cancellationPayloadIdentifier{"disposition item ID", outcome.DispositionItemID, false},
		cancellationPayloadIdentifier{"code", outcome.Code, false}); err != nil {
		return err
	}
	if outcome.DecisionStartedNS != 0 && outcome.DecisionFinishedNS != 0 &&
		outcome.DecisionFinishedNS < outcome.DecisionStartedNS {
		return errors.New("settlement producer outcome finished before it started")
	}
	return validateCancellationPayloadMessage("settlement producer outcome", outcome.Message)
}

func validateActionOutcome(outcome actionelements.Outcome) error {
	switch outcome.Kind {
	case actionelements.OutcomeSucceeded, actionelements.OutcomeRejected,
		actionelements.OutcomeDenied, actionelements.OutcomeCanceled,
		actionelements.OutcomeTimedOut, actionelements.OutcomeFailed,
		actionelements.OutcomeIgnored:
	default:
		return fmt.Errorf("action outcome kind %q is not closed", outcome.Kind)
	}
	if err := validateCancellationPayloadIdentifiers("action outcome",
		cancellationPayloadIdentifier{"stage", outcome.Stage, true},
		cancellationPayloadIdentifier{"operation", outcome.Operation, true},
		cancellationPayloadIdentifier{"call ID", outcome.CallID, false},
		cancellationPayloadIdentifier{"code", outcome.Code, false},
		cancellationPayloadIdentifier{"result digest", outcome.ResultDigest, false},
		cancellationPayloadIdentifier{"ingress item ID", outcome.IngressItemID, false},
		cancellationPayloadIdentifier{"accepted item ID", outcome.AcceptedItemID, false},
		cancellationPayloadIdentifier{"canonical envelope item ID", outcome.CanonicalEnvelopeItemID, false},
		cancellationPayloadIdentifier{"canonical trajectory item ID", outcome.CanonicalTrajectoryItemID, false}); err != nil {
		return err
	}
	if outcome.StartedNS != 0 && outcome.FinishedNS != 0 && outcome.FinishedNS < outcome.StartedNS {
		return errors.New("action outcome finished before it started")
	}
	return validateCancellationPayloadMessage("action outcome", outcome.Message)
}

type cancellationPayloadIdentifier struct {
	label    string
	value    string
	required bool
}

func validateCancellationPayloadIdentifiers(
	label string, fields ...cancellationPayloadIdentifier,
) error {
	for _, field := range fields {
		if !boundedActivationIdentifier(field.value, field.required) {
			return fmt.Errorf("%s %s is invalid or oversized", label, field.label)
		}
	}
	return nil
}

func validateCancellationPayloadMessage(label, message string) error {
	if len(message) > maximumCancellationReasonBytes || !utf8.ValidString(message) {
		return fmt.Errorf("%s message exceeds %d bytes or is not valid UTF-8",
			label, maximumCancellationReasonBytes)
	}
	return nil
}

// validateCancellationCoordinatorEnvelope bounds every metadata field that
// can be retained in a transaction or copied into an outcome. Payloads have
// their own closed validators. Two parent slots remain available for the
// request and the stage-specific control when an outcome is projected.
func validateCancellationCoordinatorEnvelope(
	label string, envelope element.Envelope, requireSequence bool,
) error {
	for _, identity := range []struct {
		field    string
		value    string
		required bool
	}{
		{"item ID", envelope.ItemID, true},
		{"session ID", envelope.SessionID, true},
		{"source ID", envelope.SourceID, false},
		{"opportunity ID", envelope.OpportunityID, false},
		{"run ID", envelope.RunID, false},
		{"trace ID", envelope.TraceID, false},
		{"cancellation scope", envelope.CancellationScope, false},
	} {
		if !boundedActivationIdentifier(identity.value, identity.required) {
			return fmt.Errorf("%s %s is invalid or oversized", label, identity.field)
		}
	}
	if requireSequence && envelope.Sequence == 0 {
		return fmt.Errorf("%s requires a positive sequence", label)
	}
	if len(envelope.CausalParents) > maximumCancellationInputParents {
		return fmt.Errorf("%s causal parents exceed %d entries",
			label, maximumCancellationInputParents)
	}
	seen := make(map[string]struct{}, len(envelope.CausalParents))
	for index, parent := range envelope.CausalParents {
		if !boundedActivationIdentifier(parent, true) || parent == envelope.ItemID {
			return fmt.Errorf("%s causal parent %d is invalid", label, index)
		}
		if _, duplicate := seen[parent]; duplicate {
			return fmt.Errorf("%s causal parent %d is duplicated", label, index)
		}
		seen[parent] = struct{}{}
	}
	return nil
}

func canonicalCancellationCoordinatorCause(
	cause element.Envelope, mountedSession string,
) element.Envelope {
	if validateCancellationCoordinatorEnvelope("refusal", cause, false) == nil &&
		cause.SessionID == mountedSession {
		return cause.Clone()
	}
	return element.Envelope{
		Type: cause.Type.Clone(), ItemID: invalidCancellationInputItemID,
		SessionID: mountedSession,
	}
}

func cancellationRequestDigest(envelope element.Envelope, request SessionCancellation) string {
	payload, _ := json.Marshal(struct {
		ItemID, SessionID, CancellationScope string
		Sequence                             uint64
		Request                              SessionCancellation
	}{
		ItemID: envelope.ItemID, SessionID: envelope.SessionID,
		CancellationScope: envelope.CancellationScope, Sequence: envelope.Sequence,
		Request: request,
	})
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func cancellationTransactionID(
	sessionID, requestItemID string,
	intent policyelements.TemporalEvidenceItemIdentity,
	snapshotVersion uint64,
) string {
	payload, _ := json.Marshal(struct {
		SessionID, RequestItemID string
		Intent                   policyelements.TemporalEvidenceItemIdentity
		SnapshotVersion          uint64
	}{sessionID, requestItemID, intent, snapshotVersion})
	digest := sha256.Sum256(payload)
	return "realtime-cu-cancel:sha256:" + hex.EncodeToString(digest[:])
}

func settlementCancellationTerminal(outcome policyelements.IntentSettlementOutcome) bool {
	if outcome.Kind == policyelements.IntentSettlementCanceled {
		return true
	}
	if outcome.Kind != policyelements.IntentSettlementIgnored {
		return false
	}
	switch outcome.Code {
	case "cancellation_recorded", "duplicate_cancellation", "superseded_cancellation":
		return true
	default:
		return false
	}
}

func modelOutcomeTerminal(outcome cognitionelements.Outcome) bool {
	// Control acknowledgements are not provider-quiescence evidence. In
	// particular, TextModel can emit cancel/already_canceled while the first
	// addressed provider call is still unwinding. Only the terminal outcome of
	// the generation itself proves that no provider work remains.
	if outcome.Operation != "generate" {
		return false
	}
	switch outcome.Kind {
	case cognitionelements.OutcomeSucceeded, cognitionelements.OutcomeCanceled,
		cognitionelements.OutcomeRefused, cognitionelements.OutcomeFailed:
		return true
	default:
		return false
	}
}

func modelOutcomeHasPreparedResult(outcome cognitionelements.Outcome) bool {
	if outcome.Operation != "generate" {
		return false
	}
	if outcome.Code == "canceled_before_start" || outcome.StartedNS == 0 &&
		outcome.Kind == cognitionelements.OutcomeRefused {
		return false
	}
	return true
}

func actionCancellationPending(outcome actionelements.Outcome) bool {
	if outcome.Kind != actionelements.OutcomeIgnored {
		return false
	}
	switch outcome.Code {
	case "cancellation_pending_commit", "cancellation_already_pending", "result_must_commit":
		return true
	default:
		return false
	}
}

func actionCancellationTerminal(outcome actionelements.Outcome) bool {
	if outcome.Crossed || actionCancellationPending(outcome) {
		return false
	}
	switch outcome.Kind {
	case actionelements.OutcomeCanceled:
		switch outcome.Code {
		case "", "canceled", "preempted", "pre_canceled",
			"canceled_before_commit", "canceled_before_ledger":
			return true
		default:
			return false
		}
	case actionelements.OutcomeIgnored:
		return outcome.Code == "already_terminal" || outcome.Code == "already_preempted"
	default:
		return false
	}
}

func sessionCancellationPayload(payload any) (SessionCancellation, bool) {
	switch value := payload.(type) {
	case SessionCancellation:
		return value, true
	case *SessionCancellation:
		if value != nil {
			return *value, true
		}
	}
	return SessionCancellation{}, false
}

func intentSettlementOutcomePayload(payload any) (policyelements.IntentSettlementOutcome, bool) {
	switch value := payload.(type) {
	case policyelements.IntentSettlementOutcome:
		return value, true
	case *policyelements.IntentSettlementOutcome:
		if value != nil {
			return *value, true
		}
	}
	return policyelements.IntentSettlementOutcome{}, false
}

func intentDispositionProducerOutcomePayload(
	payload any,
) (policyelements.IntentDispositionProducerOutcome, bool) {
	switch value := payload.(type) {
	case policyelements.IntentDispositionProducerOutcome:
		return value, true
	case *policyelements.IntentDispositionProducerOutcome:
		if value != nil {
			return *value, true
		}
	}
	return policyelements.IntentDispositionProducerOutcome{}, false
}

func cognitionOutcomePayload(payload any) (cognitionelements.Outcome, bool) {
	switch value := payload.(type) {
	case cognitionelements.Outcome:
		return value, true
	case *cognitionelements.Outcome:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.Outcome{}, false
}

func modelCommitOutcomePayload(payload any) (interactionelements.ModelCommitOutcome, bool) {
	switch value := payload.(type) {
	case interactionelements.ModelCommitOutcome:
		return value, true
	case *interactionelements.ModelCommitOutcome:
		if value != nil {
			return *value, true
		}
	}
	return interactionelements.ModelCommitOutcome{}, false
}

func actionOutcomePayload(payload any) (actionelements.Outcome, bool) {
	switch value := payload.(type) {
	case actionelements.Outcome:
		return value, true
	case *actionelements.Outcome:
		if value != nil {
			return *value, true
		}
	}
	return actionelements.Outcome{}, false
}
