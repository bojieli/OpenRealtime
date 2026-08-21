package omni_test

import (
	"context"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/duplex"
	"github.com/bojieli/OpenRealtime/binding/omni"
	"github.com/bojieli/OpenRealtime/binding/sidecarbinding"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/sidecar"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// silentSlow is the background reasoner these bindings require. It is never
// invoked here: nothing in this file starts a session, because what is being
// checked is the declaration a binding makes before any session exists.
type silentSlow struct{}

func (silentSlow) Descriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "slow", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, ToolAuthority: continuation.ToolAuthorityExecute,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
}

func (silentSlow) Continue(
	context.Context, continuation.Request, continuation.Emit,
) (continuation.Completion, error) {
	return continuation.Completion{StopReason: "stop"}, nil
}

func config() sidecarbinding.Config {
	return sidecarbinding.Config{
		Sidecar: sidecar.Config{Address: "tcp:127.0.0.1:9"},
		Slow:    silentSlow{},
	}
}

// The ownership table is the plan's claim about these two bindings, and factor
// F5 is the claim that the engine keeping the floor for an Omni model is worth
// something. Both levels have to exist for that to be a question.
func TestOmniKeepsTheFloorInTheEngineByDefault(t *testing.T) {
	bind, err := omni.New(config())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ownership := bind.Ownership()
	if err := ownership.Validate(); err != nil {
		t.Fatalf("ownership: %v", err)
	}
	if ownership.Floor != binding.OwnerEngine {
		t.Fatalf("the engine keeps the floor for a turn-based generator, got %q", ownership.Floor)
	}
	if ownership.SlowCognition != binding.OwnerEngine {
		t.Fatalf("slow cognition is always the engine's, got %q", ownership.SlowCognition)
	}
	if ownership.FastCognition != binding.OwnerModel || ownership.Action != binding.OwnerModel {
		t.Fatalf("an Omni model owns its own voice: %+v", ownership)
	}

	control, err := omni.NewWithModelFloor(config())
	if err != nil {
		t.Fatalf("new with model floor: %v", err)
	}
	if control.Ownership().Floor != binding.OwnerModel {
		t.Fatal("the control condition for factor F5 must exist and differ")
	}
}

// A full-duplex model owns its floor - that is what full-duplex means - and
// the engine still supplies the background reasoner it cannot have.
func TestDuplexOwnsItsFloorAndStillBorrowsTheReasoner(t *testing.T) {
	bind, err := duplex.New(config())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ownership := bind.Ownership()
	if err := ownership.Validate(); err != nil {
		t.Fatalf("ownership: %v", err)
	}
	if ownership.Floor != binding.OwnerModel {
		t.Fatalf("a duplex model owns its own floor, got %q", ownership.Floor)
	}
	if ownership.SlowCognition != binding.OwnerEngine {
		t.Fatalf("slow cognition is always the engine's, got %q", ownership.SlowCognition)
	}
	if !bind.Capabilities().FastSlow {
		t.Fatal("the background reasoner is the whole thing this binding adds")
	}

	measured, err := duplex.NewWithEngineFloor(config())
	if err != nil {
		t.Fatalf("new with engine floor: %v", err)
	}
	if measured.Ownership().Floor != binding.OwnerEngine {
		t.Fatal("the other level of factor F5 must exist and differ")
	}
}

func TestTheTwoBindingsAreDistinguishableInAReport(t *testing.T) {
	omniBinding, err := omni.New(config())
	if err != nil {
		t.Fatalf("new omni: %v", err)
	}
	duplexBinding, err := duplex.New(config())
	if err != nil {
		t.Fatalf("new duplex: %v", err)
	}
	if omniBinding.Name() == duplexBinding.Name() {
		t.Fatalf("two bindings cannot share one name: %q", omniBinding.Name())
	}
	registry := binding.NewRegistry()
	for _, bind := range []binding.Binding{omniBinding, duplexBinding} {
		if err := registry.Register(bind); err != nil {
			t.Fatalf("register %s: %v", bind.Name(), err)
		}
	}
}
