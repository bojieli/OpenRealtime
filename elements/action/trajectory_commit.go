package action

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type authorizedCallCommitFactory struct{}

var (
	_ element.Factory         = authorizedCallCommitFactory{}
	_ element.ConfigValidator = authorizedCallCommitFactory{}
)

func (authorizedCallCommitFactory) Descriptor() element.Descriptor {
	return AuthorizedCallCommitDescriptor()
}
func (authorizedCallCommitFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeTrajectoryCommitConfig(source)
	return err
}

func (authorizedCallCommitFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeTrajectoryCommitConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("action.AuthorizedCallCommit %s config: %w", mount.InstanceID, err)
	}
	toolService, toolServiceRevision, found := mount.Services.Lookup(ToolRegistryService)
	if !found {
		return nil, fmt.Errorf("action.AuthorizedCallCommit %s has no tool registries service", mount.InstanceID)
	}
	tools, ok := toolService.(*ToolRegistries)
	if !ok || tools == nil {
		return nil, fmt.Errorf("tool registries service has type %T", toolService)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	actionInput, err := mount.Ports.Input("action")
	if err != nil {
		return nil, err
	}
	contextInput, err := mount.Ports.Input("context")
	if err != nil {
		return nil, err
	}
	committedInput, err := mount.Ports.Input("committed")
	if err != nil {
		return nil, err
	}
	rejectedInput, err := mount.Ports.Input("rejected")
	if err != nil {
		return nil, err
	}
	cancelInput, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	timeoutInput, err := mount.Ports.Input("timeout")
	if err != nil {
		return nil, err
	}
	appendOutput, err := mount.Ports.Output("append")
	if err != nil {
		return nil, err
	}
	canonicalOutput, err := mount.Ports.Output("canonical")
	if err != nil {
		return nil, err
	}
	outcomeOutput, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	resolvedOutput, err := mount.Ports.Output("resolved")
	if err != nil {
		return nil, err
	}
	return &authorizedCallCommitRunner{
		config: config, tools: tools, toolServiceRevision: toolServiceRevision,
		emit:        emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		actionInput: actionInput, contextInput: contextInput,
		committedInput: committedInput, rejectedInput: rejectedInput,
		cancelInput: cancelInput, timeoutInput: timeoutInput,
		appendOutput: appendOutput, canonicalOutput: canonicalOutput,
		outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		pending:  make(map[string]*pendingAuthorizedCommit, config.MaxPending),
		requests: make(map[string]string, config.MaxPending), terminal: newBoundedSet(config.TerminalMemory),
		resolution: mount.Resolution,
	}, nil
}

type retainedTrajectorySnapshot struct {
	envelope element.Envelope
	value    trajectory.Snapshot
	digest   [sha256.Size]byte
}

type pendingAuthorizedCommit struct {
	cause           element.Envelope
	cancelCause     element.Envelope
	action          AuthorizedAction
	cancelKind      OutcomeKind
	cancelOperation string
	cancelReason    string
	promotedItemID  string
	requestID       string
	waitVersion     uint64
	expected        uint64
	prefixDigest    [sha256.Size]byte
	proposalItem    string
	trajectoryItem  trajectory.Item
}

type authorizedCallCommitRunner struct {
	config              TrajectoryCommitConfig
	emit                emitter
	tools               *ToolRegistries
	toolServiceRevision uint64

	actionInput     element.InputPort
	contextInput    element.InputPort
	committedInput  element.InputPort
	rejectedInput   element.InputPort
	cancelInput     element.InputPort
	timeoutInput    element.InputPort
	appendOutput    element.OutputPort
	canonicalOutput element.OutputPort
	outcomeOutput   element.OutputPort
	resolvedOutput  element.OutputPort

	latest     *retainedTrajectorySnapshot
	pending    map[string]*pendingAuthorizedCommit
	requests   map[string]string
	terminal   *boundedSet
	resolution element.ResolutionReporter
}

func (runner *authorizedCallCommitRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := publishResolution(ctx, runner.emit, runner.resolvedOutput, Resolution{
		Stage: "authorized_call_commit", Reference: "state.TrajectoryStore",
		Identity: "action.AuthorizedCallCommit", ServiceRevision: 1,
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, AuthorizedCallCommitDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"tool-declarations", "action.ToolRegistry/v1", "registry://"+ToolRegistryService,
			runner.toolServiceRevision, "",
		)}); err != nil {
		return err
	}
	inputs := make(chan receivedInput)
	failures := make(chan error, 6)
	var receivers sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{
		{"action", runner.actionInput}, {"context", runner.contextInput},
		{"committed", runner.committedInput}, {"rejected", runner.rejectedInput},
		{"cancel", runner.cancelInput}, {"timeout", runner.timeoutInput},
	} {
		receivers.Add(1)
		go receiveInputs(ctx, input.kind, input.port, inputs, failures, &receivers)
	}
	defer func() {
		cancel(nil)
		receivers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			cancel(err)
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "action":
				err = runner.acceptAction(ctx, input.envelope)
			case "context":
				err = runner.acceptContext(ctx, input.envelope)
			case "committed":
				err = runner.acceptCommit(ctx, input.envelope)
			case "rejected":
				err = runner.acceptRejection(ctx, input.envelope)
			case "cancel", "timeout":
				err = runner.interrupt(ctx, input.envelope, input.kind)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *authorizedCallCommitRunner) acceptAction(
	ctx context.Context, envelope element.Envelope,
) error {
	action, valid := authorizedPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "action", "", "invalid_payload",
			fmt.Sprintf("authorized action payload has type %T", envelope.Payload))
	}
	call := callOfAuthorized(action)
	if err := validateAuthorizedAction(action); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "action", call.CallID,
			"invalid_authority", err.Error())
	}
	if err := attestToolDeclaration(runner.tools, action.Confirmed.Declared); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "action", call.CallID,
			"declaration_attestation_failed", err.Error())
	}
	admitted := action.Confirmed.Declared.Admitted
	if code, err := validateActionEnvelopeIdentity(envelope, admitted); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "action", call.CallID,
			code, err.Error())
	}
	identity := actionIdentity(admitted)
	if runner.terminal.contains(identity) {
		return runner.publishOutcome(ctx, envelope, OutcomeIgnored, "action", call.CallID,
			"terminal_replay", "tool call is already terminal")
	}
	if _, duplicate := runner.pending[identity]; duplicate {
		return runner.publishOutcome(ctx, envelope, OutcomeIgnored, "action", call.CallID,
			"already_pending", "tool call promotion is already pending")
	}
	if len(runner.pending) >= runner.config.MaxPending {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "action", call.CallID,
			"capacity", fmt.Sprintf("authorized call commit retains at most %d calls", runner.config.MaxPending))
	}
	runner.pending[identity] = &pendingAuthorizedCommit{
		cause: envelope.Clone(), action: cloneAuthorized(action),
		// The proposal append and the admitted action travel on independent
		// graph lanes. Do not misclassify a temporarily older retained snapshot
		// as forged context; wait until the exact admitted prefix can exist, then
		// attest it normally. Cancellation still addresses this pending identity.
		waitVersion: admitted.ContextVersion,
	}
	return runner.tryStart(ctx, identity)
}

func (runner *authorizedCallCommitRunner) acceptContext(
	ctx context.Context, envelope element.Envelope,
) error {
	snapshot, valid := trajectorySnapshotPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "context", "", "invalid_payload",
			fmt.Sprintf("trajectory snapshot payload has type %T", envelope.Payload))
	}
	if code, err := validateSnapshotSession(runner.latest, envelope); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "context", "", code, err.Error())
	}
	retained, err := retainSnapshot(envelope, snapshot, runner.latest)
	if err != nil {
		return fmt.Errorf("authorized call commit context: %w", err)
	}
	if runner.latest != nil && retained.value.Version == runner.latest.value.Version {
		return nil
	}
	runner.latest = &retained
	for identity := range runner.pending {
		if err := runner.tryStart(ctx, identity); err != nil {
			return err
		}
	}
	return nil
}

