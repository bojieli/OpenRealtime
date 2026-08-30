package realtimecu

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
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	graphID                   = "realtime_computer_use"
	audioBoundary             = "audio"
	videoBoundary             = "video"
	textBoundary              = "text"
	activationCancelBoundary  = "activation_cancel"
	actionCancelBoundary      = "action_cancel"
	observationBoundary       = "observation_events"
	transcriptBoundary        = "transcript_events"
	activationOutcomeBoundary = "activation_outcome"
	dispatchCommitBoundary    = "dispatch_commit"
	canonicalResultBoundary   = "canonical_result"
	textObserverName          = "client.text"
	// Consequences are bounded independently of the action bridge so a client
	// cannot complete effects indefinitely without supplying the screen frames
	// that make those effects observable. The bound matches the bridge's
	// pending-call limit; once exhausted, the next result fails the session
	// closed instead of overwriting causal evidence.
	maximumPendingVisualConsequences = maximumPendingClientCalls
)

type boundaries struct {
	audio, video, text, activationCancel, cancel                 ir.Boundary
	observations, transcripts, activationOutcome, calls, results ir.Boundary
}

type boundAdapter struct {
	profile      graphbinding.SessionAdapterProfile
	registration graphbinding.AdapterRegistration
}

func (adapter boundAdapter) Profile() graphbinding.SessionAdapterProfile {
	return adapter.profile.Clone()
}

func (adapter boundAdapter) Registration() graphbinding.AdapterRegistration {
	return adapter.registration
}

func bind(plan *graphconfig.Plan, plugin *Plugin) (boundAdapter, error) {
	if plan == nil || plugin == nil {
		return boundAdapter{}, errors.New("create realtime-CU adapter: plan and plugin are required")
	}
	if err := plan.Validate(); err != nil {
		return boundAdapter{}, fmt.Errorf("create realtime-CU adapter plan: %w", err)
	}
	graph := plan.Graph()
	if graph.ID != graphID {
		return boundAdapter{}, fmt.Errorf("create realtime-CU adapter: graph ID %q, want %q", graph.ID, graphID)
	}
	selected, err := validateBoundaries(graph)
	if err != nil {
		return boundAdapter{}, fmt.Errorf("create realtime-CU adapter: %w", err)
	}
	if err := validatePlanReferences(plan); err != nil {
		return boundAdapter{}, fmt.Errorf("create realtime-CU adapter: %w", err)
	}
	profile, err := graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
		FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
		Name:          ProfileName, Revision: ProfileRevision, GraphFingerprint: graph.Fingerprint,
		Ownership: legacy.Ownership{
			Perception: legacy.OwnerEngine, FastCognition: legacy.OwnerEngine,
			SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerEngine,
			Interaction: legacy.OwnerEngine, Floor: legacy.OwnerEngine,
		},
		Capabilities: legacy.Capabilities{
			Video: true, ComputerUse: true, Observations: true,
			Observers: []string{plugin.config.Observer.Name},
			Stack: legacy.StackCapabilities{
				AudioInput: true, VisualInput: true, Transcription: true,
				TurnGeneration: true, ConcurrentIO: true, TextInjection: true,
			},
		},
		Boundaries: []graphbinding.AdapterBoundary{
			{Operation: graphbinding.AdapterInputAudio, Boundary: audioBoundary, Direction: ir.InputBoundary, Type: selected.audio.Type},
			{Operation: graphbinding.AdapterInputVideo, Boundary: videoBoundary, Direction: ir.InputBoundary, Type: selected.video.Type},
			{Operation: graphbinding.AdapterInputText, Boundary: textBoundary, Direction: ir.InputBoundary, Type: selected.text.Type},
			{Operation: graphbinding.AdapterInputCancel, Boundary: actionCancelBoundary, Direction: ir.InputBoundary, Type: selected.cancel.Type},
			{Operation: graphbinding.AdapterOutputObservation, Boundary: observationBoundary, Direction: ir.OutputBoundary, Type: selected.observations.Type},
			{Operation: graphbinding.AdapterOutputTranscript, Boundary: transcriptBoundary, Direction: ir.OutputBoundary, Type: selected.transcripts.Type},
			{Operation: graphbinding.AdapterOutputToolCalls, Boundary: dispatchCommitBoundary, Direction: ir.OutputBoundary, Type: selected.calls.Type},
		},
	})
	if err != nil {
		return boundAdapter{}, fmt.Errorf("create realtime-CU adapter profile: %w", err)
	}
	frozenProfile := profile.Clone()
	registration := graphbinding.AdapterRegistration{
		Reference: AdapterReference, Artifact: plugin.config.RuntimeArtifact,
		Factory: func(
			ctx context.Context, mounted *graphruntime.Mounted, options legacy.Options,
			requested graphbinding.SessionAdapterProfile,
		) (graphbinding.SessionAdapter, error) {
			if requested.Fingerprint != frozenProfile.Fingerprint {
				return nil, errors.New("realtime-CU adapter received a different frozen profile")
			}
			if mounted == nil || mounted.Graph().Fingerprint != frozenProfile.GraphFingerprint {
				return nil, errors.New("realtime-CU adapter received a different mounted graph")
			}
			bundle, takeErr := plugin.coordinator.take(options.SessionID)
			if takeErr != nil {
				return nil, takeErr
			}
			observer, observerErr := plugin.config.Observer.Factory(ctx, options)
			if observerErr != nil {
				bundle.bridge.Close(observerErr)
				return nil, fmt.Errorf("create realtime-CU observer: %w", observerErr)
			}
			if observer == nil || reflectedNil(observer) {
				bundle.bridge.Close(errors.New("realtime-CU observer factory returned nil"))
				return nil, errors.New("create realtime-CU observer: factory returned nil")
			}
			session, sessionErr := newSession(mounted, options, plugin.config, bundle, observer)
			if sessionErr != nil {
				bundle.bridge.Close(sessionErr)
				return nil, errors.Join(sessionErr, observer.Close())
			}
			return session, nil
		},
	}
	if err := registration.Validate(); err != nil {
		return boundAdapter{}, fmt.Errorf("create realtime-CU adapter registration: %w", err)
	}
	return boundAdapter{profile: profile, registration: registration}, nil
}

