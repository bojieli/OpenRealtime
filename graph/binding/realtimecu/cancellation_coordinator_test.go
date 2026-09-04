package realtimecu

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const cancellationCoordinatorTestSession = "cancellation-coordinator-session"

type cancellationCoordinatorTestFixture struct {
	runner *cancellationCoordinatorRunner
	store  *trajectory.Store
	nextNS uint64

	settlement *recordingOutputPort
	activation *recordingOutputPort
	model      *recordingOutputPort
	action     *recordingOutputPort
	state      *recordingOutputPort
	outcome    *recordingOutputPort
}

func newCancellationCoordinatorTestFixture(t *testing.T, withIntent bool) *cancellationCoordinatorTestFixture {
	t.Helper()
	fixture := &cancellationCoordinatorTestFixture{
		store: trajectory.NewStore(),
		settlement: &recordingOutputPort{
			name: "settlement_cancel", typeName: policyelements.IntentSettlementCancelType(),
		},
		activation: &recordingOutputPort{
			name: "activation_cancel", typeName: policyelements.GenerationCancelType(),
		},
		model: &recordingOutputPort{
			name: "model_cancel", typeName: cognitionelements.CancelType(),
		},
		action: &recordingOutputPort{
			name: "action_cancel", typeName: actionelements.InterruptType(),
		},
		state: &recordingOutputPort{
			name: "state", typeName: SessionCancellationStateType(),
		},
		outcome: &recordingOutputPort{
			name: "outcome", typeName: SessionCancellationOutcomeType(),
		},
	}
	fixture.runner = &cancellationCoordinatorRunner{
		instance: "cancellation-coordinator-test", sessionID: cancellationCoordinatorTestSession,
		config: CancellationCoordinatorConfig{
			MaxTransactions: 4, TombstoneMemory: 4,
			ActionAckStages: []string{"dispatch", "tool_result_commit"},
		},
		store: fixture.store,
		clock: graphruntime.ClockFunc(func() uint64 {
			fixture.nextNS++
			return fixture.nextNS
		}),
		sequences: graphruntime.NewSequenceAllocator(),
		ports: cancellationCoordinatorPorts{
			settlementCancel: fixture.settlement, activationCancel: fixture.activation,
			modelCancel: fixture.model, actionCancel: fixture.action,
			state: fixture.state, outcome: fixture.outcome,
		},
		transactions: make(map[string]*cancellationTransaction),
		byRequest:    make(map[string]*cancellationTransaction),
		byIntent:     make(map[string]*cancellationTransaction),
		tombstones:   make(map[string]cancellationTombstone),
		modelCommits: make(map[string]element.Envelope),
		state: SessionCancellationState{
			MaxTransactions: 4, TombstoneMemory: 4, ConfiguredActionAckKinds: 2,
		},
	}
	if withIntent {
		fixture.appendIntent(t)
	}
	return fixture
}

