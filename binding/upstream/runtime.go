package upstream

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
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
	"github.com/bojieli/OpenRealtime/realtimeclient"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// runtime mirrors one remote Realtime session into the canonical trajectory
// and runs the engine's background reasoner over it.
type runtime struct {
	config    Config
	binding   *Binding
	sink      binding.Sink
	policies  interaction.Policies
	scheduler clock.Scheduler

	remote      RemoteConn
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

	media *session.MediaStore
	// observers are this side's eyes; selected is the subset the client
	// asked for, or nil for all of them.
	observers  *perception.Set
	observerMu sync.RWMutex
	selected   map[string]struct{}
	// pins are the standing instructions extracted from what the user said
	// and steered into the remote.
	pinMu sync.Mutex
	pins  []interaction.StandingInstruction
	// liveEpochNS is this side's clock when the remote's session timeline
	// began, so the remote's milliseconds become this side's nanoseconds.
	liveEpochNS uint64
	released    sync.Once

	// remoteMu guards what the remote has told us about itself and what it
	// is waiting on.
	remoteMu sync.Mutex
	remote_  binding.RemoteStatus
	// escalationPending is set when the remote asked for help and the batch
	// carrying that request has not yet been planned; it survives the signal
	// and the transcript landing in different batches.
	escalationPending bool
	// suppressAudio holds back the remote's output while a barge-in it has
	// not yet obeyed is outstanding.
	suppressAudio bool
	// bargeGate hears the user directly, so a barge-in does not wait for the
	// remote to transcribe what it just heard.
	bargeGate   *perception.EnergyGate
	bargeGateMu sync.Mutex
	// contextPending is observer evidence coalesced for the remote, and
	// contextTimer is the debounce that sends it.
	contextPending []string
	contextTimer   clock.Timer
	idleTimer      clock.Timer
	lastSpeechNS   uint64

	stateMu   sync.Mutex
	utterance *action.Utterance
	// restoreInstruction marks that the session instruction currently carries
	// a one-shot handoff and has to be put back when the response completes.
	restoreInstruction bool
	// answer holds the completed slow answer waiting to be handed off.
	answer string
	// clientCalls holds calls the remote must not see: they were issued by
	// the engine's slow provider, and only the client executes them.
	clientCalls *clientcalls.Tracker
}

