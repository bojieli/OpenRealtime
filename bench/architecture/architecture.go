// Package architecture makes interaction-architecture experiments explicit.
//
// Cascade, Omni, and duplex are not model species here. A cell is the resolved
// composition of independently selected ownership and capabilities plus the
// identities of the components that produced it. The P/T/C/N level is a
// treatment label; validation and pairing use the complete structure.
package architecture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"

	projectarch "github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/interaction"
)

const ManifestVersion = 4

// Level names the interaction treatment, not the foreground model family. C
// is an explicit composition of P and T with one arbitration contract; it is
// not a fourth model species.
type Level string

const (
	LevelPredicates Level = "predicates"
	LevelTextPolicy Level = "text-policy"
	LevelComposed   Level = "composed-policy"
	LevelNative     Level = "native"
)

// EvidenceSource names the representation available to the interaction
// decider. It is deliberately not inferred from the model name.
type EvidenceSource string

const (
	EvidenceAcousticPredicates EvidenceSource = "acoustic-predicates"
	EvidenceTranscript         EvidenceSource = "transcript"
	EvidenceNativeMultimodal   EvidenceSource = "native-multimodal"
)

// ActHandoff says how a selected act reaches the machinery that performs it.
type ActHandoff string

const (
	HandoffNone       ActHandoff = "none"
	HandoffDirect     ActHandoff = "direct"
	HandoffTyped      ActHandoff = "typed"
	HandoffTranslated ActHandoff = "translated"
)

// Availability distinguishes a runnable cell from a desired cell whose model
// boundary cannot currently realize the declared selection. Keeping the latter
// in the manifest prevents the experiment matrix from silently shrinking to
// whatever happened to work.
type Availability string

const (
	AvailabilityRunnable    Availability = "runnable"
	AvailabilityUnavailable Availability = "unavailable"
)

// ModelIdentity is an immutable identity, plus the spelling a live runtime
// uses when that spelling is not simply provider/model.
type ModelIdentity struct {
	Provider        string `json:"provider"`
	Model           string `json:"model"`
	Revision        string `json:"revision"`
	AdapterRevision string `json:"adapter_revision,omitempty"`
	RuntimeID       string `json:"runtime_id,omitempty"`
}

func (identity ModelIdentity) empty() bool {
	return strings.TrimSpace(identity.Provider) == "" && strings.TrimSpace(identity.Model) == "" &&
		strings.TrimSpace(identity.Revision) == "" && strings.TrimSpace(identity.AdapterRevision) == "" &&
		strings.TrimSpace(identity.RuntimeID) == ""
}

func (identity ModelIdentity) validate(label string) error {
	if strings.TrimSpace(identity.Provider) == "" || strings.TrimSpace(identity.Model) == "" ||
		strings.TrimSpace(identity.Revision) == "" {
		return fmt.Errorf("%s requires provider, model, and immutable revision", label)
	}
	return nil
}

func (identity ModelIdentity) runtimeID() string {
	if value := strings.TrimSpace(identity.RuntimeID); value != "" {
		return value
	}
	return strings.TrimSpace(identity.Provider) + "/" + strings.TrimSpace(identity.Model)
}

// EvidenceIdentity describes what the interaction policy sees. Source names
// the broad representation for compatibility; Capabilities is the exact
// selected vector and is the experiment authority. Recognizer is required for
// transcript evidence and absent for native multimodal evidence.
type EvidenceIdentity struct {
	Source       EvidenceSource                          `json:"source"`
	Capabilities binding.InteractionEvidenceCapabilities `json:"capabilities"`
	Recognizer   ModelIdentity                           `json:"recognizer,omitempty"`
}

// ProtocolIdentity describes the control boundary. Transport is "in-process"
// when no process boundary exists and "sidecar" when acts cross one.
type ProtocolIdentity struct {
	Transport string     `json:"transport"`
	Version   int        `json:"version,omitempty"`
	Handoff   ActHandoff `json:"handoff"`
}

