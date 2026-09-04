package interaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	coreperception "github.com/bojieli/OpenRealtime/perception"
	coresession "github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	// OverlapBargeInSchedulerService permits deterministic deadline tests. A
	// production graph normally uses the element-owned system scheduler.
	OverlapBargeInSchedulerService = "interaction.overlap-barge-in.scheduler"
	overlapBargeInRuntimeID        = "builtin://openrealtime/elements/interaction.OverlapBargeIn"
	overlapBargeInRuntimeRevision  = "implementation:9"

	defaultOverlapHoldMS       = 800
	maximumOverlapHoldMS       = 60_000
	defaultOverlapActiveRuns   = 256
	defaultOverlapUtterances   = 512
	maximumOverlapActiveMemory = 4096
	maximumOverlapTranscript   = 1 << 20
)

var (
	overlapDecisionType   = element.Event(element.Named("interaction.OverlapDecision"))
	overlapStateType      = element.State(element.Named("interaction.OverlapState"))
	overlapResolutionType = element.State(
		element.Named("interaction.OverlapPolicyResolution"),
	)
)

func OverlapDecisionType() element.Type   { return overlapDecisionType.Clone() }
func OverlapStateType() element.Type      { return overlapStateType.Clone() }
func OverlapResolutionType() element.Type { return overlapResolutionType.Clone() }

// OverlapBargeInDescriptor turns observable overlap evidence into addressed
// cancellation. It deliberately has no privileged access to a session: which
// cognition, segmentation, synthesis, and playback paths it may interrupt is
// selected solely by graph connections.
//
// Every lifecycle input is explicit. This prevents a policy from cancelling a
// guessed "current response" and makes queued synthesis, active playback, and
// still-running cognition independently inspectable. BreaksCycles is required
// because the cancellation outputs intentionally feed the same components
// whose lifecycle outputs drive this controller.
func OverlapBargeInDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "interaction.OverlapBargeIn",
		Revision:      9,
		Ports: []element.Port{
			{Name: "activity", Direction: element.Input, Type: acousticelements.ActivityType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "transcript", Direction: element.Input, Type: perceptionelements.ObservationType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "semantic", Direction: element.Input, Type: policyelements.SemanticDecisionType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "speech", Direction: element.Input, Type: speechelements.TextSegmentType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "result", Direction: element.Input, Type: safeModelResultType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "invocation", Direction: element.Input, Type: policyelements.SessionInvocationOutcomeType(),
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 32},
			{Name: "model", Direction: element.Input, Type: cognitionelements.OutcomeType(),
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 32},
			{Name: "segmentation", Direction: element.Input, Type: SegmentationOutcomeType(),
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 32},
			{Name: "tts", Direction: element.Input, Type: speechelements.TransitionType(),
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 32},
			{Name: "playback", Direction: element.Input, Type: speechelements.TransitionType(),
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 32},
			{Name: "release", Direction: element.Input, Type: speechelements.PlaybackReceiptType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "model_cancel", Direction: element.Output, Type: cognitionelements.CancelType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "segmentation_cancel", Direction: element.Output, Type: cognitionelements.CancelType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "tts_cancel", Direction: element.Output, Type: speechelements.CancelType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "playback_cancel", Direction: element.Output, Type: speechelements.CancelType(),
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "safe_result", Direction: element.Output, Type: safeModelResultType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "safe_release", Direction: element.Output, Type: speechelements.PlaybackReceiptType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "decision", Direction: element.Output, Type: overlapDecisionType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "state", Direction: element.Output, Type: overlapStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "agent_output", Direction: element.Output, Type: coreinteraction.AgentOutputType(),
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "resolved", Direction: element.Output, Type: overlapResolutionType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{
				"activity", "transcript", "semantic", "speech", "result", "invocation", "model", "segmentation", "tts", "playback", "release",
			},
			Outcomes: []string{
				"model_cancel", "segmentation_cancel", "tts_cancel", "playback_cancel",
				"safe_result", "safe_release", "decision", "state", "agent_output", "resolved",
			},
			MaxConcurrency: 1,
			BreaksCycles:   true,
		},
		StateSchema:  "schema://openrealtime/interaction/overlap-state/v4",
		ConfigSchema: "schema://openrealtime/interaction/overlap-barge-in-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
			{Name: policyelements.SemanticDeciderRegistryService, Optional: true},
			{Name: OverlapBargeInSchedulerService, Optional: true},
		},
		Effects: []element.Effect{{Name: "interaction.overlap-decision", Reversible: true}},
	}
}

type OverlapFallback string

const (
	OverlapFallbackCancel OverlapFallback = "cancel"
	OverlapFallbackKeep   OverlapFallback = "keep_speaking"
)

type OverlapBargeInConfig struct {
	// Decider selects a deployment-owned enumerated semantic classifier. Empty
	// disables classification and leaves the explicit timeout fallback in sole
	// control; it never silently selects a provider.
	Decider string `json:"decider,omitempty"`
	// HoldMS is the maximum time unclassified overlap may continue before the
	// fallback is applied. Directed semantic evidence may cancel sooner.
	HoldMS int `json:"hold_ms,omitempty"`
	// Unclassified says what to do when the hold expires without backchannel or
	// side-speech evidence. The conservative default yields the floor.
	Unclassified  OverlapFallback `json:"unclassified,omitempty"`
	MaxActiveRuns int             `json:"max_active_runs,omitempty"`
	MaxUtterances int             `json:"max_utterances,omitempty"`
}

func decodeOverlapBargeInConfig(source json.RawMessage) (OverlapBargeInConfig, error) {
	config := OverlapBargeInConfig{
		HoldMS: defaultOverlapHoldMS, Unclassified: OverlapFallbackCancel,
		MaxActiveRuns: defaultOverlapActiveRuns, MaxUtterances: defaultOverlapUtterances,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return OverlapBargeInConfig{}, err
	}
	config.Decider = strings.TrimSpace(config.Decider)
	if config.Decider != "" && !boundedOverlapIdentity(config.Decider) {
		return OverlapBargeInConfig{}, errors.New("overlap barge-in decider is not canonical")
	}
	if config.HoldMS < 1 || config.HoldMS > maximumOverlapHoldMS {
		return OverlapBargeInConfig{}, fmt.Errorf(
			"hold_ms must be between 1 and %d", maximumOverlapHoldMS,
		)
	}
	switch config.Unclassified {
	case OverlapFallbackCancel, OverlapFallbackKeep:
	default:
		return OverlapBargeInConfig{}, fmt.Errorf(
			"unclassified must be %q or %q", OverlapFallbackCancel, OverlapFallbackKeep,
		)
	}
	if config.MaxActiveRuns < 1 || config.MaxActiveRuns > maximumOverlapActiveMemory {
		return OverlapBargeInConfig{}, fmt.Errorf(
			"max_active_runs must be between 1 and %d", maximumOverlapActiveMemory,
		)
	}
	if config.MaxUtterances < 1 || config.MaxUtterances > maximumOverlapActiveMemory {
		return OverlapBargeInConfig{}, fmt.Errorf(
			"max_utterances must be between 1 and %d", maximumOverlapActiveMemory,
		)
	}
	return config, nil
}

type OverlapDecisionKind string

const (
	OverlapObserved   OverlapDecisionKind = "observed"
	OverlapClassified OverlapDecisionKind = "classified"
	OverlapKept       OverlapDecisionKind = "kept_speaking"
	OverlapCanceled   OverlapDecisionKind = "canceled"
	OverlapIgnored    OverlapDecisionKind = "ignored"
	OverlapRefused    OverlapDecisionKind = "refused"
)

// OverlapDecision is an inspectable control-plane verdict. It intentionally
// excludes transcript text; the evidence identity and revision are enough to
// reconstruct an authorized private trace without copying user speech into a
// general graph inspection surface.
type OverlapDecision struct {
	Kind                OverlapDecisionKind             `json:"kind"`
	Trigger             string                          `json:"trigger"`
	StreamID            string                          `json:"stream_id,omitempty"`
	EvidenceItemID      string                          `json:"evidence_item_id,omitempty"`
	SourceRevision      uint64                          `json:"source_revision,omitempty"`
	Evidence            coreinteraction.OverlapEvidence `json:"evidence,omitempty"`
	Policy              string                          `json:"policy"`
	Reason              string                          `json:"reason,omitempty"`
	OverlapStartedNS    uint64                          `json:"overlap_started_ns,omitempty"`
	DeadlineNS          uint64                          `json:"deadline_ns,omitempty"`
	DecidedNS           uint64                          `json:"decided_ns"`
	ActiveRunIDs        []string                        `json:"active_run_ids,omitempty"`
	ActiveUtteranceIDs  []string                        `json:"active_utterance_ids,omitempty"`
	ModelCancels        int                             `json:"model_cancels,omitempty"`
	SegmentationCancels int                             `json:"segmentation_cancels,omitempty"`
	TTSCancels          int                             `json:"tts_cancels,omitempty"`
	PlaybackCancels     int                             `json:"playback_cancels,omitempty"`
}

func (OverlapDecision) InspectionCause() element.InspectionCauseKind { return element.CausePolicy }

type OverlapState struct {
	UserSpeaking        bool                            `json:"user_speaking"`
	StreamID            string                          `json:"stream_id,omitempty"`
	OverlapActive       bool                            `json:"overlap_active"`
	OverlapStartedNS    uint64                          `json:"overlap_started_ns,omitempty"`
	DeadlineNS          uint64                          `json:"deadline_ns,omitempty"`
	Evidence            coreinteraction.OverlapEvidence `json:"evidence,omitempty"`
	ClassificationOpen  bool                            `json:"classification_open"`
	CancelIssued        bool                            `json:"cancel_issued"`
	ActiveModels        int                             `json:"active_models"`
	ActiveSegmentations int                             `json:"active_segmentations"`
	ActiveTTS           int                             `json:"active_tts"`
	ActivePlayback      int                             `json:"active_playback"`
	AgentOutput         coreinteraction.AgentOutput     `json:"agent_output"`
	Observed            uint64                          `json:"observed"`
	Classified          uint64                          `json:"classified"`
	Kept                uint64                          `json:"kept"`
	Canceled            uint64                          `json:"canceled"`
	Ignored             uint64                          `json:"ignored"`
	Refused             uint64                          `json:"refused"`
}

func (OverlapState) InspectionCause() element.InspectionCauseKind { return element.CausePolicy }

type OverlapPolicyResolution struct {
	Decider          string                                    `json:"decider,omitempty"`
	Descriptor       *policyelements.SemanticDeciderDescriptor `json:"descriptor,omitempty"`
	DescriptorDigest string                                    `json:"descriptor_digest,omitempty"`
	RegistryRevision uint64                                    `json:"registry_revision,omitempty"`
	HoldMS           int                                       `json:"hold_ms"`
	Unclassified     OverlapFallback                           `json:"unclassified"`
}

type overlapBargeInFactory struct{}

