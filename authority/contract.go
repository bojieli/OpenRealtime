// Package authority defines provider-independent evidence selected by trusted
// deployment policy. Model output may refer to this evidence, but it cannot
// manufacture it through a cognition output port.
package authority

import "github.com/bojieli/OpenRealtime/element"

var candidateType = element.Stream(element.Named("authority.Candidate"))

// CandidateType is the exact graph type emitted by activation policies and
// consumed by authority joins.
func CandidateType() element.Type { return candidateType.Clone() }

// Candidate records the observation basis selected by one activation
// decision. It deliberately contains identities and revisions rather than an
// authority label: effect admission derives actual authority from the
// deployment-owned canonical trajectory.
//
// ActivationItemID is the cognition.Generate trigger emitted by the same
// policy decision. ActivationCauseItemID is the committed-observation outcome
// that released the decision. ObservationItemID is the canonical trajectory
// item the policy selected as the basis, while ObservationTriggerItemID is the
// ingress/perception envelope recorded by that item.
type Candidate struct {
	RunID                    string `json:"run_id"`
	SessionID                string `json:"session_id"`
	ActivationItemID         string `json:"activation_item_id"`
	ActivationCauseItemID    string `json:"activation_cause_item_id"`
	ObservationItemID        string `json:"observation_item_id"`
	ObservationTriggerItemID string `json:"observation_trigger_item_id"`
	SourceRevision           uint64 `json:"source_revision"`
	ContextVersion           uint64 `json:"context_version"`
	ContextEnvelopeItemID    string `json:"context_envelope_item_id"`
	ContextTailItem          string `json:"context_tail_item"`
}