func validateBoundaries(graph ir.Graph) (boundaries, error) {
	if err := graph.Validate(); err != nil {
		return boundaries{}, err
	}
	wanted := []struct {
		name      string
		direction ir.BoundaryDirection
		typeName  element.Type
		target    *ir.Boundary
	}{
		{name: audioBoundary, direction: ir.InputBoundary, typeName: stateelements.ObservationType()},
		{name: videoBoundary, direction: ir.InputBoundary, typeName: stateelements.ObservationType()},
		{name: textBoundary, direction: ir.InputBoundary, typeName: stateelements.ObservationType()},
		{name: activationCancelBoundary, direction: ir.InputBoundary, typeName: policyelements.GenerationCancelType()},
		{name: actionCancelBoundary, direction: ir.InputBoundary, typeName: actionelements.InterruptType()},
		{name: observationBoundary, direction: ir.OutputBoundary, typeName: stateelements.ObservationType()},
		{name: transcriptBoundary, direction: ir.OutputBoundary, typeName: stateelements.ObservationType()},
		{name: activationOutcomeBoundary, direction: ir.OutputBoundary, typeName: policyelements.GenerationOutcomeType()},
		{name: dispatchCommitBoundary, direction: ir.OutputBoundary, typeName: actionelements.CommittedType()},
		{name: canonicalResultBoundary, direction: ir.OutputBoundary, typeName: actionelements.CanonicalResultType()},
	}
	result := boundaries{}
	wanted[0].target = &result.audio
	wanted[1].target = &result.video
	wanted[2].target = &result.text
	wanted[3].target = &result.activationCancel
	wanted[4].target = &result.cancel
	wanted[5].target = &result.observations
	wanted[6].target = &result.transcripts
	wanted[7].target = &result.activationOutcome
	wanted[8].target = &result.calls
	wanted[9].target = &result.results
	for _, requirement := range wanted {
		boundary, found := graphBoundary(graph, requirement.name, requirement.direction)
		if !found {
			return boundaries{}, fmt.Errorf("graph has no %s boundary %q", requirement.direction, requirement.name)
		}
		if !boundary.Type.Equal(requirement.typeName) {
			return boundaries{}, fmt.Errorf("graph boundary %q has type %s, want %s",
				requirement.name, boundary.Type.String(), requirement.typeName.String())
		}
		*requirement.target = boundary
	}
	return result, nil
}