func (overlapBargeInFactory) Descriptor() element.Descriptor { return OverlapBargeInDescriptor() }

func (overlapBargeInFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeOverlapBargeInConfig(source)
	return err
}

func (overlapBargeInFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeOverlapBargeInConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("interaction.OverlapBargeIn %s config: %w", mount.InstanceID, err)
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("overlap barge-in requires the runtime clock service")
	}
	runtimeClock, ok := clockValue.(graphruntime.Clock)
	if !ok || reflectedNilInterface(runtimeClock) {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("overlap barge-in requires the runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	scheduler := clock.Scheduler(clock.NewSystem())
	if value, _, available := mount.Services.Lookup(OverlapBargeInSchedulerService); available {
		scheduler, ok = value.(clock.Scheduler)
		if !ok || scheduler == nil {
			return nil, fmt.Errorf("overlap barge-in scheduler service has type %T", value)
		}
	}
	var registry *policyelements.SemanticDeciderRegistry
	var registryRevision uint64
	var descriptor policyelements.SemanticDeciderDescriptor
	if config.Decider != "" {
		value, revision, available := mount.Services.Lookup(policyelements.SemanticDeciderRegistryService)
		if !available {
			return nil, errors.New("overlap barge-in selected a decider without a semantic decider registry")
		}
		registry, ok = value.(*policyelements.SemanticDeciderRegistry)
		if !ok || registry == nil {
			return nil, fmt.Errorf("semantic decider registry service has type %T", value)
		}
		descriptor, err = registry.Describe(config.Decider)
		if err != nil {
			return nil, err
		}
		registryRevision = revision
	}
	ports, err := overlapPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &overlapBargeInRunner{
		instance: mount.InstanceID, config: config, clock: runtimeClock, scheduler: scheduler,
		sequences: sequences, registry: registry, registryRevision: registryRevision,
		descriptor: descriptor, resolution: mount.Resolution, ports: ports,
		runs: make(map[string]*overlapRun), terminalRuns: make(map[string]struct{}),
		utterances:         make(map[string]*overlapUtterance),
		terminalUtterances: make(map[string]struct{}),
		semantic:           make(map[string]overlapSemanticRecord),
	}, nil
}

type overlapPorts struct {
	activity, transcript, semantic, speech, result, invocation, model, segmentation, tts, playback, release element.InputPort
	modelCancel, segmentationCancel, ttsCancel, playbackCancel                                              element.OutputPort
	safeResult, safeRelease, decision, state, agentOutput, resolved                                         element.OutputPort
}

func overlapPortsFrom(ports element.Ports) (overlapPorts, error) {
	if ports == nil {
		return overlapPorts{}, errors.New("interaction.OverlapBargeIn has nil ports")
	}
	var result overlapPorts
	for _, input := range []struct {
		name string
		set  *element.InputPort
	}{
		{"activity", &result.activity}, {"transcript", &result.transcript},
		{"semantic", &result.semantic},
		{"speech", &result.speech}, {"result", &result.result},
		{"invocation", &result.invocation}, {"model", &result.model},
		{"segmentation", &result.segmentation}, {"tts", &result.tts},
		{"playback", &result.playback}, {"release", &result.release},
	} {
		port, err := ports.Input(input.name)
		if err != nil {
			return overlapPorts{}, err
		}
		*input.set = port
	}
	for _, output := range []struct {
		name string
		set  *element.OutputPort
	}{
		{"model_cancel", &result.modelCancel}, {"segmentation_cancel", &result.segmentationCancel},
		{"tts_cancel", &result.ttsCancel}, {"playback_cancel", &result.playbackCancel},
		{"safe_result", &result.safeResult}, {"safe_release", &result.safeRelease},
		{"decision", &result.decision}, {"state", &result.state},
		{"agent_output", &result.agentOutput}, {"resolved", &result.resolved},
	} {
		port, err := ports.Output(output.name)
		if err != nil {
			return overlapPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

type overlapRun struct {
	streamID                 string
	protectedStreamID        string
	sourceRevision           uint64
	observationRevision      uint64
	act                      coreinteraction.Act
	invocationSeen           bool
	modelActive              bool
	modelTerminal            bool
	modelCancelIssued        bool
	segmentationActive       bool
	segmentationTerminal     bool
	segmentationCancelIssued bool
}

type overlapUtterance struct {
	runID                string
	text                 string
	ttsActive            bool
	ttsTerminal          bool
	ttsCancelIssued      bool
	playbackActive       bool
	playbackTerminal     bool
	playbackCancelIssued bool
	playbackAudible      bool
}

type overlapInputKind uint8

const (
	overlapInputActivity overlapInputKind = iota + 1
	overlapInputTranscript
	overlapInputSemantic
	overlapInputSpeech
	overlapInputResult
	overlapInputInvocation
	overlapInputModel
	overlapInputSegmentation
	overlapInputTTS
	overlapInputPlayback
	overlapInputRelease
)

type overlapInput struct {
	kind     overlapInputKind
	envelope element.Envelope
}

type overlapClassification struct {
	generation uint64
	streamID   string
	cause      element.Envelope
	revision   uint64
	evidence   coreinteraction.OverlapEvidence
}

type overlapClassificationRequest struct {
	streamID    string
	cause       element.Envelope
	observation coreperception.Observation
}

type overlapSemanticRecord struct {
	cause    element.Envelope
	decision policyelements.SemanticDecision
}

type overlapSpeech struct {
	streamID           string
	activityItemID     string
	activityCause      element.Envelope
	startedNS          uint64
	overlapStartedNS   uint64
	deadlineNS         uint64
	timerGeneration    uint64
	armed              bool
	lastSourceRevision uint64
	evidence           coreinteraction.OverlapEvidence
	classificationEnd  bool
	cancelIssued       bool
}

type overlapBargeInRunner struct {
	instance         string
	config           OverlapBargeInConfig
	clock            graphruntime.Clock
	scheduler        clock.Scheduler
	sequences        *graphruntime.SequenceAllocator
	registry         *policyelements.SemanticDeciderRegistry
	registryRevision uint64
	descriptor       policyelements.SemanticDeciderDescriptor
	resolution       element.ResolutionReporter
	ports            overlapPorts

	decider    policyelements.SemanticDecider
	classifier coreinteraction.OverlapClassifier
	sessionID  string

	runs                   map[string]*overlapRun
	terminalRuns           map[string]struct{}
	terminalRunOrder       []string
	utterances             map[string]*overlapUtterance
	terminalUtterances     map[string]struct{}
	terminalUtteranceOrder []string
	semantic               map[string]overlapSemanticRecord
	semanticOrder          []string
	speech                 *overlapSpeech
	timer                  clock.Timer
	classifyCancel         context.CancelFunc
	classifyBusy           bool
	pendingClassification  *overlapClassificationRequest
	classifyGeneration     uint64
	revisionSequence       uint64
	state                  OverlapState
	agentOutputRevision    uint64
}

func (runner *overlapBargeInRunner) Run(parent context.Context) (runErr error) {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if runner.registry != nil {
		decider, descriptor, err := runner.registry.Open(runner.config.Decider)
		if err != nil {
			return err
		}
		runner.decider, runner.descriptor = decider, descriptor
		classifier, err := coreinteraction.NewModelOverlapClassifier(decider)
		if err != nil {
			return errors.Join(err, closeOverlapDecider(decider))
		}
		runner.classifier = classifier
		defer func() { runErr = errors.Join(runErr, closeOverlapDecider(decider)) }()
	}
	if err := runner.reportResolution(); err != nil {
		return err
	}
	if err := runner.publishResolution(ctx); err != nil {
		return err
	}
	if err := runner.publishState(ctx, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}

	inputs := make(chan overlapInput)
	fired := make(chan uint64)
	classified := make(chan overlapClassification, 1)
	failures := make(chan error, 1)
	var receivers sync.WaitGroup
	for _, source := range []struct {
		kind overlapInputKind
		port element.InputPort
	}{
		{overlapInputActivity, runner.ports.activity},
		{overlapInputTranscript, runner.ports.transcript},
		{overlapInputSemantic, runner.ports.semantic},
		{overlapInputSpeech, runner.ports.speech},
		{overlapInputResult, runner.ports.result},
		{overlapInputInvocation, runner.ports.invocation},
		{overlapInputModel, runner.ports.model},
		{overlapInputSegmentation, runner.ports.segmentation},
		{overlapInputTTS, runner.ports.tts},
		{overlapInputPlayback, runner.ports.playback},
		{overlapInputRelease, runner.ports.release},
	} {
		receivers.Add(1)
		go receiveOverlapInput(ctx, source.kind, source.port, inputs, failures, &receivers)
	}
	defer func() {
		runner.stopTimer()
		runner.stopClassification()
		cancel(nil)
		receivers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case input := <-inputs:
			if err := runner.accept(ctx, input, fired, classified); err != nil {
				return err
			}
		case generation := <-fired:
			if err := runner.onDeadline(ctx, generation); err != nil {
				return err
			}
		case result := <-classified:
			if err := runner.onClassification(ctx, result, classified); err != nil {
				return err
			}
		}
	}
}

func (runner *overlapBargeInRunner) accept(
	ctx context.Context, input overlapInput, fired chan<- uint64,
	classified chan<- overlapClassification,
) error {
	if err := runner.validateInputEnvelope(input.envelope); err != nil {
		return err
	}
	if input.kind == overlapInputResult {
		result, err := runner.acceptSafeResult(input.envelope)
		if err != nil {
			return err
		}
		if !runner.hasAgentWork() {
			runner.disarmOverlap()
		}
		// The state update and its safe-text-terminal ancestry become visible
		// before the result can reach commit, provenance, or the session adapter.
		// This is an ordering barrier, not an observation-only fork.
		if err := runner.publishState(ctx, input.envelope); err != nil {
			return err
		}
		return runner.publishSafeResult(ctx, input.envelope, result)
	}
	if input.kind == overlapInputRelease {
		receipt, err := runner.acceptPlaybackRelease(input.envelope)
		if err != nil {
			return err
		}
		if !runner.hasAgentWork() {
			runner.disarmOverlap()
		}
		// A client-visible turn release is a permit for subsequent user input.
		// Retire the exact utterance before that permit can cross the graph
		// boundary; merely observing playback status on another lane is racy.
		if err := runner.publishState(ctx, input.envelope); err != nil {
			return err
		}
		return runner.publishSafeRelease(ctx, input.envelope, receipt)
	}
	var err error
	switch input.kind {
	case overlapInputActivity:
		err = runner.acceptActivity(ctx, input.envelope, fired)
	case overlapInputTranscript:
		err = runner.acceptTranscript(ctx, input.envelope, classified)
	case overlapInputSemantic:
		err = runner.acceptSemantic(ctx, input.envelope)
	case overlapInputSpeech:
		err = runner.acceptSpeechSegment(ctx, input.envelope)
	case overlapInputInvocation:
		err = runner.acceptInvocation(ctx, input.envelope, fired)
	case overlapInputModel:
		err = runner.acceptModel(ctx, input.envelope)
	case overlapInputSegmentation:
		err = runner.acceptSegmentation(ctx, input.envelope)
	case overlapInputTTS:
		err = runner.acceptSpeechTransition(ctx, input.envelope, speechelements.StageSynthesis, fired)
	case overlapInputPlayback:
		err = runner.acceptSpeechTransition(ctx, input.envelope, speechelements.StagePlayback, fired)
	default:
		err = errors.New("overlap barge-in received an unknown input")
	}
	if err != nil {
		return err
	}
	if !runner.hasAgentWork() {
		runner.disarmOverlap()
	}
	return runner.publishState(ctx, input.envelope)
}

func (runner *overlapBargeInRunner) acceptActivity(
	ctx context.Context, envelope element.Envelope, fired chan<- uint64,
) error {
	activity, ok := overlapSpeechActivityPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "activity", "invalid_payload",
			fmt.Sprintf("speech activity payload has type %T", envelope.Payload))
	}
	if !boundedOverlapIdentity(activity.StreamID) || strings.TrimSpace(activity.Source) == "" {
		return runner.refuse(ctx, envelope, "activity", "invalid_activity",
			"speech activity requires a bounded stream and source")
	}
	if streamID, err := exactOverlapIdentity(
		activity.StreamID, envelope.SourceID, envelope.CancellationScope,
	); err != nil || streamID != activity.StreamID {
		if err == nil {
			err = errors.New("activity stream address is incomplete")
		}
		return runner.refuse(ctx, envelope, "activity", "conflicting_stream_id", err.Error())
	}
	switch activity.Kind {
	case acousticelements.SpeechStarted:
		if runner.speech != nil {
			code := "overlapping_stream"
			message := "a second speech stream started before the active stream stopped"
			if runner.speech.streamID == activity.StreamID {
				code, message = "duplicate_start", "speech stream started twice"
			}
			return runner.refuse(ctx, envelope, "activity", code, message)
		}
		now := runner.clock.NowNS()
		runner.speech = &overlapSpeech{
			streamID: activity.StreamID, activityItemID: envelope.ItemID,
			activityCause: envelope.Clone(), startedNS: now,
		}
		if runner.hasActionableOverlapWork() {
			if err := runner.armOverlap(ctx, fired); err != nil {
				return err
			}
			runner.state.Observed++
			_, err := runner.publishDecision(ctx, envelope, OverlapDecision{
				Kind: OverlapObserved, Trigger: "acoustic_start", StreamID: activity.StreamID,
				Policy: runner.policyName(), Reason: "user speech began while connected agent work was active",
			})
			return err
		}
		return nil
	case acousticelements.SpeechStopped:
		if runner.speech == nil || runner.speech.streamID != activity.StreamID {
			return runner.refuse(ctx, envelope, "activity", "unmatched_stop",
				"speech stop has no matching active stream")
		}
		runner.disarmOverlap()
		runner.speech = nil
		return nil
	default:
		return runner.refuse(ctx, envelope, "activity", "invalid_activity",
			fmt.Sprintf("unsupported speech activity kind %q", activity.Kind))
	}
}

func (runner *overlapBargeInRunner) acceptTranscript(
	ctx context.Context, envelope element.Envelope, classified chan<- overlapClassification,
) error {
	observation, ok := overlapObservationPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "transcript", "invalid_payload",
			fmt.Sprintf("transcript payload has type %T", envelope.Payload))
	}
	streamID, addressErr := exactOverlapIdentity(envelope.SourceID, envelope.CancellationScope)
	if addressErr != nil {
		return runner.refuse(ctx, envelope, "transcript", "conflicting_stream_id", addressErr.Error())
	}
	if runner.speech == nil || streamID != runner.speech.streamID ||
		runner.speech.cancelIssued || runner.speech.classificationEnd || !runner.hasActionableOverlapWork() {
		runner.state.Ignored++
		_, err := runner.publishDecision(ctx, envelope, OverlapDecision{
			Kind: OverlapIgnored, Trigger: "transcript", StreamID: streamID,
			EvidenceItemID: envelope.ItemID, SourceRevision: observation.Revision,
			Policy: runner.policyName(), Reason: "transcript did not describe an actionable active overlap",
		})
		return err
	}
	if err := validateOverlapObservation(observation); err != nil {
		return runner.refuse(ctx, envelope, "transcript", "invalid_transcript", err.Error())
	}
	if observation.Revision <= runner.speech.lastSourceRevision {
		runner.state.Ignored++
		_, err := runner.publishDecision(ctx, envelope, OverlapDecision{
			Kind: OverlapIgnored, Trigger: "transcript", StreamID: streamID,
			EvidenceItemID: envelope.ItemID, SourceRevision: observation.Revision,
			Policy: runner.policyName(), Reason: "transcript revision was stale or duplicated",
		})
		return err
	}
	if runner.speech.lastSourceRevision != 0 &&
		observation.Supersedes != runner.speech.lastSourceRevision {
		return runner.refuse(ctx, envelope, "transcript", "revision_gap",
			"overlap transcript does not supersede the last observed revision")
	}
	runner.speech.lastSourceRevision = observation.Revision
	if runner.classifier == nil {
		return nil
	}
	request := &overlapClassificationRequest{
		streamID: streamID, cause: envelope.Clone(), observation: observation,
	}
	// At most one provider call may be executing. A newer revision cancels the
	// old logical decision and replaces the single pending slot; even a broken
	// provider that ignores context cancellation therefore cannot create an
	// unbounded goroutine/retry fan-out.
	if runner.classifyBusy {
		if runner.classifyCancel != nil {
			runner.classifyCancel()
			runner.classifyCancel = nil
		}
		runner.classifyGeneration++
		runner.pendingClassification = request
		runner.speech.evidence = ""
		return nil
	}
	return runner.startClassification(ctx, request, classified)
}