func (runner *authorizedCallCommitRunner) tryStart(ctx context.Context, identity string) error {
	pending := runner.pending[identity]
	if pending == nil || pending.requestID != "" || runner.latest == nil ||
		runner.latest.value.Version < pending.waitVersion {
		return nil
	}
	if pending.cancelKind != "" && pending.promotedItemID != "" {
		return runner.startCancellationPlaceholder(ctx, identity, pending, runner.latest.value)
	}
	callID := callOfAuthorized(pending.action).CallID
	if sessionID := strings.TrimSpace(runner.latest.envelope.SessionID); sessionID != "" &&
		sessionID != pending.action.Confirmed.Declared.Admitted.SessionID {
		runner.finish(identity, pending)
		runner.terminal.add(identity)
		return runner.publishOutcome(ctx, pending.cause, OutcomeRejected, "promote", callID,
			"context_session_mismatch", "trajectory context crossed the authorized action session boundary")
	}
	evidence, code, err := authorizedPromotionEvidence(runner.latest.value, pending.action)
	if err != nil {
		if code == "proposal_not_committed" {
			return nil
		}
		runner.finish(identity, pending)
		runner.terminal.add(identity)
		return runner.publishOutcome(ctx, pending.cause, OutcomeRejected, "promote", callID, code, err.Error())
	}
	if evidence.call != nil {
		canonical := CanonicalAction{
			Authorized: cloneAuthorized(pending.action), ProposalItemID: evidence.proposal.ID,
			TrajectoryItemID: evidence.call.ID, StoreVersion: runner.latest.value.Version,
		}
		runner.finish(identity, pending)
		runner.terminal.add(identity)
		return runner.publishCanonical(ctx, pending.cause, canonical, "already_committed")
	}
	call := cloneToolCall(callOfAuthorized(pending.action))
	itemID := canonicalTrajectoryItemID("tool-call", pending.cause.SessionID,
		pending.action.Confirmed.Declared.Admitted.ModelRunID, evidence.proposal.ID, call.CallID)
	if _, collision := canonicalItem(runner.latest.value.Items, itemID); collision {
		runner.finish(identity, pending)
		runner.terminal.add(identity)
		return runner.publishOutcome(ctx, pending.cause, OutcomeRejected, "promote", callID,
			"trajectory_id_collision", fmt.Sprintf("canonical tool-call item ID %q is already occupied", itemID))
	}
	item := trajectory.Item{
		ID:   itemID,
		Kind: trajectory.KindToolCall, MonotonicNS: trajectoryCommitNS(runner.emit.clock.NowNS(), runner.latest.value),
		CausalParentIDs: []string{evidence.proposal.ID}, SourceRevision: evidence.proposal.SourceRevision,
		InvocationID: evidence.proposal.InvocationID,
		// Promotion is a runtime authority decision. Model provenance remains
		// on the exact proposal parent, invocation, and source revision.
		Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, ToolCall: &call,
		ToolCallDerivation: toolCallDerivationOfDeclared(pending.action.Confirmed.Declared),
	}
	requestSequence, err := runner.emit.sequences.Next(runner.emit.instance + ".append")
	if err != nil {
		return err
	}
	requestID := fmt.Sprintf("%s/append/%d", runner.emit.instance, requestSequence)
	pending.requestID = requestID
	pending.expected = runner.latest.value.Version
	pending.prefixDigest = runner.latest.digest
	pending.proposalItem = evidence.proposal.ID
	pending.trajectoryItem = cloneTrajectoryItemForAction(item)
	runner.requests[requestID] = identity
	appendEnvelope := pending.cause.Clone()
	appendEnvelope.Type = appendType.Clone()
	appendEnvelope.ItemID = requestID
	appendEnvelope.CausalParents = appendUnique(appendEnvelope.CausalParents, pending.cause.ItemID)
	appendEnvelope.CausalParents = appendUnique(appendEnvelope.CausalParents, evidence.proposal.ID)
	appendEnvelope.Payload = stateelements.Append{
		Compare: true, ExpectedVersion: pending.expected,
		Items: []trajectory.Item{cloneTrajectoryItemForAction(item)},
	}
	delivery, err := runner.appendOutput.Broadcast(ctx, appendEnvelope)
	if err != nil {
		delete(runner.requests, requestID)
		pending.requestID = ""
		return err
	}
	if delivery.Delivered != 1 {
		delete(runner.requests, requestID)
		pending.requestID = ""
		return fmt.Errorf("authorized tool-call append %s delivered to %d lanes", requestID, delivery.Delivered)
	}
	return nil
}

func (runner *authorizedCallCommitRunner) acceptCommit(
	ctx context.Context, envelope element.Envelope,
) error {
	requestID, identity, pending, found, err := runner.pendingForReply(envelope)
	if err != nil {
		return err
	}
	if !found {
		return runner.publishOutcome(ctx, envelope, OutcomeIgnored, "committed", "",
			"unknown_commit_reply", "trajectory commit has no pending authorized call")
	}
	commit, valid := trajectoryCommitPayloadForAction(envelope.Payload)
	if !valid {
		return fmt.Errorf("trajectory commit reply %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if err := attestSingleCommit(commit, pending.expected, pending.prefixDigest, pending.trajectoryItem); err != nil {
		return fmt.Errorf("authorized call commit reply %s: %w", envelope.ItemID, err)
	}
	if pending.cancelKind != "" {
		if pending.trajectoryItem.Kind == trajectory.KindToolCall {
			pending.promotedItemID = pending.trajectoryItem.ID
			delete(runner.requests, requestID)
			pending.requestID = ""
			return runner.startCancellationPlaceholder(ctx, identity, pending, commit.Snapshot)
		}
		if pending.trajectoryItem.Kind != trajectory.KindToolPlaceholder {
			return fmt.Errorf("canceled authorized call committed unexpected item kind %q", pending.trajectoryItem.Kind)
		}
		cause := pending.cancelCause.Clone()
		cause.CausalParents = appendUnique(cause.CausalParents, requestID)
		cause.CausalParents = appendUnique(cause.CausalParents, envelope.ItemID)
		cause.CausalParents = appendUnique(cause.CausalParents, pending.promotedItemID)
		cause.CausalParents = appendUnique(cause.CausalParents, pending.trajectoryItem.ID)
		kind, operation, reason := pending.cancelKind, pending.cancelOperation, pending.cancelReason
		runner.finish(identity, pending)
		runner.terminal.add(identity)
		return runner.publishOutcome(ctx, cause, kind, operation, callOfAuthorized(pending.action).CallID,
			"canceled_before_ledger", reason)
	}
	canonical := CanonicalAction{
		Authorized: cloneAuthorized(pending.action), ProposalItemID: pending.proposalItem,
		TrajectoryItemID: pending.trajectoryItem.ID, StoreVersion: commit.Version,
	}
	cause := pending.cause.Clone()
	cause.CausalParents = appendUnique(cause.CausalParents, requestID)
	cause.CausalParents = appendUnique(cause.CausalParents, envelope.ItemID)
	cause.CausalParents = appendUnique(cause.CausalParents, pending.proposalItem)
	cause.CausalParents = appendUnique(cause.CausalParents, pending.trajectoryItem.ID)
	runner.finish(identity, pending)
	runner.terminal.add(identity)
	return runner.publishCanonical(ctx, cause, canonical, "committed")
}

func (runner *authorizedCallCommitRunner) acceptRejection(
	ctx context.Context, envelope element.Envelope,
) error {
	requestID, identity, pending, found, err := runner.pendingForReply(envelope)
	if err != nil {
		return err
	}
	if !found {
		return runner.publishOutcome(ctx, envelope, OutcomeIgnored, "rejected", "",
			"unknown_rejection_reply", "trajectory rejection has no pending authorized call")
	}
	callID := callOfAuthorized(pending.action).CallID
	rejection, valid := trajectoryRejectionPayloadForAction(envelope.Payload)
	if !valid {
		return fmt.Errorf("trajectory rejection reply %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if rejection.ExpectedVersion != pending.expected {
		return fmt.Errorf("trajectory rejection reply %s expects version %d, pending append used %d",
			envelope.ItemID, rejection.ExpectedVersion, pending.expected)
	}
	delete(runner.requests, requestID)
	pending.requestID = ""
	if rejection.Code == "version_conflict" {
		if rejection.CurrentVersion <= pending.expected {
			return fmt.Errorf("trajectory version conflict moved from %d to invalid version %d",
				pending.expected, rejection.CurrentVersion)
		}
		if pending.cancelKind != "" {
			if pending.promotedItemID != "" {
				pending.waitVersion = rejection.CurrentVersion
				if err := runner.publishOutcome(ctx, pending.cancelCause, OutcomeIgnored, "retry", callID,
					"version_conflict", "cancellation placeholder will retry on the newer canonical prefix"); err != nil {
					return err
				}
				return runner.tryStart(ctx, identity)
			}
			kind, operation, reason := pending.cancelKind, pending.cancelOperation, pending.cancelReason
			runner.finish(identity, pending)
			runner.terminal.add(identity)
			return runner.publishOutcome(ctx, pending.cancelCause, kind, operation, callID,
				"canceled_before_commit", reason)
		}
		pending.waitVersion = rejection.CurrentVersion
		if err := runner.publishOutcome(ctx, pending.cause, OutcomeIgnored, "retry", callID,
			"version_conflict", rejection.Message); err != nil {
			return err
		}
		return runner.tryStart(ctx, identity)
	}
	if pending.cancelKind != "" {
		if pending.promotedItemID != "" {
			runner.finish(identity, pending)
			runner.terminal.add(identity)
			return runner.publishOutcome(ctx, pending.cancelCause, OutcomeRejected, "cancel", callID,
				"cancellation_placeholder_rejected",
				"canonical tool call crossed, but its mandatory cancellation placeholder was rejected: "+rejection.Message)
		}
		kind, operation, reason := pending.cancelKind, pending.cancelOperation, pending.cancelReason
		runner.finish(identity, pending)
		runner.terminal.add(identity)
		return runner.publishOutcome(ctx, pending.cancelCause, kind, operation, callID,
			"canceled_before_commit", reason)
	}
	runner.finish(identity, pending)
	runner.terminal.add(identity)
	return runner.publishOutcome(ctx, pending.cause, OutcomeRejected, "promote", callID,
		rejection.Code, rejection.Message)
}

func (runner *authorizedCallCommitRunner) pendingForReply(
	envelope element.Envelope,
) (string, string, *pendingAuthorizedCommit, bool, error) {
	requestID, err := correlatedRequest(envelope, runner.requests)
	if err != nil || requestID == "" {
		return requestID, "", nil, false, err
	}
	identity := runner.requests[requestID]
	pending := runner.pending[identity]
	if pending == nil || pending.requestID != requestID {
		return "", "", nil, false, fmt.Errorf("trajectory reply names inconsistent request %s", requestID)
	}
	if envelope.SessionID != pending.cause.SessionID || envelope.RunID != pending.cause.RunID {
		return "", "", nil, false, errors.New("trajectory reply crossed the pending action session or run")
	}
	return requestID, identity, pending, true, nil
}

func (runner *authorizedCallCommitRunner) interrupt(
	ctx context.Context, envelope element.Envelope, operation string,
) error {
	interrupt, code, err := interruptAddress(envelope)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, operation, "", code, err.Error())
	}
	identity, identityCode, err := interruptIdentity(envelope, interrupt)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, operation,
			interrupt.CallID, identityCode, err.Error())
	}
	pending, found := runner.pending[identity]
	if found {
		kind := OutcomeCanceled
		if operation == "timeout" {
			kind = OutcomeTimedOut
		}
		if pending.requestID != "" {
			if pending.cancelKind != "" {
				return runner.publishOutcome(ctx, envelope, OutcomeIgnored, operation,
					interrupt.CallID, "cancellation_already_pending", interrupt.Reason)
			}
			pending.cancelKind = kind
			pending.cancelOperation = operation
			pending.cancelReason = interrupt.Reason
			pending.cancelCause = envelope.Clone()
			if pending.cancelReason == "" {
				pending.cancelReason = operation
			}
			return runner.publishOutcome(ctx, envelope, OutcomeIgnored, operation,
				interrupt.CallID, "cancellation_pending_commit",
				"trajectory append is in flight; cancellation will close its canonical prefix")
		}
		runner.finish(identity, pending)
	}
	already := runner.terminal.contains(identity)
	runner.terminal.add(identity)
	kind := OutcomeCanceled
	if operation == "timeout" {
		kind = OutcomeTimedOut
	}
	if !found && already {
		kind, code = OutcomeIgnored, "already_terminal"
	}
	return runner.publishOutcome(ctx, envelope, kind, operation, interrupt.CallID, code, interrupt.Reason)
}

