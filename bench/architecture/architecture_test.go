package architecture_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectarch "github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/interaction"
)

func policyReport(interactionName string) interaction.Report {
	return interaction.Report{
		Trigger: "endpoint", Preparation: "endpoint", Rollout: "slow-only",
		Floor: "model:foreground", BargeIn: "never", Commitment: "complete",
		Repair: "audible", Backchannel: "none", TurnProjection: "vad",
		Overlap: "unclassified", Deferral: "always", Interaction: interactionName,
		Extraction: "unset",
	}
}

func architectureCell(name string, level architecture.Level) architecture.Cell {
	definitionRef := map[architecture.Level]string{
		architecture.LevelPredicates: "omni.external-predicates@4",
		architecture.LevelTextPolicy: "omni.external-policy@4",
		architecture.LevelNative:     "omni.native-policy@3",
	}[level]
	definition, err := projectarch.Default().Resolve(definitionRef)
	if err != nil {
		panic(err)
	}
	ownership := binding.Ownership{
		Perception: binding.OwnerModel, FastCognition: binding.OwnerModel,
		SlowCognition: binding.OwnerEngine, Action: binding.OwnerModel,
		Interaction: binding.OwnerEngine, Floor: binding.OwnerEngine,
	}
	capabilities := binding.StackCapabilities{
		AudioInput: true, AudioOutput: true, Transcription: true, TurnGeneration: true,
		ConcurrentIO: true, NativeFloor: true, NativeInteraction: true,
		InteractionActs: true, TextInjection: true,
	}
	interactionIdentity := architecture.InteractionIdentity{
		PolicyName: "model:policy-3b",
		Model: architecture.ModelIdentity{
			Provider: "vllm", Model: "policy-3b", Revision: "policy-r1",
		},
		InstructionRevision: "interaction-instruction-r2", DecisionTimeoutMS: 150,
		Evidence: architecture.EvidenceIdentity{
			Source:       architecture.EvidenceTranscript,
			Capabilities: *definition.Interaction.EvidenceCapabilities,
			Recognizer: architecture.ModelIdentity{
				Provider: "qwen", Model: "qwen3-asr", Revision: "asr-model-r7",
				AdapterRevision: "asr-adapter-r2",
			},
		},
		Protocol: architecture.ProtocolIdentity{
			Transport: "sidecar", Version: 2, Handoff: architecture.HandoffTyped,
		},
		NativeSuppressionContract: definition.Interaction.NativeSuppression,
		Control:                   *definition.Interaction.Control,
	}
	policies := policyReport(interactionIdentity.PolicyName)
	if level == architecture.LevelPredicates {
		interactionIdentity = architecture.InteractionIdentity{
			Evidence: architecture.EvidenceIdentity{
				Source:       architecture.EvidenceAcousticPredicates,
				Capabilities: *definition.Interaction.EvidenceCapabilities,
			},
			Protocol: architecture.ProtocolIdentity{
				Transport: "sidecar", Version: 2, Handoff: architecture.HandoffTyped,
			},
			NativeSuppressionContract: definition.Interaction.NativeSuppression,
			Control:                   *definition.Interaction.Control,
		}
		policies.Floor = "engine-500ms"
		policies.Interaction = "unset"
		policies.Extraction = "unset"
	} else if level == architecture.LevelNative {
		ownership.Interaction = binding.OwnerModel
		interactionIdentity = architecture.InteractionIdentity{
			Evidence: architecture.EvidenceIdentity{
				Source:       architecture.EvidenceNativeMultimodal,
				Capabilities: *definition.Interaction.EvidenceCapabilities,
			},
			Protocol: architecture.ProtocolIdentity{
				Transport: "sidecar", Version: 2, Handoff: architecture.HandoffNone,
			},
			Control: *definition.Interaction.Control,
		}
		policies.Interaction = "unset"
	}
	return architecture.Cell{Name: name, Availability: architecture.AvailabilityRunnable,
		Architecture: architecture.Architecture{
			Definition: definition, Level: level, RuntimeBinding: "sidecar", Profile: "voice",
			Foreground: architecture.ModelIdentity{
				Provider: "sidecar", Model: "same-omni", Revision: "foreground-r4",
			},
			Slow:      architecture.ModelIdentity{Provider: "gemini", Model: "slow", Revision: "slow-r3"},
			Ownership: ownership, Capabilities: capabilities, Policies: policies,
			Observers: []string{"sidecar:same-omni"}, Interaction: interactionIdentity,
			ToolAuthority: binding.ToolStatus{
				Fast: "propose", Slow: "execute", Authorization: "engine", Execution: "engine-or-client",
			},
		}}
}

