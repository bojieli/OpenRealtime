package cognition

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	committedCancelMemory           = 512
	maximumCommittedIdentifierBytes = 256
)

type textModelFactory struct{}

var (
	_ element.Factory         = textModelFactory{}
	_ element.ConfigValidator = textModelFactory{}
)

func (textModelFactory) Descriptor() element.Descriptor { return TextModelDescriptor() }

func (textModelFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeTextModelConfig(source)
	return err
}

func (textModelFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeTextModelConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("cognition.TextModel %s config: %w", mount.InstanceID, err)
	}
	service, registryRevision, found := mount.Services.Lookup(ProviderRegistryService)
	if !found {
		return nil, fmt.Errorf("cognition.TextModel %s has no provider registry service", mount.InstanceID)
	}
	registry, ok := service.(*ProviderRegistry)
	if !ok || registry == nil {
		return nil, fmt.Errorf("cognition provider registry service has type %T", service)
	}
	entry, err := registry.resolve(config.Provider)
	if err != nil {
		return nil, err
	}
	clockService, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("cognition text model has no runtime clock service")
	}
	clock, ok := clockService.(graphruntime.Clock)
	if !ok || reflectedNil(clock) {
		return nil, fmt.Errorf("runtime clock service has type %T", clockService)
	}
	var media continuation.MediaResolver
	if service, _, found := mount.Services.Lookup(MediaResolverService); found {
		switch typed := service.(type) {
		case continuation.MediaResolver:
			media = typed
		case func(string) (continuation.Media, error):
			media = continuation.MediaResolver(typed)
		default:
			return nil, fmt.Errorf("cognition media resolver service has type %T", service)
		}
		if media == nil {
			return nil, errors.New("cognition media resolver service is nil")
		}
	}

	contextInput, err := mount.Ports.Input("context")
	if err != nil {
		return nil, err
	}
	triggerInput, err := mount.Ports.Input("trigger")
	if err != nil {
		return nil, err
	}
	cancelInput, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	textOutput, err := mount.Ports.Output("text")
	if err != nil {
		return nil, err
	}
	resultOutput, err := mount.Ports.Output("result")
	if err != nil {
		return nil, err
	}
	toolOutput, err := mount.Ports.Output("tools")
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

	handle := &providerHandle{}
	if err := mount.Lifecycle.Defer("close-cognition-provider", handle.close); err != nil {
		return nil, err
	}
	return &textModelRunner{
		instance: mount.InstanceID, reference: config.Provider, entry: entry,
		registryRevision: registryRevision, providerHandle: handle, clock: clock, media: media,
		retainReasoning: config.RetainReasoning,
		maxOutputBytes:  config.MaxOutputBytes, maxEvents: config.MaxEvents,
		maxToolProposals: config.MaxToolProposals,
		contextInput:     contextInput, triggerInput: triggerInput, cancelInput: cancelInput,
		textOutput: textOutput, resultOutput: resultOutput, toolOutput: toolOutput,
		outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		resolution: mount.Resolution, committedCanceledRuns: make(map[[sha256.Size]byte]struct{}),
	}, nil
}

type sampledContext struct {
	envelope element.Envelope
	snapshot trajectory.Snapshot
}

type textModelRunner struct {
	instance         string
	reference        string
	entry            providerEntry
	registryRevision uint64
	providerHandle   *providerHandle
	provider         continuation.Provider
	clock            graphruntime.Clock
	media            continuation.MediaResolver
	retainReasoning  bool
	maxOutputBytes   int
	maxEvents        int
	maxToolProposals int

	contextInput   element.InputPort
	triggerInput   element.InputPort
	cancelInput    element.InputPort
	textOutput     element.OutputPort
	resultOutput   element.OutputPort
	toolOutput     element.OutputPort
	outcomeOutput  element.OutputPort
	resolvedOutput element.OutputPort
	resolution     element.ResolutionReporter

	latest                    *sampledContext
	committedSessionID        string
	committedVersionFloor     uint64
	committedVersionAdmitted  bool
	committedCanceledRuns     map[[sha256.Size]byte]struct{}
	committedCanceledRunOrder [][sha256.Size]byte
}

func (runner *textModelRunner) Run(parent context.Context) error {
	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	provider, err := createProvider(runner.reference, runner.entry)
	if err != nil {
		return err
	}
	if err := runner.providerHandle.set(provider); err != nil {
		return errors.Join(err, providerCloseFailure(runner.reference, provider))
	}
	runner.provider = provider
	if err := reportTextModelLiveResolution(runner.resolution, runner.entry.descriptor); err != nil {
		return fmt.Errorf("attest cognition provider %q: %w", runner.reference, err)
	}
	if err := runner.publishResolution(ctx); err != nil {
		return err
	}

	contexts := make(chan element.Envelope)
	triggers := make(chan element.Envelope)
	interrupts := make(chan element.Envelope)
	triggerPermits := make(chan struct{}, 1)
	triggerPermits <- struct{}{}
	receiveErrors := make(chan error, 3)
	var receivers sync.WaitGroup
	receivers.Add(3)
	go receiveModelInput(ctx, "context", runner.contextInput, contexts, receiveErrors, &receivers)
	go receiveModelTriggers(ctx, runner.triggerInput, triggerPermits, triggers, receiveErrors, &receivers)
	go receiveModelInput(ctx, "cancel", runner.cancelInput, interrupts, receiveErrors, &receivers)
	defer func() {
		stop(nil)
		receivers.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-receiveErrors:
			return err
		case envelope := <-contexts:
			if err := runner.acceptContext(envelope); err != nil {
				return err
			}
		case envelope := <-interrupts:
			if err := runner.cancelIdle(ctx, envelope); err != nil {
				return err
			}
		case envelope := <-triggers:
			generate, runID, refusal := runner.prepareTrigger(envelope)
			if refusal != nil {
				if err := runner.publishOutcome(ctx, envelope, *refusal); err != nil {
					return err
				}
				if !permitNextTrigger(ctx, triggerPermits) {
					return nil
				}
				continue
			}
			if err := runner.verifyLiveProvider(); err != nil {
				return err
			}
			sampled, canceled, err := runner.sampleForTrigger(
				ctx, envelope, runID, generate,
				contexts, interrupts, receiveErrors,
			)
			if err != nil {
				return err
			}
			if canceled || sampled == nil {
				if !permitNextTrigger(ctx, triggerPermits) {
					return nil
				}
				continue
			}
			if err := runner.runGeneration(
				ctx, envelope, runID, generate, *sampled,
				contexts, interrupts, receiveErrors,
			); err != nil {
				return err
			}
			if !permitNextTrigger(ctx, triggerPermits) {
				return nil
			}
		}
	}
}