func newRuntime(parent context.Context, bind *Binding, options binding.Options) (*runtime, error) {
	if options.Sink == nil {
		return nil, errors.New("an upstream session requires a sink")
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
		config: bind.config, binding: bind, sink: options.Sink, policies: policies,
		scheduler: bind.config.Scheduler, store: trajectory.NewStore(),
		duplex: session.NewDuplex(session.DuplexConfig{Scheduler: bind.config.Scheduler}),
		ledger: action.NewLedger(), registry: action.NewRegistry(),
		ctx: ctx, cancel: cancel, settings: binding.CloneSettings(options.Settings),
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
	media, err := session.NewMediaStore(bind.config.MediaRetention)
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.media = media
	if bind.config.BargeIn != nil && *bind.config.BargeIn {
		// The remote reports the user's speech only once it has transcribed
		// it, which is far too late to stop a voice mid-sentence: measured
		// against the real endpoint that path alone left 1.08 s of speech
		// after the user took the floor. The same audio arrives here first,
		// so it is heard here.
		gate, err := perception.NewEnergyGate(perception.DefaultGateConfig(), 24_000)
		if err != nil {
			cancel(err)
			return nil, fmt.Errorf("configure the barge-in gate: %w", err)
		}
		result.bargeGate = gate
	}
	if len(bind.config.Observers) > 0 {
		observers := make([]perception.Observer, 0, len(bind.config.Observers))
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
	}
	prefix := strings.TrimSpace(options.SessionID)
	if prefix == "" {
		prefix = "upstream"
	}
	nextID := func(kind string) string {
		return fmt.Sprintf("%s_%s_%d", prefix, kind, result.sequence.Add(1))
	}
	now := func() uint64 { return bind.config.Scheduler.NowNS() }

	dial := bind.config.Dial
	if dial == nil {
		dial = dialRealtime
	}
	remote, err := dial(ctx, bind.config)
	if err != nil {
		cancel(err)
		return nil, err
	}
	result.remote = remote

	// The engine's fast provider is the remote, which is not a
	// continuation.Provider at all. A silent stand-in keeps the cognition
	// engine's arrangement honest: it never runs, and nothing can accidentally
	// route a fast continuation through it.
	engine, err := cognition.New(cognition.Config{
		Store: result.store, Fast: remoteStandIn{}, Slow: bind.config.Slow,
		Catalog:          toolCatalog{runtime: result},
		AgentInstruction: cognition.Compose(bind.config.AgentInstruction, result.settings.Instruction),
		SlowMaxTokens:    bind.config.SlowMaxTokens, RequireSilentSlow: true, RetainReasoning: true,
		ExternalFast: true,
		Now:          now, NextID: nextID,
	})
	if err != nil {
		cancel(err)
		_ = remote.Close()
		return nil, err
	}
	result.engine = engine

	tools, err := action.NewTools(action.ToolsConfig{
		Registry: result.registry, Ledger: result.ledger, Store: result.store,
	})
	if err != nil {
		cancel(err)
		_ = remote.Close()
		return nil, err
	}
	result.tools = tools

	coordinator, err := eventloop.New(eventloop.Config{
		Store: result.store, Processor: result, MaxPendingEvents: bind.config.MaxPendingEvents,
		ReservedInterruptEvents: 8, Now: now, NextID: nextID,
	})
	if err != nil {
		cancel(err)
		_ = remote.Close()
		return nil, err
	}
	result.coordinator = coordinator
	gate, err := interaction.Bind(policies.Deferral, result.duplex, coordinator)
	if err != nil {
		cancel(err)
		_ = remote.Close()
		return nil, err
	}
	coordinator.SetGate(gate)

	if err := result.registry.Replace(result.settings.Tools); err != nil {
		cancel(err)
		_ = remote.Close()
		return nil, err
	}

	result.wait.Add(2)
	go func() {
		defer result.wait.Done()
		result.mirror()
	}()
	go func() {
		defer result.wait.Done()
		result.drive()
	}()
	if err := result.configureRemote(); err != nil {
		cancel(err)
		_ = remote.Close()
		return nil, err
	}
	result.armIdle()
	if greeting := bind.config.Greeting; greeting != "" {
		// Sent after the session declaration, and on GPT-Live held by the
		// translator until the session has started - which is the vendor's
		// recipe for a voice that speaks first: an instruction to greet, with
		// input audio already running.
		if err := result.Steer(ctx, greeting); err != nil {
			cancel(err)
			_ = remote.Close()
			return nil, err
		}
	}
	return result, nil
}

// selectObservers applies a client's observer choice.
func (runtime *runtime) selectObservers(names []string) error {
	if runtime.observers == nil {
		if len(names) > 0 {
			return fmt.Errorf("no observers are configured; cannot select %s", strings.Join(names, ", "))
		}
		return nil
	}
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

// observing reports whether a named observer is in the selected set.
func (runtime *runtime) observing(name string) bool {
	runtime.observerMu.RLock()
	defer runtime.observerMu.RUnlock()
	if runtime.selected == nil {
		return true
	}
	_, chosen := runtime.selected[name]
	return chosen
}

// observerNames reports the session's perception, for Status.
func (runtime *runtime) observerNames() []string {
	names := []string{"remote"}
	if runtime.observers == nil {
		return names
	}
	for _, name := range runtime.observers.Names() {
		if runtime.observing(name) {
			names = append(names, name)
		}
	}
	return names
}

// commitObserved records what an observer of this side's saw. It is
// observer-authority evidence: data the reasoner may read and the remote may
// be told, never an instruction.
func (runtime *runtime) commitObserved(ctx context.Context, observation perception.Observation) error {
	if err := observation.Validate(); err != nil {
		return err
	}
	if err := runtime.sink.Observation(ctx, observation); err != nil {
		return err
	}
	_, err := runtime.coordinator.Submit(eventloop.Event{
		Type: "observer." + observation.Observer, Source: observation.Observer, Channel: "observation",
		Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
		SourceRevision: runtime.nextRevision(), Producer: observation.Producer(),
		Content: observation.Text, Observation: observation.Meta(),
		OccurredNS: observation.OccurredNS,
	})
	return err
}

// occurredAt translates a moment on the remote's session timeline into this
// side's clock, or reports zero when the remote gave no moment or has not
// started - which is every Realtime endpoint, whose events carry none.
func (runtime *runtime) occurredAt(sessionMS int64) uint64 {
	runtime.remoteMu.Lock()
	epoch := runtime.liveEpochNS
	runtime.remoteMu.Unlock()
	if epoch == 0 || sessionMS <= 0 {
		return 0
	}
	return epoch + uint64(sessionMS)*1_000_000
}

// extractStanding reads one finished user utterance for a policy set out
// loud - "don't cut me off", "keep answers short" - and steers the remote
// with what it finds. It runs off the mirror, because it is a model call and
// the mirror is the read side of the remote.
func (runtime *runtime) extractStanding(utterance string) {
	extractor := runtime.policies.Extraction
	if extractor == nil || len(strings.TrimSpace(utterance)) < 12 {
		return
	}
	go func() {
		recent := interaction.RecentLines(runtime.store.Snapshot().Items, 6)
		runtime.pinMu.Lock()
		existing := slices.Clone(runtime.pins)
		runtime.pinMu.Unlock()
		extraction, err := extractor.Extract(runtime.ctx, existing, recent, utterance)
		if err != nil {
			runtime.debug(binding.DebugEvent{
				Category: "policy", Name: "upstream.extraction.failed", Message: err.Error(),
			})
			return
		}
		for _, pin := range extraction.Pins {
			runtime.pinMu.Lock()
			runtime.pins = append(runtime.pins, pin)
			runtime.pinMu.Unlock()
			_ = runtime.Steer(runtime.ctx, "The user set a standing instruction, which applies until they lift it: "+pin.Text)
			runtime.debug(binding.DebugEvent{
				Category: "policy", Name: "upstream.standing.pinned", Message: pin.Text,
			})
		}
		for _, revoked := range extraction.Revokes {
			runtime.pinMu.Lock()
			runtime.pins = slices.DeleteFunc(runtime.pins, func(pin interaction.StandingInstruction) bool {
				return pin.Text == revoked
			})
			runtime.pinMu.Unlock()
			_ = runtime.Steer(runtime.ctx, "The user lifted a standing instruction; it no longer applies: "+revoked)
			runtime.debug(binding.DebugEvent{
				Category: "policy", Name: "upstream.standing.revoked", Message: revoked,
			})
		}
	}()
}

// isLive reports whether the remote speaks GPT-Live, the one dialect with a
// delegation seam and push channels of its own.
func (runtime *runtime) isLive() bool { return runtime.config.Dialect == DialectGPTLive }

// debug emits implementation evidence when the sink can take it.
func (runtime *runtime) debug(event binding.DebugEvent) {
	if sink, enabled := runtime.sink.(binding.DebugSink); enabled {
		_ = sink.Debug(runtime.ctx, event)
	}
}

// remoteStatus returns a copy of what the remote has reported.
func (runtime *runtime) remoteStatus() binding.RemoteStatus {
	runtime.remoteMu.Lock()
	defer runtime.remoteMu.Unlock()
	return runtime.remote_
}

// armIdle schedules the idle check, which closes a session nobody is talking
// to. The clock is user speech, not client audio: a client streaming silence
// is exactly the abandoned tab this exists for.
func (runtime *runtime) armIdle() {
	timeout := runtime.config.IdleTimeout
	if timeout <= 0 {
		return
	}
	runtime.remoteMu.Lock()
	if runtime.idleTimer != nil {
		runtime.idleTimer.Stop()
	}
	runtime.idleTimer = runtime.scheduler.AfterFunc(timeout, func() {
		runtime.remoteMu.Lock()
		last := runtime.lastSpeechNS
		runtime.remoteMu.Unlock()
		if runtime.scheduler.NowNS()-last < uint64(timeout.Nanoseconds()) {
			runtime.armIdle()
			return
		}
		runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
			Code: "upstream_idle",
			Message: "no user speech for " + timeout.String() +
				"; the session was closed so the remote's meter stops",
		})
		_ = runtime.Close(runtime.ctx, errors.New("idle: no user speech for "+timeout.String()))
	})
	runtime.remoteMu.Unlock()
}