func TestUnavailableCellsRemainInTheExperimentMatrix(t *testing.T) {
	cell := architectureCell("N", architecture.LevelNative)
	cell.Availability = architecture.AvailabilityUnavailable
	cell.UnavailableReason = "foreground sidecar exposes no native interaction selection"
	if err := manifest(cell).Validate(); err != nil {
		t.Fatalf("a machine-readable unavailable cell should validate: %v", err)
	}
	cell.UnavailableReason = ""
	if err := manifest(cell).Validate(); err == nil || !strings.Contains(err.Error(), "without a reason") {
		t.Fatalf("an unexplained missing cell disappeared silently: %v", err)
	}
}

func manifest(cells ...architecture.Cell) architecture.Manifest {
	return architecture.Manifest{
		Version: architecture.ManifestVersion, Name: "f52", Suite: "scenario",
		FixtureRevision: "scenario-r9", Cells: cells,
	}
}

func observed(cell architecture.Cell) binding.Status {
	value := cell.Architecture
	runtimeID := func(identity architecture.ModelIdentity) string {
		if strings.TrimSpace(identity.RuntimeID) != "" {
			return strings.TrimSpace(identity.RuntimeID)
		}
		return strings.TrimSpace(identity.Provider) + "/" + strings.TrimSpace(identity.Model)
	}
	interactionStatus := binding.InteractionStatus{
		Evidence:             string(value.Interaction.Evidence.Source),
		EvidenceCapabilities: value.Interaction.Evidence.Capabilities,
		Transport:            value.Interaction.Protocol.Transport,
		ProtocolVersion:      value.Interaction.Protocol.Version,
		ActHandoff:           string(value.Interaction.Protocol.Handoff),
		DecisionTimeoutMS:    value.Interaction.DecisionTimeoutMS,
		NativeSuppression:    value.Interaction.NativeSuppressionContract,
		Control:              value.Interaction.Control,
	}
	if value.Interaction.Evidence.Source == architecture.EvidenceTranscript {
		interactionStatus.Recognizer = value.Interaction.Evidence.Recognizer.Provider + "/" +
			value.Interaction.Evidence.Recognizer.Model
		interactionStatus.RecognizerRevision = value.Interaction.Evidence.Recognizer.AdapterRevision
	}
	status := binding.Status{
		Architecture: value.Definition.Identity(), Binding: value.RuntimeBinding,
		Profile: value.Profile, Ownership: value.Ownership,
		Stack: value.Capabilities, Policies: value.Policies, Interaction: interactionStatus,
		Tools:     value.ToolAuthority,
		Observers: value.Observers,
		Fast:      runtimeID(value.Foreground), Slow: runtimeID(value.Slow),
	}
	if value.Ownership.Perception != binding.OwnerModel {
		status.Perception = runtimeID(value.Perception)
		status.PerceptionRevision = value.Perception.AdapterRevision
	}
	if value.Ownership.Action != binding.OwnerModel {
		status.Speech = runtimeID(value.Speech)
		status.SpeechRevision = value.Speech.AdapterRevision
	}
	if value.SpeakerIdentity.Provider != "" {
		status.SpeakerIdentity = runtimeID(value.SpeakerIdentity)
		status.SpeakerIdentityRevision = value.SpeakerIdentity.AdapterRevision
	}
	if value.VisualNarrator.Provider != "" {
		status.VisualNarrator = runtimeID(value.VisualNarrator)
	}
	return status
}