func graphBoundary(graph ir.Graph, name string, direction ir.BoundaryDirection) (ir.Boundary, bool) {
	for _, boundary := range graph.Boundaries {
		if boundary.Name == name && boundary.Direction == direction {
			boundary.Type = boundary.Type.Clone()
			return boundary, true
		}
	}
	return ir.Boundary{}, false
}

func validatePlanReferences(plan *graphconfig.Plan) error {
	wanted := map[string]map[string]string{
		"model":         {"provider": ModelReference},
		"tool_lookup":   {"registry": ToolReference},
		"confirmation":  {"provider": ConfirmReference},
		"target_fence":  {"target": TargetReference},
		"ledger_commit": {"ledger": LedgerReference},
		"dispatch":      {"registry": ToolReference, "ledger": LedgerReference},
	}
	values := plan.Values()
	for node, fields := range wanted {
		var decoded map[string]any
		if err := json.Unmarshal(values[node], &decoded); err != nil {
			return fmt.Errorf("decode values for %s: %w", node, err)
		}
		for field, expected := range fields {
			actual, _ := decoded[field].(string)
			if actual != expected {
				return fmt.Errorf("node %s %s reference %q, want %q", node, field, actual, expected)
			}
		}
	}
	return nil
}

type activeCall struct {
	call  trajectory.ToolCall
	runID string
}

type session struct {
	audio, video, text, activationCancel, cancel element.OutputPort
	outputs                                      map[string]element.InputPort
	sink                                         legacy.Sink
	sessionID                                    string
	config                                       PluginConfig
	bundle                                       *sessionBundle
	observer                                     Observer

	observationMu       sync.Mutex
	revisions           map[string]uint64
	captured            map[string]uint64
	seenText            map[string]struct{}
	sequence            uint64
	pendingConsequences []VisualConsequence

	effectMu    sync.Mutex
	active      map[string]activeCall
	cancelAckMu sync.Mutex
	cancelAcks  map[string]chan error

	closeOnce sync.Once
	closeErr  error
}

func newSession(
	mounted *graphruntime.Mounted, options legacy.Options, config PluginConfig,
	bundle *sessionBundle, observer Observer,
) (*session, error) {
	if options.Sink == nil || !canonical(options.SessionID) {
		return nil, errors.New("realtime-CU session requires a sink and canonical session ID")
	}
	if bundle == nil || bundle.bridge == nil || bundle.store == nil {
		return nil, errors.New("realtime-CU session requires its prepared dependency bundle")
	}
	ingress := func(name string) (element.OutputPort, error) { return mounted.Ingress(name) }
	audio, err := ingress(audioBoundary)
	if err != nil {
		return nil, err
	}
	video, err := ingress(videoBoundary)
	if err != nil {
		return nil, err
	}
	text, err := ingress(textBoundary)
	if err != nil {
		return nil, err
	}
	activationCancel, err := ingress(activationCancelBoundary)
	if err != nil {
		return nil, err
	}
	cancel, err := ingress(actionCancelBoundary)
	if err != nil {
		return nil, err
	}
	outputs := make(map[string]element.InputPort)
	for _, boundary := range mounted.Graph().Boundaries {
		if boundary.Direction != ir.OutputBoundary {
			continue
		}
		port, portErr := mounted.Egress(boundary.Name)
		if portErr != nil {
			return nil, portErr
		}
		outputs[boundary.Name] = port
	}
	return &session{
		audio: audio, video: video, text: text, activationCancel: activationCancel,
		cancel: cancel, outputs: outputs,
		sink: options.Sink, sessionID: options.SessionID, config: config,
		bundle: bundle, observer: observer, revisions: make(map[string]uint64),
		captured: make(map[string]uint64), seenText: make(map[string]struct{}),
		active: make(map[string]activeCall), cancelAcks: make(map[string]chan error),
	}, nil
}

