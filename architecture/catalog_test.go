package architecture_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/binding"
)

func TestTheEmbeddedCatalogIsACompleteLineageGraph(t *testing.T) {
	catalog := architecture.Default()
	if err := catalog.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Architectures) < 8 {
		t.Fatalf("the project catalog lost an architecture: %d", len(catalog.Architectures))
	}
	definition, err := catalog.Resolve("omni.external-policy@4")
	if err != nil {
		t.Fatal(err)
	}
	if !definition.Requires.NativeInteraction || !definition.Requires.InteractionActs ||
		definition.Ownership.Interaction != binding.OwnerEngine {
		t.Fatalf("the controlled hybrid architecture is not representable: %+v", definition)
	}
	identity := definition.Identity()
	if _, err := catalog.LookupIdentity(identity); err != nil {
		t.Fatalf("identity did not resolve: %v", err)
	}
	identity.Fingerprint = "sha256:edited-in-place"
	if _, err := catalog.LookupIdentity(identity); err == nil {
		t.Fatal("an in-place edit retained an immutable architecture identity")
	}
}

func TestSameForegroundTextAndNativeSelectionsRequireTheSameCapabilities(t *testing.T) {
	catalog := architecture.Default()
	textPolicy, err := catalog.Resolve("omni.external-policy@4")
	if err != nil {
		t.Fatal(err)
	}
	native, err := catalog.Resolve("omni.native-policy@3")
	if err != nil {
		t.Fatal(err)
	}
	if textPolicy.Requires != native.Requires {
		t.Fatalf("the T/N blueprints confound capability availability:\nT=%+v\nN=%+v", textPolicy.Requires, native.Requires)
	}
	left, right := textPolicy.Ownership.Effective(), native.Ownership.Effective()
	left.Interaction, right.Interaction = "", ""
	if left != right {
		t.Fatalf("the T/N blueprints change non-interaction ownership:\nT=%+v\nN=%+v", left, right)
	}
}

func TestRuntimeValidationAllowsUnselectedCapabilitiesButNotMissingOnes(t *testing.T) {
	definition, err := architecture.Default().Resolve("omni.external-policy@1")
	if err != nil {
		t.Fatal(err)
	}
	available := definition.Requires.Merge(binding.StackCapabilities{
		NativeInteraction: true, ConcurrentIO: true,
	})
	if err := definition.ValidateRuntime(definition.Ownership, available); err != nil {
		t.Fatalf("extra native capabilities were treated as another species: %v", err)
	}
	available.InteractionActs = false
	if err := definition.ValidateRuntime(definition.Ownership, available); err == nil ||
		!strings.Contains(err.Error(), "interaction-acts") {
		t.Fatalf("a missing requirement was accepted: %v", err)
	}
}

func TestDefinitionValidationDoesNotRequireAudioOrEngineOwnedSlowCognition(t *testing.T) {
	definition, err := architecture.Default().Resolve("cascade.text-policy@1")
	if err != nil {
		t.Fatal(err)
	}
	definition.ID = "text-only.remote-slow"
	definition.Revision = 1
	definition.DerivedFrom = nil
	definition.Ownership.SlowCognition = binding.OwnerRemote
	definition.Requires.AudioInput = false
	definition.Requires.AudioOutput = false
	if err := definition.Validate(); err != nil {
		t.Fatalf("audio-free definition with remote slow cognition hit a generic restriction: %v", err)
	}
	withoutGeneration := definition
	withoutGeneration.Requires.TurnGeneration = false
	if err := withoutGeneration.Validate(); err == nil || !strings.Contains(err.Error(), "turn generation") {
		t.Fatalf("removing the retained conversational capability was accepted: %v", err)
	}
}

func TestEvidenceCapabilitiesComposeWithoutBecomingModelSpecies(t *testing.T) {
	base := binding.InteractionEvidenceCapabilities{
		Transcript: true, AcousticActivity: true, SilenceClock: true,
	}
	vision := binding.InteractionEvidenceCapabilities{DirectVisualInput: true}
	composed := base.Merge(vision)
	if !composed.Transcript || !composed.AcousticActivity ||
		!composed.SilenceClock || !composed.DirectVisualInput {
		t.Fatalf("evidence composition lost a channel: %+v", composed)
	}
	if base.DirectVisualInput {
		t.Fatal("evidence composition mutated its input")
	}
	missing := base.Missing(composed)
	if len(missing) != 1 || missing[0] != "direct-visual-input" {
		t.Fatalf("missing evidence channels = %v", missing)
	}
}

