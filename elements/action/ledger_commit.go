package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type ledgerCommitFactory struct{}

var (
	_ element.Factory         = ledgerCommitFactory{}
	_ element.ConfigValidator = ledgerCommitFactory{}
)

func (ledgerCommitFactory) Descriptor() element.Descriptor { return LedgerCommitDescriptor() }
func (ledgerCommitFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeLedgerCommitConfig(source)
	return err
}

func (ledgerCommitFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeLedgerCommitConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("action.LedgerCommit %s config: %w", mount.InstanceID, err)
	}
	service, serviceRevision, found := mount.Services.Lookup(LedgerRegistryService)
	if !found {
		return nil, fmt.Errorf("action.LedgerCommit %s has no ledger registries service", mount.InstanceID)
	}
	registries, ok := service.(*LedgerRegistries)
	if !ok || registries == nil {
		return nil, fmt.Errorf("ledger registries service has type %T", service)
	}
	ledger, err := registries.resolve(config.Ledger)
	if err != nil {
		return nil, err
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	actionInput, err := mount.Ports.Input("action")
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
	executableOutput, err := mount.Ports.Output("executable")
	if err != nil {
		return nil, err
	}
	transitionOutput, err := mount.Ports.Output("transition")
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
	return &ledgerCommitRunner{
		config: config, ledger: ledger, serviceRevision: serviceRevision,
		emit:        emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		actionInput: actionInput, cancelInput: cancelInput, timeoutInput: timeoutInput,
		executableOutput: executableOutput, transitionOutput: transitionOutput,
		outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		preempted: newBoundedSet(config.MaxPending), resolution: mount.Resolution,
	}, nil
}

type ledgerCommitRunner struct {
	config          LedgerCommitConfig
	ledger          ledgerEntry
	serviceRevision uint64
	emit            emitter

	actionInput      element.InputPort
	cancelInput      element.InputPort
	timeoutInput     element.InputPort
	executableOutput element.OutputPort
	transitionOutput element.OutputPort
	outcomeOutput    element.OutputPort
	resolvedOutput   element.OutputPort

	preempted  *boundedSet
	resolution element.ResolutionReporter
}

func (runner *ledgerCommitRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := publishResolution(ctx, runner.emit, runner.resolvedOutput, Resolution{
		Stage: "ledger_commit", Reference: runner.ledger.reference, Identity: runner.ledger.identity,
		Digest: runner.ledger.identity, ServiceRevision: runner.serviceRevision,
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, LedgerCommitDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"ledger", "action.Ledger/v1", "ledger://"+runner.ledger.reference,
			runner.serviceRevision, runner.ledger.identity,
		)}); err != nil {
		return err
	}
	inputs := make(chan receivedInput)
	failures := make(chan error, 3)
	var wait sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{{"action", runner.actionInput}, {"cancel", runner.cancelInput}, {"timeout", runner.timeoutInput}} {
		wait.Add(1)
		go receiveInputs(ctx, input.kind, input.port, inputs, failures, &wait)
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
			cancel(err)
			return err
		case input := <-inputs:
			var err error
			if input.kind == "action" {
				err = runner.commit(ctx, input.envelope)
			} else {
				err = runner.interrupt(ctx, input.envelope, input.kind)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *ledgerCommitRunner) commit(ctx context.Context, envelope element.Envelope) error {
	authorized, ok := authorizedPayload(envelope.Payload)
	if !ok {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: "prepare", Code: "invalid_payload",
			Message: fmt.Sprintf("authorized action payload has type %T", envelope.Payload),
		})
	}
	call := callOfAuthorized(authorized)
	if err := validateAuthorizedAction(authorized); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: "prepare", CallID: call.CallID,
			Code: "invalid_authority", Message: err.Error(),
		})
	}
	if runner.preempted.contains(call.CallID) {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeCanceled, Stage: "ledger_commit", Operation: "prepare", CallID: call.CallID,
			Code: "preempted", Message: "action was canceled before ledger admission",
		})
	}
	commitmentID := "action_" + call.CallID
	kind := legacyaction.KindToolCall
	if computeruse.IsAction(call.Name) {
		kind = legacyaction.KindComputerAction
	}
	commitment := legacyaction.Commitment{
		ID: commitmentID, Kind: kind, CallID: call.CallID,
		SourceRevision: authorized.Confirmed.Declared.Admitted.SourceRevision,
		Confirm:        authorized.Confirmed.Declared.Confirmation,
	}
	if err := runner.ledger.ledger.Prepare(commitment); err != nil {
		code := "ledger_rejected"
		if errors.Is(err, legacyaction.ErrDuplicateCall) || strings.Contains(err.Error(), "duplicate commitment") {
			code = "duplicate_call"
		}
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: "prepare", CallID: call.CallID,
			Code: code, Message: err.Error(),
		})
	}
	if err := runner.publishTransition(ctx, envelope, commitmentID, call.CallID, legacyaction.StatePrepared, ""); err != nil {
		return err
	}
	if err := runner.ledger.ledger.Queue(commitmentID); err != nil {
		_, _ = runner.ledger.ledger.Cancel(commitmentID, err.Error())
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeFailed, Stage: "ledger_commit", Operation: "queue", CallID: call.CallID,
			Code: "queue_failed", Message: err.Error(),
		})
	}
	if err := runner.publishTransition(ctx, envelope, commitmentID, call.CallID, legacyaction.StateQueued, ""); err != nil {
		return err
	}
	capability, err := runner.ledger.sign(authorized, commitmentID)
	if err != nil {
		_, _ = runner.ledger.ledger.Cancel(commitmentID, err.Error())
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeFailed, Stage: "ledger_commit", Operation: "authorize", CallID: call.CallID,
			Code: "capability_failed", Message: err.Error(),
		})
	}
	executable := ExecutableAction{
		Authorized: authorized, LedgerReference: runner.ledger.reference, LedgerIdentity: runner.ledger.identity,
		CommitmentID: commitmentID, Capability: capability,
	}
	if err := publishPayload(ctx, runner.emit, runner.executableOutput, envelope, executableType, executable, "executable"); err != nil {
		return err
	}
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: OutcomeSucceeded, Stage: "ledger_commit", Operation: "queue", CallID: call.CallID,
	})
}

