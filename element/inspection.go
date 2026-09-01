package element

import (
	"fmt"
	"slices"
)

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
	DecisionPrepare, DecisionPromote, DecisionProposal, DecisionProvenance,
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