// InteractionIdentity is every treatment-specific detail needed to reproduce
// the selection. PolicyName is the live interaction.Report spelling; Model is
// the endpoint identity behind it.
type InteractionIdentity struct {
	PolicyName                string                     `json:"policy_name,omitempty"`
	Model                     ModelIdentity              `json:"model,omitempty"`
	InstructionRevision       string                     `json:"instruction_revision,omitempty"`
	DecisionTimeoutMS         int                        `json:"decision_timeout_ms,omitempty"`
	Evidence                  EvidenceIdentity           `json:"evidence"`
	Protocol                  ProtocolIdentity           `json:"protocol"`
	NativeSuppressionContract string                     `json:"native_suppression_contract,omitempty"`
	Control                   binding.InteractionControl `json:"control"`
}

// Architecture is one fully resolved composition. RuntimeBinding is only an
// evidence label; no behavior is inferred from it.
type Architecture struct {
	// Definition is the immutable project-level architecture revision this
	// deployment realises. The rest of the fields bind that structural
	// definition to exact models, adapters, policies, and experimental evidence.
	Definition      projectarch.Definition    `json:"definition"`
	Level           Level                     `json:"level"`
	RuntimeBinding  string                    `json:"runtime_binding"`
	Profile         string                    `json:"profile"`
	Foreground      ModelIdentity             `json:"foreground"`
	Perception      ModelIdentity             `json:"perception,omitempty"`
	Speech          ModelIdentity             `json:"speech,omitempty"`
	SpeakerIdentity ModelIdentity             `json:"speaker_identity,omitempty"`
	VisualNarrator  ModelIdentity             `json:"visual_narrator,omitempty"`
	Slow            ModelIdentity             `json:"slow"`
	Ownership       binding.Ownership         `json:"ownership"`
	Capabilities    binding.StackCapabilities `json:"capabilities"`
	Policies        interaction.Report        `json:"policies"`
	Observers       []string                  `json:"observers"`
	Interaction     InteractionIdentity       `json:"interaction"`
	ToolAuthority   binding.ToolStatus        `json:"tool_authority"`
}

// Cell names one architecture in an experiment manifest.
type Cell struct {
	Name              string       `json:"name"`
	Availability      Availability `json:"availability"`
	UnavailableReason string       `json:"unavailable_reason,omitempty"`
	Architecture      Architecture `json:"architecture"`
	// Execution is independent of the P/T/C/N architecture description. It
	// proves which executable graph realised that description for benchmark
	// tasks, rather than treating a binding label as execution evidence.
	Execution bench.ExecutionRequirement `json:"execution,omitzero"`
}

// Manifest is a versioned experiment definition shared by every cell in a
// controlled comparison.
type Manifest struct {
	Version         int    `json:"version"`
	Name            string `json:"name"`
	Suite           string `json:"suite"`
	FixtureRevision string `json:"fixture_revision"`
	Cells           []Cell `json:"cells"`
}

