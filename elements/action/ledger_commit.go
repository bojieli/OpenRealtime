package action

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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
	trajectoryService, trajectoryRevision, found := mount.Services.Lookup(TrajectoryStoreService)
	if !found {
		return nil, fmt.Errorf("action.LedgerCommit %s has no trajectory store service", mount.InstanceID)
	}
	store, ok := trajectoryService.(*trajectory.Store)
	if !ok || store == nil {
		return nil, fmt.Errorf("action trajectory store service has type %T", trajectoryService)
	}
	toolService, toolRevision, found := mount.Services.Lookup(ToolRegistryService)
	if !found {
		return nil, fmt.Errorf("action.LedgerCommit %s has no tool registries service", mount.InstanceID)
	}
	tools, ok := toolService.(*ToolRegistries)
	if !ok || tools == nil {
		return nil, fmt.Errorf("tool registries service has type %T", toolService)
	}
	targetService, targetRevision, found := mount.Services.Lookup(TargetRegistryService)
	if !found {
		return nil, fmt.Errorf("action.LedgerCommit %s has no target registries service", mount.InstanceID)
	}
	targets, ok := targetService.(*TargetRegistries)
	if !ok || targets == nil {
		return nil, fmt.Errorf("target registries service has type %T", targetService)
	}
	confirmationService, confirmationRevision, found := mount.Services.Lookup(ConfirmationRegistryService)
	if !found {
		return nil, fmt.Errorf("action.LedgerCommit %s has no confirmation registries service", mount.InstanceID)
	}
	confirmations, ok := confirmationService.(*ConfirmationProviders)
	if !ok || confirmations == nil {
		return nil, fmt.Errorf("confirmation registries service has type %T", confirmationService)
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
		config: config, ledger: ledger, store: store, tools: tools, targets: targets, confirmations: confirmations,
		ledgerServiceRevision: serviceRevision, trajectoryServiceRevision: trajectoryRevision,
		toolServiceRevision: toolRevision, targetServiceRevision: targetRevision,
		confirmationServiceRevision: confirmationRevision,
		emit:                        emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		actionInput:                 actionInput, cancelInput: cancelInput, timeoutInput: timeoutInput,
		executableOutput: executableOutput, transitionOutput: transitionOutput,
		outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		preempted: newBoundedSet(config.MaxPending), resolution: mount.Resolution,
	}, nil
}

type ledgerCommitRunner struct {
	config                      LedgerCommitConfig
	ledger                      ledgerEntry
	store                       *trajectory.Store
	tools                       *ToolRegistries
	targets                     *TargetRegistries
	confirmations               *ConfirmationProviders
	ledgerServiceRevision       uint64
	trajectoryServiceRevision   uint64
	toolServiceRevision         uint64
	targetServiceRevision       uint64
	confirmationServiceRevision uint64
	emit                        emitter

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
		Digest: runner.ledger.identity,
		ServiceRevision: max(runner.ledgerServiceRevision, runner.trajectoryServiceRevision,
			runner.toolServiceRevision, runner.targetServiceRevision, runner.confirmationServiceRevision),
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, LedgerCommitDescriptor(),
		[]element.CapabilityResolution{
			ledgerArchitectureCapability(runner.ledger.reference, runner.ledgerServiceRevision),
			actionCapability("trajectory", "trajectory.Store/v1",
				"go://github.com/bojieli/OpenRealtime/trajectory/Store", runner.trajectoryServiceRevision, ""),
			actionCapability("tool-declarations", "action.ToolRegistry/v1",
				"registry://"+ToolRegistryService, runner.toolServiceRevision, ""),
			actionCapability("targets", "computeruse.Target/v1",
				"registry://"+TargetRegistryService, runner.targetServiceRevision, ""),
			actionCapability("confirmations", "action.ConfirmationProvider/v1",
				"registry://"+ConfirmationRegistryService, runner.confirmationServiceRevision, ""),
		}); err != nil {
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
	canonical, ok := canonicalActionPayload(envelope.Payload)
	if !ok {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: "prepare", Code: "invalid_payload",
			Message: fmt.Sprintf("canonical action payload has type %T", envelope.Payload),
		})
	}
	call := callOfCanonical(canonical)
	if err := validateCanonicalAction(canonical); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: "prepare", CallID: call.CallID,
			Code: "invalid_canonical_action", Message: err.Error(),
		})
	}
	admitted := canonical.Authorized.Confirmed.Declared.Admitted
	if strings.TrimSpace(envelope.SessionID) == "" || envelope.SessionID != admitted.SessionID {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: "attest", CallID: call.CallID,
			Code: "session_mismatch", Message: "canonical action envelope and admitted evidence must name one non-empty session",
		})
	}
	if strings.TrimSpace(envelope.RunID) == "" || envelope.RunID != admitted.ModelRunID {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: "attest", CallID: call.CallID,
			Code: "model_run_mismatch", Message: "canonical action envelope and admitted evidence name different cognition runs",
		})
	}
	if code, err := attestCanonicalAction(runner.store, canonical); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: "attest", CallID: call.CallID,
			Code: code, Message: err.Error(),
		})
	}
	if err := attestDeploymentAuthority(runner.tools, runner.targets, runner.confirmations, canonical.Authorized); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: "attest", CallID: call.CallID,
			Code: "deployment_authority_mismatch", Message: err.Error(),
		})
	}
	identity := actionIdentity(admitted)
	if runner.preempted.contains(identity) {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeCanceled, Stage: "ledger_commit", Operation: "prepare", CallID: call.CallID,
			Code: "preempted", Message: "action was canceled before ledger admission",
		})
	}
	commitmentID := actionCommitmentID(admitted)
	kind := legacyaction.KindToolCall
	if computeruse.IsAction(call.Name) {
		kind = legacyaction.KindComputerAction
	}
	commitment := legacyaction.Commitment{
		ID: commitmentID, Kind: kind, CallID: identity,
		SourceRevision: canonical.Authorized.Confirmed.Declared.Admitted.SourceRevision,
		Confirm:        canonical.Authorized.Confirmed.Declared.Confirmation,
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
	capability, err := runner.ledger.sign(canonical, commitmentID)
	if err != nil {
		_, _ = runner.ledger.ledger.Cancel(commitmentID, err.Error())
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeFailed, Stage: "ledger_commit", Operation: "authorize", CallID: call.CallID,
			Code: "capability_failed", Message: err.Error(),
		})
	}
	executable := ExecutableAction{
		Canonical: canonical, LedgerReference: runner.ledger.reference, LedgerIdentity: runner.ledger.identity,
		CommitmentID: commitmentID, Capability: capability,
	}
	if err := publishPayload(ctx, runner.emit, runner.executableOutput, envelope, executableType, executable, "executable"); err != nil {
		return err
	}
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: OutcomeSucceeded, Stage: "ledger_commit", Operation: "queue", CallID: call.CallID,
	})
}