// receiveModelTriggers takes a permit before dequeueing. The bounded graph
// edge therefore remains the only waiting queue, and a trigger is admitted at
// the same point its State input can be sampled rather than being hidden in an
// element-local channel while an earlier run is active.
func receiveModelTriggers(
	ctx context.Context, input element.InputPort, permits <-chan struct{},
	output chan<- element.Envelope, errorsOut chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-permits:
		}
		envelope, err := input.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, graphruntime.ErrChannelClosed) {
				select {
				case errorsOut <- fmt.Errorf("receive cognition trigger: %w", err):
				case <-ctx.Done():
				}
			}
			return
		}
		select {
		case output <- envelope:
		case <-ctx.Done():
			return
		}
	}
}

func permitNextTrigger(ctx context.Context, permits chan<- struct{}) bool {
	select {
	case permits <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func receiveModelInput(
	ctx context.Context, name string, input element.InputPort, output chan<- element.Envelope,
	errorsOut chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, graphruntime.ErrChannelClosed) {
				select {
				case errorsOut <- fmt.Errorf("receive cognition %s: %w", name, err):
				case <-ctx.Done():
				}
			}
			return
		}
		select {
		case output <- envelope:
		case <-ctx.Done():
			return
		}
	}
}

func (runner *textModelRunner) acceptContext(envelope element.Envelope) error {
	snapshot, ok := snapshotPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("cognition context payload has type %T, want trajectory.Snapshot", envelope.Payload)
	}
	if runner.latest != nil && snapshot.Version < runner.latest.snapshot.Version {
		return fmt.Errorf("cognition context version regressed from %d to %d",
			runner.latest.snapshot.Version, snapshot.Version)
	}
	runner.latest = &sampledContext{envelope: envelope.Clone(), snapshot: snapshot}
	return nil
}

func (runner *textModelRunner) prepareTrigger(
	envelope element.Envelope,
) (Generate, string, *Outcome) {
	generate, ok := generatePayload(envelope.Payload)
	runID := strings.TrimSpace(envelope.RunID)
	if !ok {
		return Generate{}, runID, &Outcome{
			Kind: OutcomeRefused, Operation: "generate", RunID: runID,
			ProviderReference: runner.reference, Code: "invalid_payload",
			Message: fmt.Sprintf("generation payload has type %T", envelope.Payload),
		}
	}
	if runID == "" {
		return Generate{}, "", &Outcome{
			Kind: OutcomeRefused, Operation: "generate", ProviderReference: runner.reference,
			Code: "missing_run_id", Message: "generation trigger requires an envelope run ID",
		}
	}
	if err := continuation.ValidateInvocation(generate.Invocation, runner.entry.descriptor); err != nil {
		return Generate{}, runID, &Outcome{
			Kind: OutcomeRefused, Operation: "generate", RunID: runID,
			ProviderReference: runner.reference, Code: "invalid_invocation", Message: err.Error(),
		}
	}
	if expected := generate.ExpectedContextItemID; expected != "" &&
		(strings.TrimSpace(expected) != expected || strings.ContainsAny(expected, "\x00\r\n\t")) {
		return Generate{}, runID, &Outcome{
			Kind: OutcomeRefused, Operation: "generate", RunID: runID,
			ProviderReference: runner.reference, Code: "invalid_context_identity",
			Message: "expected context item ID is not canonical",
		}
	}
	if committed := generate.CommittedContext; committed != nil {
		if err := validateCommittedIdentifier("session ID", envelope.SessionID); err != nil {
			return Generate{}, runID, &Outcome{
				Kind: OutcomeRefused, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, Code: "invalid_context_session",
				Message: err.Error(),
			}
		}
		if committed.Prefix.Version == 0 {
			return Generate{}, runID, &Outcome{
				Kind: OutcomeRefused, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, Code: "invalid_committed_context",
				Message: "committed context prefix version must be positive",
			}
		}
		if err := validateCommittedIdentifier("State item ID", committed.StateItemID); err != nil {
			return Generate{}, runID, &Outcome{
				Kind: OutcomeRefused, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, Code: "invalid_context_identity",
				Message: err.Error(),
			}
		}
		if generate.ExpectedContextVersion == nil || generate.ExpectedContextItemID == "" {
			return Generate{}, runID, &Outcome{
				Kind: OutcomeRefused, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, Code: "incomplete_committed_context",
				Message: "committed context requires both expected context fields",
			}
		}
		if *generate.ExpectedContextVersion != committed.Prefix.Version ||
			generate.ExpectedContextItemID != committed.StateItemID {
			return Generate{}, runID, &Outcome{
				Kind: OutcomeRefused, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, Code: "committed_context_mismatch",
				Message: "committed context does not agree with expected context fields",
			}
		}
		if !slices.Contains(envelope.CausalParents, committed.StateItemID) {
			return Generate{}, runID, &Outcome{
				Kind: OutcomeRefused, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, Code: "missing_context_cause",
				Message: fmt.Sprintf("generation trigger does not causally name committed context item %q",
					committed.StateItemID),
			}
		}
	}
	if generate.ExpectedContextItemID != "" && generate.ExpectedContextVersion == nil {
		return Generate{}, runID, &Outcome{
			Kind: OutcomeRefused, Operation: "generate", RunID: runID,
			ProviderReference: runner.reference, Code: "incomplete_context_constraint",
			Message: "expected context item ID requires an expected context version",
		}
	}
	return generate, runID, nil
}

