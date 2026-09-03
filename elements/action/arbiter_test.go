package action

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const actionArbiterGraph = `graph action_arbiter_test {
    authority.ActionArbiter :: arbiter;
    input reflex_candidate = arbiter.candidate;
    input planner_candidate = arbiter.candidate;
    input reflex_proposal = arbiter.proposal;
    input planner_proposal = arbiter.proposal;
    input reflex_result = arbiter.result;
    input planner_result = arbiter.result;
    output selected = arbiter.selected;
    output cancel_upstream = arbiter.cancel_upstream;
    output outcome = arbiter.outcome;
    output resolved = arbiter.resolved;
}`

func TestActionOutcomeRefusesAnUninspectableDecisionVocabulary(t *testing.T) {
	err := publishOutcome(context.Background(), emitter{}, nil, element.Envelope{}, Outcome{
		Kind: OutcomeSucceeded, Operation: "private-payload",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid inspection decision operation") {
		t.Fatalf("uninspectable action outcome error = %v", err)
	}
}

func TestActionArbiterSelectsFirstCompleteLaneAndCancelsLateCandidate(t *testing.T) {
	mounted, done, cancel := mountActionArbiter(t, `{}`)
	defer stopMounted(t, mounted, done, cancel)

	reflexCandidate, reflexProposal, _ := actionArbitrationEvidence(
		"reflex-run", "reflex-call", "session-race", "screen-trigger",
	)
	plannerCandidate, plannerProposal, _ := actionArbitrationEvidence(
		"planner-run", "planner-call", "session-race", "screen-trigger",
	)
	send(t, mustIngressAction(t, mounted, "reflex_candidate"), reflexCandidate)
	send(t, mustIngressAction(t, mounted, "reflex_proposal"), reflexProposal)

	selectedEnvelope := receive(t, mustEgressAction(t, mounted, "selected"))
	selected, ok := selectedEnvelope.Payload.(AdmittedProposal)
	if !ok || selected.ModelRunID != "reflex-run" || selected.Proposal.Call.CallID != "reflex-call" {
		t.Fatalf("selected action = %#v envelope=%+v", selectedEnvelope.Payload, selectedEnvelope)
	}
	outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome)
	if outcome.Kind != OutcomeSucceeded || outcome.Operation != "select" || outcome.Code != "first_complete" {
		t.Fatalf("selection outcome = %+v", outcome)
	}
	var decision *inspect.AuthorityDecisionLive
	deadline := time.Now().Add(time.Second)
	for decision == nil && time.Now().Before(deadline) {
		decision = mounted.Live().Nodes["arbiter"].AuthorityDecision
		if decision == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if decision == nil || decision.Kind != element.DecisionSucceeded ||
		decision.Operation != element.DecisionSelect || decision.Crossed {
		t.Fatalf("payload-free live authority decision = %+v", decision)
	}

	// The slower activation may arrive after selection because each candidate
	// crosses an independent graph lane. It must still be canceled before any
	// later admitted proposal can reopen the effect path.
	send(t, mustIngressAction(t, mounted, "planner_candidate"), plannerCandidate)
	cancelEnvelope := receive(t, mustEgressAction(t, mounted, "cancel_upstream"))
	cancelValue, ok := cancelEnvelope.Payload.(cognitionelements.Cancel)
	if !ok || cancelEnvelope.RunID != "planner-run" ||
		cancelEnvelope.CancellationScope != "planner-run" || cancelValue.RunID != "planner-run" {
		t.Fatalf("late-loser cancellation = %#v envelope=%+v", cancelEnvelope.Payload, cancelEnvelope)
	}
	send(t, mustIngressAction(t, mounted, "planner_proposal"), plannerProposal)
	assertNoEnvelope(t, mustEgressAction(t, mounted, "selected"))
	assertNoEnvelope(t, mustEgressAction(t, mounted, "cancel_upstream"))
}

func TestActionArbiterPairsEitherArrivalOrderAndCancelsKnownLoser(t *testing.T) {
	mounted, done, cancel := mountActionArbiter(t, `{}`)
	defer stopMounted(t, mounted, done, cancel)

	reflexCandidate, _, _ := actionArbitrationEvidence(
		"reflex-run", "reflex-call", "session-order", "screen-trigger",
	)
	plannerCandidate, plannerProposal, _ := actionArbitrationEvidence(
		"planner-run", "planner-call", "session-order", "screen-trigger",
	)
	// Proposal-first delivery is legal across independent variadic inputs.
	send(t, mustIngressAction(t, mounted, "planner_proposal"), plannerProposal)
	send(t, mustIngressAction(t, mounted, "reflex_candidate"), reflexCandidate)
	send(t, mustIngressAction(t, mounted, "planner_candidate"), plannerCandidate)

	selected := receive(t, mustEgressAction(t, mounted, "selected")).Payload.(AdmittedProposal)
	if selected.ModelRunID != "planner-run" || selected.Proposal.Call.CallID != "planner-call" {
		t.Fatalf("proposal-first selection = %+v", selected)
	}
	canceled := receive(t, mustEgressAction(t, mounted, "cancel_upstream"))
	if value := canceled.Payload.(cognitionelements.Cancel); value.RunID != "reflex-run" {
		t.Fatalf("known-loser cancellation = %+v envelope=%+v", value, canceled)
	}

	send(t, mustIngressAction(t, mounted, "planner_proposal"), plannerProposal)
	assertNoEnvelope(t, mustEgressAction(t, mounted, "selected"))
}

func TestActionArbiterNoActionCompletionReleasesBoundedGroup(t *testing.T) {
	mounted, done, cancel := mountActionArbiter(t, `{"max_pending":1,"terminal_memory":4}`)
	defer stopMounted(t, mounted, done, cancel)

	reflexCandidate, _, reflexResult := actionArbitrationEvidence(
		"reflex-empty", "unused-reflex", "session-empty", "empty-trigger",
	)
	plannerCandidate, _, plannerResult := actionArbitrationEvidence(
		"planner-empty", "unused-planner", "session-empty", "empty-trigger",
	)
	reflexResult = withoutAction(reflexResult)
	plannerResult = withoutAction(plannerResult)

	// Results can overtake candidates. The bounded orphan join must close only
	// after both exact candidate/result pairs prove that every lane had no action.
	send(t, mustIngressAction(t, mounted, "reflex_result"), reflexResult)
	send(t, mustIngressAction(t, mounted, "reflex_result"), reflexResult)
	if outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome); outcome.Code != "duplicate_result" {
		t.Fatalf("duplicate pre-candidate result outcome = %+v", outcome)
	}
	send(t, mustIngressAction(t, mounted, "planner_result"), plannerResult)
	send(t, mustIngressAction(t, mounted, "reflex_candidate"), reflexCandidate)
	send(t, mustIngressAction(t, mounted, "planner_candidate"), plannerCandidate)
	outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome)
	if outcome.Kind != OutcomeSucceeded || outcome.Code != "no_action" {
		t.Fatalf("no-action arbitration outcome = %+v", outcome)
	}
	send(t, mustIngressAction(t, mounted, "planner_result"), plannerResult)
	if outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome); outcome.Code != "terminal_result" {
		t.Fatalf("terminal result replay outcome = %+v", outcome)
	}

	nextCandidate, nextProposal, _ := actionArbitrationEvidence(
		"next-reflex", "next-call", "session-next", "next-trigger",
	)
	send(t, mustIngressAction(t, mounted, "reflex_candidate"), nextCandidate)
	send(t, mustIngressAction(t, mounted, "reflex_proposal"), nextProposal)
	if selected := receive(t, mustEgressAction(t, mounted, "selected")).Payload.(AdmittedProposal); selected.ModelRunID != "next-reflex" {
		t.Fatalf("selection after bounded no-action release = %+v", selected)
	}
}

