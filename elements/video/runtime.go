package video

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

type adaptiveObservationFactory struct{}

var (
	_ element.Factory         = adaptiveObservationFactory{}
	_ element.ConfigValidator = adaptiveObservationFactory{}
)

func (adaptiveObservationFactory) Descriptor() element.Descriptor {
	return AdaptiveObservationDescriptor()
}

func (adaptiveObservationFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeAdaptiveObservationConfig(source)
	return err
}

func (adaptiveObservationFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeAdaptiveObservationConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("video.AdaptiveObservation %s config: %w", mount.InstanceID, err)
	}
	value, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("adaptive video policy has no runtime sequence service")
	}
	sequences, ok := value.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", value)
	}
	ports, err := policyPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &adaptiveObservationRunner{
		instance: mount.InstanceID, config: config, sequences: sequences,
		resolution: mount.Resolution, ports: ports, phase: PhaseIdle,
		terminated: make(map[string]terminalSource), terminalItems: make(map[string]struct{}),
	}, nil
}

type policyPorts struct {
	source, frames, references, tick, refresh, end, cancel  element.InputPort
	observe, observeReference, observerRefresh              element.OutputPort
	observerClose, observerCancel, state, decision, outcome element.OutputPort
}

func policyPortsFrom(ports element.Ports) (policyPorts, error) {
	var result policyPorts
	inputs := []struct {
		name string
		set  *element.InputPort
	}{
		{"source", &result.source}, {"frames", &result.frames},
		{"references", &result.references}, {"tick", &result.tick},
		{"refresh", &result.refresh}, {"end", &result.end}, {"cancel", &result.cancel},
	}
	for _, input := range inputs {
		port, err := ports.Input(input.name)
		if err != nil {
			return result, err
		}
		*input.set = port
	}
	outputs := []struct {
		name string
		set  *element.OutputPort
	}{
		{"observe", &result.observe}, {"observe_reference", &result.observeReference},
		{"observer_refresh", &result.observerRefresh}, {"observer_close", &result.observerClose},
		{"observer_cancel", &result.observerCancel}, {"state", &result.state},
		{"decision", &result.decision}, {"outcome", &result.outcome},
	}
	for _, output := range outputs {
		port, err := ports.Output(output.name)
		if err != nil {
			return result, err
		}
		*output.set = port
	}
	return result, nil
}

type latestKind string

const (
	latestInline    latestKind = "inline"
	latestReference latestKind = "reference"
)

type latestFrame struct {
	kind        latestKind
	itemID      string
	inline      InlineFrame
	reference   FrameReference
	sample      []byte
	fingerprint string
	index       uint64
	capturedNS  uint64
	change      float64
	observed    bool
}

type terminalSource struct {
	phase  PolicyPhase
	reason string
}

type adaptiveObservationRunner struct {
	instance   string
	config     AdaptiveObservationConfig
	sequences  *graphruntime.SequenceAllocator
	resolution element.ResolutionReporter
	ports      policyPorts

	phase             PolicyPhase
	active            bool
	boundSession      string
	sessionID         string
	streamID          string
	sourceKind        SourceKind
	sourceRevision    uint64
	highestRevision   uint64
	expectedMIMEType  string
	externalReference string
	sourceStartItemID string
	openedNS          uint64
	lastTickNS        uint64
	nextDueNS         uint64
	lastObservedNS    uint64
	lastObservedIndex uint64
	refreshArmed      bool
	refreshNS         uint64
	refreshItemID     string
	latest            *latestFrame
	lastIndex         uint64
	lastCapturedNS    uint64
	haveFrame         bool

	stateSequence    uint64
	acceptedFrames   uint64
	supersededFrames uint64
	rejectedFrames   uint64
	observations     uint64
	skippedTicks     uint64

	terminated      map[string]terminalSource
	terminatedOrder []string
	terminalItems   map[string]struct{}
	terminalOrder   []string
}

type policyInput struct {
	kind     string
	envelope element.Envelope
}

