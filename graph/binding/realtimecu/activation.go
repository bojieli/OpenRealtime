package realtimecu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	ActivationReference       = "policy.RealtimeComputerUseActivation"
	activationRuntimeID       = "go://github.com/bojieli/OpenRealtime/graph/binding/realtimecu/activation/v4"
	defaultTerminalMemory     = 512
	defaultCancellationMemory = 256
	maximumActivationMemory   = 1_000_000
	maximumInstructionBytes   = 1 << 20
)

// ActivationDescriptor keeps the latest committed user task as durable
// proposal authority while grounding cognition in each newest screen/camera
// prefix. Exactly one generation may be unsettled: a result with no proposal
// releases the next changed frame, while a proposed effect remains closed
// until a screen observation names its exact canonical tool result. A failed
// result terminally clears that selected intent at the same visual safe point;
// it cannot reopen cognition without a new canonical user task. The
// selected user item must be a canonical causal ancestor of the current visual
// tail; ProposalAdmission independently re-derives and checks that authority,
// so this element cannot weaken the shared effect contract.
func ActivationDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          ActivationReference,
		Revision:      4,
		Ports: []element.Port{
			{Name: "committed", Direction: element.Input,
				Type: stateelements.ObservationCommitOutcomeType(), Cardinality: element.One,
				Required: true, DefaultDepth: 32},
			{Name: "cancel", Direction: element.Input,
				Type: policyelements.GenerationCancelType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
			{Name: "result", Direction: element.Input,
				Type: cognitionelements.ResultType(), Cardinality: element.One,
				Required: true, DefaultDepth: 16},
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
		},
		Reaction: element.Reaction{
			Triggers: []string{"committed", "result"}, Interrupts: []string{"cancel"},
			Outcomes:       []string{"trigger", "authority", "state", "outcome"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/realtime-cu/durable-activation-state/v1",
		ConfigSchema: "schema://openrealtime/policy/generate-on-observation-config/v1",
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
	ports, err := activationPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &activationRunner{
		instance: mount.InstanceID, config: config, clock: clock, sequences: sequences,
		store: storeService.Store, resolution: mount.Resolution, ports: ports,
		terminal: make(map[string]struct{}),
		state: policyelements.GenerationState{
			Role: config.Role, TerminalMemory: config.TerminalMemory,
			CancellationMemory: config.CancelMemory,
		},
	}, nil
}

func decodeActivationConfig(source json.RawMessage) (policyelements.GenerateOnObservationConfig, error) {
	config := policyelements.GenerateOnObservationConfig{
		MaxPending: 64, TerminalMemory: defaultTerminalMemory,
		CancelMemory: defaultCancellationMemory,
	}
	if err := decodeExactJSON(source, &config); err != nil {
		return policyelements.GenerateOnObservationConfig{}, err
	}
	if !canonical(config.Role) || len(config.Role) > 256 {
		return policyelements.GenerateOnObservationConfig{}, errors.New("activation role must be a canonical identifier")
	}
	if config.MaxPending < 1 || config.MaxPending > maximumActivationMemory ||
		config.TerminalMemory < 1 || config.TerminalMemory > maximumActivationMemory ||
		config.CancelMemory < 1 || config.CancelMemory > maximumActivationMemory {
		return policyelements.GenerateOnObservationConfig{}, errors.New("activation memory bounds must be between 1 and 1000000")
	}
	if config.Invocation.SourceRevision != 0 {
		return policyelements.GenerateOnObservationConfig{}, errors.New("activation source revision is derived from canonical user authority")
	}
	if len(config.Invocation.Instruction) > maximumInstructionBytes ||
		!utf8.ValidString(config.Invocation.Instruction) {
		return policyelements.GenerateOnObservationConfig{}, errors.New("activation instruction must be bounded valid UTF-8")
	}
	if config.Invocation.MaxOutputTokens > 1_000_000 {
		return policyelements.GenerateOnObservationConfig{}, errors.New("activation maximum output tokens exceeds 1000000")
	}
	if config.Invocation.Effort != "" {
		parsed, err := continuation.ParseEffort(string(config.Invocation.Effort))
		if err != nil || parsed != config.Invocation.Effort {
			if err != nil {
				return policyelements.GenerateOnObservationConfig{}, err
			}
			return policyelements.GenerateOnObservationConfig{}, errors.New("activation effort is not canonical")
		}
	}
	validator := continuation.Descriptor{
		Provider: "realtime-cu-policy", Model: "config-validator",
		Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
	if err := continuation.ValidateInvocation(config.Invocation, validator); err != nil {
		return policyelements.GenerateOnObservationConfig{}, err
	}
	config.Invocation = cloneInvocation(config.Invocation)
	return config, nil
}

type activationPorts struct {
	committed, cancel, result          element.InputPort
	trigger, candidate, state, outcome element.OutputPort
}

func activationPortsFrom(ports element.Ports) (activationPorts, error) {
	if ports == nil {
		return activationPorts{}, errors.New("Realtime-CU activation has nil ports")
	}
	result := activationPorts{}
	for _, input := range []struct {
		name string
		set  *element.InputPort
	}{{"committed", &result.committed}, {"cancel", &result.cancel}, {"result", &result.result}} {
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
}

type activeGeneration struct {
	id             string
	contextVersion uint64
	callID         string
}

type activationRunner struct {
	instance   string
	config     policyelements.GenerateOnObservationConfig
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	store      *trajectory.Store
	resolution element.ResolutionReporter
	ports      activationPorts

	intent          *userIntentBasis
	active          *activeGeneration
	revokedSequence uint64
	terminal        map[string]struct{}
	terminalOrder   []string
	state           policyelements.GenerationState
}

type activationInput struct {
	kind     string
	envelope element.Envelope
}

func (runner *activationRunner) Run(parent context.Context) error {
	if err := reportElementRuntime(runner.resolution, activationRuntimeID,
		"implementation:4", ActivationDescriptor()); err != nil {
		return err
	}
	if err := runner.publishState(parent, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan activationInput)
	failures := make(chan error, 3)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{{"committed", runner.ports.committed}, {"cancel", runner.ports.cancel}, {"result", runner.ports.result}} {
		wait.Add(1)
		go receiveActivationInputs(ctx, source.kind, source.port, inputs, failures, &wait)
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
			case "committed":
				err = runner.acceptCommit(ctx, input.envelope)
			case "cancel":
				err = runner.acceptCancel(ctx, input.envelope)
			case "result":
				err = runner.acceptResult(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown Realtime-CU activation input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *activationRunner) acceptCommit(
	ctx context.Context, envelope element.Envelope,
) error {
	commit, ok := observationCommitOutcomePayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, stateelements.ObservationCommitOutcome{},
			"invalid_commit", fmt.Sprintf("commit payload has type %T", envelope.Payload))
	}
	if commit.Kind != stateelements.ObservationCommitted {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, commit, policyelements.GenerationOutcome{
			Kind: policyelements.GenerationIgnored, Role: runner.config.Role,
			StreamID: commit.StreamID, SourceRevision: commit.SourceRevision,
			ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
			Code:    "observation_not_committed",
			Message: "observation did not cross the canonical trajectory boundary",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	if !canonical(envelope.SessionID) || !canonical(commit.TrajectoryItemID) ||
		!canonical(commit.TriggerItemID) || commit.SourceRevision == 0 ||
		commit.StoreVersion == 0 || commit.Context.Prefix.Version != commit.StoreVersion {
		return runner.refuse(ctx, envelope, commit, "invalid_commit",
			"commit omits canonical session, observation, revision, or context identity")
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
	if err := trajectory.VerifyPrefix(snapshot, commit.Context.Prefix); err != nil {
		return runner.refuse(ctx, envelope, commit, "context_mismatch", err.Error())
	}
	prefix := snapshot.Items[:commit.StoreVersion]
	current, found := trajectoryItem(trajectory.Snapshot{Version: commit.StoreVersion, Items: prefix},
		commit.TrajectoryItemID)
	if !found || current.Kind != trajectory.KindObservation || current.Event == nil ||
		current.SourceRevision != commit.SourceRevision || current.Event.EventID != commit.TriggerItemID ||
		prefix[len(prefix)-1].ID != current.ID {
		return runner.refuse(ctx, envelope, commit, "invalid_observation_basis",
			"commit does not name the exact event-backed context tail")
	}
	authorityValue := trajectory.AuthorityOf(current)
	if authorityValue == trajectory.AuthorityUser {
		// ASR revisions are useful canonical evidence, but a revisable prefix is
		// not yet the participant's instruction. Clear an older durable intent
		// while a new utterance is provisional so visual cadence cannot reactivate
		// that older instruction as the participant is still speaking.
		if current.Event.Type != current.Event.Source+".endpoint" {
			runner.intent = nil
			return runner.ignore(ctx, envelope, commit, "user_observation_not_final",
				"a provisional user observation cannot activate computer effects")
		}
		runner.intent = &userIntentBasis{
			itemID: current.ID, triggerItemID: current.Event.EventID,
			sourceRevision: current.SourceRevision,
		}
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
	basis, found := trajectoryItem(trajectory.Snapshot{Version: commit.StoreVersion, Items: prefix},
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
			return runner.ignore(ctx, envelope, commit, "generation_pending",
				"one exact cognition turn is still in flight")
		case resultParent == "":
			return runner.ignore(ctx, envelope, commit, "effect_pending",
				"one proposed computer effect is waiting for its canonical visual consequence")
		default:
			resultItem, found := trajectoryItem(
				trajectory.Snapshot{Version: commit.StoreVersion, Items: prefix}, resultParent,
			)
			if !found || resultItem.ToolResult == nil ||
				resultItem.ToolResult.CallID != runner.active.callID {
				return runner.refuse(ctx, envelope, commit, "effect_result_mismatch",
					"visual consequence does not settle the one active computer effect")
			}
			runner.active = nil
			if resultItem.ToolResult.Error != "" {
				runner.intent = nil
				return runner.ignore(ctx, envelope, commit, "effect_failed",
					"the failed computer effect terminally cleared the selected user intent")
			}
		}
	}
	generationID := activationGenerationID(runner.config.Role, envelope.SessionID, commit, basis.ID)
	if _, duplicate := runner.terminal[generationID]; duplicate {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, commit, policyelements.GenerationOutcome{
			Kind: policyelements.GenerationIgnored, GenerationID: generationID,
			Role: runner.config.Role, StreamID: commit.StreamID,
			SourceRevision: commit.SourceRevision, ContextVersion: commit.StoreVersion,
			TriggerItemID: commit.TriggerItemID, Code: "duplicate_commit",
			Message: "observation commit already reached a terminal activation decision",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	runner.active = &activeGeneration{id: generationID, contextVersion: commit.StoreVersion}
	if err := runner.emit(ctx, envelope, generationID, commit, *runner.intent); err != nil {
		runner.active = nil
		return err
	}
	runner.rememberTerminal(generationID)
	runner.state.ContextVersion = commit.StoreVersion
	runner.state.Emitted++
	if err := runner.publishOutcome(ctx, envelope, commit, policyelements.GenerationOutcome{
		Kind: policyelements.GenerationEmitted, GenerationID: generationID,
		Role: runner.config.Role, StreamID: commit.StreamID,
		SourceRevision: commit.SourceRevision, ContextVersion: commit.StoreVersion,
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
	if !ok || !canonical(envelope.SessionID) || !canonical(envelope.RunID) ||
		result.RunID != envelope.RunID {
		return fmt.Errorf("Realtime-CU activation model result has invalid identity or payload %T", envelope.Payload)
	}
	if runner.active == nil || runner.active.id != result.RunID {
		// A result can race a user cancellation. It carries no authority by itself,
		// so an already-terminal generation is safe to discard and cannot release a
		// newer turn.
		if _, terminal := runner.terminal[result.RunID]; terminal {
			return nil
		}
		return fmt.Errorf("Realtime-CU activation received result for unknown run %q", result.RunID)
	}
	if result.ContextVersion != runner.active.contextVersion {
		return fmt.Errorf("Realtime-CU activation result context is %d, want %d",
			result.ContextVersion, runner.active.contextVersion)
	}
	if len(result.ToolProposals) > 1 {
		return fmt.Errorf("Realtime-CU activation result has %d proposals; profile allows one",
			len(result.ToolProposals))
	}
	if len(result.ToolProposals) == 0 {
		runner.active = nil
	} else {
		callID := result.ToolProposals[0].Call.CallID
		if !canonical(callID) {
			return errors.New("Realtime-CU activation result proposal has a noncanonical call ID")
		}
		runner.active.callID = callID
	}
	return runner.publishState(ctx, envelope)
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
	ctx context.Context, cause element.Envelope, generationID string,
	commit stateelements.ObservationCommitOutcome, basis userIntentBasis,
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
		commit.TriggerItemID, basis.itemID, basis.triggerItemID,
	} {
		trigger.CausalParents = appendUniqueString(trigger.CausalParents, parent)
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
		ContextTailItem:       commit.TrajectoryItemID,
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
	if !ok || !canonical(envelope.SessionID) ||
		(cancel.GenerationID == "" && cancel.StreamID == "") {
		return runner.refuse(ctx, envelope, stateelements.ObservationCommitOutcome{},
			"invalid_cancel", "intent cancellation requires canonical session and address")
	}
	runner.intent = nil
	runner.active = nil
	if envelope.Sequence > runner.revokedSequence {
		runner.revokedSequence = envelope.Sequence
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

func observationCommitOutcomePayload(payload any) (stateelements.ObservationCommitOutcome, bool) {
	switch value := payload.(type) {
	case stateelements.ObservationCommitOutcome:
		return value, true
	case *stateelements.ObservationCommitOutcome:
		if value != nil {
			return *value, true
		}
	}
	return stateelements.ObservationCommitOutcome{}, false
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
