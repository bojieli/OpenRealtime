package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const maximumTemporalEvidenceRequirements = 32

var (
	temporalEvidenceAdmittedType = element.Stream(element.Named("policy.AdmittedTemporalEvidence"))
	temporalEvidenceOutcomeType  = element.Event(element.Named("policy.TemporalEvidenceAdmissionOutcome"))
)

func AdmittedTemporalEvidenceType() element.Type { return temporalEvidenceAdmittedType.Clone() }
func TemporalEvidenceOutcomeType() element.Type  { return temporalEvidenceOutcomeType.Clone() }

type TemporalEvidenceAdmissionMode string

const (
	TemporalEvidenceAdmissionImmediate   TemporalEvidenceAdmissionMode = "immediate"
	TemporalEvidenceAdmissionAfterIntent TemporalEvidenceAdmissionMode = "after_intent"
)

// TemporalEvidenceSourceSet selects how after-intent requirements are fixed.
// Explicit uses the graph-authored Required pairs. ObservedBeforeIntent freezes
// the distinct observer/source pairs already present before the newest final
// user intent in the trigger's exact canonical prefix. In neither mode can a
// pre-intent observation satisfy the resulting freshness requirement.
type TemporalEvidenceSourceSet string

const (
	TemporalEvidenceSourceSetExplicit             TemporalEvidenceSourceSet = "explicit"
	TemporalEvidenceSourceSetObservedBeforeIntent TemporalEvidenceSourceSet = "observed_before_intent"
)

type TemporalEvidenceRequirement struct {
	Observer string `json:"observer"`
	Source   string `json:"source"`
}

// TemporalEvidenceAdmissionConfig makes the admission timing and source set a
// values-plane choice. Immediate validates and forwards the exact committed
// trigger. AfterIntent requires fresh causal evidence for either graph-authored
// pairs or the pairs discovered before the durable intent.
type TemporalEvidenceAdmissionConfig struct {
	Mode      TemporalEvidenceAdmissionMode `json:"mode"`
	SourceSet TemporalEvidenceSourceSet     `json:"source_set,omitempty"`
	Required  []TemporalEvidenceRequirement `json:"required,omitempty"`
}

// TemporalEvidenceItemIdentity is a content-free identity for one exact item
// in Prefix. StoreVersion is its one-based position in that immutable prefix.
type TemporalEvidenceItemIdentity struct {
	TrajectoryItemID string               `json:"trajectory_item_id"`
	TriggerItemID    string               `json:"trigger_item_id"`
	StoreVersion     uint64               `json:"store_version"`
	SourceRevision   uint64               `json:"source_revision"`
	OccurredNS       uint64               `json:"occurred_ns"`
	Authority        trajectory.Authority `json:"authority"`
	Observer         string               `json:"observer,omitempty"`
	Source           string               `json:"source,omitempty"`
}

// AdmittedTemporalEvidence binds a validated trigger commit to the exact
// prefix inspected by the policy. After-intent admissions additionally name
// the selected durable user intent and one distinct qualifying observation for
// every frozen requirement. The payload contains identities, never observed
// text or media bytes.
type AdmittedTemporalEvidence struct {
	Mode                   TemporalEvidenceAdmissionMode          `json:"mode"`
	SourceSet              TemporalEvidenceSourceSet              `json:"source_set,omitempty"`
	TriggerCommit          stateelements.ObservationCommitOutcome `json:"trigger_commit"`
	TriggerObservation     TemporalEvidenceItemIdentity           `json:"trigger_observation"`
	DurableIntent          *TemporalEvidenceItemIdentity          `json:"durable_intent,omitempty"`
	QualifyingObservations []TemporalEvidenceItemIdentity         `json:"qualifying_observations,omitempty"`
	Prefix                 trajectory.PrefixIdentity              `json:"prefix"`
}

type TemporalEvidenceAdmissionOutcomeKind string

const (
	TemporalEvidenceAdmissionAdmitted TemporalEvidenceAdmissionOutcomeKind = "admitted"
	TemporalEvidenceAdmissionRefused  TemporalEvidenceAdmissionOutcomeKind = "refused"
	TemporalEvidenceAdmissionIgnored  TemporalEvidenceAdmissionOutcomeKind = "ignored"
)