func (runner *adaptiveObservationRunner) Run(parent context.Context) error {
	if err := reportLiveResolution(runner.resolution, policyRuntimeID); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan policyInput)
	interrupts := make(chan element.Envelope)
	failures := make(chan error, 7)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{
		{"source", runner.ports.source}, {"frame", runner.ports.frames},
		{"reference", runner.ports.references}, {"tick", runner.ports.tick},
		{"refresh", runner.ports.refresh}, {"end", runner.ports.end},
	} {
		wait.Add(1)
		go receivePolicyInputs(ctx, source.kind, source.port, inputs, failures, &wait)
	}
	wait.Add(1)
	go receivePolicyInterrupts(ctx, runner.ports.cancel, interrupts, failures, &wait)
	defer func() {
		cancel(nil)
		wait.Wait()
		runner.latest = nil
	}()
	for {
		// Give an already-received interrupt priority without introducing a
		// hidden scheduler or allowing it to overtake an earlier graph item.
		select {
		case envelope := <-interrupts:
			if err := runner.handleCancel(ctx, envelope); err != nil {
				return err
			}
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case envelope := <-interrupts:
			if err := runner.handleCancel(ctx, envelope); err != nil {
				return err
			}
		case input := <-inputs:
			var err error
			switch input.kind {
			case "source":
				err = runner.handleSource(ctx, input.envelope)
			case "frame":
				err = runner.handleFrame(ctx, input.envelope)
			case "reference":
				err = runner.handleReference(ctx, input.envelope)
			case "tick":
				err = runner.handleTick(ctx, input.envelope)
			case "refresh":
				err = runner.handleRefresh(ctx, input.envelope)
			case "end":
				err = runner.handleEnd(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown adaptive video input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *adaptiveObservationRunner) handleSource(ctx context.Context, envelope element.Envelope) error {
	request, ok := sourceStartPayload(envelope.Payload)
	if !ok {
		return runner.finish(ctx, envelope, PolicyOutcome{Kind: OutcomeRefused, Operation: "source", Code: "invalid_payload", Message: fmt.Sprintf("source payload has type %T", envelope.Payload), Terminal: true}, true, true)
	}
	if runner.isDuplicate(envelope.ItemID) {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "source", request.StreamID, request.SourceRevision, 0, "duplicate_request", "source request item ID is already terminal"), true, false)
	}
	if err := runner.validateSourceEnvelope(envelope, request); err != nil {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "source", request.StreamID, request.SourceRevision, 0, "invalid_source", err.Error()), true, true)
	}
	if runner.active {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "source", request.StreamID, request.SourceRevision, 0, "source_active", fmt.Sprintf("stream %q revision %d is already active", runner.streamID, runner.sourceRevision)), true, true)
	}
	key := sourceKey(envelope.SessionID, request.Source, request.StreamID, request.SourceRevision)
	if terminal, found := runner.terminated[key]; found {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "source", request.StreamID, request.SourceRevision, 0, "source_terminal", fmt.Sprintf("source generation is already %s: %s", terminal.phase, terminal.reason)), true, true)
	}
	if runner.boundSession != "" && envelope.SessionID != runner.boundSession {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "source", request.StreamID, request.SourceRevision, 0, "cross_session", "policy instance is already bound to another session"), true, true)
	}
	if request.SourceRevision <= runner.highestRevision {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "source", request.StreamID, request.SourceRevision, 0, "stale_source_revision", fmt.Sprintf("source revision %d is not newer than %d", request.SourceRevision, runner.highestRevision)), true, true)
	}
	nextDue, err := runner.initialDue(request.OpenedNS)
	if err != nil {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "source", request.StreamID, request.SourceRevision, 0, "time_overflow", err.Error()), true, true)
	}
	runner.boundSession = envelope.SessionID
	runner.sessionID = envelope.SessionID
	runner.streamID = request.StreamID
	runner.sourceKind = request.Kind
	runner.sourceRevision = request.SourceRevision
	runner.highestRevision = request.SourceRevision
	runner.expectedMIMEType = request.ExpectedMIMEType
	runner.externalReference = request.ExternalReferenceURI
	runner.sourceStartItemID = envelope.ItemID
	runner.openedNS = request.OpenedNS
	runner.lastTickNS = 0
	runner.nextDueNS = nextDue
	runner.lastObservedNS = 0
	runner.lastObservedIndex = 0
	runner.refreshArmed = false
	runner.refreshNS = 0
	runner.refreshItemID = ""
	runner.latest = nil
	runner.lastIndex = 0
	runner.lastCapturedNS = 0
	runner.haveFrame = false
	runner.phase = PhaseWatching
	runner.active = true
	return runner.finish(ctx, envelope, runner.outcome(OutcomeSucceeded, "source", request.StreamID, request.SourceRevision, 0, "source_started", ""), true, true)
}

func (runner *adaptiveObservationRunner) handleFrame(ctx context.Context, envelope element.Envelope) error {
	request, ok := inlineFramePayload(envelope.Payload)
	if !ok {
		runner.rejectedFrames++
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "frame", "", 0, 0, "invalid_payload", fmt.Sprintf("frame payload has type %T", envelope.Payload)), true, true)
	}
	if runner.isDuplicate(envelope.ItemID) {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "frame", request.StreamID, request.SourceRevision, request.Frame.Index, "duplicate_request", "frame item ID is already terminal"), false, false)
	}
	if err := runner.validateFrameEnvelope(envelope, request); err != nil {
		runner.rejectedFrames++
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "frame", request.StreamID, request.SourceRevision, request.Frame.Index, "invalid_frame", err.Error()), true, true)
	}
	// Own the only retained raw payload. Envelope cloning is deliberately
	// metadata-only, so retaining the ingress slice here would let a producer
	// mutate a frame after admission and would weaken the bounded-state claim.
	request.Frame.Image = slices.Clone(request.Frame.Image)
	change := 1.0
	sample := sampleBytes(request.Frame.Image, runner.config.MaxChangeSamples)
	if runner.latest != nil && runner.latest.kind == latestInline {
		change = sampledChange(runner.latest.sample, sample)
	}
	superseded := runner.latest != nil && !runner.latest.observed
	if superseded {
		runner.supersededFrames++
	}
	runner.latest = &latestFrame{
		kind: latestInline, itemID: envelope.ItemID, inline: request,
		sample: sample, index: request.Frame.Index, capturedNS: request.Frame.CapturedNS,
		change: change,
	}
	runner.lastIndex = request.Frame.Index
	runner.lastCapturedNS = request.Frame.CapturedNS
	runner.haveFrame = true
	runner.acceptedFrames++
	code := "accepted"
	if superseded {
		code = "superseded_latest"
	}
	return runner.finish(ctx, envelope, runner.outcome(OutcomeSucceeded, "frame", request.StreamID, request.SourceRevision, request.Frame.Index, code, ""), true, true)
}

