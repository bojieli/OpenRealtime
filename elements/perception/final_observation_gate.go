package perception

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

const (
	defaultFinalObservationGateMemory = 512
	maximumFinalObservationTerminals  = 16384
)

var finalObservationGateOutcomeType = element.Event(
	element.Named("perception.FinalObservationGateOutcome"),
)

// FinalObservationGateDescriptor is a provider-neutral activation gate. A
// final observation alone is not sufficient authority to activate cognition:
// it must join the successful Flush outcome emitted for the same direct cause,
// session, and stream. Provisional revisions remain available on the ASR
// observation branch, but this element never forwards them.
func FinalObservationGateDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "perception.FinalObservationGate",
		Revision:      1,
		Ports: []element.Port{
			{Name: "observations", Direction: element.Input, Type: observationType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "flush", Direction: element.Input, Type: perceptionOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "finals", Direction: element.Output, Type: observationType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: finalObservationGateOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers: []string{"observations", "flush"}, Outcomes: []string{"finals", "outcome"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		Dependencies: []element.Dependency{{Name: graphruntime.SequenceServiceName}},
	}
}

func FinalObservationGateOutcomeType() element.Type {
	return finalObservationGateOutcomeType.Clone()
}

type FinalObservationGateOutcomeKind string

const (
	FinalObservationEmitted  FinalObservationGateOutcomeKind = "emitted"
	FinalObservationPending  FinalObservationGateOutcomeKind = "pending"
	FinalObservationIgnored  FinalObservationGateOutcomeKind = "ignored"
	FinalObservationCanceled FinalObservationGateOutcomeKind = "canceled"
	FinalObservationRefused  FinalObservationGateOutcomeKind = "refused"
	FinalObservationFailed   FinalObservationGateOutcomeKind = "failed"
)

type FinalObservationGateOutcome struct {
	Kind                     FinalObservationGateOutcomeKind `json:"kind"`
	Operation                string                          `json:"operation"`
	StreamID                 string                          `json:"stream_id,omitempty"`
	CauseItemID              string                          `json:"cause_item_id,omitempty"`
	SourceObservationItemID  string                          `json:"source_observation_item_id,omitempty"`
	EmittedObservationItemID string                          `json:"emitted_observation_item_id,omitempty"`
	Revision                 uint64                          `json:"revision,omitempty"`
	Code                     string                          `json:"code,omitempty"`
	Message                  string                          `json:"message,omitempty"`
}

type finalObservationGateFactory struct{}

func (finalObservationGateFactory) Descriptor() element.Descriptor {
	return FinalObservationGateDescriptor()
}

func (finalObservationGateFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	sequenceService, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("perception.FinalObservationGate has no sequence service")
	}
	sequences, ok := sequenceService.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("final observation gate sequence service has type %T", sequenceService)
	}
	observations, err := mount.Ports.Input("observations")
	if err != nil {
		return nil, err
	}
	flush, err := mount.Ports.Input("flush")
	if err != nil {
		return nil, err
	}
	finals, err := mount.Ports.Output("finals")
	if err != nil {
		return nil, err
	}
	outcome, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &finalObservationGateRunner{
		instance: mount.InstanceID, observations: observations, flush: flush,
		finals: finals, outcome: outcome, resolution: mount.Resolution, sequences: sequences,
		pendingFinals:  make(map[finalObservationKey]pendingFinalObservation),
		pendingFlushes: make(map[finalObservationKey]pendingFinalFlush),
		nonFinal:       make(map[finalObservationKey]struct{}),
		terminal:       newFinalObservationMemory(maximumFinalObservationTerminals),
	}, nil
}

type finalObservationKey struct {
	session string
	stream  string
	cause   string
}

type pendingFinalObservation struct {
	envelope    element.Envelope
	observation coreperception.Observation
}

type pendingFinalFlush struct {
	envelope element.Envelope
	outcome  Outcome
}

type finalObservationInput struct {
	port     string
	envelope element.Envelope
}