// sampleForTrigger waits for the required seeded State value. Legacy exact
// triggers retain their strict latest-version behavior. A commit-bound trigger
// may instead reconstruct and verify its immutable prefix from a later
// append-only snapshot.
func (runner *textModelRunner) sampleForTrigger(
	ctx context.Context, trigger element.Envelope, runID string, generate Generate,
	contexts <-chan element.Envelope, interrupts <-chan element.Envelope,
	receiveErrors <-chan error,
) (*sampledContext, bool, error) {
	expected := generate.ExpectedContextVersion
	expectedItemID := generate.ExpectedContextItemID
	committed := generate.CommittedContext
	if committed != nil {
		if runner.committedSessionID != "" && runner.committedSessionID != trigger.SessionID {
			outcome := Outcome{
				Kind: OutcomeRefused, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, ContextVersion: committed.Prefix.Version,
				Code: "committed_session_mismatch",
				Message: fmt.Sprintf("committed context session %q does not match mounted session %q",
					trigger.SessionID, runner.committedSessionID),
			}
			return nil, false, runner.publishOutcome(ctx, trigger, outcome)
		}
		if runner.wasCommittedRunCanceled(trigger.SessionID, runID) {
			outcome := Outcome{
				Kind: OutcomeRefused, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, ContextVersion: committed.Prefix.Version,
				Code:    "committed_run_replay",
				Message: "committed generation run is already terminal after cancellation",
			}
			return nil, false, runner.publishOutcome(ctx, trigger, outcome)
		}
		if runner.committedVersionAdmitted && committed.Prefix.Version <= runner.committedVersionFloor {
			outcome := Outcome{
				Kind: OutcomeRefused, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, ContextVersion: committed.Prefix.Version,
				Code: "committed_context_replay",
				Message: fmt.Sprintf("committed context version %d is not newer than admitted version %d",
					committed.Prefix.Version, runner.committedVersionFloor),
			}
			return nil, false, runner.publishOutcome(ctx, trigger, outcome)
		}
	}
	for {
		if runner.latest != nil {
			version := runner.latest.snapshot.Version
			if committed != nil && version >= committed.Prefix.Version {
				cause := trigger.Clone()
				cause.CausalParents = appendUnique(cause.CausalParents, runner.latest.envelope.ItemID)
				switch {
				case runner.latest.envelope.SessionID != trigger.SessionID:
					outcome := Outcome{
						Kind: OutcomeRefused, Operation: "generate", RunID: runID,
						ProviderReference: runner.reference, ContextVersion: committed.Prefix.Version,
						Code: "context_session_mismatch",
						Message: fmt.Sprintf("generation session %q does not match context session %q",
							trigger.SessionID, runner.latest.envelope.SessionID),
					}
					return nil, false, runner.publishOutcome(ctx, cause, outcome)
				case version == committed.Prefix.Version &&
					runner.latest.envelope.ItemID != committed.StateItemID:
					outcome := Outcome{
						Kind: OutcomeRefused, Operation: "generate", RunID: runID,
						ProviderReference: runner.reference, ContextVersion: committed.Prefix.Version,
						Code: "context_identity_mismatch",
						Message: fmt.Sprintf("generation requires context item %q, latest is %q",
							committed.StateItemID, runner.latest.envelope.ItemID),
					}
					return nil, false, runner.publishOutcome(ctx, cause, outcome)
				}
				prefix, err := trajectory.Prefix(runner.latest.snapshot, committed.Prefix)
				if err != nil {
					outcome := Outcome{
						Kind: OutcomeRefused, Operation: "generate", RunID: runID,
						ProviderReference: runner.reference, ContextVersion: committed.Prefix.Version,
						Code: "context_prefix_mismatch", Message: err.Error(),
					}
					return nil, false, runner.publishOutcome(ctx, cause, outcome)
				}
				basis := runner.latest.envelope.Clone()
				basis.ItemID = committed.StateItemID
				basis.Payload = prefix
				runner.committedSessionID = trigger.SessionID
				runner.committedVersionFloor = committed.Prefix.Version
				runner.committedVersionAdmitted = true
				return &sampledContext{envelope: basis, snapshot: prefix}, false, nil
			}
			switch {
			case expected == nil || version == *expected &&
				(expectedItemID == "" || runner.latest.envelope.ItemID == expectedItemID):
				copy := *runner.latest
				copy.envelope = runner.latest.envelope.Clone()
				copy.snapshot = cloneSnapshot(runner.latest.snapshot)
				return &copy, false, nil
			case expected != nil && version == *expected && expectedItemID != "" &&
				runner.latest.envelope.ItemID != expectedItemID:
				cause := trigger.Clone()
				cause.CausalParents = appendUnique(cause.CausalParents, runner.latest.envelope.ItemID)
				outcome := Outcome{
					Kind: OutcomeRefused, Operation: "generate", RunID: runID,
					ProviderReference: runner.reference, ContextVersion: version,
					Code: "context_identity_mismatch",
					Message: fmt.Sprintf("generation requires context item %q, latest is %q",
						expectedItemID, runner.latest.envelope.ItemID),
				}
				return nil, false, runner.publishOutcome(ctx, cause, outcome)
			case expected != nil && version > *expected:
				cause := trigger.Clone()
				cause.CausalParents = appendUnique(cause.CausalParents, runner.latest.envelope.ItemID)
				outcome := Outcome{
					Kind: OutcomeRefused, Operation: "generate", RunID: runID,
					ProviderReference: runner.reference, ContextVersion: version,
					Code:    "context_version_mismatch",
					Message: fmt.Sprintf("generation requires context version %d, latest is %d", *expected, version),
				}
				return nil, false, runner.publishOutcome(ctx, cause, outcome)
			}
		}
		select {
		case <-ctx.Done():
			return nil, false, nil
		case err := <-receiveErrors:
			return nil, false, err
		case envelope := <-contexts:
			if err := runner.acceptContext(envelope); err != nil {
				return nil, false, err
			}
		case envelope := <-interrupts:
			cancel, valid := cancelPayload(envelope.Payload)
			if !valid {
				if err := runner.publishOutcome(ctx, envelope, invalidCancelOutcome(runner.reference, envelope)); err != nil {
					return nil, false, err
				}
				continue
			}
			address, addressErr := cancelAddress(envelope, cancel)
			if addressErr != nil {
				if err := runner.publishOutcome(ctx, envelope, cancelAddressOutcome(runner.reference, envelope, addressErr)); err != nil {
					return nil, false, err
				}
				continue
			}
			if address != runID {
				if err := runner.publishOutcome(ctx, envelope, Outcome{
					Kind: OutcomeIgnored, Operation: "cancel", RunID: address,
					ProviderReference: runner.reference, Code: "run_not_active",
					Message: fmt.Sprintf("generation %q is waiting for context", runID),
				}); err != nil {
					return nil, false, err
				}
				continue
			}
			outcome := Outcome{
				Kind: OutcomeCanceled, Operation: "generate", RunID: runID,
				ProviderReference: runner.reference, Code: "canceled_before_start",
				Message: strings.TrimSpace(cancel.Reason), FinishedNS: runner.clock.NowNS(),
			}
			if committed != nil {
				runner.rememberCommittedCanceledRun(trigger.SessionID, runID)
			}
			return nil, true, runner.publishOutcomeWithParents(ctx, trigger, &envelope, outcome)
		}
	}
}

