// Package omni provides presets for a speech-to-speech model that generates
// turns.
//
// The default preset selects engine interaction and floor ownership. That is
// a selection, not a claim that every Omni-labelled model has no native
// interaction capability. The engine supplies everything about *when* - and
// keeps the floor, deliberately, because voice activity detection
// mis-endpoints on spelled identifiers and digit strings, which is exactly
// what a tool-using voice agent depends on getting right.
//
// That is a measurable claim rather than an assertion, and it is one of the
// clearer places the runtime adds value to a model that already does the rest.
package omni

import (
	"fmt"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/sidecarbinding"
)

// Config is the sidecar-backed configuration.
type Config = sidecarbinding.Config

// New creates the binding.
func New(config Config) (*sidecarbinding.Binding, error) {
	return sidecarbinding.New(sidecarbinding.Spec{
		Name: "omni",
		Ownership: binding.Ownership{
			Perception: binding.OwnerModel, FastCognition: binding.OwnerModel,
			SlowCognition: binding.OwnerEngine, Action: binding.OwnerModel,
			Interaction: binding.OwnerEngine, Floor: binding.OwnerEngine,
		},
		Capabilities: binding.StackCapabilities{
			AudioInput: true, AudioOutput: true, TurnGeneration: true,
		},
	}, config)
}

// NewWithModelFloor creates the binding with the model's own endpointing,
// which is the control condition for factor F5.
func NewWithModelFloor(config Config) (*sidecarbinding.Binding, error) {
	return sidecarbinding.New(sidecarbinding.Spec{
		Name: "omni",
		Ownership: binding.Ownership{
			Perception: binding.OwnerModel, FastCognition: binding.OwnerModel,
			SlowCognition: binding.OwnerEngine, Action: binding.OwnerModel,
			Interaction: binding.OwnerEngine, Floor: binding.OwnerModel,
		},
		Capabilities: binding.StackCapabilities{
			AudioInput: true, AudioOutput: true, TurnGeneration: true, NativeFloor: true,
		},
	}, config)
}

// NewWithTextPolicy composes a speech-to-speech turn generator with an
// engine-owned interaction model whose evidence comes from a policy-only
// recogniser. It is a named benchmark preset over capabilities, not a new
// model species: callers needing another combination can construct a
// sidecarbinding.Spec directly.
func NewWithTextPolicy(config Config) (*sidecarbinding.Binding, error) {
	if config.Policies.Interaction == nil {
		return nil, fmt.Errorf("omni+text-policy requires an interaction model")
	}
	if config.InteractionPerception == nil {
		return nil, fmt.Errorf("omni+text-policy requires policy transcript perception")
	}
	spec := sidecarbinding.Spec{
		Name: "omni+text-policy",
		Ownership: binding.Ownership{
			Perception: binding.OwnerModel, FastCognition: binding.OwnerModel,
			SlowCognition: binding.OwnerEngine, Action: binding.OwnerModel,
			Interaction: binding.OwnerEngine, Floor: binding.OwnerEngine,
		},
		Capabilities: binding.StackCapabilities{
			AudioInput: true, AudioOutput: true, TurnGeneration: true,
			Transcription: true,
		},
	}
	// Preserve the sidecar runtime's slow-only rollout and replace only the
	// interaction policies the caller explicitly composed. Keeping the
	// cascade default fast step here would try to run a second voice.
	config.Policies = sidecarbinding.ExternalInteractionPolicies(spec, config.Policies)
	return sidecarbinding.New(spec, config)
}