func (runner *adaptiveObservationRunner) handleReference(ctx context.Context, envelope element.Envelope) error {
	request, ok := frameReferencePayload(envelope.Payload)
	if !ok {
		runner.rejectedFrames++
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "reference", "", 0, 0, "invalid_payload", fmt.Sprintf("reference payload has type %T", envelope.Payload)), true, true)
	}
	if runner.isDuplicate(envelope.ItemID) {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "reference", request.StreamID, request.SourceRevision, request.Index, "duplicate_request", "reference item ID is already terminal"), false, false)
	}
	if err := runner.validateReferenceEnvelope(envelope, request); err != nil {
		runner.rejectedFrames++
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "reference", request.StreamID, request.SourceRevision, request.Index, "invalid_reference", err.Error()), true, true)
	}
	change := 1.0
	if runner.latest != nil && runner.latest.kind == latestReference && runner.latest.fingerprint == request.Fingerprint {
		change = 0
	}
	superseded := runner.latest != nil && !runner.latest.observed
	if superseded {
		runner.supersededFrames++
	}
	runner.latest = &latestFrame{
		kind: latestReference, itemID: envelope.ItemID, reference: request,
		fingerprint: request.Fingerprint, index: request.Index,
		capturedNS: request.CapturedNS, change: change,
	}
	runner.lastIndex = request.Index
	runner.lastCapturedNS = request.CapturedNS
	runner.haveFrame = true
	runner.acceptedFrames++
	code := "accepted"
	if superseded {
		code = "superseded_latest"
	}
	return runner.finish(ctx, envelope, runner.outcome(OutcomeSucceeded, "reference", request.StreamID, request.SourceRevision, request.Index, code, ""), true, true)
}

func (runner *adaptiveObservationRunner) handleTick(ctx context.Context, envelope element.Envelope) error {
	tick, ok := timingTickPayload(envelope.Payload)
	if !ok {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "tick", "", 0, 0, "invalid_payload", fmt.Sprintf("tick payload has type %T", envelope.Payload)), true, true)
	}
	if runner.isDuplicate(envelope.ItemID) {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "tick", tick.StreamID, tick.SourceRevision, 0, "duplicate_request", "tick item ID is already terminal"), false, false)
	}
	if err := runner.validateActiveScope(envelope, tick.Source, tick.StreamID, tick.SourceRevision); err != nil {
		runner.skippedTicks++
		if decisionErr := runner.publishDecision(ctx, envelope, runner.skipDecision(tick.NowNS, "invalid_scope")); decisionErr != nil {
			return decisionErr
		}
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "tick", tick.StreamID, tick.SourceRevision, 0, "invalid_scope", err.Error()), true, true)
	}
	if tick.NowNS < runner.openedNS || (runner.lastTickNS != 0 && tick.NowNS <= runner.lastTickNS) {
		runner.skippedTicks++
		if err := runner.publishDecision(ctx, envelope, runner.skipDecision(tick.NowNS, "stale_tick")); err != nil {
			return err
		}
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "tick", tick.StreamID, tick.SourceRevision, 0, "stale_tick", "tick time is not newer than the current policy clock"), true, true)
	}
	runner.lastTickNS = tick.NowNS
	observe, reason, forced := runner.decide(tick.NowNS)
	if !observe {
		runner.skippedTicks++
		if err := runner.publishDecision(ctx, envelope, runner.skipDecision(tick.NowNS, reason)); err != nil {
			return err
		}
		return runner.finish(ctx, envelope, runner.outcome(OutcomeSucceeded, "tick", tick.StreamID, tick.SourceRevision, 0, reason, ""), true, true)
	}
	if err := runner.publishLatest(ctx, envelope, tick.NowNS, reason, forced); err != nil {
		return err
	}
	return runner.finish(ctx, envelope, runner.outcome(OutcomeSucceeded, "tick", tick.StreamID, tick.SourceRevision, runner.latest.index, "observation_emitted", reason), true, true)
}

func (runner *adaptiveObservationRunner) handleRefresh(ctx context.Context, envelope element.Envelope) error {
	request, ok := refreshPayload(envelope.Payload)
	if !ok {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "refresh", "", 0, 0, "invalid_payload", fmt.Sprintf("refresh payload has type %T", envelope.Payload)), true, true)
	}
	if runner.isDuplicate(envelope.ItemID) {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "refresh", request.StreamID, request.SourceRevision, 0, "duplicate_request", "refresh item ID is already terminal"), false, false)
	}
	if err := runner.validateActiveScope(envelope, request.Source, request.StreamID, request.SourceRevision); err != nil {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "refresh", request.StreamID, request.SourceRevision, 0, "invalid_scope", err.Error()), true, true)
	}
	if len(request.Reason) > runner.config.MaxMetadataBytes {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "refresh", request.StreamID, request.SourceRevision, 0, "metadata_too_large", "refresh reason exceeds max_metadata_bytes"), true, true)
	}
	if request.RequestedNS < runner.openedNS || (runner.lastTickNS != 0 && request.RequestedNS < runner.lastTickNS) {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "refresh", request.StreamID, request.SourceRevision, 0, "stale_refresh", "refresh time predates current source/tick state"), true, true)
	}
	runner.refreshArmed = true
	runner.refreshNS = request.RequestedNS
	runner.refreshItemID = envelope.ItemID
	if err := runner.publishVisualRefresh(ctx, envelope, request); err != nil {
		return err
	}
	return runner.finish(ctx, envelope, runner.outcome(OutcomeSucceeded, "refresh", request.StreamID, request.SourceRevision, 0, "refresh_armed", "next causally later tick will observe the latest frame"), true, true)
}

