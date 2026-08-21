package cascade

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	"github.com/bojieli/OpenRealtime/trajectory"
)

// runtime is one live cascade session. It owns the four planes and the wiring
// between them, and it is the only place in the binding where they meet.
type runtime struct {
	config    Config
	binding   *Binding
	sink      binding.Sink
	policies  interaction.Policies
	scheduler clock.Scheduler

	store       *trajectory.Store
	duplex      *session.Duplex
	media       *session.MediaStore
	coordinator *eventloop.Coordinator
	gate        *interaction.Gate
	engine      *cognition.Engine
	ledger      *action.Ledger
	speech      *action.Speech
	tools       *action.Tools
	registry    *action.Registry
	observers   *perception.Set
	audio       *perception.AudioObserver

	ctx    context.Context
	cancel context.CancelCauseFunc
	wait   sync.WaitGroup

	sequence atomic.Uint64
	revision atomic.Uint64
	// continuing is true while a backchannel decision is in flight, so one
	// turn does not accumulate a model call per revision.
	continuing inFlight

	settingsMu sync.RWMutex
	settings   binding.Settings

	audioMu       sync.Mutex
	acoustic      *perception.EnergyGate
	acousticRate  uint32
	pending       []perception.Frame
	lastObserveNS uint64
	utteranceID   string
	lastStable    string
	lastCanonical uint64
	speechStartNS uint64

	clientCalls *clientcalls.Tracker

	// prepared holds speculative continuations that have been generated and
	// committed to nothing.
	prepared *preparations

	observerMu sync.RWMutex
	// selected is this session's observer set by name. Nil selects them all,
	// which is the binding's documented default.
	selected map[string]struct{}
}