// noteUserSpeech records that somebody is talking, for the idle clock.
func (runtime *runtime) noteUserSpeech() {
	runtime.remoteMu.Lock()
	runtime.lastSpeechNS = runtime.scheduler.NowNS()
	runtime.remoteMu.Unlock()
}

// considerBargeIn asks this binding's policy whether the remote should stop,
// and steers it if so.
//
// The remote owns the floor, so this is an opinion offered rather than a
// cancellation performed: what it sends is the one instruction Live honours
// mid-sentence. The policy is the same one the cascade uses, so a deployment
// that has tuned how its agent handles being talked over gets that behaviour
// here too - including the half of it that says to keep going, for a
// backchannel or for speech aimed at somebody else.
func (runtime *runtime) considerBargeIn() {
	if runtime.config.BargeIn == nil || !*runtime.config.BargeIn {
		return
	}
	state := runtime.duplex.Snapshot()
	if !state.Overlapping() {
		return
	}
	now := runtime.scheduler.NowNS()
	outcome := runtime.policies.BargeIn.Decide(interaction.BargeInInput{
		Context: interaction.Context{NowNS: now, Duplex: state},
	})
	runtime.debug(binding.DebugEvent{
		Category: "policy", Name: "upstream.bargein",
		Attributes: map[string]any{"cancel": outcome.Cancel, "reason": outcome.Reason},
	})
	if !outcome.Cancel {
		return
	}
	// The remote is speaking and the user has taken the floor. Two things
	// follow, and the second is the one the user hears.
	//
	// Telling the remote to stop is the only thing the protocol offers, and it
	// is not fast: the instruction has to be injected before the model acts on
	// it, and the vendor says in as many words that the acknowledgement does
	// not prove the assistant stopped speaking or that queued audio stopped
	// playing. Measured against the real endpoint, it took 4.6 s - better than
	// the 16.4 s of not asking, and far past the second a person waits before
	// deciding they have not been heard.
	//
	// So the audio is held here as well. This binding is the media relay
	// between the remote and the client, which is exactly where the vendor
	// says to block output when an application needs speech to stop; holding
	// it makes the silence immediate for the person who interrupted, while the
	// instruction does the slower work of stopping the model itself.
	runtime.remoteMu.Lock()
	runtime.suppressAudio = true
	runtime.remoteMu.Unlock()
	runtime.duplex.AgentAudioStopped(now)
	if err := runtime.Steer(runtime.ctx, "Stop speaking now and listen to the user."); err != nil {
		runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
			Code: "upstream_bargein", Message: err.Error(),
		})
	}
}