func (session *session) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run realtime-CU adapter: nil context")
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	results := make(chan error, len(session.outputs))
	var wait sync.WaitGroup
	for name, port := range session.outputs {
		name, port := name, port
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- session.runOutput(runCtx, name, port)
		}()
	}
	first := <-results
	cancel(first)
	wait.Wait()
	if context.Cause(ctx) != nil || terminalAdapterError(ctx, first) {
		return nil
	}
	if first == nil {
		return errors.New("realtime-CU output drainer stopped without cancellation")
	}
	return first
}

func (session *session) runOutput(ctx context.Context, name string, port element.InputPort) error {
	for {
		envelope, err := port.Receive(ctx)
		if terminalAdapterError(ctx, err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receive realtime-CU output %s: %w", name, err)
		}
		if !envelope.Type.Equal(port.Type()) {
			return fmt.Errorf("realtime-CU output %s has type %s, want %s",
				name, envelope.Type.String(), port.Type().String())
		}
		switch name {
		case observationBoundary:
			err = session.publishObservation(ctx, envelope)
		case transcriptBoundary:
			err = session.publishTranscript(ctx, envelope)
		case activationOutcomeBoundary:
			err = session.acceptActivationOutcome(envelope)
		case dispatchCommitBoundary:
			err = session.publishCall(ctx, envelope)
		case canonicalResultBoundary:
			err = session.acceptCanonicalResult(ctx, envelope)
		default:
			err = session.publishDebug(ctx, name, envelope)
		}
		if err != nil {
			return err
		}
	}
}

func (session *session) acceptActivationOutcome(envelope element.Envelope) error {
	outcome, ok := generationOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("Realtime-CU activation outcome has payload %T", envelope.Payload)
	}
	var cause string
	session.cancelAckMu.Lock()
	for _, parent := range envelope.CausalParents {
		if _, found := session.cancelAcks[parent]; found {
			cause = parent
			break
		}
	}
	ack := session.cancelAcks[cause]
	if ack != nil {
		delete(session.cancelAcks, cause)
	}
	session.cancelAckMu.Unlock()
	if ack == nil {
		return nil
	}
	if outcome.Kind != policyelements.GenerationCanceled || outcome.Code != "intent_revoked" {
		ack <- fmt.Errorf("activation cancellation reached %s/%s", outcome.Kind, outcome.Code)
		return nil
	}
	ack <- nil
	return nil
}

func (session *session) publishObservation(ctx context.Context, envelope element.Envelope) error {
	observation, ok := observationPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("realtime-CU observation output has payload %T", envelope.Payload)
	}
	if err := session.validateOutputObservation(observation); err != nil {
		return err
	}
	if err := session.sink.Observation(ctx, observation); err != nil {
		return fmt.Errorf("publish realtime-CU observation: %w", err)
	}
	return nil
}

func (session *session) publishTranscript(ctx context.Context, envelope element.Envelope) error {
	observation, ok := observationPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("realtime-CU transcript output has payload %T", envelope.Payload)
	}
	if err := session.validateOutputObservation(observation); err != nil {
		return err
	}
	if observation.Source != SourceMicrophone || observation.Authority != trajectory.AuthorityUser {
		return nil
	}
	if err := session.sink.Transcript(ctx, legacy.TranscriptEvent{
		ItemID: envelope.ItemID, Text: observation.Text, Final: observation.Final,
	}); err != nil {
		return fmt.Errorf("publish realtime-CU transcript: %w", err)
	}
	return nil
}

func (session *session) validateOutputObservation(observation perception.Observation) error {
	if err := observation.Validate(); err != nil {
		return fmt.Errorf("realtime-CU graph observation: %w", err)
	}
	switch observation.Source {
	case SourceMicrophone:
		if observation.Authority != trajectory.AuthorityUser || observation.Observer != session.config.Observer.Name {
			return errors.New("realtime-CU microphone observation drifted from user authority or selected observer")
		}
	case SourceScreen, SourceCamera:
		if observation.Authority != trajectory.AuthorityObserver || observation.Observer != session.config.Observer.Name {
			return errors.New("realtime-CU visual observation drifted from observer authority or selected observer")
		}
	case "text":
		if observation.Authority != trajectory.AuthorityUser || observation.Observer != textObserverName {
			return errors.New("realtime-CU text observation drifted from user authority")
		}
	default:
		return fmt.Errorf("realtime-CU graph emitted undeclared observation source %q", observation.Source)
	}
	return nil
}