func (runner *authorizedCallCommitRunner) startCancellationPlaceholder(
	ctx context.Context, identity string, pending *pendingAuthorizedCommit, snapshot trajectory.Snapshot,
) error {
	call := cloneToolCall(callOfAuthorized(pending.action))
	itemID := canonicalTrajectoryItemID("tool-placeholder", pending.cause.SessionID,
		pending.cause.RunID, pending.promotedItemID, call.CallID)
	if occupied, collision := canonicalItem(snapshot.Items, itemID); collision {
		exact := occupied.Kind == trajectory.KindToolPlaceholder && occupied.ToolPlaceholder != nil &&
			occupied.ToolPlaceholder.CallID == call.CallID && occupied.ToolPlaceholder.Name == call.Name &&
			occupied.ToolPlaceholder.Reason == pending.cancelReason &&
			slices.Equal(occupied.CausalParentIDs, []string{pending.promotedItemID}) &&
			occupied.SourceRevision == pending.action.Confirmed.Declared.Admitted.SourceRevision &&
			occupied.InvocationID == pending.action.Confirmed.Declared.Admitted.ModelRunID &&
			occupied.Producer.Phase == trajectory.PhaseRuntime
		if exact {
			kind, operation, reason := pending.cancelKind, pending.cancelOperation, pending.cancelReason
			runner.finish(identity, pending)
			runner.terminal.add(identity)
			return runner.publishOutcome(ctx, pending.cancelCause, kind, operation, call.CallID,
				"canceled_before_ledger", reason)
		}
		runner.finish(identity, pending)
		runner.terminal.add(identity)
		return runner.publishOutcome(ctx, pending.cancelCause, OutcomeRejected, "cancel", call.CallID,
			"trajectory_id_collision", fmt.Sprintf("canonical tool-placeholder item ID %q is already occupied", itemID))
	}
	placeholder := trajectory.ToolPlaceholder{CallID: call.CallID, Name: call.Name, Reason: pending.cancelReason}
	item := trajectory.Item{
		ID: itemID, Kind: trajectory.KindToolPlaceholder,
		MonotonicNS:     trajectoryCommitNS(runner.emit.clock.NowNS(), snapshot),
		CausalParentIDs: []string{pending.promotedItemID},
		SourceRevision:  pending.action.Confirmed.Declared.Admitted.SourceRevision,
		InvocationID:    pending.action.Confirmed.Declared.Admitted.ModelRunID,
		Producer:        trajectory.Producer{Phase: trajectory.PhaseRuntime}, ToolPlaceholder: &placeholder,
	}
	requestSequence, err := runner.emit.sequences.Next(runner.emit.instance + ".append")
	if err != nil {
		return err
	}
	requestID := fmt.Sprintf("%s/append/%d", runner.emit.instance, requestSequence)
	pending.requestID = requestID
	pending.expected = snapshot.Version
	pending.prefixDigest = digestTrajectoryItemsForAction(snapshot.Items)
	pending.trajectoryItem = cloneTrajectoryItemForAction(item)
	runner.requests[requestID] = identity
	appendEnvelope := pending.cancelCause.Clone()
	appendEnvelope.Type = appendType.Clone()
	appendEnvelope.ItemID = requestID
	appendEnvelope.CausalParents = appendUnique(appendEnvelope.CausalParents, pending.cancelCause.ItemID)
	appendEnvelope.CausalParents = appendUnique(appendEnvelope.CausalParents, pending.promotedItemID)
	appendEnvelope.Payload = stateelements.Append{
		Compare: true, ExpectedVersion: pending.expected,
		Items: []trajectory.Item{cloneTrajectoryItemForAction(item)},
	}
	delivery, err := runner.appendOutput.Broadcast(ctx, appendEnvelope)
	if err != nil {
		delete(runner.requests, requestID)
		pending.requestID = ""
		return err
	}
	if delivery.Delivered != 1 {
		delete(runner.requests, requestID)
		pending.requestID = ""
		return fmt.Errorf("tool-placeholder append %s delivered to %d lanes", requestID, delivery.Delivered)
	}
	return nil
}

func (runner *authorizedCallCommitRunner) finish(identity string, pending *pendingAuthorizedCommit) {
	delete(runner.pending, identity)
	if pending != nil && pending.requestID != "" {
		delete(runner.requests, pending.requestID)
	}
}

func (runner *authorizedCallCommitRunner) publishCanonical(
	ctx context.Context, cause element.Envelope, canonical CanonicalAction, operation string,
) error {
	if err := publishPayload(ctx, runner.emit, runner.canonicalOutput, cause,
		canonicalActionType, canonical, "canonical"); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, cause, OutcomeSucceeded, operation,
		callOfCanonical(canonical).CallID, "", "")
}

func (runner *authorizedCallCommitRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, kind OutcomeKind, operation, callID, code, message string,
) error {
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, cause, Outcome{
		Kind: kind, Stage: "authorized_call_commit", Operation: operation,
		CallID: callID, Code: code, Message: message,
	})
}

type promotionEvidence struct {
	proposal trajectory.Item
	call     *trajectory.Item
}

