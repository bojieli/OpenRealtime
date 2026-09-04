package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"mime"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const intentDispositionInstruction = "Classify whether the complete durable user intent has settled after the exact successful effect and its exact post-effect observation. Choose succeeded only when the current observation directly proves the whole user intent is complete. Choose continue when the intent remains achievable but needs another action or later observation. Choose failed only when current authoritative evidence proves the user intent cannot or should not be completed. Choose indeterminate when the evidence is missing, ambiguous, contradictory, or insufficient. A successful tool call, visual quiet, or absence of another proposal is not by itself proof of success."

var intentDispositionOptions = []string{
	string(IntentDispositionContinue),
	string(IntentDispositionSucceeded),
	string(IntentDispositionFailed),
	string(IntentDispositionIndeterminate),
}

type intentDispositionProducerFactory struct{}

var (
	_ element.Factory         = intentDispositionProducerFactory{}
	_ element.ConfigValidator = intentDispositionProducerFactory{}
	_ element.Runnable        = (*intentDispositionProducerRunner)(nil)
)

func (intentDispositionProducerFactory) Descriptor() element.Descriptor {
	return IntentDispositionProducerDescriptor()
}

func (intentDispositionProducerFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeIntentDispositionProducerConfig(source)
	return err
}

func (intentDispositionProducerFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	if err := validatePolicyIdentifier("intent disposition producer instance ID", mount.InstanceID, true); err != nil {
		return nil, fmt.Errorf("policy.IntentDispositionProducer instance ID: %w", err)
	}
	config, err := decodeIntentDispositionProducerConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("policy.IntentDispositionProducer %s config: %w", mount.InstanceID, err)
	}
	storeValue, _, found := mount.Services.Lookup(stateelements.TrajectoryStoreService)
	if !found {
		return nil, fmt.Errorf("policy.IntentDispositionProducer %s has no canonical trajectory store", mount.InstanceID)
	}
	storeService, ok := storeValue.(*stateelements.TrajectoryStoreServiceValue)
	if !ok || storeService == nil || storeService.Store == nil {
		return nil, fmt.Errorf("canonical trajectory store service has type %T", storeValue)
	}
	if err := validatePolicyIdentifier("canonical trajectory store session ID", storeService.SessionID, true); err != nil {
		return nil, fmt.Errorf("policy.IntentDispositionProducer %s: %w", mount.InstanceID, err)
	}

	registryValue, registryRevision, found := mount.Services.Lookup(SemanticDeciderRegistryService)
	if !found {
		return nil, fmt.Errorf("policy.IntentDispositionProducer %s has no semantic decider registry service", mount.InstanceID)
	}
	registry, ok := registryValue.(*SemanticDeciderRegistry)
	if !ok || registry == nil {
		return nil, fmt.Errorf("semantic decider registry service has type %T", registryValue)
	}
	detector := config.ExpectedSettlement.Detector
	entry, err := registry.resolve(detector.Reference)
	if err != nil {
		return nil, err
	}
	if entry.descriptor.Revision != detector.Revision ||
		entry.descriptor.ConfigurationDigest != detector.ConfigurationDigest {
		return nil, errors.New("intent disposition detector identity differs from the registered semantic decider")
	}

	var media continuation.MediaResolver
	if service, _, available := mount.Services.Lookup(cognitionelements.MediaResolverService); available {
		switch typed := service.(type) {
		case continuation.MediaResolver:
			media = typed
		case func(string) (continuation.Media, error):
			media = continuation.MediaResolver(typed)
		default:
			return nil, fmt.Errorf("intent disposition media resolver service has type %T", service)
		}
		if media == nil {
			return nil, errors.New("intent disposition media resolver service is nil")
		}
	}
	if config.DirectVisualInput {
		if !entry.descriptor.Vision {
			return nil, errors.New("intent disposition direct visual input requires a vision-capable decider")
		}
		if media == nil {
			return nil, errors.New("intent disposition direct visual input requires a media resolver")
		}
	}

	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("intent disposition producer has no runtime clock service")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || semanticReflectedNil(clock) {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("intent disposition producer has no runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	ports, err := intentDispositionProducerPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	handle := &semanticDeciderHandle{}
	if mount.Lifecycle == nil {
		return nil, errors.New("intent disposition producer has no mount lifecycle")
	}
	if err := mount.Lifecycle.Defer("close-intent-disposition-decider", handle.close); err != nil {
		return nil, err
	}
	return &intentDispositionProducerRunner{
		instance: mount.InstanceID, sessionID: storeService.SessionID,
		config: config, store: storeService.Store, registry: registry,
		registryRevision: registryRevision, entry: entry, handle: handle,
		clock: clock, sequences: sequences, resolution: mount.Resolution,
		ports: ports, media: media,
		terminal:      make(map[string]struct{}),
		cancellations: make(map[intentDispositionCancellationAddress]struct{}),
		state: IntentDispositionProducerState{
			MaxPending: config.MaxPending, TerminalMemory: config.TerminalMemory,
			CancellationMemory: config.CancelMemory,
		},
	}, nil
}

type intentDispositionProducerPorts struct {
	probe, cancel                         element.InputPort
	disposition, state, outcome, resolved element.OutputPort
}

func intentDispositionProducerPortsFrom(ports element.Ports) (intentDispositionProducerPorts, error) {
	if ports == nil {
		return intentDispositionProducerPorts{}, errors.New("policy.IntentDispositionProducer has nil ports")
	}
	var result intentDispositionProducerPorts
	for _, entry := range []struct {
		name string
		port *element.InputPort
	}{{"probe", &result.probe}, {"cancel", &result.cancel}} {
		port, err := ports.Input(entry.name)
		if err != nil {
			return intentDispositionProducerPorts{}, err
		}
		*entry.port = port
	}
	for _, entry := range []struct {
		name string
		port *element.OutputPort
	}{
		{"disposition", &result.disposition}, {"state", &result.state},
		{"outcome", &result.outcome}, {"resolved", &result.resolved},
	} {
		port, err := ports.Output(entry.name)
		if err != nil {
			return intentDispositionProducerPorts{}, err
		}
		*entry.port = port
	}
	return result, nil
}

type intentDispositionCancellationAddress struct {
	session string
	intent  TemporalEvidenceItemIdentity
}

type intentDispositionProducerRequest struct {
	probe IntentSettlementProbe
}

func (request intentDispositionProducerRequest) address() intentDispositionCancellationAddress {
	return intentDispositionCancellationAddress{
		session: request.probe.SessionID, intent: request.probe.DurableIntent,
	}
}

type activeIntentDisposition struct {
	request  intentDispositionProducerRequest
	cancel   context.CancelCauseFunc
	canceled bool
}

type intentDispositionEvaluation struct {
	request         intentDispositionProducerRequest
	disposition     IntentDisposition
	code            string
	message         string
	providerFailure bool
	deciderTimedOut bool
}

type intentDispositionProducerInput struct {
	kind     string
	envelope element.Envelope
}

type intentDispositionProducerRunner struct {
	instance  string
	sessionID string
	config    IntentDispositionProducerConfig
	store     *trajectory.Store
	registry  *SemanticDeciderRegistry
	entry     semanticDeciderEntry
	handle    *semanticDeciderHandle

	registryRevision uint64
	clock            graphruntime.Clock
	sequences        *graphruntime.SequenceAllocator
	resolution       element.ResolutionReporter
	ports            intentDispositionProducerPorts
	media            continuation.MediaResolver
	decider          SemanticDecider

	pending []intentDispositionProducerRequest
	active  *activeIntentDisposition

	terminal               map[string]struct{}
	terminalOrder          []string
	cancellations          map[intentDispositionCancellationAddress]struct{}
	cancelOrder            []intentDispositionCancellationAddress
	state                  IntentDispositionProducerState
	lastDecisionFinishedNS uint64
	deciderUnavailable     bool
}

func (runner *intentDispositionProducerRunner) Run(parent context.Context) error {
	decider, descriptor, err := runner.registry.Open(runner.config.ExpectedSettlement.Detector.Reference)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(descriptor, runner.entry.descriptor) {
		return errors.Join(errors.New("intent disposition semantic decider registration changed after mount"), closeSemanticDecider(decider))
	}
	if err := runner.handle.set(decider); err != nil {
		return errors.Join(err, closeSemanticDecider(decider))
	}
	runner.decider = decider
	if err := runner.reportResolution(); err != nil {
		return err
	}
	if err := runner.publishResolved(parent); err != nil {
		return err
	}
	if err := runner.publishState(parent, runner.instance+":startup"); err != nil {
		return err
	}

	ctx, stop := context.WithCancelCause(parent)
	inputs := make(chan intentDispositionProducerInput)
	failures := make(chan error, 2)
	results := make(chan intentDispositionEvaluation, 1)
	var receivers sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{{"probe", runner.ports.probe}, {"cancel", runner.ports.cancel}} {
		receivers.Add(1)
		go receiveIntentDispositionProducerInputs(
			ctx, source.kind, source.port, inputs, failures, &receivers,
		)
	}
	defer func() {
		if runner.active != nil {
			runner.active.cancel(errors.New("intent disposition producer stopped"))
		}
		stop(nil)
		receivers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case result := <-results:
			if err := runner.acceptEvaluation(ctx, result, results); err != nil {
				return err
			}
		case input := <-inputs:
			if err := runner.acceptInput(ctx, input, results); err != nil {
				return err
			}
		}
	}
}

