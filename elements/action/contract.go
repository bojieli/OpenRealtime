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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	authoritycontract "github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	"github.com/bojieli/OpenRealtime/elements/internal/factoryprofile"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// DispatchContext is immutable graph authority attached by action.Dispatch to
// the context passed to every deployment-selected Dispatcher. It lets a
// dispatcher correlate provider-local call IDs without falling back to a
// process- or session-global CallID namespace.
type DispatchContext struct {
	SessionID           string
	RunID               string
	CommitmentID        string
	CanonicalCallItemID string
}

type dispatchContextKey struct{}

func withDispatchContext(ctx context.Context, scope DispatchContext) context.Context {
	return context.WithValue(ctx, dispatchContextKey{}, scope)
}

// DispatchContextFromContext returns the exact graph authority attached at the
// irreversible dispatch boundary. A false result means the caller did not come
// through action.Dispatch and must not be treated as graph-authorized.
func DispatchContextFromContext(ctx context.Context) (DispatchContext, bool) {
	if ctx == nil {
		return DispatchContext{}, false
	}
	scope, ok := ctx.Value(dispatchContextKey{}).(DispatchContext)
	return scope, ok
}

const (
	ToolRegistryService         = "action.tool.registries"
	ConfirmationRegistryService = "action.confirmation.providers"
	TargetRegistryService       = "action.target.registries"
	LedgerRegistryService       = "action.ledger.registries"
	TrajectoryStoreService      = "action.trajectory.store"
)

var (
	proposalType          = element.Stream(element.Named("tool.Proposal"))
	candidateType         = authoritycontract.CandidateType()
	provenanceType        = element.Stream(element.Named("authority.Provenance"))
	admittedType          = element.Stream(element.Named("tool.AdmittedProposal"))
	declaredType          = element.Stream(element.Named("tool.DeclaredAction"))
	confirmedType         = element.Stream(element.Named("tool.ConfirmedAction"))
	authorizedType        = element.Stream(element.Named("tool.AuthorizedAction"))
	executableType        = element.Stream(element.Named("tool.ExecutableAction"))
	committedType         = element.Stream(element.Named("tool.CommittedAction"))
	resultType            = element.Stream(element.Named("tool.ExecutionResult"))
	interruptType         = element.Interrupt(element.Named("tool.CallID"))
	outcomeType           = element.Stream(element.Named("action.Outcome"))
	transitionType        = element.Stream(element.Named("action.LedgerTransition"))
	auditType             = element.Stream(element.Named("action.AuditRecord"))
	resolutionType        = element.State(element.Named("action.Resolution"))
	canonicalActionType   = element.Stream(element.Named("tool.CanonicalAction"))
	canonicalResultType   = element.Stream(element.Named("tool.CanonicalResult"))
	preEffectTerminalType = element.Stream(element.Named("action.PreEffectTerminal"))
	appendType            = stateelements.AppendType()
	commitType            = stateelements.CommitType()
	rejectionType         = stateelements.RejectionType()
	snapshotType          = stateelements.SnapshotType()
)

func ProposalType() element.Type          { return proposalType.Clone() }
func CandidateType() element.Type         { return candidateType.Clone() }
func ProvenanceType() element.Type        { return provenanceType.Clone() }
func AdmittedType() element.Type          { return admittedType.Clone() }
func DeclaredType() element.Type          { return declaredType.Clone() }
func ConfirmedType() element.Type         { return confirmedType.Clone() }
func AuthorizedType() element.Type        { return authorizedType.Clone() }
func ExecutableType() element.Type        { return executableType.Clone() }
func CommittedType() element.Type         { return committedType.Clone() }
func ResultType() element.Type            { return resultType.Clone() }
func InterruptType() element.Type         { return interruptType.Clone() }
func OutcomeType() element.Type           { return outcomeType.Clone() }
func TransitionType() element.Type        { return transitionType.Clone() }
func AuditType() element.Type             { return auditType.Clone() }
func ResolutionType() element.Type        { return resolutionType.Clone() }
func CanonicalActionType() element.Type   { return canonicalActionType.Clone() }
func CanonicalResultType() element.Type   { return canonicalResultType.Clone() }
func PreEffectTerminalType() element.Type { return preEffectTerminalType.Clone() }