// acceptSemantic applies the same typed transcript decision that authorized
// generation to work which has already crossed that authorization boundary.
// Decision and invocation are independent lossless lanes, so the latest
// decision is retained and checked again when a reordered invocation arrives.
// A plain listen supersedes stale ordinary/speculative speech, but continued
// evidence from the utterance which caused a deliberate speak-through or
// interrupt cannot revoke that output. Only the explicit stop-speaking act can
// do so.
func (runner *overlapBargeInRunner) acceptSemantic(
	ctx context.Context, envelope element.Envelope,
) error {
	decision, ok := overlapSemanticDecisionPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "semantic_revision", "invalid_payload",
			fmt.Sprintf("semantic decision payload has type %T", envelope.Payload))
	}
	if decision.Operation != "committed" {
		runner.state.Ignored++
		return nil
	}
	if !boundedOverlapIdentity(decision.StreamID) || decision.SourceRevision == 0 ||
		!boundedOverlapIdentity(decision.EvidenceItemID) ||
		strings.TrimSpace(decision.Policy) == "" ||
		!slices.Contains(coreinteraction.AllActs(), decision.Act) {
		return runner.refuse(ctx, envelope, "semantic_revision", "invalid_decision",
			"committed semantic decision lacks a canonical stream, revision, evidence, policy, or act")
	}
	previous, found := runner.semantic[decision.StreamID]
	if found && previous.decision.SourceRevision >= decision.SourceRevision {
		runner.state.Ignored++
		return nil
	}
	if !found {
		runner.semanticOrder = append(runner.semanticOrder, decision.StreamID)
	}
	runner.semantic[decision.StreamID] = overlapSemanticRecord{
		cause: envelope.Clone(), decision: decision,
	}
	for len(runner.semanticOrder) > runner.config.MaxActiveRuns {
		oldest := runner.semanticOrder[0]
		runner.semanticOrder = runner.semanticOrder[1:]
		delete(runner.semantic, oldest)
	}
	if decision.Act == coreinteraction.ActKeepSpeaking && runner.speech != nil &&
		runner.speech.streamID == decision.StreamID {
		runner.protectActiveContinuation(decision.StreamID)
		return runner.keepActive(
			ctx, envelope, "semantic_revision", decision.SourceRevision,
			runner.speech.evidence,
			"the transcript-event policy explicitly kept the active voice output",
			false,
		)
	}
	runIDs := runner.runsSupersededBy(decision)
	if len(runIDs) == 0 {
		return nil
	}
	reason := "newer transcript evidence selected listen and superseded stale agent output"
	if decision.Act == coreinteraction.ActStopSpeaking {
		reason = "the transcript-event policy explicitly selected stop-speaking"
	}
	return runner.cancelSelectedRuns(ctx, envelope, "semantic_revision", decision.Policy, runIDs, reason)
}

func (runner *overlapBargeInRunner) runsSupersededBy(
	decision policyelements.SemanticDecision,
) []string {
	if decision.Act != coreinteraction.ActStaySilent &&
		decision.Act != coreinteraction.ActStopSpeaking {
		return nil
	}
	var runIDs []string
	for runID, run := range runner.runs {
		if run == nil {
			continue
		}
		if decision.Act == coreinteraction.ActStaySilent {
			if run.streamID != decision.StreamID || run.sourceRevision >= decision.SourceRevision ||
				deliberateSpokeOver(run.act) {
				continue
			}
		}
		if run.modelActive || run.segmentationActive || runner.runHasActiveUtterance(runID) {
			runIDs = append(runIDs, runID)
		}
	}
	sort.Strings(runIDs)
	return runIDs
}

func deliberateSpokeOver(act coreinteraction.Act) bool {
	return act == coreinteraction.ActSpeakThrough || act == coreinteraction.ActInterrupt
}

// protectActiveContinuation records the semantic meaning of keep-speaking:
// the current ASR stream is a continuation through which deliberate output
// remains valid. Without this transition only the first partial sees the
// model's decision; every later revision looks unrelated and can oscillate
// between keep and stop despite carrying the same growing utterance.
func (runner *overlapBargeInRunner) protectActiveContinuation(streamID string) {
	for runID, run := range runner.runs {
		if run == nil || !deliberateSpokeOver(run.act) ||
			(!run.modelActive && !run.segmentationActive && !runner.runHasActiveUtterance(runID)) {
			continue
		}
		run.protectedStreamID = streamID
	}
}

