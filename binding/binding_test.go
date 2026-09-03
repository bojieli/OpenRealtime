package binding_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/action"
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
		Interaction: binding.OwnerEngine, Floor: binding.OwnerEngine,
	}
}

func TestInteractionAndFloorOwnershipAreIndependent(t *testing.T) {
	ownership := engineOwned()
	ownership.Interaction = binding.OwnerModel
	if err := ownership.Validate(); err != nil {
		t.Fatalf("a model interaction policy with an engine floor is composable: %v", err)
	}
	ownership.Interaction = binding.OwnerEngine
	ownership.Floor = binding.OwnerModel
	if err := ownership.Validate(); err != nil {
		t.Fatalf("an engine interaction policy with a model floor is composable: %v", err)
	}
}

func TestLegacyOwnershipCouplesInteractionToItsFloor(t *testing.T) {
	ownership := engineOwned()
	ownership.Interaction = ""
	if err := ownership.Validate(); err != nil {
		t.Fatalf("a pre-interaction-column binding must remain loadable: %v", err)
	}
	if got := ownership.Effective().Interaction; got != binding.OwnerEngine {
		t.Fatalf("legacy effective interaction owner = %q", got)
	}
}

func TestStackCapabilitiesComposeByUnion(t *testing.T) {
	turnModel := binding.StackCapabilities{AudioInput: true, VisualInput: true, TurnGeneration: true}
	interactionHead := binding.StackCapabilities{NativeInteraction: true, ConcurrentIO: true}
	combined := turnModel.Merge(interactionHead)
	if !combined.AudioInput || !combined.VisualInput || !combined.TurnGeneration || !combined.NativeInteraction || !combined.ConcurrentIO {
		t.Fatalf("capability composition lost a feature: %+v", combined)
	}
}

func TestInteractionEvidenceCapabilitiesComposeIndependently(t *testing.T) {
	text := binding.InteractionEvidenceCapabilities{
		Transcript: true, ConversationState: true,
	}
	timing := binding.InteractionEvidenceCapabilities{
		AcousticActivity: true, SilenceClock: true,
	}
	combined := text.Merge(timing)
	if !combined.Transcript || !combined.ConversationState ||
		!combined.AcousticActivity || !combined.SilenceClock {
		t.Fatalf("evidence composition lost a channel: %+v", combined)
	}
	if combined.DirectVisualInput || combined.NativeModelState {
		t.Fatalf("evidence composition invented a channel: %+v", combined)
	}
}

func TestInteractionControllersComposeWithoutImplicitArbitration(t *testing.T) {
	predicates := binding.InteractionControllers{Predicates: true}
	text := binding.InteractionControllers{TextPolicy: true}
	composed := predicates.Merge(text)
	if composed != (binding.InteractionControllers{Predicates: true, TextPolicy: true}) {
		t.Fatalf("controller composition lost a selector: %+v", composed)
	}
	if got := composed.Names(); len(got) != 2 || got[0] != "predicates" || got[1] != "text-policy" {
		t.Fatalf("controller names = %v", got)
	}
}

func TestSlowCognitionOwnershipIsAComposableAdapterChoice(t *testing.T) {
	for _, owner := range []binding.Owner{binding.OwnerModel, binding.OwnerRemote} {
		ownership := engineOwned()
		ownership.SlowCognition = owner
		if err := ownership.Validate(); err != nil {
			t.Fatalf("slow cognition owned by %q was treated as a kernel error: %v", owner, err)
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
	broken.SlowCognition = "somebody"
	if err := registry.Register(stubBinding{name: "broken", ownership: broken}); err == nil {
		t.Fatal("a binding with an unknown owner must be refused at registration")
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
		Tools: []action.ToolSpec{{
			Name: "track_order", Description: "track an order",
			Parameters: json.RawMessage(`{"type":"object"}`),
			ArgumentNormalizers: []action.ToolArgumentNormalizer{{
				Argument: "order_id", Normalizer: action.ToolParameterCompactASCIIAlphanumericV1,
			}},
		}},
	}
	copied := binding.CloneSettings(original)
	copied.Modalities[0] = "text"
	copied.Observers[0] = "lidar"
	copied.Tools[0].Parameters[0] = '['
	copied.Tools[0].ArgumentNormalizers[0].Argument = "mutated"
	if original.Modalities[0] != "audio" || original.Observers[0] != "audio" {
		t.Fatalf("the caller's value was mutated: %+v", original)
	}
	if got := string(original.Tools[0].Parameters); got != `{"type":"object"}` ||
		original.Tools[0].ArgumentNormalizers[0].Argument != "order_id" {
		t.Fatalf("the caller's tool declaration was mutated: %+v", original.Tools[0])
	}
}
