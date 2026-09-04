package realtimecu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

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
	graphID                     = "realtime_computer_use"
	audioBoundary               = "audio"
	videoBoundary               = "video"
	textBoundary                = "text"
	sessionCancelBoundary       = "session_cancel"
	observationBoundary         = "observation_events"
	transcriptBoundary          = "transcript_events"
	observationOutcomeBoundary  = "observation_outcome"
	cancellationOutcomeBoundary = "cancellation_outcome"
	dispatchCommitBoundary      = "dispatch_commit"
	canonicalResultBoundary     = "canonical_result"
	textObserverName            = "client.text"
	// Consequences are bounded independently of the action bridge so a client
	// cannot complete effects indefinitely without supplying the screen frames
	// that make those effects observable. The bound matches the bridge's
	// pending-call limit; once exhausted, the next result fails the session
	// closed instead of overwriting causal evidence.
	maximumPendingVisualConsequences = maximumPendingClientCalls
)

type boundaries struct {
	audio, video, text, sessionCancel             ir.Boundary
	observations, transcripts, observationOutcome ir.Boundary
	cancellationOutcome, calls, results           ir.Boundary
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
			{Operation: graphbinding.AdapterInputCancel, Boundary: sessionCancelBoundary, Direction: ir.InputBoundary, Type: selected.sessionCancel.Type},
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
			var observer Observer
			var observerErr error
			if plugin.config.Observer.ResourceFactory != nil {
				observer, observerErr = plugin.config.Observer.ResourceFactory(
					ctx, options, ObserverResources{Retainer: bundle.media},
				)
			} else {
				observer, observerErr = plugin.config.Observer.Factory(ctx, options)
			}
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
		{name: sessionCancelBoundary, direction: ir.InputBoundary, typeName: SessionCancellationType()},
		{name: observationBoundary, direction: ir.OutputBoundary, typeName: stateelements.ObservationType()},
		{name: transcriptBoundary, direction: ir.OutputBoundary, typeName: stateelements.ObservationType()},
		{name: observationOutcomeBoundary, direction: ir.OutputBoundary, typeName: stateelements.ObservationCommitOutcomeType()},
		{name: cancellationOutcomeBoundary, direction: ir.OutputBoundary, typeName: SessionCancellationOutcomeType()},
		{name: dispatchCommitBoundary, direction: ir.OutputBoundary, typeName: actionelements.CommittedType()},
		{name: canonicalResultBoundary, direction: ir.OutputBoundary, typeName: actionelements.CanonicalResultType()},
	}
	result := boundaries{}
	wanted[0].target = &result.audio
	wanted[1].target = &result.video
	wanted[2].target = &result.text
	wanted[3].target = &result.sessionCancel
	wanted[4].target = &result.observations
	wanted[5].target = &result.transcripts
	wanted[6].target = &result.observationOutcome
	wanted[7].target = &result.cancellationOutcome
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
	var settlement policyelements.IntentSettlementConfig
	if err := json.Unmarshal(values["settlement"], &settlement); err != nil {
		return fmt.Errorf("decode values for settlement: %w", err)
	}
	var producer policyelements.IntentDispositionProducerConfig
	if err := json.Unmarshal(values["settlement_producer"], &producer); err != nil {
		return fmt.Errorf("decode values for settlement_producer: %w", err)
	}
	activation, err := decodeActivationConfig(values["activation"])
	if err != nil {
		return fmt.Errorf("decode values for activation: %w", err)
	}
	if producer.ExpectedSettlement.Detector.Reference != SettlementPolicyReference {
		return fmt.Errorf("node settlement_producer detector reference %q, want %q",
			producer.ExpectedSettlement.Detector.Reference, SettlementPolicyReference)
	}
	if !reflect.DeepEqual(settlement, producer.ExpectedSettlement) {
		return errors.New("nodes settlement and settlement_producer carry different settlement contracts")
	}
	if activation.ExpectedSettlement == nil ||
		!reflect.DeepEqual(settlement, *activation.ExpectedSettlement) {
		return errors.New("nodes settlement and activation carry different settlement contracts")
	}
	if !reflect.DeepEqual(activation.ExpectedAdmission, settlement.ExpectedAdmission) {
		return errors.New("node activation expected_admission differs from the settlement admission contract")
	}
	return nil
}

