package perception

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	coreperception "github.com/bojieli/OpenRealtime/perception"
)

const (
	VisualProviderRegistryService = "perception.visual.providers"
	MediaRetainerService          = "perception.media.retainer"
)

var (
	imageBatchType       = element.Trigger(element.Named("image.FrameBatch"))
	visualRefreshType    = element.Trigger(element.Named("image.Refresh"))
	visualCloseType      = element.Trigger(element.Named("image.SourceClose"))
	visualCancelType     = element.Interrupt(element.Named("image.StreamID"))
	visualOutcomeType    = element.Event(element.Named("perception.VisualOutcome"))
	visualResolutionType = element.State(element.Named("perception.VisualProviderResolution"))
	visualMetricsType    = element.State(element.Named("perception.VisualMetrics"))
)

// VisualObserverDescriptor exposes visual admission as an ordinary graph
// reaction. The node performs change detection and narration, but it has no
// cadence setting: timers, sampling, and burst collapse belong in upstream
// elements where they remain visible and independently replaceable.
func VisualObserverDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "perception.VisualObserver",
		Revision:      1,
		Ports: []element.Port{
			{Name: "observe", Direction: element.Input, Type: imageBatchType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 2},
			{Name: "refresh", Direction: element.Input, Type: visualRefreshType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "close", Direction: element.Input, Type: visualCloseType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "cancel", Direction: element.Input, Type: visualCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "observations", Direction: element.Output, Type: observationType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: visualOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "resolved", Direction: element.Output, Type: visualResolutionType,
				Cardinality: element.One, Required: true, DefaultDepth: 1},
			{Name: "metrics", Direction: element.Output, Type: visualMetricsType,
				Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{"observe", "refresh", "close"}, Interrupts: []string{"cancel"},
			Outcomes:       []string{"observations", "outcome", "resolved", "metrics"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/perception/visual-observer-state/v1",
		ConfigSchema: "schema://openrealtime/perception/visual-observer-config/v1",
		Dependencies: []element.Dependency{
			{Name: VisualProviderRegistryService},
			{Name: MediaRetainerService, Optional: true},
		},
		Effects: []element.Effect{
			{Name: "perception.visual.session", Reversible: true},
			{Name: "perception.media.retention", Reversible: true},
		},
	}
}

type VisualObserverConfig struct {
	Provider          string  `json:"provider"`
	Source            string  `json:"source"`
	Name              string  `json:"name,omitempty"`
	ChangeThreshold   float64 `json:"change_threshold,omitempty"`
	AttachKeyframes   bool    `json:"attach_keyframes,omitempty"`
	MaxFramesPerBatch int     `json:"max_frames_per_batch,omitempty"`
	MaxFrameBytes     int     `json:"max_frame_bytes,omitempty"`
}

type ImageBatch struct {
	StreamID string                 `json:"stream_id"`
	Frames   []coreperception.Frame `json:"frames"`
}

type VisualRefresh struct {
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type VisualSourceClose struct {
	Source string `json:"source,omitempty"`
}

type VisualCancel struct {
	StreamID string `json:"stream_id,omitempty"`
	Source   string `json:"source,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type VisualOutcomeKind string

const (
	VisualSucceeded VisualOutcomeKind = "succeeded"
	VisualCanceled  VisualOutcomeKind = "canceled"
	VisualRefused   VisualOutcomeKind = "refused"
	VisualFailed    VisualOutcomeKind = "failed"
	VisualIgnored   VisualOutcomeKind = "ignored"
)

type VisualOutcome struct {
	Kind             VisualOutcomeKind `json:"kind"`
	Operation        string            `json:"operation"`
	Source           string            `json:"source,omitempty"`
	StreamID         string            `json:"stream_id,omitempty"`
	ObservationCount int               `json:"observation_count,omitempty"`
	Code             string            `json:"code,omitempty"`
	Message          string            `json:"message,omitempty"`
}

// VisualProviderDescriptor is the immutable deployment identity of the
// narrator selected by a visual node. Name is also verified against the live
// instance before the node reports ready.
type VisualProviderDescriptor struct {
	Name     string `json:"name"`
	Revision string `json:"revision"`
	Digest   string `json:"digest,omitempty"`
}

type VisualProviderResolution struct {
	Reference        string                   `json:"reference"`
	Descriptor       VisualProviderDescriptor `json:"descriptor"`
	RegistryRevision uint64                   `json:"registry_revision"`
}

type VisualMetrics struct {
	Source     string `json:"source"`
	Frames     uint64 `json:"frames"`
	Admitted   uint64 `json:"admitted"`
	Narrations uint64 `json:"narrations"`
}

type VisualProviderFactory func() (coreperception.Narrator, error)

type visualProviderEntry struct {
	descriptor VisualProviderDescriptor
	factory    VisualProviderFactory
}

// VisualProviderRegistry is deployment state. Graph topology selects only a
// symbolic reference; the live descriptor is checked when the node starts.
type VisualProviderRegistry struct {
	mu      sync.RWMutex
	entries map[string]visualProviderEntry
}

func NewVisualProviderRegistry() *VisualProviderRegistry {
	return &VisualProviderRegistry{entries: make(map[string]visualProviderEntry)}
}

func (registry *VisualProviderRegistry) Register(
	reference string, descriptor VisualProviderDescriptor, factory VisualProviderFactory,
) error {
	if registry == nil {
		return errors.New("register visual provider: nil registry")
	}
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return errors.New("register visual provider: empty reference")
	}
	if err := descriptor.validate(); err != nil {
		return fmt.Errorf("register visual provider %q: %w", reference, err)
	}
	if factory == nil {
		return fmt.Errorf("register visual provider %q: nil factory", reference)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = make(map[string]visualProviderEntry)
	}
	if _, duplicate := registry.entries[reference]; duplicate {
		return fmt.Errorf("visual provider %q is already registered", reference)
	}
	registry.entries[reference] = visualProviderEntry{descriptor: descriptor, factory: factory}
	return nil
}

func (registry *VisualProviderRegistry) resolve(reference string) (visualProviderEntry, error) {
	if registry == nil {
		return visualProviderEntry{}, errors.New("visual provider registry is nil")
	}
	registry.mu.RLock()
	entry, found := registry.entries[strings.TrimSpace(reference)]
	registry.mu.RUnlock()
	if !found {
		return visualProviderEntry{}, fmt.Errorf("visual provider %q is not registered", reference)
	}
	return entry, nil
}

func (descriptor VisualProviderDescriptor) validate() error {
	if descriptor.Name == "" || descriptor.Name != strings.TrimSpace(descriptor.Name) {
		return errors.New("visual provider descriptor requires a name")
	}
	if descriptor.Revision != strings.TrimSpace(descriptor.Revision) ||
		descriptor.Digest != strings.TrimSpace(descriptor.Digest) {
		return errors.New("visual provider descriptor identity is not canonical")
	}
	if descriptor.Revision == "" && descriptor.Digest == "" {
		return errors.New("visual provider descriptor requires a revision or digest")
	}
	if descriptor.Digest != "" {
		const prefix = "sha256:"
		if !strings.HasPrefix(descriptor.Digest, prefix) ||
			len(descriptor.Digest) != len(prefix)+sha256.Size*2 {
			return errors.New("visual provider descriptor has an invalid SHA-256 digest")
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(descriptor.Digest, prefix)); err != nil {
			return fmt.Errorf("visual provider descriptor has an invalid SHA-256 digest: %w", err)
		}
	}
	return nil
}

type visualObserverFactory struct{}

var (
	_ element.Factory         = visualObserverFactory{}
	_ element.ConfigValidator = visualObserverFactory{}
)

func (visualObserverFactory) Descriptor() element.Descriptor { return VisualObserverDescriptor() }

func (visualObserverFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeVisualObserverConfig(source)
	return err
}

func (visualObserverFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeVisualObserverConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("perception.VisualObserver %s config: %w", mount.InstanceID, err)
	}
	service, registryRevision, found := mount.Services.Lookup(VisualProviderRegistryService)
	if !found {
		return nil, fmt.Errorf("perception.VisualObserver %s has no provider registry service", mount.InstanceID)
	}
	registry, ok := service.(*VisualProviderRegistry)
	if !ok || registry == nil {
		return nil, fmt.Errorf("visual provider registry service has type %T", service)
	}
	entry, err := registry.resolve(config.Provider)
	if err != nil {
		return nil, err
	}
	narrator, err := entry.factory()
	if err != nil {
		return nil, fmt.Errorf("create visual provider %q: %w", config.Provider, err)
	}
	if narrator == nil || reflectedNil(narrator) {
		return nil, fmt.Errorf("visual provider %q factory returned nil", config.Provider)
	}
	if narrator.Name() != entry.descriptor.Name {
		_ = closeNarrator(narrator)
		return nil, fmt.Errorf("visual provider %q identity drifted: registered %q, live %q",
			config.Provider, entry.descriptor.Name, narrator.Name())
	}
	var retainer coreperception.Retainer
	if config.AttachKeyframes {
		retainerService, _, present := mount.Services.Lookup(MediaRetainerService)
		if !present {
			_ = closeNarrator(narrator)
			return nil, errors.New("visual observer keyframe attachment requires a media retainer service")
		}
		var valid bool
		retainer, valid = retainerService.(coreperception.Retainer)
		if !valid || retainer == nil || reflectedNil(retainer) {
			_ = closeNarrator(narrator)
			return nil, fmt.Errorf("media retainer service has type %T", retainerService)
		}
	}
	observer, err := coreperception.NewVideoObserver(coreperception.VideoConfig{
		Name: config.Name, Sources: []string{config.Source}, ExternalCadence: true,
		ChangeThreshold: config.ChangeThreshold, Narrator: narrator,
		Retainer: retainer, AttachKeyframes: config.AttachKeyframes,
	})
	if err != nil {
		_ = closeNarrator(narrator)
		return nil, err
	}
	if err := mount.Lifecycle.Defer("close-visual-provider", func(context.Context) error {
		observer.Reset()
		return closeNarrator(narrator)
	}); err != nil {
		observer.Reset()
		return nil, errors.Join(err, closeNarrator(narrator))
	}
	ports, err := visualPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &visualObserverRunner{
		instance: mount.InstanceID, config: config, observer: observer,
		providerReference: config.Provider, providerDescriptor: entry.descriptor,
		registryRevision: registryRevision, ports: ports,
	}, nil
}

func decodeVisualObserverConfig(source json.RawMessage) (VisualObserverConfig, error) {
	config := VisualObserverConfig{MaxFramesPerBatch: 8, MaxFrameBytes: 8 << 20}
	if err := elementconfig.Decode(source, &config); err != nil {
		return VisualObserverConfig{}, err
	}
	config.Provider = strings.TrimSpace(config.Provider)
	config.Source = strings.TrimSpace(config.Source)
	config.Name = strings.TrimSpace(config.Name)
	if config.Provider == "" {
		return VisualObserverConfig{}, errors.New("visual observer requires a provider reference")
	}
	if config.Source == "" {
		return VisualObserverConfig{}, errors.New("visual observer requires exactly one source")
	}
	if config.Name == "" {
		config.Name = "visual-" + config.Source
	}
	if config.ChangeThreshold < 0 || config.ChangeThreshold > 1 {
		return VisualObserverConfig{}, errors.New("visual change threshold must be between zero and one")
	}
	if config.MaxFramesPerBatch <= 0 || config.MaxFramesPerBatch > 256 {
		return VisualObserverConfig{}, errors.New("visual max_frames_per_batch must be between 1 and 256")
	}
	if config.MaxFrameBytes <= 0 || config.MaxFrameBytes > 64<<20 {
		return VisualObserverConfig{}, errors.New("visual max_frame_bytes must be between 1 and 67108864")
	}
	return config, nil
}

type visualPorts struct {
	observe, refresh, close, cancel          element.InputPort
	observations, outcome, resolved, metrics element.OutputPort
}

func visualPortsFrom(ports element.Ports) (visualPorts, error) {
	var result visualPorts
	var err error
	if result.observe, err = ports.Input("observe"); err != nil {
		return result, err
	}
	if result.refresh, err = ports.Input("refresh"); err != nil {
		return result, err
	}
	if result.close, err = ports.Input("close"); err != nil {
		return result, err
	}
	if result.cancel, err = ports.Input("cancel"); err != nil {
		return result, err
	}
	if result.observations, err = ports.Output("observations"); err != nil {
		return result, err
	}
	if result.outcome, err = ports.Output("outcome"); err != nil {
		return result, err
	}
	if result.resolved, err = ports.Output("resolved"); err != nil {
		return result, err
	}
	if result.metrics, err = ports.Output("metrics"); err != nil {
		return result, err
	}
	return result, nil
}

type visualCommandKind string

const (
	visualObserve        visualCommandKind = "observe"
	visualRefreshCommand visualCommandKind = "refresh"
	visualCloseCommand   visualCommandKind = "close"
)

type visualCommand struct {
	kind     visualCommandKind
	envelope element.Envelope
}

type visualOperationResult struct {
	observations []coreperception.Observation
	err          error
}

type visualObserverRunner struct {
	instance           string
	config             VisualObserverConfig
	observer           *coreperception.VideoObserver
	providerReference  string
	providerDescriptor VisualProviderDescriptor
	registryRevision   uint64
	ports              visualPorts
	activeStream       string
}

func (runner *visualObserverRunner) Run(parent context.Context) error {
	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	if err := runner.publishResolution(ctx); err != nil {
		return err
	}
	if err := runner.publishMetrics(ctx, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}
	commands := make(chan visualCommand)
	interrupts := make(chan element.Envelope)
	failures := make(chan error, 4)
	var receivers sync.WaitGroup
	for _, input := range []struct {
		kind visualCommandKind
		port element.InputPort
	}{
		{visualObserve, runner.ports.observe}, {visualRefreshCommand, runner.ports.refresh},
		{visualCloseCommand, runner.ports.close},
	} {
		receivers.Add(1)
		go receiveVisualCommands(ctx, input.kind, input.port, commands, failures, &receivers)
	}
	receivers.Add(1)
	go receiveVisualInterrupts(ctx, runner.ports.cancel, interrupts, failures, &receivers)
	defer func() { stop(nil); receivers.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case interrupt := <-interrupts:
			if err := runner.cancelIdle(ctx, interrupt); err != nil {
				return err
			}
		case command := <-commands:
			if err := runner.handle(ctx, command, interrupts, failures); err != nil {
				return err
			}
		}
	}
}

func (runner *visualObserverRunner) handle(
	ctx context.Context, command visualCommand, interrupts <-chan element.Envelope,
	failures <-chan error,
) error {
	switch command.kind {
	case visualRefreshCommand:
		request, ok := visualRefreshPayload(command.envelope.Payload)
		if !ok {
			return runner.publishOutcome(ctx, command.envelope, runner.refusal(command, "invalid_payload", fmt.Sprintf("refresh payload has type %T", command.envelope.Payload)))
		}
		if request.Source != "" && request.Source != runner.config.Source {
			return runner.publishOutcome(ctx, command.envelope, runner.ignored(command, "source_not_selected", request.Source))
		}
		runner.observer.RefreshNext()
		return runner.publishOutcome(ctx, command.envelope, runner.success(command, 0))
	case visualCloseCommand:
		request, ok := visualClosePayload(command.envelope.Payload)
		if !ok {
			return runner.publishOutcome(ctx, command.envelope, runner.refusal(command, "invalid_payload", fmt.Sprintf("close payload has type %T", command.envelope.Payload)))
		}
		if request.Source != "" && request.Source != runner.config.Source {
			return runner.publishOutcome(ctx, command.envelope, runner.ignored(command, "source_not_selected", request.Source))
		}
		observations, err := runner.observer.Flush(ctx)
		if err != nil {
			return runner.publishOutcome(ctx, command.envelope, runner.failure(command, "provider_error", err))
		}
		if err := runner.publishObservations(ctx, command.envelope, observations); err != nil {
			return err
		}
		runner.observer.Reset()
		runner.activeStream = ""
		if err := runner.publishMetrics(ctx, command.envelope); err != nil {
			return err
		}
		return runner.publishOutcome(ctx, command.envelope, runner.success(command, len(observations)))
	case visualObserve:
		return runner.observe(ctx, command, interrupts, failures)
	default:
		return fmt.Errorf("unknown visual command %q", command.kind)
	}
}

func (runner *visualObserverRunner) observe(
	ctx context.Context, command visualCommand, interrupts <-chan element.Envelope,
	failures <-chan error,
) error {
	batch, ok := imageBatchPayload(command.envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, command.envelope, runner.refusal(command, "invalid_payload", fmt.Sprintf("observe payload has type %T", command.envelope.Payload)))
	}
	streamID := firstNonempty(batch.StreamID, envelopeScope(command.envelope))
	if streamID == "" {
		return runner.publishOutcome(ctx, command.envelope, runner.refusal(command, "missing_stream_id", "visual observation requires a stream ID"))
	}
	if runner.activeStream != "" && runner.activeStream != streamID {
		return runner.publishOutcome(ctx, command.envelope, runner.refusal(command, "stream_in_progress", fmt.Sprintf("stream %q is active", runner.activeStream)))
	}
	if len(batch.Frames) == 0 || len(batch.Frames) > runner.config.MaxFramesPerBatch {
		return runner.publishOutcome(ctx, command.envelope, runner.refusal(command, "invalid_batch_size", fmt.Sprintf("visual batch has %d frames; limit is %d", len(batch.Frames), runner.config.MaxFramesPerBatch)))
	}
	for index, frame := range batch.Frames {
		if err := frame.Validate(); err != nil {
			return runner.publishOutcome(ctx, command.envelope, runner.refusal(command, "invalid_frame", fmt.Sprintf("frame %d: %v", index, err)))
		}
		if frame.Kind != coreperception.FrameImage || frame.Source != runner.config.Source {
			return runner.publishOutcome(ctx, command.envelope, runner.refusal(command, "source_not_selected", fmt.Sprintf("frame %d source %q is not %q", index, frame.Source, runner.config.Source)))
		}
		if len(frame.Image) > runner.config.MaxFrameBytes {
			return runner.publishOutcome(ctx, command.envelope, runner.refusal(command, "frame_too_large", fmt.Sprintf("frame %d has %d bytes; limit is %d", index, len(frame.Image), runner.config.MaxFrameBytes)))
		}
	}
	runner.activeStream = streamID
	admitted := batch.Frames[len(batch.Frames)-1]
	if !runner.observer.Gate(admitted) {
		if err := runner.publishMetrics(ctx, command.envelope); err != nil {
			return err
		}
		return runner.publishOutcome(ctx, command.envelope, runner.success(command, 0))
	}
	operationCtx, cancel := context.WithCancelCause(ctx)
	results := make(chan visualOperationResult, 1)
	go func() {
		observations, err := runner.observer.Observe(operationCtx, batch.Frames)
		results <- visualOperationResult{observations: observations, err: err}
	}()
	var interrupt *element.Envelope
	for {
		select {
		case <-ctx.Done():
			cancel(context.Cause(ctx))
			<-results
			return nil
		case err := <-failures:
			cancel(err)
			<-results
			return err
		case candidate := <-interrupts:
			request, valid := visualCancelPayload(candidate.Payload)
			if !valid {
				if err := runner.publishOutcome(ctx, candidate, VisualOutcome{Kind: VisualRefused, Operation: "cancel", Source: runner.config.Source, Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", candidate.Payload)}); err != nil {
					cancel(err)
					<-results
					return err
				}
				continue
			}
			requestedStream := firstNonempty(request.StreamID, envelopeScope(candidate))
			if (request.Source != "" && request.Source != runner.config.Source) || (requestedStream != "" && requestedStream != streamID) {
				if err := runner.publishOutcome(ctx, candidate, VisualOutcome{Kind: VisualIgnored, Operation: "cancel", Source: runner.config.Source, StreamID: requestedStream, Code: "scope_not_active"}); err != nil {
					cancel(err)
					<-results
					return err
				}
				continue
			}
			copy := candidate.Clone()
			interrupt = &copy
			cancel(errors.New("visual observation canceled: " + request.Reason))
		case result := <-results:
			cancel(nil)
			if interrupt != nil || errors.Is(result.err, context.Canceled) {
				runner.observer.Reset()
				runner.activeStream = ""
				if err := runner.publishMetrics(ctx, command.envelope); err != nil {
					return err
				}
				if err := runner.publishOutcome(ctx, command.envelope, VisualOutcome{Kind: VisualCanceled, Operation: "observe", Source: runner.config.Source, StreamID: streamID, Code: "canceled"}); err != nil {
					return err
				}
				if interrupt != nil {
					return runner.publishOutcome(ctx, *interrupt, VisualOutcome{Kind: VisualSucceeded, Operation: "cancel", Source: runner.config.Source, StreamID: streamID})
				}
				return nil
			}
			if result.err != nil {
				runner.observer.Reset()
				runner.activeStream = ""
				if err := runner.publishMetrics(ctx, command.envelope); err != nil {
					return err
				}
				return runner.publishOutcome(ctx, command.envelope, runner.failure(command, "provider_error", result.err))
			}
			if err := runner.publishObservations(ctx, command.envelope, result.observations); err != nil {
				return err
			}
			if err := runner.publishMetrics(ctx, command.envelope); err != nil {
				return err
			}
			return runner.publishOutcome(ctx, command.envelope, runner.success(command, len(result.observations)))
		}
	}
}

func (runner *visualObserverRunner) cancelIdle(ctx context.Context, envelope element.Envelope) error {
	request, ok := visualCancelPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, VisualOutcome{Kind: VisualRefused, Operation: "cancel", Source: runner.config.Source, Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload)})
	}
	requestedStream := firstNonempty(request.StreamID, envelopeScope(envelope))
	if (request.Source != "" && request.Source != runner.config.Source) || (requestedStream != "" && runner.activeStream != "" && requestedStream != runner.activeStream) {
		return runner.publishOutcome(ctx, envelope, VisualOutcome{Kind: VisualIgnored, Operation: "cancel", Source: runner.config.Source, StreamID: requestedStream, Code: "scope_not_active"})
	}
	runner.observer.Reset()
	runner.activeStream = ""
	if err := runner.publishMetrics(ctx, envelope); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, VisualOutcome{Kind: VisualSucceeded, Operation: "cancel", Source: runner.config.Source, StreamID: requestedStream})
}

func (runner *visualObserverRunner) publishResolution(ctx context.Context) error {
	_, err := runner.ports.resolved.Broadcast(ctx, element.Envelope{
		Type: visualResolutionType, ItemID: runner.instance + ":resolved",
		Payload: VisualProviderResolution{Reference: runner.providerReference, Descriptor: runner.providerDescriptor, RegistryRevision: runner.registryRevision},
	})
	return err
}

func (runner *visualObserverRunner) publishMetrics(ctx context.Context, cause element.Envelope) error {
	metrics := runner.observer.Metrics()
	itemID := cause.ItemID + ":visual-metrics"
	if cause.ItemID == "" {
		itemID = runner.instance + ":visual-metrics"
	}
	_, err := runner.ports.metrics.Broadcast(ctx, element.Envelope{
		Type: visualMetricsType, ItemID: itemID, SourceID: runner.config.Source,
		CausalParents: appendUnique(nil, cause.ItemID),
		Payload:       VisualMetrics{Source: runner.config.Source, Frames: metrics.Frames, Admitted: metrics.Admitted, Narrations: metrics.Narrations},
	})
	return err
}

func (runner *visualObserverRunner) publishObservations(ctx context.Context, cause element.Envelope, observations []coreperception.Observation) error {
	for index, observation := range observations {
		envelope := cause.Clone()
		envelope.Type = observationType
		envelope.ItemID = fmt.Sprintf("%s:observation:%d:%d", cause.ItemID, observation.Revision, index)
		envelope.SourceID = runner.config.Source
		envelope.CaptureNS = observation.OccurredNS
		envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
		observation.Media = slices.Clone(observation.Media)
		envelope.Payload = observation
		if _, err := runner.ports.observations.Broadcast(ctx, envelope); err != nil {
			return err
		}
	}
	return nil
}

func (runner *visualObserverRunner) publishOutcome(ctx context.Context, cause element.Envelope, outcome VisualOutcome) error {
	envelope := cause.Clone()
	envelope.Type = visualOutcomeType
	envelope.ItemID = cause.ItemID + ":outcome"
	envelope.SourceID = runner.config.Source
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	_, err := runner.ports.outcome.Broadcast(ctx, envelope)
	return err
}

func (runner *visualObserverRunner) success(command visualCommand, count int) VisualOutcome {
	return VisualOutcome{Kind: VisualSucceeded, Operation: string(command.kind), Source: runner.config.Source, StreamID: firstNonempty(envelopeScope(command.envelope), runner.activeStream), ObservationCount: count}
}
func (runner *visualObserverRunner) refusal(command visualCommand, code, message string) VisualOutcome {
	return VisualOutcome{Kind: VisualRefused, Operation: string(command.kind), Source: runner.config.Source, StreamID: envelopeScope(command.envelope), Code: code, Message: message}
}
func (runner *visualObserverRunner) failure(command visualCommand, code string, err error) VisualOutcome {
	return VisualOutcome{Kind: VisualFailed, Operation: string(command.kind), Source: runner.config.Source, StreamID: runner.activeStream, Code: code, Message: err.Error()}
}
func (runner *visualObserverRunner) ignored(command visualCommand, code, selected string) VisualOutcome {
	return VisualOutcome{Kind: VisualIgnored, Operation: string(command.kind), Source: runner.config.Source, StreamID: envelopeScope(command.envelope), Code: code, Message: fmt.Sprintf("source %q is not selected", selected)}
}

func receiveVisualCommands(ctx context.Context, kind visualCommandKind, input element.InputPort, output chan<- visualCommand, failures chan<- error, wait *sync.WaitGroup) {
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
		case output <- visualCommand{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func receiveVisualInterrupts(ctx context.Context, input element.InputPort, output chan<- element.Envelope, failures chan<- error, wait *sync.WaitGroup) {
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

func imageBatchPayload(payload any) (ImageBatch, bool) {
	var batch ImageBatch
	switch typed := payload.(type) {
	case ImageBatch:
		batch = typed
	case *ImageBatch:
		if typed == nil {
			return ImageBatch{}, false
		}
		batch = *typed
	default:
		return ImageBatch{}, false
	}
	batch.Frames = slices.Clone(batch.Frames)
	for index := range batch.Frames {
		batch.Frames[index].Image = slices.Clone(batch.Frames[index].Image)
		batch.Frames[index].PCM16LE = slices.Clone(batch.Frames[index].PCM16LE)
	}
	return batch, true
}

func visualRefreshPayload(payload any) (VisualRefresh, bool) {
	switch typed := payload.(type) {
	case nil:
		return VisualRefresh{}, true
	case VisualRefresh:
		return typed, true
	case *VisualRefresh:
		if typed != nil {
			return *typed, true
		}
	}
	return VisualRefresh{}, false
}

func visualClosePayload(payload any) (VisualSourceClose, bool) {
	switch typed := payload.(type) {
	case nil:
		return VisualSourceClose{}, true
	case VisualSourceClose:
		return typed, true
	case *VisualSourceClose:
		if typed != nil {
			return *typed, true
		}
	}
	return VisualSourceClose{}, false
}

func visualCancelPayload(payload any) (VisualCancel, bool) {
	switch typed := payload.(type) {
	case nil:
		return VisualCancel{}, true
	case VisualCancel:
		return typed, true
	case *VisualCancel:
		if typed != nil {
			return *typed, true
		}
	}
	return VisualCancel{}, false
}

func closeNarrator(narrator coreperception.Narrator) error {
	if closer, ok := narrator.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

func registerVisualFactory(registry *graphruntime.Registry) error {
	return registry.Register("", visualObserverFactory{})
}