func receiveIntentDispositionProducerInputs(
	ctx context.Context, kind string, port element.InputPort,
	inputs chan<- intentDispositionProducerInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := port.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, graphruntime.ErrChannelClosed) {
				select {
				case failures <- err:
				case <-ctx.Done():
				}
			}
			return
		}
		select {
		case inputs <- intentDispositionProducerInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func (runner *intentDispositionProducerRunner) acceptInput(
	ctx context.Context, input intentDispositionProducerInput,
	results chan<- intentDispositionEvaluation,
) error {
	var parent string
	var err error
	switch input.kind {
	case "probe":
		parent, err = runner.acceptProbe(ctx, input.envelope)
	case "cancel":
		parent, err = runner.acceptCancellation(ctx, input.envelope)
	default:
		err = fmt.Errorf("unknown intent disposition producer input %q", input.kind)
	}
	if err != nil {
		return err
	}
	if err := runner.startNext(ctx, results); err != nil {
		return err
	}
	return runner.publishState(ctx, parent)
}

func (runner *intentDispositionProducerRunner) acceptProbe(
	ctx context.Context, envelope element.Envelope,
) (string, error) {
	probe, ok := intentDispositionProducerProbePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentSettlementProbeType()) {
		runner.state.Refused++
		return "", runner.publishOutcome(ctx, "", IntentDispositionProducerOutcome{
			Kind: IntentDispositionProducerRefused, Code: "invalid_probe",
			Message: boundedPolicyReason(fmt.Sprintf("intent disposition probe payload has type %T", envelope.Payload)),
		})
	}
	if err := preflightIntentSettlementProbe(probe); err != nil {
		runner.state.Refused++
		return "", runner.publishOutcome(ctx, "", IntentDispositionProducerOutcome{
			Kind: IntentDispositionProducerRefused, Code: "invalid_probe",
			Message: boundedPolicyReason(err.Error()),
		})
	}
	probe = cloneIntentSettlementProbe(probe)
	if err := validateIntentDispositionProbeEnvelope(envelope, probe, runner.sessionID); err != nil {
		runner.state.Refused++
		return "", runner.publishOutcome(ctx, "", producerOutcomeForProbe(
			probe, IntentDispositionProducerRefused, "invalid_probe_envelope", err.Error(),
		))
	}
	if err := VerifyIntentSettlementProbe(
		runner.store.Snapshot(), probe, runner.config.ExpectedSettlement,
	); err != nil {
		runner.state.Refused++
		return probe.ProbeID, runner.publishOutcome(ctx, probe.ProbeID, producerOutcomeForProbe(
			probe, IntentDispositionProducerRefused, "unverified_probe", err.Error(),
		))
	}
	if _, terminal := runner.terminal[probe.ProbeID]; terminal {
		runner.state.Ignored++
		return probe.ProbeID, runner.publishOutcome(ctx, probe.ProbeID, producerOutcomeForProbe(
			probe, IntentDispositionProducerIgnored, "duplicate_terminal_probe",
			"intent disposition probe is already terminal",
		))
	}
	if runner.hasProbe(probe.ProbeID) {
		runner.state.Ignored++
		return probe.ProbeID, runner.publishOutcome(ctx, probe.ProbeID, producerOutcomeForProbe(
			probe, IntentDispositionProducerIgnored, "duplicate_pending_probe",
			"intent disposition probe is already pending or active",
		))
	}
	request := intentDispositionProducerRequest{probe: probe}
	if _, canceled := runner.cancellations[request.address()]; canceled {
		runner.state.Canceled++
		return probe.ProbeID, runner.publishOutcome(ctx, probe.ProbeID, producerOutcomeForProbe(
			probe, IntentDispositionProducerCanceled, "canceled",
			"intent disposition probe names a canceled durable intent",
		))
	}
	if len(runner.pending) >= runner.config.MaxPending {
		runner.state.Refused++
		return probe.ProbeID, runner.publishOutcome(ctx, probe.ProbeID, producerOutcomeForProbe(
			probe, IntentDispositionProducerRefused, "pending_capacity",
			"intent disposition pending capacity is exhausted",
		))
	}
	runner.pending = append(runner.pending, request)
	runner.state.Pending = len(runner.pending)
	return probe.ProbeID, nil
}

