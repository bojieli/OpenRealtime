package action

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/element"
)

type confirmationFactory struct{}

var (
	_ element.Factory         = confirmationFactory{}
	_ element.ConfigValidator = confirmationFactory{}
)

func (confirmationFactory) Descriptor() element.Descriptor { return ConfirmationDescriptor() }
func (confirmationFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeConfirmationConfig(source)
	return err
}

func (confirmationFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeConfirmationConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("authority.Confirmation %s config: %w", mount.InstanceID, err)
	}
	service, serviceRevision, found := mount.Services.Lookup(ConfirmationRegistryService)
	if !found {
		return nil, fmt.Errorf("authority.Confirmation %s has no confirmation providers service", mount.InstanceID)
	}
	providers, ok := service.(*ConfirmationProviders)
	if !ok || providers == nil {
		return nil, fmt.Errorf("confirmation providers service has type %T", service)
	}
	registration, err := providers.resolve(config.Provider)
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
	confirmedOutput, err := mount.Ports.Output("confirmed")
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
	handle := &confirmationProviderHandle{}
	if err := mount.Lifecycle.Defer("close-confirmation-provider", handle.close); err != nil {
		return nil, err
	}
	return &confirmationRunner{
		config: config, registration: registration, serviceRevision: serviceRevision, providerHandle: handle,
		emit:        emitter{instance: mount.InstanceID, clock: dependencies.clock, sequences: dependencies.sequences},
		actionInput: actionInput, cancelInput: cancelInput, timeoutInput: timeoutInput,
		confirmedOutput: confirmedOutput, outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		terminal: newBoundedSet(config.MaxPending), resolution: mount.Resolution,
	}, nil
}

type confirmationProviderHandle struct {
	mu       sync.Mutex
	provider ConfirmationProvider
}

func (handle *confirmationProviderHandle) set(provider ConfirmationProvider) error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.provider != nil {
		return errors.New("confirmation provider handle is already initialized")
	}
	handle.provider = provider
	return nil
}

func (handle *confirmationProviderHandle) close(context.Context) error {
	handle.mu.Lock()
	provider := handle.provider
	handle.provider = nil
	handle.mu.Unlock()
	return closeIfPossible(provider)
}

var (
	errConfirmationCanceled = errors.New("confirmation canceled")
	errConfirmationTimedOut = errors.New("confirmation timed out")
)

type confirmationJob struct {
	envelope element.Envelope
	action   DeclaredAction
	started  uint64
	cancel   context.CancelCauseFunc
}

type confirmationCompletion struct {
	job      *confirmationJob
	approved bool
	err      error
	finished uint64
}

type confirmationRunner struct {
	config          ConfirmationConfig
	registration    confirmationRegistration
	serviceRevision uint64
	providerHandle  *confirmationProviderHandle
	provider        ConfirmationProvider
	emit            emitter

	actionInput     element.InputPort
	cancelInput     element.InputPort
	timeoutInput    element.InputPort
	confirmedOutput element.OutputPort
	outcomeOutput   element.OutputPort
	resolvedOutput  element.OutputPort

	active     *confirmationJob
	queue      []*confirmationJob
	terminal   *boundedSet
	resolution element.ResolutionReporter
}

