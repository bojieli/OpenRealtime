// Package graphnative binds the owned interaction-scenario suite to an exact
// graph-native Realtime adapter contract.
//
// This package does not score behavior and does not turn static compatibility
// into benchmark evidence. It identifies the external session seams exercised
// by each scenario, fingerprints that inventory, and rejects a selected graph
// adapter that cannot carry the suite before a listener or session is started.
package graphnative

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/bench/scenario"
	legacy "github.com/bojieli/OpenRealtime/binding"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

const (
	ContractFormatVersion uint64 = 1
	SuiteName                    = "scenario"
)

// Seam names one stable external session composition seam. Seams describe
// protocol-to-graph wiring only. The scenario checks remain the authority for
// whether a graph actually waits, interrupts, distinguishes speakers, uses a
// deadline, or says the right thing.
type Seam string

const (
	SeamSessionConfiguration Seam = "session-configuration"
	SeamContinuousAudio      Seam = "continuous-audio"
	SeamResponseLifecycle    Seam = "response-lifecycle"
	SeamFunctionTool         Seam = "function-tool-round-trip"
	SeamStillImageMessage    Seam = "still-image-message"
)

var coreOperations = []graphbinding.AdapterOperation{
	graphbinding.AdapterInputUpdate,
	graphbinding.AdapterInputAudio,
	graphbinding.AdapterOutputTurnBegin,
	graphbinding.AdapterOutputTurnEnd,
	graphbinding.AdapterOutputActivity,
	graphbinding.AdapterOutputTranscript,
	graphbinding.AdapterOutputSpeechBegin,
	graphbinding.AdapterOutputSpeechText,
	graphbinding.AdapterOutputSpeechAudio,
	graphbinding.AdapterOutputSpeechEnd,
	graphbinding.AdapterOutputFailed,
}

var toolOperations = []graphbinding.AdapterOperation{
	graphbinding.AdapterInputToolResult,
	graphbinding.AdapterInputCreateResponse,
	graphbinding.AdapterOutputToolCalls,
}

// StackRequirement is the graph adapter's minimum executable capability
// projection. These fields deliberately mirror only stable, inspectable
// binding capabilities. Interaction-policy behavior is established by live
// scenario results, never inferred from this declaration.
type StackRequirement struct {
	AudioInput     bool `json:"audio_input"`
	AudioOutput    bool `json:"audio_output"`
	VisualInput    bool `json:"visual_input,omitempty"`
	Transcription  bool `json:"transcription"`
	TurnGeneration bool `json:"turn_generation"`
	ConcurrentIO   bool `json:"concurrent_io,omitempty"`
	TextInjection  bool `json:"text_injection,omitempty"`
}

// CaseRequirement is the exact external composition surface for one owned
// scenario. Operations and seams are canonical, sorted, duplicate-free sets.
type CaseRequirement struct {
	Name         string                          `json:"name"`
	Seams        []Seam                          `json:"seams"`
	Operations   []graphbinding.AdapterOperation `json:"operations"`
	Capabilities StackRequirement                `json:"capabilities"`
}

// Contract is the immutable aggregate for a selected scenario slab. An empty
// selection passed to BuildContract means the reviewed twelve-case suite.
type Contract struct {
	FormatVersion uint64                          `json:"format_version"`
	Suite         string                          `json:"suite"`
	Cases         []CaseRequirement               `json:"cases"`
	Operations    []graphbinding.AdapterOperation `json:"operations"`
	Capabilities  StackRequirement                `json:"capabilities"`
	Fingerprint   string                          `json:"fingerprint"`
}