func authorizedPromotionEvidence(
	snapshot trajectory.Snapshot, action AuthorizedAction,
) (promotionEvidence, string, error) {
	call := callOfAuthorized(action)
	admitted := action.Confirmed.Declared.Admitted
	modelCall := admitted.Proposal.Call
	if admitted.ContextVersion > snapshot.Version || admitted.ContextVersion > uint64(len(snapshot.Items)) {
		return promotionEvidence{}, "context_not_committed", errors.New("authorized context exceeds canonical trajectory")
	}
	prefix := snapshot.Items[:admitted.ContextVersion]
	if len(prefix) == 0 || prefix[len(prefix)-1].ID != admitted.ContextTailItem {
		return promotionEvidence{}, "context_tail_mismatch", errors.New("authorized context tail is not canonical")
	}
	authority, found := canonicalItem(prefix, admitted.AuthorityItemID)
	if !found || authority.SourceRevision != admitted.SourceRevision ||
		trajectory.AuthorityOf(authority) != admitted.Authority ||
		!causalAncestor(prefix, authority.ID, admitted.ContextTailItem) {
		return promotionEvidence{}, "authority_mismatch", errors.New("authorized authority evidence is not in the causal context prefix")
	}
	if authority.Event == nil || authority.Event.EventID != admitted.ObservationTriggerItemID {
		return promotionEvidence{}, "authority_event_mismatch", errors.New(
			"authorized authority evidence does not attest the selected observation trigger")
	}
	var proposal *trajectory.Item
	var committedCall *trajectory.Item
	for index := range snapshot.Items {
		item := &snapshot.Items[index]
		if item.Kind == trajectory.KindToolProposal && item.ToolCall != nil &&
			item.ToolCall.CallID == call.CallID && item.InvocationID == admitted.ModelRunID {
			if proposal != nil {
				return promotionEvidence{}, "duplicate_canonical_proposal", errors.New("canonical trajectory repeats a proposal in one invocation scope")
			}
			proposal = item
		}
		if item.Kind == trajectory.KindToolCall && item.ToolCall != nil &&
			item.ToolCall.CallID == call.CallID && item.InvocationID == admitted.ModelRunID {
			if committedCall != nil {
				return promotionEvidence{}, "duplicate_canonical_call", errors.New("canonical trajectory repeats a tool call in one invocation scope")
			}
			committedCall = item
		}
	}
	if proposal == nil {
		return promotionEvidence{}, "proposal_not_committed", errors.New("model proposal is not yet canonical")
	}
	if proposal.InvocationID != admitted.ModelRunID || proposal.SourceRevision != admitted.SourceRevision ||
		proposal.ToolCall.CallID != modelCall.CallID || proposal.ToolCall.Name != modelCall.Name ||
		!bytes.Equal(proposal.ToolCall.Arguments, modelCall.Arguments) {
		return promotionEvidence{}, "canonical_proposal_mismatch", errors.New("canonical proposal differs from the authorized model proposal")
	}
	if proposal.Producer != admitted.ModelProducer {
		return promotionEvidence{}, "canonical_model_producer_mismatch",
			errors.New("canonical proposal was committed by a different model/provider choice")
	}
	proposalPrefix := snapshot.Items[:indexOfTrajectoryItem(snapshot.Items, proposal.ID)+1]
	if !causalAncestor(proposalPrefix, admitted.ContextTailItem, proposal.ID) {
		return promotionEvidence{}, "proposal_context_mismatch", errors.New("canonical proposal is not derived from the authorized context tail")
	}
	evidence := promotionEvidence{proposal: cloneTrajectoryItemForAction(*proposal)}
	if committedCall != nil {
		expectedDerivation := toolCallDerivationOfDeclared(action.Confirmed.Declared)
		if committedCall.InvocationID != proposal.InvocationID || committedCall.SourceRevision != proposal.SourceRevision ||
			committedCall.ToolCall.Name != call.Name || !bytes.Equal(committedCall.ToolCall.Arguments, call.Arguments) ||
			!slices.Contains(committedCall.CausalParentIDs, proposal.ID) ||
			!sameToolCallDerivation(committedCall.ToolCallDerivation, expectedDerivation) {
			return promotionEvidence{}, "canonical_call_mismatch", errors.New("existing canonical tool call is not the authorized effective call")
		}
		copy := cloneTrajectoryItemForAction(*committedCall)
		evidence.call = &copy
	}
	return evidence, "", nil
}

type toolResultCommitFactory struct{}

var (
	_ element.Factory         = toolResultCommitFactory{}
	_ element.ConfigValidator = toolResultCommitFactory{}
)

func (toolResultCommitFactory) Descriptor() element.Descriptor { return ToolResultCommitDescriptor() }
func (toolResultCommitFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeTrajectoryCommitConfig(source)
	return err
}

func (toolResultCommitFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeTrajectoryCommitConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("action.ToolResultCommit %s config: %w", mount.InstanceID, err)
	}
	ledgerService, ledgerRevision, found := mount.Services.Lookup(LedgerRegistryService)
	if !found {
		return nil, fmt.Errorf("action.ToolResultCommit %s has no ledger registries service", mount.InstanceID)
	}
	ledgers, ok := ledgerService.(*LedgerRegistries)
	if !ok || ledgers == nil {
		return nil, fmt.Errorf("ledger registries service has type %T", ledgerService)
	}
	trajectoryService, trajectoryRevision, found := mount.Services.Lookup(TrajectoryStoreService)
	if !found {
		return nil, fmt.Errorf("action.ToolResultCommit %s has no trajectory store service", mount.InstanceID)
	}
	store, ok := trajectoryService.(*trajectory.Store)
	if !ok || store == nil {
		return nil, fmt.Errorf("action trajectory store service has type %T", trajectoryService)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	resultInput, err := mount.Ports.Input("result")
	if err != nil {
		return nil, err
	}
	contextInput, err := mount.Ports.Input("context")
	if err != nil {
		return nil, err
	}
	committedInput, err := mount.Ports.Input("committed")
	if err != nil {
		return nil, err
	}
	rejectedInput, err := mount.Ports.Input("rejected")
	if err != nil {
		return nil, err
	}
	cancelInput, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	timeoutInput, err := mount.Ports.Input("timeout")
	if err != nil {
		return nil, err
	}
	appendOutput, err := mount.Ports.Output("append")
	if err != nil {
		return nil, err
	}
	canonicalOutput, err := mount.Ports.Output("canonical")
	if err != nil {
		return nil, err
	}
	outcomeOutput, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	resolvedOutput, err := mount.Ports.Output("resolved")
	if err != nil {
		return nil, err
	}
	return &toolResultCommitRunner{
		config: config, ledgers: ledgers, store: store,
		ledgerServiceRevision: ledgerRevision, trajectoryServiceRevision: trajectoryRevision,
		emit:        emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		resultInput: resultInput, contextInput: contextInput,
		committedInput: committedInput, rejectedInput: rejectedInput,
		cancelInput: cancelInput, timeoutInput: timeoutInput,
		appendOutput: appendOutput, canonicalOutput: canonicalOutput,
		outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		pending:       make(map[string]*pendingToolResultCommit, config.MaxPending),
		cancellations: make(map[string]*toolResultCancellation, config.MaxPending),
		requests:      make(map[string]string, config.MaxPending), terminal: newBoundedSet(config.TerminalMemory),
		resolution: mount.Resolution,
	}, nil
}

type toolResultCancellation struct {
	cause     element.Envelope
	operation string
	kind      OutcomeKind
	callID    string
	reason    string
}

type pendingToolResultCommit struct {
	cause          element.Envelope
	result         ExecutionResult
	cancellation   *toolResultCancellation
	requestID      string
	waitVersion    uint64
	expected       uint64
	prefixDigest   [sha256.Size]byte
	trajectoryItem trajectory.Item
}

type toolResultCommitRunner struct {
	config                    TrajectoryCommitConfig
	ledgers                   *LedgerRegistries
	store                     *trajectory.Store
	ledgerServiceRevision     uint64
	trajectoryServiceRevision uint64
	emit                      emitter

	resultInput     element.InputPort
	contextInput    element.InputPort
	committedInput  element.InputPort
	rejectedInput   element.InputPort
	cancelInput     element.InputPort
	timeoutInput    element.InputPort
	appendOutput    element.OutputPort
	canonicalOutput element.OutputPort
	outcomeOutput   element.OutputPort
	resolvedOutput  element.OutputPort

	latest        *retainedTrajectorySnapshot
	pending       map[string]*pendingToolResultCommit
	cancellations map[string]*toolResultCancellation
	requests      map[string]string
	terminal      *boundedSet
	resolution    element.ResolutionReporter
}

