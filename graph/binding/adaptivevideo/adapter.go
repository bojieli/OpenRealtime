package adaptivevideo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	videograph "github.com/bojieli/OpenRealtime/elements/video"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreperception "github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	sourceBoundary        = "source"
	frameBoundary         = "frame"
	tickBoundary          = "tick"
	policyOutcomeBoundary = "policy_outcome"
	observationsBoundary  = "observations"

	// Realtime video source declarations do not carry a source-open timestamp.
	// One is the earliest valid external-clock value, so capture timestamps
	// remain the sole clock supplied by the client and cadence remains graph
	// policy rather than adapter policy.
	sourceOpenedNS uint64 = 1
)

var observationType = element.Revisions(
	element.Named("perception.Observation"), element.Named("perception.RevisionID"),
)

var auxiliaryOutputs = []struct {
	name     string
	typeName element.Type
}{
	{"reference_observe", videograph.ReferenceBatchType()},
	{"policy_state", videograph.PolicyStateType()},
	{"decision", videograph.DecisionType()},
	{"visual_outcome", element.Event(element.Named("perception.VisualOutcome"))},
	{"visual_resolved", element.State(element.Named("perception.VisualProviderResolution"))},
	{"visual_metrics", element.State(element.Named("perception.VisualMetrics"))},
}

// Config supplies only deployment identity and the public ownership
// projection. Source, observer name, and all boundary types are derived from
// the immutable graph plan, never duplicated as caller assertions.
type Config struct {
	ProfileName     string
	ProfileRevision uint64
	Reference       string
	Artifact        inspect.ArtifactIdentity
	Ownership       legacy.Ownership
}

// Adapter is an immutable profile/factory pair for one exact plan. Accessors
// return independent values, so a caller cannot mutate the profile captured
// by the registered factory.
type Adapter struct {
	profile      graphbinding.SessionAdapterProfile
	registration graphbinding.AdapterRegistration
}

// New validates the complete adaptive-video boundary contract and freezes a
// stable Realtime Video -> graph -> Observation projection.
func New(plan *graphconfig.Plan, config Config) (Adapter, error) {
	if plan == nil {
		return Adapter{}, errors.New("create adaptive video adapter: nil graph plan")
	}
	if err := plan.Validate(); err != nil {
		return Adapter{}, fmt.Errorf("create adaptive video adapter plan: %w", err)
	}
	graph := plan.Graph()
	boundaries, err := validateBoundaries(graph)
	if err != nil {
		return Adapter{}, fmt.Errorf("create adaptive video adapter: %w", err)
	}
	projection, err := deriveProjection(plan, boundaries)
	if err != nil {
		return Adapter{}, fmt.Errorf("create adaptive video adapter: %w", err)
	}
	profile, err := graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
		FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
		Name:          config.ProfileName, Revision: config.ProfileRevision,
		GraphFingerprint: graph.Fingerprint,
		Ownership:        config.Ownership,
		Capabilities: legacy.Capabilities{
			Video: true, Observations: true, Observers: []string{projection.observer},
		},
		Boundaries: []graphbinding.AdapterBoundary{
			{
				Operation: graphbinding.AdapterInputVideo, Boundary: frameBoundary,
				Direction: ir.InputBoundary, Type: boundaries.frame.Type,
			},
			{
				Operation: graphbinding.AdapterOutputObservation, Boundary: observationsBoundary,
				Direction: ir.OutputBoundary, Type: boundaries.observations.Type,
			},
		},
	})
	if err != nil {
		return Adapter{}, fmt.Errorf("create adaptive video adapter profile: %w", err)
	}
	registration := graphbinding.AdapterRegistration{
		Reference: config.Reference, Artifact: config.Artifact,
	}
	frozenProfile := profile.Clone()
	registration.Factory = func(
		_ context.Context, mounted *graphruntime.Mounted, options legacy.Options,
		selected graphbinding.SessionAdapterProfile,
	) (graphbinding.SessionAdapter, error) {
		if selected.Fingerprint != frozenProfile.Fingerprint {
			return nil, errors.New("adaptive video adapter received a different frozen profile")
		}
		if mounted == nil || mounted.Graph().Fingerprint != frozenProfile.GraphFingerprint {
			return nil, errors.New("adaptive video adapter received a different mounted graph")
		}
		return newSession(mounted, options, projection)
	}
	// AdapterRegistration validation is intentionally performed by NewNative,
	// but validate its public identity here too so this constructor really does
	// return a coherent frozen pair.
	if err := config.Artifact.Validate(); err != nil {
		return Adapter{}, fmt.Errorf("create adaptive video adapter artifact: %w", err)
	}
	if config.Reference == "" || config.Reference != strings.TrimSpace(config.Reference) ||
		strings.ContainsAny(config.Reference, "\x00\r\n") || mutableReference(config.Reference) {
		return Adapter{}, errors.New("create adaptive video adapter: registration requires a canonical reference")
	}
	return Adapter{profile: profile, registration: registration}, nil
}

