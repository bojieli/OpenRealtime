// Package action exposes graph-native, independently rewritable authority and
// tool-execution elements over the repository's proven action, computer-use,
// and trajectory contracts.
//
// Model output enters this package only as a proposal. A proposal must join
// canonical provenance, resolve against a deployment-owned declaration,
// satisfy confirmation, pass a target fence, and enter the irreversibility
// ledger before the sole external-effect element can dispatch it. No one node
// owns all of those decisions.
package action

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	ToolRegistryService         = "action.tool.registries"
	ConfirmationRegistryService = "action.confirmation.providers"
	TargetRegistryService       = "action.target.registries"
	LedgerRegistryService       = "action.ledger.registries"
	TrajectoryStoreService      = "action.trajectory.store"
)

var (
	proposalType   = element.Stream(element.Named("tool.Proposal"))
	provenanceType = element.Stream(element.Named("authority.Provenance"))
	admittedType   = element.Stream(element.Named("tool.AdmittedProposal"))
	declaredType   = element.Stream(element.Named("tool.DeclaredAction"))
	confirmedType  = element.Stream(element.Named("tool.ConfirmedAction"))
	authorizedType = element.Stream(element.Named("tool.AuthorizedAction"))
	executableType = element.Stream(element.Named("tool.ExecutableAction"))
	committedType  = element.Stream(element.Named("tool.CommittedAction"))
	resultType     = element.Stream(element.Named("tool.ExecutionResult"))
	interruptType  = element.Interrupt(element.Named("tool.CallID"))
	outcomeType    = element.Stream(element.Named("action.Outcome"))
	transitionType = element.Stream(element.Named("action.LedgerTransition"))
	auditType      = element.Stream(element.Named("action.AuditRecord"))
	resolutionType = element.State(element.Named("action.Resolution"))
)

func ProposalType() element.Type   { return proposalType.Clone() }
func ProvenanceType() element.Type { return provenanceType.Clone() }
func AdmittedType() element.Type   { return admittedType.Clone() }
func DeclaredType() element.Type   { return declaredType.Clone() }
func ConfirmedType() element.Type  { return confirmedType.Clone() }
func AuthorizedType() element.Type { return authorizedType.Clone() }
func ExecutableType() element.Type { return executableType.Clone() }
func CommittedType() element.Type  { return committedType.Clone() }
func ResultType() element.Type     { return resultType.Clone() }
func InterruptType() element.Type  { return interruptType.Clone() }
func OutcomeType() element.Type    { return outcomeType.Clone() }
func TransitionType() element.Type { return transitionType.Clone() }
func AuditType() element.Type      { return auditType.Clone() }
func ResolutionType() element.Type { return resolutionType.Clone() }

// Provenance identifies canonical trajectory evidence for one proposal. The
// authority is deliberately not supplied here: ProposalAdmission derives it
// from the deployment-owned trajectory, so observed screen text cannot label
// itself as user-authoritative.
type Provenance struct {
	CallID          string `json:"call_id"`
	ProposalItemID  string `json:"proposal_item_id"`
	ModelRunID      string `json:"model_run_id"`
	SessionID       string `json:"session_id,omitempty"`
	TrajectoryItem  string `json:"trajectory_item"`
	SourceRevision  uint64 `json:"source_revision"`
	ContextVersion  uint64 `json:"context_version"`
	ContextTailItem string `json:"context_tail_item"`
}