// A commit-bound trigger canceled before its prefix arrives never reaches the
// monotonic version floor. Retain a bounded terminal tombstone so replaying
// that same addressed run cannot turn the canceled request into model work.
// Keys are fixed-size commitments to keep hostile envelope identifiers from
// defeating the memory bound. Eviction matches the repository's other
// terminal-memory horizons; admitted versions remain refused permanently by
// the scalar floor.
func (runner *textModelRunner) rememberCommittedCanceledRun(sessionID, runID string) {
	key := committedRunKey(sessionID, runID)
	if _, found := runner.committedCanceledRuns[key]; found {
		return
	}
	runner.committedCanceledRuns[key] = struct{}{}
	runner.committedCanceledRunOrder = append(runner.committedCanceledRunOrder, key)
	if len(runner.committedCanceledRunOrder) <= committedCancelMemory {
		return
	}
	oldest := runner.committedCanceledRunOrder[0]
	runner.committedCanceledRunOrder = runner.committedCanceledRunOrder[1:]
	delete(runner.committedCanceledRuns, oldest)
}

func (runner *textModelRunner) wasCommittedRunCanceled(sessionID, runID string) bool {
	_, found := runner.committedCanceledRuns[committedRunKey(sessionID, runID)]
	return found
}

func committedRunKey(sessionID, runID string) [sha256.Size]byte {
	hash := sha256.New()
	_, _ = hash.Write([]byte("openrealtime.cognition/committed-run/v1"))
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(sessionID)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write([]byte(sessionID))
	binary.BigEndian.PutUint64(size[:], uint64(len(runID)))
	_, _ = hash.Write(size[:])
	_, _ = hash.Write([]byte(runID))
	var key [sha256.Size]byte
	copy(key[:], hash.Sum(nil))
	return key
}

func validateCommittedIdentifier(label, value string) error {
	if value == "" {
		return fmt.Errorf("committed context %s is required", label)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("committed context %s has surrounding whitespace", label)
	}
	if len(value) > maximumCommittedIdentifierBytes {
		return fmt.Errorf("committed context %s exceeds %d bytes", label, maximumCommittedIdentifierBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("committed context %s is not valid UTF-8", label)
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return fmt.Errorf("committed context %s contains whitespace or control characters", label)
		}
	}
	return nil
}

var errGenerationCanceled = errors.New("cognition generation canceled")

func (runner *textModelRunner) runGeneration(
	ctx context.Context, trigger element.Envelope, runID string, generate Generate,
	sampled sampledContext, contexts <-chan element.Envelope,
	interrupts <-chan element.Envelope, receiveErrors <-chan error,
) error {
	runCtx, cancelRun := context.WithCancelCause(ctx)
	started := runner.clock.NowNS()
	execution := newGenerationExecution(runner, runCtx, trigger, runID, generate, sampled)
	finished := make(chan executionResult, 1)
	go func() { finished <- execution.invoke() }()
	var interrupt *element.Envelope
	for {
		select {
		case <-ctx.Done():
			cancelRun(context.Cause(ctx))
			<-finished
			return nil
		case err := <-receiveErrors:
			cancelRun(err)
			<-finished
			return err
		case envelope := <-contexts:
			if err := runner.acceptContext(envelope); err != nil {
				cancelRun(err)
				<-finished
				return err
			}
		case envelope := <-interrupts:
			request, valid := cancelPayload(envelope.Payload)
			if !valid {
				if err := runner.publishOutcome(ctx, envelope, invalidCancelOutcome(runner.reference, envelope)); err != nil {
					cancelRun(err)
					<-finished
					return err
				}
				continue
			}
			address, addressErr := cancelAddress(envelope, request)
			if addressErr != nil {
				if err := runner.publishOutcome(ctx, envelope, cancelAddressOutcome(runner.reference, envelope, addressErr)); err != nil {
					cancelRun(err)
					<-finished
					return err
				}
				continue
			}
			if address != runID {
				if err := runner.publishOutcome(ctx, envelope, Outcome{
					Kind: OutcomeIgnored, Operation: "cancel", RunID: address,
					ProviderReference: runner.reference, Code: "run_not_active",
					Message: fmt.Sprintf("active generation is %q", runID),
				}); err != nil {
					cancelRun(err)
					<-finished
					return err
				}
				continue
			}
			if interrupt != nil {
				if err := runner.publishOutcome(ctx, envelope, Outcome{
					Kind: OutcomeIgnored, Operation: "cancel", RunID: runID,
					ProviderReference: runner.reference, Code: "already_canceled",
				}); err != nil {
					cancelRun(err)
					<-finished
					return err
				}
				continue
			}
			copy := envelope.Clone()
			interrupt = &copy
			reason := strings.TrimSpace(request.Reason)
			if reason == "" {
				reason = "interrupt"
			}
			cancelRun(fmt.Errorf("%w: %s", errGenerationCanceled, reason))
		case result := <-finished:
			cancelRun(nil)
			return runner.completeGeneration(ctx, trigger, interrupt, started, execution, result)
		}
	}
}