// acceptSpeechSegment retains only the bounded, safe-to-speak text already
// selected by the graph. Overlap classification needs this observation to
// distinguish a relevant correction or reply from an unrelated remark in the
// room; asking the policy model to infer that relationship from the new words
// alone makes the two situations observationally identical.
func (runner *overlapBargeInRunner) acceptSpeechSegment(
	ctx context.Context, envelope element.Envelope,
) error {
	segment, ok := overlapTextSegmentPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "speech", "invalid_payload",
			fmt.Sprintf("speech segment payload has type %T", envelope.Payload))
	}
	utteranceID, err := exactOverlapIdentity(segment.ID, envelope.CancellationScope)
	if err != nil || !boundedOverlapIdentity(utteranceID) {
		if err == nil {
			err = errors.New("speech segment requires a bounded utterance ID")
		}
		return runner.refuse(ctx, envelope, "speech", "invalid_utterance_id", err.Error())
	}
	runID := strings.TrimSpace(envelope.RunID)
	if !boundedOverlapIdentity(runID) {
		return runner.refuse(ctx, envelope, "speech", "invalid_run_id",
			"speech segment requires a bounded producing run ID")
	}
	text := strings.TrimSpace(segment.Text)
	if text == "" || text != segment.Text || len(text) > maximumOverlapTranscript ||
		!utf8.ValidString(text) || !carriesOverlapSpeech(text) {
		return runner.refuse(ctx, envelope, "speech", "invalid_text",
			"speech segment requires bounded canonical spoken text")
	}
	if _, terminal := runner.terminalUtterances[utteranceID]; terminal {
		return nil
	}
	utterance := runner.utterances[utteranceID]
	if utterance == nil {
		if len(runner.utterances) >= runner.config.MaxUtterances {
			return runner.refuse(ctx, envelope, "speech", "utterance_bound",
				"overlap active-utterance bound reached")
		}
		utterance = &overlapUtterance{runID: runID}
		runner.utterances[utteranceID] = utterance
	}
	if utterance.runID != "" && utterance.runID != runID {
		return runner.refuse(ctx, envelope, "speech", "run_changed",
			"speech utterance changed its producing run")
	}
	if utterance.text != "" {
		if utterance.text == text {
			return nil
		}
		return runner.refuse(ctx, envelope, "speech", "text_changed",
			"prepared speech text changed for an existing utterance")
	}
	utterance.runID = runID
	utterance.text = text
	return nil
}

// acceptSafeResult is the ordered completion barrier between foreground model
// preparation and every downstream result consumer. A validated final result
// proves the provider callback is closed, even when its independently drained
// Outcome arrives later. Session invocation also starts a conservative speech
// horizon before model output is known; a result with no speakable assistant
// text is causally downstream of an exact SafePreparedText TextEnd and can
// retire that provisional horizon without guessing from outcome arrival order.
func (runner *overlapBargeInRunner) acceptSafeResult(
	envelope element.Envelope,
) (cognitionelements.Result, error) {
	result, ok := cognitionResultPayload(envelope.Payload)
	if !ok {
		return cognitionelements.Result{}, fmt.Errorf(
			"overlap barge-in safe result has payload %T", envelope.Payload,
		)
	}
	if err := validateCognitionResult(result); err != nil {
		return cognitionelements.Result{}, fmt.Errorf("overlap barge-in safe result: %w", err)
	}
	runID, err := exactOverlapIdentity(result.RunID, envelope.RunID)
	if err != nil || !boundedOverlapIdentity(runID) {
		if err == nil {
			err = errors.New("safe result requires a bounded run ID")
		}
		return cognitionelements.Result{}, err
	}
	if _, terminal := runner.terminalRuns[runID]; terminal {
		return result, nil
	}
	run, err := runner.ensureRun(runID)
	if err != nil {
		return cognitionelements.Result{}, err
	}
	// TextModel publishes Result only after the provider callback is closed and
	// its complete prepared artifact is immutable. It is therefore stronger
	// completion evidence than the independently drained model Outcome lane.
	run.modelActive = false
	run.modelTerminal = true
	if strings.TrimSpace(result.AssistantText) == "" {
		run.segmentationActive = false
		run.segmentationTerminal = true
	}
	runner.pruneRun(runID)
	return result, nil
}

func (runner *overlapBargeInRunner) publishSafeResult(
	ctx context.Context, cause element.Envelope, result cognitionelements.Result,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".safe-result")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = safeModelResultType
	envelope.ItemID = fmt.Sprintf("%s:safe_result:%d", runner.instance, sequence)
	envelope.Sequence = sequence
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = result
	return broadcastInteraction(ctx, runner.ports.safeResult, envelope)
}

// acceptPlaybackRelease treats the post-sink release receipt as the causal
// terminal for both speech stages of one exact utterance. Synthesis necessarily
// emitted AudioEnd before Playback could release the sink reservation, but the
// independently connected TTS and playback status lanes may drain later. The
// release barrier must therefore tombstone both stages without waiting for
// those observation-only terminals.
func (runner *overlapBargeInRunner) acceptPlaybackRelease(
	envelope element.Envelope,
) (speechelements.PlaybackReceipt, error) {
	receipt, ok := overlapPlaybackReceiptPayload(envelope.Payload)
	if !ok {
		return speechelements.PlaybackReceipt{}, fmt.Errorf(
			"overlap barge-in playback release has payload %T", envelope.Payload,
		)
	}
	if receipt.Kind != speechelements.PlaybackReleased || receipt.Sequence == 0 {
		return speechelements.PlaybackReceipt{}, errors.New(
			"overlap barge-in playback release requires a released receipt with a positive sequence",
		)
	}
	utteranceID, err := exactOverlapIdentity(
		receipt.Utterance.ID, envelope.SourceID, envelope.CancellationScope,
	)
	if err != nil || !boundedOverlapIdentity(utteranceID) {
		if err == nil {
			err = errors.New("playback release requires a bounded utterance ID")
		}
		return speechelements.PlaybackReceipt{}, err
	}
	runID := strings.TrimSpace(envelope.RunID)
	if !boundedOverlapIdentity(runID) {
		return speechelements.PlaybackReceipt{}, errors.New(
			"playback release requires a bounded producing run ID",
		)
	}
	text := strings.TrimSpace(receipt.Utterance.Text)
	failedBeforeAudio := !receipt.Outcome.Completed && receipt.Outcome.PlayedMS == 0 &&
		strings.TrimSpace(receipt.Outcome.Reason) != ""
	if text == "" || text != receipt.Utterance.Text || len(text) > maximumOverlapTranscript ||
		!utf8.ValidString(text) ||
		(!carriesOverlapSpeech(text) && !(failedBeforeAudio && punctuationOnlyOverlapText(text))) {
		return speechelements.PlaybackReceipt{}, errors.New(
			"playback release requires bounded canonical spoken text",
		)
	}
	if _, terminal := runner.terminalUtterances[utteranceID]; terminal {
		return receipt, nil
	}
	utterance := runner.utterances[utteranceID]
	if utterance != nil {
		if utterance.runID != "" && utterance.runID != runID {
			return speechelements.PlaybackReceipt{}, errors.New(
				"playback release changed its producing run",
			)
		}
		if utterance.text != "" && utterance.text != text {
			return speechelements.PlaybackReceipt{}, errors.New(
				"playback release changed its prepared speech text",
			)
		}
	}
	delete(runner.utterances, utteranceID)
	runner.rememberTerminalUtterance(utteranceID)
	runner.pruneRun(runID)
	return receipt, nil
}

func (runner *overlapBargeInRunner) publishSafeRelease(
	ctx context.Context, cause element.Envelope, receipt speechelements.PlaybackReceipt,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".safe-release")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = speechelements.PlaybackReceiptType()
	envelope.ItemID = fmt.Sprintf("%s:safe_release:%d", runner.instance, sequence)
	envelope.Sequence = sequence
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = receipt
	return broadcastInteraction(ctx, runner.ports.safeRelease, envelope)
}

func (runner *overlapBargeInRunner) startClassification(
	ctx context.Context, request *overlapClassificationRequest,
	classified chan<- overlapClassification,
) error {
	if request == nil || runner.classifier == nil || runner.classifyBusy {
		return nil
	}
	if runner.speech == nil || runner.speech.streamID != request.streamID ||
		runner.speech.cancelIssued || runner.speech.classificationEnd || !runner.hasActionableOverlapWork() {
		return nil
	}
	observation := request.observation
	runner.classifyGeneration++
	generation := runner.classifyGeneration
	runner.revisionSequence++
	revisionID := runner.revisionSequence
	decisionCtx, cancel := context.WithTimeout(
		ctx, time.Duration(runner.descriptor.DecisionTimeoutMS)*time.Millisecond,
	)
	runner.classifyCancel = cancel
	runner.classifyBusy = true
	runner.pendingClassification = nil
	runner.speech.evidence = ""
	stable := observation.StableText
	unstable := observation.Text
	if stable != "" {
		unstable = strings.TrimPrefix(observation.Text, stable)
	}
	now := runner.clock.NowNS()
	decision := coreinteraction.Context{
		NowNS: now,
		Duplex: coresession.Snapshot{
			UserSpeaking: true, AgentSpeaking: true, Phase: coresession.PhaseOverlap,
			UserSpeechStartedNS: runner.speech.overlapStartedNS,
		},
		Revision: coreinteraction.Revision{
			ID: revisionID, StableText: stable, UnstableText: unstable,
			Final: observation.Final, ObservedNS: firstNonzeroOverlap(observation.OccurredNS, now),
		},
		Situation: &coreinteraction.Situation{
			AgentSpeaking: true,
			AgentSaying:   runner.activeAgentText(),
			Speaker:       "user",
			Speaking:      true,
			Heard:         observation.Text,
			HeardSince:    observation.Text,
		},
	}
	cause := request.cause.Clone()
	stream := request.streamID
	go func() {
		evidence := runner.classifier.Classify(decisionCtx, decision)
		result := overlapClassification{
			generation: generation, streamID: stream, cause: cause,
			revision: observation.Revision, evidence: evidence,
		}
		select {
		case classified <- result:
		case <-ctx.Done():
		}
	}()
	return nil
}

func (runner *overlapBargeInRunner) acceptInvocation(
	ctx context.Context, envelope element.Envelope, fired chan<- uint64,
) error {
	outcome, ok := overlapInvocationPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "invocation", "invalid_payload",
			fmt.Sprintf("invocation outcome payload has type %T", envelope.Payload))
	}
	if outcome.Kind != policyelements.SessionInvocationEmitted {
		return nil
	}
	runID, err := exactOverlapIdentity(outcome.GenerationID, envelope.RunID)
	if err != nil {
		return runner.refuse(ctx, envelope, "invocation", "conflicting_run_id", err.Error())
	}
	if !boundedOverlapIdentity(runID) {
		return runner.refuse(ctx, envelope, "invocation", "invalid_run_id",
			"emitted invocation requires a bounded generation ID")
	}
	if _, terminal := runner.terminalRuns[runID]; terminal {
		return nil
	}
	run, err := runner.ensureRun(runID)
	if err != nil {
		return runner.refuse(ctx, envelope, "invocation", "active_run_bound", err.Error())
	}
	if run.invocationSeen {
		return nil
	}
	if outcome.Operation == "committed" {
		if !boundedOverlapIdentity(outcome.StreamID) || outcome.SourceRevision == 0 ||
			!slices.Contains([]coreinteraction.Act{
				coreinteraction.ActAnswer, coreinteraction.ActSpeakThrough,
				coreinteraction.ActInterrupt,
			}, outcome.Act) {
			return runner.refuse(ctx, envelope, "invocation", "invalid_semantic_invocation",
				"committed invocation lacks its exact stream, source revision, or voice act")
		}
		run.streamID = outcome.StreamID
		run.sourceRevision = outcome.SourceRevision
		run.observationRevision = outcome.ObservationRevision
		run.act = outcome.Act
	}
	run.invocationSeen = true
	if !run.modelTerminal {
		run.modelActive = true
	}
	if !run.segmentationTerminal {
		run.segmentationActive = true
	}
	if !run.modelActive && !run.segmentationActive {
		runner.pruneRun(runID)
		return nil
	}
	if runner.speech != nil && runner.speech.cancelIssued {
		return runner.cancelActive(ctx, envelope, "late_invocation", 0,
			runner.speech.evidence, "connected cognition started after overlap cancellation", false)
	}
	if record, found := runner.semantic[run.streamID]; found &&
		record.decision.SourceRevision > run.sourceRevision &&
		slices.Contains(runner.runsSupersededBy(record.decision), runID) {
		reason := "newer transcript evidence selected listen and superseded stale agent output"
		if record.decision.Act == coreinteraction.ActStopSpeaking {
			reason = "the transcript-event policy explicitly selected stop-speaking"
		}
		return runner.cancelSelectedRuns(
			ctx, record.cause, "semantic_revision", record.decision.Policy,
			[]string{runID}, reason,
		)
	}
	return runner.maybeArmOverlap(ctx, fired)
}

