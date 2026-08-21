// Package cascade is the reference binding: the engine owns everything.
//
// A cascade has no interaction capability inside any of its models. The
// recogniser knows nothing about turn-taking, the language model knows nothing
// about the conversation's timing, and the synthesiser knows nothing at all -
// so every bit of responsiveness the system exhibits is manufactured in the
// interaction plane. That makes this binding the honest test of whether the
// control plane works, and it is why it is the default: it is fully local, it
// needs no third-party account, and it is the right first impression for an
// open implementation.
package cascade

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
)

// ObservationPolicy controls which perception revisions may enter the
// canonical trajectory and therefore start authoritative cognition.
//
// It is independent from preparation: speculative work has no speech sink and
// no tool authority, while every observation admitted here may eventually
// produce both.
type ObservationPolicy string

const (
	// ObservationEndpointOnly admits only the terminal revision.
	ObservationEndpointOnly ObservationPolicy = "endpoint-only"
	// ObservationStablePartial also admits a changed, non-empty stable prefix
	// before the endpoint. Eligibility comes only from the recogniser's typed
	// stable/unstable boundary, never from inspecting the words.
	ObservationStablePartial ObservationPolicy = "stable-partial"
)

// ParseObservationPolicy validates a configured level.
func ParseObservationPolicy(value string) (ObservationPolicy, error) {
	switch policy := ObservationPolicy(strings.ToLower(strings.TrimSpace(value))); policy {
	case "", ObservationEndpointOnly:
		return ObservationEndpointOnly, nil
	case ObservationStablePartial:
		return policy, nil
	default:
		return "", errors.New("observation policy must be endpoint-only or stable-partial")
	}
}

// Config is the cascade's component set and its defaults.
type Config struct {
	// Perception creates one streaming recogniser per utterance, so
	// recogniser state cannot leak between turns.
	Perception func() (v1.PerceptionProvider, error)
	// ASRCadence is how often the recogniser is advanced.
	ASRCadence time.Duration

	// Fast is the voice; Slow is the background reasoner. Slow must be
	// configured silent: the cognition engine refuses the pair otherwise.
	Fast          continuation.Provider
	Slow          continuation.Provider
	FastMaxTokens int
	SlowMaxTokens int

	// Speech synthesises what the fast provider says.
	Speech v1.StreamingSpeechProvider
	// FrameDuration is the paced wire frame size.
	FrameDuration time.Duration

	// Policies is the interaction policy set. A zero value selects the
	// shipped defaults.
	Policies          interaction.Policies
	ObservationPolicy ObservationPolicy

	// Observers extends the default set with one factory per additional
	// observer. The audio observer is always present. Factories rather than
	// instances because an observer holds per-session state: two sessions
	// sharing one video observer would each see the other's screen as
	// unchanged.
	Observers []perception.Factory

	// Tools are server-side tools every session declares, on top of whatever a
	// client declares for itself. Computer use arrives this way: a client
	// should not have to know the coordinate space of a browser the server is
	// driving.
	Tools []action.ToolSpec

	MaxPendingEvents int
	MediaRetention   session.MediaConfig
	Scheduler        clock.Scheduler
	// ClientToolTimeout bounds how long a client has to return results for
	// the tools it executes. Zero selects the default; negative disables the
	// deadline, which only a harness driving results by hand should do.
	ClientToolTimeout time.Duration
	// AgentInstruction is the deployment's own instruction.
	AgentInstruction string
}

// Binding is the cascade binding.
type Binding struct {
	config Config
}

// New validates the component set and creates the binding.
func New(config Config) (*Binding, error) {
	if config.Perception == nil {
		return nil, errors.New("cascade requires a perception provider factory")
	}
	if config.Fast == nil || config.Slow == nil {
		return nil, errors.New("cascade requires fast and slow continuation providers")
	}
	if config.Speech == nil {
		return nil, errors.New("cascade requires a streaming speech provider")
	}
	policy, err := ParseObservationPolicy(string(config.ObservationPolicy))
	if err != nil {
		return nil, err
	}
	config.ObservationPolicy = policy
	if config.ASRCadence <= 0 {
		config.ASRCadence = 200 * time.Millisecond
	}
	if config.FrameDuration <= 0 {
		config.FrameDuration = 100 * time.Millisecond
	}
	if config.MaxPendingEvents <= 0 {
		config.MaxPendingEvents = 128
	}
	if config.Scheduler == nil {
		config.Scheduler = clock.NewSystem()
	}
	if config.Policies.Validate() != nil {
		config.Policies = interaction.Defaults()
	}
	if err := validateObservationAgainstDeferral(config.ObservationPolicy, config.Policies.Deferral); err != nil {
		return nil, err
	}
	for index, factory := range config.Observers {
		if err := factory.Validate(); err != nil {
			return nil, fmt.Errorf("observer %d: %w", index, err)
		}
	}
	return &Binding{config: config}, nil
}

// validateObservationAgainstDeferral refuses a pair of policies that cancel
// each other out.
//
// The stable-partial policy exists to let the agent start answering before the
// user has finished, and that is the whole of its value. A deferral policy
// that waits for the user to stop speaking holds every one of those partials
// until the endpoint, where the final observation arrives anyway - so the
// configuration costs an extra committed observation and a supersession and
// buys exactly nothing.
//
// Refusing is better than quietly picking one. Both policies are measured
// factors, and a runtime that silently overrode one of them would report a
// configuration it was not running - which is the failure this project keeps
// pointing at everywhere else.
func validateObservationAgainstDeferral(observation ObservationPolicy, deferral interaction.Deferral) error {
	if observation != ObservationStablePartial || deferral == nil {
		return nil
	}
	if !slices.Contains(deferral.Conditions(), session.UserSpeechStopped) {
		return nil
	}
	return fmt.Errorf(
		"the %s observation policy admits partials before the endpoint, but the %q deferral policy defers "+
			"every run until the user stops speaking, so nothing would ever act on one: either configure a "+
			"deferral that allows running while the user is speaking, or use the %s observation policy",
		ObservationStablePartial, deferral.Name(), ObservationEndpointOnly)
}

// Name is the binding's stable identifier.
func (bind *Binding) Name() string { return "cascade" }

// Ownership declares that the engine provides everything.
func (bind *Binding) Ownership() binding.Ownership {
	return binding.Ownership{
		Perception: binding.OwnerEngine, FastCognition: binding.OwnerEngine,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerEngine,
		Floor: binding.OwnerEngine,
	}
}

// Capabilities reports what a cascade session supports.
//
// Video is reported from the configured observer set rather than from a build
// flag, so a deployment that did not configure a video observer negotiates
// honestly instead of accepting frames it will discard.
func (bind *Binding) Capabilities() binding.Capabilities {
	video := false
	for _, factory := range bind.config.Observers {
		if factory.Kind == perception.FrameImage {
			video = true
		}
	}
	return binding.Capabilities{
		Video: video, ComputerUse: true, Observations: true, FastSlow: true,
	}
}

// Start creates a session runtime.
func (bind *Binding) Start(ctx context.Context, options binding.Options) (binding.Runtime, error) {
	return newRuntime(ctx, bind, options)
}

var _ binding.Binding = (*Binding)(nil)