func (session *session) publishCall(ctx context.Context, envelope element.Envelope) error {
	committed, ok := committedActionPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("realtime-CU dispatch commit has payload %T", envelope.Payload)
	}
	admitted := committed.Executable.Canonical.Authorized.Confirmed.Declared.Admitted
	call := cloneToolCall(admitted.Proposal.Call)
	if err := validateClientCall(call); err != nil {
		return err
	}
	if admitted.SessionID != session.sessionID || admitted.ModelRunID == "" ||
		(envelope.SessionID != "" && envelope.SessionID != session.sessionID) ||
		(envelope.RunID != "" && envelope.RunID != admitted.ModelRunID) {
		return errors.New("realtime-CU committed call drifted from session or cognition-run identity")
	}
	if err := session.bundle.bridge.AwaitEmission(ctx, call); err != nil {
		return fmt.Errorf("authorize realtime-CU client emission: %w", err)
	}
	session.effectMu.Lock()
	if _, duplicate := session.active[call.CallID]; duplicate {
		session.effectMu.Unlock()
		err := fmt.Errorf("realtime-CU call %q is already active", call.CallID)
		session.bundle.bridge.FailEmission(call.CallID, err)
		return err
	}
	session.active[call.CallID] = activeCall{call: call, runID: admitted.ModelRunID}
	session.effectMu.Unlock()
	fail := func(err error) error {
		session.bundle.bridge.FailEmission(call.CallID, err)
		return err
	}
	if err := session.sink.TurnBegin(ctx); err != nil {
		return fail(fmt.Errorf("begin realtime-CU action turn: %w", err))
	}
	if err := session.sink.ToolCalls(ctx, legacy.ToolCallEvent{
		InvocationID: admitted.ModelRunID, Calls: []trajectory.ToolCall{call},
	}); err != nil {
		return fail(fmt.Errorf("publish realtime-CU client call: %w", err))
	}
	if err := session.sink.TurnEnd(ctx, legacy.TurnOutcome{}); err != nil {
		return fail(fmt.Errorf("end realtime-CU action turn: %w", err))
	}
	return nil
}

func (session *session) acceptCanonicalResult(ctx context.Context, envelope element.Envelope) error {
	result, ok := canonicalResultPayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("realtime-CU canonical result has payload %T", envelope.Payload)
	}
	callID := result.Execution.CallID
	session.effectMu.Lock()
	active, found := session.active[callID]
	if found {
		delete(session.active, callID)
	}
	session.effectMu.Unlock()
	if !found || active.call.Name != result.Execution.Name || result.TrajectoryItemID == "" || result.StoreVersion == 0 {
		return fmt.Errorf("realtime-CU canonical result %q has no matching emitted action", callID)
	}
	feedback := VisualConsequence{
		CallID: callID, Name: active.call.Name, CanonicalResultItemID: result.TrajectoryItemID,
		TargetSource: SourceScreen, StoreVersion: result.StoreVersion,
	}
	return session.queueVisualConsequence(ctx, feedback)
}

func (session *session) queueVisualConsequence(ctx context.Context, feedback VisualConsequence) error {
	session.observationMu.Lock()
	defer session.observationMu.Unlock()
	if len(session.pendingConsequences) >= maximumPendingVisualConsequences {
		return fmt.Errorf("realtime-CU visual consequence capacity is exhausted at %d pending effects",
			maximumPendingVisualConsequences)
	}
	if err := session.observer.Consequence(ctx, feedback); err != nil {
		return fmt.Errorf("request realtime-CU visual consequence observation: %w", err)
	}
	session.pendingConsequences = append(session.pendingConsequences, feedback)
	return nil
}

func (session *session) publishDebug(ctx context.Context, name string, envelope element.Envelope) error {
	sink, ok := session.sink.(legacy.DebugSink)
	if !ok {
		return nil
	}
	return sink.Debug(ctx, legacy.DebugEvent{
		Category: "graph", Name: name, Phase: "output", CorrelationID: envelope.ItemID,
		Attributes: map[string]any{"run_id": envelope.RunID, "type": envelope.Type.String()},
	})
}