func (runner *textModelRunner) cancelIdle(ctx context.Context, envelope element.Envelope) error {
	request, valid := cancelPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, invalidCancelOutcome(runner.reference, envelope))
	}
	address, err := cancelAddress(envelope, request)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, cancelAddressOutcome(runner.reference, envelope, err))
	}
	if runner.wasCommittedRunCanceled(envelope.SessionID, address) {
		return runner.publishOutcome(ctx, envelope, Outcome{
			Kind: OutcomeIgnored, Operation: "cancel", RunID: address,
			ProviderReference: runner.reference, Code: "already_canceled",
			Message: "the exact committed-context run is already canceled",
		})
	}
	// A lossless cancellation lane can overtake its independently queued
	// committed-context trigger. Retain the exact run tombstone before
	// acknowledging quiescence so that a later trigger is refused before any
	// provider work. Legacy triggers do not consult this memory.
	runner.rememberCommittedCanceledRun(envelope.SessionID, address)
	return runner.publishOutcome(ctx, envelope, Outcome{
		Kind: OutcomeCanceled, Operation: "generate", RunID: address,
		ProviderReference: runner.reference, Code: "canceled_before_start",
		Message: strings.TrimSpace(request.Reason), FinishedNS: runner.clock.NowNS(),
	})
}

type executionResult struct {
	completion    continuation.Completion
	providerErr   error
	eventErr      error
	completionErr error
	deliveryErr   error
}

type generationExecution struct {
	mu     sync.Mutex
	closed bool

	runner   *textModelRunner
	ctx      context.Context
	trigger  element.Envelope
	runID    string
	generate Generate
	sampled  sampledContext

	declared    map[string]struct{}
	outputs     []PreparedOutput
	tools       []ToolProposal
	assistant   strings.Builder
	reasoning   strings.Builder
	textOpened  bool
	textIndex   uint64
	toolIndex   uint64
	eventCount  int
	outputBytes int
	eventErr    error
	deliveryErr error
	runtimeText continuation.RuntimeAnnotationFilter
}

func newGenerationExecution(
	runner *textModelRunner, ctx context.Context, trigger element.Envelope,
	runID string, generate Generate, sampled sampledContext,
) *generationExecution {
	declared := make(map[string]struct{}, len(generate.Invocation.Tools))
	for _, tool := range generate.Invocation.Tools {
		declared[tool.Name] = struct{}{}
	}
	return &generationExecution{
		runner: runner, ctx: ctx, trigger: trigger.Clone(), runID: runID,
		generate: generate, sampled: sampled, declared: declared,
	}
}

func (execution *generationExecution) invoke() (result executionResult) {
	request := continuation.Request{
		InvocationID: execution.runID,
		Descriptor:   execution.runner.entry.descriptor,
		Trajectory:   cloneSnapshot(execution.sampled.snapshot),
		Invocation:   cloneInvocation(execution.generate.Invocation),
		Media:        execution.runner.media,
	}
	result.completion, result.providerErr = execution.callProvider(request)
	execution.mu.Lock()
	result.eventErr = execution.eventErr
	result.deliveryErr = execution.deliveryErr
	execution.mu.Unlock()
	result.completion = cloneCompletion(result.completion)
	if result.providerErr == nil {
		result.completionErr = validateCompletion(result.completion, execution.runner.entry.descriptor)
	}
	return result
}

// callProvider closes the callback before it returns, even when a plugin
// panics. A provider that incorrectly retains Emit receives a stable error and
// cannot mutate an already-published Result or race the next invocation.
func (execution *generationExecution) callProvider(
	request continuation.Request,
) (completion continuation.Completion, providerErr error) {
	defer func() {
		execution.mu.Lock()
		execution.closed = true
		execution.mu.Unlock()
		if recovered := recover(); recovered != nil {
			providerErr = fmt.Errorf("continuation provider panic: %v", recovered)
		}
	}()
	return execution.runner.provider.Continue(execution.ctx, request, execution.emit)
}

