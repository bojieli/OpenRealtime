// Package duplex provides the native-interaction preset for a model that can
// listen and speak concurrently.
//
// The preset selects concurrent I/O, model interaction, and model floor. Those
// are independent capabilities and ownership choices rather than implications
// of one "duplex" species.
// What such a model does not have is a background reasoner, and it cannot: a
// single model has no second model and no shared log to put one on.
//
// The open question is narrower than "duplex support": where a slow
// continuation splices into a model that owns its own floor. Text injection is
// the interesting path - the answer becomes context and the model decides for
// itself when to say it. Explicit hand-off is the documented fallback, it
// certainly works, and it is why this binding ships regardless of how the
// research resolves.
package duplex

import (
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/sidecarbinding"
)

// Config is the sidecar-backed configuration.
type Config = sidecarbinding.Config

// New creates the binding.
func New(config Config) (*sidecarbinding.Binding, error) {
	return sidecarbinding.New(sidecarbinding.Spec{
		Name: "duplex",
		Ownership: binding.Ownership{
			Perception: binding.OwnerModel, FastCognition: binding.OwnerModel,
			SlowCognition: binding.OwnerEngine, Action: binding.OwnerModel,
			Interaction: binding.OwnerModel, Floor: binding.OwnerModel,
		},
		Capabilities: binding.StackCapabilities{
			AudioInput: true, AudioOutput: true, TurnGeneration: true,
			ConcurrentIO: true, NativeFloor: true, NativeInteraction: true,
		},
	}, config)
}

// NewWithEngineFloor creates the binding with the engine endpointing instead,
// which is the other level of factor F5 and the honest way to ask whether a
// duplex model's own floor is better than a measured one.
func NewWithEngineFloor(config Config) (*sidecarbinding.Binding, error) {
	return sidecarbinding.New(sidecarbinding.Spec{
		Name: "duplex",
		Ownership: binding.Ownership{
			Perception: binding.OwnerModel, FastCognition: binding.OwnerModel,
			SlowCognition: binding.OwnerEngine, Action: binding.OwnerModel,
			Interaction: binding.OwnerModel, Floor: binding.OwnerEngine,
		},
		Capabilities: binding.StackCapabilities{
			AudioInput: true, AudioOutput: true, TurnGeneration: true,
			ConcurrentIO: true, NativeFloor: true, NativeInteraction: true,
		},
	}, config)
}