type activeCall struct {
	call  trajectory.ToolCall
	runID string
}

type observationCommitWaiter struct {
	itemID string
	ack    chan error
}

type session struct {
	audio, video, text, sessionCancel element.OutputPort
	outputs                           map[string]element.InputPort
	sink                              legacy.Sink
	sessionID                         string
	config                            PluginConfig
	bundle                            *sessionBundle
	observer                          Observer

	audioObservationMu  sync.Mutex
	videoObservationMu  sync.Mutex
	mediaLifecycleMu    sync.RWMutex
	mediaClosed         atomic.Bool
	observationMu       sync.Mutex
	revisions           map[string]uint64
	captured            map[string]uint64
	seenText            map[string]struct{}
	sequence            uint64
	pendingConsequences []VisualConsequence

	effectMu         sync.Mutex
	active           map[string]activeCall
	observationAckMu sync.Mutex
	observationAcks  map[string]chan error
	cancelAckMu      sync.Mutex
	cancelAcks       map[string]chan error

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
	if bundle == nil || bundle.bridge == nil || bundle.store == nil || bundle.media == nil {
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
	sessionCancel, err := ingress(sessionCancelBoundary)
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
		audio: audio, video: video, text: text, sessionCancel: sessionCancel,
		outputs: outputs,
		sink:    options.Sink, sessionID: options.SessionID, config: config,
		bundle: bundle, observer: observer, revisions: make(map[string]uint64),
		captured: make(map[string]uint64), seenText: make(map[string]struct{}),
		active:          make(map[string]activeCall),
		observationAcks: make(map[string]chan error), cancelAcks: make(map[string]chan error),
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
		case observationOutcomeBoundary:
			err = session.acceptObservationOutcome(envelope)
		case cancellationOutcomeBoundary:
			err = session.acceptCancellationOutcome(envelope)
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

func (session *session) acceptObservationOutcome(envelope element.Envelope) error {
	outcome, ok := observationCommitOutcomePayload(envelope.Payload)
	if !ok || !envelope.Type.Equal(stateelements.ObservationCommitOutcomeType()) {
		return fmt.Errorf("Realtime-CU observation commit outcome has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID || outcome.TriggerItemID == "" {
		return errors.New("Realtime-CU observation commit outcome crossed the mounted session or omitted its trigger")
	}
	if !slices.Contains(envelope.CausalParents, outcome.TriggerItemID) {
		return errors.New("Realtime-CU observation commit outcome omitted its exact trigger lineage")
	}
	switch outcome.Kind {
	case stateelements.ObservationCommitted:
		if outcome.TrajectoryItemID == "" || outcome.StoreVersion == 0 {
			return errors.New("Realtime-CU committed observation outcome omitted canonical trajectory identity")
		}
	case stateelements.ObservationRejected:
	default:
		return fmt.Errorf("Realtime-CU observation commit outcome has unknown kind %q", outcome.Kind)
	}
	session.observationAckMu.Lock()
	ack := session.observationAcks[outcome.TriggerItemID]
	if ack != nil {
		delete(session.observationAcks, outcome.TriggerItemID)
	}
	session.observationAckMu.Unlock()
	if ack == nil {
		return nil
	}
	if outcome.Kind != stateelements.ObservationCommitted {
		ack <- fmt.Errorf("observation commit reached %s/%s: %s", outcome.Kind, outcome.Code, outcome.Message)
	} else {
		ack <- nil
	}
	return nil
}

func (session *session) acceptCancellationOutcome(envelope element.Envelope) error {
	outcome, ok := sessionCancellationOutcomePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("Realtime-CU cancellation outcome has payload %T", envelope.Payload)
	}
	if envelope.SessionID != session.sessionID || outcome.SessionID != session.sessionID {
		return errors.New("Realtime-CU cancellation outcome crossed the mounted session")
	}
	if outcome.RequestItemID == "" {
		return errors.New("Realtime-CU cancellation outcome has no request identity")
	}
	session.cancelAckMu.Lock()
	ack := session.cancelAcks[outcome.RequestItemID]
	session.cancelAckMu.Unlock()
	if ack == nil {
		return nil
	}
	var result error
	terminal := true
	switch outcome.Kind {
	case SessionCancellationAccepted, SessionCancellationProgress:
		terminal = false
	case SessionCancellationCompleted, SessionCancellationNoCurrentIntent:
	case SessionCancellationIgnored:
		if outcome.Code == "duplicate_active_request" {
			terminal = false
		} else if outcome.Code == "intent_cancellation_in_progress" {
			result = errors.New("cancellation of the current durable intent is still in progress; retry after completion")
		} else if outcome.Code != "duplicate_terminal_request" {
			result = fmt.Errorf("cancellation request was ignored: %s/%s",
				outcome.Operation, outcome.Code)
		}
	case SessionCancellationRefused, SessionCancellationIncomplete:
		result = fmt.Errorf("cancellation reached %s/%s: %s",
			outcome.Kind, outcome.Code, outcome.Message)
	default:
		result = fmt.Errorf("cancellation returned unknown outcome kind %q", outcome.Kind)
	}
	if !terminal {
		return nil
	}
	session.cancelAckMu.Lock()
	if session.cancelAcks[outcome.RequestItemID] == ack {
		delete(session.cancelAcks, outcome.RequestItemID)
	} else {
		ack = nil
	}
	session.cancelAckMu.Unlock()
	if ack != nil {
		ack <- result
	}
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
	declared := committed.Executable.Canonical.Authorized.Confirmed.Declared
	admitted := declared.Admitted
	call := cloneToolCall(admitted.Proposal.Call)
	// A deployment-owned normalizer preserves the model's byte-exact proposal
	// under Admitted while carrying the call that actually crossed Dispatch in
	// EffectiveCall. The client boundary must render that effective call. The
	// bridge still compares it with the independently registered dispatcher
	// input, so this does not weaken the drift check.
	if declared.EffectiveCall != nil {
		call = cloneToolCall(*declared.EffectiveCall)
	}
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
	session.videoObservationMu.Lock()
	defer session.videoObservationMu.Unlock()
	session.mediaLifecycleMu.RLock()
	defer session.mediaLifecycleMu.RUnlock()
	if session.mediaClosed.Load() {
		return errors.New("realtime-CU session is closed")
	}
	session.observationMu.Lock()
	if len(session.pendingConsequences) >= maximumPendingVisualConsequences {
		session.observationMu.Unlock()
		return fmt.Errorf("realtime-CU visual consequence capacity is exhausted at %d pending effects",
			maximumPendingVisualConsequences)
	}
	session.observationMu.Unlock()
	if err := session.observer.Consequence(ctx, feedback); err != nil {
		return fmt.Errorf("request realtime-CU visual consequence observation: %w", err)
	}
	session.observationMu.Lock()
	session.pendingConsequences = append(session.pendingConsequences, feedback)
	session.observationMu.Unlock()
	return nil
}

func (session *session) publishDebug(ctx context.Context, name string, envelope element.Envelope) error {
	sink, ok := session.sink.(legacy.DebugSink)
	if !ok {
		return nil
	}
	attributes := map[string]any{"run_id": envelope.RunID, "type": envelope.Type.String()}
	if outcome, found := generationOutcomePayload(envelope.Payload); found {
		attributes["outcome_kind"] = string(outcome.Kind)
		attributes["outcome_code"] = outcome.Code
		attributes["generation_id"] = outcome.GenerationID
	}
	return sink.Debug(ctx, legacy.DebugEvent{
		Category: "graph", Name: name, Phase: "output", CorrelationID: envelope.ItemID,
		Attributes: attributes,
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
	sensorMu := &session.videoObservationMu
	if frame.Kind == perception.FrameAudio {
		sensorMu = &session.audioObservationMu
	}
	sensorMu.Lock()
	defer sensorMu.Unlock()
	if session.mediaClosed.Load() {
		return errors.New("realtime-CU session is closed")
	}
	session.observationMu.Lock()
	if previous := session.captured[frame.Source]; previous != 0 && frame.CapturedNS <= previous {
		session.observationMu.Unlock()
		return fmt.Errorf("realtime-CU %s capture timestamps must increase", frame.Source)
	}
	session.observationMu.Unlock()
	observations, err := session.observeMedia(ctx, frame)
	if err != nil {
		return err
	}
	// Provider work is sensor-local. Only validation and graph commit below take
	// the shared state lock, so a slow vision model cannot starve microphone ASR.
	// Cross-sensor commits deliberately follow provider completion order; each
	// source's timestamps/revisions and every declared causal parent remain exact.
	session.observationMu.Lock()
	defer session.observationMu.Unlock()
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

// observeMedia holds the lifecycle read lock only while calling the observer.
// Close takes the corresponding write lock before Observer.Close, so provider
// resources cannot be used concurrently with or after their teardown. The
// canonical graph-commit wait happens later, outside this lock.
func (session *session) observeMedia(
	ctx context.Context, frame perception.Frame,
) ([]perception.Observation, error) {
	session.mediaLifecycleMu.RLock()
	defer session.mediaLifecycleMu.RUnlock()
	if session.mediaClosed.Load() {
		return nil, errors.New("realtime-CU session is closed")
	}
	var observations []perception.Observation
	var err error
	if frame.Kind == perception.FrameAudio {
		observations, err = session.observer.Audio(ctx, frame)
	} else {
		observations, err = session.observer.Video(ctx, frame)
	}
	if err != nil {
		return nil, fmt.Errorf("observe realtime-CU %s frame: %w", frame.Source, err)
	}
	return observations, nil
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
		OccurredNS: input.OccurredNS,
	}
	if err := session.sendObservation(
		ctx, session.text, observation, input.OccurredNS, nil, input.ItemID,
	); err != nil {
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
	waiter, err := session.beginObservationCommit(ctx, port, observation, capturedNS, parents, itemID)
	if err != nil {
		return err
	}
	return session.awaitObservationCommit(ctx, waiter)
}

// beginObservationCommit linearizes an accepted ingress publication against
// Close. It registers the commit waiter and broadcasts while the session is
// known to be open. The acknowledgement lock is released when this function
// returns, before awaitObservationCommit can block on the canonical outcome.
func (session *session) beginObservationCommit(
	ctx context.Context, port element.OutputPort, observation perception.Observation,
	capturedNS uint64, parents []string, itemID string,
) (observationCommitWaiter, error) {
	// The acknowledgement registry is also the close/publication
	// linearization boundary. Close marks the atomic lifecycle closed before
	// taking this lock; a publication either completes its broadcast first and
	// leaves a waiter for Close to release, or observes closure and publishes
	// nothing.
	session.observationAckMu.Lock()
	if session.mediaClosed.Load() {
		session.observationAckMu.Unlock()
		return observationCommitWaiter{}, errors.New("realtime-CU session is closed")
	}
	if itemID == "" {
		itemID = session.nextItemID("observation")
	} else {
		session.sequence++
	}
	// Production sessions wait for the exact canonical commit outcome. This is
	// both delivery evidence and an ingress ordering barrier: once Text/Audio/
	// Video returns, a later session cancellation cannot race ahead of that
	// already-accepted user observation and incorrectly report no current
	// durable intent. Small direct unit fixtures may leave observationAcks nil
	// when they intentionally exercise only envelope construction.
	var ack chan error
	if session.observationAcks != nil {
		ack = make(chan error, 1)
		if _, duplicate := session.observationAcks[itemID]; duplicate {
			session.observationAckMu.Unlock()
			return observationCommitWaiter{}, fmt.Errorf(
				"Realtime-CU observation %q already awaits canonical commit", itemID,
			)
		}
		session.observationAcks[itemID] = ack
	}
	result, err := port.Broadcast(ctx, element.Envelope{
		Type: port.Type(), ItemID: itemID, SessionID: session.sessionID,
		SourceID:      observation.Observer + ":" + observation.Source,
		OpportunityID: itemID, Sequence: session.sequence, CaptureNS: capturedNS,
		TraceID: itemID, CausalParents: slices.Clone(parents), Payload: observation,
	})
	if err != nil {
		if ack != nil {
			delete(session.observationAcks, itemID)
		}
		session.observationAckMu.Unlock()
		return observationCommitWaiter{}, fmt.Errorf("send realtime-CU observation: %w", err)
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		if ack != nil {
			delete(session.observationAcks, itemID)
		}
		session.observationAckMu.Unlock()
		return observationCommitWaiter{}, fmt.Errorf("send realtime-CU observation delivered %d and dropped %d lanes",
			result.Delivered, result.Dropped)
	}
	session.observationAckMu.Unlock()
	return observationCommitWaiter{itemID: itemID, ack: ack}, nil
}

func (session *session) awaitObservationCommit(
	ctx context.Context, waiter observationCommitWaiter,
) error {
	if waiter.ack == nil {
		return nil
	}
	select {
	case err := <-waiter.ack:
		if err != nil {
			return fmt.Errorf("commit realtime-CU observation: %w", err)
		}
		return nil
	case <-ctx.Done():
		session.observationAckMu.Lock()
		if session.observationAcks[waiter.itemID] == waiter.ack {
			delete(session.observationAcks, waiter.itemID)
		}
		session.observationAckMu.Unlock()
		return context.Cause(ctx)
	}
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
	if err := usableContext(ctx, "cancel realtime-CU session work"); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "client canceled response"
	}
	if len(reason) > maximumCancellationReasonBytes {
		return fmt.Errorf("cancel Realtime-CU session work: reason exceeds %d bytes",
			maximumCancellationReasonBytes)
	}
	// Establish one explicit adapter ordering boundary. Earlier observation
	// publications retain this lock until their exact canonical commit; later
	// publications cannot overtake this cancellation broadcast. Sensor provider
	// work that is genuinely concurrent and has not reached this boundary may
	// linearize on either side, matching the graph's actor-order semantics.
	session.observationMu.Lock()
	requestItemID := session.nextItemID("session-cancel")
	requestSequence := session.sequence
	ack := make(chan error, 1)
	session.cancelAckMu.Lock()
	session.cancelAcks[requestItemID] = ack
	session.cancelAckMu.Unlock()
	removeAck := func() {
		session.cancelAckMu.Lock()
		if session.cancelAcks[requestItemID] == ack {
			delete(session.cancelAcks, requestItemID)
		}
		session.cancelAckMu.Unlock()
	}
	result, err := session.sessionCancel.Broadcast(ctx, element.Envelope{
		Type: session.sessionCancel.Type(), ItemID: requestItemID,
		SessionID: session.sessionID, Sequence: requestSequence,
		CancellationScope: session.sessionID, TraceID: requestItemID,
		Payload: SessionCancellation{SessionID: session.sessionID, Reason: reason},
	})
	session.observationMu.Unlock()
	if err != nil {
		removeAck()
		return fmt.Errorf("cancel Realtime-CU session work: %w", err)
	}
	if result.Delivered != 1 || result.Dropped != 0 {
		removeAck()
		return fmt.Errorf("cancel Realtime-CU session work delivered %d and dropped %d lanes",
			result.Delivered, result.Dropped)
	}
	select {
	case ackErr := <-ack:
		if ackErr != nil {
			return fmt.Errorf("cancel Realtime-CU session work: %w", ackErr)
		}
		return nil
	case <-ctx.Done():
		removeAck()
		return context.Cause(ctx)
	}
}

func (*session) Truncate(context.Context, legacy.Truncation) error { return legacy.ErrUnsupported }

func (session *session) Trajectory() trajectory.Snapshot {
	return session.bundle.store.Snapshot()
}

func (session *session) Close(_ context.Context, cause error) error {
	session.closeOnce.Do(func() {
		session.mediaLifecycleMu.Lock()
		defer session.mediaLifecycleMu.Unlock()
		session.mediaClosed.Store(true)
		session.bundle.bridge.Close(cause)
		session.observationAckMu.Lock()
		for id, ack := range session.observationAcks {
			delete(session.observationAcks, id)
			ack <- errors.New("Realtime-CU session closed before observation committed")
		}
		session.observationAckMu.Unlock()
		session.cancelAckMu.Lock()
		for id, ack := range session.cancelAcks {
			delete(session.cancelAcks, id)
			ack <- errors.New("Realtime-CU session closed before cancellation completed")
		}
		session.cancelAckMu.Unlock()
		session.closeErr = session.observer.Close()
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

func observationCommitOutcomePayload(payload any) (stateelements.ObservationCommitOutcome, bool) {
	switch value := payload.(type) {
	case stateelements.ObservationCommitOutcome:
		return value, true
	case *stateelements.ObservationCommitOutcome:
		if value != nil {
			return *value, true
		}
	}
	return stateelements.ObservationCommitOutcome{}, false
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

func sessionCancellationOutcomePayload(payload any) (SessionCancellationOutcome, bool) {
	switch value := payload.(type) {
	case SessionCancellationOutcome:
		return value, true
	case *SessionCancellationOutcome:
		if value != nil {
			return *value, true
		}
	}
	return SessionCancellationOutcome{}, false
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