func completedResult(definition architecture.Manifest, cell architecture.Cell) architecture.Result {
	result := architecture.NewResult(definition, cell, 1)
	result.Measurement.Provenance = bench.Provenance{
		Revision: "source-r1", ExecutableSHA256: "binary-r1",
	}
	result.Measurement.Tasks = []bench.TaskOutcome{{ID: "task-1", Completed: true, Passed: true}}
	result.Observed = []architecture.Observation{{TaskID: "task-1", Status: observed(cell)}}
	result.Finish()
	return result
}

func TestTextPolicyRequiresAnAuditableTypedControlBoundary(t *testing.T) {
	cell := architectureCell("T", architecture.LevelTextPolicy)
	if err := cell.Architecture.Validate(); err != nil {
		t.Fatalf("valid T cell: %v", err)
	}

	translated := cell.Architecture
	translated.Interaction.Protocol.Handoff = architecture.HandoffTranslated
	if err := translated.Validate(); err == nil || !strings.Contains(err.Error(), "typed") {
		t.Fatalf("a translated act boundary was accepted: %v", err)
	}

	unsuppressed := cell.Architecture
	unsuppressed.Interaction.NativeSuppressionContract = ""
	if err := unsuppressed.Validate(); err == nil || !strings.Contains(err.Error(), "suppression") {
		t.Fatalf("native interaction was ambiguously overridden: %v", err)
	}
}

func TestNativeInteractionIsACapabilityAndOwnershipSelection(t *testing.T) {
	cell := architectureCell("N", architecture.LevelNative)
	if err := cell.Architecture.Validate(); err != nil {
		t.Fatalf("valid N cell: %v", err)
	}

	missing := cell.Architecture
	missing.Capabilities.NativeInteraction = false
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "native-interaction") {
		t.Fatalf("model interaction without its capability was accepted: %v", err)
	}
}

func TestSameForegroundTextPolicyAndNativeCellsAreArchitectureEvidence(t *testing.T) {
	textPolicy := architectureCell("T", architecture.LevelTextPolicy)
	native := architectureCell("N", architecture.LevelNative)
	definition := manifest(textPolicy, native)
	if err := definition.Validate(); err != nil {
		t.Fatalf("manifest: %v", err)
	}

	comparison := architecture.Pair(
		completedResult(definition, textPolicy), completedResult(definition, native))
	if !comparison.Reportable || comparison.Claim != architecture.ClaimArchitecture {
		t.Fatalf("same-foreground T/N pair was not architecture evidence: %+v", comparison)
	}
	if comparison.Measurement.Factor != bench.FactorInteractionArchitecture {
		t.Fatalf("unexpected generic factor %q", comparison.Measurement.Factor)
	}
}

func TestSameForegroundPredicatesAndTextPolicyAreArchitectureEvidence(t *testing.T) {
	predicates := architectureCell("P", architecture.LevelPredicates)
	textPolicy := architectureCell("T", architecture.LevelTextPolicy)
	definition := manifest(predicates, textPolicy)
	if err := definition.Validate(); err != nil {
		t.Fatalf("manifest: %v", err)
	}

	comparison := architecture.Pair(
		completedResult(definition, predicates), completedResult(definition, textPolicy))
	if !comparison.Reportable || comparison.Claim != architecture.ClaimArchitecture {
		t.Fatalf("same-foreground P/T pair was not architecture evidence: %+v", comparison)
	}
}

