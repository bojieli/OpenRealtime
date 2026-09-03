// Package policy contains graph-native control policies that translate typed
// observations and state into explicit activation decisions. A policy never
// owns model routing implicitly: developers instantiate and connect one policy
// node for each model activation path they want.
package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	generationRuntimeID             = "builtin://openrealtime/elements/policy.GenerateOnObservation"
	policyImplementationRevision    = "implementation:3"
	maximumPolicyIdentifierBytes    = 256
	maximumPolicyReasonBytes        = 1024
	maximumPolicyInstructionBytes   = 1 << 20
	maximumPolicyMetadataBytes      = 1 << 20
	maximumPolicyParametersBytes    = 1 << 20
	maximumPolicyEntries            = 4096
	maximumPolicyOutputTokens       = 1_000_000
	defaultGenerationPendingLimit   = 64
	defaultGenerationTerminalMemory = 512
	defaultGenerationCancelMemory   = 256
)

var (
	observationCommitType  = stateelements.ObservationCommitOutcomeType()
	generationCancelType   = element.Interrupt(element.Named("policy.GenerationAddress"))
	generationTriggerType  = cognitionelements.GenerateType()
	authorityCandidateType = authority.CandidateType()
	generationStateType    = element.State(element.Named("policy.GenerationState"))
	generationOutcomeType  = element.Event(element.Named("policy.GenerationOutcome"))
)

func GenerationCancelType() element.Type   { return generationCancelType.Clone() }
func AuthorityCandidateType() element.Type { return authorityCandidateType.Clone() }
func GenerationStateType() element.Type    { return generationStateType.Clone() }
func GenerationOutcomeType() element.Type  { return generationOutcomeType.Clone() }

// GenerateOnObservationDescriptor turns one successful observation commit
// into one model activation. The commit outcome carries the store-attested
// identity of the exact trajectory prefix containing the observation, so the
// policy never joins against or retains a second trajectory State payload.
func GenerateOnObservationDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "policy.GenerateOnObservation",
		Revision:      3,
		Ports: []element.Port{
			{Name: "committed", Direction: element.Input, Type: observationCommitType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "cancel", Direction: element.Input, Type: generationCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "trigger", Direction: element.Output, Type: generationTriggerType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			// Candidate is optional because policies may activate models whose
			// output is never connected to an external-effect path. When wired,
			// it makes the selected authority basis explicit and typed.
			{Name: "authority", Direction: element.Output, Type: authorityCandidateType,
				Cardinality: element.One, Required: false, DefaultDepth: 16},
			{Name: "state", Direction: element.Output, Type: generationStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "outcome", Direction: element.Output, Type: generationOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers: []string{"committed"}, Interrupts: []string{"cancel"},
			Outcomes: []string{"trigger", "authority", "state", "outcome"}, MaxConcurrency: 1,
			BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/policy/generate-on-observation-state/v2",
		ConfigSchema: "schema://openrealtime/policy/generate-on-observation-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName},
		},
		Effects: []element.Effect{{Name: "policy.activation.memory", Reversible: true}},
	}
}

// GenerateOnObservationConfig is a values-plane activation template. Role
// distinguishes independently connected fast, deliberative, or specialist
// nodes. SourceRevision and ExpectedContextVersion are derived from live
// commit evidence and therefore cannot be authored here.
type GenerateOnObservationConfig struct {
	Role       string                  `json:"role"`
	Invocation continuation.Invocation `json:"invocation"`
	// MaxPending is deprecated and ignored by descriptor revision 3. It remains
	// accepted, with its historical validation bound, so existing values files
	// do not fail merely because activation no longer has a pending context join.
	MaxPending     int `json:"max_pending,omitempty"`
	TerminalMemory int `json:"terminal_memory,omitempty"`
	CancelMemory   int `json:"cancel_memory,omitempty"`
}