// Provenance identifies canonical trajectory evidence for one proposal. The
// authority is deliberately not supplied here: ProposalAdmission derives it
// from the deployment-owned trajectory, so observed screen text cannot label
// itself as user-authoritative.
type Provenance struct {
	CallID                   string              `json:"call_id"`
	ProposalItemID           string              `json:"proposal_item_id"`
	CandidateItemID          string              `json:"candidate_item_id"`
	ResultItemID             string              `json:"result_item_id"`
	ModelRunID               string              `json:"model_run_id"`
	SessionID                string              `json:"session_id"`
	ActivationItemID         string              `json:"activation_item_id"`
	ActivationCauseItemID    string              `json:"activation_cause_item_id"`
	ObservationItemID        string              `json:"observation_item_id"`
	ObservationTriggerItemID string              `json:"observation_trigger_item_id"`
	SourceRevision           uint64              `json:"source_revision"`
	ContextVersion           uint64              `json:"context_version"`
	ContextEnvelopeItemID    string              `json:"context_envelope_item_id"`
	ContextTailItem          string              `json:"context_tail_item"`
	ProviderReference        string              `json:"provider_reference"`
	ModelResultDigest        string              `json:"model_result_digest"`
	ModelProducer            trajectory.Producer `json:"model_producer"`
}