type finalObservationGateRunner struct {
	instance     string
	observations element.InputPort
	flush        element.InputPort
	finals       element.OutputPort
	outcome      element.OutputPort
	resolution   element.ResolutionReporter
	sequences    *graphruntime.SequenceAllocator

	pendingFinals  map[finalObservationKey]pendingFinalObservation
	pendingFlushes map[finalObservationKey]pendingFinalFlush
	pendingOrder   []finalObservationKey
	nonFinal       map[finalObservationKey]struct{}
	nonFinalOrder  []finalObservationKey
	terminal       *finalObservationMemory
}

func (runner *finalObservationGateRunner) Run(parent context.Context) error {
	if err := reportFinalObservationGateResolution(runner.resolution); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	inputs := make(chan finalObservationInput)
	failures := make(chan error, 2)
	var receivers sync.WaitGroup
	for _, source := range []struct {
		name string
		port element.InputPort
	}{{"observation", runner.observations}, {"flush", runner.flush}} {
		receivers.Add(1)
		go receiveFinalObservationInputs(ctx, source.name, source.port, inputs, failures, &receivers)
	}
	defer func() {
		cancel()
		receivers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			cancel()
			return err
		case input := <-inputs:
			var err error
			if input.port == "observation" {
				err = runner.acceptObservation(ctx, input.envelope)
			} else {
				err = runner.acceptFlush(ctx, input.envelope)
			}
			if err != nil {
				cancel()
				return err
			}
		}
	}
}

func (runner *finalObservationGateRunner) acceptObservation(
	ctx context.Context, envelope element.Envelope,
) error {
	observation, ok := observationPayloadForFinalGate(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "observation", Code: "invalid_payload",
			Message: fmt.Sprintf("final observation gate received payload %T", envelope.Payload),
		})
	}
	stream, cause, err := finalObservationAddress(envelope)
	if err != nil {
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "observation", Code: "invalid_address", Message: err.Error(),
		})
	}
	if err := observation.Validate(); err != nil || observation.Revision == 0 ||
		(observation.Final && observation.Provisional) {
		if err == nil {
			err = errors.New("observation requires a positive revision and mutually exclusive final/provisional state")
		}
		key := finalObservationKey{session: envelope.SessionID, stream: stream, cause: cause}
		runner.finishKey(key)
		runner.terminal.add(key)
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "observation", StreamID: stream,
			CauseItemID: cause, Revision: observation.Revision, Code: "invalid_observation", Message: err.Error(),
		})
	}
	key := finalObservationKey{session: envelope.SessionID, stream: stream, cause: cause}
	if runner.terminal.contains(key) {
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationIgnored, Operation: "observation", StreamID: stream,
			CauseItemID: cause, SourceObservationItemID: envelope.ItemID, Revision: observation.Revision,
			Code: "terminal_replay", Message: "observation cause is already terminal",
		})
	}
	if runner.terminal.full() {
		runner.finishKey(key)
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "observation", StreamID: stream,
			CauseItemID: cause, SourceObservationItemID: envelope.ItemID,
			Code: "terminal_capacity", Message: "final observation gate exhausted its exact replay memory",
		})
	}
	if !observation.Final {
		if flush, found := runner.pendingFlushes[key]; found {
			delete(runner.pendingFlushes, key)
			runner.removePendingOrder(key)
			runner.removeNonFinal(key)
			runner.terminal.add(key)
			if err := runner.publishOutcome(ctx, flush.envelope, FinalObservationGateOutcome{
				Kind: FinalObservationRefused, Operation: "flush", StreamID: stream,
				CauseItemID: cause, SourceObservationItemID: envelope.ItemID,
				Revision: observation.Revision, Code: "flush_observation_not_final",
				Message: "successful flush authority was paired with a non-final observation",
			}); err != nil {
				return err
			}
		} else {
			runner.rememberNonFinal(key)
		}
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationIgnored, Operation: "observation", StreamID: stream,
			CauseItemID: cause, SourceObservationItemID: envelope.ItemID, Revision: observation.Revision,
			Code: "non_final", Message: "only a terminal observation may activate cognition",
		})
	}
	if _, duplicate := runner.pendingFinals[key]; duplicate {
		runner.finishKey(key)
		runner.terminal.add(key)
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "observation", StreamID: stream,
			CauseItemID: cause, SourceObservationItemID: envelope.ItemID, Revision: observation.Revision,
			Code: "duplicate_final", Message: "flush cause produced more than one final observation",
		})
	}
	if err := runner.makePendingRoom(ctx, key); err != nil {
		return err
	}
	if runner.terminal.full() {
		runner.finishKey(key)
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "observation", StreamID: stream,
			CauseItemID: cause, SourceObservationItemID: envelope.ItemID,
			Code: "terminal_capacity", Message: "final observation gate exhausted its exact replay memory",
		})
	}
	runner.pendingFinals[key] = pendingFinalObservation{
		envelope: envelope.Clone(), observation: observation,
	}
	runner.rememberPending(key)
	if flush, found := runner.pendingFlushes[key]; found {
		delete(runner.pendingFlushes, key)
		return runner.emit(ctx, key, runner.pendingFinals[key], flush)
	}
	return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
		Kind: FinalObservationPending, Operation: "observation", StreamID: stream,
		CauseItemID: cause, SourceObservationItemID: envelope.ItemID, Revision: observation.Revision,
		Code: "awaiting_flush", Message: "final observation is waiting for successful flush authority",
	})
}

