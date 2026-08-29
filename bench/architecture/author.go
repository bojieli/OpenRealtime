package architecture

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	projectarch "github.com/bojieli/OpenRealtime/architecture"
	"github.com/bojieli/OpenRealtime/binding"
)

const PinsVersion = 2

// Pins supplies immutable deployment identities that a live protocol status
// cannot discover honestly. It contains no endpoint, credential, or launch
// option: status remains authoritative for what ran, while pins identify the
// model and instruction artifacts behind those runtime spellings.
type Pins struct {
	Version               int           `json:"version"`
	Foreground            ModelIdentity `json:"foreground"`
	Perception            ModelIdentity `json:"perception,omitempty"`
	Speech                ModelIdentity `json:"speech,omitempty"`
	Slow                  ModelIdentity `json:"slow"`
	InteractionPolicy     ModelIdentity `json:"interaction_policy,omitempty"`
	InteractionRecognizer ModelIdentity `json:"interaction_recognizer,omitempty"`
	SpeakerIdentity       ModelIdentity `json:"speaker_identity,omitempty"`
	VisualNarrator        ModelIdentity `json:"visual_narrator,omitempty"`
	InstructionRevision   string        `json:"interaction_instruction_revision,omitempty"`
}

// ReadPins strictly reads one immutable deployment pin set.
func ReadPins(path string) (Pins, error) {
	if strings.TrimSpace(path) == "" {
		return Pins{}, errors.New("an architecture cell needs a pins path")
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return Pins{}, err
	}
	var pins Pins
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pins); err != nil {
		return Pins{}, fmt.Errorf("decode architecture pins: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Pins{}, errors.New("decode architecture pins: trailing JSON value")
		}
		return Pins{}, fmt.Errorf("decode architecture pins trailing content: %w", err)
	}
	if pins.Version != PinsVersion {
		return Pins{}, fmt.Errorf("architecture pins version must be %d, got %d", PinsVersion, pins.Version)
	}
	return pins, nil
}

// ReadStatus strictly reads a post-handshake runtime status captured through
// the protocol rather than reconstructed from launch flags.
func ReadStatus(path string) (binding.Status, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return binding.Status{}, err
	}
	var status binding.Status
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&status); err != nil {
		return binding.Status{}, fmt.Errorf("decode architecture status: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return binding.Status{}, errors.New("decode architecture status: trailing JSON value")
		}
		return binding.Status{}, fmt.Errorf("decode architecture status trailing content: %w", err)
	}
	return status, nil
}

// BuildCell combines one immutable architecture definition, one live status,
// and one immutable deployment pin set. No source is allowed to stand in for
// another: definitions provide structure, status provides negotiated reality,
// and pins provide artifact identities.
func BuildCell(
	name string, definition projectarch.Definition, status binding.Status, pins Pins,
) (Cell, error) {
	if strings.TrimSpace(name) == "" {
		return Cell{}, errors.New("an architecture cell requires a name")
	}
	if pins.Version != PinsVersion {
		return Cell{}, fmt.Errorf("architecture pins version must be %d, got %d", PinsVersion, pins.Version)
	}
	if status.Architecture != definition.Identity() {
		return Cell{}, fmt.Errorf("live status attests %+v, not architecture %s", status.Architecture, definition.Ref())
	}
	if err := definition.ValidateStatus(status); err != nil {
		return Cell{}, err
	}
	level := map[projectarch.InteractionMode]Level{
		projectarch.InteractionPredicates: LevelPredicates,
		projectarch.InteractionTextPolicy: LevelTextPolicy,
		projectarch.InteractionComposed:   LevelComposed,
		projectarch.InteractionNative:     LevelNative,
	}[definition.Interaction.Mode]
	if level == "" {
		return Cell{}, fmt.Errorf("architecture %s is not an F52 P/T/C/N definition", definition.Ref())
	}
	protocol := ProtocolIdentity{
		Transport: status.Interaction.Transport,
		Version:   status.Interaction.ProtocolVersion,
		Handoff:   ActHandoff(status.Interaction.ActHandoff),
	}
	identity := InteractionIdentity{
		Evidence: EvidenceIdentity{
			Source:       EvidenceSource(status.Interaction.Evidence),
			Capabilities: status.Interaction.EvidenceCapabilities,
		},
		Protocol: protocol, DecisionTimeoutMS: status.Interaction.DecisionTimeoutMS,
		NativeSuppressionContract: status.Interaction.NativeSuppression,
		Control:                   status.Interaction.Control,
	}
	if level == LevelTextPolicy || level == LevelComposed {
		identity.PolicyName = status.Policies.Interaction
		identity.Model = pins.InteractionPolicy
		identity.InstructionRevision = pins.InstructionRevision
		identity.Evidence.Recognizer = pins.InteractionRecognizer
	}
	cell := Cell{
		Name: name, Availability: AvailabilityRunnable,
		Architecture: Architecture{
			Definition: definition, Level: level, RuntimeBinding: status.Binding,
			Profile: status.Profile, Foreground: pins.Foreground,
			Perception: pins.Perception, Speech: pins.Speech, Slow: pins.Slow,
			SpeakerIdentity: pins.SpeakerIdentity,
			VisualNarrator:  pins.VisualNarrator,
			Ownership:       status.Ownership, Capabilities: status.Stack,
			Policies: status.Policies, Observers: status.Observers,
			Interaction: identity, ToolAuthority: status.Tools,
		},
	}
	if err := cell.Architecture.Validate(); err != nil {
		return Cell{}, err
	}
	if err := cell.ValidateObserved(status); err != nil {
		return Cell{}, err
	}
	return cell, nil
}

// ReadCell strictly reads one authored architecture cell.
func ReadCell(path string) (Cell, error) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return Cell{}, err
	}
	var cell Cell
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cell); err != nil {
		return Cell{}, fmt.Errorf("decode architecture cell: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Cell{}, errors.New("decode architecture cell: trailing JSON value")
		}
		return Cell{}, fmt.Errorf("decode architecture cell trailing content: %w", err)
	}
	if err := cell.Architecture.Validate(); err != nil {
		return Cell{}, err
	}
	if err := cell.Execution.Validate(); err != nil {
		return Cell{}, fmt.Errorf("architecture cell execution: %w", err)
	}
	return cell, nil
}

// WriteCell writes one authored cell as an intermediate, reviewable artifact.
func WriteCell(path string, cell Cell) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("an architecture cell needs an output path")
	}
	if err := cell.Architecture.Validate(); err != nil {
		return err
	}
	if err := cell.Execution.Validate(); err != nil {
		return fmt.Errorf("architecture cell execution: %w", err)
	}
	payload, err := json.MarshalIndent(cell, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(payload, '\n'), 0o644)
}

// WriteManifest writes a validated experiment manifest.
func WriteManifest(path string, manifest Manifest) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("an architecture manifest needs an output path")
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(payload, '\n'), 0o644)
}