func (runner *overlapBargeInRunner) acceptModel(
	ctx context.Context, envelope element.Envelope,
) error {
	outcome, ok := overlapModelOutcomePayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "model", "invalid_payload",
			fmt.Sprintf("model outcome payload has type %T", envelope.Payload))
	}
	if outcome.Operation != "generate" {
		return nil
	}
	runID, err := exactOverlapIdentity(outcome.RunID, envelope.RunID, envelope.CancellationScope)
	if err != nil || !boundedOverlapIdentity(runID) {
		if err == nil {
			err = errors.New("generation outcome requires a bounded run ID")
		}
		return runner.refuse(ctx, envelope, "model", "invalid_run_id", err.Error())
	}
	if _, terminal := runner.terminalRuns[runID]; terminal {
		return nil
	}
	run, ensureErr := runner.ensureRun(runID)
	if ensureErr != nil {
		return runner.refuse(ctx, envelope, "model", "active_run_bound", ensureErr.Error())
	}
	run.modelActive = false
	run.modelTerminal = true
	runner.pruneRun(runID)
	return nil
}

func (runner *overlapBargeInRunner) acceptSegmentation(
	ctx context.Context, envelope element.Envelope,
) error {
	outcome, ok := overlapSegmentationPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "segmentation", "invalid_payload",
			fmt.Sprintf("segmentation outcome payload has type %T", envelope.Payload))
	}
	if outcome.Kind == OutcomeIgnored {
		return nil
	}
	runID, err := exactOverlapIdentity(outcome.RunID, envelope.RunID, envelope.CancellationScope)
	if err != nil || !boundedOverlapIdentity(runID) {
		if err == nil {
			err = errors.New("segmentation outcome requires a bounded run ID")
		}
		return runner.refuse(ctx, envelope, "segmentation", "invalid_run_id", err.Error())
	}
	if _, terminal := runner.terminalRuns[runID]; terminal {
		return nil
	}
	run, ensureErr := runner.ensureRun(runID)
	if ensureErr != nil {
		return runner.refuse(ctx, envelope, "segmentation", "active_run_bound", ensureErr.Error())
	}
	run.segmentationActive = false
	run.segmentationTerminal = true
	runner.pruneRun(runID)
	return nil
}

func (runner *overlapBargeInRunner) acceptSpeechTransition(
	ctx context.Context, envelope element.Envelope, expected speechelements.Stage,
	fired chan<- uint64,
) error {
	transition, ok := overlapTransitionPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, string(expected), "invalid_payload",
			fmt.Sprintf("speech transition payload has type %T", envelope.Payload))
	}
	if transition.Stage != expected || !boundedOverlapIdentity(transition.UtteranceID) ||
		!validOverlapTransitionState(expected, transition.State) {
		return runner.refuse(ctx, envelope, string(expected), "invalid_transition",
			"speech transition changed stage, state, or omitted its utterance ID")
	}
	if utteranceID, err := exactOverlapIdentity(
		transition.UtteranceID, envelope.SourceID, envelope.CancellationScope,
	); err != nil || utteranceID != transition.UtteranceID {
		if err == nil {
			err = errors.New("speech transition address is incomplete")
		}
		return runner.refuse(ctx, envelope, string(expected), "conflicting_utterance_id", err.Error())
	}
	runID := strings.TrimSpace(envelope.RunID)
	if runID != "" && !boundedOverlapIdentity(runID) {
		return runner.refuse(ctx, envelope, string(expected), "invalid_run_id",
			"speech transition carries a noncanonical run ID")
	}
	if _, terminal := runner.terminalUtterances[transition.UtteranceID]; terminal {
		return nil
	}
	utterance := runner.utterances[transition.UtteranceID]
	active := overlapTransitionActive(transition)
	if utterance == nil {
		if len(runner.utterances) >= runner.config.MaxUtterances {
			return runner.refuse(ctx, envelope, string(expected), "utterance_bound",
				"overlap active-utterance bound reached")
		}
		utterance = &overlapUtterance{runID: runID}
		runner.utterances[transition.UtteranceID] = utterance
	}
	if utterance == nil {
		return nil
	}
	if utterance.runID != "" && runID != "" && utterance.runID != runID {
		return runner.refuse(ctx, envelope, string(expected), "run_changed",
			"speech utterance changed its producing run")
	}
	if utterance.runID == "" {
		utterance.runID = runID
	}
	if expected == speechelements.StageSynthesis {
		if active {
			if utterance.ttsTerminal {
				return nil
			}
			utterance.ttsActive = true
		} else {
			utterance.ttsActive = false
			utterance.ttsTerminal = true
			if transition.State != speechelements.StateGenerated {
				utterance.playbackActive = false
				utterance.playbackTerminal = true
			}
		}
	} else {
		if active {
			if utterance.playbackTerminal {
				return nil
			}
			utterance.playbackActive = true
			utterance.playbackAudible = transition.State == speechelements.StateEmitting ||
				transition.CrossedBoundary
		} else {
			utterance.playbackActive = false
			utterance.playbackAudible = false
			utterance.playbackTerminal = true
		}
	}
	if utterance.ttsTerminal && utterance.playbackTerminal {
		delete(runner.utterances, transition.UtteranceID)
		runner.rememberTerminalUtterance(transition.UtteranceID)
		runner.pruneRun(runID)
		return nil
	}
	if active {
		if runner.speech != nil && runner.speech.cancelIssued {
			return runner.cancelActive(ctx, envelope, "late_speech_lifecycle", 0,
				runner.speech.evidence, "connected speech work started after overlap cancellation", false)
		}
		return runner.maybeArmOverlap(ctx, fired)
	}
	return nil
}

func overlapTransitionActive(transition speechelements.Transition) bool {
	switch transition.State {
	case speechelements.StateGenerating, speechelements.StateQueued, speechelements.StateEmitting:
		return true
	default:
		return false
	}
}

func validOverlapTransitionState(stage speechelements.Stage, state speechelements.State) bool {
	switch stage {
	case speechelements.StageSynthesis:
		switch state {
		case speechelements.StateGenerating, speechelements.StateGenerated,
			speechelements.StateCancelled, speechelements.StateFailed, speechelements.StateRefused:
			return true
		}
	case speechelements.StagePlayback:
		switch state {
		case speechelements.StateQueued, speechelements.StateEmitting,
			speechelements.StatePlayed, speechelements.StateCancelled,
			speechelements.StateFailed, speechelements.StateRefused:
			return true
		}
	}
	return false
}

func (runner *overlapBargeInRunner) maybeArmOverlap(
	ctx context.Context, fired chan<- uint64,
) error {
	if runner.speech == nil || runner.speech.cancelIssued ||
		runner.speech.classificationEnd || !runner.hasActionableOverlapWork() {
		return nil
	}
	return runner.armOverlap(ctx, fired)
}

func (runner *overlapBargeInRunner) armOverlap(
	ctx context.Context, fired chan<- uint64,
) error {
	if runner.speech == nil || runner.speech.armed {
		return nil
	}
	now := runner.clock.NowNS()
	delay := time.Duration(runner.config.HoldMS) * time.Millisecond
	deadline := now + uint64(delay)
	if deadline < now {
		return errors.New("overlap deadline overflow")
	}
	runner.speech.overlapStartedNS = now
	runner.speech.deadlineNS = deadline
	runner.speech.armed = true
	runner.speech.timerGeneration++
	generation := runner.speech.timerGeneration
	runner.stopTimer()
	runner.timer = runner.scheduler.AfterFunc(delay, func() {
		select {
		case fired <- generation:
		case <-ctx.Done():
		}
	})
	return nil
}

func (runner *overlapBargeInRunner) onDeadline(ctx context.Context, generation uint64) error {
	if runner.speech == nil || generation != runner.speech.timerGeneration ||
		!runner.speech.armed || runner.speech.cancelIssued ||
		runner.speech.classificationEnd || !runner.hasActionableOverlapWork() {
		return nil
	}
	runner.timer = nil
	runner.stopClassification()
	cause := runner.speech.activityCause.Clone()
	cause.ItemID = runner.speech.activityItemID + ":overlap-deadline"
	cause.SourceID = runner.speech.streamID
	cause.CancellationScope = runner.speech.streamID
	if runner.speech.evidence == coreinteraction.OverlapBackchannel ||
		runner.speech.evidence == coreinteraction.OverlapSide {
		return runner.keepActive(ctx, cause, "semantic_deadline", 0,
			runner.speech.evidence, "overlap was classified as non-directed speech", true)
	}
	return runner.applyFallback(ctx, cause, "hold_timeout")
}