func (runner *confirmationRunner) Run(parent context.Context) error {
	ctx, stop := context.WithCancelCause(parent)
	provider, err := createConfirmationProvider(runner.config.Provider, runner.registration)
	if err != nil {
		stop(err)
		return err
	}
	if err := runner.providerHandle.set(provider); err != nil {
		stop(err)
		return errors.Join(err, closeIfPossible(provider))
	}
	runner.provider = provider
	if err := publishResolution(ctx, runner.emit, runner.resolvedOutput, Resolution{
		Stage: "confirmation", Reference: runner.config.Provider, Identity: runner.registration.identity,
		Digest: mustDigestIdentity(runner.registration.identity), ServiceRevision: runner.serviceRevision,
	}); err != nil {
		stop(err)
		return err
	}
	if err := reportActionResolution(runner.resolution, ConfirmationDescriptor(),
		[]element.CapabilityResolution{actionCapability(
			"confirmation", "action.ConfirmationProvider/v1",
			"confirmation://"+runner.registration.identity, runner.serviceRevision,
			mustDigestIdentity(runner.registration.identity),
		)}); err != nil {
		stop(err)
		return err
	}

	inputs := make(chan receivedInput)
	failures := make(chan error, 3)
	completions := make(chan confirmationCompletion, 1)
	var receivers sync.WaitGroup
	for _, input := range []struct {
		kind string
		port element.InputPort
	}{{"action", runner.actionInput}, {"cancel", runner.cancelInput}, {"timeout", runner.timeoutInput}} {
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
			switch input.kind {
			case "action":
				err = runner.accept(ctx, input.envelope, completions, &workers)
			case "cancel", "timeout":
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

func (runner *confirmationRunner) accept(
	ctx context.Context, envelope element.Envelope, completions chan<- confirmationCompletion, workers *sync.WaitGroup,
) error {
	declared, ok := declaredPayload(envelope.Payload)
	if !ok {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "confirmation", Operation: "confirm", Code: "invalid_payload",
			Message: fmt.Sprintf("declared action payload has type %T", envelope.Payload),
		})
	}
	call := callOfDeclared(declared)
	if err := validateToolCall(call); err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
			Code: "invalid_action", Message: err.Error(),
		})
	}
	confirm, err := legacyaction.ParseConfirm(string(declared.Confirmation))
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
			Code: "invalid_requirement", Message: err.Error(),
		})
	}
	declared.Confirmation = confirm
	if runner.terminal.contains(call.CallID) {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeIgnored, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
			Code: "terminal_replay", Message: "confirmation call ID is already terminal",
		})
	}
	if confirm == legacyaction.ConfirmNever {
		runner.terminal.add(call.CallID)
		confirmed := ConfirmedAction{Declared: declared}
		if err := publishPayload(ctx, runner.emit, runner.confirmedOutput, envelope, confirmedType, confirmed, "confirmed"); err != nil {
			return err
		}
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeSucceeded, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
		})
	}
	if runner.active != nil && callOfDeclared(runner.active.action).CallID == call.CallID {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeIgnored, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
			Code: "duplicate_inflight", Message: "confirmation is already in flight",
		})
	}
	for _, queued := range runner.queue {
		if callOfDeclared(queued.action).CallID == call.CallID {
			return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
				Kind: OutcomeIgnored, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
				Code: "duplicate_queued", Message: "confirmation is already queued",
			})
		}
	}
	if len(runner.queue) >= runner.config.MaxPending {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
			Code: "capacity", Message: "confirmation queue is full",
		})
	}
	job := &confirmationJob{envelope: envelope.Clone(), action: cloneDeclared(declared)}
	if runner.active == nil {
		return runner.start(ctx, job, completions, workers)
	} else {
		runner.queue = append(runner.queue, job)
	}
	return nil
}

func (runner *confirmationRunner) start(
	ctx context.Context, job *confirmationJob, completions chan<- confirmationCompletion, workers *sync.WaitGroup,
) error {
	if actual := strings.TrimSpace(runner.provider.Name()); actual != runner.registration.identity {
		return fmt.Errorf("confirmation provider identity drifted after readiness: resolved %q, live %q",
			runner.registration.identity, actual)
	}
	job.started = runner.emit.clock.NowNS()
	confirmCtx, cancel := context.WithCancelCause(ctx)
	job.cancel = cancel
	runner.active = job
	request := legacyaction.ConfirmationRequest{
		Call: cloneToolCall(callOfDeclared(job.action)), Confirm: job.action.Confirmation, Target: job.action.Target,
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		approved, err := runner.provider.Confirm(confirmCtx, request)
		completion := confirmationCompletion{job: job, approved: approved, err: err, finished: runner.emit.clock.NowNS()}
		select {
		case completions <- completion:
		case <-ctx.Done():
		}
	}()
	return nil
}