func validateAuthorizedAction(value AuthorizedAction) error {
	call := callOfAuthorized(value)
	if err := validateToolCall(call); err != nil {
		return err
	}
	admitted := value.Confirmed.Declared.Admitted
	if admitted.AuthorityItemID == "" || admitted.ProposalItemID == "" ||
		admitted.ModelRunID == "" || admitted.ContextVersion == 0 || admitted.ContextTailItem == "" {
		return errors.New("authorized action has incomplete proposal and canonical authority provenance")
	}
	if admitted.Authority != trajectory.AuthorityUser && admitted.Authority != trajectory.AuthoritySystem {
		return fmt.Errorf("authority %q cannot authorize an external effect", admitted.Authority)
	}
	declared := value.Confirmed.Declared
	if declared.RegistryReference == "" || declared.RegistryDigest == "" || declared.DeclarationDigest == "" {
		return errors.New("authorized action has incomplete declaration resolution")
	}
	if value.TargetReference == "" || value.TargetDigest == "" {
		return errors.New("authorized action has incomplete target resolution")
	}
	confirm, err := legacyaction.ParseConfirm(string(declared.Confirmation))
	if err != nil {
		return err
	}
	if confirm != legacyaction.ConfirmNever && !value.Confirmed.ConfirmationNeeded {
		return errors.New("required confirmation has no provider decision")
	}
	return nil
}

func (runner *ledgerCommitRunner) interrupt(ctx context.Context, envelope element.Envelope, operation string) error {
	interrupt, code, err := interruptAddress(envelope)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: operation, Code: code, Message: err.Error(),
		})
	}
	commitmentID := "action_" + interrupt.CallID
	commitment, found := runner.ledger.ledger.Lookup(commitmentID)
	if !found {
		already := runner.preempted.contains(interrupt.CallID)
		runner.preempted.add(interrupt.CallID)
		kind := OutcomeCanceled
		if operation == "timeout" {
			kind = OutcomeTimedOut
		}
		if already {
			kind, code = OutcomeIgnored, "already_preempted"
		}
		if interrupt.Reason == "" {
			interrupt.Reason = operation
		}
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: kind, Stage: "ledger_commit", Operation: operation, CallID: interrupt.CallID,
			Code: code, Message: interrupt.Reason,
		})
	}
	if commitment.CallID != interrupt.CallID {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: operation, CallID: interrupt.CallID,
			Code: "identity_mismatch", Message: "ledger commitment call ID does not match interrupt",
		})
	}
	if interrupt.Reason == "" {
		interrupt.Reason = operation
	}
	crossed, cancelErr := runner.ledger.ledger.Cancel(commitmentID, interrupt.Reason)
	if cancelErr != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeFailed, Stage: "ledger_commit", Operation: operation, CallID: interrupt.CallID,
			Code: "ledger_cancel_failed", Message: cancelErr.Error(),
		})
	}
	current, _ := runner.ledger.ledger.Lookup(commitmentID)
	if err := runner.publishTransition(ctx, envelope, commitmentID, interrupt.CallID, current.State, interrupt.Reason); err != nil {
		return err
	}
	kind := OutcomeCanceled
	if operation == "timeout" {
		kind = OutcomeTimedOut
	}
	if crossed {
		kind, code = OutcomeIgnored, "already_crossed"
	}
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: kind, Stage: "ledger_commit", Operation: operation, CallID: interrupt.CallID,
		Code: code, Message: interrupt.Reason, Crossed: crossed,
	})
}

func (runner *ledgerCommitRunner) publishTransition(
	ctx context.Context, parent element.Envelope, commitmentID, callID string,
	state legacyaction.State, reason string,
) error {
	transition := LedgerTransition{
		CallID: callID, CommitmentID: commitmentID, State: state, Crossed: state.Crossed(),
		Reason: reason, AtNS: runner.emit.clock.NowNS(),
	}
	return publishPayload(ctx, runner.emit, runner.transitionOutput, parent, transitionType, transition, "transition")
}