type GenerationCancel struct {
	GenerationID string `json:"generation_id,omitempty"`
	StreamID     string `json:"stream_id,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type GenerationOutcomeKind string

const (
	GenerationEmitted  GenerationOutcomeKind = "emitted"
	GenerationCanceled GenerationOutcomeKind = "canceled"
	GenerationRefused  GenerationOutcomeKind = "refused"
	GenerationIgnored  GenerationOutcomeKind = "ignored"
)

type GenerationOutcome struct {
	Kind           GenerationOutcomeKind `json:"kind"`
	GenerationID   string                `json:"generation_id,omitempty"`
	Role           string                `json:"role"`
	StreamID       string                `json:"stream_id,omitempty"`
	SourceRevision uint64                `json:"source_revision,omitempty"`
	ContextVersion uint64                `json:"context_version,omitempty"`
	TriggerItemID  string                `json:"trigger_item_id,omitempty"`
	Code           string                `json:"code,omitempty"`
	Message        string                `json:"message,omitempty"`
	FinishedNS     uint64                `json:"finished_ns,omitempty"`
}

func (GenerationOutcome) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

type GenerationState struct {
	Role               string `json:"role"`
	ContextVersion     uint64 `json:"context_version"`
	Emitted            uint64 `json:"emitted"`
	Canceled           uint64 `json:"canceled"`
	Refused            uint64 `json:"refused"`
	Ignored            uint64 `json:"ignored"`
	TerminalMemory     int    `json:"terminal_memory"`
	CancellationMemory int    `json:"cancellation_memory"`
}

func (GenerationState) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

var (
	_ element.InspectionCauseProvider = GenerationOutcome{}
	_ element.InspectionCauseProvider = GenerationState{}
)

func decodeGenerateOnObservationConfig(source json.RawMessage) (GenerateOnObservationConfig, error) {
	config := GenerateOnObservationConfig{
		MaxPending:     defaultGenerationPendingLimit,
		TerminalMemory: defaultGenerationTerminalMemory,
		CancelMemory:   defaultGenerationCancelMemory,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return GenerateOnObservationConfig{}, err
	}
	if err := validatePolicyIdentifier("generation role", config.Role, true); err != nil {
		return GenerateOnObservationConfig{}, err
	}
	if config.MaxPending < 1 || config.MaxPending > 1_000_000 {
		return GenerateOnObservationConfig{}, errors.New("max_pending must be between 1 and 1000000")
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > 1_000_000 {
		return GenerateOnObservationConfig{}, errors.New("terminal_memory must be between 1 and 1000000")
	}
	if config.CancelMemory < 1 || config.CancelMemory > 1_000_000 {
		return GenerateOnObservationConfig{}, errors.New("cancel_memory must be between 1 and 1000000")
	}
	if err := validateInvocation(config.Invocation); err != nil {
		return GenerateOnObservationConfig{}, err
	}
	config.Invocation = cloneInvocation(config.Invocation)
	return config, nil
}

func validateInvocation(invocation continuation.Invocation) error {
	if strings.TrimSpace(invocation.Instruction) == "" {
		return errors.New("generation invocation instruction is required")
	}
	if len(invocation.Instruction) > maximumPolicyInstructionBytes || !utf8.ValidString(invocation.Instruction) {
		return fmt.Errorf("generation invocation instruction must be valid UTF-8 and no larger than %d bytes", maximumPolicyInstructionBytes)
	}
	if invocation.SourceRevision != 0 {
		return errors.New("generation invocation source_revision is derived from commit evidence")
	}
	if invocation.MaxOutputTokens < 0 || invocation.MaxOutputTokens > maximumPolicyOutputTokens {
		return fmt.Errorf("generation max_output_tokens must be between 0 and %d", maximumPolicyOutputTokens)
	}
	if invocation.Effort != "" {
		if parsed, err := continuation.ParseEffort(string(invocation.Effort)); err != nil || parsed != invocation.Effort {
			if err != nil {
				return err
			}
			return errors.New("generation effort must already be canonical")
		}
	}
	if len(invocation.Capabilities) > maximumPolicyEntries || len(invocation.Tools) > maximumPolicyEntries {
		return fmt.Errorf("generation invocation lists more than %d capabilities or tools", maximumPolicyEntries)
	}
	metadataBytes := 0
	for index, capability := range invocation.Capabilities {
		if err := validatePolicyIdentifier(fmt.Sprintf("capability %d name", index), capability.Name, true); err != nil {
			return err
		}
		if !utf8.ValidString(capability.Description) {
			return fmt.Errorf("capability %d description is not valid UTF-8", index)
		}
		metadataBytes += len(capability.Name) + len(capability.Description) + len(capability.ExecutionPhase)
	}
	for index, tool := range invocation.Tools {
		if err := validatePolicyIdentifier(fmt.Sprintf("tool %d name", index), tool.Name, true); err != nil {
			return err
		}
		if !utf8.ValidString(tool.Description) {
			return fmt.Errorf("tool %d description is not valid UTF-8", index)
		}
		if len(tool.Parameters) == 0 || !json.Valid(tool.Parameters) || len(tool.Parameters) > maximumPolicyParametersBytes {
			return fmt.Errorf("tool %d parameters must be valid JSON no larger than %d bytes", index, maximumPolicyParametersBytes)
		}
		metadataBytes += len(tool.Name) + len(tool.Description) + len(tool.Parameters)
	}
	if metadataBytes > maximumPolicyMetadataBytes {
		return fmt.Errorf("generation invocation metadata exceeds %d bytes", maximumPolicyMetadataBytes)
	}
	return nil
}

func cloneInvocation(invocation continuation.Invocation) continuation.Invocation {
	invocation.Capabilities = slices.Clone(invocation.Capabilities)
	invocation.Tools = slices.Clone(invocation.Tools)
	for index := range invocation.Tools {
		invocation.Tools[index].Parameters = slices.Clone(invocation.Tools[index].Parameters)
	}
	return invocation
}

func validatePolicyIdentifier(label, value string, required bool) error {
	if len(value) > maximumPolicyIdentifierBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, maximumPolicyIdentifierBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", label)
	}
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return fmt.Errorf("%s is required", label)
	}
	if value != trimmed {
		return fmt.Errorf("%s must not have surrounding whitespace", label)
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return fmt.Errorf("%s contains whitespace or control characters", label)
		}
	}
	return nil
}

func boundedPolicyReason(value string) string {
	if !utf8.ValidString(value) {
		return ""
	}
	if len(value) <= maximumPolicyReasonBytes {
		return value
	}
	const suffix = "…"
	value = value[:maximumPolicyReasonBytes-len(suffix)]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + suffix
}

func Descriptors() []element.Descriptor {
	return []element.Descriptor{
		GenerateOnObservationDescriptor(), SessionInvocationDescriptor(), SemanticAdmissionDescriptor(),
		TemporalEvidenceAdmissionDescriptor(), IntentSettlementDescriptor(),
	}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register policy descriptors: nil catalog")
	}
	for _, descriptor := range Descriptors() {
		if err := catalog.Register(descriptor); err != nil {
			return err
		}
	}
	return nil
}
