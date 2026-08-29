// Package sidecarbinding is the runtime shared by every binding whose model
// lives behind a process boundary.
//
// Turn generation, concurrent I/O, native floor, and native interaction are
// independent capabilities, but every combination needs the same machinery:
// a sidecar connection, mirrored trajectory, background reasoner, and a way to
// hand that reasoner's answer back. One runtime selected by ownership and
// capabilities is honest about that; one runtime per model species would drift.
package sidecarbinding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/sidecar"
)

// Spec is what a concrete binding declares about itself.
type Spec struct {
	// Name is a preset or deployment identity used in configuration and evidence.
	Name string
	// Ownership selects providers for this session. It is independent of
	// Capabilities: a model can have a native interaction head while an engine
	// controller owns interaction for one benchmark cell.
	Ownership binding.Ownership
	// Capabilities declares what the configured stack can do. The sidecar's
	// ready frame augments this with runtime-discovered transport features.
	Capabilities binding.StackCapabilities
}

// Config configures one sidecar-backed binding.
type Config struct {
	// Sidecar is how the model process is reached.
	Sidecar sidecar.Config
	// Instructions is the deployment's own instruction, passed to the model
	// and composed ahead of the slow phase instruction.
	Instructions string
	// Voice selects the model's voice, where it has more than one.
	Voice string
	// InputRate is the sample rate the engine sends. Zero selects 24 kHz.
	InputRate int

	// Slow is the background reasoner. It is what this binding adds to a model
	// that already speaks, so it is required.
	Slow          continuation.Provider
	SlowMaxTokens int
	// ModelCapabilities augments the preset's minimum capability declaration.
	// It is how a deployment describes a hybrid model without adding another
	// binding package or teaching this runtime a new species name.
	ModelCapabilities binding.StackCapabilities
	// Tools are deployment-owned actions installed beside client declarations.
	// FastComputerUse grants the sidecar only the bounded computer.* subset;
	// every call still crosses the engine authorization and execution boundary.
	Tools           []action.ToolSpec
	FastComputerUse bool
	ConfirmPolicy   action.PolicyDecision
	ActionAudit     func(action.Record)

	// Policies overrides the interaction policy set.
	Policies interaction.Policies
	// InteractionPerception is an optional policy-only streaming recogniser.
	// Its transcript is evidence for an engine-owned interaction model; the
	// voice model still receives raw audio and still produces speech directly,
	// so configuring it does not turn the foreground into an ASR/LLM/TTS
	// cascade.
	InteractionPerception func() (v1.PerceptionProvider, error)
	// InteractionPerceptionDescriptor identifies the policy-only evidence path
	// before the first utterance lazily opens it. It is evidence metadata, not a
	// second configuration source; the factory remains authoritative.
	InteractionPerceptionDescriptor v1.Descriptor
	// InteractionCadence is how often that recogniser is advanced. Zero uses
	// the audio observer's 200 ms reference cadence.
	InteractionCadence time.Duration
	// InteractionTimeout bounds one policy decision on the live path.
	InteractionTimeout time.Duration
	// Gate configures the engine's acoustic floor. It is unused when the model
	// owns the floor.
	Gate perception.GateConfig

	MaxPendingEvents int
	MediaRetention   session.MediaConfig
	Scheduler        clock.Scheduler
	// HandoffTimeout bounds how long a completed answer waits to be voiced.
	HandoffTimeout time.Duration
	// ClientToolTimeout bounds how long a client has to return results for
	// the tools it executes. Zero selects the default; negative disables the
	// deadline, which only a harness driving results by hand should do.
	ClientToolTimeout time.Duration
	Logf              func(string, ...any)
}

// Binding is a sidecar-backed voice stack.
type Binding struct {
	spec   Spec
	config Config
}