func (runner *overlapBargeInRunner) onClassification(
	ctx context.Context, result overlapClassification,
	classified chan<- overlapClassification,
) error {
	// Only one provider call can be live, so every received result retires the
	// physical worker even when a newer logical revision invalidated it.
	runner.classifyBusy = false
	if runner.classifyCancel != nil {
		runner.classifyCancel()
		runner.classifyCancel = nil
	}
	pending := runner.pendingClassification
	runner.pendingClassification = nil
	if pending != nil {
		if runner.speech != nil && runner.speech.streamID == pending.streamID &&
			runner.speech.armed && !runner.speech.cancelIssued &&
			!runner.speech.classificationEnd && runner.hasActionableOverlapWork() &&
			runner.clock.NowNS() < runner.speech.deadlineNS {
			return runner.startClassification(ctx, pending, classified)
		}
		return nil
	}
	if result.generation != runner.classifyGeneration {
		return nil
	}
	if runner.speech == nil || runner.speech.streamID != result.streamID ||
		!runner.speech.armed || runner.speech.cancelIssued ||
		runner.speech.classificationEnd || !runner.hasActionableOverlapWork() {
		return nil
	}
	runner.speech.evidence = result.evidence
	runner.state.Classified++
	switch result.evidence {
	case coreinteraction.OverlapDirected:
		return runner.cancelActive(ctx, result.cause, "semantic_transcript",
			result.revision, result.evidence, "overlapping speech is directed at the agent", true)
	case coreinteraction.OverlapBackchannel, coreinteraction.OverlapSide:
		if runner.clock.NowNS() >= runner.speech.deadlineNS {
			return runner.keepActive(ctx, result.cause, "semantic_transcript",
				result.revision, result.evidence, "overlap was classified as non-directed speech", true)
		}
		_, err := runner.publishDecision(ctx, result.cause, OverlapDecision{
			Kind: OverlapClassified, Trigger: "semantic_transcript", StreamID: result.streamID,
			EvidenceItemID: result.cause.ItemID, SourceRevision: result.revision,
			Evidence: result.evidence, Policy: runner.policyName(),
			Reason: "non-directed evidence is retained until the overlap deadline or a newer revision",
		})
		return err
	case coreinteraction.OverlapAmbiguous, "":
		if runner.clock.NowNS() >= runner.speech.deadlineNS {
			return runner.applyFallback(ctx, result.cause, "classification_inconclusive")
		}
		_, err := runner.publishDecision(ctx, result.cause, OverlapDecision{
			Kind: OverlapClassified, Trigger: "semantic_transcript", StreamID: result.streamID,
			EvidenceItemID: result.cause.ItemID, SourceRevision: result.revision,
			Evidence: result.evidence, Policy: runner.policyName(),
			Reason: "semantic evidence remains inconclusive before the overlap deadline",
		})
		return err
	default:
		return runner.refuse(ctx, result.cause, "classification", "invalid_evidence",
			fmt.Sprintf("overlap classifier returned unsupported evidence %q", result.evidence))
	}
}

func (runner *overlapBargeInRunner) keepActive(
	ctx context.Context, cause element.Envelope, trigger string, revision uint64,
	evidence coreinteraction.OverlapEvidence, reason string, publishState bool,
) error {
	if runner.speech == nil || runner.speech.cancelIssued || runner.speech.classificationEnd {
		return nil
	}
	runner.speech.classificationEnd = true
	runner.stopTimer()
	runner.stopClassification()
	runner.state.Kept++
	_, err := runner.publishDecision(ctx, cause, OverlapDecision{
		Kind: OverlapKept, Trigger: trigger, StreamID: runner.speech.streamID,
		EvidenceItemID: cause.ItemID, SourceRevision: revision, Evidence: evidence,
		Policy: runner.policyName(), Reason: reason,
	})
	if err != nil {
		return err
	}
	if publishState {
		return runner.publishState(ctx, cause)
	}
	return nil
}

func (runner *overlapBargeInRunner) applyFallback(
	ctx context.Context, cause element.Envelope, trigger string,
) error {
	if runner.config.Unclassified == OverlapFallbackKeep {
		return runner.keepActive(ctx, cause, trigger, 0, runner.speech.evidence,
			"configured unclassified-overlap fallback keeps speaking", true)
	}
	return runner.cancelActive(ctx, cause, trigger, 0, runner.speech.evidence,
		"configured unclassified-overlap fallback yields the floor", true)
}

func (runner *overlapBargeInRunner) cancelSelectedRuns(
	ctx context.Context, cause element.Envelope, trigger, policy string,
	runIDs []string, reason string,
) error {
	selected := make(map[string]struct{}, len(runIDs))
	modelIDs := make([]string, 0, len(runIDs))
	segmentIDs := make([]string, 0, len(runIDs))
	for _, runID := range runIDs {
		run := runner.runs[runID]
		if run == nil {
			continue
		}
		selected[runID] = struct{}{}
		if run.modelActive && !run.modelCancelIssued {
			run.modelCancelIssued = true
			modelIDs = append(modelIDs, runID)
		}
		if run.segmentationActive && !run.segmentationCancelIssued {
			run.segmentationCancelIssued = true
			segmentIDs = append(segmentIDs, runID)
		}
	}
	ttsIDs := make([]string, 0)
	playbackIDs := make([]string, 0)
	utteranceSet := make(map[string]struct{})
	for utteranceID, utterance := range runner.utterances {
		if utterance == nil {
			continue
		}
		if _, applies := selected[utterance.runID]; !applies {
			continue
		}
		if utterance.ttsActive && !utterance.ttsCancelIssued {
			utterance.ttsCancelIssued = true
			ttsIDs = append(ttsIDs, utteranceID)
			utteranceSet[utteranceID] = struct{}{}
		}
		if utterance.playbackActive && !utterance.playbackCancelIssued {
			utterance.playbackCancelIssued = true
			playbackIDs = append(playbackIDs, utteranceID)
			utteranceSet[utteranceID] = struct{}{}
		}
	}
	if len(modelIDs)+len(segmentIDs)+len(ttsIDs)+len(playbackIDs) == 0 {
		return nil
	}
	utteranceIDs := make([]string, 0, len(utteranceSet))
	for utteranceID := range utteranceSet {
		utteranceIDs = append(utteranceIDs, utteranceID)
	}
	for _, values := range [][]string{modelIDs, segmentIDs, ttsIDs, playbackIDs, utteranceIDs} {
		sort.Strings(values)
	}
	revision := uint64(0)
	streamID := ""
	if semantic, ok := overlapSemanticDecisionPayload(cause.Payload); ok {
		revision, streamID = semantic.SourceRevision, semantic.StreamID
	}
	runner.state.Canceled++
	decision, err := runner.publishDecision(ctx, cause, OverlapDecision{
		Kind: OverlapCanceled, Trigger: trigger, StreamID: streamID,
		EvidenceItemID: cause.ItemID, SourceRevision: revision,
		Policy: policy, Reason: reason, ActiveRunIDs: slices.Clone(runIDs),
		ActiveUtteranceIDs: utteranceIDs, ModelCancels: len(modelIDs),
		SegmentationCancels: len(segmentIDs), TTSCancels: len(ttsIDs),
		PlaybackCancels: len(playbackIDs),
	})
	if err != nil {
		return err
	}
	for _, runID := range modelIDs {
		if err := runner.publishRunCancel(ctx, runner.ports.modelCancel, decision, runID, reason); err != nil {
			return err
		}
	}
	for _, runID := range segmentIDs {
		if err := runner.publishRunCancel(ctx, runner.ports.segmentationCancel, decision, runID, reason); err != nil {
			return err
		}
	}
	for _, utteranceID := range ttsIDs {
		if err := runner.publishSpeechCancel(ctx, runner.ports.ttsCancel, decision, utteranceID, reason); err != nil {
			return err
		}
	}
	for _, utteranceID := range playbackIDs {
		if err := runner.publishSpeechCancel(ctx, runner.ports.playbackCancel, decision, utteranceID, reason); err != nil {
			return err
		}
	}
	return nil
}

func (runner *overlapBargeInRunner) runHasActiveUtterance(runID string) bool {
	for _, utterance := range runner.utterances {
		if utterance != nil && utterance.runID == runID &&
			(utterance.ttsActive || utterance.playbackActive) {
			return true
		}
	}
	return false
}

func (runner *overlapBargeInRunner) cancelActive(
	ctx context.Context, cause element.Envelope, trigger string, revision uint64,
	evidence coreinteraction.OverlapEvidence, reason string, publishState bool,
) error {
	if runner.speech == nil {
		return nil
	}
	firstDecision := !runner.speech.cancelIssued
	modelIDs := make([]string, 0, len(runner.runs))
	segmentIDs := make([]string, 0, len(runner.runs))
	runSet := make(map[string]struct{}, len(runner.runs))
	for runID, run := range runner.runs {
		if runner.runProtectedFromCurrentSpeech(run) {
			continue
		}
		if run.modelActive && !run.modelCancelIssued {
			run.modelCancelIssued = true
			modelIDs = append(modelIDs, runID)
			runSet[runID] = struct{}{}
		}
		if run.segmentationActive && !run.segmentationCancelIssued {
			run.segmentationCancelIssued = true
			segmentIDs = append(segmentIDs, runID)
			runSet[runID] = struct{}{}
		}
	}
	ttsIDs := make([]string, 0, len(runner.utterances))
	playbackIDs := make([]string, 0, len(runner.utterances))
	utteranceSet := make(map[string]struct{}, len(runner.utterances))
	for utteranceID, utterance := range runner.utterances {
		if utterance == nil {
			continue
		}
		if runner.runProtectedFromCurrentSpeech(runner.runs[utterance.runID]) {
			continue
		}
		if utterance.ttsActive && !utterance.ttsCancelIssued {
			utterance.ttsCancelIssued = true
			ttsIDs = append(ttsIDs, utteranceID)
			utteranceSet[utteranceID] = struct{}{}
		}
		if utterance.playbackActive && !utterance.playbackCancelIssued {
			utterance.playbackCancelIssued = true
			playbackIDs = append(playbackIDs, utteranceID)
			utteranceSet[utteranceID] = struct{}{}
		}
	}
	if !firstDecision && len(modelIDs)+len(segmentIDs)+len(ttsIDs)+len(playbackIDs) == 0 {
		return nil
	}
	runIDs := make([]string, 0, len(runSet))
	for runID := range runSet {
		runIDs = append(runIDs, runID)
	}
	utteranceIDs := make([]string, 0, len(utteranceSet))
	for utteranceID := range utteranceSet {
		utteranceIDs = append(utteranceIDs, utteranceID)
	}
	for _, values := range [][]string{runIDs, utteranceIDs, modelIDs, segmentIDs, ttsIDs, playbackIDs} {
		sort.Strings(values)
	}
	/*
		Cancellation identities stay independent even when a run and its speech
		are causally related. A graph may connect only some outputs, and late
		work on a previously canceled overlap receives its own addressed cancel.
	*/
	for _, runID := range runIDs {
		run := runner.runs[runID]
		if run == nil {
			return errors.New("overlap cancellation lost an active run")
		}
	}
	runner.speech.cancelIssued = true
	runner.stopTimer()
	runner.stopClassification()
	runner.state.Canceled++
	decision, err := runner.publishDecision(ctx, cause, OverlapDecision{
		Kind: OverlapCanceled, Trigger: trigger, StreamID: runner.speech.streamID,
		EvidenceItemID: cause.ItemID, SourceRevision: revision, Evidence: evidence,
		Policy: runner.policyName(), Reason: reason,
		ActiveRunIDs: runIDs, ActiveUtteranceIDs: utteranceIDs,
		ModelCancels: len(modelIDs), SegmentationCancels: len(segmentIDs),
		TTSCancels: len(ttsIDs), PlaybackCancels: len(playbackIDs),
	})
	if err != nil {
		return err
	}
	for _, runID := range modelIDs {
		if err := runner.publishRunCancel(ctx, runner.ports.modelCancel, decision, runID, reason); err != nil {
			return err
		}
	}
	for _, runID := range segmentIDs {
		if err := runner.publishRunCancel(ctx, runner.ports.segmentationCancel, decision, runID, reason); err != nil {
			return err
		}
	}
	for _, utteranceID := range ttsIDs {
		if err := runner.publishSpeechCancel(ctx, runner.ports.ttsCancel, decision, utteranceID, reason); err != nil {
			return err
		}
	}
	for _, utteranceID := range playbackIDs {
		if err := runner.publishSpeechCancel(ctx, runner.ports.playbackCancel, decision, utteranceID, reason); err != nil {
			return err
		}
	}
	if publishState {
		return runner.publishState(ctx, decision)
	}
	return nil
}