func (runner *intentDispositionProducerRunner) acceptCancellation(
	ctx context.Context, envelope element.Envelope,
) (string, error) {
	cancellation, ok := intentSettlementCancellationPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentSettlementCancelType()) {
		runner.state.Refused++
		return "", runner.publishOutcome(ctx, "", IntentDispositionProducerOutcome{
			Kind: IntentDispositionProducerRefused, Code: "invalid_cancellation",
			Message: boundedPolicyReason(fmt.Sprintf("intent disposition cancellation payload has type %T", envelope.Payload)),
		})
	}
	if err := preflightIntentSettlementCancellation(cancellation); err != nil {
		runner.state.Refused++
		return "", runner.publishOutcome(ctx, "", IntentDispositionProducerOutcome{
			Kind: IntentDispositionProducerRefused, Code: "invalid_cancellation",
			Message: boundedPolicyReason(err.Error()),
		})
	}
	if err := validateIntentDispositionCancellationEnvelope(envelope, cancellation, runner.sessionID); err != nil {
		runner.state.Refused++
		return "", runner.publishOutcome(ctx, "", IntentDispositionProducerOutcome{
			Kind: IntentDispositionProducerRefused, Code: "invalid_cancellation_envelope",
			Message: boundedPolicyReason(err.Error()),
		})
	}
	if err := VerifyIntentSettlementCancellation(runner.store.Snapshot(), cancellation); err != nil {
		runner.state.Refused++
		return envelope.ItemID, runner.publishOutcome(ctx, envelope.ItemID, IntentDispositionProducerOutcome{
			Kind:                IntentDispositionProducerRefused,
			DurableIntentItemID: cancellation.DurableIntent.TrajectoryItemID,
			Code:                "unverified_cancellation", Message: boundedPolicyReason(err.Error()),
		})
	}
	address := intentDispositionCancellationAddress{
		session: cancellation.SessionID, intent: cancellation.DurableIntent,
	}
	if _, duplicate := runner.cancellations[address]; duplicate {
		runner.state.Ignored++
		return envelope.ItemID, runner.publishOutcome(ctx, envelope.ItemID, IntentDispositionProducerOutcome{
			Kind:                IntentDispositionProducerIgnored,
			DurableIntentItemID: cancellation.DurableIntent.TrajectoryItemID,
			Code:                "duplicate_cancellation", Message: "durable intent is already canceled",
		})
	}
	runner.rememberCancellation(address)
	retained := runner.pending[:0]
	for _, pending := range runner.pending {
		if pending.address() != address {
			retained = append(retained, pending)
		}
	}
	runner.pending = retained
	runner.state.Pending = len(runner.pending)
	if runner.active != nil && runner.active.request.address() == address && !runner.active.canceled {
		runner.active.canceled = true
		runner.active.cancel(errors.New("intent disposition canceled"))
	}
	runner.state.Canceled++
	return envelope.ItemID, runner.publishOutcome(ctx, envelope.ItemID, IntentDispositionProducerOutcome{
		Kind:                IntentDispositionProducerCanceled,
		DurableIntentItemID: cancellation.DurableIntent.TrajectoryItemID,
		Code:                "canceled", Message: boundedPolicyReason(cancellation.Reason),
	})
}