// New validates a declaration and its configuration.
func New(spec Spec, config Config) (*Binding, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return nil, errors.New("a binding requires a name")
	}
	spec.Ownership = spec.Ownership.Effective()
	if err := spec.Ownership.Validate(); err != nil {
		return nil, fmt.Errorf("binding %q: %w", spec.Name, err)
	}
	if spec.Ownership.Perception != binding.OwnerModel ||
		spec.Ownership.FastCognition != binding.OwnerModel ||
		spec.Ownership.SlowCognition != binding.OwnerEngine ||
		spec.Ownership.Action != binding.OwnerModel {
		return nil, fmt.Errorf("binding %q is sidecar-backed and requires model perception, fast cognition, and action with engine slow cognition", spec.Name)
	}
	stack := spec.Capabilities.Merge(config.ModelCapabilities)
	if !stack.AudioInput || !stack.AudioOutput {
		return nil, fmt.Errorf("binding %q requires sidecar audio input and output capabilities", spec.Name)
	}
	if spec.Ownership.Floor == binding.OwnerModel && !stack.NativeFloor {
		return nil, fmt.Errorf("binding %q gives the floor to a model that declares no native-floor capability", spec.Name)
	}
	if spec.Ownership.Interaction == binding.OwnerModel && !stack.NativeInteraction {
		return nil, fmt.Errorf("binding %q gives interaction to a model that declares no native-interaction capability", spec.Name)
	}
	if len(config.Sidecar.Command) == 0 && strings.TrimSpace(config.Sidecar.Address) == "" {
		return nil, fmt.Errorf("binding %q needs a sidecar command or address", spec.Name)
	}
	if config.Slow == nil {
		return nil, fmt.Errorf("binding %q requires a slow continuation provider: it is what the engine adds", spec.Name)
	}
	if config.InputRate <= 0 {
		config.InputRate = 24_000
	}
	if config.SlowMaxTokens <= 0 {
		config.SlowMaxTokens = 2048
	}
	if config.MaxPendingEvents <= 0 {
		config.MaxPendingEvents = 128
	}
	if config.HandoffTimeout <= 0 {
		config.HandoffTimeout = 30 * time.Second
	}
	if config.InteractionTimeout <= 0 {
		config.InteractionTimeout = 150 * time.Millisecond
	}
	if config.Scheduler == nil {
		config.Scheduler = clock.NewSystem()
	}
	if config.Gate.SilenceDurationMS == 0 {
		config.Gate = perception.DefaultGateConfig()
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
	if config.Policies.Validate() != nil {
		config.Policies = DefaultPolicies(spec)
	}
	if config.Policies.Interaction != nil {
		if spec.Ownership.Interaction != binding.OwnerEngine {
			return nil, fmt.Errorf("binding %q configured an engine interaction model while interaction is owned by %s", spec.Name, spec.Ownership.Interaction)
		}
		if config.InteractionPerception == nil {
			return nil, fmt.Errorf("binding %q needs interaction perception to run an engine interaction model over live audio", spec.Name)
		}
	}
	return &Binding{spec: spec, config: config}, nil
}

// ExternalInteractionPolicies adapts the engine's general policy assembly to
// a sidecar voice. The sidecar is the only fast provider, so its event-loop
// rollout must remain slow-only; this copies only the policies that constitute
// the external interaction controller.
func ExternalInteractionPolicies(spec Spec, policies interaction.Policies) interaction.Policies {
	selected := DefaultPolicies(spec)
	selected.Interaction = policies.Interaction
	selected.Extraction = policies.Extraction
	selected.ShadowInteraction = policies.ShadowInteraction
	return selected
}

// DefaultPolicies is the policy set a sidecar-backed binding runs by default.
//
// The model is the fast provider, so the engine's rollout runs slow and hands
// the answer back rather than producing a second voice. The floor policy
// follows the declaration rather than the other way round, which is what keeps
// "who owns the floor" a single answer.
func DefaultPolicies(spec Spec) interaction.Policies {
	policies := interaction.Defaults()
	policies.Rollout = interaction.NewEndpointedSlowOnlyRollout(interaction.RolloutOptions{})
	policies.Trigger = interaction.NewEndpointTrigger()
	policies.Preparation = interaction.NewEndpointPreparation()
	if spec.Ownership.Floor == binding.OwnerModel {
		policies.Floor = interaction.NewModelFloor(spec.Name)
		policies.Deferral = interaction.AlwaysRun{}
	}
	if spec.Ownership.Interaction == binding.OwnerModel {
		// Barge-in is an interaction act, not an endpoint detector. An engine
		// floor may establish the boundary while a native interaction policy
		// still decides whether overlap should cancel or be spoken through.
		policies.BargeIn = interaction.NewNeverBargeIn()
	}
	return policies
}

// Name is the registry identifier.
func (bind *Binding) Name() string { return bind.spec.Name }

// Ownership declares what the model provides and what the engine supplies.
func (bind *Binding) Ownership() binding.Ownership {
	return bind.spec.Ownership
}

// Capabilities reports what a session supports. Video and computer use depend
// on the model, and the sidecar protocol has no way to declare them yet, so
// they are reported off rather than guessed at.
func (bind *Binding) Capabilities() binding.Capabilities {
	// The model may have more than one voice and takes the session's choice,
	// so unlike a synthesiser configured once at startup, this one is
	// selectable.
	stack := bind.spec.Capabilities.Merge(bind.config.ModelCapabilities)
	observers := []string{"audio"}
	if stack.VisualInput {
		observers = append(observers, "video")
	}
	return binding.Capabilities{
		Video: stack.VisualInput, ComputerUse: bind.config.FastComputerUse,
		Observations: true, FastSlow: true, Observers: observers,
		Voice: binding.VoiceControl{Selectable: true, InForce: bind.config.Voice},
		// Only the engine can hand over a floor it holds. When the selected
		// floor owner is the model, the client cannot take it; saying otherwise
		// would be the engine promising on the model's behalf. Concurrent I/O
		// is an independent capability.
		ManualTurns: bind.spec.Ownership.Floor == binding.OwnerEngine,
		Stack:       stack,
	}
}

// Start opens a sidecar session.
func (bind *Binding) Start(ctx context.Context, options binding.Options) (binding.Runtime, error) {
	return newRuntime(ctx, bind, options)
}

var _ binding.Binding = (*Binding)(nil)