func (runner *overlapBargeInRunner) publishRunCancel(
	ctx context.Context, output element.OutputPort, decision element.Envelope, runID, reason string,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".overlap-run-cancel")
	if err != nil {
		return err
	}
	envelope := decision.Clone()
	envelope.Type = output.Type()
	envelope.ItemID = fmt.Sprintf("%s:run_cancel:%d", decision.ItemID, sequence)
	envelope.RunID = runID
	envelope.CancellationScope = runID
	envelope.Sequence = sequence
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, decision.ItemID)
	envelope.Payload = cognitionelements.Cancel{RunID: runID, Reason: reason}
	return broadcastInteraction(ctx, output, envelope)
}

func (runner *overlapBargeInRunner) publishSpeechCancel(
	ctx context.Context, output element.OutputPort, decision element.Envelope, utteranceID, reason string,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".overlap-speech-cancel")
	if err != nil {
		return err
	}
	envelope := decision.Clone()
	envelope.Type = output.Type()
	envelope.ItemID = fmt.Sprintf("%s:speech_cancel:%d", decision.ItemID, sequence)
	envelope.SourceID = utteranceID
	envelope.CancellationScope = utteranceID
	envelope.Sequence = sequence
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, decision.ItemID)
	envelope.Payload = speechelements.Cancel{UtteranceID: utteranceID, Reason: reason}
	return broadcastInteraction(ctx, output, envelope)
}

func (runner *overlapBargeInRunner) publishDecision(
	ctx context.Context, cause element.Envelope, decision OverlapDecision,
) (element.Envelope, error) {
	sequence, err := runner.sequences.Next(runner.instance + ".overlap-decision")
	if err != nil {
		return element.Envelope{}, err
	}
	decision.DecidedNS = runner.clock.NowNS()
	if runner.speech != nil {
		decision.OverlapStartedNS = runner.speech.overlapStartedNS
		decision.DeadlineNS = runner.speech.deadlineNS
		if decision.StreamID == "" {
			decision.StreamID = runner.speech.streamID
		}
	}
	decision.ActiveRunIDs = slices.Clone(decision.ActiveRunIDs)
	decision.ActiveUtteranceIDs = slices.Clone(decision.ActiveUtteranceIDs)
	envelope := cause.Clone()
	envelope.Type = overlapDecisionType
	envelope.ItemID = fmt.Sprintf("%s:overlap_decision:%d", runner.instance, sequence)
	envelope.Sequence = sequence
	envelope.CaptureNS = decision.DecidedNS
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = decision
	return envelope, broadcastInteraction(ctx, runner.ports.decision, envelope)
}

func (runner *overlapBargeInRunner) publishState(
	ctx context.Context, cause element.Envelope,
) error {
	runner.refreshState()
	runner.agentOutputRevision++
	runner.state.AgentOutput = runner.agentOutputSnapshot()
	runner.state.AgentOutput.Revision = runner.agentOutputRevision
	sequence, err := runner.sequences.Next(runner.instance + ".overlap-state")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = overlapStateType
	envelope.ItemID = fmt.Sprintf("%s:overlap_state:%d", runner.instance, sequence)
	envelope.Sequence = sequence
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = runner.state
	if err := broadcastInteraction(ctx, runner.ports.state, envelope); err != nil {
		return err
	}
	output := envelope.Clone()
	output.Type = runner.ports.agentOutput.Type()
	output.ItemID = fmt.Sprintf("%s:agent_output:%d", runner.instance, runner.agentOutputRevision)
	output.Payload = runner.state.AgentOutput
	return broadcastInteraction(ctx, runner.ports.agentOutput, output)
}

func (runner *overlapBargeInRunner) publishResolution(ctx context.Context) error {
	resolution := OverlapPolicyResolution{
		Decider: runner.config.Decider, HoldMS: runner.config.HoldMS,
		Unclassified: runner.config.Unclassified, RegistryRevision: runner.registryRevision,
	}
	if runner.config.Decider != "" {
		descriptor := runner.descriptor
		resolution.Descriptor = &descriptor
		digest, err := overlapSemanticDescriptorDigest(descriptor)
		if err != nil {
			return err
		}
		resolution.DescriptorDigest = digest
	}
	sequence, err := runner.sequences.Next(runner.instance + ".overlap-resolution")
	if err != nil {
		return err
	}
	envelope := element.Envelope{
		Type: overlapResolutionType, ItemID: fmt.Sprintf("%s:resolved:%d", runner.instance, sequence),
		Sequence: sequence, Payload: resolution,
	}
	return broadcastInteraction(ctx, runner.ports.resolved, envelope)
}

func (runner *overlapBargeInRunner) reportResolution() error {
	capabilities := []element.CapabilityResolution(nil)
	if runner.config.Decider != "" {
		digest, err := overlapSemanticDescriptorDigest(runner.descriptor)
		if err != nil {
			return err
		}
		provider := liveidentity.Artifact{
			ID:       "interaction-model://" + runner.descriptor.Provider,
			Revision: runner.descriptor.Model + "@" + runner.descriptor.Revision,
			Digest:   digest,
		}
		adapter := liveidentity.Artifact{
			ID:       "builtin://openrealtime/adapters/interaction.OverlapBargeIn-interaction.Decider",
			Revision: overlapBargeInRuntimeRevision,
		}
		capabilities = []element.CapabilityResolution{liveidentity.Capability(
			"interaction.overlap-classification",
			"openrealtime.interaction/OverlapClassifier-v1", provider, adapter,
		)}
	}
	return liveidentity.Report(runner.resolution, liveidentity.Artifact{
		ID: overlapBargeInRuntimeID, Revision: overlapBargeInRuntimeRevision,
	}, capabilities)
}