func (execution *generationExecution) emit(event continuation.Event) error {
	execution.mu.Lock()
	defer execution.mu.Unlock()
	if execution.closed {
		return errors.New("continuation provider emitted after completion")
	}
	if execution.eventErr != nil {
		return execution.eventErr
	}
	if execution.deliveryErr != nil {
		return execution.deliveryErr
	}
	if err := continuation.ValidateEvent(event); err != nil {
		execution.eventErr = err
		return err
	}
	if execution.eventCount >= execution.runner.maxEvents {
		err := fmt.Errorf("continuation exceeded the configured %d-event output bound",
			execution.runner.maxEvents)
		execution.eventErr = err
		return err
	}
	execution.eventCount++
	switch event.Kind {
	case continuation.EventReasoningDelta:
		if err := execution.consumeOutputBytes(len(event.Text)); err != nil {
			execution.eventErr = err
			return err
		}
		if execution.runner.retainReasoning {
			execution.reasoning.WriteString(event.Text)
			execution.appendTextOutput(PreparedReasoning, event.Text)
		}
	case continuation.EventAssistantDelta:
		if err := execution.consumeOutputBytes(len(event.Text)); err != nil {
			execution.eventErr = err
			return err
		}
		if !execution.textOpened {
			if err := execution.publishText(TextBegin, "", false); err != nil {
				execution.deliveryErr = err
				return err
			}
			execution.textOpened = true
		}
		safe := execution.runtimeText.Push(event.Text)
		if safe == "" {
			break
		}
		if err := execution.publishText(TextChunk, safe, false); err != nil {
			execution.deliveryErr = err
			return err
		}
		execution.assistant.WriteString(safe)
		execution.appendTextOutput(PreparedAssistant, safe)
	case continuation.EventToolCall:
		if execution.runner.entry.descriptor.EffectiveToolAuthority() == continuation.ToolAuthorityNone {
			err := errors.New("continuation emitted a tool call without proposal or execution authority")
			execution.eventErr = err
			return err
		}
		if len(execution.tools) >= execution.runner.maxToolProposals {
			err := fmt.Errorf("continuation exceeded the configured %d-tool-proposal bound",
				execution.runner.maxToolProposals)
			execution.eventErr = err
			return err
		}
		if err := execution.consumeOutputBytes(
			len(event.ToolCall.CallID) + len(event.ToolCall.Name) + len(event.ToolCall.Arguments),
		); err != nil {
			execution.eventErr = err
			return err
		}
		_, declared := execution.declared[event.ToolCall.Name]
		proposal := ToolProposal{
			Call: cloneToolCall(*event.ToolCall), Declared: declared,
			ProviderAuthority: execution.runner.entry.descriptor.EffectiveToolAuthority(),
		}
		if err := execution.publishTool(proposal); err != nil {
			execution.deliveryErr = err
			return err
		}
		execution.tools = append(execution.tools, cloneToolProposal(proposal))
		copy := cloneToolProposal(proposal)
		execution.outputs = append(execution.outputs, PreparedOutput{Kind: PreparedTool, Proposal: &copy})
	}
	return nil
}

func (execution *generationExecution) consumeOutputBytes(count int) error {
	if count < 0 || count > execution.runner.maxOutputBytes-execution.outputBytes {
		return fmt.Errorf("continuation exceeded the configured %d-byte output bound",
			execution.runner.maxOutputBytes)
	}
	execution.outputBytes += count
	return nil
}

func (execution *generationExecution) appendTextOutput(kind PreparedOutputKind, text string) {
	if len(execution.outputs) > 0 {
		last := &execution.outputs[len(execution.outputs)-1]
		if last.Kind == kind && last.Proposal == nil {
			last.Text += text
			return
		}
	}
	execution.outputs = append(execution.outputs, PreparedOutput{Kind: kind, Text: text})
}

func (execution *generationExecution) publishText(
	boundary TextBoundary, text string, interrupted bool,
) error {
	index := execution.textIndex
	execution.textIndex++
	envelope := execution.baseEnvelope(textType, "text", index)
	envelope.Sequence = index + 1
	envelope.Payload = PreparedTextDelta{
		Boundary: boundary, Text: text, Index: index, Interrupted: interrupted,
		SpokeOver: execution.generate.SpokeOver,
	}
	return broadcast(execution.ctx, execution.runner.textOutput, envelope)
}

func (execution *generationExecution) publishTool(proposal ToolProposal) error {
	index := execution.toolIndex
	execution.toolIndex++
	envelope := execution.baseEnvelope(toolProposalType, "tool", index)
	envelope.Sequence = index + 1
	envelope.Payload = cloneToolProposal(proposal)
	return broadcast(execution.ctx, execution.runner.toolOutput, envelope)
}

func (execution *generationExecution) baseEnvelope(
	typeOf element.Type, label string, index uint64,
) element.Envelope {
	envelope := execution.trigger.Clone()
	envelope.Type = typeOf
	envelope.RunID = execution.runID
	envelope.ItemID = execution.trigger.ItemID + ":" + label + ":" + strconv.FormatUint(index, 10)
	envelope.CausalParents = appendUnique(envelope.CausalParents, execution.trigger.ItemID)
	envelope.CausalParents = appendUnique(envelope.CausalParents, execution.sampled.envelope.ItemID)
	if items := execution.sampled.snapshot.Items; len(items) != 0 {
		// Bind every streaming model artifact to the exact canonical prefix it
		// sampled. Authority adapters can then verify that a proposed external
		// effect descends from the named context tail rather than joining an
		// unrelated user item by call ID alone.
		envelope.CausalParents = appendUnique(envelope.CausalParents, items[len(items)-1].ID)
	}
	return envelope
}