// audioSuppressed reports whether the remote's output is being held back
// because it was asked to stop and has not yet done so.
func (runtime *runtime) audioSuppressed() bool {
	runtime.remoteMu.Lock()
	defer runtime.remoteMu.Unlock()
	return runtime.suppressAudio
}

// resumeAudio lets the remote be heard again. The interrupted utterance is
// over, so whatever it says next is an answer to the person who interrupted.
func (runtime *runtime) resumeAudio() {
	runtime.remoteMu.Lock()
	runtime.suppressAudio = false
	runtime.remoteMu.Unlock()
}

// configureRemote declares the session on the remote side.
//
// The remote is told about the tools so its own fast turn can mention them,
// but it is never given execution authority over them: the engine's slow
// provider is the only thing that calls them, which is the same authority
// boundary every other binding holds.
func (runtime *runtime) configureRemote() error {
	settings := runtime.Settings()
	return runtime.remote.Send(runtime.ctx, sessionUpdate(
		remoteInstruction(settings.Instruction), settings))
}

// sessionUpdate builds the session declaration. It is shared with the
// session-instruction handoff, which is the same event carrying different
// text.
func sessionUpdate(instruction string, settings binding.Settings) map[string]any {
	manualTurns, modalities := settings.ManualTurns, settings.Modalities
	input := map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}}
	if manualTurns {
		// The client took the floor, and the remote is the side that holds it
		// here. Forwarding the declaration is the whole of what this binding
		// can do about it, and the whole of what it needs to do: the remote
		// speaks this protocol, so a null detector means the same thing to it.
		// Keeping it to ourselves would leave the remote ending turns on
		// silence while the client believed it had stopped that.
		input["turn_detection"] = nil
	}
	output := map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}}
	if voice := strings.TrimSpace(settings.Voice); voice != "" {
		// The client chose a voice, and the remote owns the voice. Forwarding
		// it is the whole of what this binding can do about it.
		output["voice"] = voice
	}
	update := map[string]any{
		"type":         "realtime",
		"instructions": instruction,
		"audio": map[string]any{
			"input":  input,
			"output": output,
		},
	}
	if len(modalities) > 0 {
		// The remote speaks this protocol and would honour this, so not
		// telling it leaves it synthesising a full audio response for a client
		// that asked for text - billed as audio output, sent over the network,
		// and decoded and dropped on this side. Unlike the other bindings,
		// where a text session merely wastes local synthesis, here the waste
		// is the provider's meter and the client is the one paying it.
		update["output_modalities"] = slices.Clone(modalities)
	}
	return map[string]any{"type": "session.update", "session": update}
}