// Read decodes and validates a manifest.
func Read(path string) (Manifest, error) {
	if strings.TrimSpace(path) == "" {
		return Manifest{}, errors.New("an architecture experiment needs a manifest path")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode architecture manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Manifest{}, errors.New("decode architecture manifest: trailing JSON value")
		}
		return Manifest{}, fmt.Errorf("decode architecture manifest trailing content: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Validate rejects manifests that omit a confounder the comparison needs.
func (manifest Manifest) Validate() error {
	if manifest.Version != ManifestVersion {
		return fmt.Errorf("architecture manifest version must be %d, got %d", ManifestVersion, manifest.Version)
	}
	if strings.TrimSpace(manifest.Name) == "" || strings.TrimSpace(manifest.Suite) == "" ||
		strings.TrimSpace(manifest.FixtureRevision) == "" {
		return errors.New("an architecture manifest requires name, suite, and fixture revision")
	}
	if len(manifest.Cells) == 0 {
		return errors.New("an architecture manifest requires at least one cell")
	}
	seen := make(map[string]bool, len(manifest.Cells))
	for _, cell := range manifest.Cells {
		name := strings.TrimSpace(cell.Name)
		if name == "" || seen[name] {
			return fmt.Errorf("architecture cell names must be non-empty and unique, got %q", cell.Name)
		}
		seen[name] = true
		switch cell.Availability {
		case AvailabilityRunnable:
			if strings.TrimSpace(cell.UnavailableReason) != "" {
				return fmt.Errorf("architecture cell %q is runnable but has an unavailable reason", cell.Name)
			}
		case AvailabilityUnavailable:
			if strings.TrimSpace(cell.UnavailableReason) == "" {
				return fmt.Errorf("architecture cell %q is unavailable without a reason", cell.Name)
			}
		default:
			return fmt.Errorf("architecture cell %q must declare runnable or unavailable", cell.Name)
		}
		if err := cell.Architecture.Validate(); err != nil {
			return fmt.Errorf("architecture cell %q: %w", cell.Name, err)
		}
		if err := cell.Execution.Validate(); err != nil {
			return fmt.Errorf("architecture cell %q execution: %w", cell.Name, err)
		}
	}
	return nil
}

// Cell returns one named cell.
func (manifest Manifest) Cell(name string) (Cell, error) {
	for _, cell := range manifest.Cells {
		if cell.Name == strings.TrimSpace(name) {
			return cell, nil
		}
	}
	return Cell{}, fmt.Errorf("architecture manifest %q has no cell %q", manifest.Name, name)
}

// ID is the complete manifest fingerprint.
func (manifest Manifest) ID() string { return bench.Fingerprint(manifest) }

// MeasurementCell is the compact cell used by the generic benchmark report.
// The architecture artifact carries the structure this F52 level cannot.
func (cell Cell) MeasurementCell() bench.Cell {
	return bench.Cell{Name: cell.Name, Levels: map[bench.Factor]string{
		bench.FactorInteractionArchitecture: string(cell.Architecture.Level),
	}, Execution: cell.Execution}
}

// Validate checks one resolved architecture from first principles.
func (architecture Architecture) Validate() error {
	if err := architecture.Definition.Validate(); err != nil {
		return fmt.Errorf("architecture definition: %w", err)
	}
	if architecture.Definition.Interaction.EvidenceCapabilities == nil {
		return fmt.Errorf("architecture definition %s predates exact interaction evidence attestation and is not valid for a new F52 cell", architecture.Definition.Ref())
	}
	if architecture.Definition.Interaction.Control == nil {
		return fmt.Errorf("architecture definition %s predates exact interaction-controller attestation and is not valid for a new F52 cell", architecture.Definition.Ref())
	}
	if err := architecture.Ownership.Validate(); err != nil {
		return err
	}
	architecture.Ownership = architecture.Ownership.Effective()
	if strings.TrimSpace(architecture.RuntimeBinding) == "" || strings.TrimSpace(architecture.Profile) == "" {
		return errors.New("runtime binding and profile are required evidence labels")
	}
	if err := architecture.Foreground.validate("foreground model"); err != nil {
		return err
	}
	if err := architecture.Slow.validate("slow model"); err != nil {
		return err
	}
	if strings.TrimSpace(architecture.ToolAuthority.Fast) == "" ||
		strings.TrimSpace(architecture.ToolAuthority.Slow) == "" ||
		strings.TrimSpace(architecture.ToolAuthority.Authorization) == "" ||
		strings.TrimSpace(architecture.ToolAuthority.Execution) == "" {
		return errors.New("fast, slow, authorization, and execution tool authority must be explicit")
	}
	if !architecture.Capabilities.AudioInput || !architecture.Capabilities.AudioOutput {
		return errors.New("a voice architecture requires audio input and output capabilities")
	}
	if err := architecture.Definition.ValidateRuntime(
		architecture.Ownership, architecture.Capabilities,
	); err != nil {
		return err
	}
	if err := validateDefinitionSelection(architecture); err != nil {
		return err
	}
	if architecture.Ownership.Perception != binding.OwnerModel {
		if err := architecture.Perception.validate("perception model"); err != nil {
			return err
		}
		if strings.TrimSpace(architecture.Perception.AdapterRevision) == "" {
			return errors.New("perception model requires an adapter revision")
		}
	}
	if !architecture.SpeakerIdentity.empty() {
		if err := architecture.SpeakerIdentity.validate("speaker identity model"); err != nil {
			return err
		}
		if strings.TrimSpace(architecture.SpeakerIdentity.AdapterRevision) == "" {
			return errors.New("speaker identity model requires an adapter revision")
		}
	} else if architecture.Interaction.Evidence.Capabilities.SpeakerIdentity {
		return errors.New("speaker-identity evidence requires a pinned speaker identity model")
	}
	if !architecture.VisualNarrator.empty() {
		if err := architecture.VisualNarrator.validate("visual narrator model"); err != nil {
			return err
		}
	} else if architecture.Interaction.Evidence.Capabilities.VisualDescription {
		return errors.New("visual-description evidence requires a pinned visual narrator model")
	}
	if architecture.Ownership.Action != binding.OwnerModel {
		if err := architecture.Speech.validate("speech model"); err != nil {
			return err
		}
		if strings.TrimSpace(architecture.Speech.AdapterRevision) == "" {
			return errors.New("speech model requires an adapter revision")
		}
	}
	if architecture.Ownership.Floor == binding.OwnerModel && !architecture.Capabilities.NativeFloor {
		return errors.New("model-owned floor requires native_floor capability")
	}
	if err := validatePolicyReport(architecture.Policies); err != nil {
		return err
	}

	switch architecture.Level {
	case LevelPredicates:
		if architecture.Ownership.Interaction != binding.OwnerEngine {
			return errors.New("predicate interaction requires engine interaction ownership")
		}
		if architecture.Interaction.Evidence.Source != EvidenceAcousticPredicates {
			return errors.New("predicate interaction requires acoustic-predicates evidence")
		}
		if !architecture.Interaction.Evidence.Capabilities.AcousticActivity ||
			!architecture.Interaction.Evidence.Capabilities.SilenceClock {
			return errors.New("predicate interaction requires acoustic activity and silence-clock evidence")
		}
		if !architecture.Interaction.Model.empty() {
			return errors.New("predicate interaction cannot declare an external policy model")
		}
		if strings.TrimSpace(architecture.Interaction.PolicyName) != "" ||
			strings.TrimSpace(architecture.Interaction.InstructionRevision) != "" ||
			architecture.Interaction.DecisionTimeoutMS != 0 ||
			!architecture.Interaction.Evidence.Recognizer.empty() {
			return errors.New("predicate interaction cannot carry text-policy identity or recognizer evidence")
		}
		if architecture.Policies.Interaction != "unset" {
			return errors.New("predicate interaction requires the engine interaction model to be unset")
		}
		if architecture.Interaction.Control != (binding.InteractionControl{
			Selectors: binding.InteractionControllers{Predicates: true}, Arbitration: "single",
		}) {
			return errors.New("predicate interaction requires one predicate controller")
		}
	case LevelTextPolicy, LevelComposed:
		if architecture.Ownership.Interaction != binding.OwnerEngine {
			return errors.New("external policy interaction requires engine interaction ownership")
		}
		if err := architecture.Interaction.Model.validate("interaction policy model"); err != nil {
			return err
		}
		if strings.TrimSpace(architecture.Interaction.PolicyName) == "" ||
			strings.TrimSpace(architecture.Interaction.InstructionRevision) == "" ||
			architecture.Interaction.DecisionTimeoutMS <= 0 {
			return errors.New("text-policy interaction requires policy name, instruction revision, and positive timeout")
		}
		if architecture.Policies.Interaction != architecture.Interaction.PolicyName {
			return errors.New("the policy report and interaction policy identity disagree")
		}
		if architecture.Interaction.Evidence.Source != EvidenceTranscript {
			return errors.New("text-policy interaction requires transcript evidence")
		}
		if !architecture.Interaction.Evidence.Capabilities.Transcript {
			return errors.New("text-policy interaction evidence vector omits the transcript")
		}
		if err := architecture.Interaction.Evidence.Recognizer.validate("interaction recognizer"); err != nil {
			return err
		}
		if strings.TrimSpace(architecture.Interaction.Evidence.Recognizer.AdapterRevision) == "" {
			return errors.New("interaction recognizer requires an adapter revision")
		}
		if err := validateActBoundary(architecture); err != nil {
			return err
		}
		want := binding.InteractionControl{
			Selectors: binding.InteractionControllers{TextPolicy: true}, Arbitration: "single",
		}
		if architecture.Level == LevelComposed {
			want = binding.InteractionControl{
				Selectors:   binding.InteractionControllers{Predicates: true, TextPolicy: true},
				Arbitration: "predicate-floor",
			}
		}
		if architecture.Interaction.Control != want {
			return fmt.Errorf("%s interaction has the wrong controller composition", architecture.Level)
		}
	case LevelNative:
		if architecture.Ownership.Interaction != binding.OwnerModel || !architecture.Capabilities.NativeInteraction {
			return errors.New("native interaction requires model ownership and native_interaction capability")
		}
		if architecture.Interaction.Evidence.Source != EvidenceNativeMultimodal {
			return errors.New("native interaction requires native-multimodal evidence")
		}
		if !architecture.Interaction.Evidence.Capabilities.NativeModelState {
			return errors.New("native interaction evidence vector omits native model state")
		}
		if !architecture.Interaction.Model.empty() {
			return errors.New("native interaction is part of the foreground and cannot declare an external policy model")
		}
		if strings.TrimSpace(architecture.Interaction.PolicyName) != "" ||
			strings.TrimSpace(architecture.Interaction.InstructionRevision) != "" ||
			architecture.Interaction.DecisionTimeoutMS != 0 ||
			!architecture.Interaction.Evidence.Recognizer.empty() {
			return errors.New("native interaction cannot carry an external policy or recognizer identity")
		}
		if architecture.Policies.Interaction != "unset" {
			return errors.New("native interaction requires the engine interaction model to be unset")
		}
		if architecture.Interaction.Control != (binding.InteractionControl{
			Selectors: binding.InteractionControllers{Native: true}, Arbitration: "single",
		}) {
			return errors.New("native interaction requires one native controller")
		}
	default:
		return fmt.Errorf("unknown interaction architecture level %q", architecture.Level)
	}

	if architecture.Ownership.Interaction == binding.OwnerEngine && architecture.Capabilities.NativeInteraction {
		if strings.TrimSpace(architecture.Interaction.NativeSuppressionContract) == "" {
			return errors.New("external interaction over a native-interaction foreground requires an explicit suppression contract")
		}
		protocol := architecture.Interaction.Protocol
		if protocol.Transport != "sidecar" || protocol.Version < 2 || protocol.Handoff != HandoffTyped ||
			!architecture.Capabilities.InteractionActs {
			return errors.New("suppressing native interaction requires sidecar v2 typed acts and interaction_acts capability")
		}
	} else if strings.TrimSpace(architecture.Interaction.NativeSuppressionContract) != "" {
		return errors.New("a native suppression contract is valid only when engine interaction overrides a native-interaction foreground")
	}
	return nil
}

func validateDefinitionSelection(architecture Architecture) error {
	definition := architecture.Definition
	expectedLevel := map[projectarch.InteractionMode]Level{
		projectarch.InteractionPredicates: LevelPredicates,
		projectarch.InteractionTextPolicy: LevelTextPolicy,
		projectarch.InteractionComposed:   LevelComposed,
		projectarch.InteractionNative:     LevelNative,
	}[definition.Interaction.Mode]
	if expectedLevel == "" || architecture.Level != expectedLevel {
		return fmt.Errorf("architecture definition %s interaction mode %q does not match F52 level %q",
			definition.Ref(), definition.Interaction.Mode, architecture.Level)
	}
	boundary := architecture.Interaction
	if string(definition.Interaction.Evidence) != string(boundary.Evidence.Source) ||
		definition.Interaction.Transport != boundary.Protocol.Transport ||
		definition.Interaction.ProtocolVersion != boundary.Protocol.Version ||
		string(definition.Interaction.Handoff) != string(boundary.Protocol.Handoff) ||
		definition.Interaction.NativeSuppression != boundary.NativeSuppressionContract {
		return fmt.Errorf(
			"architecture definition %s interaction boundary disagrees with the experiment cell: "+
				"evidence=%q/%q transport=%q/%q protocol=%d/%d handoff=%q/%q suppression=%q/%q",
			definition.Ref(), boundary.Evidence.Source, definition.Interaction.Evidence,
			boundary.Protocol.Transport, definition.Interaction.Transport,
			boundary.Protocol.Version, definition.Interaction.ProtocolVersion,
			boundary.Protocol.Handoff, definition.Interaction.Handoff,
			boundary.NativeSuppressionContract, definition.Interaction.NativeSuppression,
		)
	}
	if definition.Interaction.EvidenceCapabilities == nil ||
		*definition.Interaction.EvidenceCapabilities != boundary.Evidence.Capabilities {
		return fmt.Errorf(
			"architecture definition %s interaction evidence capabilities disagree with the experiment cell: cell=%s definition=%s",
			definition.Ref(), strings.Join(boundary.Evidence.Capabilities.Names(), ", "),
			strings.Join(definition.Interaction.EvidenceCapabilities.Names(), ", "),
		)
	}
	if definition.Interaction.Control == nil ||
		*definition.Interaction.Control != boundary.Control {
		return fmt.Errorf(
			"architecture definition %s interaction control disagrees with the experiment cell: cell=%s/%q definition=%s/%q",
			definition.Ref(), strings.Join(boundary.Control.Selectors.Names(), ", "), boundary.Control.Arbitration,
			strings.Join(definition.Interaction.Control.Selectors.Names(), ", "), definition.Interaction.Control.Arbitration,
		)
	}
	return nil
}

func validateActBoundary(architecture Architecture) error {
	protocol := architecture.Interaction.Protocol
	switch strings.TrimSpace(protocol.Transport) {
	case "in-process":
		if protocol.Handoff != HandoffDirect {
			return errors.New("an in-process interaction policy requires direct act handoff")
		}
	case "sidecar":
		if protocol.Version < 2 || protocol.Handoff != HandoffTyped || !architecture.Capabilities.InteractionActs {
			return errors.New("a sidecar text-policy cell requires protocol v2, typed handoff, and interaction_acts")
		}
	default:
		return fmt.Errorf("interaction protocol transport must be in-process or sidecar, got %q", protocol.Transport)
	}
	return nil
}

func validatePolicyReport(report interaction.Report) error {
	report = effectivePolicyReport(report)
	values := reflect.ValueOf(report)
	typeOf := values.Type()
	for index := 0; index < values.NumField(); index++ {
		if strings.TrimSpace(values.Field(index).String()) == "" {
			return fmt.Errorf("policy report field %s is missing", typeOf.Field(index).Name)
		}
	}
	return nil
}

// effectivePolicyReport gives the additive transcript-event row a value in
// manifests authored before that policy existed. Empty and "unset" describe
// the same resolved architecture: no transcript-event controller is installed.
// Keeping that normalization at the evidence boundary lets historical cells
// remain inspectable without weakening validation for any established row.
func effectivePolicyReport(report interaction.Report) interaction.Report {
	if strings.TrimSpace(report.TranscriptEvents) == "" {
		report.TranscriptEvents = "unset"
	}
	return report
}

// ValidateObserved checks that a live session resolved to the declared cell.
func (cell Cell) ValidateObserved(status binding.Status) error {
	expected := cell.Architecture
	var differences []string
	if err := cell.Execution.MatchStatus(status); err != nil {
		differences = append(differences, "execution: "+err.Error())
	}
	if status.Binding != expected.RuntimeBinding {
		differences = append(differences, fmt.Sprintf("binding=%q want %q", status.Binding, expected.RuntimeBinding))
	}
	if status.Profile != expected.Profile {
		differences = append(differences, fmt.Sprintf("profile=%q want %q", status.Profile, expected.Profile))
	}
	// A graph-native result is identified by exact Graph IR plus separately
	// attested configuration, deployment, element, capability, and route facts.
	// Requiring the legacy architecture/role vectors as well would make an
	// adapter's lossy projection compete with that evidence.
	if cell.Execution.Kind == bench.ExecutionGraphNative {
		if len(differences) > 0 {
			return fmt.Errorf("live graph did not match cell %q: %s", cell.Name, strings.Join(differences, "; "))
		}
		return nil
	}
	if status.Architecture != expected.Definition.Identity() {
		differences = append(differences, "architecture definition identity differs")
	}
	if status.Ownership.Effective() != expected.Ownership.Effective() {
		differences = append(differences, "ownership vector differs")
	}
	if status.Stack != expected.Capabilities {
		differences = append(differences, "capability vector differs")
	}
	if effectivePolicyReport(status.Policies) != effectivePolicyReport(expected.Policies) {
		differences = append(differences, "policy report differs")
	}
	if status.Tools != expected.ToolAuthority {
		differences = append(differences, "tool authority differs")
	}
	if status.Interaction != expected.interactionStatus() {
		differences = append(differences, "interaction evidence or act boundary differs")
	}
	if !slices.Equal(status.Observers, expected.Observers) {
		differences = append(differences, "observer selection differs")
	}
	if status.Fast != expected.Foreground.runtimeID() {
		differences = append(differences, fmt.Sprintf("foreground=%q want %q", status.Fast, expected.Foreground.runtimeID()))
	}
	if status.Slow != expected.Slow.runtimeID() {
		differences = append(differences, fmt.Sprintf("slow=%q want %q", status.Slow, expected.Slow.runtimeID()))
	}
	if expected.Ownership.Perception != binding.OwnerModel &&
		(status.Perception != expected.Perception.runtimeID() ||
			status.PerceptionRevision != expected.Perception.AdapterRevision) {
		differences = append(differences, "perception identity differs")
	}
	if expected.Ownership.Action != binding.OwnerModel &&
		(status.Speech != expected.Speech.runtimeID() ||
			status.SpeechRevision != expected.Speech.AdapterRevision) {
		differences = append(differences, "speech identity differs")
	}
	if !expected.SpeakerIdentity.empty() &&
		(status.SpeakerIdentity != expected.SpeakerIdentity.runtimeID() ||
			status.SpeakerIdentityRevision != expected.SpeakerIdentity.AdapterRevision) {
		differences = append(differences, "speaker identity differs")
	}
	if !expected.VisualNarrator.empty() && status.VisualNarrator != expected.VisualNarrator.runtimeID() {
		differences = append(differences, "visual narrator differs")
	}
	if len(differences) > 0 {
		return fmt.Errorf("live architecture did not match cell %q: %s", cell.Name, strings.Join(differences, "; "))
	}
	return nil
}

func (architecture Architecture) interactionStatus() binding.InteractionStatus {
	status := binding.InteractionStatus{
		Evidence:             string(architecture.Interaction.Evidence.Source),
		EvidenceCapabilities: architecture.Interaction.Evidence.Capabilities,
		Transport:            architecture.Interaction.Protocol.Transport,
		ProtocolVersion:      architecture.Interaction.Protocol.Version,
		ActHandoff:           string(architecture.Interaction.Protocol.Handoff),
		DecisionTimeoutMS:    architecture.Interaction.DecisionTimeoutMS,
		NativeSuppression:    architecture.Interaction.NativeSuppressionContract,
		Control:              architecture.Interaction.Control,
	}
	if architecture.Interaction.Evidence.Source == EvidenceTranscript {
		status.Recognizer = architecture.Interaction.Evidence.Recognizer.runtimeID()
		status.RecognizerRevision = architecture.Interaction.Evidence.Recognizer.AdapterRevision
	}
	return status
}