func (runner *adaptiveObservationRunner) handleEnd(ctx context.Context, envelope element.Envelope) error {
	request, ok := sourceEndPayload(envelope.Payload)
	if !ok {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "end", "", 0, 0, "invalid_payload", fmt.Sprintf("end payload has type %T", envelope.Payload)), true, true)
	}
	if runner.isDuplicate(envelope.ItemID) {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "end", request.StreamID, request.SourceRevision, 0, "duplicate_request", "end item ID is already terminal"), false, false)
	}
	if err := runner.validateActiveScope(envelope, request.Source, request.StreamID, request.SourceRevision); err != nil {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "end", request.StreamID, request.SourceRevision, 0, "invalid_scope", err.Error()), true, true)
	}
	if len(request.Reason) > runner.config.MaxMetadataBytes {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "end", request.StreamID, request.SourceRevision, 0, "metadata_too_large", "end reason exceeds max_metadata_bytes"), true, true)
	}
	if request.EndedNS < runner.openedNS || request.EndedNS < runner.lastCapturedNS || request.EndedNS < runner.lastTickNS {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "end", request.StreamID, request.SourceRevision, 0, "stale_end", "end time predates accepted source evidence"), true, true)
	}
	if err := runner.publishVisualClose(ctx, envelope, request); err != nil {
		return err
	}
	runner.recordTerminated(sourceKey(runner.sessionID, runner.config.Source, runner.streamID, runner.sourceRevision), PhaseEnded, request.Reason)
	runner.phase = PhaseEnded
	runner.active = false
	runner.latest = nil
	runner.refreshArmed = false
	return runner.finish(ctx, envelope, runner.outcome(OutcomeSucceeded, "end", request.StreamID, request.SourceRevision, 0, "source_ended", request.Reason), true, true)
}

func (runner *adaptiveObservationRunner) handleCancel(ctx context.Context, envelope element.Envelope) error {
	request, ok := cancelPayload(envelope.Payload)
	if !ok {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "cancel", "", 0, 0, "invalid_payload", fmt.Sprintf("cancel payload has type %T", envelope.Payload)), true, true)
	}
	if runner.isDuplicate(envelope.ItemID) {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "cancel", request.StreamID, request.SourceRevision, 0, "duplicate_request", "cancel item ID is already terminal"), false, false)
	}
	if err := validateControlIdentity(envelope, runner.config.Source, request.Source, request.StreamID, request.SourceRevision); err != nil {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "cancel", request.StreamID, request.SourceRevision, 0, "invalid_scope", err.Error()), true, true)
	}
	if request.CanceledNS == 0 {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "cancel", request.StreamID, request.SourceRevision, 0, "invalid_cancel_time", "canceled_ns must be positive"), true, true)
	}
	if len(request.Reason) > runner.config.MaxMetadataBytes {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "cancel", request.StreamID, request.SourceRevision, 0, "metadata_too_large", "cancel reason exceeds max_metadata_bytes"), true, true)
	}
	if runner.boundSession != "" && envelope.SessionID != runner.boundSession {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "cancel", request.StreamID, request.SourceRevision, 0, "cross_session", "cancel crossed the policy session boundary"), true, true)
	}
	key := sourceKey(envelope.SessionID, request.Source, request.StreamID, request.SourceRevision)
	if terminal, found := runner.terminated[key]; found {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeIgnored, "cancel", request.StreamID, request.SourceRevision, 0, "already_terminal", string(terminal.phase)), true, true)
	}
	if !runner.active {
		runner.recordTerminated(key, PhaseCanceled, request.Reason)
		return runner.finish(ctx, envelope, runner.outcome(OutcomeSucceeded, "cancel", request.StreamID, request.SourceRevision, 0, "cancel_recorded", request.Reason), true, true)
	}
	if err := runner.validateActiveScope(envelope, request.Source, request.StreamID, request.SourceRevision); err != nil {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "cancel", request.StreamID, request.SourceRevision, 0, "invalid_scope", err.Error()), true, true)
	}
	if request.CanceledNS < runner.openedNS || request.CanceledNS < runner.lastCapturedNS || request.CanceledNS < runner.lastTickNS {
		return runner.finish(ctx, envelope, runner.outcome(OutcomeRefused, "cancel", request.StreamID, request.SourceRevision, 0, "stale_cancel", "cancel time predates accepted source timing"), true, true)
	}
	if err := runner.publishVisualCancel(ctx, envelope, request); err != nil {
		return err
	}
	runner.recordTerminated(key, PhaseCanceled, request.Reason)
	runner.phase = PhaseCanceled
	runner.active = false
	runner.latest = nil
	runner.refreshArmed = false
	return runner.finish(ctx, envelope, runner.outcome(OutcomeCanceled, "cancel", request.StreamID, request.SourceRevision, 0, "source_canceled", request.Reason), true, true)
}

func (runner *adaptiveObservationRunner) decide(now uint64) (bool, string, bool) {
	if runner.latest == nil {
		return false, "no_frame", false
	}
	if runner.refreshArmed {
		if now <= runner.refreshNS {
			return false, "refresh_not_due", false
		}
		return true, "refresh", true
	}
	switch runner.config.Mode {
	case CadenceManual:
		return false, "manual_wait", false
	case CadenceFixed:
		if now < runner.nextDueNS {
			return false, "not_due", false
		}
		if runner.latest.observed {
			runner.nextDueNS = advanceFixedDue(runner.nextDueNS, now, runner.fixedIntervalNS())
			return false, "no_new_frame", false
		}
		return true, "fixed_due", false
	case CadenceAdaptive:
		anchor := runner.lastObservedNS
		if anchor == 0 {
			anchor = runner.openedNS
		}
		elapsed := now - anchor
		if elapsed >= runner.maxIntervalNS() {
			return true, "max_interval", false
		}
		if !runner.latest.observed && runner.latest.change >= runner.config.ChangeThreshold && elapsed >= runner.minIntervalNS() {
			return true, "changed", false
		}
		if runner.latest.observed {
			return false, "no_new_frame", false
		}
		if runner.latest.change < runner.config.ChangeThreshold {
			return false, "unchanged", false
		}
		return false, "min_interval", false
	default:
		return false, "invalid_mode", false
	}
}