func (runner *textModelRunner) completeGeneration(
	ctx context.Context, trigger element.Envelope, interrupt *element.Envelope,
	started uint64, execution *generationExecution, executionResult executionResult,
) error {
	canceled := interrupt != nil
	if executionResult.deliveryErr != nil && !canceledDelivery(executionResult.deliveryErr, canceled) {
		return fmt.Errorf("publish cognition stream for run %q: %w", execution.runID, executionResult.deliveryErr)
	}
	providerErr := executionResult.providerErr
	if providerErr == nil {
		providerErr = executionResult.eventErr
	}
	if providerErr == nil {
		providerErr = executionResult.completionErr
	}
	interrupted := canceled || providerErr != nil
	if execution.textOpened {
		// A short suffix may have been held because it looked like the start of
		// a reserved runtime marker. Release it only after a successful provider
		// terminal proves that the marker never completed. An interrupted stream
		// fails closed instead of making a partial control token audible.
		if !interrupted {
			if tail := execution.runtimeText.Finish(); tail != "" {
				if err := execution.publishText(TextChunk, tail, false); err != nil {
					return fmt.Errorf("flush cognition text stream for run %q: %w", execution.runID, err)
				}
				execution.assistant.WriteString(tail)
				execution.appendTextOutput(PreparedAssistant, tail)
			}
		}
		// The provider's run context may already be canceled. Framing closure
		// uses the still-live element context so an explicit interrupt cannot
		// strand a stream-aware downstream arbiter.
		original := execution.ctx
		execution.ctx = ctx
		err := execution.publishText(TextEnd, "", interrupted)
		execution.ctx = original
		if err != nil {
			return fmt.Errorf("close cognition text stream for run %q: %w", execution.runID, err)
		}
	}
	finished := runner.clock.NowNS()
	result := Result{
		RunID: execution.runID, ProviderReference: runner.reference,
		Descriptor:     runner.entry.descriptor,
		ContextVersion: execution.sampled.snapshot.Version,
		Invocation:     cloneInvocation(execution.generate.Invocation),
		Outputs:        clonePreparedOutputs(execution.outputs),
		AssistantText:  execution.assistant.String(), ReasoningText: execution.reasoning.String(),
		ReasoningRetained: execution.runner.retainReasoning,
		ToolProposals:     cloneToolProposals(execution.tools),
		Completion:        cloneCompletion(executionResult.completion), Interrupted: interrupted,
	}
	if len(execution.sampled.snapshot.Items) > 0 {
		result.ContextTailID = execution.sampled.snapshot.Items[len(execution.sampled.snapshot.Items)-1].ID
	}
	resultEnvelope := resultEnvelope(trigger, execution.sampled.envelope, execution.runID, result)
	if err := broadcast(ctx, runner.resultOutput, resultEnvelope); err != nil {
		return err
	}
	outcome := Outcome{
		Kind: OutcomeSucceeded, Operation: "generate", RunID: execution.runID,
		ProviderReference: runner.reference, ContextVersion: execution.sampled.snapshot.Version,
		StartedNS: started, FinishedNS: finished, DurationNS: elapsed(started, finished),
	}
	switch {
	case canceled:
		outcome.Kind, outcome.Code = OutcomeCanceled, "canceled"
		request, _ := cancelPayload(interrupt.Payload)
		outcome.Message = strings.TrimSpace(request.Reason)
	case executionResult.eventErr != nil:
		outcome.Kind, outcome.Code, outcome.Message = OutcomeFailed, "invalid_provider_event", executionResult.eventErr.Error()
	case executionResult.completionErr != nil:
		outcome.Kind, outcome.Code, outcome.Message = OutcomeFailed, "invalid_completion", executionResult.completionErr.Error()
	case executionResult.providerErr != nil:
		outcome.Kind, outcome.Code, outcome.Message = OutcomeFailed, "provider_error", executionResult.providerErr.Error()
	}
	cause := trigger.Clone()
	cause.CausalParents = appendUnique(cause.CausalParents, execution.sampled.envelope.ItemID)
	return runner.publishOutcomeWithParents(ctx, cause, interrupt, outcome)
}

func (runner *textModelRunner) verifyLiveProvider() error {
	actual := runner.provider.Descriptor()
	if err := continuation.ValidateDescriptor(actual); err != nil {
		return fmt.Errorf("cognition provider %q changed to an invalid descriptor: %w", runner.reference, err)
	}
	if !reflect.DeepEqual(actual, runner.entry.descriptor) {
		return fmt.Errorf("cognition provider %q descriptor drifted after resolution: resolved %+v, live %+v",
			runner.reference, runner.entry.descriptor, actual)
	}
	return nil
}

func (runner *textModelRunner) publishResolution(ctx context.Context) error {
	digest, err := descriptorDigest(runner.entry.descriptor)
	if err != nil {
		return err
	}
	envelope := element.Envelope{
		Type: providerResolutionType, ItemID: runner.instance + ":resolved",
		Payload: ProviderResolution{
			Reference: runner.reference, Descriptor: runner.entry.descriptor,
			DescriptorDigest: digest, RegistryRevision: runner.registryRevision,
		},
	}
	return broadcast(ctx, runner.resolvedOutput, envelope)
}

func (runner *textModelRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome Outcome,
) error {
	return runner.publishOutcomeWithParents(ctx, cause, nil, outcome)
}

func (runner *textModelRunner) publishOutcomeWithParents(
	ctx context.Context, cause element.Envelope, other *element.Envelope, outcome Outcome,
) error {
	envelope := cause.Clone()
	envelope.Type = outcomeType
	envelope.RunID = outcome.RunID
	envelope.ItemID = cause.ItemID + ":outcome"
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	if other != nil {
		envelope.CausalParents = appendUnique(envelope.CausalParents, other.ItemID)
	}
	envelope.Payload = outcome
	return broadcast(ctx, runner.outcomeOutput, envelope)
}

func resultEnvelope(
	trigger element.Envelope, contextEnvelope element.Envelope, runID string, result Result,
) element.Envelope {
	envelope := trigger.Clone()
	envelope.Type = resultType
	envelope.RunID = runID
	envelope.ItemID = trigger.ItemID + ":result"
	envelope.CausalParents = appendUnique(envelope.CausalParents, trigger.ItemID)
	envelope.CausalParents = appendUnique(envelope.CausalParents, contextEnvelope.ItemID)
	envelope.Payload = result
	return envelope
}

func broadcast(ctx context.Context, output element.OutputPort, envelope element.Envelope) error {
	result, err := output.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if result.Delivered == 0 && len(output.Lanes()) != 0 {
		return fmt.Errorf("cognition output %s was not delivered", output.Name())
	}
	return nil
}