func (runner *toolResultCommitRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := publishResolution(ctx, runner.emit, runner.resolvedOutput, Resolution{
		Stage: "tool_result_commit", Reference: LedgerRegistryService + "+" + TrajectoryStoreService,
		Identity:        "action.ToolResultCommit",
		ServiceRevision: max(runner.ledgerServiceRevision, runner.trajectoryServiceRevision),
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, ToolResultCommitDescriptor(),
		[]element.CapabilityResolution{
			actionCapability("ledgers", "action.LedgerRegistry/v1", "registry://"+LedgerRegistryService,
				runner.ledgerServiceRevision, ""),
			actionCapability("trajectory", "trajectory.Store/v1",
				"go://github.com/bojieli/OpenRealtime/trajectory/Store", runner.trajectoryServiceRevision, ""),
		}); err != nil {
		return err
	}
	inputs := make(chan receivedInput)
	failures := make(chan error, 6)
	var receivers sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{
		{"result", runner.resultInput}, {"context", runner.contextInput},
		{"committed", runner.committedInput}, {"rejected", runner.rejectedInput},
		{"cancel", runner.cancelInput}, {"timeout", runner.timeoutInput},
	} {
		receivers.Add(1)
		go receiveInputs(ctx, input.kind, input.port, inputs, failures, &receivers)
	}
	defer func() {
		cancel(nil)
		receivers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			cancel(err)
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "result":
				err = runner.acceptResult(ctx, input.envelope)
			case "context":
				err = runner.acceptContext(ctx, input.envelope)
			case "committed":
				err = runner.acceptCommit(ctx, input.envelope)
			case "rejected":
				err = runner.acceptRejection(ctx, input.envelope)
			case "cancel", "timeout":
				err = runner.interrupt(ctx, input.envelope, input.kind)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *toolResultCommitRunner) acceptResult(
	ctx context.Context, envelope element.Envelope,
) error {
	result, valid := executionResultPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "result", "", "invalid_payload",
			fmt.Sprintf("execution result payload has type %T", envelope.Payload))
	}
	if err := validateExecutionResult(result); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "result", result.CallID,
			"invalid_result", err.Error())
	}
	if code, err := runner.attestExecutionResult(envelope, result); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "attest", result.CallID, code, err.Error())
	}
	if err := validateExecutionResultOriginSemantics(result); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "attest", result.CallID,
			"invalid_result_origin", err.Error())
	}
	identity := actionIdentity(result.Executable.Canonical.Authorized.Confirmed.Declared.Admitted)
	if runner.terminal.contains(identity) {
		return runner.publishOutcome(ctx, envelope, OutcomeIgnored, "result", result.CallID,
			"terminal_replay", "tool result is already terminal")
	}
	if _, duplicate := runner.pending[identity]; duplicate {
		return runner.publishOutcome(ctx, envelope, OutcomeIgnored, "result", result.CallID,
			"already_pending", "tool result commit is already pending")
	}
	cancellation := runner.cancellations[identity]
	if len(runner.pending)+len(runner.cancellations) >= runner.config.MaxPending && cancellation == nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "result", result.CallID,
			"capacity", fmt.Sprintf("tool result commit retains at most %d calls", runner.config.MaxPending))
	}
	delete(runner.cancellations, identity)
	runner.pending[identity] = &pendingToolResultCommit{
		cause: envelope.Clone(), result: cloneExecutionResult(result), cancellation: cancellation,
	}
	return runner.tryStart(ctx, identity)
}

func validateExecutionResult(result ExecutionResult) error {
	if err := validateCanonicalAction(result.Executable.Canonical); err != nil {
		return fmt.Errorf("execution authorization: %w", err)
	}
	call := callOfExecutable(result.Executable)
	admitted := result.Executable.Canonical.Authorized.Confirmed.Declared.Admitted
	if strings.TrimSpace(result.CallID) == "" || strings.TrimSpace(result.Name) == "" ||
		strings.TrimSpace(result.ResultCapability) == "" ||
		result.CommitmentID != actionCommitmentID(admitted) || result.Executable.CommitmentID != result.CommitmentID ||
		call.CallID != result.CallID || call.Name != result.Name {
		return errors.New("execution result has incomplete call, name, or commitment identity")
	}
	if result.CompletionOrigin != CompletionReturned &&
		result.CompletionOrigin != CompletionDispatcherError {
		return fmt.Errorf("execution result has unknown completion origin %q", result.CompletionOrigin)
	}
	if result.Result.CallID != result.CallID || result.Result.Name != result.Name {
		return errors.New("execution result payload does not match its dispatch identity")
	}
	hasOutput := len(result.Result.Output) != 0
	hasError := strings.TrimSpace(result.Result.Error) != ""
	if hasOutput == hasError || (hasOutput && !json.Valid(result.Result.Output)) {
		return errors.New("tool result requires exactly one valid JSON output or error")
	}
	if result.CrossedNS == 0 || result.FinishedNS < result.CrossedNS {
		return errors.New("execution result does not prove a valid external-effect interval")
	}
	return nil
}

func validateExecutionResultOriginSemantics(result ExecutionResult) error {
	if result.CompletionOrigin == CompletionDispatcherError && result.Result.Error == "" {
		return errors.New("dispatcher-generated completion requires an error result")
	}
	return nil
}

func (runner *toolResultCommitRunner) attestExecutionResult(
	envelope element.Envelope, result ExecutionResult,
) (string, error) {
	executable := result.Executable
	admitted := executable.Canonical.Authorized.Confirmed.Declared.Admitted
	if strings.TrimSpace(envelope.SessionID) == "" || admitted.SessionID == "" ||
		envelope.SessionID != admitted.SessionID {
		return "session_mismatch", errors.New("execution result and admitted action must name one non-empty session")
	}
	if strings.TrimSpace(envelope.RunID) == "" || envelope.RunID != admitted.ModelRunID {
		return "model_run_mismatch", errors.New("execution result and admitted action name different cognition runs")
	}
	entry, err := runner.ledgers.resolve(executable.LedgerReference)
	if err != nil {
		return "ledger_unresolved", err
	}
	if !entry.verify(executable) || !entry.verifyResult(result) {
		return "invalid_capability", errors.New("execution result does not carry exact ledger-authenticated execution and result evidence")
	}
	commitment, found := entry.ledger.Lookup(executable.CommitmentID)
	if !found || commitment.ID != executable.CommitmentID || commitment.CallID != actionIdentity(admitted) {
		return "commitment_mismatch", errors.New("execution result has no matching ledger commitment")
	}
	if commitment.State != legacyaction.StatePlayed || !commitment.State.Crossed() {
		return "commitment_not_played", fmt.Errorf("ledger commitment state is %q, want played", commitment.State)
	}
	if code, err := attestCanonicalAction(runner.store, executable.Canonical); err != nil {
		return code, err
	}
	return "", nil
}

func (runner *toolResultCommitRunner) acceptContext(
	ctx context.Context, envelope element.Envelope,
) error {
	snapshot, valid := trajectorySnapshotPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "context", "", "invalid_payload",
			fmt.Sprintf("trajectory snapshot payload has type %T", envelope.Payload))
	}
	if code, err := validateSnapshotSession(runner.latest, envelope); err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, "context", "", code, err.Error())
	}
	retained, err := retainSnapshot(envelope, snapshot, runner.latest)
	if err != nil {
		return fmt.Errorf("tool result commit context: %w", err)
	}
	if runner.latest != nil && retained.value.Version == runner.latest.value.Version {
		return nil
	}
	runner.latest = &retained
	for identity := range runner.pending {
		if err := runner.tryStart(ctx, identity); err != nil {
			return err
		}
	}
	return nil
}