func overlapSemanticDescriptorDigest(
	descriptor policyelements.SemanticDeciderDescriptor,
) (string, error) {
	encoded, err := json.Marshal(descriptor)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (runner *overlapBargeInRunner) refuse(
	ctx context.Context, cause element.Envelope, trigger, code, message string,
) error {
	runner.state.Refused++
	_, err := runner.publishDecision(ctx, cause, OverlapDecision{
		Kind: OverlapRefused, Trigger: trigger, Policy: runner.policyName(),
		Reason: code + ": " + message,
	})
	return err
}

func (runner *overlapBargeInRunner) refreshState() {
	preserved := runner.state
	runner.state = OverlapState{
		Observed: preserved.Observed, Classified: preserved.Classified, Kept: preserved.Kept,
		Canceled: preserved.Canceled, Ignored: preserved.Ignored, Refused: preserved.Refused,
		ClassificationOpen: runner.classifyBusy,
	}
	if runner.speech != nil {
		runner.state.UserSpeaking = true
		runner.state.StreamID = runner.speech.streamID
		runner.state.OverlapActive = runner.speech.armed && !runner.speech.classificationEnd &&
			runner.hasActionableOverlapWork()
		runner.state.OverlapStartedNS = runner.speech.overlapStartedNS
		runner.state.DeadlineNS = runner.speech.deadlineNS
		runner.state.Evidence = runner.speech.evidence
		runner.state.CancelIssued = runner.speech.cancelIssued
	}
	for _, run := range runner.runs {
		if run.modelActive {
			runner.state.ActiveModels++
		}
		if run.segmentationActive {
			runner.state.ActiveSegmentations++
		}
	}
	for _, utterance := range runner.utterances {
		if utterance.ttsActive {
			runner.state.ActiveTTS++
		}
		if utterance.playbackActive {
			runner.state.ActivePlayback++
		}
	}
}

func (runner *overlapBargeInRunner) agentOutputSnapshot() coreinteraction.AgentOutput {
	active := runner.hasAgentWork()
	audible := false
	for _, utterance := range runner.utterances {
		if utterance != nil && utterance.playbackActive && utterance.playbackAudible {
			audible = true
			break
		}
	}
	inFlight := ""
	if active {
		inFlight = fmt.Sprintf(
			"voice output active: model=%d, segmentation=%d, synthesis=%d, playback=%d",
			runner.state.ActiveModels, runner.state.ActiveSegmentations,
			runner.state.ActiveTTS, runner.state.ActivePlayback,
		)
	}
	protected := make([]string, 0, len(runner.runs))
	for runID, run := range runner.runs {
		if run == nil || run.streamID == "" || !deliberateSpokeOver(run.act) ||
			(!run.modelActive && !run.segmentationActive && !runner.runHasActiveUtterance(runID)) {
			continue
		}
		protected = append(protected, run.streamID)
		if run.protectedStreamID != "" {
			protected = append(protected, run.protectedStreamID)
		}
	}
	sort.Strings(protected)
	protected = slices.Compact(protected)
	return coreinteraction.AgentOutput{
		Active: active, Queued: active && !audible, Audible: audible,
		Saying: runner.activeAgentText(), InFlight: inFlight,
		ProtectedStreams: protected,
	}
}

func (runner *overlapBargeInRunner) activeAddresses() ([]string, []string) {
	runs := make([]string, 0, len(runner.runs))
	for runID, run := range runner.runs {
		if run.modelActive || run.segmentationActive {
			runs = append(runs, runID)
		}
	}
	utterances := make([]string, 0, len(runner.utterances))
	for utteranceID, utterance := range runner.utterances {
		if utterance.ttsActive || utterance.playbackActive {
			utterances = append(utterances, utteranceID)
		}
	}
	sort.Strings(runs)
	sort.Strings(utterances)
	return runs, utterances
}

func (runner *overlapBargeInRunner) activeAgentText() string {
	type candidate struct {
		id       string
		text     string
		priority int
	}
	var selected candidate
	for utteranceID, utterance := range runner.utterances {
		if utterance == nil || utterance.text == "" {
			continue
		}
		priority := 0
		switch {
		case utterance.playbackActive:
			priority = 2
		case utterance.ttsActive:
			priority = 1
		default:
			continue
		}
		if priority > selected.priority ||
			(priority == selected.priority && (selected.id == "" || utteranceID < selected.id)) {
			selected = candidate{id: utteranceID, text: utterance.text, priority: priority}
		}
	}
	return selected.text
}

func (runner *overlapBargeInRunner) hasAgentWork() bool {
	for _, run := range runner.runs {
		if run.modelActive || run.segmentationActive {
			return true
		}
	}
	for _, utterance := range runner.utterances {
		if utterance.ttsActive || utterance.playbackActive {
			return true
		}
	}
	return false
}

// hasActionableOverlapWork excludes output which was explicitly authorized to
// interrupt or speak through. Acoustic onset cannot tell whether a following
// ASR stream is a continuation, acknowledgement, correction, or new request;
// canceling before its first transcript revision defeats the semantic policy
// that can. A later typed stop-speaking decision still addresses deliberate
// output through cancelSelectedRuns, while ordinary answers retain the bounded
// acoustic fallback.
func (runner *overlapBargeInRunner) hasActionableOverlapWork() bool {
	for _, run := range runner.runs {
		if runner.runProtectedFromCurrentSpeech(run) {
			continue
		}
		if run.modelActive || run.segmentationActive {
			return true
		}
	}
	for _, utterance := range runner.utterances {
		if utterance == nil || runner.runProtectedFromCurrentSpeech(runner.runs[utterance.runID]) {
			continue
		}
		if utterance.ttsActive || utterance.playbackActive {
			return true
		}
	}
	return false
}

func (runner *overlapBargeInRunner) runProtectedFromCurrentSpeech(run *overlapRun) bool {
	return run != nil && runner.speech != nil && run.streamID != "" && deliberateSpokeOver(run.act)
}

func (runner *overlapBargeInRunner) ensureRun(runID string) (*overlapRun, error) {
	if run := runner.runs[runID]; run != nil {
		return run, nil
	}
	if len(runner.runs) >= runner.config.MaxActiveRuns {
		return nil, errors.New("overlap active-run bound reached")
	}
	run := &overlapRun{}
	runner.runs[runID] = run
	return run, nil
}

func (runner *overlapBargeInRunner) pruneRun(runID string) {
	run := runner.runs[runID]
	if run == nil || !run.invocationSeen || !run.modelTerminal ||
		!run.segmentationTerminal || run.modelActive || run.segmentationActive {
		return
	}
	for _, utterance := range runner.utterances {
		if utterance.runID == runID && (utterance.ttsActive || utterance.playbackActive) {
			return
		}
	}
	delete(runner.runs, runID)
	runner.rememberTerminalRun(runID)
}

func (runner *overlapBargeInRunner) rememberTerminalRun(runID string) {
	if _, found := runner.terminalRuns[runID]; found {
		return
	}
	runner.terminalRuns[runID] = struct{}{}
	runner.terminalRunOrder = append(runner.terminalRunOrder, runID)
	if len(runner.terminalRunOrder) <= runner.config.MaxActiveRuns {
		return
	}
	oldest := runner.terminalRunOrder[0]
	runner.terminalRunOrder = runner.terminalRunOrder[1:]
	delete(runner.terminalRuns, oldest)
}

func (runner *overlapBargeInRunner) rememberTerminalUtterance(utteranceID string) {
	if _, found := runner.terminalUtterances[utteranceID]; found {
		return
	}
	runner.terminalUtterances[utteranceID] = struct{}{}
	runner.terminalUtteranceOrder = append(runner.terminalUtteranceOrder, utteranceID)
	if len(runner.terminalUtteranceOrder) <= runner.config.MaxUtterances {
		return
	}
	oldest := runner.terminalUtteranceOrder[0]
	runner.terminalUtteranceOrder = runner.terminalUtteranceOrder[1:]
	delete(runner.terminalUtterances, oldest)
}

func (runner *overlapBargeInRunner) disarmOverlap() {
	runner.stopTimer()
	runner.stopClassification()
	if runner.speech != nil {
		runner.speech.armed = false
		runner.speech.overlapStartedNS = 0
		runner.speech.deadlineNS = 0
		runner.speech.evidence = ""
		runner.speech.classificationEnd = false
	}
}

func (runner *overlapBargeInRunner) stopTimer() {
	if runner.timer != nil {
		runner.timer.Stop()
		runner.timer = nil
	}
}

func (runner *overlapBargeInRunner) stopClassification() {
	if runner.classifyCancel != nil {
		runner.classifyCancel()
		runner.classifyCancel = nil
	}
	runner.pendingClassification = nil
	runner.classifyGeneration++
}

func (runner *overlapBargeInRunner) policyName() string {
	if runner.classifier == nil {
		return fmt.Sprintf("sustained-%dms/%s", runner.config.HoldMS, runner.config.Unclassified)
	}
	return runner.classifier.Name() + fmt.Sprintf("/sustained-%dms/%s", runner.config.HoldMS, runner.config.Unclassified)
}

func receiveOverlapInput(
	ctx context.Context, kind overlapInputKind, input element.InputPort,
	output chan<- overlapInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminalInteractionReceive(ctx, err) {
			return
		}
		if err != nil {
			sendInteractionFailure(ctx, failures, err)
			return
		}
		select {
		case output <- overlapInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func overlapSpeechActivityPayload(payload any) (acousticelements.SpeechActivity, bool) {
	switch value := payload.(type) {
	case acousticelements.SpeechActivity:
		return value, true
	case *acousticelements.SpeechActivity:
		if value != nil {
			return *value, true
		}
	}
	return acousticelements.SpeechActivity{}, false
}

func overlapObservationPayload(payload any) (coreperception.Observation, bool) {
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

func overlapTextSegmentPayload(payload any) (speechelements.TextSegment, bool) {
	switch value := payload.(type) {
	case speechelements.TextSegment:
		value.AssistantItemIDs = slices.Clone(value.AssistantItemIDs)
		return value, true
	case *speechelements.TextSegment:
		if value != nil {
			copy := *value
			copy.AssistantItemIDs = slices.Clone(value.AssistantItemIDs)
			return copy, true
		}
	}
	return speechelements.TextSegment{}, false
}

func overlapInvocationPayload(payload any) (policyelements.SessionInvocationOutcome, bool) {
	switch value := payload.(type) {
	case policyelements.SessionInvocationOutcome:
		return value, true
	case *policyelements.SessionInvocationOutcome:
		if value != nil {
			return *value, true
		}
	}
	return policyelements.SessionInvocationOutcome{}, false
}

func overlapSemanticDecisionPayload(payload any) (policyelements.SemanticDecision, bool) {
	switch value := payload.(type) {
	case policyelements.SemanticDecision:
		return value, true
	case *policyelements.SemanticDecision:
		if value != nil {
			return *value, true
		}
	}
	return policyelements.SemanticDecision{}, false
}

func overlapModelOutcomePayload(payload any) (cognitionelements.Outcome, bool) {
	switch value := payload.(type) {
	case cognitionelements.Outcome:
		return value, true
	case *cognitionelements.Outcome:
		if value != nil {
			return *value, true
		}
	}
	return cognitionelements.Outcome{}, false
}

func overlapSegmentationPayload(payload any) (SegmentationOutcome, bool) {
	switch value := payload.(type) {
	case SegmentationOutcome:
		return value, true
	case *SegmentationOutcome:
		if value != nil {
			return *value, true
		}
	}
	return SegmentationOutcome{}, false
}

func overlapTransitionPayload(payload any) (speechelements.Transition, bool) {
	switch value := payload.(type) {
	case speechelements.Transition:
		return value, true
	case *speechelements.Transition:
		if value != nil {
			return *value, true
		}
	}
	return speechelements.Transition{}, false
}

func overlapPlaybackReceiptPayload(payload any) (speechelements.PlaybackReceipt, bool) {
	var receipt speechelements.PlaybackReceipt
	switch value := payload.(type) {
	case speechelements.PlaybackReceipt:
		receipt = value
	case *speechelements.PlaybackReceipt:
		if value == nil {
			return speechelements.PlaybackReceipt{}, false
		}
		receipt = *value
	default:
		return speechelements.PlaybackReceipt{}, false
	}
	receipt.Utterance.AssistantItemIDs = slices.Clone(receipt.Utterance.AssistantItemIDs)
	receipt.Frame.PCM16LE = slices.Clone(receipt.Frame.PCM16LE)
	return receipt, true
}

func (runner *overlapBargeInRunner) validateInputEnvelope(envelope element.Envelope) error {
	if !boundedOverlapIdentity(envelope.ItemID) {
		return errors.New("overlap barge-in input requires a bounded item ID")
	}
	sessionID := strings.TrimSpace(envelope.SessionID)
	if !boundedOverlapIdentity(sessionID) {
		return errors.New("overlap barge-in input requires a bounded session ID")
	}
	if runner.sessionID == "" {
		runner.sessionID = sessionID
	} else if runner.sessionID != sessionID {
		return fmt.Errorf("overlap barge-in input changed session from %q to %q",
			runner.sessionID, sessionID)
	}
	return nil
}

func validateOverlapObservation(observation coreperception.Observation) error {
	if err := observation.Validate(); err != nil {
		return err
	}
	if observation.Authority != trajectory.AuthorityUser ||
		strings.TrimSpace(observation.Source) == "" {
		return errors.New("overlap transcript must be user-authority speech with an explicit source")
	}
	if observation.Described || len(observation.Media) != 0 {
		return errors.New("overlap transcript cannot be described or media-bearing content")
	}
	if observation.Revision == 0 {
		return errors.New("overlap transcript requires a positive source revision")
	}
	if len(observation.Text) > maximumOverlapTranscript || !utf8.ValidString(observation.Text) ||
		strings.TrimSpace(observation.Text) != observation.Text || !carriesOverlapSpeech(observation.Text) {
		return errors.New("overlap transcript must contain bounded canonical speech text")
	}
	if len(observation.StableText) > len(observation.Text) ||
		!strings.HasPrefix(observation.Text, observation.StableText) {
		return errors.New("overlap transcript stable text is not a prefix of its text")
	}
	if observation.Final == observation.Provisional {
		return errors.New("overlap transcript must be exactly one of provisional or final")
	}
	return nil
}

func carriesOverlapSpeech(text string) bool {
	for _, value := range text {
		if unicode.IsLetter(value) || unicode.IsNumber(value) {
			return true
		}
	}
	return false
}

func punctuationOnlyOverlapText(text string) bool {
	for _, value := range text {
		if !unicode.IsPunct(value) {
			return false
		}
	}
	return text != ""
}

func exactOverlapIdentity(values ...string) (string, error) {
	var identity string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if identity != "" && identity != value {
			return "", fmt.Errorf("conflicting identities %q and %q", identity, value)
		}
		identity = value
	}
	return identity, nil
}

func closeOverlapDecider(decider policyelements.SemanticDecider) error {
	if closer, ok := decider.(io.Closer); ok && closer != nil {
		return closer.Close()
	}
	return nil
}

func boundedOverlapIdentity(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 1024 && !strings.ContainsAny(value, "\r\n\x00")
}

func firstNonzeroOverlap(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

var _ element.Factory = overlapBargeInFactory{}
var _ element.ConfigValidator = overlapBargeInFactory{}
var _ element.Runnable = (*overlapBargeInRunner)(nil)