// dialRealtime opens an ordinary Realtime WebSocket.
func dialRealtime(ctx context.Context, config Config) (RemoteConn, error) {
	return realtimeclient.Dial(ctx, realtimeclient.Config{
		URL: config.URL, Token: config.Token, Model: config.Model,
		Header: config.Header, EventAliases: config.EventAliases,
	})
}

func remoteInstruction(agent string) string {
	instruction := "You are the voice of this agent. Answer briefly and naturally. " +
		"A background reasoner shares this conversation and will hand you completed answers to say; " +
		"when one arrives, say it and add nothing to it."
	return cognition.Compose(agent, instruction)
}

// Status reports what this session is running.
func (runtime *runtime) Status() binding.Status {
	_, slow := runtime.engine.Descriptors()
	var remote *binding.RemoteStatus
	if status := runtime.remoteStatus(); status != (binding.RemoteStatus{}) {
		remote = &status
	}
	return binding.Status{
		Binding: runtime.binding.Name(), Profile: "voice", Ownership: runtime.binding.Ownership(),
		Stack:    runtime.binding.Capabilities().Stack,
		Policies: runtime.policies.Report(), Interaction: binding.InteractionStatus{
			Evidence: "remote-multimodal",
			EvidenceCapabilities: binding.InteractionEvidenceCapabilities{
				NativeModelState: true,
			},
			Transport: "upstream", ActHandoff: "none",
			Control: binding.InteractionControl{
				Selectors: binding.InteractionControllers{Remote: true}, Arbitration: "single",
			},
		}, Tools: binding.ToolStatus{
			Fast: "propose", Slow: string(slow.EffectiveToolAuthority()),
			Authorization: "engine", Execution: "engine-or-client",
		}, Observers: runtime.observerNames(),
		Fast: "remote/" + runtime.config.Model, Slow: slow.Provider + "/" + slow.Model,
		Remote: remote,
	}
}

func (runtime *runtime) Trajectory() trajectory.Snapshot { return runtime.store.Snapshot() }

func (runtime *runtime) Settings() binding.Settings {
	runtime.settingsMu.RLock()
	defer runtime.settingsMu.RUnlock()
	return binding.CloneSettings(runtime.settings)
}

// Update applies a client configuration change to both sides.
func (runtime *runtime) Update(_ context.Context, settings binding.Settings) error {
	settings = binding.CloneSettings(settings)
	if err := runtime.registry.Replace(settings.Tools); err != nil {
		return err
	}
	runtime.settingsMu.Lock()
	runtime.settings = settings
	runtime.settingsMu.Unlock()
	if len(settings.Observers) > 0 || runtime.observers != nil {
		if err := runtime.selectObservers(settings.Observers); err != nil {
			return err
		}
	}
	return runtime.configureRemote()
}

// Audio forwards input to the remote, which owns perception.
//
// It also listens, when a barge-in policy is in force. The remote owns
// perception and reports the user only after transcribing them, which cannot
// stop a voice mid-sentence; this side has the same audio first, so the
// acoustic gate here decides when the user has taken the floor and the policy
// acts on it at once.
func (runtime *runtime) Audio(ctx context.Context, frame perception.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	runtime.hearUser(frame)
	return runtime.remote.Send(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(frame.PCM16LE),
	})
}

