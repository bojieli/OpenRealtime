package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type dispatchFactory struct{}

var (
	_ element.Factory         = dispatchFactory{}
	_ element.ConfigValidator = dispatchFactory{}
)

func (dispatchFactory) Descriptor() element.Descriptor { return DispatchDescriptor() }
func (dispatchFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeDispatchConfig(source)
	return err
}

func (dispatchFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeDispatchConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("action.Dispatch %s config: %w", mount.InstanceID, err)
	}
	toolService, toolRevision, found := mount.Services.Lookup(ToolRegistryService)
	if !found {
		return nil, fmt.Errorf("action.Dispatch %s has no tool registries service", mount.InstanceID)
	}
	toolRegistries, ok := toolService.(*ToolRegistries)
	if !ok || toolRegistries == nil {
		return nil, fmt.Errorf("tool registries service has type %T", toolService)
	}
	tools, err := toolRegistries.resolve(config.Registry)
	if err != nil {
		return nil, err
	}
	ledgerService, ledgerRevision, found := mount.Services.Lookup(LedgerRegistryService)
	if !found {
		return nil, fmt.Errorf("action.Dispatch %s has no ledger registries service", mount.InstanceID)
	}
	ledgerRegistries, ok := ledgerService.(*LedgerRegistries)
	if !ok || ledgerRegistries == nil {
		return nil, fmt.Errorf("ledger registries service has type %T", ledgerService)
	}
	ledger, err := ledgerRegistries.resolve(config.Ledger)
	if err != nil {
		return nil, err
	}
	targetService, targetRevision, found := mount.Services.Lookup(TargetRegistryService)
	if !found {
		return nil, fmt.Errorf("action.Dispatch %s has no target registries service", mount.InstanceID)
	}
	targets, ok := targetService.(*TargetRegistries)
	if !ok || targets == nil {
		return nil, fmt.Errorf("target registries service has type %T", targetService)
	}
	confirmationService, confirmationRevision, found := mount.Services.Lookup(ConfirmationRegistryService)
	if !found {
		return nil, fmt.Errorf("action.Dispatch %s has no confirmation registries service", mount.InstanceID)
	}
	confirmations, ok := confirmationService.(*ConfirmationProviders)
	if !ok || confirmations == nil {
		return nil, fmt.Errorf("confirmation registries service has type %T", confirmationService)
	}
	dependencies, err := resolveRuntimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	executeInput, err := mount.Ports.Input("execute")
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
	committedOutput, err := mount.Ports.Output("committed")
	if err != nil {
		return nil, err
	}
	resultOutput, err := mount.Ports.Output("result")
	if err != nil {
		return nil, err
	}
	transitionOutput, err := mount.Ports.Output("transition")
	if err != nil {
		return nil, err
	}
	auditOutput, err := mount.Ports.Output("audit")
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
	return &dispatchRunner{
		config: config, toolRegistries: toolRegistries, tools: tools, ledger: ledger,
		targets: targets, confirmations: confirmations,
		toolServiceRevision: toolRevision, ledgerServiceRevision: ledgerRevision,
		targetServiceRevision: targetRevision, confirmationServiceRevision: confirmationRevision,
		emit:         emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		executeInput: executeInput, cancelInput: cancelInput, timeoutInput: timeoutInput,
		committedOutput: committedOutput, resultOutput: resultOutput, transitionOutput: transitionOutput,
		auditOutput: auditOutput, outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		terminal: newBoundedSet(config.MaxCompleted), preempted: newBoundedSet(config.MaxPending),
		resolution: mount.Resolution,
	}, nil
}

var (
	errDispatchCanceled = errors.New("dispatch canceled")
	errDispatchTimedOut = errors.New("dispatch timed out")
)

type dispatchJob struct {
	envelope   element.Envelope
	executable ExecutableAction
	started    uint64
	crossed    uint64
	cancel     context.CancelCauseFunc
}

type dispatchCompletion struct {
	job      *dispatchJob
	result   trajectory.ToolResult
	err      error
	finished uint64
}