func TestActionArbiterRejectsCandidateDriftAndExcessLanes(t *testing.T) {
	mounted, done, cancel := mountActionArbiter(t, `{}`)
	defer stopMounted(t, mounted, done, cancel)

	firstCandidate, firstProposal, _ := actionArbitrationEvidence(
		"first-run", "first-call", "session-drift", "same-trigger",
	)
	secondCandidate, _, _ := actionArbitrationEvidence(
		"second-run", "second-call", "session-drift", "same-trigger",
	)
	thirdCandidate, _, _ := actionArbitrationEvidence(
		"third-run", "third-call", "session-drift", "same-trigger",
	)
	send(t, mustIngressAction(t, mounted, "reflex_candidate"), firstCandidate)
	send(t, mustIngressAction(t, mounted, "planner_candidate"), secondCandidate)
	send(t, mustIngressAction(t, mounted, "planner_candidate"), thirdCandidate)
	if outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome); outcome.Code != "lane_capacity" {
		t.Fatalf("excess-lane outcome = %+v", outcome)
	}
	_ = receive(t, mustEgressAction(t, mounted, "cancel_upstream"))

	drifted := firstProposal.Clone()
	driftedValue := drifted.Payload.(AdmittedProposal)
	driftedValue.ContextTailItem = "different-context-tail"
	drifted.Payload = driftedValue
	send(t, mustIngressAction(t, mounted, "reflex_proposal"), drifted)
	if outcome := receive(t, mustEgressAction(t, mounted, "outcome")).Payload.(Outcome); outcome.Code != "candidate_binding_mismatch" {
		t.Fatalf("candidate drift outcome = %+v", outcome)
	}
	assertNoEnvelope(t, mustEgressAction(t, mounted, "selected"))
}

