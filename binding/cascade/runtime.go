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
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/perception/voices"
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
	voices      *voices.Recogniser
	holdStartNS uint64

	ctx    context.Context
	cancel context.CancelCauseFunc
	wait   sync.WaitGroup

	// window and pinboard are the two halves of what an interaction model
	// reads about the past. They are separate because truncating the first
	// must never be able to repeal something in the second.
	window   *interaction.Window
	pinboard *interaction.Pinboard

	sequence atomic.Uint64
	revision atomic.Uint64
	// continuing is true while a backchannel decision is in flight, so one
	// turn does not accumulate a model call per revision.
	continuing inFlight
	// interjecting_ guards the single in-flight interjection.
	interjecting_ inFlight
	// ordinaryFastRunning includes the interval after an ordinary voice
	// continuation starts and before it has queued speech. Duplex state cannot
	// cover that interval: no audio is audible yet. A user who resumes then has
	// nevertheless overtaken the response. Deliberate interjections are excluded:
	// continued user speech is their premise, not a reason to cancel them.
	ordinaryFastRunning atomic.Int32

	settingsMu sync.RWMutex
	settings   binding.Settings

	// inputMu serialises media-driven endpoint transitions. Audio normally
	// arrives on one protocol reader, but a time-based floor retry is a second
	// producer; without one owner, resumed speech could start between a retry's
	// ForceStop and UserSpeechStopped and be erased by the older transition.
	inputMu       sync.Mutex
	audioMu       sync.Mutex
	acoustic      *perception.EnergyGate
	acousticRate  uint32
	pending       []perception.Frame
	lastObserveNS uint64
	utteranceID   string
	lastStable    string
	// heard is the most recent revision of the utterance in progress. The
	// endpoint decision needs the words so far, and the frame that closes the
	// gate usually carries no new ones - an unchanged transcript produces no
	// observation, which is right for the log and useless for judging a pause.
	heard interaction.Revision
	// pauseStartNS is when the floor first held the current pause, and
	// pauseSilenceNS is how much silence the gate had already accumulated then.
	// Together they form a clock the gate cannot reset by reopening. Keeping an
	// explicit active bit matters for uploaded audio, which may contain 600 ms
	// of silence while only a few milliseconds of wall time have elapsed; zero
	// is a valid monotonic start in deterministic tests and cannot be a sentinel.
	pauseStartNS   uint64
	pauseSilenceNS uint64
	pauseActive    bool
	// pauseHeard is what had been heard when the current pause began, so that a
	// speaker talking through a hold starts a new pause rather than extending
	// one that ended when they spoke.
	pauseHeard string
	// pauseTimer is the wake-up a temporary floor hold owes when no more audio
	// arrives. pauseGeneration makes its callback conditional on this still
	// being the same uninterrupted pause.
	pauseTimer      clock.Timer
	pauseGeneration uint64
	// extractedText is the stretch extraction last read, so an utterance that
	// keeps growing is not re-read from the beginning on every partial.
	extractedText string
	// previousUtterance is the last thing the speaker finished saying, and
	// lastPin is the policy that was read out of it. Both exist for the case
	// where the recogniser cut one sentence into two: the pieces are joined
	// back up before the next reading, and the policy read off the first piece
	// alone goes with it.
	//
	// extractUtterance and its text track which utterance the standing pass is
	// currently reading, because it reads an utterance many times as it grows.
	// Without that, every partial would be joined onto the last joined text
	// and the sentence would compound with itself.
	previousUtterance    string
	extractUtterance     string
	extractUtteranceText string
	// lastPin is the policy read out of the utterance being read now;
	// previousPin is the one read out of the piece before it, which is what a
	// join retires. Kept apart because a revocation matches loosely, so
	// revoking the wrong one lifts a policy nobody cancelled - and because the
	// pin produced from the joined text is the good one and must survive.
	lastPin     interaction.StandingInstruction
	previousPin interaction.StandingInstruction
	// lastPartialExtractNS bounds how often an unfinished utterance is re-read.
	lastPartialExtractNS uint64
	// heardWhenSpoke is how much of the current utterance had been heard when
	// the agent last said something, so a decision can be told what is new.
	heardWhenSpoke string
	// interjectStartNS is when the in-flight interjection claimed its slot.
	interjectStartNS uint64
	// lastSilentActRev is the revision the last silent act answered, and
	// actedOnHeard is what had been heard when it was taken.
	//
	// Both, because a revision is a fresh question and "I already did this" is
	// a property of the stretch of speech rather than of one revision. A
	// recorded menu is one utterance producing a revision every few hundred
	// milliseconds, and one press per revision is nine presses in a call.
	lastSilentActRev uint64
	// lastSilentActNS is when the last one happened, because a fresh utterance
	// looks like a fresh stretch and the prefix test lets it through.
	lastSilentActNS uint64
	// lastQuietNS is when the quiet was last asked about, so a stretch of
	// nothing costs one decision a second rather than one a frame.
	// quietSpokeSince is the stretch already spoken into, because a silence
	// does not stop being evidence once it has been acted on.
	lastQuietNS     uint64
	quietSpokeSince uint64
	actedOnHeard    string
	lastCanonical   uint64
	// interjectingPending bridges the floor's decision to the canonical
	// observation committed by that endpoint; interjectingRevs then key the act
	// to every exact source revision the event loop will process. This is a set
	// because several projected endpoints can queue before cognition catches up.
	interjectingPending bool
	interjectingRevs    map[uint64]struct{}
	// lastInterjectRev is the revision the last interjection answered, so a
	// speaker who keeps talking is not answered once per partial.
	lastInterjectRev uint64
	speechStartNS    uint64

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
		window:   &interaction.Window{},
		pinboard: &interaction.Pinboard{},
		// One per session: the first voice of this conversation is the person
		// this conversation is with, and that is not a fact about the process.
		voices: voices.New(bind.config.Voices, voices.DefaultThreshold, voices.DefaultMinimum),
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
	result.settings.Gate = result.withEndpointSilence(result.settings.Gate)
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
	var fastToolFilter func(continuation.ToolDefinition) bool
	if bind.config.FastComputerUse {
		fastToolFilter = func(tool continuation.ToolDefinition) bool {
			return result.fastExecutableTool(tool.Name)
		}
	}
	engine, err := cognition.New(cognition.Config{
		Store: result.store, Fast: bind.config.Fast, Slow: bind.config.Slow,
		Catalog:          toolCatalog{runtime: result},
		AgentInstruction: cognition.Compose(bind.config.AgentInstruction, result.settings.Instruction),
		FastMaxTokens:    bind.config.FastMaxTokens, SlowMaxTokens: bind.config.SlowMaxTokens,
		FastToolFilter:    fastToolFilter,
		VisualReflex:      result.selectedVisualReflexConfig(),
		RequireSilentSlow: true, RetainReasoning: true,
		// Without this a provider that can see gets the narration and nothing
		// else, which is enough to reason about a screen and not enough to
		// click on one.
		Media: resolveMedia(media),
		Now:   now, NextID: nextID,
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

	audit := func(record action.Record) {
		if bind.config.ActionAudit != nil {
			bind.config.ActionAudit(record)
		}
		phase := "decision"
		if record.Error != "" {
			phase = "error"
		}
		result.debug(result.ctx, binding.DebugEvent{
			Category: "policy", Name: "policy.tool_authorization", Phase: phase,
			CorrelationID: record.CallID, Message: record.Error, Attributes: map[string]any{
				"name": record.Name, "target": record.Target,
				"producer_phase": record.ProducerPhase, "confirmed": record.Confirmed,
				"executed": record.Executed,
			},
		})
	}
	tools, err := action.NewTools(action.ToolsConfig{
		Registry: result.registry, Ledger: result.ledger, Store: result.store,
		Confirmer: bind.config.Confirmer, Policy: bind.config.ConfirmPolicy,
		Audit: audit,
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
	reflex := ""
	if descriptor, enabled := runtime.engine.VisualReflexDescriptor(); enabled {
		reflex = descriptor.Provider + "/" + descriptor.Model
	}
	return binding.Status{
		Binding: runtime.binding.Name(), Profile: runtime.config.Profile,
		Ownership: runtime.binding.Ownership(),
		Policies:  runtime.policyReport(), Observers: runtime.observerNames(),
		Fast: fast.Provider + "/" + fast.Model, Reflex: reflex,
		Slow:   slow.Provider + "/" + slow.Model,
		Speech: runtime.config.Speech.Descriptor().Name,
	}
}

// resolveMedia adapts the session's media store to what a provider adapter
// asks for: the bytes and their type, without the trajectory reference the
// store keeps for its own accounting.
func resolveMedia(store *session.MediaStore) continuation.MediaResolver {
	return func(handle string) (continuation.Media, error) {
		media, err := store.Resolve(handle)
		if err != nil {
			return continuation.Media{}, err
		}
		return continuation.Media{MIMEType: media.Ref.MIMEType, Bytes: media.Bytes}, nil
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
	if runtime.engine != nil {
		if err := runtime.engine.ConfigureVisualReflex(runtime.selectedVisualReflexConfig()); err != nil {
			return err
		}
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
	settings.Gate = runtime.withEndpointSilence(settings.Gate)
	runtime.settings = settings
	runtime.settingsMu.Unlock()
	// The instruction a client sends here is the deployment's own, and the
	// prompts were composed once from whatever was known when the session was
	// built - which is never this. Without it the voice works from the default
	// prompt while the interaction model, which reads the settings on every
	// decision, works from the real one.
	if runtime.engine != nil {
		runtime.engine.SetAgentInstruction(
			cognition.Compose(runtime.config.AgentInstruction, settings.Instruction))
	}
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
	runtime.inputMu.Lock()
	defer runtime.inputMu.Unlock()
	runtime.discardPreparations()
	runtime.audioMu.Lock()
	runtime.cancelPauseRetryLocked()
	runtime.audioMu.Unlock()
	// Release the recogniser. One instance exists per utterance and the
	// endpoint is what normally retires it, but a session that ends while the
	// user is still speaking never reaches an endpoint - and that is the
	// common case, because hanging up mid-sentence is a thing people do. The
	// observer's own documentation already claims this path; nothing was
	// calling it, so the socket and the goroutine reading it outlived the
	// session, and the utterance's counters were never folded in.
	runtime.audio.Reset()
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
	if errors.Is(err, continuation.ErrStalePrefix) {
		// The safe point refused output that answers something the
		// conversation has since moved past. That is the guarantee working -
		// nothing was committed, the trajectory is intact, and whatever
		// overtook it gets its own turn - and it is never a fault in the
		// session.
		//
		// The rule lives here because a continuation commits from more than
		// one place. It was answered at the step that produced it, then at the
		// driver, then at the holding line, and the phone menu still died on
		// it - a recording talks continuously, so something arrives during
		// almost every turn and each route out had to be found separately.
		// One judgement, one place.
		if recorder := runtime.policies.ShadowInteraction; recorder != nil {
			recorder(interaction.ShadowDecision{
				NowNS: runtime.scheduler.NowNS(), Act: "withheld",
				Situation:  "overtaken: " + err.Error(),
				Predicates: map[string]string{"where": code},
			})
		}
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

// toolCatalog exposes the declared tool surface to cognition. Every tool is
// visible as a capability, slow receives all executable schemas, and fast
// receives either proposal-only schemas or only runtime.fastExecutableTool
// definitions at eligible safe points. Schema visibility never changes the
// authority recorded by the provider descriptor and continuation runner.
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
		phase := trajectory.PhaseSlow
		if catalog.runtime.fastExecutableTool(spec.Name) {
			phase = trajectory.PhaseFast
		}
		capabilities = append(capabilities, continuation.Capability{
			Name: spec.Name, Description: spec.Description, Available: true,
			ExecutionPhase:       string(phase),
			ConfirmationRequired: spec.Confirm != action.ConfirmNever,
		})
	}
	return capabilities
}

func (runtime *runtime) fastExecutableTool(name string) bool {
	if !runtime.config.FastComputerUse {
		return false
	}
	return runtime.boundedComputerTool(name)
}

func (runtime *runtime) visualReflexExecutableTool(name string) bool {
	if runtime.config.VisualReflex == nil {
		return false
	}
	return runtime.boundedComputerTool(name)
}

func (runtime *runtime) selectedVisualReflexConfig() *cognition.VisualReflexConfig {
	if runtime.config.VisualReflex == nil {
		return nil
	}
	selectedVisualObserver := false
	for _, factory := range runtime.config.Observers {
		if factory.Kind == perception.FrameImage && runtime.observing(factory.Name) {
			selectedVisualObserver = true
			break
		}
	}
	if !selectedVisualObserver {
		return nil
	}
	return &cognition.VisualReflexConfig{
		Provider: runtime.config.VisualReflex,
		ToolFilter: func(tool continuation.ToolDefinition) bool {
			return runtime.visualReflexExecutableTool(tool.Name)
		},
		MaxOutputTokens: runtime.config.VisualReflexMaxTokens,
		Timeout:         runtime.config.VisualReflexTimeout,
	}
}

func (runtime *runtime) boundedComputerTool(name string) bool {
	if !computeruse.IsReflexAction(name) {
		return false
	}
	spec, declared := runtime.registry.Lookup(name)
	if !declared {
		return false
	}
	if spec.Dispatcher != nil {
		// A server-owned standard action crosses the local action boundary,
		// which applies its declared confirmation policy and target dispatcher.
		return true
	}
	// The client is the action environment. Fast may emit to it only when the
	// client explicitly waived confirmation and declared the bounded context;
	// policy/always must be answered before the call crosses the wire.
	return spec.Confirm == action.ConfirmNever && strings.TrimSpace(spec.Target) != ""
}

// withEndpointSilence applies the deployment's own endpoint threshold.
//
// The gate's job here is to say when the energy stopped, not when the
// turn did. Waiting half a second to say so is a turn-taking decision
// taken by a threshold, and it is taken before the model that owns
// turn-taking is ever asked: answer is withheld from the act menu
// while the gate reports somebody speaking, so the one act that could
// end a turn early is unavailable until the wait is already over.
//
// Measured on a finished question, the model chose listen on every
// partial with the whole sentence in front of it and answered only at
// the 500ms mark - not a judgement, an act it was not offered.
//
// Closing sooner costs decisions rather than correctness, because a
// speaker who was only drawing breath is caught by the same path that
// already exists: the floor is asked, says listen, and the gate is
// reopened.
//
// Applied wherever settings arrive, not only at construction: every client
// sends a gate config in session.update - the harness here sends the library
// default - and a deployment threshold that the first update overwrites is a
// flag that does nothing. Measured that way it did exactly nothing, and the
// decisions still read silence: 500ms.
func (runtime *runtime) withEndpointSilence(gate perception.GateConfig) perception.GateConfig {
	if runtime.config.EndpointSilenceMS > 0 {
		gate.SilenceDurationMS = runtime.config.EndpointSilenceMS
	}
	return gate
}
