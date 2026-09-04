package policy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	internalclock "github.com/bojieli/OpenRealtime/internal/clock"
)

func (intentDispositionRetryFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	if err := validatePolicyIdentifier("intent disposition retry instance ID", mount.InstanceID, true); err != nil {
		return nil, fmt.Errorf("policy.IntentDispositionRetry instance ID: %w", err)
	}
	config, err := decodeIntentDispositionRetryConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("policy.IntentDispositionRetry %s config: %w", mount.InstanceID, err)
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("intent disposition retry has no runtime clock service")
	}
	runtimeClock, ok := clockValue.(graphruntime.Clock)
	if !ok || reflectedIntentDispositionRetryNil(runtimeClock) {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("intent disposition retry has no runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	scheduler := internalclock.Scheduler(internalclock.NewSystem())
	if value, _, available := mount.Services.Lookup(IntentDispositionRetrySchedulerService); available {
		scheduler, ok = value.(internalclock.Scheduler)
		if !ok || reflectedIntentDispositionRetryNil(scheduler) {
			return nil, fmt.Errorf("intent disposition retry scheduler service has type %T", value)
		}
	}
	ports, err := intentDispositionRetryPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &intentDispositionRetryRunner{
		instance: mount.InstanceID, config: config, clock: runtimeClock,
		sequences: sequences, scheduler: scheduler, ports: ports, resolution: mount.Resolution,
		records:       make(map[string]*intentDispositionRetryRecord),
		terminal:      make(map[string]struct{}),
		cancellations: make(map[intentDispositionRetryAddress]TemporalEvidenceItemIdentity),
		state: IntentDispositionRetryState{
			MaxPending: config.MaxPending, MaxRetries: config.MaxRetries,
		},
	}, nil
}

func reflectedIntentDispositionRetryNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type intentDispositionRetryPorts struct {
	probe, disposition, cancel, reset        element.InputPort
	attempt, forwardedCancel, forwardedReset element.OutputPort
	exhausted, state, outcome                element.OutputPort
}

func intentDispositionRetryPortsFrom(ports element.Ports) (intentDispositionRetryPorts, error) {
	if ports == nil {
		return intentDispositionRetryPorts{}, errors.New("policy.IntentDispositionRetry has nil ports")
	}
	var result intentDispositionRetryPorts
	for _, entry := range []struct {
		name string
		port *element.InputPort
	}{
		{"probe", &result.probe}, {"disposition", &result.disposition},
		{"cancel", &result.cancel}, {"reset", &result.reset},
	} {
		port, err := ports.Input(entry.name)
		if err != nil {
			return intentDispositionRetryPorts{}, err
		}
		*entry.port = port
	}
	for _, entry := range []struct {
		name string
		port *element.OutputPort
	}{
		{"attempt", &result.attempt}, {"forwarded_cancel", &result.forwardedCancel},
		{"forwarded_reset", &result.forwardedReset}, {"exhausted", &result.exhausted},
		{"state", &result.state}, {"outcome", &result.outcome},
	} {
		port, err := ports.Output(entry.name)
		if err != nil {
			return intentDispositionRetryPorts{}, err
		}
		*entry.port = port
	}
	return result, nil
}

type intentDispositionRetryAddress struct {
	session string
	intent  string
}

func intentDispositionRetryAddressFor(
	session string, intent TemporalEvidenceItemIdentity,
) intentDispositionRetryAddress {
	return intentDispositionRetryAddress{session: session, intent: intent.TrajectoryItemID}
}

type intentDispositionRetryRecord struct {
	probeEnvelope element.Envelope
	probe         IntentSettlementProbe
	firstSeenNS   uint64
	deadlineNS    uint64
	retries       int
	generation    uint64
	timer         internalclock.Timer
	armed         bool

	lastDisposition       *IntentDisposition
	lastDispositionItemID string
}

type intentDispositionRetryInput struct {
	kind     string
	envelope element.Envelope
}

type intentDispositionRetryFire struct {
	probeID    string
	generation uint64
}

type intentDispositionRetryRunner struct {
	instance   string
	config     IntentDispositionRetryConfig
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	scheduler  internalclock.Scheduler
	ports      intentDispositionRetryPorts
	resolution element.ResolutionReporter

	records map[string]*intentDispositionRetryRecord

	terminal      map[string]struct{}
	terminalOrder []string

	cancellations map[intentDispositionRetryAddress]TemporalEvidenceItemIdentity
	cancelOrder   []intentDispositionRetryAddress

	state IntentDispositionRetryState
}

func (runner *intentDispositionRetryRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := liveidentity.Report(runner.resolution, liveidentity.Artifact{
		ID: intentDispositionRetryRuntimeID, Revision: intentDispositionRetryRuntimeRevision,
	}, nil); err != nil {
		return err
	}
	startup := element.Envelope{ItemID: runner.instance + ":startup"}
	if err := runner.publishState(ctx, startup); err != nil {
		return err
	}

	inputs := make(chan intentDispositionRetryInput)
	fires := make(chan intentDispositionRetryFire)
	failures := make(chan error, 4)
	var receivers sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{
		{"probe", runner.ports.probe}, {"disposition", runner.ports.disposition},
		{"cancel", runner.ports.cancel}, {"reset", runner.ports.reset},
	} {
		receivers.Add(1)
		go receiveIntentDispositionRetryInput(
			ctx, source.kind, source.port, inputs, failures, &receivers,
		)
	}
	defer func() {
		for _, record := range runner.records {
			if record.timer != nil {
				record.timer.Stop()
			}
		}
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
		case fire := <-fires:
			if err := runner.acceptFire(ctx, fire); err != nil {
				cancel(err)
				return err
			}
		case input := <-inputs:
			var err error
			switch input.kind {
			case "probe":
				err = runner.acceptProbe(ctx, input.envelope)
			case "disposition":
				err = runner.acceptDisposition(ctx, input.envelope, fires)
			case "cancel":
				err = runner.acceptCancel(ctx, input.envelope)
			case "reset":
				err = runner.acceptReset(ctx, input.envelope)
			default:
				err = fmt.Errorf("intent disposition retry received unknown input %q", input.kind)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func receiveIntentDispositionRetryInput(
	ctx context.Context, kind string, port element.InputPort,
	inputs chan<- intentDispositionRetryInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := port.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, graphruntime.ErrChannelClosed) {
				select {
				case failures <- fmt.Errorf("receive intent disposition retry %s: %w", kind, err):
				case <-ctx.Done():
				}
			}
			return
		}
		select {
		case inputs <- intentDispositionRetryInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func (runner *intentDispositionRetryRunner) acceptProbe(
	ctx context.Context, envelope element.Envelope,
) error {
	probe, ok := intentDispositionProducerProbePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentSettlementProbeType()) {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "probe", "invalid_probe",
			fmt.Sprintf("settlement retry probe payload has type %T", envelope.Payload))
	}
	if err := preflightIntentSettlementProbe(probe); err != nil {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "probe", "invalid_probe", err.Error())
	}
	if err := validateIntentDispositionProbeEnvelope(envelope, probe, probe.SessionID); err != nil {
		return runner.refuse(ctx, envelope, probe, "probe", "invalid_probe", err.Error())
	}
	wantID, err := intentSettlementProbeID(probe)
	if err != nil || wantID != probe.ProbeID {
		if err == nil {
			err = errors.New("settlement retry probe ID does not match its canonical payload")
		}
		return runner.refuse(ctx, envelope, probe, "probe", "invalid_probe", err.Error())
	}
	if _, terminal := runner.terminal[probe.ProbeID]; terminal {
		return runner.ignore(ctx, envelope, probe, "probe", "terminal_probe",
			"the exact probe has already settled or exhausted its retry policy")
	}
	address := intentDispositionRetryAddressFor(probe.SessionID, probe.DurableIntent)
	if canceled, found := runner.cancellations[address]; found && canceled == probe.DurableIntent {
		return runner.ignore(ctx, envelope, probe, "probe", "intent_canceled",
			"the exact durable intent was canceled before this probe reached retry policy")
	}
	if current := runner.records[probe.ProbeID]; current != nil {
		if reflect.DeepEqual(current.probe, probe) &&
			intentDispositionRetryProbeEnvelopeEqual(current.probeEnvelope, envelope) {
			return runner.ignore(ctx, envelope, probe, "probe", "duplicate_probe",
				"the exact probe is already tracked")
		}
		return runner.refuse(ctx, envelope, probe, "probe", "conflicting_probe",
			"the probe ID was reused with different immutable content")
	}
	if len(runner.records) >= runner.config.MaxPending {
		return runner.refuse(ctx, envelope, probe, "probe", "capacity_exhausted",
			"retry policy is at its configured live-probe bound")
	}
	now := runner.clock.NowNS()
	elapsed := uint64(time.Duration(runner.config.MaxElapsedMS) * time.Millisecond)
	deadline := now + elapsed
	if deadline < now {
		return runner.refuse(ctx, envelope, probe, "probe", "deadline_overflow",
			"retry deadline overflowed the monotonic clock")
	}
	retained := envelope.Clone()
	retained.Payload = cloneIntentSettlementProbe(probe)
	runner.records[probe.ProbeID] = &intentDispositionRetryRecord{
		probeEnvelope: retained, probe: cloneIntentSettlementProbe(probe),
		firstSeenNS: now, deadlineNS: deadline,
	}
	runner.state.Observed++
	runner.bumpState()
	if err := runner.broadcastExact(ctx, runner.ports.attempt, retained, "initial probe attempt"); err != nil {
		return err
	}
	if err := runner.publishOutcome(ctx, envelope, IntentDispositionRetryOutcome{
		Kind: IntentDispositionRetryObserved, SessionID: probe.SessionID,
		ProbeID: probe.ProbeID, DurableIntentItemID: probe.DurableIntent.TrajectoryItemID,
		DeadlineNS: deadline, Code: "initial_attempt",
		Message: "the immutable settlement probe was forwarded for its initial classification",
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func intentDispositionRetryProbeEnvelopeEqual(left, right element.Envelope) bool {
	left.Payload = nil
	right.Payload = nil
	return reflect.DeepEqual(left, right)
}

func (runner *intentDispositionRetryRunner) acceptDisposition(
	ctx context.Context, envelope element.Envelope,
	fires chan<- intentDispositionRetryFire,
) error {
	disposition, ok := intentDispositionPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentDispositionType()) {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "disposition", "invalid_disposition",
			fmt.Sprintf("settlement retry disposition payload has type %T", envelope.Payload))
	}
	if err := preflightIntentDisposition(disposition); err != nil {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "disposition", "invalid_disposition", err.Error())
	}
	probe := disposition.Probe
	if err := validateIntentSettlementInputEnvelope("disposition", envelope); err != nil {
		return runner.refuse(ctx, envelope, probe, "disposition", "invalid_disposition", err.Error())
	}
	record := runner.records[probe.ProbeID]
	if record == nil {
		if _, terminal := runner.terminal[probe.ProbeID]; terminal {
			return runner.ignore(ctx, envelope, probe, "disposition", "terminal_disposition",
				"the exact probe is already terminal in retry policy")
		}
		return runner.refuse(ctx, envelope, probe, "disposition", "unknown_probe",
			"retry policy has not forwarded the exact probe")
	}
	if !reflect.DeepEqual(record.probe, probe) {
		return runner.refuse(ctx, envelope, probe, "disposition", "probe_mismatch",
			"disposition does not echo the exact tracked probe")
	}
	if err := validateIntentDisposition(disposition, record.probe.Detector); err != nil {
		return runner.refuse(ctx, envelope, probe, "disposition", "invalid_disposition", err.Error())
	}
	if envelope.SessionID != probe.SessionID ||
		!slices.Contains(envelope.CausalParents, probe.ProbeID) {
		return runner.refuse(ctx, envelope, probe, "disposition", "invalid_disposition",
			"disposition envelope lacks the exact session or probe lineage")
	}
	if record.lastDisposition != nil {
		if record.lastDispositionItemID == envelope.ItemID &&
			reflect.DeepEqual(*record.lastDisposition, disposition) {
			return runner.ignore(ctx, envelope, probe, "disposition", "duplicate_disposition",
				"the exact disposition was already applied")
		}
		if disposition.DecisionStartedNS < record.lastDisposition.DecisionFinishedNS ||
			disposition.DecisionFinishedNS <= record.lastDisposition.DecisionFinishedNS {
			return runner.refuse(ctx, envelope, probe, "disposition", "non_monotonic_disposition",
				"disposition timing does not follow the last applied classification")
		}
	}
	if disposition.Kind == IntentDispositionIndeterminate && (record.armed || record.timer != nil) {
		return runner.refuse(ctx, envelope, probe, "disposition", "retry_already_armed",
			"an indeterminate disposition arrived while the exact retry timer was already armed")
	}
	copy := cloneIntentDisposition(disposition)
	record.lastDisposition = &copy
	record.lastDispositionItemID = envelope.ItemID

	switch disposition.Kind {
	case IntentDispositionIndeterminate:
		runner.state.Indeterminate++
		return runner.schedule(ctx, envelope, record, fires)
	case IntentDispositionContinue, IntentDispositionSucceeded, IntentDispositionFailed:
		if record.timer != nil {
			record.timer.Stop()
		}
		delete(runner.records, probe.ProbeID)
		runner.rememberTerminal(probe.ProbeID)
		runner.state.Settled++
		runner.bumpState()
		if err := runner.publishOutcome(ctx, envelope, IntentDispositionRetryOutcome{
			Kind: IntentDispositionRetrySettled, SessionID: probe.SessionID,
			ProbeID: probe.ProbeID, DurableIntentItemID: probe.DurableIntent.TrajectoryItemID,
			Disposition: disposition.Kind, DispositionItemID: envelope.ItemID,
			Retry: record.retries, Code: "terminal_disposition",
			Message: "retry policy stopped after a determinate disposition",
		}); err != nil {
			return err
		}
		return runner.publishState(ctx, envelope)
	default:
		return runner.refuse(ctx, envelope, probe, "disposition", "unknown_disposition",
			fmt.Sprintf("unknown intent disposition %q", disposition.Kind))
	}
}

func (runner *intentDispositionRetryRunner) schedule(
	ctx context.Context, cause element.Envelope, record *intentDispositionRetryRecord,
	fires chan<- intentDispositionRetryFire,
) error {
	if record.retries >= runner.config.MaxRetries {
		return runner.exhaust(ctx, cause, record, "max_retries",
			"the exact probe reached its configured retry-attempt bound")
	}
	delayMS := runner.retryDelayMS(record.retries)
	now := runner.clock.NowNS()
	delayNS := uint64(time.Duration(delayMS) * time.Millisecond)
	if now >= record.deadlineNS || delayNS > record.deadlineNS-now {
		return runner.exhaust(ctx, cause, record, "retry_deadline",
			"the next retry would cross the configured elapsed-time bound")
	}
	record.generation++
	record.armed = true
	generation := record.generation
	probeID := record.probe.ProbeID
	record.timer = runner.scheduler.AfterFunc(time.Duration(delayMS)*time.Millisecond, func() {
		select {
		case fires <- intentDispositionRetryFire{probeID: probeID, generation: generation}:
		case <-ctx.Done():
		}
	})
	runner.state.Scheduled++
	runner.bumpState()
	if err := runner.publishOutcome(ctx, cause, IntentDispositionRetryOutcome{
		Kind: IntentDispositionRetryScheduled, SessionID: record.probe.SessionID,
		ProbeID: probeID, DurableIntentItemID: record.probe.DurableIntent.TrajectoryItemID,
		Disposition: IntentDispositionIndeterminate, DispositionItemID: cause.ItemID,
		Retry: record.retries + 1, DelayMS: delayMS, DeadlineNS: record.deadlineNS,
		Code:    "indeterminate",
		Message: "an exact probe replay was scheduled by explicit graph policy",
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *intentDispositionRetryRunner) retryDelayMS(retries int) int {
	delay := runner.config.InitialDelayMS
	for index := 0; index < retries; index++ {
		if delay >= runner.config.MaxDelayMS ||
			delay > runner.config.MaxDelayMS/runner.config.BackoffFactor {
			return runner.config.MaxDelayMS
		}
		delay *= runner.config.BackoffFactor
	}
	if delay > runner.config.MaxDelayMS {
		return runner.config.MaxDelayMS
	}
	return delay
}

func (runner *intentDispositionRetryRunner) acceptFire(
	ctx context.Context, fire intentDispositionRetryFire,
) error {
	record := runner.records[fire.probeID]
	if record == nil || !record.armed || fire.generation != record.generation {
		return nil
	}
	record.timer = nil
	record.armed = false
	now := runner.clock.NowNS()
	if now > record.deadlineNS {
		return runner.exhaust(ctx, record.probeEnvelope, record, "retry_deadline",
			"the scheduled retry fired after its configured elapsed-time bound")
	}
	record.retries++
	runner.state.Retried++
	runner.bumpState()
	attempt := record.probeEnvelope.Clone()
	attempt.Payload = cloneIntentSettlementProbe(record.probe)
	if err := runner.broadcastExact(ctx, runner.ports.attempt, attempt, "retry probe attempt"); err != nil {
		return err
	}
	cause := attempt.Clone()
	if record.lastDispositionItemID != "" {
		cause.CausalParents = appendUnique(cause.CausalParents, record.lastDispositionItemID)
	}
	if err := runner.publishOutcome(ctx, cause, IntentDispositionRetryOutcome{
		Kind: IntentDispositionRetryEmitted, SessionID: record.probe.SessionID,
		ProbeID:             record.probe.ProbeID,
		DurableIntentItemID: record.probe.DurableIntent.TrajectoryItemID,
		Disposition:         IntentDispositionIndeterminate,
		DispositionItemID:   record.lastDispositionItemID,
		Retry:               record.retries, DeadlineNS: record.deadlineNS,
		Code: "retry_emitted", Message: "the exact immutable probe was replayed",
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *intentDispositionRetryRunner) exhaust(
	ctx context.Context, cause element.Envelope, record *intentDispositionRetryRecord,
	code, message string,
) error {
	if record.timer != nil {
		record.timer.Stop()
	}
	record.timer = nil
	record.armed = false
	delete(runner.records, record.probe.ProbeID)
	runner.rememberTerminal(record.probe.ProbeID)
	runner.state.Exhausted++
	runner.bumpState()
	finished := runner.clock.NowNS()
	exhaustion := IntentDispositionRetryExhaustion{
		Probe: cloneIntentSettlementProbe(record.probe), Retries: record.retries,
		FirstObservedNS: record.firstSeenNS, FinishedNS: finished,
		LastDispositionItemID: record.lastDispositionItemID, Code: code,
	}
	if err := runner.publishExhausted(ctx, cause, exhaustion); err != nil {
		return err
	}
	if err := runner.publishOutcome(ctx, cause, IntentDispositionRetryOutcome{
		Kind: IntentDispositionRetryExhausted, SessionID: record.probe.SessionID,
		ProbeID:             record.probe.ProbeID,
		DurableIntentItemID: record.probe.DurableIntent.TrajectoryItemID,
		Disposition:         IntentDispositionIndeterminate,
		DispositionItemID:   record.lastDispositionItemID,
		Retry:               record.retries, DeadlineNS: record.deadlineNS,
		Code: code, Message: message,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *intentDispositionRetryRunner) acceptCancel(
	ctx context.Context, envelope element.Envelope,
) error {
	cancellation, ok := intentSettlementCancellationPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentSettlementCancelType()) {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "cancel", "invalid_cancel",
			fmt.Sprintf("settlement retry cancellation payload has type %T", envelope.Payload))
	}
	if err := preflightIntentSettlementCancellation(cancellation); err != nil {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "cancel", "invalid_cancel", err.Error())
	}
	if err := validateIntentDispositionCancellationEnvelope(
		envelope, cancellation, cancellation.SessionID,
	); err != nil {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "cancel", "invalid_cancel", err.Error())
	}
	address := intentDispositionRetryAddressFor(cancellation.SessionID, cancellation.DurableIntent)
	duplicate := false
	if retained, found := runner.cancellations[address]; found {
		if retained != cancellation.DurableIntent {
			return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "cancel", "conflicting_cancel",
				"the durable-intent ID was reused with a different cancellation identity")
		}
		duplicate = true
	} else {
		runner.rememberCancellation(address, cancellation.DurableIntent)
	}
	removed := runner.stopAddress(address, cancellation.DurableIntent)
	runner.state.Canceled++
	runner.bumpState()
	forwarded := envelope.Clone()
	forwarded.Payload = cancellation
	if err := runner.broadcastExact(
		ctx, runner.ports.forwardedCancel, forwarded, "forwarded settlement cancellation",
	); err != nil {
		return err
	}
	code := "canceled"
	message := "retry timers were stopped before forwarding exact cancellation"
	if duplicate {
		code = "duplicate_cancellation"
		message = "the exact cancellation was forwarded again after retry policy was already quiescent"
	}
	if err := runner.publishOutcome(ctx, envelope, IntentDispositionRetryOutcome{
		Kind: IntentDispositionRetryCanceled, SessionID: cancellation.SessionID,
		DurableIntentItemID: cancellation.DurableIntent.TrajectoryItemID,
		Retry:               removed, Code: code, Message: message,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *intentDispositionRetryRunner) acceptReset(
	ctx context.Context, envelope element.Envelope,
) error {
	reset, ok := intentSettlementAddressPayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(IntentSettlementResetType()) {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "reset", "invalid_reset",
			fmt.Sprintf("settlement retry reset payload has type %T", envelope.Payload))
	}
	if err := preflightIntentSettlementAddress("settlement retry reset", reset); err != nil {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "reset", "invalid_reset", err.Error())
	}
	if err := validateIntentSettlementInputEnvelope("reset", envelope); err != nil {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "reset", "invalid_reset", err.Error())
	}
	if envelope.SessionID == "" || envelope.SessionID != reset.SessionID {
		return runner.refuse(ctx, envelope, IntentSettlementProbe{}, "reset", "invalid_reset",
			"settlement retry reset requires one exact envelope and payload session")
	}
	address := intentDispositionRetryAddressFor(reset.SessionID, reset.DurableIntent)
	removed := runner.stopAddress(address, reset.DurableIntent)
	runner.state.Reset++
	runner.bumpState()
	forwarded := envelope.Clone()
	forwarded.Payload = reset
	if err := runner.broadcastExact(
		ctx, runner.ports.forwardedReset, forwarded, "forwarded settlement reset",
	); err != nil {
		return err
	}
	if err := runner.publishOutcome(ctx, envelope, IntentDispositionRetryOutcome{
		Kind: IntentDispositionRetryReset, SessionID: reset.SessionID,
		DurableIntentItemID: reset.DurableIntent.TrajectoryItemID,
		Retry:               removed, Code: "reset",
		Message: "retry timers were stopped before forwarding the exact reset",
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *intentDispositionRetryRunner) stopAddress(
	address intentDispositionRetryAddress, identity TemporalEvidenceItemIdentity,
) int {
	removed := 0
	for probeID, record := range runner.records {
		if intentDispositionRetryAddressFor(record.probe.SessionID, record.probe.DurableIntent) != address ||
			record.probe.DurableIntent != identity {
			continue
		}
		if record.timer != nil {
			record.timer.Stop()
		}
		delete(runner.records, probeID)
		runner.rememberTerminal(probeID)
		removed++
	}
	return removed
}

func (runner *intentDispositionRetryRunner) rememberTerminal(probeID string) {
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

func (runner *intentDispositionRetryRunner) rememberCancellation(
	address intentDispositionRetryAddress, identity TemporalEvidenceItemIdentity,
) {
	runner.cancellations[address] = identity
	runner.cancelOrder = append(runner.cancelOrder, address)
	for len(runner.cancelOrder) > runner.config.CancelMemory {
		oldest := runner.cancelOrder[0]
		runner.cancelOrder = runner.cancelOrder[1:]
		delete(runner.cancellations, oldest)
	}
}

func (runner *intentDispositionRetryRunner) bumpState() {
	runner.state.Revision++
	runner.updateStateCounts()
}

func (runner *intentDispositionRetryRunner) updateStateCounts() {
	runner.state.Pending = len(runner.records)
	runner.state.Armed = 0
	runner.state.EarliestDeadlineNS = 0
	for _, record := range runner.records {
		if !record.armed {
			continue
		}
		runner.state.Armed++
		if runner.state.EarliestDeadlineNS == 0 || record.deadlineNS < runner.state.EarliestDeadlineNS {
			runner.state.EarliestDeadlineNS = record.deadlineNS
		}
	}
	runner.state.TerminalEntries = len(runner.terminal)
	runner.state.CancellationEntries = len(runner.cancellations)
}

func (runner *intentDispositionRetryRunner) ignore(
	ctx context.Context, cause element.Envelope, probe IntentSettlementProbe,
	operation, code, message string,
) error {
	runner.state.Ignored++
	runner.bumpState()
	if err := runner.publishOutcome(ctx, cause, IntentDispositionRetryOutcome{
		Kind: IntentDispositionRetryIgnored, SessionID: probe.SessionID,
		ProbeID: probe.ProbeID, DurableIntentItemID: probe.DurableIntent.TrajectoryItemID,
		Code: code, Message: operation + ": " + message,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *intentDispositionRetryRunner) refuse(
	ctx context.Context, cause element.Envelope, probe IntentSettlementProbe,
	operation, code, message string,
) error {
	runner.state.Refused++
	runner.bumpState()
	projected := canonicalIntentSettlementRefusalCause(cause)
	if err := runner.publishOutcome(ctx, projected, IntentDispositionRetryOutcome{
		Kind:                IntentDispositionRetryRefused,
		SessionID:           safeIntentSettlementIdentifier(probe.SessionID),
		ProbeID:             safeIntentSettlementIdentifier(probe.ProbeID),
		DurableIntentItemID: safeIntentSettlementIdentifier(probe.DurableIntent.TrajectoryItemID),
		Code:                code, Message: operation + ": " + boundedPolicyReason(message),
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, projected)
}

func (runner *intentDispositionRetryRunner) publishExhausted(
	ctx context.Context, cause element.Envelope,
	exhaustion IntentDispositionRetryExhaustion,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".intent-disposition-retry-exhausted")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = runner.ports.exhausted.Type()
	envelope.ItemID = fmt.Sprintf("%s:intent_disposition_retry_exhausted:%d", runner.instance, sequence)
	envelope.SessionID = exhaustion.Probe.SessionID
	envelope.RunID = exhaustion.Probe.Result.InvocationID
	envelope.CancellationScope = exhaustion.Probe.DurableIntent.TrajectoryItemID
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.CausalParents = appendUnique(envelope.CausalParents, exhaustion.Probe.ProbeID)
	envelope.Payload = exhaustion
	return runner.broadcastExact(ctx, runner.ports.exhausted, envelope, "retry exhaustion")
}

func (runner *intentDispositionRetryRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome IntentDispositionRetryOutcome,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".intent-disposition-retry-outcome")
	if err != nil {
		return err
	}
	outcome.FinishedNS = runner.clock.NowNS()
	outcome.Message = boundedPolicyReason(outcome.Message)
	envelope := cause.Clone()
	envelope.Type = runner.ports.outcome.Type()
	envelope.ItemID = fmt.Sprintf("%s:intent_disposition_retry_outcome:%d", runner.instance, sequence)
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	return runner.broadcastExact(ctx, runner.ports.outcome, envelope, "retry outcome")
}

func (runner *intentDispositionRetryRunner) publishState(
	ctx context.Context, cause element.Envelope,
) error {
	runner.updateStateCounts()
	sequence, err := runner.sequences.Next(runner.instance + ".intent-disposition-retry-state")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = runner.ports.state.Type()
	envelope.ItemID = fmt.Sprintf("%s:intent_disposition_retry_state:%d", runner.instance, sequence)
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.Payload = runner.state
	_, err = runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *intentDispositionRetryRunner) broadcastExact(
	ctx context.Context, output element.OutputPort, envelope element.Envelope, label string,
) error {
	delivery, err := output.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered != 1 {
		return fmt.Errorf("intent disposition %s delivered to %d lanes", label, delivery.Delivered)
	}
	return nil
}