func (runner *intentDispositionProducerRunner) startNext(
	ctx context.Context, results chan<- intentDispositionEvaluation,
) error {
	if runner.active != nil || len(runner.pending) == 0 {
		return nil
	}
	request := runner.pending[0]
	runner.pending = slices.Delete(runner.pending, 0, 1)
	runner.state.Pending = len(runner.pending)
	if _, canceled := runner.cancellations[request.address()]; canceled {
		runner.state.Canceled++
		if err := runner.publishOutcome(ctx, request.probe.ProbeID, producerOutcomeForProbe(
			request.probe, IntentDispositionProducerCanceled, "canceled",
			"intent disposition probe names a canceled durable intent",
		)); err != nil {
			return err
		}
		return runner.startNext(ctx, results)
	}
	if err := VerifyIntentSettlementProbe(
		runner.store.Snapshot(), request.probe, runner.config.ExpectedSettlement,
	); err != nil {
		runner.state.Refused++
		if publishErr := runner.publishOutcome(ctx, request.probe.ProbeID, producerOutcomeForProbe(
			request.probe, IntentDispositionProducerRefused, "stale_probe", err.Error(),
		)); publishErr != nil {
			return publishErr
		}
		return runner.startNext(ctx, results)
	}
	decisionCtx, cancel := context.WithCancelCause(ctx)
	runner.active = &activeIntentDisposition{request: request, cancel: cancel}
	runner.state.Active = true
	startedFloor := request.probe.IssuedNS
	if runner.lastDecisionFinishedNS >= startedFloor {
		if runner.lastDecisionFinishedNS == ^uint64(0) {
			cancel(errors.New("intent disposition decision clock exhausted"))
			runner.active = nil
			runner.state.Active = false
			runner.state.Refused++
			if err := runner.publishOutcome(ctx, request.probe.ProbeID, producerOutcomeForProbe(
				request.probe, IntentDispositionProducerRefused, "decision_clock_exhausted",
				"intent disposition timing cannot advance beyond the prior decision",
			)); err != nil {
				return err
			}
			return runner.startNext(ctx, results)
		}
		startedFloor = runner.lastDecisionFinishedNS + 1
	}
	go runner.evaluate(decisionCtx, request, startedFloor, results)
	return nil
}

func (runner *intentDispositionProducerRunner) evaluate(
	ctx context.Context, request intentDispositionProducerRequest, startedFloor uint64,
	results chan<- intentDispositionEvaluation,
) {
	started := runner.clock.NowNS()
	if started < startedFloor {
		started = startedFloor
	}
	decisionCtx, cancel := context.WithTimeout(
		ctx, time.Duration(runner.entry.descriptor.DecisionTimeoutMS)*time.Millisecond,
	)
	defer cancel()
	evaluation := intentDispositionEvaluation{request: request}
	input, err := runner.decisionInput(decisionCtx, request.probe)
	var outcome coreinteraction.Outcome
	if err != nil {
		evaluation.code = "evidence_unavailable"
		if errors.Is(err, context.DeadlineExceeded) {
			evaluation.code = "decision_timeout"
		}
		evaluation.message = err.Error()
		evaluation.providerFailure = true
	} else {
		if runner.deciderUnavailable {
			err = errors.New("semantic decider is unavailable after an earlier deadline violation")
			evaluation.code = "decider_unavailable"
		} else {
			outcome, err = decideIntentDisposition(decisionCtx, runner.decider, input)
		}
		if err != nil {
			if evaluation.code == "" {
				evaluation.code = "decision_failed"
			}
			if errors.Is(err, context.DeadlineExceeded) {
				evaluation.code = "decision_timeout"
				evaluation.deciderTimedOut = true
			}
			evaluation.message = err.Error()
			evaluation.providerFailure = true
		}
	}
	kind := IntentDispositionIndeterminate
	if err == nil {
		kind, err = validateIntentDispositionOutcome(outcome)
		if err != nil {
			evaluation.code = "invalid_decision"
			evaluation.message = err.Error()
			evaluation.providerFailure = true
			kind = IntentDispositionIndeterminate
		}
	}
	finished := runner.clock.NowNS()
	if finished < started {
		finished = started
	}
	evaluation.disposition = IntentDisposition{
		Probe:    cloneIntentSettlementProbe(request.probe),
		Detector: request.probe.Detector, Kind: kind,
		DecisionStartedNS: started, DecisionFinishedNS: finished,
	}
	// There is exactly one active evaluation and results has capacity one. A
	// cancellation still has to return through the actor so it can retire the
	// active slot; dropping that result on ctx.Done would strand the queue.
	results <- evaluation
}