func (runner *toolResultCommitRunner) tryStart(ctx context.Context, identity string) error {
	pending := runner.pending[identity]
	if pending == nil || pending.requestID != "" || runner.latest == nil ||
		runner.latest.value.Version < pending.waitVersion {
		return nil
	}
	callID := pending.result.CallID
	if sessionID := strings.TrimSpace(runner.latest.envelope.SessionID); sessionID != "" {
		admitted := pending.result.Executable.Canonical.Authorized.Confirmed.Declared.Admitted
		if sessionID != admitted.SessionID {
			return runner.finishRejected(ctx, identity, pending, "commit", callID,
				"context_session_mismatch", "trajectory context crossed the executed action session boundary")
		}
	}
	callItem, resultItem, code, err := toolResultEvidence(
		runner.latest.value, pending.result, pending.cause.RunID,
	)
	if err != nil {
		if code == "call_not_committed" {
			return nil
		}
		return runner.finishRejected(ctx, identity, pending, "commit", callID, code, err.Error())
	}
	if resultItem != nil {
		canonical := CanonicalResult{
			Execution: cloneExecutionResult(pending.result), TrajectoryItemID: resultItem.ID,
			StoreVersion: runner.latest.value.Version,
		}
		return runner.finishCanonical(ctx, identity, pending, pending.cause, canonical, "already_committed")
	}
	toolResult := cloneToolResult(pending.result.Result)
	itemID := canonicalTrajectoryItemID("tool-result", pending.cause.SessionID,
		pending.cause.RunID, callItem.ID, pending.result.ResultCapability)
	if _, collision := canonicalItem(runner.latest.value.Items, itemID); collision {
		return runner.finishRejected(ctx, identity, pending, "commit", callID,
			"trajectory_id_collision", fmt.Sprintf("canonical tool-result item ID %q is already occupied", itemID))
	}
	item := trajectory.Item{
		ID:   itemID,
		Kind: trajectory.KindToolResult, MonotonicNS: trajectoryCommitNS(runner.emit.clock.NowNS(), runner.latest.value),
		CausalParentIDs: []string{callItem.ID}, SourceRevision: callItem.SourceRevision,
		InvocationID: callItem.InvocationID, Producer: trajectory.Producer{Phase: trajectory.PhaseTool},
		ToolResult: &toolResult,
	}
	requestSequence, err := runner.emit.sequences.Next(runner.emit.instance + ".append")
	if err != nil {
		return err
	}
	requestID := fmt.Sprintf("%s/append/%d", runner.emit.instance, requestSequence)
	pending.requestID = requestID
	pending.expected = runner.latest.value.Version
	pending.prefixDigest = runner.latest.digest
	pending.trajectoryItem = cloneTrajectoryItemForAction(item)
	runner.requests[requestID] = identity
	appendEnvelope := pending.cause.Clone()
	appendEnvelope.Type = appendType.Clone()
	appendEnvelope.ItemID = requestID
	appendEnvelope.CausalParents = appendUnique(appendEnvelope.CausalParents, pending.cause.ItemID)
	appendEnvelope.CausalParents = appendUnique(appendEnvelope.CausalParents, callItem.ID)
	appendEnvelope.Payload = stateelements.Append{
		Compare: true, ExpectedVersion: pending.expected,
		Items: []trajectory.Item{cloneTrajectoryItemForAction(item)},
	}
	delivery, err := runner.appendOutput.Broadcast(ctx, appendEnvelope)
	if err != nil {
		delete(runner.requests, requestID)
		pending.requestID = ""
		return err
	}
	if delivery.Delivered != 1 {
		delete(runner.requests, requestID)
		pending.requestID = ""
		return fmt.Errorf("tool-result append %s delivered to %d lanes", requestID, delivery.Delivered)
	}
	return nil
}

func (runner *toolResultCommitRunner) acceptCommit(
	ctx context.Context, envelope element.Envelope,
) error {
	requestID, identity, pending, found, err := runner.pendingForReply(envelope)
	if err != nil {
		return err
	}
	if !found {
		return runner.publishOutcome(ctx, envelope, OutcomeIgnored, "committed", "",
			"unknown_commit_reply", "trajectory commit has no pending tool result")
	}
	commit, valid := trajectoryCommitPayloadForAction(envelope.Payload)
	if !valid {
		return fmt.Errorf("trajectory commit reply %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if err := attestSingleCommit(commit, pending.expected, pending.prefixDigest, pending.trajectoryItem); err != nil {
		return fmt.Errorf("tool result commit reply %s: %w", envelope.ItemID, err)
	}
	canonical := CanonicalResult{
		Execution:        cloneExecutionResult(pending.result),
		TrajectoryItemID: pending.trajectoryItem.ID, StoreVersion: commit.Version,
	}
	cause := pending.cause.Clone()
	cause.CausalParents = appendUnique(cause.CausalParents, requestID)
	cause.CausalParents = appendUnique(cause.CausalParents, envelope.ItemID)
	cause.CausalParents = appendUnique(cause.CausalParents, pending.trajectoryItem.ID)
	return runner.finishCanonical(ctx, identity, pending, cause, canonical, "committed")
}

func (runner *toolResultCommitRunner) acceptRejection(
	ctx context.Context, envelope element.Envelope,
) error {
	requestID, identity, pending, found, err := runner.pendingForReply(envelope)
	if err != nil {
		return err
	}
	if !found {
		return runner.publishOutcome(ctx, envelope, OutcomeIgnored, "rejected", "",
			"unknown_rejection_reply", "trajectory rejection has no pending tool result")
	}
	callID := pending.result.CallID
	rejection, valid := trajectoryRejectionPayloadForAction(envelope.Payload)
	if !valid {
		return fmt.Errorf("trajectory rejection reply %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if rejection.ExpectedVersion != pending.expected {
		return fmt.Errorf("trajectory rejection reply %s expects version %d, pending append used %d",
			envelope.ItemID, rejection.ExpectedVersion, pending.expected)
	}
	delete(runner.requests, requestID)
	pending.requestID = ""
	if rejection.Code == "version_conflict" {
		if rejection.CurrentVersion <= pending.expected {
			return fmt.Errorf("trajectory version conflict moved from %d to invalid version %d",
				pending.expected, rejection.CurrentVersion)
		}
		pending.waitVersion = rejection.CurrentVersion
		if err := runner.publishOutcome(ctx, pending.cause, OutcomeIgnored, "retry", callID,
			"version_conflict", rejection.Message); err != nil {
			return err
		}
		return runner.tryStart(ctx, identity)
	}
	return runner.finishRejected(ctx, identity, pending, "commit", callID,
		rejection.Code, rejection.Message)
}

func (runner *toolResultCommitRunner) pendingForReply(
	envelope element.Envelope,
) (string, string, *pendingToolResultCommit, bool, error) {
	requestID, err := correlatedRequest(envelope, runner.requests)
	if err != nil || requestID == "" {
		return requestID, "", nil, false, err
	}
	identity := runner.requests[requestID]
	pending := runner.pending[identity]
	if pending == nil || pending.requestID != requestID {
		return "", "", nil, false, fmt.Errorf("trajectory reply names inconsistent request %s", requestID)
	}
	if envelope.SessionID != pending.cause.SessionID || envelope.RunID != pending.cause.RunID {
		return "", "", nil, false, errors.New("trajectory reply crossed the pending result session or run")
	}
	return requestID, identity, pending, true, nil
}

func (runner *toolResultCommitRunner) interrupt(
	ctx context.Context, envelope element.Envelope, operation string,
) error {
	interrupt, code, err := interruptAddress(envelope)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, operation, "", code, err.Error())
	}
	identity, identityCode, err := interruptIdentity(envelope, interrupt)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, operation,
			interrupt.CallID, identityCode, err.Error())
	}
	cancellation := &toolResultCancellation{
		cause: envelope.Clone(), operation: operation, callID: interrupt.CallID, reason: interrupt.Reason,
		kind: OutcomeCanceled,
	}
	if operation == "timeout" {
		cancellation.kind = OutcomeTimedOut
	}
	if pending := runner.pending[identity]; pending != nil {
		if pending.cancellation != nil {
			return runner.duplicateCancellation(ctx, *cancellation, pending.cancellation)
		}
		pending.cancellation = cancellation
		return runner.publishCancellationPending(ctx, *cancellation, "result_must_commit")
	}
	if retained := runner.cancellations[identity]; retained != nil {
		return runner.duplicateCancellation(ctx, *cancellation, retained)
	}

	state, found, err := runner.commitmentState(identity)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, OutcomeRejected, operation, interrupt.CallID,
			"ledger_commitment_ambiguous", err.Error())
	}
	// A terminal result has already reached this element, so the mandatory
	// canonicalization attempt is over. Report the irreversible boundary
	// honestly instead of retaining an interrupt that can never be released.
	if runner.terminal.contains(identity) {
		return runner.publishCancellationTerminal(ctx, *cancellation, found && state.Crossed())
	}
	if found && state.Crossed() {
		if len(runner.pending)+len(runner.cancellations) >= runner.config.MaxPending {
			return runner.publishOutcome(ctx, envelope, OutcomeRejected, operation, interrupt.CallID,
				"capacity", fmt.Sprintf("tool result commit retains at most %d calls", runner.config.MaxPending))
		}
		// Dispatch has crossed but its authenticated result may still be on an
		// independent lane. Preserve the exact interrupt until that result is a
		// canonical trajectory item; only then can cancellation settle.
		runner.cancellations[identity] = cancellation
		return runner.publishCancellationPending(ctx, *cancellation, "result_must_commit")
	}
	// No result is in flight here and the shared ledger proves that this stage
	// has not crossed. Other cancellation stages fence their own independent
	// lanes. Do not add the action to terminal memory: an authenticated late
	// crossed result is still a mandatory canonical safe point.
	return runner.publishCancellationTerminal(ctx, *cancellation, false)
}