type dispatchRunner struct {
	config                      DispatchConfig
	toolRegistries              *ToolRegistries
	tools                       toolSet
	ledger                      ledgerEntry
	targets                     *TargetRegistries
	confirmations               *ConfirmationProviders
	toolServiceRevision         uint64
	ledgerServiceRevision       uint64
	targetServiceRevision       uint64
	confirmationServiceRevision uint64
	emit                        emitter

	executeInput     element.InputPort
	cancelInput      element.InputPort
	timeoutInput     element.InputPort
	committedOutput  element.OutputPort
	resultOutput     element.OutputPort
	transitionOutput element.OutputPort
	auditOutput      element.OutputPort
	outcomeOutput    element.OutputPort
	resolvedOutput   element.OutputPort

	active     *dispatchJob
	queue      []*dispatchJob
	terminal   *boundedSet
	preempted  *boundedSet
	resolution element.ResolutionReporter
}

func (runner *dispatchRunner) Run(parent context.Context) error {
	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	resolutionDigest, err := digestJSON(struct {
		ToolRegistry string `json:"tool_registry"`
		ToolDigest   string `json:"tool_digest"`
		Ledger       string `json:"ledger"`
		LedgerID     string `json:"ledger_identity"`
	}{runner.tools.reference, runner.tools.digest, runner.ledger.reference, runner.ledger.identity})
	if err != nil {
		return err
	}
	if err := publishResolution(ctx, runner.emit, runner.resolvedOutput, Resolution{
		Stage: "dispatch", Reference: runner.tools.reference + "+" + runner.ledger.reference,
		Identity: "action.Dispatch:" + runner.ledger.identity, Digest: resolutionDigest,
		ServiceRevision: max(runner.toolServiceRevision, runner.ledgerServiceRevision,
			runner.targetServiceRevision, runner.confirmationServiceRevision),
	}); err != nil {
		return err
	}
	if err := reportActionResolution(runner.resolution, DispatchDescriptor(),
		[]element.CapabilityResolution{
			actionCapability("dispatchers", "action.ToolRegistry/v1", "registry://"+runner.tools.reference,
				runner.toolServiceRevision, runner.tools.digest),
			ledgerArchitectureCapability(runner.ledger.reference, runner.ledgerServiceRevision),
			actionCapability("targets", "computeruse.Target/v1", "registry://"+TargetRegistryService,
				runner.targetServiceRevision, ""),
			actionCapability("confirmations", "action.ConfirmationProvider/v1",
				"registry://"+ConfirmationRegistryService, runner.confirmationServiceRevision, ""),
		}); err != nil {
		return err
	}
	inputs := make(chan receivedInput)
	failures := make(chan error, 3)
	completions := make(chan dispatchCompletion, 1)
	var receivers sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{{"execute", runner.executeInput}, {"cancel", runner.cancelInput}, {"timeout", runner.timeoutInput}} {
		receivers.Add(1)
		go receiveInputs(ctx, input.kind, input.port, inputs, failures, &receivers)
	}
	var workers sync.WaitGroup
	defer func() {
		stop(nil)
		if runner.active != nil {
			runner.active.cancel(context.Canceled)
		}
		receivers.Wait()
		workers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			stop(err)
			return err
		case input := <-inputs:
			var err error
			if input.kind == "execute" {
				err = runner.accept(ctx, input.envelope, completions, &workers)
			} else {
				err = runner.interrupt(ctx, input.envelope, input.kind, completions, &workers)
			}
			if err != nil {
				stop(err)
				return err
			}
		case completion := <-completions:
			if err := runner.complete(ctx, completion, completions, &workers); err != nil {
				stop(err)
				return err
			}
		}
	}
}