// Interrupt is addressed by an explicit call ID and the containing envelope's
// non-empty session and cognition-run identities. Cancel and timeout use
// distinct graph ports even though they share this payload, keeping their
// policy paths independently inspectable and preventing a provider-local call
// ID from canceling another session or invocation.
type Interrupt struct {
	CallID string `json:"call_id,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type AdmittedProposal struct {
	Proposal                 cognitionelements.ToolProposal `json:"proposal"`
	ProposalItemID           string                         `json:"proposal_item_id"`
	CandidateItemID          string                         `json:"candidate_item_id"`
	ResultItemID             string                         `json:"result_item_id"`
	ModelRunID               string                         `json:"model_run_id"`
	SessionID                string                         `json:"session_id"`
	ActivationItemID         string                         `json:"activation_item_id"`
	ActivationCauseItemID    string                         `json:"activation_cause_item_id"`
	Authority                trajectory.Authority           `json:"authority"`
	AuthorityItemID          string                         `json:"authority_item_id"`
	ObservationTriggerItemID string                         `json:"observation_trigger_item_id"`
	SourceRevision           uint64                         `json:"source_revision"`
	ContextVersion           uint64                         `json:"context_version"`
	ContextEnvelopeItemID    string                         `json:"context_envelope_item_id"`
	ContextTailItem          string                         `json:"context_tail_item"`
	ProviderReference        string                         `json:"provider_reference"`
	ModelResultDigest        string                         `json:"model_result_digest"`
	ModelProducer            trajectory.Producer            `json:"model_producer"`
}

// ArgumentRewrite records one schema-authorized deterministic transformation.
// The value itself is not duplicated in audit evidence; the before/after
// argument digests bind it without exposing potentially sensitive identifiers.
type ArgumentRewrite struct {
	Argument   string `json:"argument"`
	Normalizer string `json:"normalizer"`
}

// ArgumentNormalization is evidence that an explicit graph element derived
// EffectiveCall from the exact admitted model proposal under the immutable
// tool declaration. Omission means the model proposal is used byte-for-byte.
type ArgumentNormalization struct {
	Rewrites                 []ArgumentRewrite `json:"rewrites"`
	OriginalArgumentsDigest  string            `json:"original_arguments_digest"`
	EffectiveArgumentsDigest string            `json:"effective_arguments_digest"`
	RegistryReference        string            `json:"registry_reference"`
	RegistryDigest           string            `json:"registry_digest"`
	DeclarationDigest        string            `json:"declaration_digest"`
}

type DeclaredAction struct {
	Admitted           AdmittedProposal       `json:"admitted"`
	EffectiveCall      *trajectory.ToolCall   `json:"effective_call,omitempty"`
	Normalization      *ArgumentNormalization `json:"argument_normalization,omitempty"`
	Confirmation       legacyaction.Confirm   `json:"confirmation"`
	Target             string                 `json:"target,omitempty"`
	Background         bool                   `json:"background,omitempty"`
	RegistryReference  string                 `json:"registry_reference"`
	RegistryDigest     string                 `json:"registry_digest"`
	DeclarationDigest  string                 `json:"declaration_digest"`
	DispatcherIdentity string                 `json:"dispatcher_identity,omitempty"`
}

type ConfirmedAction struct {
	Declared               DeclaredAction `json:"declared"`
	ConfirmationNeeded     bool           `json:"confirmation_needed"`
	ProviderReference      string         `json:"provider_reference,omitempty"`
	ProviderIdentity       string         `json:"provider_identity,omitempty"`
	ConfirmationCapability string         `json:"confirmation_capability,omitempty"`
}

type AuthorizedAction struct {
	Confirmed       ConfirmedAction `json:"confirmed"`
	TargetReference string          `json:"target_reference"`
	TargetDigest    string          `json:"target_digest"`
}

// CanonicalAction proves that an authorized action was promoted from the
// exact canonical model proposal into a trajectory tool_call safe point. The
// distinct port type prevents TargetFence output from bypassing this commit.
type CanonicalAction struct {
	Authorized       AuthorizedAction `json:"authorized"`
	ProposalItemID   string           `json:"proposal_item_id"`
	TrajectoryItemID string           `json:"trajectory_item_id"`
	StoreVersion     uint64           `json:"store_version"`
}

// ExecutableAction is the typed authority accepted by Dispatch. Capability is
// an HMAC minted by LedgerCommit from deployment-owned state; runtime payload
// forgery therefore cannot bypass the graph's nominal type boundary.
type ExecutableAction struct {
	Canonical       CanonicalAction `json:"canonical"`
	LedgerReference string          `json:"ledger_reference"`
	LedgerIdentity  string          `json:"ledger_identity"`
	CommitmentID    string          `json:"commitment_id"`
	Capability      string          `json:"capability"`
}

type CommittedAction struct {
	Executable ExecutableAction `json:"executable"`
	CrossedNS  uint64           `json:"crossed_ns"`
}

type ExecutionResult struct {
	Executable       ExecutableAction      `json:"executable"`
	ResultCapability string                `json:"result_capability"`
	CompletionOrigin CompletionOrigin      `json:"completion_origin"`
	CallID           string                `json:"call_id"`
	Name             string                `json:"name"`
	CommitmentID     string                `json:"commitment_id"`
	Result           trajectory.ToolResult `json:"result"`
	CrossedNS        uint64                `json:"crossed_ns"`
	FinishedNS       uint64                `json:"finished_ns"`
}

// CompletionOrigin is ledger-authenticated dispatch evidence. Returned means
// the selected dispatcher supplied the terminal ToolResult; DispatcherError
// means Dispatch synthesized the ToolResult from the dispatcher's returned
// error (including cancellation after the external boundary was crossed).
// Profiles that accept client tool results can therefore require an explicit
// graph ingress attestation only for bytes claimed to have been returned.
type CompletionOrigin string

const (
	CompletionReturned        CompletionOrigin = "returned"
	CompletionDispatcherError CompletionOrigin = "dispatcher_error"
)

// CanonicalResult attests the trajectory safe point containing a dispatch
// result. Consumers can sample the matching snapshot without racing a merely
// completed external call that has not entered canonical state yet.
type CanonicalResult struct {
	Execution        ExecutionResult `json:"execution"`
	TrajectoryItemID string          `json:"trajectory_item_id"`
	StoreVersion     uint64          `json:"store_version"`
}

// PreEffectTerminal is a typed, non-result terminal decision made before an
// external effect crosses the dispatch boundary. Consumers must not fabricate
// a ToolResult for this decision: no tool ran and no visual consequence will
// arrive. The containing envelope supplies the session and cognition-run
// identities; CallID addresses the exact proposal within that run.
type PreEffectTerminal struct {
	Kind   PreEffectTerminalKind `json:"kind"`
	CallID string                `json:"call_id"`
}

type PreEffectTerminalKind string

const (
	PreEffectRepetitionSuppressed PreEffectTerminalKind = "repetition_suppressed"
	PreEffectToolPolicySuppressed PreEffectTerminalKind = "tool_policy_suppressed"
)

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
	Kind                      OutcomeKind `json:"kind"`
	Stage                     string      `json:"stage"`
	Operation                 string      `json:"operation"`
	CallID                    string      `json:"call_id,omitempty"`
	Code                      string      `json:"code,omitempty"`
	Message                   string      `json:"message,omitempty"`
	Crossed                   bool        `json:"crossed,omitempty"`
	StartedNS                 uint64      `json:"started_ns,omitempty"`
	FinishedNS                uint64      `json:"finished_ns,omitempty"`
	ResultDigest              string      `json:"result_digest,omitempty"`
	IngressItemID             string      `json:"ingress_item_id,omitempty"`
	AcceptedItemID            string      `json:"accepted_item_id,omitempty"`
	CanonicalEnvelopeItemID   string      `json:"canonical_envelope_item_id,omitempty"`
	CanonicalTrajectoryItemID string      `json:"canonical_trajectory_item_id,omitempty"`
	CanonicalStoreVersion     uint64      `json:"canonical_store_version,omitempty"`
}

// InspectionDecision deliberately projects only closed categorical outcome
// evidence. Call/session/run identities, codes, messages, digests, provider
// values, and payload-derived timestamps never enter live inspection.
func (outcome Outcome) InspectionDecision() element.InspectionDecision {
	return element.InspectionDecision{
		Kind:      element.InspectionDecisionKind(outcome.Kind),
		Operation: element.InspectionDecisionOperation(outcome.Operation),
		Crossed:   outcome.Crossed,
	}
}

func (Outcome) InspectionCause() element.InspectionCauseKind {
	return element.CausePolicy
}

var _ element.InspectionDecisionProvider = Outcome{}
var _ element.InspectionCauseProvider = Outcome{}

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
	ProviderReference  string               `json:"provider_reference"`
	ModelResultDigest  string               `json:"model_result_digest"`
	ModelProducer      trajectory.Producer  `json:"model_producer"`
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

// ActionArbiterConfig bounds the observation-scoped race between independently
// admitted action lanes. MaxPending counts observation groups that have not yet
// produced a complete admitted action; TerminalMemory retains completed group
// identities so delayed loser output cannot reopen an effect path.
type ActionArbiterConfig struct {
	MaxPending     int `json:"max_pending,omitempty"`
	TerminalMemory int `json:"terminal_memory,omitempty"`
}

type ToolLookupConfig struct {
	Registry string `json:"registry"`
}

type NormalizeArgumentsConfig struct{}

// ToolAdmissionConfig is a static, graph-authored action-surface policy over
// deployment-declared tool names. An empty allow list admits every name not in
// DeniedTools; a non-empty allow list admits only its members, still subject to
// the deny list. This boundary controls which proposals may become effects; it
// does not mutate the deployment registry or the provider-facing declaration.
type ToolAdmissionConfig struct {
	AllowedTools []string `json:"allowed_tools,omitempty"`
	DeniedTools  []string `json:"denied_tools,omitempty"`
}

type RepetitionAdmissionMode string

const (
	RepetitionAdmissionAllow                               RepetitionAdmissionMode = "allow"
	RepetitionAdmissionAtMostOnceAfterSuccessPerUserIntent RepetitionAdmissionMode = "at_most_once_after_success_per_user_intent"
)

// RepetitionAdmissionConfig selects whether semantically identical successful
// effects remain admissible under one durable user-authority observation.
// Compatibility defaults to allow. RepeatableTools are explicit exceptions;
// MaxTrackedEffects bounds retained successful-effect identities.
type RepetitionAdmissionConfig struct {
	Mode              RepetitionAdmissionMode `json:"mode,omitempty"`
	RepeatableTools   []string                `json:"repeatable_tools,omitempty"`
	MaxTrackedEffects int                     `json:"max_tracked_effects,omitempty"`
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

type ProvenanceJoinConfig struct {
	MaxPending     int `json:"max_pending,omitempty"`
	TerminalMemory int `json:"terminal_memory,omitempty"`
}

type TrajectoryCommitConfig struct {
	MaxPending     int `json:"max_pending,omitempty"`
	TerminalMemory int `json:"terminal_memory,omitempty"`
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

func decodeActionArbiterConfig(source json.RawMessage) (ActionArbiterConfig, error) {
	var config ActionArbiterConfig
	err := decodeBoundedConfig(source, &config, &config.MaxPending, &config.TerminalMemory)
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

func decodeNormalizeArgumentsConfig(source json.RawMessage) (NormalizeArgumentsConfig, error) {
	var config NormalizeArgumentsConfig
	err := elementconfig.Decode(source, &config)
	return config, err
}

func decodeToolAdmissionConfig(source json.RawMessage) (ToolAdmissionConfig, error) {
	var config ToolAdmissionConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return ToolAdmissionConfig{}, err
	}
	validate := func(label string, names []string) (map[string]struct{}, error) {
		if len(names) > 4096 {
			return nil, fmt.Errorf("%s tools must contain at most 4096 names", label)
		}
		seen := make(map[string]struct{}, len(names))
		for index, name := range names {
			if strings.TrimSpace(name) != name || name == "" || len(name) > 1024 {
				return nil, fmt.Errorf(
					"%s tool %d must be a non-empty canonical name", label, index)
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, fmt.Errorf("%s tool %q is duplicated", label, name)
			}
			seen[name] = struct{}{}
		}
		return seen, nil
	}
	allowed, err := validate("allowed", config.AllowedTools)
	if err != nil {
		return ToolAdmissionConfig{}, err
	}
	denied, err := validate("denied", config.DeniedTools)
	if err != nil {
		return ToolAdmissionConfig{}, err
	}
	for name := range allowed {
		if _, overlap := denied[name]; overlap {
			return ToolAdmissionConfig{}, fmt.Errorf(
				"tool %q cannot be both allowed and denied", name)
		}
	}
	config.AllowedTools = slices.Clone(config.AllowedTools)
	config.DeniedTools = slices.Clone(config.DeniedTools)
	return config, nil
}

func decodeRepetitionAdmissionConfig(source json.RawMessage) (RepetitionAdmissionConfig, error) {
	config := RepetitionAdmissionConfig{
		Mode: RepetitionAdmissionAllow, MaxTrackedEffects: 512,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return RepetitionAdmissionConfig{}, err
	}
	switch config.Mode {
	case RepetitionAdmissionAllow, RepetitionAdmissionAtMostOnceAfterSuccessPerUserIntent:
	default:
		return RepetitionAdmissionConfig{}, fmt.Errorf("unknown repetition admission mode %q", config.Mode)
	}
	if config.MaxTrackedEffects < 1 || config.MaxTrackedEffects > 4096 {
		return RepetitionAdmissionConfig{}, fmt.Errorf(
			"max tracked effects must be between 1 and 4096, got %d", config.MaxTrackedEffects)
	}
	if len(config.RepeatableTools) > 4096 {
		return RepetitionAdmissionConfig{}, errors.New("repeatable tools must contain at most 4096 names")
	}
	seen := make(map[string]struct{}, len(config.RepeatableTools))
	for index, name := range config.RepeatableTools {
		if strings.TrimSpace(name) != name || name == "" || len(name) > 1024 {
			return RepetitionAdmissionConfig{}, fmt.Errorf(
				"repeatable tool %d must be a non-empty canonical name", index)
		}
		if _, duplicate := seen[name]; duplicate {
			return RepetitionAdmissionConfig{}, fmt.Errorf("repeatable tool %q is duplicated", name)
		}
		seen[name] = struct{}{}
	}
	config.RepeatableTools = slices.Clone(config.RepeatableTools)
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

func decodeProvenanceJoinConfig(source json.RawMessage) (ProvenanceJoinConfig, error) {
	var config ProvenanceJoinConfig
	if err := decodeBoundedConfig(source, &config, &config.MaxPending, &config.TerminalMemory); err != nil {
		return ProvenanceJoinConfig{}, err
	}
	return config, nil
}

func decodeTrajectoryCommitConfig(source json.RawMessage) (TrajectoryCommitConfig, error) {
	var config TrajectoryCommitConfig
	if err := decodeBoundedConfig(source, &config, &config.MaxPending, &config.TerminalMemory); err != nil {
		return TrajectoryCommitConfig{}, err
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
		FormatVersion: element.DescriptorFormatVersion, Name: "authority.ProposalAdmission", Revision: 2,
		Ports: []element.Port{
			port("proposal", element.Input, proposalType, 32), port("provenance", element.Input, provenanceType, 32),
			port("cancel", element.Input, interruptType, 16), port("timeout", element.Input, interruptType, 16),
			port("admitted", element.Output, admittedType, 32), port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType),
		},
		Reaction: element.Reaction{Triggers: []string{"proposal", "provenance"}, Interrupts: []string{"cancel", "timeout"},
			Outcomes: []string{"admitted", "outcome", "resolved"}, MaxConcurrency: 1},
		StateSchema:  "schema://openrealtime/authority/proposal-admission-state/v2",
		ConfigSchema: "schema://openrealtime/authority/proposal-admission-config/v1",
		Dependencies: []element.Dependency{{Name: TrajectoryStoreService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
	}
}

// ActionArbiterDescriptor selects the first completely admitted action for one
// canonical observation across two or more independently activated cognition
// lanes. Candidate evidence lets it cancel a slower lane even before that lane
// emits a proposal; only the selected admitted action can reach tool lookup.
func ActionArbiterDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "authority.ActionArbiter", Revision: 2,
		Ports: []element.Port{
			{Name: "candidate", Direction: element.Input, Type: candidateType,
				Cardinality: element.Variadic, Required: true, MinConnections: 2, DefaultDepth: 16},
			{Name: "proposal", Direction: element.Input, Type: admittedType,
				Cardinality: element.Variadic, Required: true, MinConnections: 2, DefaultDepth: 16},
			{Name: "result", Direction: element.Input, Type: interactionelements.SafeModelResultType(),
				Cardinality: element.Variadic, Required: true, MinConnections: 2, DefaultDepth: 16},
			port("selected", element.Output, admittedType, 32),
			port("cancel_upstream", element.Output, cognitionelements.CancelType(), 16),
			port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType),
		},
		Reaction: element.Reaction{
			Triggers:       []string{"candidate", "proposal", "result"},
			Outcomes:       []string{"selected", "cancel_upstream", "outcome", "resolved"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/authority/action-arbiter-state/v1",
		ConfigSchema: "schema://openrealtime/authority/action-arbiter-config/v1",
		Dependencies: []element.Dependency{{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
		Effects: []element.Effect{{Name: "authority.action-arbitration.memory", Reversible: true}},
	}
}

// ProvenanceJoinDescriptor derives authority provenance from an exact model
// proposal/result join. It never accepts a caller-authored authority claim.
func ProvenanceJoinDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "authority.ProvenanceJoin", Revision: 2,
		Ports: []element.Port{
			port("candidate", element.Input, candidateType, 16),
			port("proposal", element.Input, proposalType, 32),
			port("result", element.Input, interactionelements.SafeModelResultType(), 16),
			port("cancel", element.Input, interruptType, 16), port("timeout", element.Input, interruptType, 16),
			port("provenance", element.Output, provenanceType, 32), port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType),
		},
		Reaction: element.Reaction{Triggers: []string{"candidate", "proposal", "result"}, Interrupts: []string{"cancel", "timeout"},
			Outcomes: []string{"provenance", "outcome", "resolved"}, MaxConcurrency: 1},
		StateSchema:  "schema://openrealtime/authority/provenance-join-state/v1",
		ConfigSchema: "schema://openrealtime/authority/provenance-join-config/v1",
		Dependencies: []element.Dependency{{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
		Effects: []element.Effect{{Name: "authority.provenance.pending", Reversible: true}},
	}
}

func ToolLookupDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.ToolLookup", Revision: 2,
		Ports: []element.Port{port("proposal", element.Input, admittedType, 32), port("declared", element.Output, declaredType, 32),
			port("outcome", element.Output, outcomeType, 32), statePort("resolved", element.Output, resolutionType)},
		Reaction:     element.Reaction{Triggers: []string{"proposal"}, Outcomes: []string{"declared", "outcome", "resolved"}, MaxConcurrency: 1},
		ConfigSchema: "schema://openrealtime/action/tool-lookup-config/v1",
		Dependencies: []element.Dependency{{Name: ToolRegistryService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
	}
}

// NormalizeArgumentsDescriptor is deliberately a separate graph element from
// lookup, admission, and dispatch. A composition opts into schema-directed
// normalization by inserting it; omitting it preserves the provider's exact
// proposal. It cannot grant action authority or dispatch an effect.
func NormalizeArgumentsDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.NormalizeArguments", Revision: 1,
		Ports: []element.Port{
			port("action", element.Input, declaredType, 32), port("normalized", element.Output, declaredType, 32),
			port("outcome", element.Output, outcomeType, 32), statePort("resolved", element.Output, resolutionType),
		},
		Reaction:     element.Reaction{Triggers: []string{"action"}, Outcomes: []string{"normalized", "outcome", "resolved"}, MaxConcurrency: 1},
		ConfigSchema: "schema://openrealtime/action/normalize-arguments-config/v1",
		Dependencies: []element.Dependency{{Name: ToolRegistryService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
	}
}

// ToolAdmission is an optional, explicit policy boundary over exact declared
// tool names. A suppressed proposal terminates before Dispatch and emits a
// typed terminal so an upstream activation policy can release or replay newer
// observations without fabricating a ToolResult for an effect that never ran.
func ToolAdmissionDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.ToolAdmission", Revision: 1,
		Ports: []element.Port{
			port("action", element.Input, declaredType, 32),
			port("admitted", element.Output, declaredType, 32),
			port("terminal", element.Output, preEffectTerminalType, 32),
			port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType),
		},
		Reaction: element.Reaction{
			Triggers:       []string{"action"},
			Outcomes:       []string{"admitted", "terminal", "outcome", "resolved"},
			MaxConcurrency: 1,
		},
		ConfigSchema: "schema://openrealtime/action/tool-admission-config/v1",
		Dependencies: []element.Dependency{{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
	}
}

// RepetitionAdmission is an optional graph policy boundary between argument
// normalization and confirmation. In protected mode it suppresses a semantic
// effect only after the same effect has a successful canonical result under
// the same durable user intent. The result is recorded before being forwarded,
// serializing replay memory ahead of downstream consequence handling.
func RepetitionAdmissionDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.RepetitionAdmission", Revision: 1,
		Ports: []element.Port{
			port("action", element.Input, declaredType, 32),
			port("result", element.Input, canonicalResultType, 32),
			port("admitted", element.Output, declaredType, 32),
			port("canonical_result", element.Output, canonicalResultType, 32),
			port("terminal", element.Output, preEffectTerminalType, 32),
			port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType),
		},
		Reaction: element.Reaction{
			Triggers:       []string{"action", "result"},
			Outcomes:       []string{"admitted", "canonical_result", "terminal", "outcome", "resolved"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/action/repetition-admission-state/v1",
		ConfigSchema: "schema://openrealtime/action/repetition-admission-config/v1",
		Dependencies: []element.Dependency{{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
		Effects: []element.Effect{{Name: "action.successful-effect-repetition.memory", Reversible: true}},
	}
}

func ConfirmationDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "authority.Confirmation", Revision: 3,
		Ports: []element.Port{port("action", element.Input, declaredType, 32), port("cancel", element.Input, interruptType, 16),
			port("timeout", element.Input, interruptType, 16), port("confirmed", element.Output, confirmedType, 32),
			port("outcome", element.Output, outcomeType, 32), statePort("resolved", element.Output, resolutionType)},
		Reaction: element.Reaction{Triggers: []string{"action"}, Interrupts: []string{"cancel", "timeout"},
			Outcomes: []string{"confirmed", "outcome", "resolved"}, MaxConcurrency: 1},
		StateSchema:  "schema://openrealtime/authority/confirmation-state/v2",
		ConfigSchema: "schema://openrealtime/authority/confirmation-config/v1",
		Dependencies: []element.Dependency{{Name: ConfirmationRegistryService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
		Effects: []element.Effect{{Name: "authority.confirmation.session", Reversible: true}},
	}
}

func TargetFenceDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "authority.TargetFence", Revision: 2,
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
		FormatVersion: element.DescriptorFormatVersion, Name: "action.LedgerCommit", Revision: 2,
		Ports: []element.Port{port("action", element.Input, canonicalActionType, 32), port("cancel", element.Input, interruptType, 16),
			port("timeout", element.Input, interruptType, 16), port("executable", element.Output, executableType, 32),
			port("transition", element.Output, transitionType, 32), port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType)},
		Reaction: element.Reaction{Triggers: []string{"action"}, Interrupts: []string{"cancel", "timeout"},
			Outcomes: []string{"executable", "transition", "outcome", "resolved"}, MaxConcurrency: 1},
		StateSchema:  "schema://openrealtime/action/ledger-commit-state/v2",
		ConfigSchema: "schema://openrealtime/action/ledger-commit-config/v1",
		Dependencies: []element.Dependency{{Name: LedgerRegistryService}, {Name: TrajectoryStoreService},
			{Name: ToolRegistryService}, {Name: TargetRegistryService}, {Name: ConfirmationRegistryService},
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName}},
	}
}

// AuthorizedCallCommitDescriptor is the mandatory canonical promotion between
// target authorization and irreversibility-ledger admission.
func AuthorizedCallCommitDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.AuthorizedCallCommit", Revision: 4,
		Ports: []element.Port{
			port("action", element.Input, authorizedType, 32), port("context", element.Input, snapshotType, 1),
			port("committed", element.Input, commitType, 16), port("rejected", element.Input, rejectionType, 16),
			port("cancel", element.Input, interruptType, 16), port("timeout", element.Input, interruptType, 16),
			port("append", element.Output, appendType, 32), port("canonical", element.Output, canonicalActionType, 32),
			port("outcome", element.Output, outcomeType, 32), statePort("resolved", element.Output, resolutionType),
		},
		Reaction: element.Reaction{
			Triggers:   []string{"action", "context", "committed", "rejected"},
			Interrupts: []string{"cancel", "timeout"}, Outcomes: []string{"append", "canonical", "outcome", "resolved"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/action/authorized-call-commit-state/v1",
		ConfigSchema: "schema://openrealtime/action/authorized-call-commit-config/v1",
		Dependencies: []element.Dependency{{Name: ToolRegistryService}, {Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName}},
		Effects: []element.Effect{{Name: "action.authorized-call.pending", Reversible: true}},
	}
}

// ToolResultCommitDescriptor closes the canonical call/result lifecycle after
// dispatch. External completion is not model context until this commit lands.
func ToolResultCommitDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.ToolResultCommit", Revision: 4,
		Ports: []element.Port{
			port("result", element.Input, resultType, 32), port("context", element.Input, snapshotType, 1),
			port("committed", element.Input, commitType, 16), port("rejected", element.Input, rejectionType, 16),
			port("cancel", element.Input, interruptType, 16), port("timeout", element.Input, interruptType, 16),
			port("append", element.Output, appendType, 32), port("canonical", element.Output, canonicalResultType, 32),
			port("outcome", element.Output, outcomeType, 32), statePort("resolved", element.Output, resolutionType),
		},
		Reaction: element.Reaction{
			Triggers:   []string{"result", "context", "committed", "rejected"},
			Interrupts: []string{"cancel", "timeout"}, Outcomes: []string{"append", "canonical", "outcome", "resolved"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/action/tool-result-commit-state/v1",
		ConfigSchema: "schema://openrealtime/action/tool-result-commit-config/v1",
		Dependencies: []element.Dependency{{Name: LedgerRegistryService}, {Name: TrajectoryStoreService},
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName}},
		Effects: []element.Effect{{Name: "action.tool-result.pending", Reversible: true}},
	}
}

func DispatchDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "action.Dispatch", Revision: 3,
		Ports: []element.Port{port("execute", element.Input, executableType, 32), port("cancel", element.Input, interruptType, 16),
			port("timeout", element.Input, interruptType, 16), port("committed", element.Output, committedType, 32),
			port("result", element.Output, resultType, 32), port("transition", element.Output, transitionType, 32),
			port("audit", element.Output, auditType, 32), port("outcome", element.Output, outcomeType, 32),
			statePort("resolved", element.Output, resolutionType)},
		Reaction: element.Reaction{Triggers: []string{"execute"}, Interrupts: []string{"cancel", "timeout"},
			Outcomes: []string{"committed", "result", "transition", "audit", "outcome", "resolved"}, MaxConcurrency: 1},
		StateSchema:  "schema://openrealtime/action/dispatch-state/v2",
		ConfigSchema: "schema://openrealtime/action/dispatch-config/v1",
		Dependencies: []element.Dependency{{Name: ToolRegistryService}, {Name: LedgerRegistryService},
			{Name: TargetRegistryService}, {Name: ConfirmationRegistryService},
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName}},
		Effects: []element.Effect{{Name: "action.external.dispatch", External: true, Authority: "tool.ExecutableAction"}},
	}
}

func Descriptors() []element.Descriptor {
	return []element.Descriptor{
		ProvenanceJoinDescriptor(), ProposalAdmissionDescriptor(), ActionArbiterDescriptor(), ToolLookupDescriptor(), NormalizeArgumentsDescriptor(), ToolAdmissionDescriptor(), RepetitionAdmissionDescriptor(), ConfirmationDescriptor(),
		TargetFenceDescriptor(), AuthorizedCallCommitDescriptor(), ClientToolResultDescriptor(), ClientToolResultJoinDescriptor(),
		LedgerCommitDescriptor(), DispatchDescriptor(),
		ToolResultCommitDescriptor(),
	}
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
	registrations, err := FactoryRegistrations()
	if err != nil {
		return err
	}
	for _, registration := range registrations {
		if err := registry.RegisterFactory(registration); err != nil {
			return err
		}
	}
	return nil
}

func FactoryRegistrations() ([]graphruntime.FactoryRegistration, error) {
	entries := make([]factoryprofile.Entry, 0, len(Descriptors()))
	for _, factory := range []element.Factory{
		provenanceJoinFactory{}, proposalAdmissionFactory{}, actionArbiterFactory{}, toolLookupFactory{}, normalizeArgumentsFactory{}, toolAdmissionFactory{}, repetitionAdmissionFactory{}, confirmationFactory{},
		targetFenceFactory{}, authorizedCallCommitFactory{}, clientToolResultFactory{}, clientToolResultJoinFactory{},
		ledgerCommitFactory{}, dispatchFactory{},
		toolResultCommitFactory{},
	} {
		entries = append(entries, factoryprofile.Entry{Factory: factory})
	}
	return factoryprofile.Registrations(entries...)
}
