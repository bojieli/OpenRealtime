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

	audioMu     sync.Mutex
	acoustic    *perception.EnergyGate
	utteranceID string

	stateMu    sync.Mutex
	utterance  *action.Utterance
	spokenText string
	answer     string
	pending    map[string]*pendingInvocation
	callOwner  map[string]string
	callNames  map[string]string
}

type pendingInvocation struct {
	calls      []trajectory.ToolCall
	results    map[string]trajectory.ToolResult
	dispatched bool
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
		pending:   make(map[string]*pendingInvocation),
		callOwner: make(map[string]string), callNames: make(map[string]string),
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
	gate, err := perception.NewEnergyGate(bind.config.Gate, uint32(bind.config.InputRate))
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.acoustic = gate

	model, err := sidecar.Dial(ctx, bind.config.Sidecar, sidecar.Message{
		SampleRate:   bind.config.InputRate,
		Instructions: cognition.Compose(bind.config.Instructions, result.settings.Instruction),
		Voice:        firstNonEmpty(result.settings.Voice, bind.config.Voice),
		Tools:        sidecarTools(result.registry),
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.model, result.ready = model, model.Ready()

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
	return binding.Status{
		Binding: runtime.spec.Name, Ownership: runtime.binding.Ownership(),
		Policies: runtime.policies.Report(), Observers: []string{"sidecar:" + runtime.ready.Model},
		Fast: "sidecar/" + runtime.ready.Model, Slow: slow.Provider + "/" + slow.Model,
	}
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
	runtime.settingsMu.Lock()
	runtime.settings = settings
	runtime.settingsMu.Unlock()
	return nil
}

// Audio forwards input to the model and, when the engine owns the floor, runs
// the acoustic gate that decides when the turn ended.
func (runtime *runtime) Audio(ctx context.Context, frame perception.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if err := runtime.model.Audio(frame.PCM16LE); err != nil {
		return err
	}
	if runtime.spec.Floor == binding.OwnerModel {
		// The model reports its own voice activity; running a second detector
		// over the same audio would give the session two answers to one
		// question.
		return nil
	}
	runtime.audioMu.Lock()
	result, err := runtime.acoustic.Push(frame.PCM16LE)
	if err != nil {
		runtime.audioMu.Unlock()
		return err
	}
	if result.Started {
		runtime.utteranceID = fmt.Sprintf("%s_item_%d", runtime.spec.Name, runtime.sequence.Add(1))
	}
	utteranceID := runtime.utteranceID
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
	if result.Stopped {
		runtime.duplex.UserSpeechStopped(now)
		if err := runtime.sink.Activity(ctx, binding.ActivityEvent{
			Stopped: true, ItemID: utteranceID, AudioEndMS: result.AudioEndMS,
		}); err != nil {
			return err
		}
		// The engine holds the floor for exactly this decision, so the turn
		// ends when the engine says so rather than when the model guesses.
		return runtime.model.Send(sidecar.Message{Type: sidecar.TypeRespond})
	}
	return nil
}

func (runtime *runtime) onBargeIn() error {
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

// CreateResponse asks the model for a turn now.
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