func TestLegacyDefinitionsRemainResolvableAcrossEvidenceAndControllerAttestation(t *testing.T) {
	catalog := architecture.Default()
	legacy, err := catalog.Resolve("omni.external-policy@2")
	if err != nil {
		t.Fatal(err)
	}
	current, err := catalog.Resolve("omni.external-policy@3")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Interaction.EvidenceCapabilities != nil {
		t.Fatal("editing a legacy definition would change its immutable fingerprint")
	}
	if current.Interaction.EvidenceCapabilities == nil ||
		!current.Interaction.EvidenceCapabilities.Transcript ||
		current.Interaction.EvidenceCapabilities.DirectVisualInput {
		t.Fatalf("current evidence boundary is not exact: %+v", current.Interaction)
	}
	if current.Interaction.Control != nil {
		t.Fatal("editing the exact-evidence revision would change its immutable fingerprint")
	}
	controllerCurrent, err := catalog.Resolve("omni.external-policy@4")
	if err != nil {
		t.Fatal(err)
	}
	if controllerCurrent.Interaction.Control == nil ||
		!controllerCurrent.Interaction.Control.Selectors.TextPolicy ||
		controllerCurrent.Interaction.Control.Arbitration != "single" {
		t.Fatalf("current controller boundary is not exact: %+v", controllerCurrent.Interaction.Control)
	}
}

func TestCatalogCanComposeEvidenceBranchesWithoutANewRuntimeSpecies(t *testing.T) {
	catalog := architecture.Default()
	visual, err := catalog.Resolve("cascade.text-policy-visual@1")
	if err != nil {
		t.Fatal(err)
	}
	speaker, err := catalog.Resolve("cascade.text-policy-speaker@1")
	if err != nil {
		t.Fatal(err)
	}
	combined, err := catalog.Resolve("cascade.text-policy-visual-speaker@1")
	if err != nil {
		t.Fatal(err)
	}
	want := visual.Interaction.EvidenceCapabilities.Merge(
		*speaker.Interaction.EvidenceCapabilities,
	)
	if combined.Interaction.EvidenceCapabilities == nil ||
		*combined.Interaction.EvidenceCapabilities != want {
		t.Fatalf("catalog composition differs from the capability union:\ncombined=%+v\nwant=%+v",
			combined.Interaction.EvidenceCapabilities, want)
	}
	for _, definition := range []architecture.Definition{visual, speaker, combined} {
		topology, err := definition.Topology()
		if err != nil || topology != architecture.TopologyComponents {
			t.Fatalf("evidence branch became a runtime species: %s topology=%q err=%v",
				definition.Ref(), topology, err)
		}
	}
}

func TestControllerCompositionIsIndependentFromEvidenceAndTopology(t *testing.T) {
	catalog := architecture.Default()
	pure, err := catalog.Resolve("cascade.text-policy-visual-speaker@2")
	if err != nil {
		t.Fatal(err)
	}
	composed, err := catalog.Resolve("cascade.composed-policy-visual-speaker@1")
	if err != nil {
		t.Fatal(err)
	}
	if pure.Interaction.EvidenceCapabilities == nil || composed.Interaction.EvidenceCapabilities == nil ||
		*pure.Interaction.EvidenceCapabilities != *composed.Interaction.EvidenceCapabilities {
		t.Fatal("the pure/composed controller pair changed its evidence treatment")
	}
	if pure.Requires != composed.Requires || pure.Ownership != composed.Ownership {
		t.Fatal("the pure/composed controller pair changed capabilities or ownership")
	}
	if pure.Interaction.Control == nil || composed.Interaction.Control == nil ||
		pure.Interaction.Control.Selectors.TextPolicy != true ||
		composed.Interaction.Control.Selectors != (binding.InteractionControllers{
			Predicates: true, TextPolicy: true,
		}) || composed.Interaction.Control.Arbitration != "predicate-floor" {
		t.Fatalf("controller composition is not exact: pure=%+v composed=%+v",
			pure.Interaction.Control, composed.Interaction.Control)
	}
	for _, definition := range []architecture.Definition{pure, composed} {
		topology, topologyErr := definition.Topology()
		if topologyErr != nil || topology != architecture.TopologyComponents {
			t.Fatalf("controller composition became a runtime species: %s %q %v",
				definition.Ref(), topology, topologyErr)
		}
	}
}