func (session *session) Update(ctx context.Context, settings legacy.Settings) error {
	if err := usableContext(ctx, "update realtime-CU session"); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(settings.Observers))
	for _, observer := range settings.Observers {
		if observer != session.config.Observer.Name {
			return fmt.Errorf("realtime-CU observer %q is not available", observer)
		}
		if _, duplicate := seen[observer]; duplicate {
			return fmt.Errorf("realtime-CU observer %q is repeated", observer)
		}
		seen[observer] = struct{}{}
	}
	return session.bundle.bridge.Update(settings)
}

func (session *session) Audio(ctx context.Context, frame perception.Frame) error {
	if frame.Kind != perception.FrameAudio || frame.Source != SourceMicrophone {
		return errors.New("realtime-CU audio input must be a microphone audio frame")
	}
	return session.observe(ctx, session.audio, frame, trajectory.AuthorityUser)
}

func (session *session) Video(ctx context.Context, frame perception.Frame) error {
	if frame.Kind != perception.FrameImage || (frame.Source != SourceScreen && frame.Source != SourceCamera) {
		return errors.New("realtime-CU video input must be a screen or camera image frame")
	}
	return session.observe(ctx, session.video, frame, trajectory.AuthorityObserver)
}

func (session *session) observe(
	ctx context.Context, port element.OutputPort, frame perception.Frame, authority trajectory.Authority,
) error {
	if err := usableContext(ctx, "send realtime-CU media"); err != nil {
		return err
	}
	if err := frame.Validate(); err != nil {
		return fmt.Errorf("realtime-CU %s frame: %w", frame.Source, err)
	}
	if frame.CapturedNS == 0 {
		return fmt.Errorf("realtime-CU %s frame requires a positive capture timestamp", frame.Source)
	}
	frame.PCM16LE = slices.Clone(frame.PCM16LE)
	frame.Image = slices.Clone(frame.Image)
	session.observationMu.Lock()
	defer session.observationMu.Unlock()
	if previous := session.captured[frame.Source]; previous != 0 && frame.CapturedNS <= previous {
		return fmt.Errorf("realtime-CU %s capture timestamps must increase", frame.Source)
	}
	var observations []perception.Observation
	var err error
	if frame.Kind == perception.FrameAudio {
		observations, err = session.observer.Audio(ctx, frame)
	} else {
		observations, err = session.observer.Video(ctx, frame)
	}
	if err != nil {
		return fmt.Errorf("observe realtime-CU %s frame: %w", frame.Source, err)
	}
	validated := make([]perception.Observation, len(observations))
	nextRevisions := make(map[string]uint64, len(session.revisions))
	for key, revision := range session.revisions {
		nextRevisions[key] = revision
	}
	for index, observation := range observations {
		observation.Media = slices.Clone(observation.Media)
		if observation.OccurredNS == 0 {
			observation.OccurredNS = frame.CapturedNS
		}
		if err := observation.Validate(); err != nil {
			return fmt.Errorf("realtime-CU %s observation %d: %w", frame.Source, index, err)
		}
		if observation.Observer != session.config.Observer.Name || observation.Source != frame.Source ||
			observation.Authority != authority {
			return fmt.Errorf("realtime-CU %s observation %d drifted from exact observer/source/authority", frame.Source, index)
		}
		key := observation.Observer + "\x00" + observation.Source
		if observation.Revision == 0 || observation.Revision <= nextRevisions[key] {
			return fmt.Errorf("realtime-CU %s observation revisions must strictly increase", frame.Source)
		}
		nextRevisions[key] = observation.Revision
		validated[index] = observation
	}
	effectParent := ""
	if frame.Source == SourceScreen && len(session.pendingConsequences) != 0 {
		effectParent = session.pendingConsequences[0].CanonicalResultItemID
	}
	for _, observation := range validated {
		parents := []string(nil)
		if effectParent != "" {
			parents = []string{effectParent}
		}
		if err := session.sendObservation(ctx, port, observation, frame.CapturedNS, parents, ""); err != nil {
			return err
		}
		session.revisions[observation.Observer+"\x00"+observation.Source] = observation.Revision
	}
	if effectParent != "" && len(validated) != 0 {
		session.pendingConsequences = session.pendingConsequences[1:]
	}
	session.captured[frame.Source] = frame.CapturedNS
	return nil
}

