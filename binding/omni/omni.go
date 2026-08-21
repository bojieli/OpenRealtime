// Package omni binds a speech-to-speech model that generates turns.
//
// An Omni model does perception and generation itself but has no interaction
// capability inside it: it is a turn-based generator handed a turn by an
// external detector. So the engine supplies everything about *when* - and
// keeps the floor, deliberately, because voice activity detection
// mis-endpoints on spelled identifiers and digit strings, which is exactly
// what a tool-using voice agent depends on getting right.
//
// That is a measurable claim rather than an assertion, and it is one of the
// clearer places the runtime adds value to a model that already does the rest.
package omni

import (
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/sidecarbinding"
)

// Config is the sidecar-backed configuration.
type Config = sidecarbinding.Config

// New creates the binding.
func New(config Config) (*sidecarbinding.Binding, error) {
	return sidecarbinding.New(sidecarbinding.Spec{
		Name: "omni", Floor: binding.OwnerEngine,
	}, config)
}

// NewWithModelFloor creates the binding with the model's own endpointing,
// which is the control condition for factor F5.
func NewWithModelFloor(config Config) (*sidecarbinding.Binding, error) {
	return sidecarbinding.New(sidecarbinding.Spec{
		Name: "omni", Floor: binding.OwnerModel,
	}, config)
}
