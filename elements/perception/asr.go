// Package perception implements graph-native perception elements over the
// repository's stable provider APIs.
package perception

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

const ASRProviderRegistryService = "perception.asr.providers"

var (
	audioBatchType  = element.Trigger(element.Named("audio.FrameBatch"))
	audioFlushType  = element.Trigger(element.Named("audio.Flush"))
	audioCancelType = element.Interrupt(element.Named("audio.StreamID"))
	observationType = element.Revisions(
		element.Named("perception.Observation"), element.Named("perception.RevisionID"),
	)
	perceptionOutcomeType  = element.Event(element.Named("perception.Outcome"))
	providerResolutionType = element.State(element.Named("perception.ProviderResolution"))
)

// ASRDescriptor separates batching/cadence and acoustic endpointing from
// recognition. Each admitted batch is an explicit trigger; Flush is the
// independently visible utterance boundary; Cancel is an addressed interrupt.
func ASRDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "perception.ASR",
		Revision:      1,
		Ports: []element.Port{
			{Name: "observe", Direction: element.Input, Type: audioBatchType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "flush", Direction: element.Input, Type: audioFlushType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "cancel", Direction: element.Input, Type: audioCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "observations", Direction: element.Output, Type: observationType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "outcome", Direction: element.Output, Type: perceptionOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "resolved", Direction: element.Output, Type: providerResolutionType,
				Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{"observe", "flush"}, Interrupts: []string{"cancel"},
			Outcomes:       []string{"observations", "outcome", "resolved"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/perception/asr-state/v1",
		ConfigSchema: "schema://openrealtime/perception/asr-config/v1",
		Dependencies: []element.Dependency{{Name: ASRProviderRegistryService}},
		Effects:      []element.Effect{{Name: "perception.asr.session", Reversible: true}},
	}
}

type ASRConfig struct {
	Provider string `json:"provider"`
	Name     string `json:"name,omitempty"`
	Source   string `json:"source,omitempty"`
}

type AudioBatch struct {
	StreamID string                 `json:"stream_id"`
	Frames   []coreperception.Frame `json:"frames"`
}

type Flush struct {
	StreamID string `json:"stream_id,omitempty"`
}

type Cancel struct {
	StreamID string `json:"stream_id,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type OutcomeKind string

const (
	OutcomeSucceeded OutcomeKind = "succeeded"
	OutcomeCanceled  OutcomeKind = "canceled"
	OutcomeRefused   OutcomeKind = "refused"
	OutcomeFailed    OutcomeKind = "failed"
	OutcomeIgnored   OutcomeKind = "ignored"
)

type Outcome struct {
	Kind             OutcomeKind `json:"kind"`
	Operation        string      `json:"operation"`
	StreamID         string      `json:"stream_id,omitempty"`
	ObservationCount int         `json:"observation_count,omitempty"`
	Code             string      `json:"code,omitempty"`
	Message          string      `json:"message,omitempty"`
}

type ProviderResolution struct {
	Reference  string          `json:"reference"`
	Descriptor v1.Descriptor   `json:"descriptor"`
	Selected   map[string]bool `json:"selected,omitempty"`
}

type ASRProviderFactory func() (v1.PerceptionProvider, error)

type asrProviderEntry struct {
	descriptor v1.Descriptor
	factory    ASRProviderFactory
}

// ASRProviderRegistry is deployment state, not topology. Several ASR nodes
// can select different symbolic registrations from one immutable registry.
type ASRProviderRegistry struct {
	mu      sync.RWMutex
	entries map[string]asrProviderEntry
}

func NewASRProviderRegistry() *ASRProviderRegistry {
	return &ASRProviderRegistry{entries: make(map[string]asrProviderEntry)}
}

func (registry *ASRProviderRegistry) Register(
	reference string, descriptor v1.Descriptor, factory ASRProviderFactory,
) error {
	if registry == nil {
		return errors.New("register ASR provider: nil registry")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return errors.New("register ASR provider: empty reference")
	}
	if err := descriptor.Validate(); err != nil {
		return fmt.Errorf("register ASR provider %q: %w", reference, err)
	}
	if factory == nil {
		return fmt.Errorf("register ASR provider %q: nil factory", reference)
	}
	descriptor.Capabilities = maps.Clone(descriptor.Capabilities)
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]asrProviderEntry)
	}
	if _, duplicate := registry.entries[reference]; duplicate {
		return fmt.Errorf("ASR provider %q is already registered", reference)
	}
	registry.entries[reference] = asrProviderEntry{descriptor: descriptor, factory: factory}
	return nil
}

func (registry *ASRProviderRegistry) resolve(reference string) (asrProviderEntry, error) {
	if registry == nil {
		return asrProviderEntry{}, errors.New("ASR provider registry is nil")
	}
	registry.mu.RLock()
	entry, found := registry.entries[strings.TrimSpace(reference)]
	registry.mu.RUnlock()
	if !found {
		return asrProviderEntry{}, fmt.Errorf("ASR provider %q is not registered", reference)
	}
	entry.descriptor.Capabilities = maps.Clone(entry.descriptor.Capabilities)
	return entry, nil
}

type asrFactory struct{}

func (asrFactory) Descriptor() element.Descriptor { return ASRDescriptor() }

func (asrFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeASRConfig(source)
	return err
}

func (asrFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeASRConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("perception.ASR %s config: %w", mount.InstanceID, err)
	}
	service, _, found := mount.Services.Lookup(ASRProviderRegistryService)
	if !found {
		return nil, fmt.Errorf("perception.ASR %s has no provider registry service", mount.InstanceID)
	}
	registry, ok := service.(*ASRProviderRegistry)
	if !ok || registry == nil {
		return nil, fmt.Errorf("ASR provider registry service has type %T", service)
	}
	entry, err := registry.resolve(config.Provider)
	if err != nil {
		return nil, err
	}
	providers := &primedASRProviderFactory{
		reference: config.Provider, descriptor: entry.descriptor, factory: entry.factory,
	}
	observer, err := coreperception.NewAudioObserver(coreperception.AudioConfig{
		Provider: providers.New, Name: config.Name, Source: config.Source,
	})
	if err != nil {
		return nil, err
	}
	if err := mount.Lifecycle.Defer("reset-audio-observer", func(context.Context) error {
		return errors.Join(observer.Close(), providers.Close())
	}); err != nil {
		return nil, errors.Join(err, observer.Close(), providers.Close())
	}
	observeInput, err := mount.Ports.Input("observe")
	if err != nil {
		return nil, err
	}
	flushInput, err := mount.Ports.Input("flush")
	if err != nil {
		return nil, err
	}
	cancelInput, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	observationsOutput, err := mount.Ports.Output("observations")
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
	return &asrRunner{
		instance: mount.InstanceID, observer: observer, providerReference: config.Provider,
		providerDescriptor: entry.descriptor, primeProvider: providers.Prime,
		observeInput: observeInput, flushInput: flushInput,
		cancelInput: cancelInput, observationsOutput: observationsOutput,
		outcomeOutput: outcomeOutput, resolvedOutput: resolvedOutput,
		resolution: mount.Resolution,
	}, nil
}

func decodeASRConfig(source json.RawMessage) (ASRConfig, error) {
	var config ASRConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return ASRConfig{}, err
	}
	if strings.TrimSpace(config.Provider) == "" {
		return ASRConfig{}, errors.New("ASR config requires a provider reference")
	}
	return config, nil
}

// primedASRProviderFactory validates one live instance in the supervised Run
// phase, then hands that same instance to the first utterance. This avoids a
// throwaway network session while ensuring the resolved state is live evidence
// rather than only a deployment assertion.
type primedASRProviderFactory struct {
	mu         sync.Mutex
	reference  string
	descriptor v1.Descriptor
	factory    ASRProviderFactory
	primed     v1.PerceptionProvider
}

func (factory *primedASRProviderFactory) create() (v1.PerceptionProvider, error) {
	provider, err := factory.factory()
	if err != nil {
		return nil, err
	}
	if provider == nil || reflectedNil(provider) {
		return nil, fmt.Errorf("ASR provider %q factory returned nil", factory.reference)
	}
	actual := provider.Descriptor()
	if err := actual.Validate(); err != nil {
		closeProvider(provider)
		return nil, fmt.Errorf("ASR provider %q returned an invalid descriptor: %w", factory.reference, err)
	}
	if !reflect.DeepEqual(actual, factory.descriptor) {
		closeProvider(provider)
		return nil, fmt.Errorf("ASR provider %q descriptor drifted: registered %+v, live %+v",
			factory.reference, factory.descriptor, actual)
	}
	return provider, nil
}

func (factory *primedASRProviderFactory) Prime() error {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if factory.primed != nil {
		return nil
	}
	provider, err := factory.create()
	if err != nil {
		return err
	}
	factory.primed = provider
	return nil
}

func (factory *primedASRProviderFactory) New() (v1.PerceptionProvider, error) {
	factory.mu.Lock()
	if factory.primed != nil {
		provider := factory.primed
		factory.primed = nil
		factory.mu.Unlock()
		return provider, nil
	}
	factory.mu.Unlock()
	return factory.create()
}

func (factory *primedASRProviderFactory) Close() error {
	factory.mu.Lock()
	provider := factory.primed
	factory.primed = nil
	factory.mu.Unlock()
	return closeProvider(provider)
}

func closeProvider(provider v1.PerceptionProvider) error {
	if closer, ok := provider.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func reflectedNil(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type asrCommandKind string

const (
	commandObserve asrCommandKind = "observe"
	commandFlush   asrCommandKind = "flush"
)

type asrCommand struct {
	kind         asrCommandKind
	envelope     element.Envelope
	payloadValid bool
	batch        AudioBatch
	flush        Flush
}

type asrOperationResult struct {
	command      asrCommand
	observations []coreperception.Observation
	err          error
}

var errASRCanceled = errors.New("ASR operation canceled")

type asrRunner struct {
	instance           string
	observer           *coreperception.AudioObserver
	providerReference  string
	providerDescriptor v1.Descriptor
	primeProvider      func() error
	observeInput       element.InputPort
	flushInput         element.InputPort
	cancelInput        element.InputPort
	observationsOutput element.OutputPort
	outcomeOutput      element.OutputPort
	resolvedOutput     element.OutputPort
	resolution         element.ResolutionReporter
	currentStream      string
}

func (runner *asrRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := runner.primeProvider(); err != nil {
		return fmt.Errorf("resolve ASR provider %q: %w", runner.providerReference, err)
	}
	if err := reportASRLiveResolution(runner.resolution, runner.providerDescriptor); err != nil {
		return fmt.Errorf("attest ASR provider %q: %w", runner.providerReference, err)
	}
	if err := runner.publishResolution(ctx); err != nil {
		return err
	}
	work := make(chan asrCommand)
	interrupts := make(chan element.Envelope)
	receiveErrors := make(chan error, 3)
	var receivers sync.WaitGroup
	receivers.Add(3)
	go receiveASRWork(ctx, commandObserve, runner.observeInput, work, receiveErrors, &receivers)
	go receiveASRWork(ctx, commandFlush, runner.flushInput, work, receiveErrors, &receivers)
	go receiveASRInterrupts(ctx, runner.cancelInput, interrupts, receiveErrors, &receivers)
	defer func() {
		cancel(nil)
		receivers.Wait()
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-receiveErrors:
			return err
		case interrupt := <-interrupts:
			if err := runner.cancelIdle(ctx, interrupt); err != nil {
				return err
			}
		case command := <-work:
			prepared, outcome := runner.prepare(command)
			if outcome != nil {
				if err := runner.publishOutcome(ctx, command.envelope, *outcome); err != nil {
					return err
				}
				continue
			}
			if err := runner.runOperation(ctx, prepared, interrupts, receiveErrors); err != nil {
				return err
			}
		}
	}
}

func (runner *asrRunner) prepare(command asrCommand) (asrCommand, *Outcome) {
	streamID := commandStreamID(command)
	if !command.payloadValid {
		outcome := Outcome{
			Kind: OutcomeRefused, Operation: string(command.kind), StreamID: streamID,
			Code:    "invalid_payload",
			Message: fmt.Sprintf("%s payload has type %T", command.kind, command.envelope.Payload),
		}
		return command, &outcome
	}
	if streamID == "" && command.kind == commandObserve {
		outcome := Outcome{
			Kind: OutcomeRefused, Operation: string(command.kind), Code: "missing_stream_id",
			Message: "audio observation requires a stream ID",
		}
		return command, &outcome
	}
	if command.kind == commandObserve {
		if len(command.batch.Frames) == 0 {
			outcome := Outcome{
				Kind: OutcomeRefused, Operation: string(command.kind), StreamID: streamID,
				Code: "empty_batch", Message: "audio frame batch is empty",
			}
			return command, &outcome
		}
		for index, frame := range command.batch.Frames {
			if err := frame.Validate(); err != nil {
				outcome := Outcome{
					Kind: OutcomeRefused, Operation: string(command.kind), StreamID: streamID,
					Code: "invalid_frame", Message: fmt.Sprintf("frame %d: %v", index, err),
				}
				return command, &outcome
			}
			if !runner.observer.Accepts(frame) {
				outcome := Outcome{
					Kind: OutcomeRefused, Operation: string(command.kind), StreamID: streamID,
					Code:    "source_not_accepted",
					Message: fmt.Sprintf("frame %d source %q is not accepted", index, frame.Source),
				}
				return command, &outcome
			}
		}
	}
	if runner.currentStream != "" && streamID != "" && runner.currentStream != streamID {
		outcome := Outcome{
			Kind: OutcomeRefused, Operation: string(command.kind), StreamID: streamID,
			Code:    "stream_in_progress",
			Message: fmt.Sprintf("stream %q is active; flush or cancel it before %q", runner.currentStream, streamID),
		}
		return command, &outcome
	}
	if runner.currentStream == "" && streamID != "" {
		runner.currentStream = streamID
	}
	if command.kind == commandFlush && streamID == "" {
		command.flush.StreamID = runner.currentStream
	}
	return command, nil
}

func (runner *asrRunner) runOperation(
	ctx context.Context, command asrCommand, interrupts <-chan element.Envelope,
	receiveErrors <-chan error,
) error {
	operationCtx, stop := context.WithCancelCause(ctx)
	resultChannel := make(chan asrOperationResult, 1)
	go func() {
		observations, err := runner.execute(operationCtx, command)
		resultChannel <- asrOperationResult{command: command, observations: observations, err: err}
	}()
	var canceled *element.Envelope
	for {
		select {
		case <-ctx.Done():
			stop(context.Cause(ctx))
			<-resultChannel
			return nil
		case err := <-receiveErrors:
			stop(err)
			<-resultChannel
			return err
		case interrupt := <-interrupts:
			cancelRequest, valid := cancelPayload(interrupt.Payload)
			if !valid {
				if err := runner.publishOutcome(ctx, interrupt, invalidCancelOutcome(interrupt)); err != nil {
					stop(err)
					<-resultChannel
					return err
				}
				continue
			}
			requested := firstNonempty(cancelRequest.StreamID, envelopeScope(interrupt))
			active := commandStreamID(command)
			if active == "" {
				active = runner.currentStream
			}
			if requested != "" && requested != active {
				if err := runner.publishOutcome(ctx, interrupt, Outcome{
					Kind: OutcomeIgnored, Operation: "cancel", StreamID: requested,
					Code: "scope_not_active", Message: fmt.Sprintf("active stream is %q", active),
				}); err != nil {
					stop(err)
					<-resultChannel
					return err
				}
				continue
			}
			copy := interrupt
			canceled = &copy
			stop(fmt.Errorf("%w: %s", errASRCanceled, cancelRequest.Reason))
		case result := <-resultChannel:
			stop(nil)
			return runner.completeOperation(ctx, result, canceled != nil)
		}
	}
}

func (runner *asrRunner) execute(ctx context.Context, command asrCommand) ([]coreperception.Observation, error) {
	switch command.kind {
	case commandObserve:
		if len(command.batch.Frames) == 0 {
			return nil, errors.New("audio frame batch is empty")
		}
		return runner.observer.Observe(ctx, command.batch.Frames)
	case commandFlush:
		return runner.observer.Flush(ctx)
	default:
		return nil, fmt.Errorf("unknown ASR command %q", command.kind)
	}
}

func (runner *asrRunner) completeOperation(
	ctx context.Context, result asrOperationResult, canceled bool,
) error {
	for index, observation := range result.observations {
		if err := runner.publishObservation(ctx, result.command.envelope, observation, index); err != nil {
			return err
		}
	}
	streamID := commandStreamID(result.command)
	if streamID == "" {
		streamID = runner.currentStream
	}
	outcome := Outcome{
		Kind: OutcomeSucceeded, Operation: string(result.command.kind), StreamID: streamID,
		ObservationCount: len(result.observations),
	}
	if canceled || errors.Is(result.err, errASRCanceled) || errors.Is(result.err, context.Canceled) {
		outcome.Kind, outcome.Code = OutcomeCanceled, "canceled"
		if result.err != nil {
			outcome.Message = result.err.Error()
		}
	} else if result.err != nil {
		outcome.Kind, outcome.Code, outcome.Message = OutcomeFailed, "provider_error", result.err.Error()
	}
	if result.command.kind == commandFlush || outcome.Kind == OutcomeCanceled || outcome.Kind == OutcomeFailed {
		if closeErr := runner.observer.Close(); closeErr != nil {
			outcome.Kind = OutcomeFailed
			outcome.Code = "provider_close_error"
			outcome.Message = errors.Join(result.err, closeErr).Error()
		}
		runner.currentStream = ""
	}
	return runner.publishOutcome(ctx, result.command.envelope, outcome)
}

func (runner *asrRunner) cancelIdle(ctx context.Context, envelope element.Envelope) error {
	request, valid := cancelPayload(envelope.Payload)
	if !valid {
		return runner.publishOutcome(ctx, envelope, invalidCancelOutcome(envelope))
	}
	requested := firstNonempty(request.StreamID, envelopeScope(envelope))
	if requested != "" && runner.currentStream != "" && requested != runner.currentStream {
		return runner.publishOutcome(ctx, envelope, Outcome{
			Kind: OutcomeIgnored, Operation: "cancel", StreamID: requested,
			Code: "scope_not_active", Message: fmt.Sprintf("active stream is %q", runner.currentStream),
		})
	}
	streamID := runner.currentStream
	if streamID == "" {
		streamID = requested
	}
	if err := runner.observer.Close(); err != nil {
		runner.currentStream = ""
		return runner.publishOutcome(ctx, envelope, Outcome{
			Kind: OutcomeFailed, Operation: "cancel", StreamID: streamID,
			Code: "provider_close_error", Message: err.Error(),
		})
	}
	runner.currentStream = ""
	return runner.publishOutcome(ctx, envelope, Outcome{
		Kind: OutcomeCanceled, Operation: "cancel", StreamID: streamID, Code: "canceled", Message: request.Reason,
	})
}

func (runner *asrRunner) publishResolution(ctx context.Context) error {
	capabilities := make(map[string]bool, len(runner.providerDescriptor.Capabilities))
	for capability, selected := range runner.providerDescriptor.Capabilities {
		capabilities[string(capability)] = selected
	}
	resolution := ProviderResolution{
		Reference: runner.providerReference,
		Descriptor: v1.Descriptor{
			Name: runner.providerDescriptor.Name, Version: runner.providerDescriptor.Version,
			Capabilities: maps.Clone(runner.providerDescriptor.Capabilities),
		},
		Selected: capabilities,
	}
	_, err := runner.resolvedOutput.Broadcast(ctx, element.Envelope{
		Type: providerResolutionType, ItemID: runner.instance + "-resolved", Payload: resolution,
	})
	return err
}

func (runner *asrRunner) publishObservation(
	ctx context.Context, cause element.Envelope, observation coreperception.Observation, index int,
) error {
	envelope := cause.Clone()
	envelope.Type = observationType
	envelope.ItemID = fmt.Sprintf("%s:observation:%d:%d", cause.ItemID, observation.Revision, index)
	if envelope.SourceID == "" {
		envelope.SourceID = observation.Source
	}
	envelope.CaptureNS = observation.OccurredNS
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	observation.Media = slices.Clone(observation.Media)
	envelope.Payload = observation
	_, err := runner.observationsOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *asrRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome Outcome,
) error {
	envelope := cause.Clone()
	envelope.Type = perceptionOutcomeType
	envelope.ItemID = cause.ItemID + ":outcome"
	envelope.SourceID = firstNonempty(outcome.StreamID, envelope.SourceID)
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	_, err := runner.outcomeOutput.Broadcast(ctx, envelope)
	return err
}

func receiveASRWork(
	ctx context.Context, kind asrCommandKind, input element.InputPort,
	output chan<- asrCommand, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			sendReceiverError(ctx, failures, err)
			return
		}
		command := asrCommand{kind: kind, envelope: envelope}
		var valid bool
		switch kind {
		case commandObserve:
			command.batch, valid = audioBatchPayload(envelope.Payload)
		case commandFlush:
			command.flush, valid = flushPayload(envelope.Payload)
		}
		// Keep malformed data on the ordinary outcome path. The event loop
		// owns output ordering; receiver goroutines own only port admission.
		command.payloadValid = valid
		select {
		case output <- command:
		case <-ctx.Done():
			return
		}
	}
}

func receiveASRInterrupts(
	ctx context.Context, input element.InputPort, output chan<- element.Envelope,
	failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			sendReceiverError(ctx, failures, err)
			return
		}
		select {
		case output <- envelope:
		case <-ctx.Done():
			return
		}
	}
}

func sendReceiverError(ctx context.Context, failures chan<- error, err error) {
	select {
	case failures <- err:
	case <-ctx.Done():
	}
}

func audioBatchPayload(payload any) (AudioBatch, bool) {
	var batch AudioBatch
	switch typed := payload.(type) {
	case AudioBatch:
		batch = typed
	case *AudioBatch:
		if typed == nil {
			return AudioBatch{}, false
		}
		batch = *typed
	default:
		return AudioBatch{}, false
	}
	batch.Frames = slices.Clone(batch.Frames)
	for index := range batch.Frames {
		batch.Frames[index].PCM16LE = slices.Clone(batch.Frames[index].PCM16LE)
		batch.Frames[index].Image = slices.Clone(batch.Frames[index].Image)
	}
	return batch, true
}

func flushPayload(payload any) (Flush, bool) {
	switch typed := payload.(type) {
	case nil:
		return Flush{}, true
	case Flush:
		return typed, true
	case *Flush:
		if typed == nil {
			return Flush{}, false
		}
		return *typed, true
	default:
		return Flush{}, false
	}
}

func cancelPayload(payload any) (Cancel, bool) {
	switch typed := payload.(type) {
	case nil:
		return Cancel{}, true
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

func commandStreamID(command asrCommand) string {
	if command.kind == commandObserve {
		return firstNonempty(command.batch.StreamID, envelopeScope(command.envelope))
	}
	return firstNonempty(command.flush.StreamID, envelopeScope(command.envelope))
}

func envelopeScope(envelope element.Envelope) string {
	return firstNonempty(envelope.CancellationScope, envelope.SourceID, envelope.RunID)
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func appendUnique(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func invalidCancelOutcome(envelope element.Envelope) Outcome {
	return Outcome{
		Kind: OutcomeRefused, Operation: "cancel", StreamID: envelopeScope(envelope),
		Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload),
	}
}

func terminal(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}

func Descriptors() []element.Descriptor {
	return []element.Descriptor{ASRDescriptor(), VisualObserverDescriptor()}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register perception descriptors: nil catalog")
	}
	for _, descriptor := range Descriptors() {
		if err := catalog.Register(descriptor); err != nil {
			return err
		}
	}
	return nil
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if registry == nil {
		return errors.New("register perception factories: nil registry")
	}
	registrations, err := FactoryRegistrations()
	if err != nil {
		return err
	}
	for _, registration := range registrations {
		if err := registry.RegisterFactory(registration); err != nil {
			return err
		}
	}
	return nil
}

func FactoryRegistrations() ([]graphruntime.FactoryRegistration, error) {
	return factoryprofile.Registrations(
		factoryprofile.Entry{Factory: asrFactory{}, Artifact: inspect.ArtifactIdentity{
			ID: asrRuntimeID, Revision: perceptionImplementationRevision,
		}},
		factoryprofile.Entry{Factory: visualObserverFactory{}, Artifact: inspect.ArtifactIdentity{
			ID: visualRuntimeID, Revision: perceptionImplementationRevision,
		}},
	)
}