func (session *session) Text(ctx context.Context, input legacy.TextInput) error {
	if err := usableContext(ctx, "send realtime-CU text"); err != nil {
		return err
	}
	if !canonical(input.ItemID) || input.Role != "user" || strings.TrimSpace(input.Text) == "" {
		return errors.New("realtime-CU text input requires a canonical item ID, user role, and text")
	}
	if len(input.Images) != 0 {
		return errors.New("realtime-CU attached message images are not video sources and are not supported by this profile")
	}
	session.observationMu.Lock()
	defer session.observationMu.Unlock()
	if _, duplicate := session.seenText[input.ItemID]; duplicate {
		return fmt.Errorf("realtime-CU text item %q is duplicated", input.ItemID)
	}
	key := textObserverName + "\x00text"
	revision := session.revisions[key] + 1
	observation := perception.Observation{
		Text: input.Text, Observer: textObserverName, Source: "text",
		Authority: trajectory.AuthorityUser, Revision: revision, Final: true,
	}
	if err := session.sendObservation(ctx, session.text, observation, 0, nil, input.ItemID); err != nil {
		return err
	}
	session.seenText[input.ItemID] = struct{}{}
	session.revisions[key] = revision
	return nil
}

func (session *session) sendObservation(
	ctx context.Context, port element.OutputPort, observation perception.Observation,
	capturedNS uint64, parents []string, itemID string,
) error {
	if itemID == "" {
		itemID = session.nextItemID("observation")
	} else {
		session.sequence++
	}
	result, err := port.Broadcast(ctx, element.Envelope{
		Type: port.Type(), ItemID: itemID, SessionID: session.sessionID,
		SourceID:      observation.Observer + ":" + observation.Source,
		OpportunityID: itemID, Sequence: session.sequence, CaptureNS: capturedNS,
		TraceID: itemID, CausalParents: slices.Clone(parents), Payload: observation,
	})
	if err != nil {
		return fmt.Errorf("send realtime-CU observation: %w", err)
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		return fmt.Errorf("send realtime-CU observation delivered %d and dropped %d lanes",
			result.Delivered, result.Dropped)
	}
	return nil
}

func (session *session) ToolResult(ctx context.Context, result trajectory.ToolResult) error {
	if err := usableContext(ctx, "return realtime-CU tool result"); err != nil {
		return err
	}
	return session.bundle.bridge.Complete(result)
}

func (*session) CommitAudio(context.Context) error { return legacy.ErrUnsupported }

// The graph is observation-driven. Realtime clients still send
// response.create after function_call_output; accepting it preserves the wire
// contract while the forced post-effect screen observation supplies the next
// typed cognition trigger.
func (session *session) CreateResponse(ctx context.Context) error {
	return usableContext(ctx, "create realtime-CU response")
}

func (session *session) Cancel(ctx context.Context, reason string) error {
	if err := usableContext(ctx, "cancel realtime-CU action"); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "client canceled response"
	}
	session.observationMu.Lock()
	activationItemID := session.nextItemID("intent-cancel")
	activationSequence := session.sequence
	session.observationMu.Unlock()
	ack := make(chan error, 1)
	session.cancelAckMu.Lock()
	session.cancelAcks[activationItemID] = ack
	session.cancelAckMu.Unlock()
	removeAck := func() {
		session.cancelAckMu.Lock()
		delete(session.cancelAcks, activationItemID)
		session.cancelAckMu.Unlock()
	}
	activationResult, err := session.activationCancel.Broadcast(ctx, element.Envelope{
		Type: session.activationCancel.Type(), ItemID: activationItemID,
		SessionID: session.sessionID, Sequence: activationSequence,
		CancellationScope: session.sessionID, TraceID: activationItemID,
		Payload: policyelements.GenerationCancel{StreamID: session.sessionID, Reason: reason},
	})
	if err != nil {
		removeAck()
		return fmt.Errorf("cancel Realtime-CU durable user intent: %w", err)
	}
	if activationResult.Delivered != 1 || activationResult.Dropped != 0 {
		removeAck()
		return fmt.Errorf("cancel Realtime-CU durable user intent delivered %d and dropped %d lanes",
			activationResult.Delivered, activationResult.Dropped)
	}
	select {
	case ackErr := <-ack:
		if ackErr != nil {
			return fmt.Errorf("cancel Realtime-CU durable user intent: %w", ackErr)
		}
	case <-ctx.Done():
		removeAck()
		return context.Cause(ctx)
	}
	session.effectMu.Lock()
	active := make([]activeCall, 0, len(session.active))
	for _, call := range session.active {
		active = append(active, call)
	}
	session.effectMu.Unlock()
	for _, call := range active {
		session.observationMu.Lock()
		itemID := session.nextItemID("cancel")
		session.observationMu.Unlock()
		result, err := session.cancel.Broadcast(ctx, element.Envelope{
			Type: session.cancel.Type(), ItemID: itemID, SessionID: session.sessionID,
			RunID: call.runID, CancellationScope: call.runID, TraceID: itemID,
			Payload: actionelements.Interrupt{CallID: call.call.CallID, Reason: reason},
		})
		if err != nil {
			return fmt.Errorf("send realtime-CU action cancellation: %w", err)
		}
		if result.Delivered != 1 || result.Dropped != 0 {
			return fmt.Errorf("send realtime-CU action cancellation delivered %d and dropped %d lanes",
				result.Delivered, result.Dropped)
		}
	}
	return nil
}