func (runner *intentDispositionProducerRunner) acceptEvaluation(
	ctx context.Context, result intentDispositionEvaluation,
	results chan<- intentDispositionEvaluation,
) error {
	active := runner.active
	if active == nil || active.request.probe.ProbeID != result.request.probe.ProbeID {
		return errors.New("intent disposition producer received a result without its exact active probe")
	}
	runner.active = nil
	runner.state.Active = false
	if active.canceled {
		return runner.finishEvaluation(ctx, result.request.probe.ProbeID, results)
	}
	if result.deciderTimedOut {
		// Do not issue another call on a client that ignored its deadline: the
		// timed-out call may still be running and SemanticDecider does not grant
		// concurrent use. Later probes receive explicit indeterminate results.
		runner.deciderUnavailable = true
	}
	probe := result.request.probe
	if err := VerifyIntentSettlementProbe(
		runner.store.Snapshot(), probe, runner.config.ExpectedSettlement,
	); err != nil {
		runner.state.Refused++
		if publishErr := runner.publishOutcome(ctx, probe.ProbeID, producerOutcomeForProbe(
			probe, IntentDispositionProducerRefused, "stale_probe_after_decision", err.Error(),
		)); publishErr != nil {
			return publishErr
		}
		return runner.finishEvaluation(ctx, probe.ProbeID, results)
	}
	if _, canceled := runner.cancellations[result.request.address()]; canceled {
		runner.state.Canceled++
		if err := runner.publishOutcome(ctx, probe.ProbeID, producerOutcomeForProbe(
			probe, IntentDispositionProducerCanceled, "canceled",
			"intent disposition completed after its durable intent was canceled",
		)); err != nil {
			return err
		}
		return runner.finishEvaluation(ctx, probe.ProbeID, results)
	}
	if err := validateIntentDisposition(result.disposition, runner.config.ExpectedSettlement.Detector); err != nil {
		return fmt.Errorf("intent disposition producer constructed invalid disposition: %w", err)
	}
	dispositionID, _, err := runner.publishDisposition(ctx, result.disposition)
	if err != nil {
		return err
	}
	runner.lastDecisionFinishedNS = result.disposition.DecisionFinishedNS
	if result.disposition.Kind != IntentDispositionIndeterminate {
		runner.rememberTerminal(probe.ProbeID)
	}
	runner.state.Produced++
	switch result.disposition.Kind {
	case IntentDispositionContinue:
		runner.state.Continued++
	case IntentDispositionSucceeded:
		runner.state.Succeeded++
	case IntentDispositionFailed:
		runner.state.FailedIntent++
	case IntentDispositionIndeterminate:
		runner.state.Indeterminate++
	}
	outcomeKind := IntentDispositionProducerProduced
	if result.providerFailure {
		outcomeKind = IntentDispositionProducerFailed
		runner.state.ProviderFailures++
	}
	outcome := producerOutcomeForProbe(probe, outcomeKind, result.code, result.message)
	outcome.Disposition = result.disposition.Kind
	outcome.DispositionItemID = dispositionID
	outcome.DecisionStartedNS = result.disposition.DecisionStartedNS
	outcome.DecisionFinishedNS = result.disposition.DecisionFinishedNS
	if err := runner.publishOutcome(ctx, dispositionID, outcome); err != nil {
		return err
	}
	return runner.finishEvaluation(ctx, dispositionID, results)
}

func (runner *intentDispositionProducerRunner) finishEvaluation(
	ctx context.Context, parent string, results chan<- intentDispositionEvaluation,
) error {
	if err := runner.startNext(ctx, results); err != nil {
		return err
	}
	return runner.publishState(ctx, parent)
}

func (runner *intentDispositionProducerRunner) decisionInput(
	ctx context.Context, probe IntentSettlementProbe,
) (coreinteraction.Decision, error) {
	snapshot := runner.store.Snapshot()
	if err := VerifyIntentSettlementProbe(snapshot, probe, runner.config.ExpectedSettlement); err != nil {
		return coreinteraction.Decision{}, err
	}
	intent, err := intentDispositionItem(snapshot, probe.DurableIntent.StoreVersion, probe.DurableIntent.TrajectoryItemID)
	if err != nil {
		return coreinteraction.Decision{}, fmt.Errorf("resolve durable intent: %w", err)
	}
	observation, err := intentDispositionItem(snapshot, probe.TriggerObservation.StoreVersion, probe.TriggerObservation.TrajectoryItemID)
	if err != nil {
		return coreinteraction.Decision{}, fmt.Errorf("resolve trigger observation: %w", err)
	}
	result, err := intentDispositionItem(snapshot, probe.Result.StoreVersion, probe.Result.TrajectoryItemID)
	if err != nil {
		return coreinteraction.Decision{}, fmt.Errorf("resolve successful result: %w", err)
	}
	if result.ToolResult == nil || observation.Observation == nil {
		return coreinteraction.Decision{}, errors.New("verified settlement evidence lost its result or observation metadata")
	}
	pair := TemporalEvidenceRequirement{
		Observer: observation.Observation.Observer, Source: observation.Observation.Source,
	}
	if !slices.Contains(runner.config.ExpectedSettlement.CandidateSources, pair) {
		return coreinteraction.Decision{}, errors.New("exact post-effect observation is not a configured settlement candidate")
	}
	if !utf8.ValidString(intent.Content) || !utf8.ValidString(observation.Content) {
		return coreinteraction.Decision{}, errors.New("settlement decision text is not valid UTF-8")
	}
	if len(intent.Content) > runner.config.MaxEvidenceBytes ||
		len(observation.Content) > runner.config.MaxEvidenceBytes ||
		len(result.ToolResult.Output) > runner.config.MaxEvidenceBytes {
		return coreinteraction.Decision{}, errors.New("settlement decision evidence exceeds the configured byte bound")
	}
	evidence := strings.Builder{}
	evidence.Grow(len(intent.Content) + len(observation.Content) + len(result.ToolResult.Output) + 256)
	fmt.Fprintf(&evidence, "Durable user intent:\n%s\n\nSuccessful effect:\nTool: %s\nResult: %s\n\nExact post-effect observation:\nObserver: %s\nSource: %s\nText: %s",
		intent.Content, probe.Result.Tool, result.ToolResult.Output,
		probe.TriggerObservation.Observer, probe.TriggerObservation.Source, observation.Content,
	)
	if evidence.Len() > runner.config.MaxEvidenceBytes {
		return coreinteraction.Decision{}, errors.New("rendered settlement decision evidence exceeds the configured byte bound")
	}
	decision := coreinteraction.Decision{
		Prompt:  intentDispositionInstruction,
		Options: slices.Clone(intentDispositionOptions), Evidence: evidence.String(),
	}
	if !runner.config.DirectVisualInput {
		return decision, nil
	}
	mediaRefs := observation.Observation.Media
	if len(mediaRefs) == 0 {
		return coreinteraction.Decision{}, errors.New("exact post-effect observation has no retained visual media")
	}
	if len(mediaRefs) > runner.config.MaxMediaItems {
		return coreinteraction.Decision{}, fmt.Errorf(
			"exact post-effect observation has %d media items, exceeding the configured bound %d",
			len(mediaRefs), runner.config.MaxMediaItems,
		)
	}
	total := 0
	for index, ref := range mediaRefs {
		if ref.Bytes > runner.config.MaxMediaBytes-total {
			return coreinteraction.Decision{}, errors.New("canonical retained visual media exceeds the configured byte bound")
		}
		media, resolveErr := resolveIntentDispositionMedia(
			ctx, runner.media, ref.Handle, runner.config.MaxMediaBytes-total,
		)
		if resolveErr != nil {
			return coreinteraction.Decision{}, fmt.Errorf("resolve exact retained media %d: %w", index, resolveErr)
		}
		if media.MIMEType != ref.MIMEType {
			return coreinteraction.Decision{}, fmt.Errorf("resolved media %d MIME type differs from its canonical reference", index)
		}
		mediaType, _, parseErr := mime.ParseMediaType(media.MIMEType)
		if parseErr != nil || !strings.HasPrefix(strings.ToLower(mediaType), "image/") {
			return coreinteraction.Decision{}, fmt.Errorf("resolved media %d is not a canonical image", index)
		}
		if ref.Bytes > 0 && len(media.Bytes) != ref.Bytes {
			return coreinteraction.Decision{}, fmt.Errorf("resolved media %d byte length differs from its canonical reference", index)
		}
		if len(media.Bytes) > runner.config.MaxMediaBytes-total {
			return coreinteraction.Decision{}, errors.New("resolved visual media exceeds the configured aggregate byte bound")
		}
		total += len(media.Bytes)
		decision.Images = append(decision.Images, coreinteraction.Image{
			MIMEType: media.MIMEType, Bytes: media.Bytes,
		})
	}
	return decision, nil
}