// Profile returns the frozen Realtime-to-graph projection.
func (adapter Adapter) Profile() graphbinding.SessionAdapterProfile {
	return adapter.profile.Clone()
}

// Registration returns the exact executable registration. Its factory closes
// over private immutable copies of the derived plan projection.
func (adapter Adapter) Registration() graphbinding.AdapterRegistration {
	return adapter.registration
}

type adaptiveBoundaries struct {
	source, frame, tick, policyOutcome, observations ir.Boundary
}

func validateBoundaries(graph ir.Graph) (adaptiveBoundaries, error) {
	if err := graph.Validate(); err != nil {
		return adaptiveBoundaries{}, err
	}
	wanted := []struct {
		name      string
		direction ir.BoundaryDirection
		typeName  element.Type
		target    *ir.Boundary
	}{
		{sourceBoundary, ir.InputBoundary, videograph.SourceStartType(), nil},
		{frameBoundary, ir.InputBoundary, videograph.InlineFrameType(), nil},
		{tickBoundary, ir.InputBoundary, videograph.TimingTickType(), nil},
		{policyOutcomeBoundary, ir.OutputBoundary, videograph.PolicyOutcomeType(), nil},
		{observationsBoundary, ir.OutputBoundary, observationType, nil},
	}
	result := adaptiveBoundaries{}
	wanted[0].target = &result.source
	wanted[1].target = &result.frame
	wanted[2].target = &result.tick
	wanted[3].target = &result.policyOutcome
	wanted[4].target = &result.observations
	for _, requirement := range wanted {
		boundary, found := graphBoundary(graph, requirement.name, requirement.direction)
		if !found {
			return adaptiveBoundaries{}, fmt.Errorf(
				"graph %s has no %s boundary %q", graph.ID, requirement.direction, requirement.name,
			)
		}
		if !boundary.Type.Equal(requirement.typeName) {
			return adaptiveBoundaries{}, fmt.Errorf(
				"graph boundary %q has type %s, want %s",
				requirement.name, boundary.Type.String(), requirement.typeName.String(),
			)
		}
		*requirement.target = boundary
	}
	if result.source.Endpoint.Node != result.tick.Endpoint.Node ||
		result.policyOutcome.Endpoint.Node != result.source.Endpoint.Node {
		return adaptiveBoundaries{}, errors.New("source, tick, and policy_outcome must expose one adaptive policy node")
	}
	for _, auxiliary := range auxiliaryOutputs {
		boundary, found := graphBoundary(graph, auxiliary.name, ir.OutputBoundary)
		if !found {
			return adaptiveBoundaries{}, fmt.Errorf(
				"graph %s has no %s boundary %q", graph.ID, ir.OutputBoundary, auxiliary.name,
			)
		}
		if !boundary.Type.Equal(auxiliary.typeName) {
			return adaptiveBoundaries{}, fmt.Errorf(
				"graph boundary %q has type %s, want %s",
				auxiliary.name, boundary.Type.String(), auxiliary.typeName.String(),
			)
		}
	}
	return result, nil
}

func graphBoundary(
	graph ir.Graph, name string, direction ir.BoundaryDirection,
) (ir.Boundary, bool) {
	for _, boundary := range graph.Boundaries {
		if boundary.Name == name && boundary.Direction == direction {
			boundary.Type = boundary.Type.Clone()
			return boundary, true
		}
	}
	return ir.Boundary{}, false
}

type projection struct {
	source   string
	observer string
}