// Interrupt is addressed by call ID. Cancel and timeout use distinct graph
// ports even though they share this payload, keeping their policy paths
// independently inspectable.
type Interrupt struct {
	CallID string `json:"call_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type AdmittedProposal struct {
	Proposal        cognitionelements.ToolProposal `json:"proposal"`
	ProposalItemID  string                         `json:"proposal_item_id"`
	ModelRunID      string                         `json:"model_run_id"`
	SessionID       string                         `json:"session_id,omitempty"`
	Authority       trajectory.Authority           `json:"authority"`
	AuthorityItemID string                         `json:"authority_item_id"`
	SourceRevision  uint64                         `json:"source_revision,omitempty"`
	ContextVersion  uint64                         `json:"context_version"`
	ContextTailItem string                         `json:"context_tail_item"`
}

type DeclaredAction struct {
	Admitted           AdmittedProposal     `json:"admitted"`
	Confirmation       legacyaction.Confirm `json:"confirmation"`
	Target             string               `json:"target,omitempty"`
	Background         bool                 `json:"background,omitempty"`
	RegistryReference  string               `json:"registry_reference"`
	RegistryDigest     string               `json:"registry_digest"`
	DeclarationDigest  string               `json:"declaration_digest"`
	DispatcherIdentity string               `json:"dispatcher_identity,omitempty"`
}

type ConfirmedAction struct {
	Declared           DeclaredAction `json:"declared"`
	ConfirmationNeeded bool           `json:"confirmation_needed"`
	ProviderReference  string         `json:"provider_reference,omitempty"`
	ProviderIdentity   string         `json:"provider_identity,omitempty"`
}

type AuthorizedAction struct {
	Confirmed       ConfirmedAction `json:"confirmed"`
	TargetReference string          `json:"target_reference"`
	TargetDigest    string          `json:"target_digest"`
}

// ExecutableAction is the typed authority accepted by Dispatch. Capability is
// an HMAC minted by LedgerCommit from deployment-owned state; runtime payload
// forgery therefore cannot bypass the graph's nominal type boundary.
type ExecutableAction struct {
	Authorized      AuthorizedAction `json:"authorized"`
	LedgerReference string           `json:"ledger_reference"`
	LedgerIdentity  string           `json:"ledger_identity"`
	CommitmentID    string           `json:"commitment_id"`
	Capability      string           `json:"capability"`
}

type CommittedAction struct {
	Executable ExecutableAction `json:"executable"`
	CrossedNS  uint64           `json:"crossed_ns"`
}

type ExecutionResult struct {
	CallID       string                `json:"call_id"`
	Name         string                `json:"name"`
	CommitmentID string                `json:"commitment_id"`
	Result       trajectory.ToolResult `json:"result"`
	CrossedNS    uint64                `json:"crossed_ns"`
	FinishedNS   uint64                `json:"finished_ns"`
}

type OutcomeKind string

const (
	OutcomeSucceeded OutcomeKind = "succeeded"
	OutcomeRejected  OutcomeKind = "rejected"
	OutcomeDenied    OutcomeKind = "denied"
	OutcomeCanceled  OutcomeKind = "canceled"
	OutcomeTimedOut  OutcomeKind = "timed_out"
	OutcomeFailed    OutcomeKind = "failed"
	OutcomeIgnored   OutcomeKind = "ignored"
)

type Outcome struct {
	Kind       OutcomeKind `json:"kind"`
	Stage      string      `json:"stage"`
	Operation  string      `json:"operation"`
	CallID     string      `json:"call_id,omitempty"`
	Code       string      `json:"code,omitempty"`
	Message    string      `json:"message,omitempty"`
	Crossed    bool        `json:"crossed,omitempty"`
	StartedNS  uint64      `json:"started_ns,omitempty"`
	FinishedNS uint64      `json:"finished_ns,omitempty"`
}

type LedgerTransition struct {
	CallID       string             `json:"call_id"`
	CommitmentID string             `json:"commitment_id"`
	State        legacyaction.State `json:"state"`
	Crossed      bool               `json:"crossed"`
	Reason       string             `json:"reason,omitempty"`
	AtNS         uint64             `json:"at_ns"`
}

type AuditRecord struct {
	CallID             string               `json:"call_id"`
	Name               string               `json:"name"`
	ProposalItemID     string               `json:"proposal_item_id"`
	ModelRunID         string               `json:"model_run_id"`
	SessionID          string               `json:"session_id,omitempty"`
	CommitmentID       string               `json:"commitment_id"`
	Authority          trajectory.Authority `json:"authority"`
	AuthorityItemID    string               `json:"authority_item_id"`
	Confirmation       legacyaction.Confirm `json:"confirmation"`
	Target             string               `json:"target,omitempty"`
	RegistryReference  string               `json:"registry_reference"`
	DeclarationDigest  string               `json:"declaration_digest"`
	DispatcherIdentity string               `json:"dispatcher_identity"`
	Crossed            bool                 `json:"crossed"`
	Executed           bool                 `json:"executed"`
	ResultError        string               `json:"result_error,omitempty"`
	StartedNS          uint64               `json:"started_ns"`
	FinishedNS         uint64               `json:"finished_ns"`
}

// Resolution is live startup evidence. Digest binds immutable selected
// deployment data; ServiceRevision binds the deployment service generation.
type Resolution struct {
	Stage           string `json:"stage"`
	Reference       string `json:"reference"`
	Identity        string `json:"identity"`
	Digest          string `json:"digest,omitempty"`
	ServiceRevision uint64 `json:"service_revision"`
}

type ProposalAdmissionConfig struct {
	MaxPending  int  `json:"max_pending,omitempty"`
	AllowSystem bool `json:"allow_system,omitempty"`
}

type ToolLookupConfig struct {
	Registry string `json:"registry"`
}

type ConfirmationConfig struct {
	Provider   string `json:"provider"`
	MaxPending int    `json:"max_pending,omitempty"`
}

type TargetFenceConfig struct {
	Target string `json:"target"`
}

type LedgerCommitConfig struct {
	Ledger     string `json:"ledger"`
	MaxPending int    `json:"max_pending,omitempty"`
}

type DispatchConfig struct {
	Registry     string `json:"registry"`
	Ledger       string `json:"ledger"`
	MaxPending   int    `json:"max_pending,omitempty"`
	MaxCompleted int    `json:"max_completed,omitempty"`
}

func decodeBoundedConfig[T any](source json.RawMessage, destination *T, bounds ...*int) error {
	if err := elementconfig.Decode(source, destination); err != nil {
		return err
	}
	for _, value := range bounds {
		if *value == 0 {
			*value = 64
		}
		if *value < 1 || *value > 4096 {
			return fmt.Errorf("bounded state limit must be between 1 and 4096, got %d", *value)
		}
	}
	return nil
}

func decodeProposalAdmissionConfig(source json.RawMessage) (ProposalAdmissionConfig, error) {
	var config ProposalAdmissionConfig
	err := decodeBoundedConfig(source, &config, &config.MaxPending)
	return config, err
}

func decodeToolLookupConfig(source json.RawMessage) (ToolLookupConfig, error) {
	var config ToolLookupConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return config, err
	}
	config.Registry = strings.TrimSpace(config.Registry)
	if config.Registry == "" {
		return config, errors.New("tool lookup config requires a registry reference")
	}
	return config, nil
}

func decodeConfirmationConfig(source json.RawMessage) (ConfirmationConfig, error) {
	var config ConfirmationConfig
	if err := decodeBoundedConfig(source, &config, &config.MaxPending); err != nil {
		return config, err
	}
	config.Provider = strings.TrimSpace(config.Provider)
	if config.Provider == "" {
		return config, errors.New("confirmation config requires a provider reference")
	}
	return config, nil
}

func decodeTargetFenceConfig(source json.RawMessage) (TargetFenceConfig, error) {
	var config TargetFenceConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return config, err
	}
	config.Target = strings.TrimSpace(config.Target)
	if config.Target == "" {
		return config, errors.New("target fence config requires a target reference")
	}
	return config, nil
}

func decodeLedgerCommitConfig(source json.RawMessage) (LedgerCommitConfig, error) {
	var config LedgerCommitConfig
	if err := decodeBoundedConfig(source, &config, &config.MaxPending); err != nil {
		return config, err
	}
	config.Ledger = strings.TrimSpace(config.Ledger)
	if config.Ledger == "" {
		return config, errors.New("ledger commit config requires a ledger reference")
	}
	return config, nil
}

func decodeDispatchConfig(source json.RawMessage) (DispatchConfig, error) {
	var config DispatchConfig
	if err := decodeBoundedConfig(source, &config, &config.MaxPending, &config.MaxCompleted); err != nil {
		return config, err
	}
	config.Registry = strings.TrimSpace(config.Registry)
	config.Ledger = strings.TrimSpace(config.Ledger)
	if config.Registry == "" || config.Ledger == "" {
		return config, errors.New("dispatch config requires registry and ledger references")
	}
	return config, nil
}

func port(name string, direction element.Direction, value element.Type, depth int) element.Port {
	return element.Port{Name: name, Direction: direction, Type: value,
		Cardinality: element.One, Required: true, DefaultDepth: depth}
}

func statePort(name string, direction element.Direction, value element.Type) element.Port {
	result := port(name, direction, value, 1)
	if direction == element.Input {
		result.LossAllowed = true
	}
	return result
}

func ProposalAdmissionDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "authority.ProposalAdmission", Revision: 1,
		Ports: []element.Port{
			port("proposal", element.Input, proposalType, 32), port("provenance", element.Input, provenanceType, 32),
			port("cancel", element.Input, interruptType, 16), port("timeout", element.Input, interruptType, 16),
			port("admitted", element.Output, admittedType, 32), port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType),
		},
		Reaction: element.Reaction{Triggers: []string{"proposal", "provenance"}, Interrupts: []string{"cancel", "timeout"},
			Outcomes: []string{"admitted", "outcome", "resolved"}, MaxConcurrency: 1},
		StateSchema:  "schema://openrealtime/authority/proposal-admission-state/v1",
		ConfigSchema: "schema://openrealtime/authority/proposal-admission-config/v1",
		Dependencies: []element.Dependency{{Name: TrajectoryStoreService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
	}
}

func ToolLookupDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.ToolLookup", Revision: 1,
		Ports: []element.Port{port("proposal", element.Input, admittedType, 32), port("declared", element.Output, declaredType, 32),
			port("outcome", element.Output, outcomeType, 32), statePort("resolved", element.Output, resolutionType)},
		Reaction:     element.Reaction{Triggers: []string{"proposal"}, Outcomes: []string{"declared", "outcome", "resolved"}, MaxConcurrency: 1},
		ConfigSchema: "schema://openrealtime/action/tool-lookup-config/v1",
		Dependencies: []element.Dependency{{Name: ToolRegistryService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
	}
}

func ConfirmationDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "authority.Confirmation", Revision: 1,
		Ports: []element.Port{port("action", element.Input, declaredType, 32), port("cancel", element.Input, interruptType, 16),
			port("timeout", element.Input, interruptType, 16), port("confirmed", element.Output, confirmedType, 32),
			port("outcome", element.Output, outcomeType, 32), statePort("resolved", element.Output, resolutionType)},
		Reaction: element.Reaction{Triggers: []string{"action"}, Interrupts: []string{"cancel", "timeout"},
			Outcomes: []string{"confirmed", "outcome", "resolved"}, MaxConcurrency: 1},
		StateSchema:  "schema://openrealtime/authority/confirmation-state/v1",
		ConfigSchema: "schema://openrealtime/authority/confirmation-config/v1",
		Dependencies: []element.Dependency{{Name: ConfirmationRegistryService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
		Effects: []element.Effect{{Name: "authority.confirmation.session", Reversible: true}},
	}
}

func TargetFenceDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "authority.TargetFence", Revision: 1,
		Ports: []element.Port{port("action", element.Input, confirmedType, 32), port("authorized", element.Output, authorizedType, 32),
			port("outcome", element.Output, outcomeType, 32), statePort("resolved", element.Output, resolutionType)},
		Reaction:     element.Reaction{Triggers: []string{"action"}, Outcomes: []string{"authorized", "outcome", "resolved"}, MaxConcurrency: 1},
		ConfigSchema: "schema://openrealtime/authority/target-fence-config/v1",
		Dependencies: []element.Dependency{{Name: TargetRegistryService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
	}
}

func LedgerCommitDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.LedgerCommit", Revision: 1,
		Ports: []element.Port{port("action", element.Input, authorizedType, 32), port("cancel", element.Input, interruptType, 16),
			port("timeout", element.Input, interruptType, 16), port("executable", element.Output, executableType, 32),
			port("transition", element.Output, transitionType, 32), port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType)},
		Reaction: element.Reaction{Triggers: []string{"action"}, Interrupts: []string{"cancel", "timeout"},
			Outcomes: []string{"executable", "transition", "outcome", "resolved"}, MaxConcurrency: 1},
		StateSchema:  "schema://openrealtime/action/ledger-commit-state/v1",
		ConfigSchema: "schema://openrealtime/action/ledger-commit-config/v1",
		Dependencies: []element.Dependency{{Name: LedgerRegistryService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
	}
}

func DispatchDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.Dispatch", Revision: 1,
		Ports: []element.Port{port("execute", element.Input, executableType, 32), port("cancel", element.Input, interruptType, 16),
			port("timeout", element.Input, interruptType, 16), port("committed", element.Output, committedType, 32),
			port("result", element.Output, resultType, 32), port("transition", element.Output, transitionType, 32),
			port("audit", element.Output, auditType, 32), port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType)},
		Reaction: element.Reaction{Triggers: []string{"execute"}, Interrupts: []string{"cancel", "timeout"},
			Outcomes: []string{"committed", "result", "transition", "audit", "outcome", "resolved"}, MaxConcurrency: 1},
		StateSchema:  "schema://openrealtime/action/dispatch-state/v1",
		ConfigSchema: "schema://openrealtime/action/dispatch-config/v1",
		Dependencies: []element.Dependency{{Name: ToolRegistryService}, {Name: LedgerRegistryService},
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName}},
		Effects: []element.Effect{{Name: "action.external.dispatch", External: true, Authority: "tool.ExecutableAction"}},
	}
}

func Descriptors() []element.Descriptor {
	return []element.Descriptor{ProposalAdmissionDescriptor(), ToolLookupDescriptor(), ConfirmationDescriptor(),
		TargetFenceDescriptor(), LedgerCommitDescriptor(), DispatchDescriptor()}
}

func RegisterDescriptors(catalog *resolve.Catalog) error {
	if catalog == nil {
		return errors.New("register action descriptors: nil catalog")
	}
	for _, descriptor := range Descriptors() {
		if err := catalog.Register(descriptor); err != nil {
			return err
		}
	}
	return nil
}

func RegisterFactories(registry *graphruntime.Registry) error {
	if registry == nil {
		return errors.New("register action factories: nil registry")
	}
	for _, factory := range []element.Factory{proposalAdmissionFactory{}, toolLookupFactory{}, confirmationFactory{},
		targetFenceFactory{}, ledgerCommitFactory{}, dispatchFactory{}} {
		if err := registry.Register("", factory); err != nil {
			return err
		}
	}
	return nil
}