func (runner *adaptiveObservationRunner) publishLatest(ctx context.Context, trigger element.Envelope, now uint64, reason string, forced bool) error {
	if runner.latest == nil {
		return errors.New("publish latest video frame: no frame is retained")
	}
	itemID, err := runner.nextItemID(trigger.ItemID, "observe")
	if err != nil {
		return err
	}
	envelope := trigger.Clone()
	envelope.ItemID = itemID
	envelope.SessionID = runner.sessionID
	envelope.SourceID = runner.config.Source
	envelope.CancellationScope = runner.streamID
	envelope.CaptureNS = runner.latest.capturedNS
	envelope.CausalParents = uniqueParents(
		runner.sourceStartItemID, runner.latest.itemID,
		trigger.ItemID, runner.refreshItemID,
	)
	if runner.latest.kind == latestInline {
		envelope.Type = imageBatchType
		frame := runner.latest.inline.Frame
		frame.Image = slices.Clone(frame.Image)
		envelope.Payload = perceptionelements.ImageBatch{
			StreamID: runner.streamID, Frames: []coreperception.Frame{frame},
		}
		if _, err := runner.ports.observe.Broadcast(ctx, envelope); err != nil {
			return err
		}
	} else {
		envelope.Type = referenceBatchType
		envelope.Payload = ReferenceBatch{
			StreamID: runner.streamID, References: []FrameReference{runner.latest.reference},
		}
		if _, err := runner.ports.observeReference.Broadcast(ctx, envelope); err != nil {
			return err
		}
	}
	runner.latest.observed = true
	runner.lastObservedIndex = runner.latest.index
	runner.lastObservedNS = now
	runner.observations++
	runner.refreshArmed = false
	runner.refreshNS = 0
	runner.refreshItemID = ""
	if runner.config.Mode == CadenceFixed {
		runner.nextDueNS = advanceFixedDue(runner.nextDueNS, now, runner.fixedIntervalNS())
	} else if runner.config.Mode == CadenceAdaptive {
		runner.nextDueNS = saturatingAddNS(now, runner.minIntervalNS())
	}
	return runner.publishDecision(ctx, trigger, ObservationDecision{
		Kind: DecisionObserve, Mode: runner.config.Mode, Reason: reason,
		Source: runner.config.Source, StreamID: runner.streamID,
		SourceRevision: runner.sourceRevision, FrameKind: string(runner.latest.kind),
		FrameIndex: runner.latest.index, FrameItemID: runner.latest.itemID,
		TriggerItemID: trigger.ItemID, AtNS: now, ChangeScore: runner.latest.change,
		NextDueNS: runner.nextDueNS, Forced: forced,
	})
}

func (runner *adaptiveObservationRunner) skipDecision(now uint64, reason string) ObservationDecision {
	decision := ObservationDecision{
		Kind: DecisionSkip, Mode: runner.config.Mode, Reason: reason,
		Source: runner.config.Source, StreamID: runner.streamID,
		SourceRevision: runner.sourceRevision, TriggerItemID: "", AtNS: now,
		NextDueNS: runner.nextDueNS,
	}
	if runner.latest != nil {
		decision.FrameKind = string(runner.latest.kind)
		decision.FrameIndex = runner.latest.index
		decision.FrameItemID = runner.latest.itemID
		decision.ChangeScore = runner.latest.change
	}
	return decision
}

func (runner *adaptiveObservationRunner) publishDecision(ctx context.Context, cause element.Envelope, decision ObservationDecision) error {
	itemID, err := runner.nextItemID(cause.ItemID, "decision")
	if err != nil {
		return err
	}
	decision.TriggerItemID = cause.ItemID
	envelope := cause.Clone()
	envelope.Type = decisionType
	envelope.ItemID = itemID
	envelope.SourceID = runner.config.Source
	envelope.CausalParents = uniqueParents(runner.sourceStartItemID, cause.ItemID, decision.FrameItemID)
	envelope.Payload = decision
	_, err = runner.ports.decision.Broadcast(ctx, envelope)
	return err
}

func (runner *adaptiveObservationRunner) publishVisualRefresh(ctx context.Context, cause element.Envelope, request Refresh) error {
	itemID, err := runner.nextItemID(cause.ItemID, "observer_refresh")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = visualRefreshType
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = perceptionelements.VisualRefresh{Source: request.Source, Reason: request.Reason}
	_, err = runner.ports.observerRefresh.Broadcast(ctx, envelope)
	return err
}

func (runner *adaptiveObservationRunner) publishVisualClose(ctx context.Context, cause element.Envelope, request SourceEnd) error {
	itemID, err := runner.nextItemID(cause.ItemID, "observer_close")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = visualCloseType
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = perceptionelements.VisualSourceClose{Source: request.Source}
	_, err = runner.ports.observerClose.Broadcast(ctx, envelope)
	return err
}

func (runner *adaptiveObservationRunner) publishVisualCancel(ctx context.Context, cause element.Envelope, request Cancel) error {
	itemID, err := runner.nextItemID(cause.ItemID, "observer_cancel")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = visualCancelType
	envelope.ItemID = itemID
	envelope.CancellationScope = request.StreamID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = perceptionelements.VisualCancel{StreamID: request.StreamID, Source: request.Source, Reason: request.Reason}
	_, err = runner.ports.observerCancel.Broadcast(ctx, envelope)
	return err
}

