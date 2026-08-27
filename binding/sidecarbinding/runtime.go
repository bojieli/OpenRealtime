package sidecarbinding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/clientcalls"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// runtime is one sidecar-backed session.
type runtime struct {
	spec      Spec
	config    Config
	binding   *Binding
	sink      binding.Sink
	policies  interaction.Policies
	scheduler clock.Scheduler

	model       *sidecar.Client
	ready       sidecar.Message
	store       *trajectory.Store
	duplex      *session.Duplex
	coordinator *eventloop.Coordinator
	engine      *cognition.Engine
	ledger      *action.Ledger
	registry    *action.Registry
	tools       *action.Tools

	ctx    context.Context
	cancel context.CancelCauseFunc
	wait   sync.WaitGroup

	sequence atomic.Uint64
	revision atomic.Uint64

	settingsMu sync.RWMutex
	settings   binding.Settings

	inputMu                  sync.Mutex
	audioMu                  sync.Mutex
	acoustic                 *perception.EnergyGate
	acousticRate             uint32
	utteranceID              string
	interactionAudio         *perception.AudioObserver
	interactionPending       []perception.Frame
	interactionLastObserveNS uint64
	interactionHeard         interaction.Revision

	stateMu     sync.Mutex
	utterance   *action.Utterance
	spokenText  string
	answer      string
	clientCalls *clientcalls.Tracker
	// The policy state is independent from the model's generation state. One
	// decision runs at a time; the window and pinboard are stateful and are
	// therefore read under the same lock as the decision they feed.
	interactionMu       sync.Mutex
	interactionWindow   *interaction.Window
	interactionPinboard *interaction.Pinboard
	interactionInFlight atomic.Bool
	lastPlanAct         interaction.Act
	lastPlanHeard       string
	lastPlanUtterance   string
	policyActions       map[uint64]interaction.Act
	committedUtterance  string
	silentToolWork      atomic.Bool
}