func TestComposedDirectVisualAddsOnlyTheDeclaredPixelChannel(t *testing.T) {
	catalog := architecture.Default()
	baseline, err := catalog.Resolve("cascade.composed-policy@1")
	if err != nil {
		t.Fatal(err)
	}
	direct, err := catalog.Resolve("cascade.composed-policy-direct-visual@1")
	if err != nil {
		t.Fatal(err)
	}
	if baseline.Interaction.EvidenceCapabilities == nil || direct.Interaction.EvidenceCapabilities == nil {
		t.Fatal("composed controller evidence vectors must be explicit")
	}
	want := *baseline.Interaction.EvidenceCapabilities
	want.DirectVisualInput = true
	if *direct.Interaction.EvidenceCapabilities != want {
		t.Fatalf("direct visual composition changed unrelated evidence: got=%+v want=%+v",
			*direct.Interaction.EvidenceCapabilities, want)
	}
	wantRequires := baseline.Requires
	wantRequires.VisualInput = true
	if direct.Interaction.Control == nil || baseline.Interaction.Control == nil ||
		*direct.Interaction.Control != *baseline.Interaction.Control ||
		direct.Ownership != baseline.Ownership || direct.Requires != wantRequires {
		t.Fatalf("direct visual evidence changed controller/topology: baseline=%+v direct=%+v", baseline, direct)
	}
}

func TestComposedDirectVisualSpeakerAddsOnlyTheDeclaredSpeakerChannel(t *testing.T) {
	catalog := architecture.Default()
	direct, err := catalog.Resolve("cascade.composed-policy-direct-visual@1")
	if err != nil {
		t.Fatal(err)
	}
	speaker, err := catalog.Resolve("cascade.composed-policy-direct-visual-speaker@1")
	if err != nil {
		t.Fatal(err)
	}
	if direct.Interaction.EvidenceCapabilities == nil || speaker.Interaction.EvidenceCapabilities == nil {
		t.Fatal("composed controller evidence vectors must be explicit")
	}
	want := *direct.Interaction.EvidenceCapabilities
	want.SpeakerIdentity = true
	if *speaker.Interaction.EvidenceCapabilities != want {
		t.Fatalf("speaker composition changed unrelated evidence: got=%+v want=%+v",
			*speaker.Interaction.EvidenceCapabilities, want)
	}
	if speaker.Interaction.Control == nil || direct.Interaction.Control == nil ||
		*speaker.Interaction.Control != *direct.Interaction.Control ||
		speaker.Ownership != direct.Ownership || speaker.Requires != direct.Requires {
		t.Fatalf("speaker evidence changed controller/topology: direct=%+v speaker=%+v", direct, speaker)
	}
}

func TestControllerCompositionRequiresOneExplicitArbiter(t *testing.T) {
	definition, err := architecture.Default().Resolve("cascade.composed-policy@1")
	if err != nil {
		t.Fatal(err)
	}
	broken := definition
	control := *definition.Interaction.Control
	control.Arbitration = "single"
	broken.Interaction.Control = &control
	if err := broken.Validate(); err == nil || !strings.Contains(err.Error(), "single-controller") {
		t.Fatalf("two selectors acquired implicit single arbitration: %v", err)
	}
	control.Arbitration = "last-response-wins"
	broken.Interaction.Control = &control
	if err := broken.Validate(); err == nil || !strings.Contains(err.Error(), "unknown interaction arbitration") {
		t.Fatalf("unknown scheduling-order arbitration was accepted: %v", err)
	}
}

func TestCatalogDecoderIsStrictAndRejectsBrokenEvolution(t *testing.T) {
	catalog := architecture.Default()
	payload, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := architecture.Decode(append(payload, []byte("\n{}")...)); err == nil {
		t.Fatal("a second catalog value was accepted")
	}

	broken := catalog
	broken.Architectures = append([]architecture.Definition(nil), catalog.Architectures...)
	for index := range broken.Architectures {
		if broken.Architectures[index].ID == "omni.external-policy" &&
			broken.Architectures[index].Revision == 2 {
			broken.Architectures[index].DerivedFrom = nil
		}
	}
	payload, err = json.Marshal(broken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := architecture.Decode(payload); err == nil || !strings.Contains(err.Error(), "without lineage") {
		t.Fatalf("a later revision silently replaced its lineage: %v", err)
	}
}

func TestArchitectureReferencesAreExact(t *testing.T) {
	if _, err := architecture.ParseRef("omni.external-policy"); err == nil {
		t.Fatal("an unpinned architecture reference was accepted")
	}
	ref, err := architecture.ParseRef("omni.external-policy@2")
	if err != nil || ref.ID != "omni.external-policy" || ref.Revision != 2 {
		t.Fatalf("exact ref: %+v %v", ref, err)
	}
}
