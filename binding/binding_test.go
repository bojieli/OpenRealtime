package binding_test

import (
	"context"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/perception"
)

type stubBinding struct {
	name      string
	ownership binding.Ownership
}

func (bind stubBinding) Name() string                       { return bind.name }
func (bind stubBinding) Ownership() binding.Ownership       { return bind.ownership }
func (bind stubBinding) Capabilities() binding.Capabilities { return binding.Capabilities{} }

func (bind stubBinding) Start(context.Context, binding.Options) (binding.Runtime, error) {
	return nil, nil
}

func engineOwned() binding.Ownership {
	return binding.Ownership{
		Perception: binding.OwnerEngine, FastCognition: binding.OwnerEngine,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerEngine,
		Floor: binding.OwnerEngine,
	}
}

// The slow column never varies. It is the whole differentiator, and a binding
// that delegated it would be a binding this project adds nothing to.
func TestSlowCognitionIsAlwaysTheEngines(t *testing.T) {
	for _, owner := range []binding.Owner{binding.OwnerModel, binding.OwnerRemote} {
		ownership := engineOwned()
		ownership.SlowCognition = owner
		err := ownership.Validate()
		if err == nil {
			t.Fatalf("slow cognition owned by %q must be refused", owner)
		}
		if !strings.Contains(err.Error(), "slow cognition") {
			t.Fatalf("the refusal must say what it is about: %v", err)
		}
	}
}

func TestEveryOwnershipColumnMustNameARealOwner(t *testing.T) {
	ownership := engineOwned()
	ownership.Floor = "somebody"
	if err := ownership.Validate(); err == nil {
		t.Fatal("an unknown owner must be refused rather than treated as the engine")
	}
	if err := engineOwned().Validate(); err != nil {
		t.Fatalf("the cascade's own declaration must validate: %v", err)
	}
}

// The registry is the stable extension surface from v1.0: a deployment names a
// binding in configuration and a third party registers its own.
func TestRegistryRefusesWhatCannotBeNamedOrRun(t *testing.T) {
	registry := binding.NewRegistry()
	if err := registry.Register(nil); err == nil {
		t.Fatal("a nil binding must be refused")
	}
	if err := registry.Register(stubBinding{name: "  ", ownership: engineOwned()}); err == nil {
		t.Fatal("a binding with no name must be refused")
	}
	broken := engineOwned()
	broken.SlowCognition = binding.OwnerModel
	if err := registry.Register(stubBinding{name: "broken", ownership: broken}); err == nil {
		t.Fatal("a binding whose declaration contradicts the architecture must be refused at registration")
	}

	if err := registry.Register(stubBinding{name: "cascade", ownership: engineOwned()}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := registry.Register(stubBinding{name: "cascade", ownership: engineOwned()}); err == nil {
		t.Fatal("a duplicate name must be refused: a deployment names a binding and gets one")
	}
	if _, err := registry.Lookup("cascade"); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	_, err := registry.Lookup("omni")
	if err == nil {
		t.Fatal("an unknown binding must be refused")
	}
	if !strings.Contains(err.Error(), "cascade") {
		t.Fatalf("the refusal must say what is available: %v", err)
	}
	if names := registry.Names(); len(names) != 1 || names[0] != "cascade" {
		t.Fatalf("unexpected registry contents %v", names)
	}
}

// A binding must not be mutable through the settings value a client handed it.
func TestCloneSettingsIsADeepCopy(t *testing.T) {
	original := binding.Settings{
		Modalities: []string{"audio"}, Observers: []string{"audio", "video"},
		Gate: perception.DefaultGateConfig(),
	}
	copied := binding.CloneSettings(original)
	copied.Modalities[0] = "text"
	copied.Observers[0] = "lidar"
	if original.Modalities[0] != "audio" || original.Observers[0] != "audio" {
		t.Fatalf("the caller's value was mutated: %+v", original)
	}
}