type intentDispositionMediaResult struct {
	media continuation.Media
	err   error
}

// MediaResolver predates context-aware calls. Isolating it behind the exact
// classification deadline prevents a stalled media bridge from withholding an
// explicit indeterminate result. Byte length is checked before cloning, so an
// untrusted resolver cannot make the producer duplicate an oversized payload.
func resolveIntentDispositionMedia(
	ctx context.Context, resolver continuation.MediaResolver, handle string, maximumBytes int,
) (continuation.Media, error) {
	if resolver == nil {
		return continuation.Media{}, errors.New("intent disposition media resolver is unavailable")
	}
	if err := context.Cause(ctx); err != nil {
		return continuation.Media{}, err
	}
	result := make(chan intentDispositionMediaResult, 1)
	go func() {
		media, err := resolver(handle)
		result <- intentDispositionMediaResult{media: media, err: err}
	}()
	select {
	case resolved := <-result:
		if resolved.err != nil {
			return continuation.Media{}, resolved.err
		}
		if maximumBytes < 0 || len(resolved.media.Bytes) > maximumBytes {
			return continuation.Media{}, errors.New("resolved visual media exceeds the configured aggregate byte bound")
		}
		resolved.media.Bytes = slices.Clone(resolved.media.Bytes)
		return resolved.media, nil
	case <-ctx.Done():
		return continuation.Media{}, context.Cause(ctx)
	}
}

func intentDispositionItem(
	snapshot trajectory.Snapshot, version uint64, itemID string,
) (trajectory.Item, error) {
	if version == 0 || version > snapshot.Version || version > uint64(len(snapshot.Items)) {
		return trajectory.Item{}, errors.New("canonical item position is outside the snapshot")
	}
	item := snapshot.Items[version-1]
	if item.ID != itemID {
		return trajectory.Item{}, errors.New("canonical item identity differs at its exact position")
	}
	return item, nil
}

func validateIntentDispositionOutcome(
	outcome coreinteraction.Outcome,
) (IntentDispositionKind, error) {
	if outcome.Index < 0 || outcome.Index >= len(intentDispositionOptions) {
		return IntentDispositionIndeterminate, errors.New("semantic decider returned an out-of-range disposition index")
	}
	if outcome.Option != intentDispositionOptions[outcome.Index] {
		return IntentDispositionIndeterminate, errors.New("semantic decider disposition option and index disagree")
	}
	if outcome.Measured && (math.IsNaN(outcome.Confidence) || math.IsInf(outcome.Confidence, 0) ||
		outcome.Confidence < 0 || outcome.Confidence > 1) {
		return IntentDispositionIndeterminate, errors.New("semantic decider returned invalid measured confidence")
	}
	return IntentDispositionKind(outcome.Option), nil
}

type intentDispositionDeciderResult struct {
	outcome coreinteraction.Outcome
	err     error
}