func cascadeControllerCell(name, reference string, level architecture.Level) architecture.Cell {
	definition, err := projectarch.Default().Resolve(reference)
	if err != nil {
		panic(err)
	}
	perception := architecture.ModelIdentity{
		Provider: "openai-compatible", Model: "whisper", Revision: "whisper-r1",
		AdapterRevision: "whisper-adapter-r1", RuntimeID: "openai-compatible/whisper",
	}
	policy := architecture.InteractionIdentity{
		PolicyName: "model:policy-3b",
		Model: architecture.ModelIdentity{
			Provider: "vllm", Model: "policy-3b", Revision: "policy-r1",
		},
		InstructionRevision: "interaction-instruction-r2", DecisionTimeoutMS: 150,
		Evidence: architecture.EvidenceIdentity{
			Source: architecture.EvidenceTranscript, Capabilities: *definition.Interaction.EvidenceCapabilities,
			Recognizer: perception,
		},
		Protocol: architecture.ProtocolIdentity{Transport: "in-process", Handoff: architecture.HandoffDirect},
		Control:  *definition.Interaction.Control,
	}
	report := interaction.Defaults().Report()
	report.Interaction, report.Extraction = policy.PolicyName, "model:policy-3b"
	if level == architecture.LevelTextPolicy {
		report.Floor, report.BargeIn = "act:model:policy-3b", "act:model:policy-3b"
	}
	return architecture.Cell{Name: name, Availability: architecture.AvailabilityRunnable,
		Architecture: architecture.Architecture{
			Definition: definition, Level: level, RuntimeBinding: "cascade", Profile: "voice",
			Foreground: architecture.ModelIdentity{Provider: "vllm", Model: "qwen", Revision: "qwen-r1"},
			Perception: perception,
			Speech: architecture.ModelIdentity{
				Provider: "fish", Model: "fish-speech", Revision: "fish-r1",
				AdapterRevision: "fish-adapter-r1", RuntimeID: "fish/fish-speech",
			},
			Slow:      architecture.ModelIdentity{Provider: "vllm", Model: "qwen", Revision: "qwen-r1"},
			Ownership: definition.Ownership, Capabilities: definition.Requires,
			Policies: report, Observers: []string{"audio"}, Interaction: policy,
			ToolAuthority: binding.ToolStatus{
				Fast: "propose", Slow: "execute", Authorization: "engine", Execution: "engine-or-client",
			},
		},
	}
}