func newRuntime(parent context.Context, bind *Binding, options binding.Options) (*runtime, error) {
	if options.Sink == nil {
		return nil, errors.New("a cascade session requires a sink")
	}
	policies := bind.config.Policies
	if options.Policies != nil {
		policies = *options.Policies
	}
	if err := policies.Validate(); err != nil {
		return nil, err
	}
	scheduler := bind.config.Scheduler
	ctx, cancel := context.WithCancelCause(parent)
	result := &runtime{
		config: bind.config, binding: bind, sink: options.Sink, policies: policies,
		scheduler: scheduler, store: trajectory.NewStore(),
		duplex:   session.NewDuplex(session.DuplexConfig{Scheduler: scheduler}),
		ledger:   action.NewLedger(),
		registry: action.NewRegistry(),
		ctx:      ctx, cancel: cancel,
		settings: binding.CloneSettings(options.Settings),
		prepared: newPreparations(),
	}
	tracker, err := clientcalls.New(clientcalls.Config{
		Timeout: bind.config.ClientToolTimeout, Scheduler: scheduler,
		Commit: result.commitToolResults, Expired: result.reportUnanswered,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.clientCalls = tracker
	if result.settings.Gate.SilenceDurationMS == 0 {
		result.settings.Gate = perception.DefaultGateConfig()
	}
	prefix := strings.TrimSpace(options.SessionID)
	if prefix == "" {
		prefix = "sess"
	}
	nextID := func(kind string) string {
		return fmt.Sprintf("%s_%s_%d", prefix, kind, result.sequence.Add(1))
	}
	now := func() uint64 { return scheduler.NowNS() }

	media, err := session.NewMediaStore(bind.config.MediaRetention)
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.media = media

	audio, err := perception.NewAudioObserver(perception.AudioConfig{
		Provider: bind.config.Perception, Cadence: bind.config.ASRCadence,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.audio = audio
	observers := []perception.Observer{audio}
	for _, factory := range bind.config.Observers {
		observer, err := factory.New(media)
		if err != nil {
			cancel(err)
			return nil, fmt.Errorf("start observer %q: %w", factory.Name, err)
		}
		observers = append(observers, observer)
	}
	set, err := perception.NewSet(observers...)
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.observers = set
	selection := result.settings.Observers
	if len(selection) == 0 {
		selection = bind.config.DefaultObservers
	}
	if err := result.selectObservers(selection); err != nil {
		cancel(err)
		return nil, err
	}

	engine, err := cognition.New(cognition.Config{
		Store: result.store, Fast: bind.config.Fast, Slow: bind.config.Slow,
		Catalog:          toolCatalog{runtime: result},
		AgentInstruction: cognition.Compose(bind.config.AgentInstruction, result.settings.Instruction),
		FastMaxTokens:    bind.config.FastMaxTokens, SlowMaxTokens: bind.config.SlowMaxTokens,
		RequireSilentSlow: true, RetainReasoning: true,
		Now: now, NextID: nextID,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.engine = engine

	speech, err := action.NewSpeech(action.SpeechConfig{
		Provider: bind.config.Speech, Sink: speechSink{runtime: result}, Ledger: result.ledger,
		Playback: result.duplex, FrameDuration: bind.config.FrameDuration, Scheduler: scheduler,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.speech = speech

	tools, err := action.NewTools(action.ToolsConfig{
		Registry: result.registry, Ledger: result.ledger, Store: result.store,
		Confirmer: bind.config.Confirmer, Policy: bind.config.ConfirmPolicy,
		Audit: bind.config.ActionAudit,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.tools = tools

	reserved := 8
	if reserved >= bind.config.MaxPendingEvents {
		reserved = bind.config.MaxPendingEvents - 1
	}
	coordinator, err := eventloop.New(eventloop.Config{
		Store: result.store, Processor: result, MaxPendingEvents: bind.config.MaxPendingEvents,
		ReservedInterruptEvents: reserved, Now: now, NextID: nextID,
	})
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.coordinator = coordinator
	gate, err := interaction.Bind(
		deferralFor(policies.Deferral, result.settings.ManualTurns), result.duplex, coordinator)
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.gate = gate
	// The gate is bound after the coordinator exists because the wake-up
	// wiring needs both; installing it now is what makes deferral active.
	coordinator.SetGate(gate)

	if err := result.applyTools(result.settings.Tools); err != nil {
		cancel(err)
		return nil, err
	}

	result.wait.Add(2)
	go func() {
		defer result.wait.Done()
		result.speech.Run(ctx)
	}()
	go func() {
		defer result.wait.Done()
		result.drive()
	}()
	return result, nil
}

// selectObservers narrows this session's perception to the named set.
//
// Selection is per session rather than per deployment, which is what makes the
// observer set a factor rather than a build-time choice: one server, two
// sessions, one difference between them. An empty selection is the binding's
// default set, which is every observer the deployment configured.
func (runtime *runtime) selectObservers(names []string) error {
	if len(names) == 0 {
		runtime.observerMu.Lock()
		runtime.selected = nil
		runtime.observerMu.Unlock()
		return nil
	}
	available := runtime.observers.Names()
	selected := make(map[string]struct{}, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if !slices.Contains(available, name) {
			return fmt.Errorf("unknown observer %q (available: %s)", name, strings.Join(available, ", "))
		}
		selected[name] = struct{}{}
	}
	runtime.observerMu.Lock()
	runtime.selected = selected
	runtime.observerMu.Unlock()
	return nil
}

// observing reports whether one observer is part of this session's set.
func (runtime *runtime) observing(name string) bool {
	runtime.observerMu.RLock()
	defer runtime.observerMu.RUnlock()
	if runtime.selected == nil {
		return true
	}
	_, chosen := runtime.selected[name]
	return chosen
}

// activeObservers is the selected subset that accepts a frame.
func (runtime *runtime) activeObservers(frame perception.Frame) []perception.Observer {
	matched := runtime.observers.For(frame)
	active := make([]perception.Observer, 0, len(matched))
	for _, observer := range matched {
		if runtime.observing(observer.Name()) {
			active = append(active, observer)
		}
	}
	return active
}

// observerNames reports the session's perception, for evidence and health.
func (runtime *runtime) observerNames() []string {
	runtime.observerMu.RLock()
	defer runtime.observerMu.RUnlock()
	names := make([]string, 0, len(runtime.observers.Names()))
	for _, name := range runtime.observers.Names() {
		if runtime.selected == nil {
			names = append(names, name)
			continue
		}
		if _, chosen := runtime.selected[name]; chosen {
			names = append(names, name)
		}
	}
	return names
}

// Status reports what this session is running.
func (runtime *runtime) Status() binding.Status {
	fast, slow := runtime.engine.Descriptors()
	return binding.Status{
		Binding: runtime.binding.Name(), Ownership: runtime.binding.Ownership(),
		Policies: runtime.policyReport(), Observers: runtime.observerNames(),
		Fast: fast.Provider + "/" + fast.Model, Slow: slow.Provider + "/" + slow.Model,
		Speech: runtime.config.Speech.Descriptor().Name,
	}
}

// policyReport is the policy set this session is actually running, which is
// not always the one it was configured with: a client that took the floor
// replaced the deferral policy, and a report that named the configured one
// would describe a session nobody is having.
func (runtime *runtime) policyReport() interaction.Report {
	report := runtime.policies.Report()
	report.Deferral = deferralFor(runtime.policies.Deferral, runtime.manualTurns()).Name()
	return report
}

// Trajectory returns the canonical log.
func (runtime *runtime) Trajectory() trajectory.Snapshot { return runtime.store.Snapshot() }

// Update applies a client configuration change.
func (runtime *runtime) Update(_ context.Context, settings binding.Settings) error {
	settings = binding.CloneSettings(settings)
	runtime.audioMu.Lock()
	speaking := runtime.acoustic != nil && runtime.acoustic.Speaking()
	runtime.audioMu.Unlock()
	if speaking {
		return errors.New("cannot change session configuration during active speech")
	}
	if err := runtime.applyTools(settings.Tools); err != nil {
		return err
	}
	selection := settings.Observers
	if len(selection) == 0 {
		selection = runtime.config.DefaultObservers
	}
	if err := runtime.selectObservers(selection); err != nil {
		return err
	}
	if settings.ManualTurns != runtime.manualTurns() {
		// Turn detection changed hands. The gate has to be rebound, because
		// which policy defers and which transitions wake it are both decided
		// by who owns the floor.
		runtime.gate.Close()
		gate, err := interaction.Bind(
			deferralFor(runtime.policies.Deferral, settings.ManualTurns), runtime.duplex, runtime.coordinator)
		if err != nil {
			return err
		}
		runtime.gate = gate
		runtime.coordinator.SetGate(gate)
	}
	runtime.settingsMu.Lock()
	runtime.settings = settings
	runtime.settingsMu.Unlock()
	return runtime.resetAcoustic()
}

// Settings returns the current configuration.
func (runtime *runtime) Settings() binding.Settings {
	runtime.settingsMu.RLock()
	defer runtime.settingsMu.RUnlock()
	return binding.CloneSettings(runtime.settings)
}

// applyTools installs the client's declared tools alongside the server's own.
//
// A server-side tool wins on a name collision. A client that declared a tool
// the server also provides is describing something it cannot execute, and the
// declaration that comes with a dispatcher is the one that can actually
// happen.
func (runtime *runtime) applyTools(specs []action.ToolSpec) error {
	combined := make([]action.ToolSpec, 0, len(specs)+len(runtime.config.Tools))
	server := make(map[string]struct{}, len(runtime.config.Tools))
	for _, spec := range runtime.config.Tools {
		server[spec.Name] = struct{}{}
		combined = append(combined, spec)
	}
	for _, spec := range specs {
		if _, shadowed := server[spec.Name]; shadowed {
			continue
		}
		combined = append(combined, spec)
	}
	return runtime.registry.Replace(combined)
}

// resetAcoustic drops the gate so the next frame rebuilds it.
//
// The gate is built from the rate of the audio that actually arrives rather
// than from a constant. Its thresholds are in samples, so a gate built for the
// wrong rate does not fail loudly - it silently waits three times too long for
// an endpoint, which is exactly the kind of bug that looks like a slow model.
func (runtime *runtime) resetAcoustic() error {
	runtime.audioMu.Lock()
	defer runtime.audioMu.Unlock()
	runtime.acoustic, runtime.acousticRate = nil, 0
	runtime.pending = nil
	return nil
}

// acousticFor returns a gate matching the incoming sample rate, building one
// on the first frame and rebuilding it if the rate ever changes.
func (runtime *runtime) acousticFor(rate uint32) (*perception.EnergyGate, error) {
	if runtime.acoustic != nil && runtime.acousticRate == rate {
		return runtime.acoustic, nil
	}
	settings := runtime.Settings()
	gate, err := perception.NewEnergyGate(settings.Gate, rate)
	if err != nil {
		return nil, err
	}
	runtime.acoustic, runtime.acousticRate = gate, rate
	return gate, nil
}

// Video reports that this cascade session has no video observer configured.
// A session that negotiated video gets one installed at construction; one that
// did not says so rather than silently discarding frames.
func (runtime *runtime) Video(ctx context.Context, frame perception.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	matched := runtime.activeObservers(frame)
	if len(matched) == 0 {
		return fmt.Errorf("%w: video input", binding.ErrUnsupported)
	}
	for _, observer := range matched {
		if !observer.Gate(frame) {
			continue
		}
		observations, err := observer.Observe(ctx, []perception.Frame{frame})
		if err != nil {
			return err
		}
		for _, observation := range observations {
			if err := runtime.commitObservation(ctx, observation); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close ends the session.
func (runtime *runtime) Close(ctx context.Context, cause error) error {
	if cause == nil {
		cause = errors.New("session closed")
	}
	runtime.cancel(cause)
	runtime.discardPreparations()
	runtime.speech.Close("session closed")
	runtime.gate.Close()
	runtime.duplex.Close()
	runtime.clientCalls.Close()
	runtime.wait.Wait()
	return nil
}

func (runtime *runtime) nextRevision() uint64 { return runtime.revision.Add(1) }

func (runtime *runtime) fail(code string, err error) {
	if err == nil {
		return
	}
	runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{Code: code, Message: err.Error()})
}

// drive is the session's event-loop driver. It runs everything the loop is
// willing to run, then waits for the next signal - an arriving event, or a
// wake-up releasing work a policy deferred.
func (runtime *runtime) drive() {
	for {
		select {
		case <-runtime.ctx.Done():
			return
		case <-runtime.coordinator.Signal():
			runtime.drain()
		}
	}
}

func (runtime *runtime) drain() {
	for runtime.coordinator.Runnable() {
		_, err := runtime.coordinator.RunNext(runtime.ctx)
		switch {
		case err == nil:
			continue
		case errors.Is(err, eventloop.ErrIdle):
			return
		case errors.Is(err, eventloop.ErrDeferred), errors.Is(err, eventloop.ErrBusy):
			// Committed and waiting. A wake-up owes the next attempt.
			return
		case errors.Is(err, eventloop.ErrInterrupted), errors.Is(err, context.Canceled):
			continue
		default:
			runtime.fail("provider_error", err)
			return
		}
	}
}

func idFor(prefix string, sequence uint64) string {
	return prefix + "_" + strconv.FormatUint(sequence, 10)
}

// toolCatalog exposes the declared tool surface to cognition.
//
// Visibility is not authority: the fast provider sees the same schemas as the
// slow one so it can say which capability a request needs, and its descriptor
// is what makes any call it emits a non-executable proposal.
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
