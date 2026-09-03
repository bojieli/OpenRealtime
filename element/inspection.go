package element

import (
	"fmt"
	"slices"
)

// InspectionCauseKind is the closed semantic vocabulary that a typed payload
// may contribute to payload-free causal inspection. Queue traversal, trigger
// timing, and authority decisions are already retained independently; these
// four values identify the semantic work that happened between those runtime
// boundaries without exposing request, provider, or model content.
type InspectionCauseKind string

const (
	CauseObservation   InspectionCauseKind = "observation"
	CauseStateRevision InspectionCauseKind = "state_revision"
	CausePolicy        InspectionCauseKind = "policy"
	CauseModelRun      InspectionCauseKind = "model_run"
)

var supportedInspectionCauseKinds = []InspectionCauseKind{
	CauseObservation, CauseStateRevision, CausePolicy, CauseModelRun,
}

// SupportedInspectionCauseKinds returns an independent copy of the exact
// semantic-causality vocabulary used by runtime and presentation validators.
func SupportedInspectionCauseKinds() []InspectionCauseKind {
	return slices.Clone(supportedInspectionCauseKinds)
}

// Validate rejects free-form classifications that could smuggle payload or
// provider data into an inspection snapshot.
func (kind InspectionCauseKind) Validate() error {
	if !slices.Contains(supportedInspectionCauseKinds, kind) {
		return fmt.Errorf("invalid inspection cause kind %q", kind)
	}
	return nil
}

// InspectionCauseProvider is implemented only by typed payloads that can
// project one closed semantic classification. A payload that has no such
// projection remains unclassified; the runtime rejects invalid projections.
type InspectionCauseProvider interface {
	InspectionCause() InspectionCauseKind
}

// InspectionDecisionKind is a closed, payload-free authority outcome class.
// It intentionally carries no call, run, session, provider, or message value.
type InspectionDecisionKind string

const (
	DecisionSucceeded InspectionDecisionKind = "succeeded"
	DecisionRejected  InspectionDecisionKind = "rejected"
	DecisionDenied    InspectionDecisionKind = "denied"
	DecisionCanceled  InspectionDecisionKind = "canceled"
	DecisionTimedOut  InspectionDecisionKind = "timed_out"
	DecisionFailed    InspectionDecisionKind = "failed"
	DecisionIgnored   InspectionDecisionKind = "ignored"
)

// InspectionDecisionOperation is the closed action-policy operation vocabulary
// admitted to live inspection and trace artifacts. Keeping this vocabulary
// closed prevents an element from smuggling request content into evidence.
type InspectionDecisionOperation string

const (
	DecisionAccepted         InspectionDecisionOperation = "accepted"
	DecisionAction           InspectionDecisionOperation = "action"
	DecisionAdmit            InspectionDecisionOperation = "admit"
	DecisionAlreadyCommitted InspectionDecisionOperation = "already_committed"
	DecisionAttest           InspectionDecisionOperation = "attest"
	DecisionAuthorize        InspectionDecisionOperation = "authorize"
	DecisionCancel           InspectionDecisionOperation = "cancel"
	DecisionCandidate        InspectionDecisionOperation = "candidate"
	DecisionCommit           InspectionDecisionOperation = "commit"
	DecisionCommitted        InspectionDecisionOperation = "committed"
	DecisionComplete         InspectionDecisionOperation = "complete"
	DecisionConfirm          InspectionDecisionOperation = "confirm"
	DecisionContext          InspectionDecisionOperation = "context"
	DecisionExecute          InspectionDecisionOperation = "execute"
	DecisionFence            InspectionDecisionOperation = "fence"
	DecisionJoin             InspectionDecisionOperation = "join"
	DecisionLookup           InspectionDecisionOperation = "lookup"
	DecisionNormalize        InspectionDecisionOperation = "normalize"
	DecisionPrepare          InspectionDecisionOperation = "prepare"
	DecisionPromote          InspectionDecisionOperation = "promote"
	DecisionProposal         InspectionDecisionOperation = "proposal"
	DecisionProvenance       InspectionDecisionOperation = "provenance"
	DecisionQueue            InspectionDecisionOperation = "queue"
	DecisionResult           InspectionDecisionOperation = "result"
	DecisionRejection        InspectionDecisionOperation = "rejected"
	DecisionRetry            InspectionDecisionOperation = "retry"
	DecisionSelect           InspectionDecisionOperation = "select"
	DecisionTimeout          InspectionDecisionOperation = "timeout"
)

var supportedInspectionDecisionKinds = []InspectionDecisionKind{
	DecisionSucceeded, DecisionRejected, DecisionDenied, DecisionCanceled,
	DecisionTimedOut, DecisionFailed, DecisionIgnored,
}

var supportedInspectionDecisionOperations = []InspectionDecisionOperation{
	DecisionAccepted, DecisionAction, DecisionAdmit, DecisionAlreadyCommitted,
	DecisionAttest, DecisionAuthorize, DecisionCancel, DecisionCandidate,
	DecisionCommit, DecisionCommitted, DecisionComplete, DecisionConfirm,
	DecisionContext, DecisionExecute, DecisionFence, DecisionJoin, DecisionLookup,
	DecisionNormalize, DecisionPrepare, DecisionPromote, DecisionProposal, DecisionProvenance,
	DecisionQueue, DecisionResult, DecisionRejection, DecisionRetry, DecisionSelect,
	DecisionTimeout,
}

// SupportedInspectionDecisionKinds returns an independent copy of the exact
// closed outcome vocabulary used by runtime and presentation validators.
func SupportedInspectionDecisionKinds() []InspectionDecisionKind {
	return slices.Clone(supportedInspectionDecisionKinds)
}

// SupportedInspectionDecisionOperations returns an independent copy of the
// exact closed action-policy operation vocabulary.
func SupportedInspectionDecisionOperations() []InspectionDecisionOperation {
	return slices.Clone(supportedInspectionDecisionOperations)
}

// InspectionDecision is the complete authority-decision projection that an
// element may offer to the runtime. Crossed states whether an irreversible
// effect boundary had already been crossed when the outcome was produced.
type InspectionDecision struct {
	Kind      InspectionDecisionKind
	Operation InspectionDecisionOperation
	Crossed   bool
}

func (decision InspectionDecision) Validate() error {
	if !slices.Contains(supportedInspectionDecisionKinds, decision.Kind) {
		return fmt.Errorf("invalid inspection decision kind %q", decision.Kind)
	}
	if !slices.Contains(supportedInspectionDecisionOperations, decision.Operation) {
		return fmt.Errorf("invalid inspection decision operation %q", decision.Operation)
	}
	return nil
}

// InspectionDecisionProvider is implemented only by typed payloads that can
// project a closed, payload-free authority outcome. The runtime validates the
// returned value and ignores invalid or arbitrary payloads.
type InspectionDecisionProvider interface {
	InspectionDecision() InspectionDecision
}