// A provider is required to honor ctx, but the element enforces the declared
// deadline at its own boundary as well. The buffered reply lets a late client
// return without blocking; the actor marks that client unavailable so no
// second call can overlap a deadline-violating first call.
func decideIntentDisposition(
	ctx context.Context, decider SemanticDecider, decision coreinteraction.Decision,
) (coreinteraction.Outcome, error) {
	if semanticReflectedNil(decider) {
		return coreinteraction.Outcome{}, errors.New("intent disposition semantic decider is nil")
	}
	if err := context.Cause(ctx); err != nil {
		return coreinteraction.Outcome{}, err
	}
	result := make(chan intentDispositionDeciderResult, 1)
	go func() {
		outcome, err := decider.Decide(ctx, decision)
		result <- intentDispositionDeciderResult{outcome: outcome, err: err}
	}()
	select {
	case decided := <-result:
		return decided.outcome, decided.err
	case <-ctx.Done():
		return coreinteraction.Outcome{}, context.Cause(ctx)
	}
}

func validateIntentDispositionProbeEnvelope(
	envelope element.Envelope, probe IntentSettlementProbe, sessionID string,
) error {
	if err := validatePolicyIdentifier("intent disposition probe envelope item ID", envelope.ItemID, true); err != nil {
		return err
	}
	if err := validatePolicyIdentifier("intent disposition probe envelope session ID", envelope.SessionID, true); err != nil {
		return err
	}
	if envelope.ItemID != probe.ProbeID {
		return errors.New("intent disposition probe envelope does not carry the exact probe ID")
	}
	if envelope.SessionID != sessionID || probe.SessionID != sessionID {
		return errors.New("intent disposition probe crossed the mounted trajectory session")
	}
	if envelope.Sequence != probe.Sequence {
		return errors.New("intent disposition probe envelope and payload sequences differ")
	}
	return validateIntentDispositionParents(envelope.ItemID, envelope.CausalParents)
}

func validateIntentDispositionCancellationEnvelope(
	envelope element.Envelope, cancellation IntentSettlementCancellation, sessionID string,
) error {
	if err := validatePolicyIdentifier("intent disposition cancellation envelope item ID", envelope.ItemID, true); err != nil {
		return err
	}
	if err := validatePolicyIdentifier("intent disposition cancellation envelope session ID", envelope.SessionID, true); err != nil {
		return err
	}
	if envelope.SessionID != sessionID || cancellation.SessionID != sessionID {
		return errors.New("intent disposition cancellation crossed the mounted trajectory session")
	}
	if envelope.CancellationScope != cancellation.DurableIntent.TrajectoryItemID {
		return errors.New("intent disposition cancellation does not carry its exact durable-intent scope")
	}
	return validateIntentDispositionParents(envelope.ItemID, envelope.CausalParents)
}

func validateIntentDispositionParents(itemID string, parents []string) error {
	if len(parents) > maximumIntentSettlementCausalParents {
		return fmt.Errorf("intent disposition input has more than %d causal parents", maximumIntentSettlementCausalParents)
	}
	seen := make(map[string]struct{}, len(parents))
	for _, parent := range parents {
		if err := validatePolicyIdentifier("intent disposition causal parent", parent, true); err != nil {
			return err
		}
		if parent == itemID {
			return errors.New("intent disposition input is self-causal")
		}
		if _, duplicate := seen[parent]; duplicate {
			return errors.New("intent disposition input repeats a causal parent")
		}
		seen[parent] = struct{}{}
	}
	return nil
}

func (runner *intentDispositionProducerRunner) publishDisposition(
	ctx context.Context, disposition IntentDisposition,
) (string, uint64, error) {
	sequence, err := runner.sequences.Next(runner.instance + ".intent_disposition")
	if err != nil {
		return "", 0, err
	}
	itemID, err := intentDispositionProducerItemID("disposition", runner.instance, sequence, disposition)
	if err != nil {
		return "", 0, err
	}
	envelope := element.Envelope{
		Type: runner.ports.disposition.Type(), ItemID: itemID,
		SessionID: disposition.Probe.SessionID, SourceID: disposition.Detector.Reference,
		RunID:             disposition.Probe.Result.InvocationID,
		CancellationScope: disposition.Probe.DurableIntent.TrajectoryItemID,
		Sequence:          sequence, CausalParents: []string{disposition.Probe.ProbeID},
		Payload: cloneIntentDisposition(disposition),
	}
	delivery, err := runner.ports.disposition.Broadcast(ctx, envelope)
	if err != nil {
		return "", 0, err
	}
	if delivery.Dropped != 0 || delivery.Delivered != len(runner.ports.disposition.Lanes()) {
		return "", 0, fmt.Errorf("intent disposition publication delivered %d and dropped %d of %d lanes",
			delivery.Delivered, delivery.Dropped, len(runner.ports.disposition.Lanes()))
	}
	return itemID, sequence, nil
}

func (runner *intentDispositionProducerRunner) publishOutcome(
	ctx context.Context, parent string, outcome IntentDispositionProducerOutcome,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".intent_disposition_outcome")
	if err != nil {
		return err
	}
	outcome.Message = boundedPolicyReason(outcome.Message)
	itemID, err := intentDispositionProducerItemID("outcome", runner.instance, sequence, outcome)
	if err != nil {
		return err
	}
	envelope := element.Envelope{
		Type: runner.ports.outcome.Type(), ItemID: itemID, SessionID: runner.sessionID,
		SourceID: runner.config.ExpectedSettlement.Detector.Reference,
		Sequence: sequence, Payload: outcome,
	}
	if validIntentDispositionParent(parent, itemID) {
		envelope.CausalParents = []string{parent}
	}
	_, err = runner.ports.outcome.Broadcast(ctx, envelope)
	return err
}