// hearUser runs one input frame past the barge-in gate.
func (runtime *runtime) hearUser(frame perception.Frame) {
	if runtime.bargeGate == nil || len(frame.PCM16LE) == 0 {
		return
	}
	runtime.bargeGateMu.Lock()
	result, err := runtime.bargeGate.Push(frame.PCM16LE)
	runtime.bargeGateMu.Unlock()
	if err != nil || !result.Started {
		return
	}
	runtime.duplex.UserSpeechStarted(runtime.scheduler.NowNS())
	runtime.noteUserSpeech()
	runtime.considerBargeIn()
}

// Video hands a frame to this side's observers.
//
// A frame never goes to the remote: the base protocol gives no way to ask an
// endpoint whether it accepts one, and GPT-Live accepts none. It goes to an
// observer configured here - the same screen narrator the cascade runs - and
// what the observer says is committed as observer-authority evidence, which
// the reasoner reads and the remote is told as context. Without an observer
// this is unsupported, as it always was, and the protocol layer reports that
// at negotiation rather than here.
func (runtime *runtime) Video(ctx context.Context, frame perception.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	if runtime.observers == nil {
		return fmt.Errorf("%w: video input over an upstream binding without a video observer", binding.ErrUnsupported)
	}
	var active []perception.Observer
	for _, observer := range runtime.observers.For(frame) {
		if runtime.observing(observer.Name()) {
			active = append(active, observer)
		}
	}
	if len(active) == 0 {
		return fmt.Errorf("%w: no selected observer accepts %s frames", binding.ErrUnsupported, frame.Source)
	}
	for _, observer := range active {
		if !observer.Gate(frame) {
			continue
		}
		observations, err := observer.Observe(ctx, []perception.Frame{frame})
		if err != nil {
			return err
		}
		for _, observation := range observations {
			if err := runtime.commitObserved(ctx, observation); err != nil {
				return err
			}
		}
	}
	return nil
}

// Text commits something the client typed and tells the remote about it.
//
// It is committed here first, because it used to be forwarded and nothing else,
// on the assumption that the remote would echo it back as a transcript. No
// endpoint does, so the reasoner never saw a typed word. A typed message is the
// user talking, and the trajectory is where what the user said goes.
//
// What the remote is then told depends on what it is. A Realtime endpoint gets
// the item, as before, and answers it on the next response. GPT-Live gets a
// note: the vendor's guidance is that a typed value is user data for the
// backend, not an instruction to the voice, so the voice is told that the user
// typed and the reasoner is handed the text. Attached images go the same way -
// the voice cannot see, the reasoner can.
func (runtime *runtime) Text(ctx context.Context, input binding.TextInput) error {
	role := input.Role
	if role == "" {
		role = "user"
	}
	authority := trajectory.AuthorityUser
	if role == "system" {
		// A system message from a client is context, not the user talking,
		// and it must not be able to act like a request.
		authority = trajectory.AuthorityObserver
	}
	text := strings.TrimSpace(input.Text)
	media, err := runtime.retain(input.Images)
	if err != nil {
		return err
	}
	if text == "" && len(media) > 0 {
		text = "The user attached an image."
	}
	if text != "" {
		observation := perception.Observation{
			Text: text, Observer: "client", Source: "text",
			Authority: authority, Media: media, Final: true,
		}
		if err := runtime.sink.Observation(ctx, observation); err != nil {
			return err
		}
		if _, err := runtime.coordinator.Submit(eventloop.Event{
			Type: "client.text", Source: "client", Channel: "text",
			Priority: eventloop.PriorityRoutine, Kind: trajectory.KindObservation,
			SourceRevision: runtime.nextRevision(), Producer: observation.Producer(),
			Content: text, Observation: observation.Meta(),
		}); err != nil {
			return err
		}
		if authority == trajectory.AuthorityUser {
			runtime.noteUserSpeech()
			if runtime.config.delegationGated {
				// Typed input never passes through the voice, so the voice
				// will never delegate it. The reasoner is the backend the
				// typed value was meant for, and it runs.
				runtime.remoteMu.Lock()
				runtime.escalationPending = true
				runtime.remoteMu.Unlock()
			}
		}
	}
	if runtime.isLive() {
		if authority != trajectory.AuthorityUser || text == "" {
			// Observer-authority context reaches the voice through the same
			// path every other observation takes.
			return nil
		}
		return runtime.remote.Send(ctx, map[string]any{
			"type": "openrealtime.upstream.context",
			"text": "The user typed rather than said: " + text,
		})
	}
	return runtime.remote.Send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": role,
			"content": []map[string]any{{"type": "input_text", "text": input.Text}},
		},
	})
}

