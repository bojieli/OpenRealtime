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
	return result, nil
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
		remoteInstruction(settings.Instruction), settings.ManualTurns, settings.Modalities))
}

// sessionUpdate builds the session declaration. It is shared with the
// session-instruction handoff, which is the same event carrying different
// text.
func sessionUpdate(instruction string, manualTurns bool, modalities []string) map[string]any {
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
	update := map[string]any{
		"type":         "realtime",
		"instructions": instruction,
		"audio": map[string]any{
			"input":  input,
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
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
		}, Observers: []string{"remote"},
		Fast: "remote/" + runtime.config.Model, Slow: slow.Provider + "/" + slow.Model,
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
	return runtime.configureRemote()
}

// Audio forwards input to the remote, which owns perception.
func (runtime *runtime) Audio(ctx context.Context, frame perception.Frame) error {
	if err := frame.Validate(); err != nil {
		return err
	}
	return runtime.remote.Send(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(frame.PCM16LE),
	})
}

// Video is not supported: the base protocol gives no way to ask a remote
// endpoint whether it accepts video, so this reports honestly rather than
// forwarding events the remote will reject.
func (runtime *runtime) Video(context.Context, perception.Frame) error {
	return fmt.Errorf("%w: video input over an upstream binding", binding.ErrUnsupported)
}

// Text forwards something the client typed to the remote, which owns the
// conversation the user is having.
func (runtime *runtime) Text(ctx context.Context, input binding.TextInput) error {
	role := input.Role
	if role == "" {
		role = "user"
	}
	return runtime.remote.Send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": role,
			"content": []map[string]any{{"type": "input_text", "text": input.Text}},
		},
	})
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
func (runtime *runtime) Cancel(ctx context.Context, reason string) error {
	runtime.coordinator.Interrupt(fmt.Errorf("%s: %w", reason, eventloop.ErrInterrupted))
	return runtime.remote.Send(ctx, map[string]any{"type": "response.cancel"})
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