func (runner *toolResultCommitRunner) duplicateCancellation(
	ctx context.Context, attempted toolResultCancellation,
	retained *toolResultCancellation,
) error {
	if retained != nil && sameClientToolResultEnvelopeAddress(retained.cause, attempted.cause) &&
		retained.operation == attempted.operation && retained.kind == attempted.kind &&
		retained.callID == attempted.callID && retained.reason == attempted.reason {
		return runner.publishCancellationPending(ctx, *retained, "cancellation_already_pending")
	}
	return runner.publishOutcome(ctx, attempted.cause, OutcomeRejected, attempted.operation, attempted.callID,
		"cancellation_identity_conflict", "a different interrupt already owns this tool-result cancellation")
}

func (runner *toolResultCommitRunner) commitmentState(identity string) (legacyaction.State, bool, error) {
	commitmentID := "action:" + identity
	runner.ledgers.mu.RLock()
	entries := make([]ledgerEntry, 0, len(runner.ledgers.entries))
	for _, entry := range runner.ledgers.entries {
		entries = append(entries, entry)
	}
	runner.ledgers.mu.RUnlock()
	var state legacyaction.State
	found := false
	for _, entry := range entries {
		commitment, exists := entry.ledger.Lookup(commitmentID)
		if !exists {
			continue
		}
		if found {
			return "", false, fmt.Errorf("commitment %q exists in more than one registered ledger", commitmentID)
		}
		if commitment.ID != commitmentID || commitment.CallID != identity {
			return "", false, fmt.Errorf("commitment %q has inconsistent action identity", commitmentID)
		}
		state, found = commitment.State, true
	}
	return state, found, nil
}

func (runner *toolResultCommitRunner) publishCancellationPending(
	ctx context.Context, cancellation toolResultCancellation, code string,
) error {
	return runner.publishOutcome(ctx, cancellation.cause, OutcomeIgnored, cancellation.operation,
		cancellation.callID, code, "an executed tool result remains a mandatory canonical safe point")
}

func (runner *toolResultCommitRunner) publishCancellationTerminal(
	ctx context.Context, cancellation toolResultCancellation, crossed bool,
) error {
	code := "canceled"
	if cancellation.operation == "timeout" {
		code = "timed_out"
	}
	message := cancellation.reason
	if message == "" {
		message = cancellation.operation
	}
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, cancellation.cause, Outcome{
		Kind: cancellation.kind, Stage: "tool_result_commit", Operation: cancellation.operation,
		CallID: cancellation.callID, Code: code, Message: message, Crossed: crossed,
	})
}

func (runner *toolResultCommitRunner) finishCanonical(
	ctx context.Context, identity string, pending *pendingToolResultCommit,
	cause element.Envelope, canonical CanonicalResult, operation string,
) error {
	runner.finish(identity, pending)
	runner.terminal.add(identity)
	if err := runner.publishCanonical(ctx, cause, canonical, operation); err != nil {
		return err
	}
	if pending.cancellation != nil {
		return runner.publishCancellationTerminal(ctx, *pending.cancellation, true)
	}
	return nil
}

func (runner *toolResultCommitRunner) finishRejected(
	ctx context.Context, identity string, pending *pendingToolResultCommit,
	operation, callID, code, message string,
) error {
	runner.finish(identity, pending)
	runner.terminal.add(identity)
	if err := runner.publishOutcome(ctx, pending.cause, OutcomeRejected, operation, callID, code, message); err != nil {
		return err
	}
	if pending.cancellation != nil {
		return runner.publishCancellationTerminal(ctx, *pending.cancellation, true)
	}
	return nil
}

func (runner *toolResultCommitRunner) finish(identity string, pending *pendingToolResultCommit) {
	delete(runner.pending, identity)
	if pending != nil && pending.requestID != "" {
		delete(runner.requests, pending.requestID)
	}
}

func (runner *toolResultCommitRunner) publishCanonical(
	ctx context.Context, cause element.Envelope, canonical CanonicalResult, operation string,
) error {
	if err := publishPayload(ctx, runner.emit, runner.canonicalOutput, cause,
		canonicalResultType, canonical, "canonical"); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, cause, OutcomeSucceeded, operation,
		canonical.Execution.CallID, "", "")
}

func (runner *toolResultCommitRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, kind OutcomeKind, operation, callID, code, message string,
) error {
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, cause, Outcome{
		Kind: kind, Stage: "tool_result_commit", Operation: operation,
		CallID: callID, Code: code, Message: message,
	})
}

func toolResultEvidence(
	snapshot trajectory.Snapshot, execution ExecutionResult, envelopeRunID string,
) (trajectory.Item, *trajectory.Item, string, error) {
	canonical := execution.Executable.Canonical
	declared := canonical.Authorized.Confirmed.Declared
	modelRunID := declared.Admitted.ModelRunID
	modelCall := declared.Admitted.Proposal.Call
	effectiveCall := callOfCanonical(canonical)
	var proposal *trajectory.Item
	var call *trajectory.Item
	var result *trajectory.Item
	for index := range snapshot.Items {
		item := &snapshot.Items[index]
		switch {
		case item.Kind == trajectory.KindToolProposal && item.ToolCall != nil &&
			item.ToolCall.CallID == execution.CallID && item.InvocationID == modelRunID:
			if proposal != nil {
				return trajectory.Item{}, nil, "duplicate_canonical_proposal", errors.New("canonical trajectory repeats a proposal in one invocation scope")
			}
			proposal = item
		case item.Kind == trajectory.KindToolCall && item.ToolCall != nil &&
			item.ToolCall.CallID == execution.CallID && item.InvocationID == modelRunID:
			if call != nil {
				return trajectory.Item{}, nil, "duplicate_canonical_call", errors.New("canonical trajectory repeats a tool call in one invocation scope")
			}
			call = item
		case item.Kind == trajectory.KindToolResult && item.ToolResult != nil &&
			item.ToolResult.CallID == execution.CallID && item.InvocationID == modelRunID:
			if result != nil {
				return trajectory.Item{}, nil, "duplicate_canonical_result", errors.New("canonical trajectory repeats a tool result in one invocation scope")
			}
			result = item
		}
	}
	if call == nil {
		return trajectory.Item{}, nil, "call_not_committed", errors.New("canonical tool call is not yet available")
	}
	if proposal == nil || !slices.Contains(call.CausalParentIDs, proposal.ID) ||
		call.InvocationID != proposal.InvocationID || call.SourceRevision != proposal.SourceRevision ||
		proposal.ToolCall.CallID != modelCall.CallID || proposal.ToolCall.Name != modelCall.Name ||
		!bytes.Equal(proposal.ToolCall.Arguments, modelCall.Arguments) ||
		call.ToolCall.CallID != effectiveCall.CallID || call.ToolCall.Name != effectiveCall.Name ||
		!bytes.Equal(call.ToolCall.Arguments, effectiveCall.Arguments) ||
		!sameToolCallDerivation(call.ToolCallDerivation, toolCallDerivationOfDeclared(declared)) {
		return trajectory.Item{}, nil, "noncanonical_call", errors.New("tool call is not the attested effective form of its canonical proposal")
	}
	if proposal.ID != canonical.ProposalItemID || call.ID != canonical.TrajectoryItemID ||
		call.Producer.Phase != trajectory.PhaseRuntime {
		return trajectory.Item{}, nil, "canonical_action_mismatch",
			errors.New("tool result is not bound to the ledger-attested runtime promotion")
	}
	if call.ToolCall.Name != execution.Name {
		return trajectory.Item{}, nil, "call_result_mismatch", errors.New("execution result names a different canonical call")
	}
	if envelopeRunID != "" && call.InvocationID != envelopeRunID {
		return trajectory.Item{}, nil, "model_run_mismatch", errors.New(
			"execution result envelope is not bound to the canonical call invocation")
	}
	callCopy := cloneTrajectoryItemForAction(*call)
	if result == nil {
		return callCopy, nil, "", nil
	}
	if result.ToolResult.Name != execution.Name ||
		!sameToolResult(*result.ToolResult, execution.Result) ||
		!slices.Contains(result.CausalParentIDs, call.ID) ||
		result.InvocationID != call.InvocationID || result.SourceRevision != call.SourceRevision {
		return trajectory.Item{}, nil, "canonical_result_mismatch", errors.New("existing canonical result differs from dispatch completion")
	}
	resultCopy := cloneTrajectoryItemForAction(*result)
	return callCopy, &resultCopy, "", nil
}