func generatePayload(payload any) (Generate, bool) {
	switch typed := payload.(type) {
	case Generate:
		typed.Invocation = cloneInvocation(typed.Invocation)
		typed.ExpectedContextVersion = cloneUint64Pointer(typed.ExpectedContextVersion)
		typed.CommittedContext = cloneCommittedContext(typed.CommittedContext)
		return typed, true
	case *Generate:
		if typed == nil {
			return Generate{}, false
		}
		copy := *typed
		copy.Invocation = cloneInvocation(typed.Invocation)
		copy.ExpectedContextVersion = cloneUint64Pointer(typed.ExpectedContextVersion)
		copy.CommittedContext = cloneCommittedContext(typed.CommittedContext)
		return copy, true
	default:
		return Generate{}, false
	}
}

func cancelPayload(payload any) (Cancel, bool) {
	switch typed := payload.(type) {
	case Cancel:
		return typed, true
	case *Cancel:
		if typed == nil {
			return Cancel{}, false
		}
		return *typed, true
	default:
		return Cancel{}, false
	}
}

var (
	errMissingCancelAddress  = errors.New("cancel requires a run ID")
	errConflictingCancelRuns = errors.New("cancel payload and envelope name different run IDs")
)

func cancelAddress(envelope element.Envelope, request Cancel) (string, error) {
	payloadRun := strings.TrimSpace(request.RunID)
	envelopeRun := strings.TrimSpace(envelope.RunID)
	if payloadRun != "" && envelopeRun != "" && payloadRun != envelopeRun {
		return "", errConflictingCancelRuns
	}
	if payloadRun != "" {
		return payloadRun, nil
	}
	if envelopeRun != "" {
		return envelopeRun, nil
	}
	if scope := strings.TrimSpace(envelope.CancellationScope); scope != "" {
		return scope, nil
	}
	return "", errMissingCancelAddress
}

func invalidCancelOutcome(reference string, envelope element.Envelope) Outcome {
	return Outcome{
		Kind: OutcomeRefused, Operation: "cancel", RunID: strings.TrimSpace(envelope.RunID),
		ProviderReference: reference, Code: "invalid_payload",
		Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload),
	}
}

func cancelAddressOutcome(reference string, envelope element.Envelope, err error) Outcome {
	code := "conflicting_run_id"
	if errors.Is(err, errMissingCancelAddress) {
		code = "missing_run_id"
	}
	return Outcome{
		Kind: OutcomeRefused, Operation: "cancel", RunID: strings.TrimSpace(envelope.RunID),
		ProviderReference: reference, Code: code, Message: err.Error(),
	}
}

func cloneUint64Pointer(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneCommittedContext(value *stateelements.CommittedContext) *stateelements.CommittedContext {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func snapshotPayload(payload any) (trajectory.Snapshot, bool) {
	switch typed := payload.(type) {
	case trajectory.Snapshot:
		return cloneSnapshot(typed), true
	case *trajectory.Snapshot:
		if typed == nil {
			return trajectory.Snapshot{}, false
		}
		return cloneSnapshot(*typed), true
	default:
		return trajectory.Snapshot{}, false
	}
}

func cloneSnapshot(snapshot trajectory.Snapshot) trajectory.Snapshot {
	result := trajectory.Snapshot{Version: snapshot.Version, Items: make([]trajectory.Item, len(snapshot.Items))}
	for index := range snapshot.Items {
		result.Items[index] = cloneTrajectoryItem(snapshot.Items[index])
	}
	return result
}

func cloneTrajectoryItem(item trajectory.Item) trajectory.Item {
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
		copy := *item.ToolResult
		copy.Output = slices.Clone(item.ToolResult.Output)
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

func cloneInvocation(invocation continuation.Invocation) continuation.Invocation {
	invocation.Capabilities = slices.Clone(invocation.Capabilities)
	invocation.Tools = slices.Clone(invocation.Tools)
	for index := range invocation.Tools {
		invocation.Tools[index].Parameters = slices.Clone(invocation.Tools[index].Parameters)
	}
	return invocation
}

func cloneCompletion(completion continuation.Completion) continuation.Completion {
	completion.ProviderState = slices.Clone(completion.ProviderState)
	return completion
}

func cloneToolCall(call trajectory.ToolCall) trajectory.ToolCall {
	call.Arguments = slices.Clone(call.Arguments)
	return call
}

func cloneToolProposal(proposal ToolProposal) ToolProposal {
	proposal.Call = cloneToolCall(proposal.Call)
	return proposal
}

func cloneToolProposals(proposals []ToolProposal) []ToolProposal {
	result := make([]ToolProposal, len(proposals))
	for index := range proposals {
		result[index] = cloneToolProposal(proposals[index])
	}
	return result
}

func clonePreparedOutputs(outputs []PreparedOutput) []PreparedOutput {
	result := make([]PreparedOutput, len(outputs))
	for index := range outputs {
		result[index] = outputs[index]
		if outputs[index].Proposal != nil {
			copy := cloneToolProposal(*outputs[index].Proposal)
			result[index].Proposal = &copy
		}
	}
	return result
}

func validateCompletion(
	completion continuation.Completion, descriptor continuation.Descriptor,
) error {
	if len(completion.ProviderState) == 0 && completion.ProviderStateType == "" {
		return nil
	}
	if completion.ProviderStateType == "" || len(completion.ProviderState) == 0 || !json.Valid(completion.ProviderState) {
		return errors.New("completion provider state type and valid JSON must be supplied together")
	}
	if descriptor.NativeStateType != "" && completion.ProviderStateType != descriptor.NativeStateType {
		return fmt.Errorf("completion state type %q does not match descriptor %q",
			completion.ProviderStateType, descriptor.NativeStateType)
	}
	return nil
}

func canceledDelivery(err error, canceled bool) bool {
	return canceled && (errors.Is(err, errGenerationCanceled) || errors.Is(err, context.Canceled))
}

func elapsed(start, finish uint64) uint64 {
	if finish < start {
		return 0
	}
	return finish - start
}

func appendUnique(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}