func deriveProjection(plan *graphconfig.Plan, boundaries adaptiveBoundaries) (projection, error) {
	values := plan.Values()
	var policy struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(values[boundaries.source.Endpoint.Node], &policy); err != nil {
		return projection{}, fmt.Errorf("decode adaptive policy values: %w", err)
	}
	policy.Source = strings.TrimSpace(policy.Source)
	if !canonicalProjectionText(policy.Source) {
		return projection{}, errors.New("adaptive policy values require a canonical source")
	}
	var visual struct {
		Source string `json:"source"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(values[boundaries.observations.Endpoint.Node], &visual); err != nil {
		return projection{}, fmt.Errorf("decode visual observer values: %w", err)
	}
	visual.Source = strings.TrimSpace(visual.Source)
	visual.Name = strings.TrimSpace(visual.Name)
	if visual.Name == "" {
		visual.Name = "visual-" + visual.Source
	}
	if !canonicalProjectionText(visual.Source) || visual.Source != policy.Source {
		return projection{}, fmt.Errorf(
			"visual observer source %q does not match adaptive policy source %q",
			visual.Source, policy.Source,
		)
	}
	if !canonicalProjectionText(visual.Name) {
		return projection{}, errors.New("visual observer values require a canonical name")
	}
	return projection{source: policy.Source, observer: visual.Name}, nil
}

func canonicalProjectionText(value string) bool {
	return value != "" && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "\x00\r\n")
}

func mutableReference(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, placeholder := range []string{"latest", "current", "unknown", "unresolved"} {
		if value == placeholder || strings.HasSuffix(value, ":"+placeholder) ||
			strings.HasSuffix(value, "@"+placeholder) || strings.HasSuffix(value, "/"+placeholder) {
			return true
		}
	}
	return strings.Contains(value, "${") || strings.Contains(value, "{{")
}

type session struct {
	sourceInput, frameInput, tickInput element.OutputPort
	policyOutcomes, observations       element.InputPort
	auxiliary                          []element.InputPort
	sourceType, frameType, tickType    element.Type
	policyOutcomeType, observationType element.Type
	sink                               legacy.Sink
	sessionID, source, observer        string
	streamID                           string

	mu             sync.Mutex
	sequence       uint64
	sourceStarted  bool
	lastCapturedNS uint64
}

func newSession(
	mounted *graphruntime.Mounted, options legacy.Options, selected projection,
) (*session, error) {
	if options.Sink == nil {
		return nil, errors.New("adaptive video session requires a sink")
	}
	if !canonicalProjectionText(options.SessionID) {
		return nil, errors.New("adaptive video session requires a canonical session ID")
	}
	sourceInput, err := mounted.Ingress(sourceBoundary)
	if err != nil {
		return nil, err
	}
	frameInput, err := mounted.Ingress(frameBoundary)
	if err != nil {
		return nil, err
	}
	tickInput, err := mounted.Ingress(tickBoundary)
	if err != nil {
		return nil, err
	}
	policyOutcomes, err := mounted.Egress(policyOutcomeBoundary)
	if err != nil {
		return nil, err
	}
	observations, err := mounted.Egress(observationsBoundary)
	if err != nil {
		return nil, err
	}
	auxiliary := make([]element.InputPort, 0, len(auxiliaryOutputs))
	for _, requirement := range auxiliaryOutputs {
		port, portErr := mounted.Egress(requirement.name)
		if portErr != nil {
			return nil, portErr
		}
		if !port.Type().Equal(requirement.typeName) {
			return nil, fmt.Errorf("adaptive video auxiliary output %q has type %s, want %s",
				requirement.name, port.Type().String(), requirement.typeName.String())
		}
		auxiliary = append(auxiliary, port)
	}
	return &session{
		sourceInput: sourceInput, frameInput: frameInput, tickInput: tickInput,
		policyOutcomes: policyOutcomes, observations: observations, auxiliary: auxiliary,
		sourceType: sourceInput.Type(), frameType: frameInput.Type(), tickType: tickInput.Type(),
		policyOutcomeType: policyOutcomes.Type(), observationType: observations.Type(),
		sink: options.Sink, sessionID: options.SessionID,
		source: selected.source, observer: selected.observer,
		streamID: options.SessionID + ":" + selected.source,
	}, nil
}

func (adapter *session) Run(ctx context.Context) error {
	sessionCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	results := make(chan error, len(adapter.auxiliary)+1)
	var wait sync.WaitGroup
	run := func(operation func(context.Context) error) {
		defer wait.Done()
		results <- operation(sessionCtx)
	}
	wait.Add(1)
	go run(adapter.runObservations)
	for _, input := range adapter.auxiliary {
		port := input
		wait.Add(1)
		go run(func(drainCtx context.Context) error {
			for {
				_, err := port.Receive(drainCtx)
				if terminalAdapterError(drainCtx, err) {
					return nil
				}
				if err != nil {
					return fmt.Errorf("drain adaptive video output %s: %w", port.Name(), err)
				}
			}
		})
	}
	first := <-results
	cancel(first)
	wait.Wait()
	if context.Cause(ctx) != nil || terminalAdapterError(ctx, first) {
		return nil
	}
	if first == nil {
		return errors.New("adaptive video output drainer stopped without cancellation")
	}
	return first
}

func (adapter *session) runObservations(ctx context.Context) error {
	for {
		envelope, err := adapter.observations.Receive(ctx)
		if terminalAdapterError(ctx, err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receive adaptive video observation: %w", err)
		}
		if !envelope.Type.Equal(adapter.observationType) {
			return fmt.Errorf("adaptive video observation has type %s, want %s",
				envelope.Type.String(), adapter.observationType.String())
		}
		observation, ok := observationPayload(envelope.Payload)
		if !ok {
			return fmt.Errorf("adaptive video observation has payload %T", envelope.Payload)
		}
		if observation.Observer != adapter.observer || observation.Source != adapter.source {
			return fmt.Errorf(
				"adaptive video observation provenance %q/%q drifted from %q/%q",
				observation.Observer, observation.Source, adapter.observer, adapter.source,
			)
		}
		if err := observation.Validate(); err != nil {
			return fmt.Errorf("adaptive video observation: %w", err)
		}
		observation.Media = slices.Clone(observation.Media)
		if err := adapter.sink.Observation(ctx, observation); err != nil {
			if terminalAdapterError(ctx, err) {
				return nil
			}
			return fmt.Errorf("publish adaptive video observation: %w", err)
		}
	}
}

func (adapter *session) Update(_ context.Context, settings legacy.Settings) error {
	for _, observer := range settings.Observers {
		if observer != adapter.observer {
			return fmt.Errorf("adaptive video observer %q is not available", observer)
		}
	}
	return nil
}

func (*session) Audio(context.Context, coreperception.Frame) error {
	return legacy.ErrUnsupported
}

func (adapter *session) Video(ctx context.Context, frame coreperception.Frame) error {
	if ctx == nil {
		return errors.New("adaptive video input: nil context")
	}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("adaptive video input: %w", err)
	}
	if frame.Kind != coreperception.FrameImage || frame.Source != adapter.source {
		return fmt.Errorf("adaptive video input source %q does not match selected source %q",
			frame.Source, adapter.source)
	}
	if frame.CapturedNS < sourceOpenedNS {
		return errors.New("adaptive video input requires a positive capture timestamp")
	}
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.lastCapturedNS != 0 && frame.CapturedNS <= adapter.lastCapturedNS {
		return errors.New("adaptive video input capture timestamps must increase")
	}
	if !adapter.sourceStarted {
		itemID := adapter.nextItemID("source")
		if err := adapter.sendAndAwait(ctx, adapter.sourceInput, adapter.sourceType, itemID,
			"source", videograph.SourceStart{
				Source: adapter.source, StreamID: adapter.streamID,
				Kind: videograph.SourceVideo, SourceRevision: 1,
				OpenedNS: sourceOpenedNS, ExpectedMIMEType: frame.MIMEType,
			}, sourceOpenedNS, nil); err != nil {
			return err
		}
		adapter.sourceStarted = true
	}
	frameID := adapter.nextItemID("frame")
	frame.Image = slices.Clone(frame.Image)
	if err := adapter.sendAndAwait(ctx, adapter.frameInput, adapter.frameType, frameID,
		"frame", videograph.InlineFrame{
			StreamID: adapter.streamID, SourceRevision: 1, Frame: frame,
		}, frame.CapturedNS, nil); err != nil {
		return err
	}
	tickID := adapter.nextItemID("tick")
	if err := adapter.sendAndAwait(ctx, adapter.tickInput, adapter.tickType, tickID,
		"tick", videograph.TimingTick{
			Source: adapter.source, StreamID: adapter.streamID,
			SourceRevision: 1, NowNS: frame.CapturedNS,
		}, frame.CapturedNS, []string{frameID}); err != nil {
		return err
	}
	adapter.lastCapturedNS = frame.CapturedNS
	return nil
}

func (adapter *session) sendAndAwait(
	ctx context.Context, output element.OutputPort, typeName element.Type,
	itemID, operation string, payload any, captureNS uint64, parents []string,
) error {
	result, err := output.Broadcast(ctx, element.Envelope{
		Type: typeName, ItemID: itemID, SessionID: adapter.sessionID,
		SourceID: adapter.source, TraceID: itemID,
		CancellationScope: adapter.streamID, CaptureNS: captureNS,
		CausalParents: slices.Clone(parents), Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("send adaptive video %s: %w", operation, err)
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		return fmt.Errorf("send adaptive video %s delivered %d and dropped %d lanes",
			operation, result.Delivered, result.Dropped)
	}
	outcomeEnvelope, err := adapter.policyOutcomes.Receive(ctx)
	if err != nil {
		return fmt.Errorf("receive adaptive video %s outcome: %w", operation, err)
	}
	if !outcomeEnvelope.Type.Equal(adapter.policyOutcomeType) {
		return fmt.Errorf("adaptive video %s outcome has type %s, want %s",
			operation, outcomeEnvelope.Type.String(), adapter.policyOutcomeType.String())
	}
	if len(outcomeEnvelope.CausalParents) != 1 || outcomeEnvelope.CausalParents[0] != itemID {
		return fmt.Errorf("adaptive video %s outcome is not caused by %s", operation, itemID)
	}
	outcome, ok := policyOutcomePayload(outcomeEnvelope.Payload)
	if !ok {
		return fmt.Errorf("adaptive video %s outcome has payload %T", operation, outcomeEnvelope.Payload)
	}
	if outcome.Operation != operation {
		return fmt.Errorf("adaptive video %s received %s outcome", operation, outcome.Operation)
	}
	if outcome.Kind != videograph.OutcomeSucceeded {
		return fmt.Errorf("adaptive video %s was %s (%s): %s",
			operation, outcome.Kind, outcome.Code, outcome.Message)
	}
	return nil
}

func (adapter *session) nextItemID(kind string) string {
	adapter.sequence++
	return fmt.Sprintf("%s:adaptive-video:%s:%d", adapter.sessionID, kind, adapter.sequence)
}

func observationPayload(payload any) (coreperception.Observation, bool) {
	switch typed := payload.(type) {
	case coreperception.Observation:
		typed.Media = slices.Clone(typed.Media)
		return typed, true
	case *coreperception.Observation:
		if typed != nil {
			result := *typed
			result.Media = slices.Clone(typed.Media)
			return result, true
		}
	}
	return coreperception.Observation{}, false
}

func policyOutcomePayload(payload any) (videograph.PolicyOutcome, bool) {
	switch typed := payload.(type) {
	case videograph.PolicyOutcome:
		return typed, true
	case *videograph.PolicyOutcome:
		if typed != nil {
			return *typed, true
		}
	}
	return videograph.PolicyOutcome{}, false
}

func terminalAdapterError(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, graphruntime.ErrChannelClosed) ||
		(ctx != nil && context.Cause(ctx) != nil)
}

func (*session) Text(context.Context, legacy.TextInput) error { return legacy.ErrUnsupported }
func (*session) ToolResult(context.Context, trajectory.ToolResult) error {
	return legacy.ErrUnsupported
}
func (*session) CommitAudio(context.Context) error    { return legacy.ErrUnsupported }
func (*session) CreateResponse(context.Context) error { return legacy.ErrUnsupported }
func (*session) Cancel(context.Context, string) error { return legacy.ErrUnsupported }
func (*session) Truncate(context.Context, legacy.Truncation) error {
	return legacy.ErrUnsupported
}
func (*session) Trajectory() trajectory.Snapshot    { return trajectory.Snapshot{} }
func (*session) Close(context.Context, error) error { return nil }

var _ graphbinding.SessionAdapter = (*session)(nil)