// BuildContract freezes the selected scenario composition contract. Names
// must be exact reviewed case names; whitespace normalization, duplicates, and
// unknown aliases fail closed instead of silently selecting a different slab.
func BuildContract(selected ...string) (Contract, error) {
	suite := scenario.Suite()
	wanted := make(map[string]struct{}, len(selected))
	for index, name := range selected {
		if name == "" || name != strings.TrimSpace(name) || strings.ContainsAny(name, "\x00\r\n") {
			return Contract{}, fmt.Errorf("scenario selection %d is not an exact canonical name", index)
		}
		if _, duplicate := wanted[name]; duplicate {
			return Contract{}, fmt.Errorf("scenario selection repeats %q", name)
		}
		wanted[name] = struct{}{}
	}

	result := Contract{FormatVersion: ContractFormatVersion, Suite: SuiteName}
	found := make(map[string]struct{}, len(wanted))
	reviewed := make(map[string]struct{}, len(suite))
	for _, item := range suite {
		if item.Name == "" || item.Name != strings.TrimSpace(item.Name) ||
			strings.ContainsAny(item.Name, "\x00\r\n") {
			return Contract{}, fmt.Errorf("reviewed scenario has non-canonical name %q", item.Name)
		}
		if _, duplicate := reviewed[item.Name]; duplicate {
			return Contract{}, fmt.Errorf("reviewed scenario suite repeats %q", item.Name)
		}
		reviewed[item.Name] = struct{}{}
		if len(wanted) > 0 {
			if _, selected := wanted[item.Name]; !selected {
				continue
			}
			found[item.Name] = struct{}{}
		}
		requirement := requirementFor(item)
		result.Cases = append(result.Cases, requirement)
		result.Operations = append(result.Operations, requirement.Operations...)
		result.Capabilities = result.Capabilities.merge(requirement.Capabilities)
	}
	for name := range wanted {
		if _, ok := found[name]; !ok {
			return Contract{}, fmt.Errorf("unknown scenario %q", name)
		}
	}
	if len(result.Cases) == 0 {
		return Contract{}, errors.New("scenario graph contract requires at least one case")
	}
	result.Operations = canonicalOperations(result.Operations)
	fingerprint, err := contractFingerprint(result)
	if err != nil {
		return Contract{}, err
	}
	result.Fingerprint = fingerprint
	return result, nil
}

func requirementFor(item scenario.Scenario) CaseRequirement {
	requirement := CaseRequirement{
		Name: item.Name,
		Seams: []Seam{
			SeamSessionConfiguration,
			SeamContinuousAudio,
			SeamResponseLifecycle,
		},
		Operations: slices.Clone(coreOperations),
		Capabilities: StackRequirement{
			AudioInput: true, AudioOutput: true, Transcription: true, TurnGeneration: true,
			// The harness continues streaming realtime PCM, including silence,
			// while an answer or silent action is in flight. Concurrent input and
			// output is therefore part of the ordinary path, not an optional
			// property of only the interruption cases.
			ConcurrentIO: true,
		},
	}
	if len(item.Tools) > 0 {
		requirement.Seams = append(requirement.Seams, SeamFunctionTool)
		requirement.Operations = append(requirement.Operations, toolOperations...)
	}
	if len(item.Sees) > 0 {
		requirement.Seams = append(requirement.Seams, SeamStillImageMessage)
		requirement.Operations = append(requirement.Operations,
			graphbinding.AdapterInputText,
			graphbinding.AdapterInputCreateResponse,
		)
		requirement.Capabilities.VisualInput = true
		requirement.Capabilities.TextInjection = true
	}
	sort.Slice(requirement.Seams, func(left, right int) bool {
		return requirement.Seams[left] < requirement.Seams[right]
	})
	requirement.Operations = canonicalOperations(requirement.Operations)
	return requirement
}

func (requirement StackRequirement) merge(other StackRequirement) StackRequirement {
	return StackRequirement{
		AudioInput:     requirement.AudioInput || other.AudioInput,
		AudioOutput:    requirement.AudioOutput || other.AudioOutput,
		VisualInput:    requirement.VisualInput || other.VisualInput,
		Transcription:  requirement.Transcription || other.Transcription,
		TurnGeneration: requirement.TurnGeneration || other.TurnGeneration,
		ConcurrentIO:   requirement.ConcurrentIO || other.ConcurrentIO,
		TextInjection:  requirement.TextInjection || other.TextInjection,
	}
}