func (runner *dispatchRunner) accept(
	ctx context.Context, envelope element.Envelope, completions chan<- dispatchCompletion, workers *sync.WaitGroup,
) error {
	executable, ok := executablePayload(envelope.Payload)
	if !ok {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "dispatch", Operation: "execute", Code: "invalid_payload",
			Message: fmt.Sprintf("executable action payload has type %T", envelope.Payload),
		})
	}
	call := callOfExecutable(executable)
	admitted := executable.Canonical.Authorized.Confirmed.Declared.Admitted
	identity := actionIdentity(admitted)
	if code, err := validateActionEnvelopeIdentity(envelope, admitted); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "dispatch", Operation: "execute", CallID: call.CallID,
			Code: code, Message: err.Error(),
		})
	}
	if runner.terminal.contains(identity) {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeIgnored, Stage: "dispatch", Operation: "execute", CallID: call.CallID,
			Code: "terminal_replay", Message: "call ID is already terminal; effect will not repeat",
		})
	}
	if runner.active != nil && actionIdentity(runner.active.executable.Canonical.Authorized.Confirmed.Declared.Admitted) == identity {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeIgnored, Stage: "dispatch", Operation: "execute", CallID: call.CallID,
			Code: "duplicate_inflight", Message: "call is already dispatching",
		})
	}
	for _, queued := range runner.queue {
		if actionIdentity(queued.executable.Canonical.Authorized.Confirmed.Declared.Admitted) == identity {
			return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
				Kind: OutcomeIgnored, Stage: "dispatch", Operation: "execute", CallID: call.CallID,
				Code: "duplicate_queued", Message: "call is already queued",
			})
		}
	}
	if err := runner.validateExecutable(executable); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "dispatch", Operation: "execute", CallID: call.CallID,
			Code: "invalid_capability", Message: err.Error(),
		})
	}
	if runner.preempted.contains(identity) {
		runner.terminal.add(identity)
		reason := "canceled before the commit boundary"
		_, _ = runner.ledger.ledger.Cancel(executable.CommitmentID, reason)
		if err := runner.publishTransition(ctx, envelope, executable.CommitmentID, call.CallID,
			legacyaction.StateCancelled, reason); err != nil {
			return err
		}
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeCanceled, Stage: "dispatch", Operation: "execute", CallID: call.CallID,
			Code: "preempted", Message: reason,
		})
	}
	if len(runner.queue) >= runner.config.MaxPending {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "dispatch", Operation: "execute", CallID: call.CallID,
			Code: "capacity", Message: "dispatch queue is full",
		})
	}
	job := &dispatchJob{envelope: envelope.Clone(), executable: cloneExecutable(executable)}
	if runner.active == nil {
		return runner.start(ctx, job, completions, workers)
	}
	runner.queue = append(runner.queue, job)
	return nil
}

func (runner *dispatchRunner) validateExecutable(executable ExecutableAction) error {
	if err := validateCanonicalAction(executable.Canonical); err != nil {
		return err
	}
	call := callOfExecutable(executable)
	admitted := executable.Canonical.Authorized.Confirmed.Declared.Admitted
	identity := actionIdentity(admitted)
	if executable.CommitmentID != actionCommitmentID(admitted) {
		return errors.New("commitment identity does not match call identity")
	}
	if !runner.ledger.verify(executable) {
		return errors.New("executable action capability did not verify")
	}
	if err := attestDeploymentAuthority(runner.toolRegistries, runner.targets, runner.confirmations,
		executable.Canonical.Authorized); err != nil {
		return fmt.Errorf("deployment authority: %w", err)
	}
	declared := executable.Canonical.Authorized.Confirmed.Declared
	if declared.RegistryReference != runner.tools.reference || declared.RegistryDigest != runner.tools.digest {
		return errors.New("executable action names a different tool registry")
	}
	tool, found, err := runner.tools.lookup(call.Name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("tool %q is no longer declared", call.Name)
	}
	if tool.digest != declared.DeclarationDigest || tool.dispatcherIdentity != declared.DispatcherIdentity {
		return errors.New("executable action declaration does not match live resolution")
	}
	if tool.spec.Dispatcher == nil || reflectedNil(tool.spec.Dispatcher) {
		return fmt.Errorf("tool %q has no in-process dispatcher", call.Name)
	}
	commitment, found := runner.ledger.ledger.Lookup(executable.CommitmentID)
	if !found || commitment.CallID != identity || commitment.State != legacyaction.StateQueued {
		return fmt.Errorf("ledger commitment is absent or not queued")
	}
	return nil
}