func (runner *finalObservationGateRunner) acceptFlush(
	ctx context.Context, envelope element.Envelope,
) error {
	outcome, ok := outcomePayloadForFinalGate(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "flush", Code: "invalid_payload",
			Message: fmt.Sprintf("final observation gate received flush payload %T", envelope.Payload),
		})
	}
	stream, cause, err := finalObservationAddress(envelope)
	if err != nil || outcome.StreamID != stream || outcome.CauseItemID != cause {
		if err == nil {
			err = fmt.Errorf("ASR outcome address %q/%q differs from envelope address %q/%q",
				outcome.StreamID, outcome.CauseItemID, stream, cause)
		}
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: outcome.Operation, StreamID: outcome.StreamID,
			Code: "invalid_address", Message: err.Error(),
		})
	}
	key := finalObservationKey{session: envelope.SessionID, stream: stream, cause: cause}
	if runner.terminal.contains(key) {
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationIgnored, Operation: outcome.Operation, StreamID: stream,
			CauseItemID: cause, Code: "terminal_replay", Message: "ASR operation cause is already terminal",
		})
	}
	if outcome.Operation == "observe" {
		runner.finishKey(key)
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationIgnored, Operation: "observe", StreamID: stream,
			CauseItemID: cause, Code: "not_flush",
			Message: "observe outcome cannot authorize a final observation",
		})
	}
	if outcome.Operation == "cancel" {
		kind := FinalObservationCanceled
		switch outcome.Kind {
		case OutcomeCanceled, OutcomeSucceeded:
			runner.cancelStream(envelope.SessionID, stream)
			runner.terminal.add(key)
		case OutcomeRefused:
			kind = FinalObservationRefused
		case OutcomeIgnored:
			kind = FinalObservationIgnored
		default:
			kind = FinalObservationFailed
			runner.cancelStream(envelope.SessionID, stream)
			runner.terminal.add(key)
		}
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: kind, Operation: "cancel", StreamID: stream,
			CauseItemID: cause, Code: firstNonempty(outcome.Code, "canceled"), Message: outcome.Message,
		})
	}
	if outcome.Operation != "flush" {
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationIgnored, Operation: outcome.Operation, StreamID: stream,
			CauseItemID: cause, Code: "not_flush",
			Message: "ASR outcome cannot authorize a final observation",
		})
	}
	if runner.terminal.full() {
		runner.finishKey(key)
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "flush", StreamID: stream,
			CauseItemID: cause, Code: "terminal_capacity",
			Message: "final observation gate exhausted its exact replay memory",
		})
	}
	if outcome.Kind != OutcomeSucceeded || outcome.ObservationCount != 1 {
		runner.finishKey(key)
		runner.terminal.add(key)
		kind := FinalObservationFailed
		if outcome.Kind == OutcomeCanceled || outcome.Kind == OutcomeIgnored {
			kind = FinalObservationCanceled
		} else if outcome.Kind == OutcomeRefused ||
			(outcome.Kind == OutcomeSucceeded && outcome.ObservationCount != 1) {
			kind = FinalObservationRefused
		}
		code := outcome.Code
		if code == "" {
			code = "flush_without_one_final"
		}
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: kind, Operation: "flush", StreamID: stream, CauseItemID: cause,
			Code: code, Message: outcome.Message,
		})
	}
	if _, sawNonFinal := runner.nonFinal[key]; sawNonFinal {
		runner.finishKey(key)
		runner.terminal.add(key)
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "flush", StreamID: stream,
			CauseItemID: cause, Code: "flush_observation_not_final",
			Message: "successful flush authority named a non-final observation",
		})
	}
	if _, duplicate := runner.pendingFlushes[key]; duplicate {
		runner.finishKey(key)
		runner.terminal.add(key)
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "flush", StreamID: stream,
			CauseItemID: cause, Code: "duplicate_flush", Message: "flush authority was delivered more than once",
		})
	}
	if final, found := runner.pendingFinals[key]; found {
		return runner.emit(ctx, key, final, pendingFinalFlush{envelope: envelope.Clone(), outcome: outcome})
	}
	if err := runner.makePendingRoom(ctx, key); err != nil {
		return err
	}
	if runner.terminal.full() {
		runner.finishKey(key)
		return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "flush", StreamID: stream,
			CauseItemID: cause, Code: "terminal_capacity",
			Message: "final observation gate exhausted its exact replay memory",
		})
	}
	runner.pendingFlushes[key] = pendingFinalFlush{envelope: envelope.Clone(), outcome: outcome}
	runner.rememberPending(key)
	return runner.publishOutcome(ctx, envelope, FinalObservationGateOutcome{
		Kind: FinalObservationPending, Operation: "flush", StreamID: stream,
		CauseItemID: cause, Code: "awaiting_final", Message: "successful flush is waiting for its final observation",
	})
}