// retain keeps attached images for the reasoner and returns their handles.
func (runtime *runtime) retain(images []binding.Image) ([]trajectory.MediaRef, error) {
	if len(images) == 0 {
		return nil, nil
	}
	refs := make([]trajectory.MediaRef, 0, len(images))
	for _, image := range images {
		reference, err := runtime.media.Retain(trajectory.MediaRef{
			MIMEType: image.MIMEType, Source: "message",
			Width: image.Width, Height: image.Height,
			CapturedNS: runtime.scheduler.NowNS(),
		}, image.Bytes)
		if err != nil {
			return nil, fmt.Errorf("retain attached image: %w", err)
		}
		refs = append(refs, reference)
	}
	return refs, nil
}

// CreateResponse asks the remote to respond now.
// CommitAudio forwards the client's turn declaration to the remote.
//
// This binding's floor belongs to whatever is at the other end of the socket,
// so a commit is not something to interpret here - it is something to pass on,
// exactly as the client sent it. The remote answers with its own
// input_audio_buffer.committed, which the mirror renders.
func (runtime *runtime) CommitAudio(ctx context.Context) error {
	return runtime.remote.Send(ctx, map[string]any{"type": "input_audio_buffer.commit"})
}

func (runtime *runtime) CreateResponse(ctx context.Context) error {
	return runtime.remote.Send(ctx, map[string]any{"type": "response.create"})
}

// Cancel cancels generation on both sides.
//
// A Realtime endpoint has a response to cancel. GPT-Live has none; what it has
// is an instruction channel that interrupts speech in progress, which is what
// the caller meant by cancelling.
func (runtime *runtime) Cancel(ctx context.Context, reason string) error {
	runtime.coordinator.Interrupt(fmt.Errorf("%s: %w", reason, eventloop.ErrInterrupted))
	if runtime.isLive() {
		return runtime.remote.Send(ctx, map[string]any{
			"type": "openrealtime.upstream.steer", "text": "Stop speaking now and wait for the user.",
		})
	}
	return runtime.remote.Send(ctx, map[string]any{"type": "response.cancel"})
}

// Steer gives the remote an application instruction that applies now.
//
// It is exported for the pieces of this runtime that have something to say
// which is neither an answer nor evidence: a guardrail, a standing instruction
// somebody set out loud, a greeting. On GPT-Live it interrupts speech in
// progress; on a Realtime endpoint it is context the next response reads.
func (runtime *runtime) Steer(ctx context.Context, instruction string) error {
	instruction = strings.TrimSpace(instruction)
	if instruction == "" {
		return nil
	}
	if runtime.isLive() {
		return runtime.remote.Send(ctx, map[string]any{
			"type": "openrealtime.upstream.steer", "text": instruction,
		})
	}
	return runtime.remote.Send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": instruction}},
		},
	})
}

// pushContext gives the remote evidence it should know without saying.
//
// This is the silent hand-off. Observer-authority evidence - a screen change,
// a typed field, a camera frame - reaches the voice without a model call,
// which is the vendor's own guidance: build the summary from state, not from a
// model. On GPT-Live it is the thinking channel; on a Realtime endpoint it is a
// conversation item with no response requested, which is the portable
// equivalent - context added, nothing asked.
func (runtime *runtime) pushContext(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if runtime.isLive() {
		return runtime.remote.Send(ctx, map[string]any{
			"type": "openrealtime.upstream.context", "text": text,
		})
	}
	return runtime.remote.Send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "system",
			"content": []map[string]any{{"type": "input_text", "text": text}},
		},
	})
}