func (runner *dispatchRunner) start(
	ctx context.Context, job *dispatchJob, completions chan<- dispatchCompletion, workers *sync.WaitGroup,
) error {
	call := cloneToolCall(callOfExecutable(job.executable))
	// Queue residence must not turn an earlier deployment resolution into
	// durable authority. Re-resolve the declaration, confirmation decision,
	// and target ownership at the last reversible point before ledger crossing.
	if err := attestDeploymentAuthority(runner.toolRegistries, runner.targets, runner.confirmations,
		job.executable.Canonical.Authorized); err != nil {
		identity := actionIdentity(job.executable.Canonical.Authorized.Confirmed.Declared.Admitted)
		runner.terminal.add(identity)
		reason := "deployment authority changed before dispatch: " + err.Error()
		_, _ = runner.ledger.ledger.Cancel(job.executable.CommitmentID, reason)
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, job.envelope, Outcome{
			Kind: OutcomeRejected, Stage: "dispatch", Operation: "execute", CallID: call.CallID,
			Code: "deployment_authority_mismatch", Message: reason,
		})
	}
	tool, _, err := runner.tools.lookup(call.Name)
	if err != nil {
		return err
	}
	job.started = runner.emit.clock.NowNS()
	dispatchCtx, cancel := context.WithCancelCause(ctx)
	admitted := job.executable.Canonical.Authorized.Confirmed.Declared.Admitted
	dispatchCtx = withDispatchContext(dispatchCtx, DispatchContext{
		SessionID: admitted.SessionID, RunID: admitted.ModelRunID,
		CommitmentID:        job.executable.CommitmentID,
		CanonicalCallItemID: job.executable.Canonical.TrajectoryItemID,
	})
	job.cancel = cancel
	runner.active = job

	// This is the only irreversible boundary in the graph-native action path.
	// The ledger records crossing immediately before the external dispatcher is
	// invoked, conservatively accounting for a dispatcher that starts an effect
	// and then returns an error.
	if err := runner.ledger.ledger.Emit(job.executable.CommitmentID); err != nil {
		runner.active = nil
		runner.terminal.add(actionIdentity(job.executable.Canonical.Authorized.Confirmed.Declared.Admitted))
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, job.envelope, Outcome{
			Kind: OutcomeFailed, Stage: "dispatch", Operation: "commit", CallID: call.CallID,
			Code: "ledger_emit_failed", Message: err.Error(),
		})
	}
	job.crossed = runner.emit.clock.NowNS()
	transition := LedgerTransition{
		CallID: call.CallID, CommitmentID: job.executable.CommitmentID, State: legacyaction.StateEmitting,
		Crossed: true, AtNS: job.crossed,
	}
	if err := publishPayload(ctx, runner.emit, runner.transitionOutput, job.envelope, transitionType, transition, "transition"); err != nil {
		return err
	}
	if err := publishPayload(ctx, runner.emit, runner.committedOutput, job.envelope, committedType,
		CommittedAction{Executable: job.executable, CrossedNS: job.crossed}, "committed"); err != nil {
		return err
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		result, dispatchErr := tool.spec.Dispatcher.Dispatch(dispatchCtx, call)
		completion := dispatchCompletion{job: job, result: result, err: dispatchErr, finished: runner.emit.clock.NowNS()}
		select {
		case completions <- completion:
		case <-ctx.Done():
		}
	}()
	return nil
}