func (runner *finalObservationGateRunner) emit(
	ctx context.Context, key finalObservationKey, final pendingFinalObservation, flush pendingFinalFlush,
) error {
	runner.finishKey(key)
	runner.terminal.add(key)
	if final.envelope.ItemID == flush.envelope.ItemID {
		return runner.publishOutcome(ctx, flush.envelope, FinalObservationGateOutcome{
			Kind: FinalObservationRefused, Operation: "flush", StreamID: key.stream,
			CauseItemID: key.cause, SourceObservationItemID: final.envelope.ItemID,
			Revision: final.observation.Revision, Code: "evidence_identity_collision",
			Message: "final observation and flush authority require distinct immutable item IDs",
		})
	}
	envelope := final.envelope.Clone()
	envelope.Type = runner.finals.Type()
	sequence, err := runner.sequences.Next(runner.instance + ".final")
	if err != nil {
		return err
	}
	envelope.ItemID = fmt.Sprintf("%s/final/%d", runner.instance, sequence)
	envelope.CausalParents = []string{final.envelope.ItemID, flush.envelope.ItemID}
	observation := final.observation
	observation.Media = slices.Clone(final.observation.Media)
	// Supersedes is an ASR-revision relationship. Provisional revisions stay
	// observable but deliberately never cross this activation gate, so carrying
	// their revision ID into the canonical commit boundary would falsely claim
	// that the predecessor was committed. The original final observation and
	// its complete revision relationship remain immutable causal evidence; the
	// derived flush-attested snapshot starts the canonical revision chain.
	observation.Supersedes = 0
	envelope.Payload = observation
	delivery, err := runner.finals.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return fmt.Errorf("final observation gate delivery = %+v, want exactly one delivered consumer", delivery)
	}
	return runner.publishOutcome(ctx, flush.envelope, FinalObservationGateOutcome{
		Kind: FinalObservationEmitted, Operation: "flush", StreamID: key.stream,
		CauseItemID: key.cause, SourceObservationItemID: final.envelope.ItemID,
		EmittedObservationItemID: envelope.ItemID, Revision: final.observation.Revision,
	})
}