func canonicalOperations(source []graphbinding.AdapterOperation) []graphbinding.AdapterOperation {
	result := slices.Clone(source)
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return slices.Compact(result)
}

// Clone returns an independently mutable copy.
func (contract Contract) Clone() Contract {
	result := contract
	result.Operations = slices.Clone(contract.Operations)
	result.Cases = make([]CaseRequirement, len(contract.Cases))
	for index, item := range contract.Cases {
		result.Cases[index] = item
		result.Cases[index].Seams = slices.Clone(item.Seams)
		result.Cases[index].Operations = slices.Clone(item.Operations)
	}
	return result
}

// Validate proves that a contract is exactly the canonical contract derivable
// from the repository-owned suite and that its fingerprint is current.
func (contract Contract) Validate() error {
	if contract.FormatVersion != ContractFormatVersion || contract.Suite != SuiteName {
		return fmt.Errorf("scenario graph contract has format %d suite %q", contract.FormatVersion, contract.Suite)
	}
	names := make([]string, len(contract.Cases))
	for index, item := range contract.Cases {
		names[index] = item.Name
	}
	want, err := BuildContract(names...)
	if err != nil {
		return fmt.Errorf("scenario graph contract cases: %w", err)
	}
	if !reflect.DeepEqual(contract, want) {
		return errors.New("scenario graph contract is not canonical or its fingerprint is stale")
	}
	return nil
}

// Marshal emits deterministic, newline-terminated JSON suitable for a
// preregistered launch artifact.
func (contract Contract) Marshal() ([]byte, error) {
	if err := contract.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.MarshalIndent(contract, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode scenario graph contract: %w", err)
	}
	return append(payload, '\n'), nil
}

func contractFingerprint(contract Contract) (string, error) {
	contract.Fingerprint = ""
	payload, err := json.Marshal(contract)
	if err != nil {
		return "", fmt.Errorf("fingerprint scenario graph contract: %w", err)
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// ValidateProfile proves that one frozen adapter can carry this scenario slab
// over the exact selected Graph IR. It establishes launch compatibility only;
// live benchmark results remain required for behavioral or latency claims.
func (contract Contract) ValidateProfile(profile graphbinding.SessionAdapterProfile, graph ir.Graph) error {
	if err := contract.Validate(); err != nil {
		return err
	}
	if err := profile.ValidateGraph(graph); err != nil {
		return fmt.Errorf("scenario graph adapter profile: %w", err)
	}
	available := make(map[graphbinding.AdapterOperation]struct{}, len(profile.Boundaries))
	for _, boundary := range profile.Boundaries {
		available[boundary.Operation] = struct{}{}
	}
	var missing []string
	for _, operation := range contract.Operations {
		if _, ok := available[operation]; !ok {
			missing = append(missing, string(operation))
		}
	}
	missing = append(missing, contract.Capabilities.missing(profile.Capabilities)...)
	sort.Strings(missing)
	if len(missing) > 0 {
		return fmt.Errorf("scenario graph adapter is missing required seams: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (requirement StackRequirement) missing(capabilities legacy.Capabilities) []string {
	stack := capabilities.Stack
	checks := []struct {
		name     string
		required bool
		present  bool
	}{
		{"capability.audio_input", requirement.AudioInput, stack.AudioInput},
		{"capability.audio_output", requirement.AudioOutput, stack.AudioOutput},
		{"capability.visual_input", requirement.VisualInput, stack.VisualInput},
		{"capability.transcription", requirement.Transcription, stack.Transcription},
		{"capability.turn_generation", requirement.TurnGeneration, stack.TurnGeneration},
		{"capability.concurrent_io", requirement.ConcurrentIO, stack.ConcurrentIO},
		{"capability.text_injection", requirement.TextInjection, stack.TextInjection},
	}
	var result []string
	for _, check := range checks {
		if check.required && !check.present {
			result = append(result, check.name)
		}
	}
	return result
}