func (runner *adaptiveObservationRunner) finish(ctx context.Context, cause element.Envelope, outcome PolicyOutcome, publishState, remember bool) error {
	if remember {
		runner.rememberTerminal(cause.ItemID)
	}
	if publishState {
		if err := runner.publishState(ctx, cause); err != nil {
			return err
		}
	}
	itemID, err := runner.nextItemID(cause.ItemID, "outcome")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = policyOutcomeType
	envelope.ItemID = itemID
	envelope.SourceID = runner.config.Source
	envelope.CausalParents = []string{cause.ItemID}
	outcome.Terminal = true
	envelope.Payload = outcome
	_, err = runner.ports.outcome.Broadcast(ctx, envelope)
	return err
}

func (runner *adaptiveObservationRunner) publishState(ctx context.Context, cause element.Envelope) error {
	if runner.stateSequence == math.MaxUint64 {
		return errors.New("adaptive video state sequence exhausted")
	}
	runner.stateSequence++
	itemID, err := runner.nextItemID(cause.ItemID, "state")
	if err != nil {
		return err
	}
	state := ObservationPolicyState{
		Sequence: runner.stateSequence, Phase: runner.phase, Mode: runner.config.Mode,
		Source: runner.config.Source, StreamID: runner.streamID, SessionID: runner.sessionID,
		SourceRevision: runner.sourceRevision, SourceStartItemID: runner.sourceStartItemID,
		LastObservedIndex: runner.lastObservedIndex, LastObservedNS: runner.lastObservedNS,
		LastTickNS: runner.lastTickNS, NextDueNS: runner.nextDueNS,
		RefreshArmed: runner.refreshArmed, AcceptedFrames: runner.acceptedFrames,
		SupersededFrames: runner.supersededFrames, RejectedFrames: runner.rejectedFrames,
		Observations: runner.observations, SkippedTicks: runner.skippedTicks,
		TerminalTombstones: len(runner.terminated), TerminalMemoryLimit: runner.config.TerminalMemory,
		MaxRetainedFrameBytes: runner.config.MaxFrameBytes,
	}
	if runner.latest != nil {
		state.LatestKind = string(runner.latest.kind)
		state.LatestItemID = runner.latest.itemID
		state.LatestIndex = runner.latest.index
		state.LatestCapturedNS = runner.latest.capturedNS
		state.ChangeScore = runner.latest.change
	}
	envelope := cause.Clone()
	envelope.Type = policyStateType
	envelope.ItemID = itemID
	envelope.SourceID = runner.config.Source
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = state
	_, err = runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *adaptiveObservationRunner) outcome(kind OutcomeKind, operation, stream string, revision, index uint64, code, message string) PolicyOutcome {
	return PolicyOutcome{
		Kind: kind, Operation: operation, Source: runner.config.Source,
		StreamID: stream, SourceRevision: revision, FrameIndex: index,
		Code: code, Message: message, Terminal: true,
	}
}

func (runner *adaptiveObservationRunner) validateSourceEnvelope(envelope element.Envelope, request SourceStart) error {
	if strings.TrimSpace(envelope.SessionID) == "" {
		return errors.New("source start requires a session ID")
	}
	if request.Source != runner.config.Source || envelope.SourceID != runner.config.Source {
		return fmt.Errorf("source %q/envelope source %q does not match configured source %q", request.Source, envelope.SourceID, runner.config.Source)
	}
	if _, err := canonicalID(request.StreamID, "stream_id"); err != nil {
		return err
	}
	if err := request.Kind.validate(); err != nil {
		return err
	}
	if request.SourceRevision == 0 || request.OpenedNS == 0 {
		return errors.New("source_revision and opened_ns must be positive")
	}
	metadata := len(request.Source) + len(request.StreamID) + len(request.ExpectedMIMEType) + len(request.ExternalReferenceURI)
	if metadata > runner.config.MaxMetadataBytes {
		return fmt.Errorf("source metadata has %d bytes; max_metadata_bytes is %d", metadata, runner.config.MaxMetadataBytes)
	}
	if strings.ContainsAny(request.ExpectedMIMEType+request.ExternalReferenceURI, "\x00\r\n") {
		return errors.New("source metadata is not canonical")
	}
	if request.ExpectedMIMEType != strings.TrimSpace(request.ExpectedMIMEType) ||
		request.ExternalReferenceURI != strings.TrimSpace(request.ExternalReferenceURI) {
		return errors.New("source MIME type and reference locator must be canonical")
	}
	return nil
}