func (runner *confirmationRunner) complete(
	ctx context.Context, completion confirmationCompletion, completions chan<- confirmationCompletion, workers *sync.WaitGroup,
) error {
	job := completion.job
	if runner.active != job {
		return nil
	}
	runner.active = nil
	call := callOfDeclared(job.action)
	runner.terminal.add(call.CallID)
	var err error
	switch {
	case errors.Is(completion.err, errConfirmationTimedOut):
		err = publishOutcome(ctx, runner.emit, runner.outcomeOutput, job.envelope, Outcome{
			Kind: OutcomeTimedOut, Stage: "confirmation", Operation: "timeout", CallID: call.CallID,
			Code: "timed_out", Message: completion.err.Error(), StartedNS: job.started, FinishedNS: completion.finished,
		})
	case errors.Is(completion.err, errConfirmationCanceled), errors.Is(completion.err, context.Canceled):
		err = publishOutcome(ctx, runner.emit, runner.outcomeOutput, job.envelope, Outcome{
			Kind: OutcomeCanceled, Stage: "confirmation", Operation: "cancel", CallID: call.CallID,
			Code: "canceled", Message: completion.err.Error(), StartedNS: job.started, FinishedNS: completion.finished,
		})
	case completion.err != nil:
		err = publishOutcome(ctx, runner.emit, runner.outcomeOutput, job.envelope, Outcome{
			Kind: OutcomeFailed, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
			Code: "provider_failed", Message: completion.err.Error(), StartedNS: job.started, FinishedNS: completion.finished,
		})
	case !completion.approved:
		err = publishOutcome(ctx, runner.emit, runner.outcomeOutput, job.envelope, Outcome{
			Kind: OutcomeDenied, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
			Code: "not_confirmed", Message: "action was not confirmed", StartedNS: job.started, FinishedNS: completion.finished,
		})
	default:
		confirmed := ConfirmedAction{
			Declared: job.action, ConfirmationNeeded: true,
			ProviderReference: runner.config.Provider, ProviderIdentity: runner.registration.identity,
		}
		if err = publishPayload(ctx, runner.emit, runner.confirmedOutput, job.envelope, confirmedType, confirmed, "confirmed"); err == nil {
			err = publishOutcome(ctx, runner.emit, runner.outcomeOutput, job.envelope, Outcome{
				Kind: OutcomeSucceeded, Stage: "confirmation", Operation: "confirm", CallID: call.CallID,
				StartedNS: job.started, FinishedNS: completion.finished,
			})
		}
	}
	if err != nil {
		return err
	}
	if len(runner.queue) != 0 {
		next := runner.queue[0]
		runner.queue = runner.queue[1:]
		return runner.start(ctx, next, completions, workers)
	}
	return nil
}

func (runner *confirmationRunner) interrupt(
	ctx context.Context, envelope element.Envelope, operation string,
	completions chan<- confirmationCompletion, workers *sync.WaitGroup,
) error {
	interrupt, code, err := interruptAddress(envelope)
	if err != nil {
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: OutcomeRejected, Stage: "confirmation", Operation: operation, Code: code, Message: err.Error(),
		})
	}
	cause := error(errConfirmationCanceled)
	kind := OutcomeCanceled
	if operation == "timeout" {
		cause, kind = errConfirmationTimedOut, OutcomeTimedOut
	}
	if interrupt.Reason != "" {
		cause = fmt.Errorf("%w: %s", cause, interrupt.Reason)
	}
	found := false
	if runner.active != nil && callOfDeclared(runner.active.action).CallID == interrupt.CallID {
		found = true
		runner.active.cancel(cause)
	}
	for index := 0; index < len(runner.queue); index++ {
		if callOfDeclared(runner.queue[index].action).CallID != interrupt.CallID {
			continue
		}
		found = true
		runner.queue = append(runner.queue[:index], runner.queue[index+1:]...)
		break
	}
	if !found {
		alreadyTerminal := runner.terminal.contains(interrupt.CallID)
		runner.terminal.add(interrupt.CallID)
		if alreadyTerminal {
			kind, code = OutcomeIgnored, "already_terminal"
		}
		if interrupt.Reason == "" {
			interrupt.Reason = operation
		}
		return publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
			Kind: kind, Stage: "confirmation", Operation: operation, CallID: interrupt.CallID,
			Code: code, Message: interrupt.Reason,
		})
	}
	// Active completion publishes the terminal event after the provider has
	// acknowledged cancellation. A queued job has no worker, so close it here.
	if runner.active != nil && callOfDeclared(runner.active.action).CallID == interrupt.CallID {
		return nil
	}
	runner.terminal.add(interrupt.CallID)
	if interrupt.Reason == "" {
		interrupt.Reason = operation
	}
	if err := publishOutcome(ctx, runner.emit, runner.outcomeOutput, envelope, Outcome{
		Kind: kind, Stage: "confirmation", Operation: operation, CallID: interrupt.CallID,
		Message: interrupt.Reason,
	}); err != nil {
		return err
	}
	if runner.active == nil && len(runner.queue) != 0 {
		next := runner.queue[0]
		runner.queue = runner.queue[1:]
		return runner.start(ctx, next, completions, workers)
	}
	return nil
}

func mustDigestIdentity(identity string) string {
	digest, _ := digestJSON(struct {
		Identity string `json:"identity"`
	}{Identity: identity})
	return digest
}