func TestPureAndComposedPolicyCellsAreControlledArchitectureEvidence(t *testing.T) {
	pure := cascadeControllerCell("T", "cascade.text-policy@3", architecture.LevelTextPolicy)
	composed := cascadeControllerCell("C", "cascade.composed-policy@1", architecture.LevelComposed)
	definition := manifest(pure, composed)
	if err := definition.Validate(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	comparison := architecture.Pair(
		completedResult(definition, pure), completedResult(definition, composed))
	if !comparison.Reportable || comparison.Claim != architecture.ClaimArchitecture {
		t.Fatalf("same-component T/C pair was not architecture evidence: %+v", comparison)
	}

	wrong := composed.Architecture
	wrong.Interaction.Control.Selectors.Predicates = false
	if err := wrong.Validate(); err == nil || !strings.Contains(err.Error(), "control") {
		t.Fatalf("a mislabeled composed controller was accepted: %v", err)
	}
}

func TestDifferentForegroundsRemainASystemComparison(t *testing.T) {
	textPolicy := architectureCell("qwen-T", architecture.LevelTextPolicy)
	native := architectureCell("moshi-N", architecture.LevelNative)
	native.Architecture.Foreground = architecture.ModelIdentity{
		Provider: "sidecar", Model: "moshi", Revision: "moshi-r2",
	}
	native.Architecture.Observers = []string{"sidecar:moshi"}
	definition := manifest(textPolicy, native)
	if err := definition.Validate(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	nativeResult := completedResult(definition, native)
	nativeResult.Observed[0].Status.Fast = "sidecar/moshi"
	nativeResult.Observed[0].Status.Observers = []string{"sidecar:moshi"}

	comparison := architecture.Pair(completedResult(definition, textPolicy), nativeResult)
	if !comparison.Reportable || comparison.Claim != architecture.ClaimSystem {
		t.Fatalf("a confounded model-family comparison was mislabeled: %+v", comparison)
	}
	if len(comparison.Differences) == 0 || comparison.Differences[0] != "foreground" {
		t.Fatalf("the confounders were not retained: %+v", comparison.Differences)
	}
}

func TestObservedCapabilitiesMustMatchTheManifest(t *testing.T) {
	cell := architectureCell("T", architecture.LevelTextPolicy)
	status := observed(cell)
	status.Stack.InteractionActs = false
	if err := cell.ValidateObserved(status); err == nil || !strings.Contains(err.Error(), "capability") {
		t.Fatalf("a mislabeled live session was accepted: %v", err)
	}
}

func TestObservedToolAuthorityMustMatchTheManifest(t *testing.T) {
	cell := architectureCell("T", architecture.LevelTextPolicy)
	status := observed(cell)
	status.Tools.Fast = "execute"
	if err := cell.ValidateObserved(status); err == nil || !strings.Contains(err.Error(), "tool authority") {
		t.Fatalf("an authority change was accepted as the same architecture: %v", err)
	}
}

func TestArchitectureEvidenceMustBelongToTheMeasuredTask(t *testing.T) {
	cell := architectureCell("T", architecture.LevelTextPolicy)
	definition := manifest(cell)
	result := completedResult(definition, cell)
	result.Observed[0].TaskID = "different-task"
	if err := result.Reportable(); err == nil ||
		!strings.Contains(err.Error(), "not a measured task") ||
		!strings.Contains(err.Error(), "has no live architecture evidence") {
		t.Fatalf("unbound live evidence was accepted: %v", err)
	}
}

func TestReadersRejectTrailingJSONValues(t *testing.T) {
	cell := architectureCell("T", architecture.LevelTextPolicy)
	definition := manifest(cell)
	result := completedResult(definition, cell)

	tests := []struct {
		name  string
		value any
		read  func(string) error
	}{
		{name: "manifest", value: definition, read: func(path string) error {
			_, err := architecture.Read(path)
			return err
		}},
		{name: "result", value: result, read: func(path string) error {
			_, err := architecture.ReadResult(path)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), test.name+".json")
			if err := os.WriteFile(path, append(payload, []byte("\n{}\n")...), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := test.read(path); err == nil || !strings.Contains(err.Error(), "trailing JSON value") {
				t.Fatalf("trailing value was accepted: %v", err)
			}
		})
	}
}

func TestHistoricalResultsRemainInspectableButNotReportable(t *testing.T) {
	cell := architectureCell("T", architecture.LevelTextPolicy)
	definition := manifest(cell)
	for _, version := range []int{2, 3} {
		result := completedResult(definition, cell)
		result.Version = version
		payload, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), fmt.Sprintf("historical-v%d.json", version))
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		read, err := architecture.ReadResult(path)
		if err != nil {
			t.Fatalf("historical v%d diagnostic became unreadable: %v", version, err)
		}
		if err := read.Reportable(); err == nil || !strings.Contains(err.Error(), "version must be") {
			t.Fatalf("historical v%d schema became current evidence: %v", version, err)
		}
	}
}

func TestCellAuthoringKeepsDefinitionsStatusAndPinsIndependent(t *testing.T) {
	expected := architectureCell("T", architecture.LevelTextPolicy)
	status := observed(expected)
	pins := architecture.Pins{
		Version:               architecture.PinsVersion,
		Foreground:            expected.Architecture.Foreground,
		Perception:            expected.Architecture.Perception,
		Speech:                expected.Architecture.Speech,
		Slow:                  expected.Architecture.Slow,
		InteractionPolicy:     expected.Architecture.Interaction.Model,
		InteractionRecognizer: expected.Architecture.Interaction.Evidence.Recognizer,
		InstructionRevision:   expected.Architecture.Interaction.InstructionRevision,
	}
	authored, err := architecture.BuildCell(
		"authored-T", expected.Architecture.Definition, status, pins,
	)
	if err != nil {
		t.Fatal(err)
	}
	if authored.Architecture.Definition.Fingerprint() != expected.Architecture.Definition.Fingerprint() ||
		authored.Architecture.Foreground != pins.Foreground ||
		authored.Architecture.Interaction.PolicyName != status.Policies.Interaction {
		t.Fatalf("one authoring source replaced another: %+v", authored)
	}

	status.Architecture.Fingerprint = "sha256:wrong-definition"
	if _, err := architecture.BuildCell(
		"wrong", expected.Architecture.Definition, status, pins,
	); err == nil || !strings.Contains(err.Error(), "not architecture") {
		t.Fatalf("a status from another definition was accepted: %v", err)
	}
}