func (runner *finalObservationGateRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome FinalObservationGateOutcome,
) error {
	envelope := cause.Clone()
	envelope.Type = runner.outcome.Type()
	sequence, err := runner.sequences.Next(runner.instance + ".outcome")
	if err != nil {
		return err
	}
	envelope.ItemID = fmt.Sprintf("%s/outcome/%d", runner.instance, sequence)
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = outcome
	if outcome.Kind == FinalObservationEmitted {
		envelope.CausalParents = nil
		for _, itemID := range []string{
			outcome.SourceObservationItemID, cause.ItemID, outcome.EmittedObservationItemID,
		} {
			if itemID != "" && !slices.Contains(envelope.CausalParents, itemID) {
				envelope.CausalParents = append(envelope.CausalParents, itemID)
			}
		}
	}
	delivery, err := runner.outcome.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return fmt.Errorf("final observation gate outcome delivery = %+v, want exactly one delivered consumer", delivery)
	}
	return nil
}

func finalObservationAddress(envelope element.Envelope) (string, string, error) {
	if !canonicalFinalObservationIdentity(envelope.SessionID) {
		return "", "", errors.New("final observation gate requires a canonical session ID")
	}
	stream := envelope.CancellationScope
	if stream == "" {
		stream = envelope.SourceID
	}
	if !canonicalFinalObservationIdentity(stream) {
		return "", "", errors.New("final observation gate requires a stream scope")
	}
	cause := envelope.OpportunityID
	if !canonicalFinalObservationIdentity(cause) || !slices.Contains(envelope.CausalParents, cause) {
		return "", "", errors.New("final observation gate requires an explicit causal operation ID")
	}
	if !canonicalFinalObservationIdentity(envelope.ItemID) || envelope.ItemID == cause {
		return "", "", errors.New("final observation gate requires a distinct canonical evidence item ID")
	}
	return stream, cause, nil
}