func validateCanonicalAction(value CanonicalAction) error {
	if err := validateAuthorizedAction(value.Authorized); err != nil {
		return err
	}
	if strings.TrimSpace(value.ProposalItemID) == "" || strings.TrimSpace(value.TrajectoryItemID) == "" ||
		value.StoreVersion == 0 {
		return errors.New("canonical action has incomplete trajectory promotion evidence")
	}
	if value.ProposalItemID == value.TrajectoryItemID {
		return errors.New("canonical proposal and tool-call identities must be distinct")
	}
	return nil
}

// attestCanonicalAction turns CanonicalAction from a typed claim into an
// attestation over one immutable historical prefix. A newer live store version
// is harmless: Prefix reads exactly the version named by the canonical safe
// point, while an altered identity or payload cannot be signed.
func attestCanonicalAction(store *trajectory.Store, canonical CanonicalAction) (string, error) {
	if store == nil {
		return "trajectory_unavailable", errors.New("canonical action attestation has no trajectory store")
	}
	prefix, err := store.Prefix(canonical.StoreVersion)
	if err != nil {
		return "store_version_mismatch", err
	}
	evidence, code, err := authorizedPromotionEvidence(prefix, canonical.Authorized)
	if err != nil {
		return code, err
	}
	if evidence.call == nil {
		return "canonical_call_missing", errors.New("canonical prefix does not contain the promoted tool call")
	}
	if evidence.proposal.ID != canonical.ProposalItemID || evidence.call.ID != canonical.TrajectoryItemID {
		return "canonical_identity_mismatch", fmt.Errorf(
			"canonical identities name proposal %q and call %q, trajectory attests %q and %q",
			canonical.ProposalItemID, canonical.TrajectoryItemID, evidence.proposal.ID, evidence.call.ID)
	}
	call := callOfCanonical(canonical)
	admitted := canonical.Authorized.Confirmed.Declared.Admitted
	modelCall := admitted.Proposal.Call
	if evidence.proposal.InvocationID != admitted.ModelRunID || evidence.call.InvocationID != admitted.ModelRunID ||
		evidence.proposal.SourceRevision != admitted.SourceRevision || evidence.call.SourceRevision != admitted.SourceRevision ||
		evidence.proposal.ToolCall.CallID != modelCall.CallID || evidence.call.ToolCall.CallID != call.CallID ||
		evidence.proposal.ToolCall.Name != modelCall.Name || evidence.call.ToolCall.Name != call.Name ||
		!bytes.Equal(evidence.proposal.ToolCall.Arguments, modelCall.Arguments) ||
		!bytes.Equal(evidence.call.ToolCall.Arguments, call.Arguments) ||
		!sameToolCallDerivation(evidence.call.ToolCallDerivation,
			toolCallDerivationOfDeclared(canonical.Authorized.Confirmed.Declared)) {
		return "canonical_payload_mismatch", errors.New("canonical proposal and effective call do not attest the admitted invocation")
	}
	if evidence.call.Producer.Phase != trajectory.PhaseRuntime {
		return "canonical_producer_mismatch", fmt.Errorf("canonical tool call producer phase is %q, want runtime", evidence.call.Producer.Phase)
	}
	if !slices.Contains(evidence.call.CausalParentIDs, evidence.proposal.ID) {
		return "canonical_promotion_mismatch", errors.New("canonical tool call does not causally derive from the exact proposal")
	}
	return "", nil
}

func validateAuthorizedAction(value AuthorizedAction) error {
	if err := validateConfirmedAction(value.Confirmed); err != nil {
		return err
	}
	if value.TargetReference == "" || value.TargetDigest == "" {
		return errors.New("authorized action has incomplete target resolution")
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
	identity, identityCode, err := interruptIdentity(envelope, interrupt)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "ledger_commit", Operation: operation,
			CallID: interrupt.CallID, Code: identityCode, Message: err.Error(),
		})
	}
	commitmentID := "action:" + identity
	commitment, found := runner.ledger.ledger.Lookup(commitmentID)
	if !found {
		already := runner.preempted.contains(identity)
		runner.preempted.add(identity)
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
	if commitment.CallID != identity {
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
