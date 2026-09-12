package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
)

type sessionInvocationFactory struct{}

func (sessionInvocationFactory) Descriptor() element.Descriptor { return SessionInvocationDescriptor() }

func (sessionInvocationFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeSessionInvocationConfig(source)
	return err
}

func (sessionInvocationFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeSessionInvocationConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("policy.SessionInvocation %s config: %w", mount.InstanceID, err)
	}
	return mountSessionInvocation(mount, config)
}

func mountSessionInvocation(mount element.MountContext, config SessionInvocationConfig) (*sessionInvocationRunner, error) {
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("session invocation policy has no runtime clock service")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("session invocation policy has no runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	ports, err := sessionInvocationPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &sessionInvocationRunner{
		instance: mount.InstanceID, config: config, clock: clock, sequences: sequences,
		resolution: mount.Resolution, ports: ports,
		terminal: make(map[string]struct{}), preCanceled: make(map[cancellationAddress]string),
	}, nil
}

type sessionInvocationPorts struct {
	update, committed, create, cancel  element.InputPort
	trigger, authority, state, outcome element.OutputPort
}

func sessionInvocationPortsFrom(ports element.Ports) (sessionInvocationPorts, error) {
	if ports == nil {
		return sessionInvocationPorts{}, errors.New("policy.SessionInvocation has nil ports")
	}
	var result sessionInvocationPorts
	for _, input := range []struct {
		name string
		set  *element.InputPort
	}{{"update", &result.update}, {"committed", &result.committed}, {"create", &result.create}, {"cancel", &result.cancel}} {
		port, err := ports.Input(input.name)
		if err != nil {
			return sessionInvocationPorts{}, err
		}
		*input.set = port
	}
	for _, output := range []struct {
		name string
		set  *element.OutputPort
	}{{"trigger", &result.trigger}, {"authority", &result.authority}, {"state", &result.state}, {"outcome", &result.outcome}} {
		port, err := ports.Output(output.name)
		if err != nil {
			return sessionInvocationPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

type sessionInvocationRunner struct {
	instance   string
	config     SessionInvocationConfig
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	resolution element.ResolutionReporter
	ports      sessionInvocationPorts

	invocation       SessionInvocationUpdate
	invocationDigest string
	state            SessionInvocationState
	terminal         map[string]struct{}
	terminalOrder    []string
	preCanceled      map[cancellationAddress]string
	preCancelOrder   []cancellationAddress
}

type sessionInvocationInput struct {
	kind     string
	envelope element.Envelope
}

func (runner *sessionInvocationRunner) Run(parent context.Context) error {
	artifact := liveidentity.Artifact{
		ID: sessionInvocationRuntimeID, Revision: sessionInvocationRuntimeRevision,
	}
	return runner.run(parent, artifact, runner.acceptCommit, runner.acceptCreate)
}

func (runner *sessionInvocationRunner) run(
	parent context.Context, artifact liveidentity.Artifact,
	acceptCommit, acceptCreate func(context.Context, element.Envelope) error,
) error {
	if err := liveidentity.Report(runner.resolution, artifact, nil); err != nil {
		return err
	}
	runner.state = SessionInvocationState{
		Role: runner.config.Role, TerminalMemory: runner.config.TerminalMemory,
		CancellationMemory: runner.config.CancelMemory,
	}
	if err := runner.publishState(parent, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan sessionInvocationInput)
	failures := make(chan error, 4)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{{"update", runner.ports.update}, {"committed", runner.ports.committed}, {"create", runner.ports.create}, {"cancel", runner.ports.cancel}} {
		wait.Add(1)
		go receiveSessionInvocationInputs(ctx, source.kind, source.port, inputs, failures, &wait)
	}
	defer func() { cancel(nil); wait.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "update":
				err = runner.acceptUpdate(ctx, input.envelope)
			case "committed":
				err = acceptCommit(ctx, input.envelope)
			case "create":
				err = acceptCreate(ctx, input.envelope)
			case "cancel":
				err = runner.acceptCancel(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown session invocation input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *sessionInvocationRunner) acceptUpdate(ctx context.Context, envelope element.Envelope) error {
	update, ok := sessionInvocationUpdatePayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "update", "", "invalid_update",
			fmt.Sprintf("session invocation update payload has type %T", envelope.Payload))
	}
	if err := runner.validateEnvelope(envelope, "session update"); err != nil {
		return runner.refuse(ctx, envelope, "update", "", "invalid_update", err.Error())
	}
	validated, digest, err := validateSessionInvocationUpdate(update)
	if err != nil {
		return runner.refuse(ctx, envelope, "update", "", "invalid_update", err.Error())
	}
	if validated.Revision <= runner.invocation.Revision {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, SessionInvocationOutcome{
			Kind: SessionInvocationIgnored, Operation: "update", Role: runner.config.Role,
			InvocationRevision: validated.Revision, InvocationDigest: digest,
			Code: "stale_update", Message: "session invocation revision must increase",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	runner.invocation = cloneSessionInvocationUpdate(validated)
	runner.invocationDigest = digest
	runner.state.InvocationRevision = validated.Revision
	runner.state.InvocationDigest = digest
	runner.state.Updated++
	if err := runner.publishOutcome(ctx, envelope, SessionInvocationOutcome{
		Kind: SessionInvocationUpdated, Operation: "update", Role: runner.config.Role,
		InvocationRevision: validated.Revision, InvocationDigest: digest,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *sessionInvocationRunner) acceptCommit(ctx context.Context, envelope element.Envelope) error {
	grant, ok := semanticGrantPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "committed", "", "invalid_commit",
			fmt.Sprintf("semantic grant payload has type %T", envelope.Payload))
	}
	commit := grant.Commit
	if commit.Kind != stateelements.ObservationCommitted {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, envelope, SessionInvocationOutcome{
			Kind: SessionInvocationIgnored, Operation: "committed", Role: runner.config.Role,
			ContextVersion: commit.StoreVersion, Code: "observation_not_committed",
			Message: "observation commit did not cross the canonical trajectory boundary",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	if err := runner.validateEnvelope(envelope, "observation commit"); err != nil {
		return runner.refuse(ctx, envelope, "committed", "", "invalid_commit", err.Error())
	}
	if err := validateSemanticGrant(grant, runner.config.Role); err != nil {
		return runner.refuse(ctx, envelope, "committed", "", "invalid_commit", err.Error())
	}
	return runner.emitCommit(ctx, envelope, commit, grant.Choice, grant.SpokeOver)
}

func (runner *sessionInvocationRunner) emitCommit(
	ctx context.Context, envelope element.Envelope,
	commit stateelements.ObservationCommitOutcome, choice coreinteraction.Choice, spokeOver bool,
) error {
	if runner.invocation.Revision == 0 {
		return runner.refuse(ctx, envelope, "committed", "", "invocation_unset",
			"session invocation must be installed before an observation can activate cognition")
	}
	runner.state.ContextVersion = max(runner.state.ContextVersion, commit.StoreVersion)
	generationID := generationIdentifier(runner.config.Role, envelope.SessionID, commit)
	if runner.isTerminal(generationID) {
		return runner.ignoreDuplicate(ctx, envelope, "committed", generationID)
	}
	if reason, canceled := runner.takePreCancel(generationID, commit.StreamID, envelope.SessionID); canceled {
		runner.rememberTerminal(generationID)
		runner.state.Canceled++
		if err := runner.publishOutcome(ctx, envelope, SessionInvocationOutcome{
			Kind: SessionInvocationCanceled, Operation: "committed", GenerationID: generationID,
			Role: runner.config.Role, InvocationRevision: runner.invocation.Revision,
			InvocationDigest: runner.invocationDigest, StreamID: commit.StreamID,
			SourceRevision: commit.SourceRevision, ContextVersion: commit.StoreVersion,
			TriggerItemID: commit.TriggerItemID, Code: "canceled", Message: reason,
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	version := commit.Context.Prefix.Version
	committedContext := commit.Context
	payload := cognitionelements.Generate{
		Invocation:             invocationForCommit(runner.invocation, commit),
		ExpectedContextVersion: &version, ExpectedContextItemID: commit.Context.StateItemID,
		CommittedContext: &committedContext,
		SpokeOver:        spokeOver,
	}
	trigger := runner.triggerEnvelope(envelope, generationID, payload)
	for _, parent := range []string{commit.Context.StateItemID, commit.TrajectoryItemID, commit.TriggerItemID, commit.TailItemID()} {
		trigger.CausalParents = appendUnique(trigger.CausalParents, parent)
	}
	if _, err := runner.ports.trigger.Broadcast(ctx, trigger); err != nil {
		return err
	}
	candidateEnvelope := envelope.Clone()
	candidateEnvelope.Type = authorityCandidateType
	candidateEnvelope.ItemID = generationID + ":authority"
	candidateEnvelope.RunID = generationID
	candidateEnvelope.CancellationScope = generationID
	candidateEnvelope.CausalParents = cloneCausalParents(trigger.CausalParents)
	candidateEnvelope.Payload = authorityForSessionCommit(generationID, envelope, trigger, commit)
	if _, err := runner.ports.authority.Broadcast(ctx, candidateEnvelope); err != nil {
		return err
	}
	return runner.finishEmission(ctx, envelope, "committed", generationID, commit, choice, spokeOver)
}

func (runner *sessionInvocationRunner) acceptCreate(ctx context.Context, envelope element.Envelope) error {
	create, ok := responseCreatePayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "create", "", "invalid_create",
			fmt.Sprintf("response create payload has type %T", envelope.Payload))
	}
	if err := runner.validateEnvelope(envelope, "response create"); err != nil {
		return runner.refuse(ctx, envelope, "create", "", "invalid_create", err.Error())
	}
	responseID, err := responseCreateIdentifier(create)
	if err != nil {
		return runner.refuse(ctx, envelope, "create", "", "invalid_create", err.Error())
	}
	if runner.invocation.Revision == 0 {
		return runner.refuse(ctx, envelope, "create", "", "invocation_unset",
			"session invocation must be installed before response creation")
	}
	if *create.ExpectedContextVersion < runner.state.ContextVersion {
		return runner.refuse(ctx, envelope, "create", "", "invalid_create",
			fmt.Sprintf("response context version %d moved backwards from %d",
				*create.ExpectedContextVersion, runner.state.ContextVersion))
	}
	generationID := sessionInvocationGenerationID(
		runner.config.Role, envelope.SessionID, "create", responseID, runner.invocation.Revision,
	)
	if runner.isTerminal(generationID) {
		return runner.ignoreDuplicate(ctx, envelope, "create", generationID)
	}
	if reason, canceled := runner.takePreCancel(generationID, "", envelope.SessionID); canceled {
		runner.rememberTerminal(generationID)
		runner.state.Canceled++
		if err := runner.publishOutcome(ctx, envelope, SessionInvocationOutcome{
			Kind: SessionInvocationCanceled, Operation: "create", GenerationID: generationID,
			Role: runner.config.Role, InvocationRevision: runner.invocation.Revision,
			InvocationDigest: runner.invocationDigest, Code: "canceled", Message: reason,
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	}
	version := *create.ExpectedContextVersion
	invocation, err := invocationForManualCreate(runner.invocation, create.TrustedPurpose)
	if err != nil {
		return runner.refuse(ctx, envelope, "create", generationID, "invalid_create", err.Error())
	}
	payload := cognitionelements.Generate{
		Invocation:             invocation,
		ExpectedContextVersion: &version, ExpectedContextItemID: create.ExpectedContextItemID,
		CommittedContext: cloneResponseCreateContext(create.CommittedContext),
	}
	trigger := runner.triggerEnvelope(envelope, generationID, payload)
	trigger.CausalParents = appendUnique(trigger.CausalParents, create.ExpectedContextItemID)
	if _, err := runner.ports.trigger.Broadcast(ctx, trigger); err != nil {
		return err
	}
	runner.state.ContextVersion = version
	return runner.finishEmission(ctx, envelope, "create", generationID,
		stateelements.ObservationCommitOutcome{StoreVersion: version}, coreinteraction.Choice{Speak: true}, false)
}

func (runner *sessionInvocationRunner) acceptCancel(ctx context.Context, envelope element.Envelope) error {
	cancel, ok := generationCancelPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "cancel", "", "invalid_cancel",
			fmt.Sprintf("generation cancel payload has type %T", envelope.Payload))
	}
	if err := runner.validateEnvelope(envelope, "generation cancellation"); err != nil {
		return runner.refuse(ctx, envelope, "cancel", "", "invalid_cancel", err.Error())
	}
	if (cancel.GenerationID == "") == (cancel.StreamID == "") {
		return runner.refuse(ctx, envelope, "cancel", "", "invalid_cancel",
			"cancel requires exactly one generation_id or stream_id")
	}
	address := cancellationAddress{
		generationID: cancel.GenerationID, streamID: cancel.StreamID, sessionID: envelope.SessionID,
	}
	if cancel.GenerationID != "" {
		if err := validatePolicyIdentifier("generation ID", cancel.GenerationID, true); err != nil {
			return runner.refuse(ctx, envelope, "cancel", "", "invalid_cancel", err.Error())
		}
	} else if err := validatePolicyIdentifier("stream ID", cancel.StreamID, true); err != nil {
		return runner.refuse(ctx, envelope, "cancel", "", "invalid_cancel", err.Error())
	}
	cancel.Reason = boundedPolicyReason(cancel.Reason)
	runner.recordPreCancel(address, cancel.Reason)
	runner.state.Ignored++
	if err := runner.publishOutcome(ctx, envelope, SessionInvocationOutcome{
		Kind: SessionInvocationIgnored, Operation: "cancel", Role: runner.config.Role,
		GenerationID: cancel.GenerationID, StreamID: cancel.StreamID,
		Code: "cancel_recorded", Message: cancel.Reason,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *sessionInvocationRunner) triggerEnvelope(
	cause element.Envelope, generationID string, payload cognitionelements.Generate,
) element.Envelope {
	trigger := cause.Clone()
	trigger.Type = generationTriggerType
	trigger.ItemID = generationID + ":trigger"
	trigger.RunID = generationID
	trigger.CancellationScope = generationID
	trigger.CausalParents = appendUnique(trigger.CausalParents, cause.ItemID)
	trigger.Payload = payload
	return trigger
}

func (runner *sessionInvocationRunner) finishEmission(
	ctx context.Context, cause element.Envelope, operation, generationID string,
	commit stateelements.ObservationCommitOutcome, choice coreinteraction.Choice, spokeOver bool,
) error {
	runner.rememberTerminal(generationID)
	runner.state.Emitted++
	if err := runner.publishOutcome(ctx, cause, SessionInvocationOutcome{
		Kind: SessionInvocationEmitted, Operation: operation, GenerationID: generationID,
		Role: runner.config.Role, InvocationRevision: runner.invocation.Revision,
		InvocationDigest: runner.invocationDigest, StreamID: commit.StreamID,
		SourceRevision: commit.SourceRevision, ObservationRevision: commit.ObservationRevision,
		Choice: &choice, SpokeOver: spokeOver, ContextVersion: commit.StoreVersion,
		TriggerItemID: commit.TriggerItemID,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *sessionInvocationRunner) refuse(
	ctx context.Context, cause element.Envelope, operation, generationID, code, message string,
) error {
	if generationID != "" {
		runner.rememberTerminal(generationID)
	}
	runner.state.Refused++
	if err := runner.publishOutcome(ctx, cause, SessionInvocationOutcome{
		Kind: SessionInvocationRefused, Operation: operation, GenerationID: generationID,
		Role: runner.config.Role, InvocationRevision: runner.invocation.Revision,
		InvocationDigest: runner.invocationDigest, Code: code, Message: message,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *sessionInvocationRunner) ignoreDuplicate(
	ctx context.Context, cause element.Envelope, operation, generationID string,
) error {
	runner.state.Ignored++
	if err := runner.publishOutcome(ctx, cause, SessionInvocationOutcome{
		Kind: SessionInvocationIgnored, Operation: operation, GenerationID: generationID,
		Role: runner.config.Role, InvocationRevision: runner.invocation.Revision,
		InvocationDigest: runner.invocationDigest, Code: "duplicate_trigger",
		Message: "activation already reached a terminal policy decision",
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *sessionInvocationRunner) validateEnvelope(envelope element.Envelope, label string) error {
	if err := validatePolicyIdentifier(label+" item ID", envelope.ItemID, true); err != nil {
		return err
	}
	if err := validatePolicyIdentifier(label+" session ID", envelope.SessionID, true); err != nil {
		return err
	}
	return nil
}

func (runner *sessionInvocationRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome SessionInvocationOutcome,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".session-outcome")
	if err != nil {
		return err
	}
	outcome.FinishedNS = runner.clock.NowNS()
	envelope := cause.Clone()
	envelope.Type = sessionInvocationOutcomeType
	envelope.ItemID = fmt.Sprintf("%s:session_invocation_outcome:%d", cause.ItemID, sequence)
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.Payload = cloneSessionInvocationOutcome(outcome)
	_, err = runner.ports.outcome.Broadcast(ctx, envelope)
	return err
}

func (runner *sessionInvocationRunner) publishState(ctx context.Context, cause element.Envelope) error {
	sequence, err := runner.sequences.Next(runner.instance + ".session-state")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = sessionInvocationStateType
	envelope.ItemID = fmt.Sprintf("%s:session_state:%d", runner.instance, sequence)
	if cause.ItemID != "" {
		envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	}
	envelope.Payload = cloneSessionInvocationState(runner.state)
	_, err = runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *sessionInvocationRunner) isTerminal(generationID string) bool {
	_, found := runner.terminal[generationID]
	return found
}

func (runner *sessionInvocationRunner) rememberTerminal(generationID string) {
	if generationID == "" {
		return
	}
	if _, found := runner.terminal[generationID]; !found {
		runner.terminal[generationID] = struct{}{}
		runner.terminalOrder = append(runner.terminalOrder, generationID)
	}
	for len(runner.terminalOrder) > runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		runner.terminalOrder = runner.terminalOrder[1:]
		delete(runner.terminal, oldest)
	}
}

func (runner *sessionInvocationRunner) recordPreCancel(address cancellationAddress, reason string) {
	if _, found := runner.preCanceled[address]; !found {
		runner.preCancelOrder = append(runner.preCancelOrder, address)
	}
	runner.preCanceled[address] = boundedPolicyReason(reason)
	for len(runner.preCancelOrder) > runner.config.CancelMemory {
		oldest := runner.preCancelOrder[0]
		runner.preCancelOrder = runner.preCancelOrder[1:]
		delete(runner.preCanceled, oldest)
	}
}

func (runner *sessionInvocationRunner) takePreCancel(
	generationID, streamID, sessionID string,
) (string, bool) {
	addresses := []cancellationAddress{{generationID: generationID, sessionID: sessionID}}
	if streamID != "" {
		addresses = append(addresses, cancellationAddress{streamID: streamID, sessionID: sessionID})
	}
	for _, address := range addresses {
		reason, found := runner.preCanceled[address]
		if !found {
			continue
		}
		// One exact generation is consumed here and remembered as terminal.
		// A stream address covers every revision of that stream; consuming it
		// on the first hypothesis would let a later hypothesis restart work.
		// Keep it within the existing bounded cancellation memory instead.
		if address.generationID != "" {
			delete(runner.preCanceled, address)
			if index := slices.Index(runner.preCancelOrder, address); index >= 0 {
				runner.preCancelOrder = slices.Delete(runner.preCancelOrder, index, index+1)
			}
		}
		return reason, true
	}
	return "", false
}

func receiveSessionInvocationInputs(
	ctx context.Context, kind string, input element.InputPort,
	output chan<- sessionInvocationInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if generationTerminal(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- fmt.Errorf("receive session invocation %s: %w", kind, err):
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- sessionInvocationInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

var (
	_ element.Factory         = sessionInvocationFactory{}
	_ element.ConfigValidator = sessionInvocationFactory{}
)