func (runner *adaptiveObservationRunner) validateFrameEnvelope(envelope element.Envelope, request InlineFrame) error {
	if err := runner.validateActiveScope(envelope, request.Frame.Source, request.StreamID, request.SourceRevision); err != nil {
		return err
	}
	if err := validateInlineFrame(request); err != nil {
		return err
	}
	if request.Frame.Bytes() > runner.config.MaxFrameBytes {
		return fmt.Errorf("frame has %d bytes; max_frame_bytes is %d", request.Frame.Bytes(), runner.config.MaxFrameBytes)
	}
	if request.Frame.MIMEType != strings.TrimSpace(request.Frame.MIMEType) ||
		strings.ContainsAny(request.Frame.MIMEType, "\x00\r\n") {
		return errors.New("frame MIME type is not canonical")
	}
	metadata := len(request.Frame.Source) + len(request.StreamID) + len(request.Frame.MIMEType)
	if metadata > runner.config.MaxMetadataBytes {
		return fmt.Errorf("frame metadata has %d bytes; max_metadata_bytes is %d", metadata, runner.config.MaxMetadataBytes)
	}
	if runner.expectedMIMEType != "" && request.Frame.MIMEType != runner.expectedMIMEType {
		return fmt.Errorf("frame MIME type %q does not match source MIME type %q", request.Frame.MIMEType, runner.expectedMIMEType)
	}
	if request.Frame.CapturedNS < runner.openedNS {
		return errors.New("frame capture time predates source start")
	}
	if envelope.CaptureNS != request.Frame.CapturedNS {
		return errors.New("frame capture time does not match its envelope")
	}
	if runner.haveFrame && (request.Frame.Index <= runner.lastIndex || request.Frame.CapturedNS <= runner.lastCapturedNS) {
		return errors.New("frame is stale or out of order")
	}
	return nil
}

func (runner *adaptiveObservationRunner) validateReferenceEnvelope(envelope element.Envelope, request FrameReference) error {
	if err := runner.validateActiveScope(envelope, request.Source, request.StreamID, request.SourceRevision); err != nil {
		return err
	}
	if request.Reference == "" || request.Reference != strings.TrimSpace(request.Reference) || strings.ContainsAny(request.Reference, "\x00\r\n") {
		return errors.New("frame reference requires a canonical locator")
	}
	if request.MIMEType == "" || request.MIMEType != strings.TrimSpace(request.MIMEType) || strings.ContainsAny(request.MIMEType, "\x00\r\n") {
		return errors.New("frame reference requires a canonical MIME type")
	}
	if request.Fingerprint == "" || request.Fingerprint != strings.TrimSpace(request.Fingerprint) || strings.ContainsAny(request.Fingerprint, "\x00\r\n") {
		return errors.New("frame reference requires a canonical fingerprint")
	}
	if request.Width <= 0 || request.Height <= 0 || request.Bytes < 0 || request.Bytes > runner.config.MaxFrameBytes {
		return errors.New("frame reference dimensions or byte bound are invalid")
	}
	if runner.expectedMIMEType != "" && request.MIMEType != runner.expectedMIMEType {
		return fmt.Errorf("reference MIME type %q does not match source MIME type %q", request.MIMEType, runner.expectedMIMEType)
	}
	if !referenceWithin(request.Reference, runner.externalReference) {
		return errors.New("frame reference is outside the declared source locator")
	}
	metadata := len(request.Source) + len(request.StreamID) + len(request.Reference) + len(request.MIMEType) + len(request.Fingerprint)
	if metadata > runner.config.MaxMetadataBytes {
		return fmt.Errorf("reference metadata has %d bytes; max_metadata_bytes is %d", metadata, runner.config.MaxMetadataBytes)
	}
	if request.CapturedNS < runner.openedNS {
		return errors.New("reference capture time predates source start")
	}
	if envelope.CaptureNS != request.CapturedNS {
		return errors.New("reference capture time does not match its envelope")
	}
	if runner.haveFrame && (request.Index <= runner.lastIndex || request.CapturedNS <= runner.lastCapturedNS) {
		return errors.New("frame reference is stale or out of order")
	}
	return nil
}

func (runner *adaptiveObservationRunner) validateActiveScope(envelope element.Envelope, source, stream string, revision uint64) error {
	if !runner.active || runner.phase != PhaseWatching {
		key := sourceKey(envelope.SessionID, source, stream, revision)
		if terminal, found := runner.terminated[key]; found {
			return fmt.Errorf("source generation is already %s", terminal.phase)
		}
		return errors.New("no source generation is active")
	}
	if envelope.SessionID != runner.sessionID {
		return errors.New("input crossed the active source session boundary")
	}
	if source != runner.config.Source || envelope.SourceID != runner.config.Source {
		return errors.New("input crossed the configured source boundary")
	}
	if stream != runner.streamID || revision != runner.sourceRevision {
		return errors.New("input does not match the active stream generation")
	}
	return nil
}

func validateControlIdentity(envelope element.Envelope, configuredSource, source, stream string, revision uint64) error {
	if strings.TrimSpace(envelope.SessionID) == "" {
		return errors.New("control requires a session ID")
	}
	if source != configuredSource || envelope.SourceID != configuredSource {
		return errors.New("control crossed the configured source boundary")
	}
	if _, err := canonicalID(stream, "stream_id"); err != nil {
		return err
	}
	if revision == 0 {
		return errors.New("control source_revision must be positive")
	}
	return nil
}

func (runner *adaptiveObservationRunner) initialDue(opened uint64) (uint64, error) {
	switch runner.config.Mode {
	case CadenceFixed:
		return safeAddNS(opened, runner.fixedIntervalNS())
	case CadenceAdaptive:
		return safeAddNS(opened, runner.minIntervalNS())
	case CadenceManual:
		return 0, nil
	default:
		return 0, errors.New("invalid cadence mode")
	}
}

func (runner *adaptiveObservationRunner) fixedIntervalNS() uint64 {
	return uint64(runner.config.FixedIntervalMS) * 1_000_000
}
func (runner *adaptiveObservationRunner) minIntervalNS() uint64 {
	return uint64(runner.config.MinIntervalMS) * 1_000_000
}
func (runner *adaptiveObservationRunner) maxIntervalNS() uint64 {
	return uint64(runner.config.MaxIntervalMS) * 1_000_000
}