func (fixture *cancellationCoordinatorTestFixture) appendIntent(t *testing.T) {
	t.Helper()
	fixture.nextNS++
	if err := fixture.store.Append(trajectory.Item{
		ID: "durable-user-intent", Kind: trajectory.KindObservation,
		MonotonicNS: fixture.nextNS, SourceRevision: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content:  "close the warning",
		Event: &trajectory.EventMetadata{
			EventID: "durable-user-intent-event", Type: "microphone.endpoint",
			Source: "microphone", Channel: SourceMicrophone, OccurredNS: fixture.nextNS,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *cancellationCoordinatorTestFixture) request(t *testing.T, itemID string) {
	t.Helper()
	if err := fixture.runner.acceptRequest(context.Background(), element.Envelope{
		Type: SessionCancellationType(), ItemID: itemID,
		SessionID: cancellationCoordinatorTestSession, Sequence: 1,
		CancellationScope: cancellationCoordinatorTestSession, TraceID: itemID,
		Payload: SessionCancellation{
			SessionID: cancellationCoordinatorTestSession, Reason: "user canceled response",
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *cancellationCoordinatorTestFixture) acknowledgeThroughModel(
	t *testing.T, runID string, modelOutcome cognitionelements.Outcome,
) *cancellationTransaction {
	t.Helper()
	transaction := fixture.acknowledgeThroughActivation(t, runID)
	modelOutcome.RunID = runID
	if err := fixture.runner.acceptModelOutcome(context.Background(), fixture.modelOutcomeEnvelope(
		transaction, modelOutcome,
	)); err != nil {
		t.Fatal(err)
	}
	return transaction
}

func (fixture *cancellationCoordinatorTestFixture) acknowledgeThroughActivation(
	t *testing.T, runID string,
) *cancellationTransaction {
	t.Helper()
	transaction := fixture.runner.byRequest["cancel-request"]
	if transaction == nil {
		t.Fatal("request created no cancellation transaction")
	}
	if err := fixture.runner.acceptSettlementOutcome(context.Background(), element.Envelope{
		Type: policyelements.IntentSettlementOutcomeType(), ItemID: "settlement-cancel-outcome",
		SessionID: cancellationCoordinatorTestSession, Sequence: 1,
		CausalParents: []string{transaction.settlement.ItemID},
		Payload: policyelements.IntentSettlementOutcome{
			Kind: policyelements.IntentSettlementCanceled, Operation: "cancel",
			SessionID:           cancellationCoordinatorTestSession,
			DurableIntentItemID: transaction.intent.TrajectoryItemID,
			Code:                "cancellation_recorded", StateRevisionBefore: 1,
			StateRevisionAfter: 2, FinishedNS: fixture.nextNS + 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if transaction.activationSent {
		t.Fatal("settlement gate alone emitted activation cancellation before producer quiescence")
	}
	if err := fixture.runner.acceptSettlementProducerOutcome(context.Background(), element.Envelope{
		Type:      policyelements.IntentDispositionProducerOutcomeType(),
		ItemID:    "settlement-producer-cancel-outcome",
		SessionID: cancellationCoordinatorTestSession,
		Sequence:  2,
		CausalParents: []string{
			transaction.settlement.ItemID,
		},
		Payload: policyelements.IntentDispositionProducerOutcome{
			Kind:                policyelements.IntentDispositionProducerCanceled,
			DurableIntentItemID: transaction.intent.TrajectoryItemID,
			Code:                "canceled",
			Message:             "the exact decision is quiescent",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !transaction.activationSent {
		t.Fatal("gate and producer acknowledgements did not emit activation cancellation")
	}
	if err := fixture.runner.acceptActivationOutcome(context.Background(), element.Envelope{
		Type: policyelements.GenerationOutcomeType(), ItemID: "activation-cancel-outcome",
		SessionID: cancellationCoordinatorTestSession, Sequence: 3,
		CausalParents: []string{transaction.activation.ItemID},
		Payload: policyelements.GenerationOutcome{
			Kind: policyelements.GenerationCanceled, GenerationID: runID,
			Role: "computer-use", StreamID: cancellationCoordinatorTestSession,
			Code: "intent_revoked", FinishedNS: fixture.nextNS + 2,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !transaction.modelSent {
		t.Fatal("activation acknowledgement did not emit model cancellation")
	}
	return transaction
}

func (fixture *cancellationCoordinatorTestFixture) modelOutcomeEnvelope(
	transaction *cancellationTransaction, outcome cognitionelements.Outcome,
) element.Envelope {
	return element.Envelope{
		Type: cognitionelements.OutcomeType(), ItemID: "model-cancel-outcome",
		SessionID: cancellationCoordinatorTestSession, RunID: outcome.RunID, Sequence: 3,
		CausalParents: []string{transaction.model.ItemID}, Payload: outcome,
	}
}

func (fixture *cancellationCoordinatorTestFixture) modelCommitOutcomeEnvelope(
	transaction *cancellationTransaction, runID string,
) element.Envelope {
	snapshot := fixture.store.Snapshot()
	itemIDs := make([]string, 0)
	storeVersion := uint64(0)
	for index, item := range snapshot.Items {
		if item.Kind != trajectory.KindInstruction || item.InvocationID != runID ||
			item.Producer.Phase != trajectory.PhaseRuntime {
			continue
		}
		itemIDs = append(itemIDs, item.ID)
		previous := item.ID
		storeVersion = uint64(index + 1)
		for nextIndex := index + 1; nextIndex < len(snapshot.Items); nextIndex++ {
			next := snapshot.Items[nextIndex]
			if !modelResultItemKind(next.Kind) || next.InvocationID != runID ||
				len(next.CausalParentIDs) != 1 || next.CausalParentIDs[0] != previous {
				break
			}
			itemIDs = append(itemIDs, next.ID)
			previous = next.ID
			storeVersion = uint64(nextIndex + 1)
		}
		break
	}
	requestID := "model-commit-request-" + runID
	parents := []string{requestID}
	if transaction != nil && transaction.model.ItemID != "" {
		parents = append(parents, transaction.model.ItemID)
	}
	return element.Envelope{
		Type:          interactionelements.ModelCommitOutcomeType(),
		ItemID:        "model-commit-outcome",
		SessionID:     cancellationCoordinatorTestSession,
		RunID:         runID,
		Sequence:      4,
		CausalParents: parents,
		Payload: interactionelements.ModelCommitOutcome{
			Kind: interactionelements.ModelCommitted, RunID: runID,
			RequestID: requestID, StoreVersion: storeVersion, ItemIDs: itemIDs,
		},
	}
}

func (fixture *cancellationCoordinatorTestFixture) lastOutcome(t *testing.T) SessionCancellationOutcome {
	t.Helper()
	envelopes := fixture.outcome.snapshot()
	if len(envelopes) == 0 {
		t.Fatal("coordinator emitted no outcome")
	}
	outcome, ok := sessionCancellationOutcomePayload(envelopes[len(envelopes)-1].Payload)
	if !ok {
		t.Fatalf("coordinator outcome has payload %T", envelopes[len(envelopes)-1].Payload)
	}
	return outcome
}

func TestCancellationCoordinatorNoCurrentIntentIsExactTerminal(t *testing.T) {
	fixture := newCancellationCoordinatorTestFixture(t, false)
	fixture.request(t, "empty-cancel-request")
	outcome := fixture.lastOutcome(t)
	if outcome.Kind != SessionCancellationNoCurrentIntent || outcome.Code != "no_current_intent" ||
		outcome.RequestItemID != "empty-cancel-request" || len(fixture.runner.transactions) != 0 ||
		len(fixture.settlement.snapshot()) != 0 || len(fixture.activation.snapshot()) != 0 ||
		len(fixture.model.snapshot()) != 0 || len(fixture.action.snapshot()) != 0 {
		t.Fatalf("empty cancellation = %+v; coordinator state = %+v", outcome, fixture.runner.state)
	}
}

func TestCancellationCoordinatorRunNotActiveRequiresExactCanonicalModelBatch(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		canonical bool
	}{
		{name: "cancel overtakes trigger", canonical: false},
		{name: "result committed before cancel", canonical: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCancellationCoordinatorTestFixture(t, true)
			fixture.request(t, "cancel-request")
			if testCase.canonical {
				fixture.nextNS++
				if err := fixture.store.Append(trajectory.Item{
					ID: "canonical-model-instruction", Kind: trajectory.KindInstruction,
					MonotonicNS: fixture.nextNS, CausalParentIDs: []string{"durable-user-intent"},
					SourceRevision: 1, InvocationID: "model-run",
					Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
					Content:  "act on the current committed context",
				}); err != nil {
					t.Fatal(err)
				}
			}
			transaction := fixture.acknowledgeThroughModel(t, "model-run", cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeIgnored, Operation: "cancel", Code: "run_not_active",
			})
			outcome := fixture.lastOutcome(t)
			if testCase.canonical {
				if outcome.Kind != SessionCancellationProgress ||
					outcome.Code != "canonical_result_already_committed" ||
					!outcome.PendingModelCommit {
					t.Fatalf("canonical inactive run skipped explicit commit acknowledgement: %+v", outcome)
				}
				if err := fixture.runner.acceptModelCommitOutcome(context.Background(),
					fixture.modelCommitOutcomeEnvelope(transaction, "model-run")); err != nil {
					t.Fatal(err)
				}
				outcome = fixture.lastOutcome(t)
				if outcome.Kind != SessionCancellationCompleted ||
					outcome.Code != "all_required_acknowledgements" ||
					len(fixture.runner.transactions) != 0 {
					t.Fatalf("canonical inactive run did not complete: %+v transaction=%+v", outcome, transaction)
				}
				return
			}
			if outcome.Kind != SessionCancellationProgress || outcome.Code != "model_run_not_observed" ||
				transaction.modelDone || transaction.modelCommitDone || transaction.actionsDiscovered ||
				len(fixture.runner.transactions) != 1 {
				t.Fatalf("unobserved inactive run advanced: outcome=%+v transaction=%+v", outcome, transaction)
			}
		})
	}
}

func TestCancellationCoordinatorRequiresExactCanonicalModelCommitEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*element.Envelope, *interactionelements.ModelCommitOutcome)
	}{
		{
			name: "envelope run mismatch",
			mutate: func(envelope *element.Envelope, _ *interactionelements.ModelCommitOutcome) {
				envelope.RunID = "different-run"
			},
		},
		{
			name: "missing request identity",
			mutate: func(_ *element.Envelope, outcome *interactionelements.ModelCommitOutcome) {
				outcome.RequestID = ""
			},
		},
		{
			name: "missing request lineage",
			mutate: func(envelope *element.Envelope, outcome *interactionelements.ModelCommitOutcome) {
				envelope.CausalParents = slices.DeleteFunc(envelope.CausalParents,
					func(parent string) bool { return parent == outcome.RequestID })
			},
		},
		{
			name: "future store version",
			mutate: func(_ *element.Envelope, outcome *interactionelements.ModelCommitOutcome) {
				outcome.StoreVersion++
			},
		},
		{
			name: "forged item identity",
			mutate: func(_ *element.Envelope, outcome *interactionelements.ModelCommitOutcome) {
				outcome.ItemIDs[len(outcome.ItemIDs)-1] = "forged-model-result-item"
			},
		},
		{
			name: "strict batch prefix",
			mutate: func(_ *element.Envelope, outcome *interactionelements.ModelCommitOutcome) {
				outcome.ItemIDs = slices.Clone(outcome.ItemIDs[:1])
				outcome.StoreVersion = 2 // durable intent, then runtime instruction
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCancellationCoordinatorTestFixture(t, true)
			fixture.appendCanonicalModelInstruction(t, "model-run")
			fixture.appendCanonicalToolProposal(t, "model-run", "exact-commit-call")
			fixture.request(t, "cancel-request")
			transaction := fixture.acknowledgeThroughActivation(t, "model-run")
			envelope := fixture.modelCommitOutcomeEnvelope(transaction, "model-run")
			outcome := envelope.Payload.(interactionelements.ModelCommitOutcome)
			test.mutate(&envelope, &outcome)
			envelope.Payload = outcome

			if err := fixture.runner.acceptModelCommitOutcome(context.Background(), envelope); err != nil {
				t.Fatal(err)
			}
			if transaction.modelCommitDone || transaction.modelCommitAuthorizer.ItemID != "" ||
				transaction.actionsDiscovered {
				t.Fatalf("invalid commit evidence advanced cancellation: %+v", transaction)
			}
			projected := fixture.lastOutcome(t)
			if projected.Kind != SessionCancellationIncomplete ||
				!strings.HasPrefix(projected.Code, "model_commit_") {
				t.Fatalf("invalid commit evidence projection = %+v", projected)
			}

			valid := fixture.modelCommitOutcomeEnvelope(transaction, "model-run")
			if err := fixture.runner.acceptModelCommitOutcome(context.Background(), valid); err != nil {
				t.Fatal(err)
			}
			if !transaction.modelCommitDone ||
				transaction.modelCommitAuthorizer.ItemID != valid.ItemID ||
				transaction.actionsDiscovered {
				t.Fatalf("exact commit evidence was not retained without premature action release: %+v",
					transaction)
			}
		})
	}
}

func TestCancellationCoordinatorDoesNotRebindModelCommitAuthorizer(t *testing.T) {
	fixture := newCancellationCoordinatorTestFixture(t, true)
	fixture.appendCanonicalModelInstruction(t, "model-run")
	fixture.appendCanonicalToolProposal(t, "model-run", "authorizer-call")
	fixture.request(t, "cancel-request")
	transaction := fixture.acknowledgeThroughActivation(t, "model-run")
	original := fixture.modelCommitOutcomeEnvelope(transaction, "model-run")
	if err := fixture.runner.acceptModelCommitOutcome(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	retained := transaction.modelCommitAuthorizer.Clone()
	conflict := original.Clone()
	conflict.ItemID = "conflicting-model-commit-outcome"
	conflict.Sequence++
	if err := fixture.runner.acceptModelCommitOutcome(context.Background(), conflict); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(transaction.modelCommitAuthorizer, retained) ||
		fixture.lastOutcome(t).Code != "model_commit_identity_conflict" {
		t.Fatalf("conflicting commit rebound exact authorizer: retained=%+v current=%+v",
			retained, transaction.modelCommitAuthorizer)
	}
}

func TestCancellationCoordinatorAlreadyCanceledControlIsNotModelQuiescence(t *testing.T) {
	fixture := newCancellationCoordinatorTestFixture(t, true)
	fixture.appendCanonicalModelInstruction(t, "model-run")
	fixture.appendCanonicalToolProposal(t, "model-run", "still-running-call")
	fixture.request(t, "cancel-request")
	transaction := fixture.acknowledgeThroughActivation(t, "model-run")
	commit := fixture.modelCommitOutcomeEnvelope(transaction, "model-run")
	if err := fixture.runner.acceptModelCommitOutcome(context.Background(), commit); err != nil {
		t.Fatal(err)
	}
	duplicateControl := fixture.modelOutcomeEnvelope(transaction, cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeIgnored, Operation: "cancel", RunID: "model-run",
		Code: "already_canceled",
	})
	if err := fixture.runner.acceptModelOutcome(context.Background(), duplicateControl); err != nil {
		t.Fatal(err)
	}
	if transaction.modelDone || transaction.actionsDiscovered || len(fixture.action.snapshot()) != 0 {
		t.Fatalf("duplicate cancel control claimed provider quiescence: %+v", transaction)
	}
	if outcome := fixture.lastOutcome(t); outcome.Kind != SessionCancellationIncomplete ||
		outcome.Code != "model_cancel_unacknowledged" {
		t.Fatalf("duplicate cancel control projection = %+v", outcome)
	}

	terminal := fixture.modelOutcomeEnvelope(transaction, cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeCanceled, Operation: "generate", RunID: "model-run",
		Code: "canceled", StartedNS: fixture.nextNS + 1, FinishedNS: fixture.nextNS + 2,
	})
	terminal.ItemID = "model-provider-quiescent"
	if err := fixture.runner.acceptModelOutcome(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	target := transaction.actions[cancellationActionKey("model-run", "still-running-call")]
	if target == nil || !target.sent ||
		!slices.Contains(target.envelope.CausalParents, terminal.ItemID) ||
		!slices.Contains(target.envelope.CausalParents, commit.ItemID) ||
		slices.Contains(target.envelope.CausalParents, duplicateControl.ItemID) {
		t.Fatalf("action cancellation did not retain the exact quiescence and commit authorizers: %+v",
			target)
	}
}

func TestCancellationCoordinatorIgnoresAmbientTrajectoryRepliesForSameRun(t *testing.T) {
	for _, code := range []string{"unknown_commit_reply", "unknown_rejection_reply"} {
		t.Run(code, func(t *testing.T) {
			fixture := newCancellationCoordinatorTestFixture(t, true)
			const runID = "model-run"
			fixture.appendCanonicalModelInstruction(t, runID)
			fixture.appendCanonicalToolProposal(t, runID, "ambient-reply-call")
			fixture.request(t, "cancel-request")
			transaction := fixture.acknowledgeThroughActivation(t, runID)

			before := len(fixture.outcome.snapshot())
			ambient := element.Envelope{
				Type:   interactionelements.ModelCommitOutcomeType(),
				ItemID: "ambient-" + code, SessionID: cancellationCoordinatorTestSession,
				RunID: runID, Sequence: 4,
				Payload: interactionelements.ModelCommitOutcome{
					Kind: interactionelements.ModelIgnored, RunID: runID, Code: code,
					Message: "trajectory commit has no pending request",
				},
			}
			if err := fixture.runner.acceptModelCommitOutcome(context.Background(), ambient); err != nil {
				t.Fatal(err)
			}
			if len(fixture.outcome.snapshot()) != before || transaction.completion != nil ||
				fixture.runner.transactions[transaction.id] != transaction || transaction.modelCommitDone {
				t.Fatalf("ambient %s changed cancellation transaction: %+v", code, transaction)
			}

			commit := fixture.modelCommitOutcomeEnvelope(transaction, runID)
			if err := fixture.runner.acceptModelCommitOutcome(context.Background(), commit); err != nil {
				t.Fatal(err)
			}
			if err := fixture.runner.acceptModelOutcome(context.Background(), fixture.modelOutcomeEnvelope(
				transaction, cognitionelements.Outcome{
					Kind: cognitionelements.OutcomeCanceled, Operation: "generate", RunID: runID,
					Code: "canceled", StartedNS: fixture.nextNS + 1, FinishedNS: fixture.nextNS + 2,
				},
			)); err != nil {
				t.Fatal(err)
			}
			target := transaction.actions[cancellationActionKey(runID, "ambient-reply-call")]
			if target == nil || !target.sent {
				t.Fatalf("exact model commit after ambient %s did not release action cancellation: %+v",
					code, transaction)
			}
		})
	}
}

func TestAmbientModelCommitReplyRequiresExactEmptyDiagnosticShape(t *testing.T) {
	base := interactionelements.ModelCommitOutcome{
		Kind: interactionelements.ModelIgnored, RunID: "model-run", Code: "unknown_commit_reply",
		Message: "trajectory commit has no pending request",
	}
	if !ambientModelCommitReply(base) {
		t.Fatal("exact unknown commit diagnostic was not recognized as ambient fanout")
	}
	rejection := base
	rejection.Code = "unknown_rejection_reply"
	if !ambientModelCommitReply(rejection) {
		t.Fatal("exact unknown rejection diagnostic was not recognized as ambient fanout")
	}
	for _, test := range []struct {
		name   string
		mutate func(*interactionelements.ModelCommitOutcome)
	}{
		{name: "committed kind", mutate: func(outcome *interactionelements.ModelCommitOutcome) {
			outcome.Kind = interactionelements.ModelCommitted
		}},
		{name: "request identity", mutate: func(outcome *interactionelements.ModelCommitOutcome) {
			outcome.RequestID = "claimed-model-request"
		}},
		{name: "store boundary", mutate: func(outcome *interactionelements.ModelCommitOutcome) {
			outcome.StoreVersion = 1
		}},
		{name: "item identity", mutate: func(outcome *interactionelements.ModelCommitOutcome) {
			outcome.ItemIDs = []string{"claimed-model-item"}
		}},
		{name: "different code", mutate: func(outcome *interactionelements.ModelCommitOutcome) {
			outcome.Code = "already_pending"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			test.mutate(&candidate)
			if ambientModelCommitReply(candidate) {
				t.Fatalf("non-ambient model commit outcome was ignored: %+v", candidate)
			}
		})
	}
}

func TestCancellationCoordinatorRetainsPreRequestModelCommitForLaterExactCancellation(t *testing.T) {
	fixture := newCancellationCoordinatorTestFixture(t, true)
	fixture.appendCanonicalModelInstruction(t, "model-run")
	fixture.appendCanonicalToolProposal(t, "model-run", "pre-request-call")
	commit := fixture.modelCommitOutcomeEnvelope(nil, "model-run")
	if err := fixture.runner.acceptModelCommitOutcome(context.Background(), commit); err != nil {
		t.Fatal(err)
	}
	if retained := fixture.runner.modelCommits["model-run"]; retained.ItemID != commit.ItemID {
		t.Fatalf("pre-request model commit was not retained exactly: %+v", retained)
	}

	fixture.request(t, "cancel-request")
	transaction := fixture.acknowledgeThroughActivation(t, "model-run")
	terminal := fixture.modelOutcomeEnvelope(transaction, cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeCanceled, Operation: "generate", RunID: "model-run",
		Code: "canceled_before_start", FinishedNS: fixture.nextNS + 1,
	})
	if err := fixture.runner.acceptModelOutcome(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	if !transaction.modelCommitNeeded || !transaction.modelCommitDone ||
		transaction.modelCommitAuthorizer.ItemID != commit.ItemID ||
		fixture.runner.modelCommits["model-run"].ItemID != "" {
		t.Fatalf("later cancellation did not adopt the exact retained commit: %+v", transaction)
	}
	target := transaction.actions[cancellationActionKey("model-run", "pre-request-call")]
	if target == nil || !target.sent ||
		!slices.Contains(target.envelope.CausalParents, terminal.ItemID) ||
		!slices.Contains(target.envelope.CausalParents, commit.ItemID) {
		t.Fatalf("retained pre-request commit did not authorize exact action cancellation: %+v", target)
	}
}

func TestCancellationCoordinatorBoundsPreRequestModelCommitMemory(t *testing.T) {
	fixture := newCancellationCoordinatorTestFixture(t, true)
	fixture.runner.config.TombstoneMemory = 2
	for _, runID := range []string{"model-one", "model-two", "model-three"} {
		fixture.appendCanonicalModelInstruction(t, runID)
		if err := fixture.runner.acceptModelCommitOutcome(context.Background(),
			fixture.modelCommitOutcomeEnvelope(nil, runID)); err != nil {
			t.Fatal(err)
		}
	}
	if len(fixture.runner.modelCommits) != 2 ||
		!slices.Equal(fixture.runner.modelCommitOrd, []string{"model-two", "model-three"}) ||
		fixture.runner.modelCommits["model-one"].ItemID != "" {
		t.Fatalf("pre-request model commit memory is not bounded FIFO: order=%v retained=%v",
			fixture.runner.modelCommitOrd, fixture.runner.modelCommits)
	}
}

func TestCancellationCoordinatorRejectsOversizedAcknowledgementWithoutProgress(t *testing.T) {
	fixture := newCancellationCoordinatorTestFixture(t, true)
	fixture.request(t, "cancel-request")
	transaction := fixture.runner.byRequest["cancel-request"]
	if err := fixture.runner.acceptSettlementOutcome(context.Background(), element.Envelope{
		Type: policyelements.IntentSettlementOutcomeType(), ItemID: "hostile-settlement-outcome",
		SessionID: cancellationCoordinatorTestSession, Sequence: 2,
		CausalParents: []string{transaction.settlement.ItemID},
		Payload: policyelements.IntentSettlementOutcome{
			Kind: policyelements.IntentSettlementCanceled, Operation: "cancel",
			SessionID:           cancellationCoordinatorTestSession,
			DurableIntentItemID: transaction.intent.TrajectoryItemID,
			Code:                strings.Repeat("x", 257), FinishedNS: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if transaction.settlementSeen || transaction.settlementDone || transaction.activationSent ||
		transaction.failureCode != "" || len(fixture.runner.transactions) != 1 {
		t.Fatalf("invalid acknowledgement mutated transaction: %+v", transaction)
	}
	outcome := fixture.lastOutcome(t)
	if outcome.Kind != SessionCancellationRefused || outcome.Code != "invalid_settlement_outcome" ||
		len(outcome.Message) > maximumCancellationReasonBytes {
		t.Fatalf("invalid acknowledgement refusal = %+v", outcome)
	}
}

func TestCancellationCoordinatorPayloadValidatorsBoundAllAcknowledgementFamilies(t *testing.T) {
	bad := strings.Repeat("x", 257)
	checks := []struct {
		name string
		err  error
	}{
		{"settlement", validateIntentSettlementOutcome(policyelements.IntentSettlementOutcome{
			Kind: policyelements.IntentSettlementCanceled, Operation: "cancel", SessionID: "session",
			Message: strings.Repeat("m", maximumCancellationReasonBytes+1),
		})},
		{"activation", validateGenerationOutcome(policyelements.GenerationOutcome{
			Kind: policyelements.GenerationCanceled, Role: "role", Code: bad,
		})},
		{"model", validateCognitionOutcome(cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeCanceled, Operation: "cancel", RunID: "run",
			ProviderReference: bad,
		})},
		{"model commit", validateModelCommitOutcome(interactionelements.ModelCommitOutcome{
			Kind: interactionelements.ModelCommitted, RunID: "run",
			ItemIDs: []string{bad},
		})},
		{"action", validateActionOutcome(actionelements.Outcome{
			Kind: actionelements.OutcomeCanceled, Stage: "dispatch", Operation: "cancel",
			CallID: bad,
		})},
	}
	for _, check := range checks {
		if check.err == nil {
			t.Errorf("%s acknowledgement accepted oversized metadata", check.name)
		}
	}
}

type flakyCoordinatorOutputPort struct {
	mu                sync.Mutex
	delegate          element.OutputPort
	failuresRemaining int
	attempts          []element.Envelope
}

func (port *flakyCoordinatorOutputPort) Name() string       { return port.delegate.Name() }
func (port *flakyCoordinatorOutputPort) Type() element.Type { return port.delegate.Type() }
func (port *flakyCoordinatorOutputPort) Lanes() []element.Sender {
	return port.delegate.Lanes()
}
func (port *flakyCoordinatorOutputPort) Broadcast(
	ctx context.Context, envelope element.Envelope,
) (element.SendResult, error) {
	port.mu.Lock()
	port.attempts = append(port.attempts, envelope.Clone())
	if port.failuresRemaining > 0 {
		port.failuresRemaining--
		port.mu.Unlock()
		return element.SendResult{}, errors.New("injected cancellation publication failure")
	}
	port.mu.Unlock()
	return port.delegate.Broadcast(ctx, envelope)
}
func (port *flakyCoordinatorOutputPort) snapshot() []element.Envelope {
	port.mu.Lock()
	defer port.mu.Unlock()
	result := make([]element.Envelope, len(port.attempts))
	for index, envelope := range port.attempts {
		result[index] = envelope.Clone()
	}
	return result
}
func (port *flakyCoordinatorOutputPort) recover() {
	port.mu.Lock()
	port.failuresRemaining = 0
	port.mu.Unlock()
}

func TestCancellationCoordinatorRetainsExactTerminalPublicationUntilOutcomeAndStateSucceed(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		failOutput bool
		persistent bool
	}{
		{name: "transient outcome publication", failOutput: true},
		{name: "transient state publication"},
		{name: "bounded outcome publication", failOutput: true, persistent: true},
		{name: "bounded state publication", persistent: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newCancellationCoordinatorTestFixture(t, true)
			fixture.request(t, "cancel-request")
			fixture.appendCanonicalModelInstruction(t, "model-run")
			transaction := fixture.acknowledgeThroughActivation(t, "model-run")
			modelOutcome := cognitionelements.Outcome{
				Kind: cognitionelements.OutcomeIgnored, Operation: "cancel",
				RunID: "model-run", Code: "run_not_active",
			}
			envelope := fixture.modelOutcomeEnvelope(transaction, modelOutcome)
			if err := fixture.runner.acceptModelOutcome(context.Background(), envelope); err != nil {
				t.Fatal(err)
			}
			commitEnvelope := fixture.modelCommitOutcomeEnvelope(transaction, "model-run")
			outcomesBefore := len(fixture.outcome.snapshot())
			statesBefore := len(fixture.state.snapshot())
			failures := 1
			if testCase.persistent {
				failures = maximumCancellationPublishTries
			}
			flaky := &flakyCoordinatorOutputPort{failuresRemaining: failures}
			if testCase.failOutput {
				flaky.delegate = fixture.outcome
				fixture.runner.ports.outcome = flaky
			} else {
				flaky.delegate = fixture.state
				fixture.runner.ports.state = flaky
			}

			settlementCount := len(fixture.settlement.snapshot())
			activationCount := len(fixture.activation.snapshot())
			modelCount := len(fixture.model.snapshot())
			err := fixture.runner.acceptModelCommitOutcome(context.Background(), commitEnvelope)
			if testCase.persistent {
				if err == nil || !strings.Contains(err.Error(), "failed after 3 bounded attempts") {
					t.Fatalf("bounded terminal publication error = %v", err)
				}
				if fixture.runner.transactions[transaction.id] != transaction ||
					fixture.runner.byRequest[transaction.request.ItemID] != transaction ||
					fixture.runner.byIntent[transaction.intent.TrajectoryItemID] != transaction ||
					fixture.runner.state.Completed != 0 ||
					fixture.runner.tombstones[transaction.request.ItemID].transactionID != "" ||
					transaction.completion == nil {
					t.Fatalf("failed terminal publication retired state: transaction=%+v state=%+v tombstones=%+v",
						transaction, fixture.runner.state, fixture.runner.tombstones)
				}
				flaky.recover()
				if err := fixture.runner.acceptModelCommitOutcome(context.Background(), commitEnvelope); err != nil {
					t.Fatalf("retry retained terminal publication: %v", err)
				}
			} else if err != nil {
				t.Fatalf("bounded retry did not absorb one transient publication failure: %v", err)
			}
			attempts := flaky.snapshot()
			wantAttempts := 2
			if testCase.persistent {
				wantAttempts = maximumCancellationPublishTries + 1
			}
			if len(attempts) != wantAttempts {
				t.Fatalf("terminal publication attempts = %d, want %d", len(attempts), wantAttempts)
			}
			for index := 1; index < len(attempts); index++ {
				if !reflect.DeepEqual(attempts[0], attempts[index]) {
					t.Fatalf("terminal retry %d changed its immutable envelope: %+v", index, attempts)
				}
			}
			if fixture.runner.transactions[transaction.id] != nil ||
				fixture.runner.byRequest[transaction.request.ItemID] != nil ||
				fixture.runner.byIntent[transaction.intent.TrajectoryItemID] != nil ||
				fixture.runner.state.Completed != 1 ||
				fixture.runner.tombstones[transaction.request.ItemID].kind != SessionCancellationCompleted {
				t.Fatalf("successful retry did not retire terminal state: state=%+v tombstones=%+v",
					fixture.runner.state, fixture.runner.tombstones)
			}
			if len(fixture.settlement.snapshot()) != settlementCount ||
				len(fixture.activation.snapshot()) != activationCount ||
				len(fixture.model.snapshot()) != modelCount || len(fixture.action.snapshot()) != 0 {
				t.Fatal("terminal publication retry duplicated an upstream cancellation")
			}
			if len(fixture.outcome.snapshot()) != outcomesBefore+1 ||
				len(fixture.state.snapshot()) != statesBefore+1 {
				t.Fatalf("terminal publications = %d/%d, want %d/%d",
					len(fixture.outcome.snapshot()), len(fixture.state.snapshot()),
					outcomesBefore+1, statesBefore+1)
			}
		})
	}
}

func (fixture *cancellationCoordinatorTestFixture) appendCanonicalModelInstruction(
	t *testing.T, runID string,
) {
	t.Helper()
	fixture.nextNS++
	if err := fixture.store.Append(trajectory.Item{
		ID: "canonical-model-instruction-" + runID, Kind: trajectory.KindInstruction,
		MonotonicNS: fixture.nextNS, CausalParentIDs: []string{"durable-user-intent"},
		SourceRevision: 1, InvocationID: runID,
		Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
		Content:  "act on the current committed context",
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *cancellationCoordinatorTestFixture) appendCanonicalToolProposal(
	t *testing.T, runID, callID string,
) {
	t.Helper()
	fixture.nextNS++
	call := trajectory.ToolCall{CallID: callID, Name: "computer_click", Arguments: []byte(`{"x":1,"y":2}`)}
	if err := fixture.store.Append(trajectory.Item{
		ID: "canonical-model-proposal-" + callID, Kind: trajectory.KindToolProposal,
		MonotonicNS: fixture.nextNS, CausalParentIDs: []string{"canonical-model-instruction-" + runID},
		SourceRevision: 1, InvocationID: runID,
		Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, ToolCall: &call,
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture *cancellationCoordinatorTestFixture) advanceToActionCancellation(
	t *testing.T, callID string,
) (*cancellationTransaction, *cancellationActionTarget) {
	t.Helper()
	const runID = "model-run"
	fixture.appendCanonicalModelInstruction(t, runID)
	fixture.appendCanonicalToolProposal(t, runID, callID)
	fixture.request(t, "cancel-request")
	transaction := fixture.acknowledgeThroughActivation(t, runID)
	// Model-result commit can legitimately win its lane before the model's
	// cancellation outcome. It must be retained but cannot release actions by
	// itself.
	if err := fixture.runner.acceptModelCommitOutcome(context.Background(),
		fixture.modelCommitOutcomeEnvelope(transaction, runID)); err != nil {
		t.Fatal(err)
	}
	if !transaction.modelCommitDone || transaction.actionsDiscovered {
		t.Fatalf("early model commit advanced without terminal model evidence: %+v", transaction)
	}
	if err := fixture.runner.acceptModelOutcome(context.Background(), fixture.modelOutcomeEnvelope(
		transaction, cognitionelements.Outcome{
			Kind: cognitionelements.OutcomeCanceled, Operation: "generate", RunID: runID,
			Code: "canceled", StartedNS: fixture.nextNS + 1, FinishedNS: fixture.nextNS + 2,
		},
	)); err != nil {
		t.Fatal(err)
	}
	target := transaction.actions[cancellationActionKey(runID, callID)]
	if target == nil || !target.sent || len(fixture.action.snapshot()) != 1 {
		t.Fatalf("canonical unresolved proposal did not receive one action cancellation: %+v", transaction)
	}
	return transaction, target
}

func (fixture *cancellationCoordinatorTestFixture) actionOutcomeEnvelope(
	target *cancellationActionTarget, stage string, kind actionelements.OutcomeKind,
	code string, crossed bool,
) element.Envelope {
	return element.Envelope{
		Type: actionelements.OutcomeType(), ItemID: "action-cancel-outcome-" + stage + "-" + code,
		SessionID: cancellationCoordinatorTestSession, RunID: target.runID, Sequence: 5,
		CausalParents: []string{target.envelope.ItemID},
		Payload: actionelements.Outcome{
			Kind: kind, Stage: stage, Operation: "cancel", CallID: target.callID,
			Code: code, Crossed: crossed,
		},
	}
}

func TestCancellationCoordinatorRequiresBothQuiescenceGatesAndPreservesAuthorizingLineage(t *testing.T) {
	fixture := newCancellationCoordinatorTestFixture(t, true)
	fixture.appendCanonicalModelInstruction(t, "model-run")
	fixture.appendCanonicalToolProposal(t, "model-run", "cancel-lineage-call")
	fixture.request(t, "cancel-request")
	transaction := fixture.runner.byRequest["cancel-request"]

	producerAck := element.Envelope{
		Type: policyelements.IntentDispositionProducerOutcomeType(), ItemID: "producer-first-ack",
		SessionID: cancellationCoordinatorTestSession, Sequence: 2,
		CausalParents: []string{transaction.settlement.ItemID},
		Payload: policyelements.IntentDispositionProducerOutcome{
			Kind:                policyelements.IntentDispositionProducerCanceled,
			DurableIntentItemID: transaction.intent.TrajectoryItemID, Code: "canceled",
		},
	}
	if err := fixture.runner.acceptSettlementProducerOutcome(context.Background(), producerAck); err != nil {
		t.Fatal(err)
	}
	if transaction.activationSent {
		t.Fatal("producer quiescence alone released activation cancellation")
	}
	gateAck := element.Envelope{
		Type: policyelements.IntentSettlementOutcomeType(), ItemID: "gate-second-ack",
		SessionID: cancellationCoordinatorTestSession, Sequence: 3,
		CausalParents: []string{transaction.settlement.ItemID},
		Payload: policyelements.IntentSettlementOutcome{
			Kind: policyelements.IntentSettlementCanceled, Operation: "cancel",
			SessionID:           cancellationCoordinatorTestSession,
			DurableIntentItemID: transaction.intent.TrajectoryItemID,
			Code:                "cancellation_recorded", FinishedNS: fixture.nextNS + 1,
		},
	}
	if err := fixture.runner.acceptSettlementOutcome(context.Background(), gateAck); err != nil {
		t.Fatal(err)
	}
	if !transaction.activationSent ||
		!slices.Contains(transaction.activation.CausalParents, producerAck.ItemID) ||
		!slices.Contains(transaction.activation.CausalParents, gateAck.ItemID) {
		t.Fatalf("activation cancel lacks both exact authorizers: %+v", transaction.activation)
	}
	activationAck := element.Envelope{
		Type: policyelements.GenerationOutcomeType(), ItemID: "activation-lineage-ack",
		SessionID: cancellationCoordinatorTestSession, Sequence: 4,
		CausalParents: []string{transaction.activation.ItemID},
		Payload: policyelements.GenerationOutcome{
			Kind: policyelements.GenerationCanceled, GenerationID: "model-run",
			Role: "computer-use", StreamID: cancellationCoordinatorTestSession,
			Code: "intent_revoked", FinishedNS: fixture.nextNS + 2,
		},
	}
	if err := fixture.runner.acceptActivationOutcome(context.Background(), activationAck); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(transaction.model.CausalParents, activationAck.ItemID) {
		t.Fatalf("model cancel lacks exact activation authorizer: %+v", transaction.model)
	}
	commitAck := fixture.modelCommitOutcomeEnvelope(transaction, "model-run")
	if err := fixture.runner.acceptModelCommitOutcome(context.Background(), commitAck); err != nil {
		t.Fatal(err)
	}
	modelAck := fixture.modelOutcomeEnvelope(transaction, cognitionelements.Outcome{
		Kind: cognitionelements.OutcomeCanceled, Operation: "generate", RunID: "model-run",
		Code: "canceled", StartedNS: fixture.nextNS + 3, FinishedNS: fixture.nextNS + 4,
	})
	if err := fixture.runner.acceptModelOutcome(context.Background(), modelAck); err != nil {
		t.Fatal(err)
	}
	target := transaction.actions[cancellationActionKey("model-run", "cancel-lineage-call")]
	if target == nil || !slices.Contains(target.envelope.CausalParents, modelAck.ItemID) ||
		!slices.Contains(target.envelope.CausalParents, commitAck.ItemID) {
		t.Fatalf("action cancel lacks model and commit authorizers: %+v", target)
	}
}

func TestCancellationCoordinatorPendingActionOutcomeDoesNotAcknowledgeStage(t *testing.T) {
	fixture := newCancellationCoordinatorTestFixture(t, true)
	transaction, target := fixture.advanceToActionCancellation(t, "pending-action-call")
	for _, code := range []string{
		"cancellation_pending_commit", "cancellation_already_pending", "result_must_commit",
	} {
		if err := fixture.runner.acceptActionOutcome(context.Background(), fixture.actionOutcomeEnvelope(
			target, "dispatch", actionelements.OutcomeIgnored, code, false,
		)); err != nil {
			t.Fatal(err)
		}
		if len(target.acknowledged) != 0 || fixture.runner.transactions[transaction.id] == nil {
			t.Fatalf("pending action outcome %q advanced cancellation: %+v", code, transaction)
		}
		if outcome := fixture.lastOutcome(t); outcome.Kind != SessionCancellationProgress ||
			outcome.Code != "action_stage_pending" {
			t.Fatalf("pending action projection for %q = %+v", code, outcome)
		}
	}
	for _, nonterminal := range []struct {
		kind actionelements.OutcomeKind
		code string
	}{
		{actionelements.OutcomeIgnored, "unknown_ignored_code"},
		{actionelements.OutcomeCanceled, "unknown_canceled_code"},
	} {
		if err := fixture.runner.acceptActionOutcome(context.Background(), fixture.actionOutcomeEnvelope(
			target, "dispatch", nonterminal.kind, nonterminal.code, false,
		)); err != nil {
			t.Fatal(err)
		}
		if len(target.acknowledged) != 0 || fixture.runner.transactions[transaction.id] == nil {
			t.Fatalf("unknown action outcome %s/%s advanced cancellation: %+v",
				nonterminal.kind, nonterminal.code, transaction)
		}
		if outcome := fixture.lastOutcome(t); outcome.Kind != SessionCancellationIncomplete ||
			outcome.Code != "action_cancel_unacknowledged" {
			t.Fatalf("unknown action outcome projection = %+v", outcome)
		}
	}
	for _, stage := range fixture.runner.config.ActionAckStages {
		if err := fixture.runner.acceptActionOutcome(context.Background(), fixture.actionOutcomeEnvelope(
			target, stage, actionelements.OutcomeCanceled, "canceled", false,
		)); err != nil {
			t.Fatal(err)
		}
	}
	if outcome := fixture.lastOutcome(t); outcome.Kind != SessionCancellationCompleted ||
		fixture.runner.transactions[transaction.id] != nil {
		t.Fatalf("terminal action acknowledgements did not complete: %+v", outcome)
	}
}

func TestCancellationCoordinatorCrossedActionRetiresHonestIncompleteTransaction(t *testing.T) {
	fixture := newCancellationCoordinatorTestFixture(t, true)
	transaction, target := fixture.advanceToActionCancellation(t, "crossed-action-call")
	if err := fixture.runner.acceptActionOutcome(context.Background(), fixture.actionOutcomeEnvelope(
		target, "dispatch", actionelements.OutcomeSucceeded, "already_dispatched", true,
	)); err != nil {
		t.Fatal(err)
	}
	outcome := fixture.lastOutcome(t)
	if outcome.Kind != SessionCancellationIncomplete || outcome.Code != "action_already_crossed" ||
		fixture.runner.transactions[transaction.id] != nil ||
		fixture.runner.byRequest[transaction.request.ItemID] != nil ||
		fixture.runner.byIntent[transaction.intent.TrajectoryItemID] != nil ||
		fixture.runner.tombstones[transaction.request.ItemID].kind != SessionCancellationIncomplete ||
		fixture.runner.state.Incomplete != 1 || fixture.runner.state.Saturated {
		t.Fatalf("crossed action did not retire honest incomplete transaction: outcome=%+v state=%+v tombstones=%+v",
			outcome, fixture.runner.state, fixture.runner.tombstones)
	}
}