func (runner *intentDispositionProducerRunner) publishState(ctx context.Context, parent string) error {
	sequence, err := runner.sequences.Next(runner.instance + ".intent_disposition_state")
	if err != nil {
		return err
	}
	runner.state.Revision++
	runner.state.Pending = len(runner.pending)
	runner.state.Active = runner.active != nil
	runner.state.DeciderUnavailable = runner.deciderUnavailable
	runner.state.TerminalEntries = len(runner.terminal)
	runner.state.CancellationEntries = len(runner.cancellations)
	state := runner.state
	itemID, err := intentDispositionProducerItemID("state", runner.instance, sequence, state)
	if err != nil {
		return err
	}
	envelope := element.Envelope{
		Type: runner.ports.state.Type(), ItemID: itemID, SessionID: runner.sessionID,
		SourceID: runner.config.ExpectedSettlement.Detector.Reference,
		Sequence: sequence, Payload: state,
	}
	if validIntentDispositionParent(parent, itemID) {
		envelope.CausalParents = []string{parent}
	}
	_, err = runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *intentDispositionProducerRunner) publishResolved(ctx context.Context) error {
	digest, err := semanticDescriptorDigest(runner.entry.descriptor)
	if err != nil {
		return err
	}
	sequence, err := runner.sequences.Next(runner.instance + ".intent_disposition_resolution")
	if err != nil {
		return err
	}
	resolution := IntentDispositionProducerResolution{
		Detector:   runner.config.ExpectedSettlement.Detector,
		Descriptor: runner.entry.descriptor, DescriptorDigest: digest,
		RegistryRevision:  runner.registryRevision,
		DirectVisualInput: runner.config.DirectVisualInput,
	}
	itemID, err := intentDispositionProducerItemID("resolved", runner.instance, sequence, resolution)
	if err != nil {
		return err
	}
	_, err = runner.ports.resolved.Broadcast(ctx, element.Envelope{
		Type: runner.ports.resolved.Type(), ItemID: itemID, SessionID: runner.sessionID,
		SourceID: runner.config.ExpectedSettlement.Detector.Reference,
		Sequence: sequence, Payload: resolution,
	})
	return err
}

func (runner *intentDispositionProducerRunner) reportResolution() error {
	digest, err := semanticDescriptorDigest(runner.entry.descriptor)
	if err != nil {
		return err
	}
	provider := liveidentity.Artifact{
		ID:       "interaction-model://" + runner.entry.descriptor.Provider,
		Revision: runner.entry.descriptor.Model + "@" + runner.entry.descriptor.Revision,
		Digest:   digest,
	}
	adapter := liveidentity.Artifact{
		ID:       "builtin://openrealtime/adapters/policy.IntentDispositionProducer-interaction.Decider",
		Revision: intentDispositionProducerRuntimeRevision,
	}
	return liveidentity.Report(runner.resolution, liveidentity.Artifact{
		ID: intentDispositionProducerRuntimeID, Revision: intentDispositionProducerRuntimeRevision,
	}, []element.CapabilityResolution{liveidentity.Capability(
		"policy.intent-disposition", "openrealtime.policy/IntentDisposition-v1", provider, adapter,
	)})
}

func (runner *intentDispositionProducerRunner) hasProbe(probeID string) bool {
	if runner.active != nil && runner.active.request.probe.ProbeID == probeID {
		return true
	}
	for _, request := range runner.pending {
		if request.probe.ProbeID == probeID {
			return true
		}
	}
	return false
}

func (runner *intentDispositionProducerRunner) rememberTerminal(probeID string) {
	if _, found := runner.terminal[probeID]; found {
		return
	}
	runner.terminal[probeID] = struct{}{}
	runner.terminalOrder = append(runner.terminalOrder, probeID)
	for len(runner.terminalOrder) > runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		runner.terminalOrder = runner.terminalOrder[1:]
		delete(runner.terminal, oldest)
	}
}

func (runner *intentDispositionProducerRunner) rememberCancellation(
	address intentDispositionCancellationAddress,
) {
	runner.cancellations[address] = struct{}{}
	runner.cancelOrder = append(runner.cancelOrder, address)
	for len(runner.cancelOrder) > runner.config.CancelMemory {
		oldest := runner.cancelOrder[0]
		runner.cancelOrder = runner.cancelOrder[1:]
		delete(runner.cancellations, oldest)
	}
}

func producerOutcomeForProbe(
	probe IntentSettlementProbe, kind IntentDispositionProducerOutcomeKind, code, message string,
) IntentDispositionProducerOutcome {
	return IntentDispositionProducerOutcome{
		Kind: kind, ProbeID: probe.ProbeID,
		DurableIntentItemID: probe.DurableIntent.TrajectoryItemID,
		Code:                code, Message: boundedPolicyReason(message),
	}
}

func intentDispositionProducerProbePayload(payload any) (IntentSettlementProbe, bool) {
	switch value := payload.(type) {
	case IntentSettlementProbe:
		return cloneIntentSettlementProbe(value), true
	case *IntentSettlementProbe:
		if value != nil {
			return cloneIntentSettlementProbe(*value), true
		}
	}
	return IntentSettlementProbe{}, false
}

func validIntentDispositionParent(parent, itemID string) bool {
	return parent != "" && parent != itemID && validatePolicyIdentifier(
		"intent disposition output parent", parent, true,
	) == nil
}

func intentDispositionProducerItemID(
	namespace, instance string, sequence uint64, payload any,
) (string, error) {
	canonical, err := json.Marshal(struct {
		Namespace string `json:"namespace"`
		Instance  string `json:"instance"`
		Sequence  uint64 `json:"sequence"`
		Payload   any    `json:"payload"`
	}{Namespace: namespace, Instance: instance, Sequence: sequence, Payload: payload})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(canonical)
	return "intent-disposition-" + namespace + ":sha256:" + hex.EncodeToString(digest[:]), nil
}