func mountActionArbiter(
	t *testing.T, config string,
) (*graphruntime.Mounted, chan error, context.CancelFunc) {
	t.Helper()
	return mountGraph(t, actionArbiterGraph,
		map[string]json.RawMessage{"arbiter": json.RawMessage(config)},
		graphruntime.NewServiceSet())
}

func actionArbitrationEvidence(
	runID, callID, sessionID, triggerID string,
) (element.Envelope, element.Envelope, element.Envelope) {
	activationItem := "activation-" + runID
	activationCause := "activation-cause-" + runID
	authorityItem := "observation-" + triggerID
	contextEnvelope := "context-" + runID
	contextTail := "tail-" + triggerID
	candidateItem := "candidate-" + runID
	candidate := authority.Candidate{
		RunID: runID, SessionID: sessionID, ActivationItemID: activationItem,
		ActivationCauseItemID: activationCause, ObservationItemID: authorityItem,
		ObservationTriggerItemID: triggerID, SourceRevision: 7, ContextVersion: 2,
		ContextEnvelopeItemID: contextEnvelope, ContextTailItem: contextTail,
	}
	candidateEnvelope := element.Envelope{
		Type: CandidateType(), ItemID: candidateItem, SessionID: sessionID, RunID: runID,
		CausalParents: []string{activationCause, authorityItem, triggerID, contextEnvelope, contextTail},
		Payload:       candidate,
	}
	proposal := cognitionelements.ToolProposal{
		Call: trajectory.ToolCall{CallID: callID, Name: "computer.click",
			Arguments: json.RawMessage(`{"source":"screen","x":10,"y":20}`)},
		Declared: true, ProviderAuthority: continuation.ToolAuthorityPropose,
	}
	admitted := AdmittedProposal{
		Proposal: proposal, ProposalItemID: "proposal-" + runID, CandidateItemID: candidateItem,
		ResultItemID: "result-" + runID, ModelRunID: runID, SessionID: sessionID,
		ActivationItemID: activationItem, ActivationCauseItemID: activationCause,
		Authority: trajectory.AuthorityUser, AuthorityItemID: authorityItem,
		ObservationTriggerItemID: triggerID, SourceRevision: 7, ContextVersion: 2,
		ContextEnvelopeItemID: contextEnvelope, ContextTailItem: contextTail,
		ProviderReference: "deployment." + runID, ModelResultDigest: testModelResultDigest,
		ModelProducer: testModelProducer(),
	}
	proposalEnvelope := element.Envelope{
		Type: AdmittedType(), ItemID: "admitted-" + runID, SessionID: sessionID, RunID: runID,
		CausalParents: []string{candidateItem, admitted.ResultItemID, admitted.ProposalItemID}, Payload: admitted,
	}
	descriptor := continuation.Descriptor{
		Provider: "test", Model: "action-arbitration", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
	invocation := continuation.Invocation{
		Instruction: "select a bounded action", SourceRevision: 7,
		Tools: []continuation.ToolDefinition{{
			Name: "computer.click", Description: "click", Parameters: json.RawMessage(`{"type":"object"}`),
		}},
	}
	resultValue := cognitionelements.Result{
		RunID: runID, ProviderReference: admitted.ProviderReference, Descriptor: descriptor,
		ContextVersion: 2, ContextTailID: contextTail, Invocation: invocation,
		Outputs:       []cognitionelements.PreparedOutput{{Kind: cognitionelements.PreparedTool, Proposal: &proposal}},
		ToolProposals: []cognitionelements.ToolProposal{proposal},
	}
	modelCauses := []string{activationItem, activationCause, authorityItem, triggerID, contextEnvelope, contextTail}
	resultEnvelope := element.Envelope{
		Type: interactionelements.SafeModelResultType(), ItemID: admitted.ResultItemID,
		SessionID: sessionID, RunID: runID, CausalParents: modelCauses, Payload: resultValue,
	}
	return candidateEnvelope, proposalEnvelope, resultEnvelope
}

func withoutAction(envelope element.Envelope) element.Envelope {
	result := envelope.Payload.(cognitionelements.Result)
	result.Outputs = nil
	result.ToolProposals = nil
	envelope.Payload = result
	return envelope
}