func canonicalFinalObservationIdentity(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func observationPayloadForFinalGate(payload any) (coreperception.Observation, bool) {
	switch value := payload.(type) {
	case coreperception.Observation:
		value.Media = slices.Clone(value.Media)
		return value, true
	case *coreperception.Observation:
		if value != nil {
			copy := *value
			copy.Media = slices.Clone(value.Media)
			return copy, true
		}
	}
	return coreperception.Observation{}, false
}

func outcomePayloadForFinalGate(payload any) (Outcome, bool) {
	switch value := payload.(type) {
	case Outcome:
		return value, true
	case *Outcome:
		if value != nil {
			return *value, true
		}
	}
	return Outcome{}, false
}

func receiveFinalObservationInputs(
	ctx context.Context, name string, input element.InputPort, output chan<- finalObservationInput,
	failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- err:
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- finalObservationInput{port: name, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func reportFinalObservationGateResolution(reporter element.ResolutionReporter) error {
	identity, err := FinalObservationGateDescriptor().Identity()
	if err != nil {
		return err
	}
	return liveidentity.Report(reporter, liveidentity.Artifact{
		ID: identity.Name, Digest: identity.Digest,
	}, nil)
}

func finalObservationGateRegistration() factoryprofile.Entry {
	return factoryprofile.Entry{Factory: finalObservationGateFactory{}}
}

func (runner *finalObservationGateRunner) rememberPending(key finalObservationKey) {
	for _, existing := range runner.pendingOrder {
		if existing == key {
			return
		}
	}
	runner.pendingOrder = append(runner.pendingOrder, key)
}

func (runner *finalObservationGateRunner) removePendingOrder(key finalObservationKey) {
	for index, existing := range runner.pendingOrder {
		if existing != key {
			continue
		}
		runner.pendingOrder = append(runner.pendingOrder[:index], runner.pendingOrder[index+1:]...)
		return
	}
}

func (runner *finalObservationGateRunner) removeNonFinal(key finalObservationKey) {
	delete(runner.nonFinal, key)
	for index, existing := range runner.nonFinalOrder {
		if existing != key {
			continue
		}
		runner.nonFinalOrder = append(runner.nonFinalOrder[:index], runner.nonFinalOrder[index+1:]...)
		return
	}
}

func (runner *finalObservationGateRunner) finishKey(key finalObservationKey) {
	delete(runner.pendingFinals, key)
	delete(runner.pendingFlushes, key)
	runner.removePendingOrder(key)
	runner.removeNonFinal(key)
}

func (runner *finalObservationGateRunner) makePendingRoom(
	ctx context.Context, incoming finalObservationKey,
) error {
	if _, found := runner.pendingFinals[incoming]; found {
		return nil
	}
	if _, found := runner.pendingFlushes[incoming]; found {
		return nil
	}
	for len(runner.pendingOrder) >= defaultFinalObservationGateMemory {
		oldest := runner.pendingOrder[0]
		runner.pendingOrder = runner.pendingOrder[1:]
		final, hasFinal := runner.pendingFinals[oldest]
		flush, hasFlush := runner.pendingFlushes[oldest]
		runner.finishKey(oldest)
		runner.terminal.add(oldest)
		cause := flush.envelope
		operation := "flush"
		if !hasFlush && hasFinal {
			cause, operation = final.envelope, "observation"
		}
		if cause.ItemID == "" {
			continue
		}
		if err := runner.publishOutcome(ctx, cause, FinalObservationGateOutcome{
			Kind: FinalObservationFailed, Operation: operation, StreamID: oldest.stream,
			CauseItemID: oldest.cause, Code: "pending_evicted",
			Message: "unpaired final-observation evidence exceeded bounded rendezvous memory",
		}); err != nil {
			return err
		}
	}
	return nil
}

func (runner *finalObservationGateRunner) rememberNonFinal(key finalObservationKey) {
	if _, found := runner.nonFinal[key]; found {
		return
	}
	runner.nonFinal[key] = struct{}{}
	runner.nonFinalOrder = append(runner.nonFinalOrder, key)
	for len(runner.nonFinalOrder) > defaultFinalObservationGateMemory {
		oldest := runner.nonFinalOrder[0]
		runner.removeNonFinal(oldest)
		runner.terminal.add(oldest)
	}
}

func (runner *finalObservationGateRunner) cancelStream(sessionID, streamID string) {
	for _, key := range slices.Clone(runner.pendingOrder) {
		if key.session == sessionID && key.stream == streamID {
			delete(runner.pendingFinals, key)
			delete(runner.pendingFlushes, key)
			runner.removePendingOrder(key)
			runner.terminal.add(key)
		}
	}
	for key := range runner.nonFinal {
		if key.session == sessionID && key.stream == streamID {
			runner.removeNonFinal(key)
			runner.terminal.add(key)
		}
	}
}

type finalObservationMemory struct {
	maximum int
	values  map[finalObservationKey]struct{}
}

func newFinalObservationMemory(maximum int) *finalObservationMemory {
	return &finalObservationMemory{maximum: maximum, values: make(map[finalObservationKey]struct{})}
}

func (memory *finalObservationMemory) contains(key finalObservationKey) bool {
	_, found := memory.values[key]
	return found
}

func (memory *finalObservationMemory) full() bool { return len(memory.values) >= memory.maximum }

func (memory *finalObservationMemory) add(key finalObservationKey) {
	if memory.contains(key) || memory.full() {
		return
	}
	memory.values[key] = struct{}{}
}

var _ element.Factory = finalObservationGateFactory{}
var _ element.Runnable = (*finalObservationGateRunner)(nil)