func safeAddNS(base, delta uint64) (uint64, error) {
	if math.MaxUint64-base < delta {
		return 0, errors.New("video cadence timestamp overflow")
	}
	return base + delta, nil
}

func saturatingAddNS(base, delta uint64) uint64 {
	value, err := safeAddNS(base, delta)
	if err != nil {
		return math.MaxUint64
	}
	return value
}

// A source locator is either an exact opaque locator or an explicitly
// slash-terminated namespace. Requiring the slash for prefix semantics keeps
// e.g. video/1 from accidentally authorizing video/10.
func referenceWithin(reference, declared string) bool {
	if declared == "" || reference == declared {
		return true
	}
	return strings.HasSuffix(declared, "/") && strings.HasPrefix(reference, declared)
}

func advanceFixedDue(due, now, interval uint64) uint64 {
	if interval == 0 || due > now {
		return due
	}
	steps := (now-due)/interval + 1
	if steps > (math.MaxUint64-due)/interval {
		return math.MaxUint64
	}
	return due + steps*interval
}

func sampleBytes(payload []byte, limit int) []byte {
	if len(payload) <= limit {
		return slices.Clone(payload)
	}
	result := make([]byte, limit)
	for index := range result {
		position := index * (len(payload) - 1) / (limit - 1)
		result[index] = payload[position]
	}
	return result
}

func sampledChange(previous, current []byte) float64 {
	if len(previous) == 0 || len(current) == 0 || len(previous) != len(current) {
		return 1
	}
	var difference uint64
	for index := range current {
		left, right := int(previous[index]), int(current[index])
		if left > right {
			difference += uint64(left - right)
		} else {
			difference += uint64(right - left)
		}
	}
	return float64(difference) / float64(len(current)*255)
}

func (runner *adaptiveObservationRunner) nextItemID(parent, label string) (string, error) {
	sequence, err := runner.sequences.Next(runner.instance + "." + label)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s:%d", parent, label, sequence), nil
}

func (runner *adaptiveObservationRunner) isDuplicate(itemID string) bool {
	_, found := runner.terminalItems[itemID]
	return found
}

func (runner *adaptiveObservationRunner) rememberTerminal(itemID string) {
	if itemID == "" {
		return
	}
	if _, found := runner.terminalItems[itemID]; !found {
		runner.terminalItems[itemID] = struct{}{}
		runner.terminalOrder = append(runner.terminalOrder, itemID)
	}
	for len(runner.terminalOrder) > runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		runner.terminalOrder = runner.terminalOrder[1:]
		delete(runner.terminalItems, oldest)
	}
}

func (runner *adaptiveObservationRunner) recordTerminated(key string, phase PolicyPhase, reason string) {
	if _, found := runner.terminated[key]; !found {
		runner.terminatedOrder = append(runner.terminatedOrder, key)
	}
	runner.terminated[key] = terminalSource{phase: phase, reason: strings.TrimSpace(reason)}
	for len(runner.terminatedOrder) > runner.config.TerminalMemory {
		oldest := runner.terminatedOrder[0]
		runner.terminatedOrder = runner.terminatedOrder[1:]
		delete(runner.terminated, oldest)
	}
}

func sourceKey(session, source, stream string, revision uint64) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", session, source, stream, revision)
}

func uniqueParents(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !slices.Contains(result, value) {
			result = append(result, value)
		}
	}
	return result
}

func receivePolicyInputs(ctx context.Context, kind string, input element.InputPort, output chan<- policyInput, failures chan<- error, wait *sync.WaitGroup) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- fmt.Errorf("receive adaptive video %s: %w", kind, err):
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- policyInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func receivePolicyInterrupts(ctx context.Context, input element.InputPort, output chan<- element.Envelope, failures chan<- error, wait *sync.WaitGroup) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- fmt.Errorf("receive adaptive video cancel: %w", err):
			case <-ctx.Done():
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

func terminal(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}

func sourceStartPayload(payload any) (SourceStart, bool) {
	switch value := payload.(type) {
	case SourceStart:
		return value, true
	case *SourceStart:
		if value != nil {
			return *value, true
		}
	}
	return SourceStart{}, false
}

func inlineFramePayload(payload any) (InlineFrame, bool) {
	switch value := payload.(type) {
	case InlineFrame:
		return value, true
	case *InlineFrame:
		if value != nil {
			return *value, true
		}
	}
	return InlineFrame{}, false
}

func frameReferencePayload(payload any) (FrameReference, bool) {
	switch value := payload.(type) {
	case FrameReference:
		return value, true
	case *FrameReference:
		if value != nil {
			return *value, true
		}
	}
	return FrameReference{}, false
}

func timingTickPayload(payload any) (TimingTick, bool) {
	switch value := payload.(type) {
	case TimingTick:
		return value, true
	case *TimingTick:
		if value != nil {
			return *value, true
		}
	}
	return TimingTick{}, false
}

func refreshPayload(payload any) (Refresh, bool) {
	switch value := payload.(type) {
	case Refresh:
		return value, true
	case *Refresh:
		if value != nil {
			return *value, true
		}
	}
	return Refresh{}, false
}

func sourceEndPayload(payload any) (SourceEnd, bool) {
	switch value := payload.(type) {
	case SourceEnd:
		return value, true
	case *SourceEnd:
		if value != nil {
			return *value, true
		}
	}
	return SourceEnd{}, false
}

func cancelPayload(payload any) (Cancel, bool) {
	switch value := payload.(type) {
	case Cancel:
		return value, true
	case *Cancel:
		if value != nil {
			return *value, true
		}
	}
	return Cancel{}, false
}