func retainSnapshot(
	envelope element.Envelope, snapshot trajectory.Snapshot, previous *retainedTrajectorySnapshot,
) (retainedTrajectorySnapshot, error) {
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return retainedTrajectorySnapshot{}, fmt.Errorf("snapshot version %d contains %d items",
			snapshot.Version, len(snapshot.Items))
	}
	seen := make(map[string]struct{}, len(snapshot.Items))
	for index, item := range snapshot.Items {
		if strings.TrimSpace(item.ID) == "" {
			return retainedTrajectorySnapshot{}, fmt.Errorf("snapshot item %d has no ID", index)
		}
		if _, duplicate := seen[item.ID]; duplicate {
			return retainedTrajectorySnapshot{}, fmt.Errorf("snapshot repeats item ID %q", item.ID)
		}
		for _, parent := range item.CausalParentIDs {
			if _, found := seen[parent]; !found {
				return retainedTrajectorySnapshot{}, fmt.Errorf("snapshot item %q has nonpreceding parent %q", item.ID, parent)
			}
		}
		seen[item.ID] = struct{}{}
	}
	copy := trajectory.Snapshot{Version: snapshot.Version, Items: cloneTrajectoryItemsForAction(snapshot.Items)}
	digest := digestTrajectoryItemsForAction(copy.Items)
	if previous != nil {
		if copy.Version < previous.value.Version {
			return retainedTrajectorySnapshot{}, fmt.Errorf("snapshot version regressed from %d to %d",
				previous.value.Version, copy.Version)
		}
		if copy.Version == previous.value.Version && digest != previous.digest {
			return retainedTrajectorySnapshot{}, errors.New("equal-version snapshots disagree")
		}
		if copy.Version > previous.value.Version &&
			digestTrajectoryItemsForAction(copy.Items[:previous.value.Version]) != previous.digest {
			return retainedTrajectorySnapshot{}, errors.New("newer snapshot rewrote the canonical prefix")
		}
	}
	return retainedTrajectorySnapshot{envelope: envelope.Clone(), value: copy, digest: digest}, nil
}

func validateSnapshotSession(
	previous *retainedTrajectorySnapshot, envelope element.Envelope,
) (string, error) {
	if previous == nil {
		return "", nil
	}
	bound := strings.TrimSpace(previous.envelope.SessionID)
	incoming := strings.TrimSpace(envelope.SessionID)
	if bound == "" {
		return "", nil
	}
	if incoming == "" || incoming != bound {
		return "context_session_mismatch", fmt.Errorf(
			"trajectory snapshot session changed from %q to %q", bound, incoming)
	}
	return "", nil
}

func canonicalTrajectoryItemID(kind string, identities ...string) string {
	hash := sha256.New()
	for _, identity := range append([]string{kind}, identities...) {
		_, _ = fmt.Fprintf(hash, "%d:", len(identity))
		_, _ = hash.Write([]byte(identity))
	}
	return fmt.Sprintf("action:%s:sha256:%x", kind, hash.Sum(nil))
}

func attestSingleCommit(
	commit stateelements.Commit, expected uint64, prefixDigest [sha256.Size]byte, item trajectory.Item,
) error {
	if !slices.Equal(commit.AppendedIDs, []string{item.ID}) {
		return fmt.Errorf("commit attests IDs %v, want [%s]", commit.AppendedIDs, item.ID)
	}
	if expected == ^uint64(0) || commit.Version != expected+1 ||
		commit.Snapshot.Version != commit.Version || uint64(len(commit.Snapshot.Items)) != commit.Version {
		return fmt.Errorf("commit version/snapshot is inconsistent with expected version %d", expected)
	}
	if digestTrajectoryItemsForAction(commit.Snapshot.Items[:expected]) != prefixDigest {
		return errors.New("commit reply rewrote the sampled canonical prefix")
	}
	committed := commit.Snapshot.Items[expected]
	if digestTrajectoryItemsForAction([]trajectory.Item{committed}) !=
		digestTrajectoryItemsForAction([]trajectory.Item{item}) {
		return errors.New("commit reply changed the appended trajectory item")
	}
	return nil
}

func correlatedRequest(envelope element.Envelope, requests map[string]string) (string, error) {
	candidates := slices.Clone(envelope.CausalParents)
	for _, suffix := range []string{":committed", ":rejected"} {
		if strings.HasSuffix(envelope.ItemID, suffix) {
			candidates = append(candidates, strings.TrimSuffix(envelope.ItemID, suffix))
		}
	}
	matched := ""
	for _, candidate := range candidates {
		if _, found := requests[candidate]; !found {
			continue
		}
		if matched != "" && matched != candidate {
			return "", fmt.Errorf("trajectory reply ambiguously names requests %s and %s", matched, candidate)
		}
		matched = candidate
	}
	return matched, nil
}

func trajectorySnapshotPayload(payload any) (trajectory.Snapshot, bool) {
	switch value := payload.(type) {
	case trajectory.Snapshot:
		return trajectory.Snapshot{Version: value.Version, Items: cloneTrajectoryItemsForAction(value.Items)}, true
	case *trajectory.Snapshot:
		if value != nil {
			return trajectory.Snapshot{Version: value.Version, Items: cloneTrajectoryItemsForAction(value.Items)}, true
		}
	}
	return trajectory.Snapshot{}, false
}

func trajectoryCommitPayloadForAction(payload any) (stateelements.Commit, bool) {
	switch value := payload.(type) {
	case stateelements.Commit:
		value.AppendedIDs = slices.Clone(value.AppendedIDs)
		value.Snapshot.Items = cloneTrajectoryItemsForAction(value.Snapshot.Items)
		return value, true
	case *stateelements.Commit:
		if value != nil {
			copy := *value
			copy.AppendedIDs = slices.Clone(value.AppendedIDs)
			copy.Snapshot.Items = cloneTrajectoryItemsForAction(value.Snapshot.Items)
			return copy, true
		}
	}
	return stateelements.Commit{}, false
}

func trajectoryRejectionPayloadForAction(payload any) (stateelements.Rejection, bool) {
	switch value := payload.(type) {
	case stateelements.Rejection:
		return value, true
	case *stateelements.Rejection:
		if value != nil {
			return *value, true
		}
	}
	return stateelements.Rejection{}, false
}

func cloneTrajectoryItemsForAction(items []trajectory.Item) []trajectory.Item {
	result := make([]trajectory.Item, len(items))
	for index := range items {
		result[index] = cloneTrajectoryItemForAction(items[index])
	}
	return result
}

func cloneTrajectoryItemForAction(item trajectory.Item) trajectory.Item {
	item.CausalParentIDs = slices.Clone(item.CausalParentIDs)
	item.ProviderState = slices.Clone(item.ProviderState)
	if item.ToolCall != nil {
		copy := cloneToolCall(*item.ToolCall)
		item.ToolCall = &copy
	}
	if item.ToolCallDerivation != nil {
		copy := *item.ToolCallDerivation
		copy.Rewrites = slices.Clone(item.ToolCallDerivation.Rewrites)
		item.ToolCallDerivation = &copy
	}
	if item.ToolResult != nil {
		copy := cloneToolResult(*item.ToolResult)
		item.ToolResult = &copy
	}
	if item.ToolPlaceholder != nil {
		copy := *item.ToolPlaceholder
		item.ToolPlaceholder = &copy
	}
	if item.Observation != nil {
		copy := *item.Observation
		copy.Media = slices.Clone(item.Observation.Media)
		item.Observation = &copy
	}
	if item.AssistantState != nil {
		copy := *item.AssistantState
		item.AssistantState = &copy
	}
	if item.Repair != nil {
		copy := *item.Repair
		item.Repair = &copy
	}
	if item.Event != nil {
		copy := *item.Event
		item.Event = &copy
	}
	return item
}

func digestTrajectoryItemsForAction(items []trajectory.Item) [sha256.Size]byte {
	encoded, err := json.Marshal(items)
	if err != nil {
		return sha256.Sum256([]byte("marshal-error:" + err.Error()))
	}
	return sha256.Sum256(encoded)
}

func sameToolResult(left, right trajectory.ToolResult) bool {
	return left.CallID == right.CallID && left.Name == right.Name && left.Error == right.Error &&
		bytes.Equal(left.Output, right.Output)
}

func trajectoryCommitNS(now uint64, snapshot trajectory.Snapshot) uint64 {
	if len(snapshot.Items) != 0 && now < snapshot.Items[len(snapshot.Items)-1].MonotonicNS {
		return snapshot.Items[len(snapshot.Items)-1].MonotonicNS
	}
	return now
}

func indexOfTrajectoryItem(items []trajectory.Item, id string) int {
	for index := range items {
		if items[index].ID == id {
			return index
		}
	}
	return -1
}