func (*session) Truncate(context.Context, legacy.Truncation) error { return legacy.ErrUnsupported }

func (session *session) Trajectory() trajectory.Snapshot {
	return session.bundle.store.Snapshot()
}

func (session *session) Status() legacy.Status {
	return legacy.Status{
		Fast:               session.config.Model.Descriptor.Provider + ":" + session.config.Model.Descriptor.Model,
		Perception:         session.config.Observer.Reference,
		PerceptionRevision: session.config.Observer.Artifact.Revision,
		Tools: legacy.ToolStatus{
			Fast: "propose", Slow: "propose", Authorization: "graph-native",
			Execution: session.bundle.bridge.Name(),
		},
	}
}

func (session *session) Close(_ context.Context, cause error) error {
	session.closeOnce.Do(func() {
		session.bundle.bridge.Close(cause)
		session.cancelAckMu.Lock()
		for id, ack := range session.cancelAcks {
			delete(session.cancelAcks, id)
			ack <- errors.New("Realtime-CU session closed before intent cancellation committed")
		}
		session.cancelAckMu.Unlock()
		session.observationMu.Lock()
		session.closeErr = session.observer.Close()
		session.observationMu.Unlock()
	})
	return session.closeErr
}

func (session *session) nextItemID(kind string) string {
	session.sequence++
	return fmt.Sprintf("%s:realtime-cu:%s:%d", session.sessionID, kind, session.sequence)
}

func observationPayload(payload any) (perception.Observation, bool) {
	switch value := payload.(type) {
	case perception.Observation:
		value.Media = slices.Clone(value.Media)
		return value, true
	case *perception.Observation:
		if value != nil {
			result := *value
			result.Media = slices.Clone(value.Media)
			return result, true
		}
	}
	return perception.Observation{}, false
}

func committedActionPayload(payload any) (actionelements.CommittedAction, bool) {
	switch value := payload.(type) {
	case actionelements.CommittedAction:
		return value, true
	case *actionelements.CommittedAction:
		if value != nil {
			return *value, true
		}
	}
	return actionelements.CommittedAction{}, false
}

func generationOutcomePayload(payload any) (policyelements.GenerationOutcome, bool) {
	switch value := payload.(type) {
	case policyelements.GenerationOutcome:
		return value, true
	case *policyelements.GenerationOutcome:
		if value != nil {
			return *value, true
		}
	}
	return policyelements.GenerationOutcome{}, false
}

func canonicalResultPayload(payload any) (actionelements.CanonicalResult, bool) {
	switch value := payload.(type) {
	case actionelements.CanonicalResult:
		return value, true
	case *actionelements.CanonicalResult:
		if value != nil {
			return *value, true
		}
	}
	return actionelements.CanonicalResult{}, false
}

func usableContext(ctx context.Context, operation string) error {
	if ctx == nil {
		return fmt.Errorf("%s: nil context", operation)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return nil
}

func terminalAdapterError(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, graphruntime.ErrChannelClosed) ||
		(ctx != nil && context.Cause(ctx) != nil)
}

var _ graphbinding.SessionAdapter = (*session)(nil)