type TemporalEvidenceAdmissionOutcome struct {
	Kind                  TemporalEvidenceAdmissionOutcomeKind `json:"kind"`
	Mode                  TemporalEvidenceAdmissionMode        `json:"mode"`
	TriggerItemID         string                               `json:"trigger_item_id,omitempty"`
	TrajectoryItemID      string                               `json:"trajectory_item_id,omitempty"`
	DurableIntentItemID   string                               `json:"durable_intent_item_id,omitempty"`
	RequiredObservations  int                                  `json:"required_observations,omitempty"`
	QualifiedObservations int                                  `json:"qualified_observations,omitempty"`
	Code                  string                               `json:"code,omitempty"`
	Message               string                               `json:"message,omitempty"`
	FinishedNS            uint64                               `json:"finished_ns,omitempty"`
}

func (TemporalEvidenceAdmissionOutcome) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

var _ element.InspectionCauseProvider = TemporalEvidenceAdmissionOutcome{}

// TemporalEvidenceAdmissionDescriptor is a reusable causal-evidence gate. It
// consumes only store-attested observation commit outcomes and revalidates the
// named prefix against the mounted canonical store before admitting anything.
func TemporalEvidenceAdmissionDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "policy.TemporalEvidenceAdmission",
		Revision:      1,
		Ports: []element.Port{
			{Name: "committed", Direction: element.Input, Type: observationCommitType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "admitted", Direction: element.Output, Type: temporalEvidenceAdmittedType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "outcome", Direction: element.Output, Type: temporalEvidenceOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers: []string{"committed"}, Outcomes: []string{"admitted", "outcome"},
			MaxConcurrency: 1,
		},
		ConfigSchema: "schema://openrealtime/policy/temporal-evidence-admission-config/v1",
		Dependencies: []element.Dependency{
			{Name: stateelements.TrajectoryStoreService},
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
		},
	}
}

func decodeTemporalEvidenceAdmissionConfig(
	source json.RawMessage,
) (TemporalEvidenceAdmissionConfig, error) {
	var config TemporalEvidenceAdmissionConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return TemporalEvidenceAdmissionConfig{}, err
	}
	switch config.Mode {
	case TemporalEvidenceAdmissionImmediate:
		if config.SourceSet != "" || len(config.Required) != 0 {
			return TemporalEvidenceAdmissionConfig{}, errors.New(
				"immediate temporal evidence admission cannot configure a source set or requirements")
		}
	case TemporalEvidenceAdmissionAfterIntent:
		if config.SourceSet == "" {
			config.SourceSet = TemporalEvidenceSourceSetExplicit
		}
		switch config.SourceSet {
		case TemporalEvidenceSourceSetExplicit:
			if len(config.Required) == 0 {
				return TemporalEvidenceAdmissionConfig{}, errors.New(
					"explicit after-intent admission requires at least one observer/source pair")
			}
		case TemporalEvidenceSourceSetObservedBeforeIntent:
			if len(config.Required) != 0 {
				return TemporalEvidenceAdmissionConfig{}, errors.New(
					"observed_before_intent derives its source set and cannot configure explicit requirements")
			}
		default:
			return TemporalEvidenceAdmissionConfig{}, fmt.Errorf(
				"unknown temporal evidence source set %q", config.SourceSet)
		}
	default:
		return TemporalEvidenceAdmissionConfig{}, fmt.Errorf(
			"unknown temporal evidence admission mode %q", config.Mode)
	}
	if len(config.Required) > maximumTemporalEvidenceRequirements {
		return TemporalEvidenceAdmissionConfig{}, fmt.Errorf(
			"temporal evidence requirements exceed %d pairs", maximumTemporalEvidenceRequirements)
	}
	seen := make(map[TemporalEvidenceRequirement]struct{}, len(config.Required))
	for index, requirement := range config.Required {
		if err := validatePolicyIdentifier(
			fmt.Sprintf("temporal evidence requirement %d observer", index), requirement.Observer, true,
		); err != nil {
			return TemporalEvidenceAdmissionConfig{}, err
		}
		if err := validatePolicyIdentifier(
			fmt.Sprintf("temporal evidence requirement %d source", index), requirement.Source, true,
		); err != nil {
			return TemporalEvidenceAdmissionConfig{}, err
		}
		if _, duplicate := seen[requirement]; duplicate {
			return TemporalEvidenceAdmissionConfig{}, fmt.Errorf(
				"temporal evidence observer/source pair %q/%q is duplicated",
				requirement.Observer, requirement.Source)
		}
		seen[requirement] = struct{}{}
	}
	config.Required = slices.Clone(config.Required)
	return config, nil
}