func newRuntime(parent context.Context, bind *Binding, options binding.Options) (*runtime, error) {
	if options.Sink == nil {
		return nil, errors.New("a sidecar session requires a sink")
	}
	policies := bind.config.Policies
	if options.Policies != nil {
		policies = *options.Policies
	}
	if err := policies.Validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(parent)
	result := &runtime{
		spec: bind.spec, config: bind.config, binding: bind, sink: options.Sink,
		policies: policies, scheduler: bind.config.Scheduler, store: trajectory.NewStore(),
		duplex: session.NewDuplex(session.DuplexConfig{Scheduler: bind.config.Scheduler}),
		ledger: action.NewLedger(), registry: action.NewRegistry(),
		ctx: ctx, cancel: cancel, settings: binding.CloneSettings(options.Settings),
		interactionWindow: &interaction.Window{}, interactionPinboard: &interaction.Pinboard{},
		policyActions: make(map[uint64]interaction.Act),
	}
	if result.settings.Gate.SilenceDurationMS == 0 {
		// The session said nothing about endpointing, so the deployment's
		// configuration stands. A session that did say something is answered
		// with what it asked for: the engine holds this floor, so the client's
		// endpointing parameters are ones it can actually honour.
		result.settings.Gate = bind.config.Gate
	}
	tracker, err := clientcalls.New(clientcalls.Config{
		Timeout: bind.config.ClientToolTimeout, Scheduler: bind.config.Scheduler,
		Commit: result.commitToolResults, Expired: result.reportUnanswered,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.clientCalls = tracker
	if bind.config.InteractionPerception != nil {
		observer, observerErr := perception.NewAudioObserver(perception.AudioConfig{
			Provider: bind.config.InteractionPerception, Name: "interaction-asr",
			Cadence: bind.config.InteractionCadence, Source: "microphone",
		})
		if observerErr != nil {
			cancel(observerErr)
			return nil, observerErr
		}
		result.interactionAudio = observer
	}
	prefix := strings.TrimSpace(options.SessionID)
	if prefix == "" {
		prefix = bind.spec.Name
	}
	nextID := func(kind string) string {
		return fmt.Sprintf("%s_%s_%d", prefix, kind, result.sequence.Add(1))
	}
	now := func() uint64 { return bind.config.Scheduler.NowNS() }

	if err := result.registry.Replace(result.settings.Tools); err != nil {
		cancel(err)
		return nil, err
	}

	model, err := sidecar.Dial(ctx, bind.config.Sidecar, sidecar.Message{
		SampleRate:       bind.config.InputRate,
		Instructions:     cognition.Compose(bind.config.Instructions, result.settings.Instruction),
		Voice:            firstNonEmpty(result.settings.Voice, bind.config.Voice),
		Tools:            sidecarTools(result.registry),
		InteractionOwner: string(bind.spec.Ownership.Interaction),
		FloorOwner:       string(bind.spec.Ownership.Floor),
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.model, result.ready = model, model.Ready()
	if bind.spec.Ownership.Floor == binding.OwnerModel &&
		!result.ready.Has(sidecar.CapabilityNativeVAD) {
		failure := errors.New("model floor ownership requires a sidecar that declares native_vad")
		cancel(failure)
		_ = model.Close()
		return nil, failure
	}
	if bind.spec.Ownership.Interaction == binding.OwnerModel &&
		!result.ready.Has(sidecar.CapabilityNativeInteraction) {
		failure := errors.New("model interaction ownership requires a sidecar that declares native_interaction")
		cancel(failure)
		_ = model.Close()
		return nil, failure
	}
	if bind.config.Sidecar.ProtocolVersion >= sidecar.VersionInteraction &&
		bind.spec.Ownership.Interaction == binding.OwnerEngine &&
		!result.ready.Has(sidecar.CapabilityInteractionActs) {
		failure := errors.New("engine interaction over protocol v2 requires a sidecar that declares interaction_acts")
		cancel(failure)
		_ = model.Close()
		return nil, failure
	}
	if bind.spec.Ownership.Interaction == binding.OwnerEngine &&
		result.ready.Has(sidecar.CapabilityNativeInteraction) &&
		(bind.config.Sidecar.ProtocolVersion < sidecar.VersionInteraction ||
			!result.ready.Has(sidecar.CapabilityInteractionActs)) {
		failure := errors.New("externally selecting interaction on a native-interaction sidecar requires protocol v2 and interaction_acts")
		cancel(failure)
		_ = model.Close()
		return nil, failure
	}

	engine, err := cognition.New(cognition.Config{
		Store: result.store, Fast: modelStandIn{name: bind.spec.Name}, Slow: bind.config.Slow,
		Catalog:          toolCatalog{runtime: result},
		AgentInstruction: cognition.Compose(bind.config.Instructions, result.settings.Instruction),
		SlowMaxTokens:    bind.config.SlowMaxTokens, RequireSilentSlow: true, RetainReasoning: true,
		ExternalFast: true, Now: now, NextID: nextID,
	})
	if err != nil {
		cancel(err)
		_ = model.Close()
		return nil, err
	}
	result.engine = engine

	tools, err := action.NewTools(action.ToolsConfig{
		Registry: result.registry, Ledger: result.ledger, Store: result.store,
	})
	if err != nil {
		cancel(err)
		_ = model.Close()
		return nil, err
	}
	result.tools = tools

	coordinator, err := eventloop.New(eventloop.Config{
		Store: result.store, Processor: result, MaxPendingEvents: bind.config.MaxPendingEvents,
		ReservedInterruptEvents: 8, Now: now, NextID: nextID,
	})
	if err != nil {
		cancel(err)
		_ = model.Close()
		return nil, err
	}
	result.coordinator = coordinator
	deferralGate, err := interaction.Bind(policies.Deferral, result.duplex, coordinator)
	if err != nil {
		cancel(err)
		_ = model.Close()
		return nil, err
	}
	coordinator.SetGate(deferralGate)

	result.wait.Add(2)
	go func() {
		defer result.wait.Done()
		result.mirror()
	}()
	go func() {
		defer result.wait.Done()
		result.drive()
	}()
	return result, nil
}

// Status reports what this session is running.
func (runtime *runtime) Status() binding.Status {
	_, slow := runtime.engine.Descriptors()
	fastAuthority := "none"
	if runtime.ready.Has(sidecar.CapabilityTools) {
		fastAuthority = "propose"
	}
	return binding.Status{
		Binding: runtime.spec.Name, Profile: "voice",
		Ownership: runtime.binding.Ownership(), Stack: runtime.stackCapabilities(),
		Policies: runtime.policies.Report(), Interaction: runtime.interactionStatus(),
		Tools: binding.ToolStatus{
			Fast: fastAuthority, Slow: string(slow.EffectiveToolAuthority()),
			Authorization: "engine", Execution: "engine-or-client",
		},
		Observers: []string{"sidecar:" + runtime.ready.Model},
		Fast:      "sidecar/" + runtime.ready.Model, Slow: slow.Provider + "/" + slow.Model,
	}
}

func (runtime *runtime) interactionStatus() binding.InteractionStatus {
	status := binding.InteractionStatus{Transport: "sidecar"}
	status.ProtocolVersion = runtime.config.Sidecar.ProtocolVersion
	if status.ProtocolVersion == 0 {
		status.ProtocolVersion = sidecar.Version
	}
	switch {
	case runtime.spec.Ownership.Interaction == binding.OwnerModel:
		status.Evidence = "native-multimodal"
		status.EvidenceCapabilities = binding.InteractionEvidenceCapabilities{
			NativeModelState: true,
		}
		status.ActHandoff = "none"
		status.Control = binding.InteractionControl{
			Selectors: binding.InteractionControllers{Native: true}, Arbitration: "single",
		}
	case runtime.policies.Interaction != nil:
		status.Evidence = "transcript"
		status.EvidenceCapabilities = binding.InteractionEvidenceCapabilities{
			Transcript: true, AcousticActivity: true, SilenceClock: true,
			ConversationState: true, ToolState: true,
		}
		status.DecisionTimeoutMS = int(runtime.config.InteractionTimeout.Milliseconds())
		descriptor := runtime.config.InteractionPerceptionDescriptor
		status.Recognizer, status.RecognizerRevision = descriptor.Name, descriptor.Version
		status.Control = binding.InteractionControl{
			Selectors: binding.InteractionControllers{TextPolicy: true}, Arbitration: "single",
		}
	default:
		status.Evidence = "acoustic-predicates"
		status.EvidenceCapabilities = binding.InteractionEvidenceCapabilities{
			AcousticActivity: true, SilenceClock: true,
		}
		status.Control = binding.InteractionControl{
			Selectors: binding.InteractionControllers{Predicates: true}, Arbitration: "single",
		}
	}
	if runtime.spec.Ownership.Interaction == binding.OwnerEngine {
		if status.ProtocolVersion >= sidecar.VersionInteraction &&
			runtime.ready.Has(sidecar.CapabilityInteractionActs) {
			status.ActHandoff = "typed"
		} else {
			status.ActHandoff = "translated"
		}
		if runtime.ready.Has(sidecar.CapabilityNativeInteraction) {
			status.NativeSuppression = "hello selects engine interaction; only typed interaction acts may initiate policy-controlled generation"
		}
	}
	return status
}

// stackCapabilities combines deployment knowledge with what the connected
// sidecar actually declared. The former carries structural capabilities such
// as turn generation; the latter carries optional protocol surfaces that can
// only be known after the handshake.
func (runtime *runtime) stackCapabilities() binding.StackCapabilities {
	declared := binding.StackCapabilities{
		Transcription:     runtime.ready.Has(sidecar.CapabilityTranscript),
		ConcurrentIO:      runtime.ready.Has(sidecar.CapabilityFullDuplex),
		NativeFloor:       runtime.ready.Has(sidecar.CapabilityNativeVAD),
		NativeInteraction: runtime.ready.Has(sidecar.CapabilityNativeInteraction),
		InteractionActs:   runtime.ready.Has(sidecar.CapabilityInteractionActs),
		TextInjection:     runtime.ready.Has(sidecar.CapabilityTextInjection),
	}
	return runtime.spec.Capabilities.Merge(runtime.config.ModelCapabilities).Merge(declared)
}

func (runtime *runtime) Trajectory() trajectory.Snapshot { return runtime.store.Snapshot() }

func (runtime *runtime) Settings() binding.Settings {
	runtime.settingsMu.RLock()
	defer runtime.settingsMu.RUnlock()
	return binding.CloneSettings(runtime.settings)
}

// Update applies a configuration change.
//
// The model's own instructions are fixed at the handshake: a sidecar that
// reconfigured mid-session would have to re-load, and the protocol does not
// pretend otherwise. Tool declarations do change, because the engine owns tool
// execution and can honour them immediately.
func (runtime *runtime) Update(_ context.Context, settings binding.Settings) error {
	settings = binding.CloneSettings(settings)
	if err := runtime.registry.Replace(settings.Tools); err != nil {
		return err
	}
	if settings.Gate.SilenceDurationMS == 0 {
		settings.Gate = runtime.config.Gate
	}
	runtime.settingsMu.Lock()
	changed := runtime.settings.Gate != settings.Gate
	runtime.settings = settings
	runtime.settingsMu.Unlock()
	if changed {
		// The gate is built once and reused, so new endpointing parameters
		// would otherwise be accepted, reported back, and never reach the
		// thing that decides when a turn ended. Dropping it here rebuilds it
		// from the new settings on the next frame.
		//
		// Deliberately not under settingsMu: Audio takes audioMu and then
		// reads the settings, so taking them in the other order here is how
		// this deadlocks.
		runtime.audioMu.Lock()
		runtime.acoustic, runtime.acousticRate = nil, 0
		runtime.audioMu.Unlock()
	}
	return nil
}

// Audio forwards input to the model and, when the engine owns the floor, runs
// the acoustic gate that decides when the turn ended.
func (runtime *runtime) Audio(ctx context.Context, frame perception.Frame) error {
	runtime.inputMu.Lock()
	defer runtime.inputMu.Unlock()
	if err := frame.Validate(); err != nil {
		return err
	}
	if frame.Kind != perception.FrameAudio {
		return errors.New("audio path requires an audio frame")
	}
	if err := runtime.model.Audio(frame.PCM16LE); err != nil {
		return err
	}
	if runtime.spec.Ownership.Floor == binding.OwnerModel {
		// The model reports its own boundaries. A policy-only recogniser may
		// still read the frames, but its acoustic gate must not become a second
		// floor just because the controller needs text evidence.
		if runtime.interactionAudio != nil {
			return runtime.observeModelFloorInteractionAudio(ctx, frame)
		}
		return nil
	}
	runtime.audioMu.Lock()
	// The gate is built from the rate that actually arrives: its thresholds
	// are in samples, so one built for the wrong rate waits silently for the
	// wrong amount of time rather than failing.
	if runtime.acoustic == nil || runtime.acousticRate != frame.SampleRateHz {
		gate, gateErr := perception.NewEnergyGate(runtime.Settings().Gate, frame.SampleRateHz)
		if gateErr != nil {
			runtime.audioMu.Unlock()
			return gateErr
		}
		runtime.acoustic, runtime.acousticRate = gate, frame.SampleRateHz
	}
	result, err := runtime.acoustic.Push(frame.PCM16LE)
	if err != nil {
		runtime.audioMu.Unlock()
		return err
	}
	if result.Started {
		runtime.utteranceID = fmt.Sprintf("%s_item_%d", runtime.spec.Name, runtime.sequence.Add(1))
	}
	utteranceID := runtime.utteranceID
	var interactionBatch []perception.Frame
	if runtime.interactionAudio != nil && len(result.Audio) > 0 {
		runtime.interactionPending = append(runtime.interactionPending, perception.Frame{
			Kind: perception.FrameAudio, Source: "microphone", CapturedNS: frame.CapturedNS,
			SampleRateHz: frame.SampleRateHz, PCM16LE: result.Audio,
		})
		cadence := uint64(runtime.interactionAudio.Cadence().Nanoseconds())
		now := runtime.scheduler.NowNS()
		if result.Stopped || runtime.interactionLastObserveNS == 0 ||
			now-runtime.interactionLastObserveNS >= cadence {
			interactionBatch, runtime.interactionPending = runtime.interactionPending, nil
			runtime.interactionLastObserveNS = now
		}
	}
	runtime.audioMu.Unlock()

	now := runtime.scheduler.NowNS()
	if result.Started {
		runtime.duplex.UserSpeechStarted(now)
		if err := runtime.onBargeIn(); err != nil {
			return err
		}
		if err := runtime.sink.Activity(ctx, binding.ActivityEvent{
			Started: true, ItemID: utteranceID, AudioStartMS: result.AudioStartMS,
		}); err != nil {
			return err
		}
	}
	if len(interactionBatch) > 0 {
		if err := runtime.observeInteractionAudio(ctx, interactionBatch, result.SilenceNS); err != nil {
			runtime.sink.Failed(ctx, binding.ErrorEvent{Code: "interaction_asr_error", Message: err.Error()})
		}
	}
	if result.Stopped {
		runtime.duplex.UserSpeechStopped(now)
		if err := runtime.sink.Activity(ctx, binding.ActivityEvent{
			Stopped: true, ItemID: utteranceID, AudioEndMS: result.AudioEndMS,
		}); err != nil {
			return err
		}
		// The engine holds the floor for exactly this decision, so the turn
		// ends when the engine says so rather than when the model guesses -
		// unless the client took the floor, in which case the endpoint is
		// still observed and reported but is not acted on. A client that
		// declared its own turns and got a server-created one alongside them
		// hears the agent answer twice.
		if runtime.Settings().ManualTurns {
			_ = runtime.finishInteractionTurn(ctx, interaction.ActStaySilent, false)
			return nil
		}
		if runtime.spec.Ownership.Interaction == binding.OwnerModel {
			// The engine selected the endpoint, but the model still owns the
			// conversational act. Commit the boundary without converting it into
			// an answer request; a native interaction policy may listen instead.
			return runtime.model.Send(sidecar.Message{Type: sidecar.TypeCommit})
		}
		if runtime.policies.Interaction != nil {
			return runtime.finishInteractionTurn(ctx, "", true)
		}
		if runtime.config.Sidecar.ProtocolVersion >= sidecar.VersionInteraction {
			return runtime.finishInteractionTurn(ctx, interaction.ActAnswer, true)
		}
		if runtime.interactionAudio != nil {
			_ = runtime.finishInteractionTurn(ctx, interaction.ActAnswer, false)
		}
		return runtime.model.Send(sidecar.Message{Type: sidecar.TypeRespond})
	}
	return nil
}

func (runtime *runtime) onBargeIn() error {
	if runtime.policies.Interaction != nil && runtime.spec.Ownership.Interaction == binding.OwnerEngine {
		// Acoustic onset says that something made sound, not whether it was a
		// correction, a continuer, or a cough. The policy-only recogniser supplies
		// the words shortly; interrupting before it can decide would make the
		// external controller decorative.
		return nil
	}
	decision := runtime.policies.BargeIn.Decide(interaction.BargeInInput{
		Context: interaction.Context{
			NowNS: runtime.scheduler.NowNS(), Duplex: runtime.duplex.Snapshot(),
		},
	})
	if !decision.Cancel {
		return nil
	}
	runtime.coordinator.Interrupt(fmt.Errorf("%s: %w", decision.Reason, eventloop.ErrInterrupted))
	runtime.duplex.AgentAudioStopped(runtime.scheduler.NowNS())
	return runtime.model.Send(sidecar.Message{Type: sidecar.TypeInterrupt})
}

// Video is not supported: the sidecar protocol carries audio, and a binding
// should say what it cannot do rather than discard frames quietly.
func (runtime *runtime) Video(context.Context, perception.Frame) error {
	return fmt.Errorf("%w: video input over the %s binding", binding.ErrUnsupported, runtime.spec.Name)
}

// Text injects something the client typed into the model's context.
func (runtime *runtime) Text(_ context.Context, input binding.TextInput) error {
	role := input.Role
	if role == "" {
		role = "user"
	}
	return runtime.model.Send(sidecar.Message{
		Type: sidecar.TypeText, Role: role, Text: input.Text,
	})
}

// CreateResponse asks the model for a turn now.
// CommitAudio ends the turn where the client says it ended.
//
// A model that owns its own floor is told the turn is over and decides for
// itself what to do about it; one whose floor the engine keeps has its
// acoustic gate closed here, which is the same endpoint silence would have
// produced, arriving when the client said so instead.
func (runtime *runtime) CommitAudio(ctx context.Context) error {
	if runtime.spec.Ownership.Floor == binding.OwnerModel {
		return runtime.model.Send(sidecar.Message{Type: sidecar.TypeCommit})
	}
	runtime.audioMu.Lock()
	utteranceID := runtime.utteranceID
	stopped := false
	if runtime.acoustic != nil {
		_, stopped = runtime.acoustic.ForceStop()
	}
	runtime.audioMu.Unlock()
	if !stopped || utteranceID == "" {
		return errors.New("the input audio buffer is empty")
	}
	if err := runtime.sink.Activity(ctx, binding.ActivityEvent{
		Committed: true, ItemID: utteranceID,
	}); err != nil {
		return err
	}
	return runtime.model.Send(sidecar.Message{Type: sidecar.TypeCommit})
}

func (runtime *runtime) CreateResponse(context.Context) error {
	return runtime.model.Send(sidecar.Message{Type: sidecar.TypeRespond})
}

// Cancel stops generation on both sides.
func (runtime *runtime) Cancel(_ context.Context, reason string) error {
	runtime.coordinator.Interrupt(fmt.Errorf("%s: %w", reason, eventloop.ErrInterrupted))
	runtime.duplex.AgentAudioStopped(runtime.scheduler.NowNS())
	return runtime.model.Send(sidecar.Message{Type: sidecar.TypeInterrupt})
}

// Truncate reports client playback truncation. The model owns action, so it is
// told to stop; what was heard is what the client says it was.
func (runtime *runtime) Truncate(_ context.Context, truncation binding.Truncation) error {
	if truncation.AudioEndMS >= 0 {
		runtime.duplex.AgentAudioStopped(runtime.scheduler.NowNS())
	}
	return runtime.model.Send(sidecar.Message{Type: sidecar.TypeInterrupt})
}

// Close ends the session and the sidecar with it.
func (runtime *runtime) Close(_ context.Context, cause error) error {
	if cause == nil {
		cause = errors.New("session closed")
	}
	runtime.cancel(cause)
	err := runtime.model.Close()
	runtime.duplex.Close()
	runtime.clientCalls.Close()
	if runtime.interactionAudio != nil {
		runtime.interactionAudio.Reset()
	}
	runtime.wait.Wait()
	return err
}

func (runtime *runtime) nextRevision() uint64 { return runtime.revision.Add(1) }

func (runtime *runtime) drive() {
	for {
		select {
		case <-runtime.ctx.Done():
			return
		case <-runtime.coordinator.Signal():
			for runtime.coordinator.Runnable() {
				_, err := runtime.coordinator.RunNext(runtime.ctx)
				if err == nil {
					continue
				}
				if errors.Is(err, eventloop.ErrIdle) || errors.Is(err, eventloop.ErrDeferred) ||
					errors.Is(err, eventloop.ErrBusy) {
					break
				}
				if errors.Is(err, eventloop.ErrInterrupted) || errors.Is(err, context.Canceled) {
					continue
				}
				runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{Code: "provider_error", Message: err.Error()})
				break
			}
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sidecarTools(registry *action.Registry) []sidecar.Tool {
	specs := registry.Specs()
	tools := make([]sidecar.Tool, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, sidecar.Tool{
			Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters,
		})
	}
	return tools
}

// toolCatalog is the surface the engine's slow provider may execute.
type toolCatalog struct{ runtime *runtime }

func (catalog toolCatalog) Tools() []continuation.ToolDefinition {
	specs := catalog.runtime.registry.Specs()
	tools := make([]continuation.ToolDefinition, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, continuation.ToolDefinition{
			Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters,
		})
	}
	return tools
}

func (catalog toolCatalog) Capabilities() []continuation.Capability {
	specs := catalog.runtime.registry.Specs()
	capabilities := make([]continuation.Capability, 0, len(specs))
	for _, spec := range specs {
		capabilities = append(capabilities, continuation.Capability{
			Name: spec.Name, Description: spec.Description, Available: true,
			ExecutionPhase:       string(trajectory.PhaseSlow),
			ConfirmationRequired: spec.Confirm != action.ConfirmNever,
		})
	}
	return capabilities
}

// modelStandIn occupies the fast slot. The model behind the sidecar is the
// fast provider and does not speak this interface, so the stand-in refuses to
// run: an accidental fast continuation would be a second voice.
type modelStandIn struct{ name string }

func (stand modelStandIn) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "sidecar", Model: stand.name, Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityNone,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}
}

func (stand modelStandIn) Continue(context.Context, continuation.Request, continuation.Emit) (continuation.Completion, error) {
	return continuation.Completion{}, errors.New("the sidecar model owns the fast voice; the engine must not run one")
}
