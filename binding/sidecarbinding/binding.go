// Package sidecarbinding is the runtime shared by every binding whose model
// lives behind a process boundary.
//
// Omni and full-duplex models differ in what they own - an Omni model is a
// turn-based generator handed a turn by an external detector, while a duplex
// model owns its own floor - but they need the same machinery: a sidecar
// connection, a mirrored trajectory, a background reasoner, and a way to hand
// that reasoner's answer back. One runtime with two declarations over it is
// honest about that; two runtimes would drift.
package sidecarbinding

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	// Name is the registry identifier: "omni", "duplex", or a third party's.
	Name string
	// Floor says who decides when a turn ends.
	//
	// The engine keeps it for Omni deliberately: an Omni model assumes
	// turn-taking and leans on voice activity detection, which mis-endpoints
	// on spelled identifiers and digit strings - exactly the inputs a
	// tool-using voice agent depends on getting right. A full-duplex model
	// genuinely owns its floor and is given it.
	Floor binding.Owner
	// FullDuplex means the model listens and speaks at once, so the engine
	// must not gate its audio or ask it to take turns.
	FullDuplex bool
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

	// Policies overrides the interaction policy set.
	Policies interaction.Policies
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
	switch spec.Floor {
	case binding.OwnerEngine, binding.OwnerModel:
	default:
		return nil, fmt.Errorf("binding %q must give the floor to the engine or the model", spec.Name)
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
	return &Binding{spec: spec, config: config}, nil
}

// DefaultPolicies is the policy set a sidecar-backed binding runs by default.
//
// The model is the fast provider, so the engine's rollout runs slow and hands
// the answer back rather than producing a second voice. The floor policy
// follows the declaration rather than the other way round, which is what keeps
// "who owns the floor" a single answer.
func DefaultPolicies(spec Spec) interaction.Policies {
	policies := interaction.Defaults()
	policies.Rollout = interaction.NewEndpointedSlowOnlyRollout(interaction.RolloutOptions{
		VoiceSlowOutput: true,
	})
	policies.Trigger = interaction.NewEndpointTrigger()
	policies.Preparation = interaction.NewEndpointPreparation()
	if spec.Floor == binding.OwnerModel {
		policies.Floor = interaction.NewModelFloor(spec.Name)
		// A model that owns its floor handles overlap itself; the engine
		// cancelling its speech would be the engine overruling the thing it
		// delegated to.
		policies.BargeIn = interaction.NewNeverBargeIn()
		policies.Deferral = interaction.AlwaysRun{}
	}
	return policies
}

// Name is the registry identifier.
func (bind *Binding) Name() string { return bind.spec.Name }

// Ownership declares what the model provides and what the engine supplies.
func (bind *Binding) Ownership() binding.Ownership {
	return binding.Ownership{
		Perception: binding.OwnerModel, FastCognition: binding.OwnerModel,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerModel,
		Floor: bind.spec.Floor,
	}
}

// Capabilities reports what a session supports. Video and computer use depend
// on the model, and the sidecar protocol has no way to declare them yet, so
// they are reported off rather than guessed at.
func (bind *Binding) Capabilities() binding.Capabilities {
	// The model may have more than one voice and takes the session's choice,
	// so unlike a synthesiser configured once at startup, this one is
	// selectable.
	return binding.Capabilities{
		Observations: true, FastSlow: true,
		Voice: binding.VoiceControl{Selectable: true, InForce: bind.config.Voice},
		// Only the engine can hand over a floor it holds. A full-duplex model
		// owns its own, and that is the binding's whole ownership claim - the
		// client cannot take it, and saying otherwise would be the engine
		// promising on the model's behalf.
		ManualTurns: bind.spec.Floor == binding.OwnerEngine,
	}
}

// Start opens a sidecar session.
func (bind *Binding) Start(ctx context.Context, options binding.Options) (binding.Runtime, error) {
	return newRuntime(ctx, bind, options)
}

var _ binding.Binding = (*Binding)(nil)