func (runner *dispatchRunner) complete(
	ctx context.Context, completion dispatchCompletion, completions chan<- dispatchCompletion, workers *sync.WaitGroup,
) error {
	job := completion.job
	if runner.active != job {
		return nil
	}
	runner.active = nil
	call := callOfExecutable(job.executable)
	runner.terminal.add(actionIdentity(job.executable.Canonical.Authorized.Confirmed.Declared.Admitted))
	result := cloneToolResult(completion.result)
	hostSynthesized := completion.err != nil
	if completion.err != nil {
		result = trajectory.ToolResult{CallID: call.CallID, Name: call.Name, Error: completion.err.Error()}
	}
	identityError := result.CallID != call.CallID || result.Name != call.Name
	if identityError {
		hostSynthesized = true
		result = trajectory.ToolResult{CallID: call.CallID, Name: call.Name,
			Error: fmt.Sprintf("tool %q returned mismatched result identity", call.Name)}
	}
	if len(result.Output) == 0 && result.Error == "" {
		hostSynthesized = true
		result.Error = "dispatcher returned neither output nor error"
	}
	if len(result.Output) != 0 && (!json.Valid(result.Output) || result.Error != "") {
		hostSynthesized = true
		result.Output = nil
		result.Error = "dispatcher returned an invalid terminal result"
	}
	if err := runner.ledger.ledger.Complete(job.executable.CommitmentID, 0); err != nil {
		// A concurrent cancellation after the irreversible boundary may already
		// have conservatively moved the commitment to Played. That idempotent
		// terminal state is acceptable; every other ledger error is a fatal
		// divergence and must not be hidden behind a synthetic played event.
		current, found := runner.ledger.ledger.Lookup(job.executable.CommitmentID)
		if !found || current.State != legacyaction.StatePlayed {
			return fmt.Errorf("complete ledger commitment %s: %w",
				job.executable.CommitmentID, err)
		}
	}
	transition := LedgerTransition{
		CallID: call.CallID, CommitmentID: job.executable.CommitmentID, State: legacyaction.StatePlayed,
		Crossed: true, AtNS: completion.finished,
	}
	if err := publishPayload(ctx, runner.emit, runner.transitionOutput, job.envelope, transitionType, transition, "transition"); err != nil {
		return err
	}
	executionResult := ExecutionResult{
		Executable: cloneExecutable(job.executable),
		CompletionOrigin: func() CompletionOrigin {
			if hostSynthesized {
				return CompletionDispatcherError
			}
			return CompletionReturned
		}(),
		CallID: call.CallID, Name: call.Name, CommitmentID: job.executable.CommitmentID,
		Result: result, CrossedNS: job.crossed, FinishedNS: completion.finished,
	}
	resultCapability, err := runner.ledger.signResult(executionResult)
	if err != nil {
		return fmt.Errorf("authenticate dispatch result for %s: %w", call.CallID, err)
	}
	executionResult.ResultCapability = resultCapability
	resultCause := job.envelope.Clone()
	resultCause.OpportunityID = job.executable.Canonical.Authorized.Confirmed.Declared.Admitted.ActivationItemID
	if err := publishPayload(ctx, runner.emit, runner.resultOutput, resultCause, resultType, executionResult, "result"); err != nil {
		return err
	}
	audit := runner.audit(job, result, completion.finished)
	if err := publishPayload(ctx, runner.emit, runner.auditOutput, job.envelope, auditType, audit, "audit"); err != nil {
		return err
	}
	kind, code, message := OutcomeSucceeded, "", ""
	if result.Error != "" {
		kind, code, message = OutcomeFailed, "dispatch_failed", result.Error
		if errors.Is(completion.err, errDispatchCanceled) || errors.Is(completion.err, context.Canceled) {
			kind, code = OutcomeCanceled, "canceled_after_commit"
		}
		if errors.Is(completion.err, errDispatchTimedOut) {
			kind, code = OutcomeTimedOut, "timed_out_after_commit"
		}
	}
	if err := publishOutcome(ctx, runner.emit, runner.outcomeOutput, job.envelope, Outcome{
		Kind: kind, Stage: "dispatch", Operation: "execute", CallID: call.CallID, Code: code,
		Message: message, Crossed: true, StartedNS: job.started, FinishedNS: completion.finished,
	}); err != nil {
		return err
	}
	return runner.startNext(ctx, completions, workers)
}