// queueContext coalesces evidence and schedules one push for the latest
// state. A burst of screen changes is one summary; the vendor asks for
// exactly that, and the endpoint's context window is what a summary per
// change would spend.
func (runtime *runtime) queueContext(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	runtime.remoteMu.Lock()
	defer runtime.remoteMu.Unlock()
	ratio := runtime.remote_.ContextWindowRatio
	if ratio >= 0.9 {
		// The endpoint is about to compact. Evidence pushed now is evidence
		// summarised away in seconds; the trajectory keeps it regardless.
		runtime.debug(binding.DebugEvent{
			Category: "session", Name: "upstream.context.suppressed",
			Message:    "context window at " + fmt.Sprintf("%.0f%%", ratio*100) + "; evidence kept locally only",
			Attributes: map[string]any{"context_window_ratio": ratio},
		})
		return
	}
	runtime.contextPending = append(runtime.contextPending, text)
	debounce := runtime.config.ContextPushDebounce
	if ratio >= 0.75 {
		debounce *= 2
	}
	if runtime.contextTimer != nil {
		runtime.contextTimer.Stop()
	}
	runtime.contextTimer = runtime.scheduler.AfterFunc(debounce, runtime.flushContext)
}

// flushContext sends what has been queued as one push.
func (runtime *runtime) flushContext() {
	runtime.remoteMu.Lock()
	pending := runtime.contextPending
	runtime.contextPending = nil
	runtime.contextTimer = nil
	runtime.remoteMu.Unlock()
	if len(pending) == 0 {
		return
	}
	if err := runtime.pushContext(runtime.ctx, strings.Join(pending, "\n")); err != nil {
		runtime.sink.Failed(runtime.ctx, binding.ErrorEvent{
			Code: "upstream_context_push", Message: err.Error(),
		})
	}
}

// Truncate forwards client playback truncation to the remote, which owns
// action and is therefore the side that must know what was actually heard.
func (runtime *runtime) Truncate(ctx context.Context, truncation binding.Truncation) error {
	return runtime.remote.Send(ctx, map[string]any{
		"type": "conversation.item.truncate", "item_id": truncation.ItemID,
		"content_index": 0, "audio_end_ms": truncation.AudioEndMS,
	})
}

// Close ends both sides.
func (runtime *runtime) Close(_ context.Context, cause error) error {
	if cause == nil {
		cause = errors.New("session closed")
	}
	runtime.cancel(cause)
	runtime.released.Do(runtime.binding.release)
	runtime.remoteMu.Lock()
	if runtime.idleTimer != nil {
		runtime.idleTimer.Stop()
	}
	if runtime.contextTimer != nil {
		runtime.contextTimer.Stop()
	}
	runtime.remoteMu.Unlock()
	err := runtime.remote.Close()
	runtime.duplex.Close()
	runtime.clientCalls.Close()
	runtime.wait.Wait()
	return err
}

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

func (runtime *runtime) nextRevision() uint64 { return runtime.revision.Add(1) }

// toolCatalog is the tool surface the engine's slow provider may execute.
type toolCatalog struct{ runtime *runtime }

func (catalog toolCatalog) Tools() []continuation.ToolDefinition {
	specs := catalog.runtime.registry.Specs()
	tools := make([]continuation.ToolDefinition, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, continuation.ToolDefinition{
			Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters,
			Background: spec.Background,
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

// remoteStandIn occupies the fast slot in the cognition engine.
//
// The remote model is the fast provider, and it is not a continuation.Provider
// at all: it speaks the Realtime protocol, not this one. A silent stand-in that
// refuses to run keeps the engine's arrangement checkable and makes any
// accidental fast continuation an immediate, obvious failure rather than a
// second voice nobody expected.
type remoteStandIn struct{}

func (remoteStandIn) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "remote", Model: "realtime-endpoint", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, ToolAuthority: continuation.ToolAuthorityNone,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}
}

func (remoteStandIn) Continue(context.Context, continuation.Request, continuation.Emit) (continuation.Completion, error) {
	return continuation.Completion{}, errors.New("the remote endpoint owns the fast voice; the engine must not run one")
}