func (runner *dispatchRunner) audit(job *dispatchJob, result trajectory.ToolResult, finished uint64) AuditRecord {
	declared := job.executable.Canonical.Authorized.Confirmed.Declared
	return AuditRecord{
		CallID: result.CallID, Name: result.Name, CommitmentID: job.executable.CommitmentID,
		ProposalItemID: declared.Admitted.ProposalItemID, ModelRunID: declared.Admitted.ModelRunID,
		SessionID: declared.Admitted.SessionID,
		Authority: declared.Admitted.Authority, AuthorityItemID: declared.Admitted.AuthorityItemID,
		Confirmation: declared.Confirmation, Target: declared.Target, RegistryReference: declared.RegistryReference,
		DeclarationDigest: declared.DeclarationDigest, DispatcherIdentity: declared.DispatcherIdentity,
		ProviderReference: declared.Admitted.ProviderReference,
		ModelResultDigest: declared.Admitted.ModelResultDigest, ModelProducer: declared.Admitted.ModelProducer,
		Crossed: true, Executed: true, ResultError: result.Error, StartedNS: job.started, FinishedNS: finished,
	}
}

func (runner *dispatchRunner) interrupt(
	ctx context.Context, envelope element.Envelope, operation string,
	completions chan<- dispatchCompletion, workers *sync.WaitGroup,
) error {
	interrupt, code, err := interruptAddress(envelope)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "dispatch", Operation: operation, Code: code, Message: err.Error(),
		})
	}
	identity, identityCode, err := interruptIdentity(envelope, interrupt)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "dispatch", Operation: operation,
			CallID: interrupt.CallID, Code: identityCode, Message: err.Error(),
		})
	}
	cause := error(errDispatchCanceled)
	kind := OutcomeCanceled
	if operation == "timeout" {
		cause, kind = errDispatchTimedOut, OutcomeTimedOut
	}
	if interrupt.Reason != "" {
		cause = fmt.Errorf("%w: %s", cause, interrupt.Reason)
	}
	if runner.active != nil &&
		actionIdentity(runner.active.executable.Canonical.Authorized.Confirmed.Declared.Admitted) == identity {
		runner.active.cancel(cause)
		// The external boundary was already crossed; completion reports the
		// terminal result and whether the dispatcher honored cancellation.
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeIgnored, Stage: "dispatch", Operation: operation, CallID: interrupt.CallID,
			Code: "already_crossed", Message: interrupt.Reason, Crossed: true,
		})
	}
	for index, queued := range runner.queue {
		if actionIdentity(queued.executable.Canonical.Authorized.Confirmed.Declared.Admitted) != identity {
			continue
		}
		runner.queue = append(runner.queue[:index], runner.queue[index+1:]...)
		runner.terminal.add(identity)
		_, _ = runner.ledger.ledger.Cancel(queued.executable.CommitmentID, interrupt.Reason)
		if err := runner.publishTransition(ctx, envelope, queued.executable.CommitmentID, interrupt.CallID,
			legacyaction.StateCancelled, interrupt.Reason); err != nil {
			return err
		}
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: kind, Stage: "dispatch", Operation: operation, CallID: interrupt.CallID,
			Message: interrupt.Reason,
		})
	}
	already := runner.terminal.contains(identity) || runner.preempted.contains(identity)
	runner.preempted.add(identity)
	if already {
		kind, code = OutcomeIgnored, "already_terminal"
	}
	if interrupt.Reason == "" {
		interrupt.Reason = operation
	}
	return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: kind, Stage: "dispatch", Operation: operation, CallID: interrupt.CallID,
		Code: code, Message: interrupt.Reason,
	})
}

func (runner *dispatchRunner) publishTransition(
	ctx context.Context, parent element.Envelope, commitmentID, callID string,
	state legacyaction.State, reason string,
) error {
	transition := LedgerTransition{
		CallID: callID, CommitmentID: commitmentID, State: state, Crossed: state.Crossed(),
		Reason: reason, AtNS: runner.emit.clock.NowNS(),
	}
	return publishPayload(ctx, runner.emit, runner.transitionOutput, parent, transitionType, transition, "transition")
}

func (runner *dispatchRunner) startNext(
	ctx context.Context, completions chan<- dispatchCompletion, workers *sync.WaitGroup,
) error {
	for runner.active == nil && len(runner.queue) != 0 {
		next := runner.queue[0]
		runner.queue = runner.queue[1:]
		if runner.preempted.contains(actionIdentity(next.executable.Canonical.Authorized.Confirmed.Declared.Admitted)) {
			continue
		}
		if err := runner.start(ctx, next, completions, workers); err != nil {
			return err
		}
	}
	return nil
}
