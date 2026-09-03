package realtimecu

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const activationTestSession = "activation-test-session"

func TestActivationReplaysExactDeferredVisualAfterBlockedNoProposalResult(t *testing.T) {
	fixture := newActivationTestFixture(t)
	firstRun, firstVersion := fixture.startGeneration(t, "user-task", "click when the threshold is exceeded")

	// Model the provider being blocked on the first prefix. The result cannot
	// enter the serialized activation state machine until release is closed.
	release := make(chan struct{})
	resultDone := make(chan error, 1)
	go func() {
		<-release
		resultDone <- fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
			firstRun, firstVersion, nil,
		))
	}()

	visualEnvelope, visualCommit := fixture.appendVisual(t, "screen-84", "84 C; threshold exceeded", "user-task")
	// Exercise the pointer-payload boundary and mutate the caller-owned value
	// after admission. Deferred replay must use the pinned committed value.
	visualEnvelope.Payload = &visualCommit
	if err := fixture.runner.acceptCommit(context.Background(), visualEnvelope); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.deferred == nil || fixture.runner.deferred.commit.StoreVersion != visualCommit.StoreVersion {
		t.Fatalf("deferred visual = %+v, want version %d", fixture.runner.deferred, visualCommit.StoreVersion)
	}
	deferredOutcome := fixture.lastOutcome(t)
	if deferredOutcome.Kind != policyelements.GenerationIgnored || deferredOutcome.Code != "generation_deferred" {
		t.Fatalf("deferred outcome = %+v", deferredOutcome)
	}

	wantCommit := visualCommit
	visualCommit.StoreVersion = 999
	visualCommit.Context.Prefix.Digest = "mutated-by-caller"
	fixture.appendInstruction(t, "later-unrelated-state")
	close(release)
	if err := <-resultDone; err != nil {
		t.Fatal(err)
	}

	triggers := fixture.trigger.snapshot()
	if len(triggers) != 2 {
		t.Fatalf("generation triggers = %d, want 2", len(triggers))
	}
	replayed, ok := triggers[1].Payload.(cognitionelements.Generate)
	if !ok {
		t.Fatalf("replayed trigger payload = %T", triggers[1].Payload)
	}
	if replayed.ExpectedContextVersion == nil || *replayed.ExpectedContextVersion != wantCommit.StoreVersion ||
		replayed.ExpectedContextItemID != wantCommit.Context.StateItemID ||
		replayed.CommittedContext == nil || !reflect.DeepEqual(*replayed.CommittedContext, wantCommit.Context) {
		t.Fatalf("replayed generation = %+v, want exact commit %+v", replayed, wantCommit)
	}
	if fixture.store.Snapshot().Version <= wantCommit.StoreVersion {
		t.Fatal("test did not advance the store beyond the deferred prefix")
	}
	if fixture.runner.deferred != nil || fixture.runner.active == nil ||
		fixture.runner.active.contextVersion != wantCommit.StoreVersion {
		t.Fatalf("activation state after replay: deferred=%+v active=%+v",
			fixture.runner.deferred, fixture.runner.active)
	}
	replayedOutcome := fixture.lastOutcome(t)
	if replayedOutcome.Kind != policyelements.GenerationEmitted ||
		replayedOutcome.ContextVersion != wantCommit.StoreVersion ||
		replayedOutcome.TriggerItemID != wantCommit.TriggerItemID {
		t.Fatalf("replayed outcome = %+v", replayedOutcome)
	}
}

func TestActivationDeferredVisualIsCapacityOneLatestWins(t *testing.T) {
	fixture := newActivationTestFixture(t)
	firstRun, firstVersion := fixture.startGeneration(t, "user-task", "watch the temperature")

	firstEnvelope, firstCommit := fixture.appendVisual(t, "screen-80", "80 C", "user-task")
	if err := fixture.runner.acceptCommit(context.Background(), firstEnvelope); err != nil {
		t.Fatal(err)
	}
	latestEnvelope, latestCommit := fixture.appendVisual(t, "screen-84", "84 C", "user-task")
	if err := fixture.runner.acceptCommit(context.Background(), latestEnvelope); err != nil {
		t.Fatal(err)
	}
	// A delayed older lane must not replace the newer retained world state.
	if err := fixture.runner.acceptCommit(context.Background(), firstEnvelope); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.deferred == nil || fixture.runner.deferred.commit.StoreVersion != latestCommit.StoreVersion ||
		fixture.runner.deferred.commit.StoreVersion == firstCommit.StoreVersion {
		t.Fatalf("coalesced deferred visual = %+v, want latest version %d",
			fixture.runner.deferred, latestCommit.StoreVersion)
	}
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		firstRun, firstVersion, nil,
	)); err != nil {
		t.Fatal(err)
	}

	triggers := fixture.trigger.snapshot()
	if len(triggers) != 2 {
		t.Fatalf("generation triggers = %d, want 2", len(triggers))
	}
	payload := triggers[1].Payload.(cognitionelements.Generate)
	if payload.ExpectedContextVersion == nil || *payload.ExpectedContextVersion != latestCommit.StoreVersion ||
		payload.ExpectedContextItemID != latestCommit.Context.StateItemID {
		t.Fatalf("coalesced replay = %+v, want version %d", payload, latestCommit.StoreVersion)
	}
	if fixture.runner.deferred != nil {
		t.Fatalf("deferred visual survived replay: %+v", fixture.runner.deferred)
	}
}

func TestActivationProposalRetainsLatestVisualUntilEffectDisposition(t *testing.T) {
	fixture := newActivationTestFixture(t)
	firstRun, firstVersion := fixture.startGeneration(t, "user-task", "click the warning")
	visualEnvelope, _ := fixture.appendVisual(t, "screen-warning", "warning visible", "user-task")
	if err := fixture.runner.acceptCommit(context.Background(), visualEnvelope); err != nil {
		t.Fatal(err)
	}

	proposal := cognitionelements.ToolProposal{
		Call: trajectory.ToolCall{
			CallID: "call-warning", Name: "computer.click", Arguments: json.RawMessage(`{"x":1,"y":2}`),
		},
		Declared: true, ProviderAuthority: continuation.ToolAuthorityPropose,
	}
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		firstRun, firstVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.deferred == nil || fixture.runner.active == nil ||
		fixture.runner.active.callID != proposal.Call.CallID {
		t.Fatalf("state after proposal: deferred=%+v active=%+v",
			fixture.runner.deferred, fixture.runner.active)
	}
	if triggers := fixture.trigger.snapshot(); len(triggers) != 1 {
		t.Fatalf("pre-effect visual was replayed after a proposal: %+v", triggers)
	}

	preEffectEnvelope, _ := fixture.appendVisual(t, "screen-still-warning", "warning still visible", "user-task")
	if err := fixture.runner.acceptCommit(context.Background(), preEffectEnvelope); err != nil {
		t.Fatal(err)
	}
	if outcome := fixture.lastOutcome(t); outcome.Code != "effect_pending" {
		t.Fatalf("pre-consequence visual outcome = %+v", outcome)
	}
	if triggers := fixture.trigger.snapshot(); len(triggers) != 1 {
		t.Fatalf("effect-pending visual activated cognition: %+v", triggers)
	}
	if fixture.runner.deferred == nil ||
		fixture.runner.deferred.commit.TrajectoryItemID != "screen-still-warning" {
		t.Fatalf("latest effect-disposition visual was not retained: %+v", fixture.runner.deferred)
	}
}

func TestActivationRepetitionSuppressionClearsProposalAfterModelResult(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	proposal := activationTestProposal("repeat-call")
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.active == nil || fixture.runner.active.callID != "repeat-call" {
		t.Fatalf("active proposal = %+v", fixture.runner.active)
	}
	fixture.appendModelProposal(t, runID, proposal)
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, "repeat-call")); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.active == nil || fixture.runner.pendingDisposition == nil {
		t.Fatalf("suppressed proposal released before disposition commit: active=%+v disposition=%+v",
			fixture.runner.active, fixture.runner.pendingDisposition)
	}
	fixture.commitDisposition(t)
	if fixture.runner.active != nil || fixture.runner.pendingTerminal != nil ||
		fixture.runner.pendingDisposition != nil {
		t.Fatalf("suppressed proposal remained active: active=%+v pending=%+v",
			fixture.runner.active, fixture.runner.pendingTerminal)
	}
	if outcome := fixture.lastOutcome(t); outcome.Kind != policyelements.GenerationIgnored ||
		outcome.Code != "effect_repetition_suppressed" || outcome.GenerationID != runID {
		t.Fatalf("suppression outcome = %+v", outcome)
	}
}

func TestActivationDispositionAppendIsArgumentFreeAndCanonicalOnlyAfterCommit(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	proposal := activationTestProposal("private-call")
	proposal.Call.Arguments = json.RawMessage(`{"x":17,"secret":"never-copy-me"}`)
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	proposalItem := fixture.appendModelProposal(t, runID, proposal)
	before := fixture.store.Snapshot()
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, proposal.Call.CallID)); err != nil {
		t.Fatal(err)
	}

	afterRequest := fixture.store.Snapshot()
	if !reflect.DeepEqual(afterRequest, before) {
		t.Fatalf("activation mutated canonical Store before acknowledgement: before=%+v after=%+v",
			before, afterRequest)
	}
	if fixture.runner.active == nil || fixture.runner.pendingDisposition == nil ||
		len(fixture.trigger.snapshot()) != 1 {
		t.Fatalf("proposal crossed acknowledgement barrier: active=%+v disposition=%+v triggers=%d",
			fixture.runner.active, fixture.runner.pendingDisposition, len(fixture.trigger.snapshot()))
	}
	_, request := fixture.latestDispositionRequest(t)
	item := request.Items[0]
	if item.Kind != trajectory.KindToolProposalDisposition || item.ToolProposalDisposition == nil ||
		item.ToolProposalDisposition.ProposalItemID != proposalItem.ID ||
		item.ToolProposalDisposition.CallID != proposal.Call.CallID ||
		item.ToolProposalDisposition.Name != proposal.Call.Name ||
		item.ToolProposalDisposition.Kind != trajectory.ToolProposalRepetitionSuppressed ||
		item.ToolCall != nil || item.Content != "" {
		t.Fatalf("proposal-disposition append item = %+v", item)
	}
	wire, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), "never-copy-me") || strings.Contains(string(wire), `"arguments"`) {
		t.Fatalf("proposal arguments escaped into disposition append: %s", wire)
	}

	commit, dispositionItem := fixture.commitDisposition(t)
	terminal, evidence := trajectory.TerminalToolProposalIDs(commit.Snapshot)
	if _, ok := terminal[proposalItem.ID]; !ok {
		t.Fatalf("committed snapshot did not terminate proposal %q: %v", proposalItem.ID, terminal)
	}
	if _, ok := evidence[dispositionItem.ID]; !ok {
		t.Fatalf("committed snapshot did not retain disposition evidence %q: %v",
			dispositionItem.ID, evidence)
	}
}

func TestActivationDispositionRepliesAreCorrelatedAndFailClosed(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	proposal := activationTestProposal("reply-call")
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	fixture.appendModelProposal(t, runID, proposal)
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, proposal.Call.CallID)); err != nil {
		t.Fatal(err)
	}
	requestEnvelope, request := fixture.latestDispositionRequest(t)
	wantRequestID := fixture.runner.pendingDisposition.requestID

	foreignCommit := element.Envelope{
		Type: stateelements.CommitType(), ItemID: "foreign-request:committed",
		SessionID: activationTestSession, RunID: runID, Payload: stateelements.Commit{},
	}
	if err := fixture.runner.acceptDispositionCommit(context.Background(), foreignCommit); err != nil {
		t.Fatalf("unrelated commit was not ignored: %v", err)
	}
	foreignRejection := element.Envelope{
		Type: stateelements.RejectionType(), ItemID: "foreign-request:rejected",
		SessionID: activationTestSession, RunID: runID, Payload: stateelements.Rejection{},
	}
	if err := fixture.runner.acceptDispositionRejection(context.Background(), foreignRejection); err != nil {
		t.Fatalf("unrelated rejection was not ignored: %v", err)
	}
	if fixture.runner.pendingDisposition == nil ||
		fixture.runner.pendingDisposition.requestID != wantRequestID || fixture.runner.active == nil {
		t.Fatalf("unrelated reply changed pending state: active=%+v disposition=%+v",
			fixture.runner.active, fixture.runner.pendingDisposition)
	}

	malformed := requestEnvelope.Clone()
	malformed.Type = stateelements.CommitType()
	malformed.ItemID = requestEnvelope.ItemID + ":committed"
	malformed.CausalParents = appendUniqueString(malformed.CausalParents, requestEnvelope.ItemID)
	malformed.Payload = "not-a-trajectory-commit"
	if err := fixture.runner.acceptDispositionCommit(context.Background(), malformed); err == nil {
		t.Fatal("matching malformed commit acknowledgement was accepted")
	}
	if fixture.runner.pendingDisposition == nil ||
		fixture.runner.pendingDisposition.requestID != wantRequestID || fixture.runner.active == nil {
		t.Fatalf("malformed commit released pending state: active=%+v disposition=%+v",
			fixture.runner.active, fixture.runner.pendingDisposition)
	}

	permanent := dispositionRejectionEnvelope(
		requestEnvelope, request, "invalid_item", "policy refused append", request.ExpectedVersion,
	)
	if err := fixture.runner.acceptDispositionRejection(context.Background(), permanent); err == nil {
		t.Fatal("non-version-conflict rejection did not fail closed")
	}
	if fixture.runner.pendingDisposition == nil || fixture.runner.active == nil ||
		len(fixture.trigger.snapshot()) != 1 {
		t.Fatalf("permanent rejection released generation: active=%+v disposition=%+v triggers=%d",
			fixture.runner.active, fixture.runner.pendingDisposition, len(fixture.trigger.snapshot()))
	}
}

func TestActivationDispositionVersionConflictRetriesFreshCASAndIsBounded(t *testing.T) {
	t.Run("fresh prefix retry", func(t *testing.T) {
		fixture := newActivationTestFixture(t)
		runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
		proposal := activationTestProposal("retry-call")
		if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
			runID, contextVersion, []cognitionelements.ToolProposal{proposal},
		)); err != nil {
			t.Fatal(err)
		}
		fixture.appendModelProposal(t, runID, proposal)
		if err := fixture.runner.acceptEffectTerminal(context.Background(),
			activationEffectTerminalEnvelope(runID, proposal.Call.CallID)); err != nil {
			t.Fatal(err)
		}
		firstEnvelope, first := fixture.latestDispositionRequest(t)
		fixture.appendInstruction(t, "concurrent-state")
		currentVersion := fixture.store.Snapshot().Version
		if err := fixture.runner.acceptDispositionRejection(context.Background(),
			dispositionRejectionEnvelope(
				firstEnvelope, first, "version_conflict", "concurrent append", currentVersion,
			)); err != nil {
			t.Fatal(err)
		}
		secondEnvelope, second := fixture.latestDispositionRequest(t)
		if secondEnvelope.ItemID == firstEnvelope.ItemID ||
			second.ExpectedVersion != currentVersion || second.ExpectedVersion == first.ExpectedVersion {
			t.Fatalf("retry request was not fresh: first=%s@%d second=%s@%d current=%d",
				firstEnvelope.ItemID, first.ExpectedVersion,
				secondEnvelope.ItemID, second.ExpectedVersion, currentVersion)
		}
		fixture.commitDisposition(t)
		if fixture.runner.active != nil || fixture.runner.pendingDisposition != nil {
			t.Fatalf("committed retry did not release barrier: active=%+v disposition=%+v",
				fixture.runner.active, fixture.runner.pendingDisposition)
		}
	})

	t.Run("bounded exhaustion", func(t *testing.T) {
		fixture := newActivationTestFixture(t)
		runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
		proposal := activationTestProposal("exhaust-call")
		if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
			runID, contextVersion, []cognitionelements.ToolProposal{proposal},
		)); err != nil {
			t.Fatal(err)
		}
		fixture.appendModelProposal(t, runID, proposal)
		if err := fixture.runner.acceptEffectTerminal(context.Background(),
			activationEffectTerminalEnvelope(runID, proposal.Call.CallID)); err != nil {
			t.Fatal(err)
		}
		for conflict := 0; conflict <= maximumDispositionRetries; conflict++ {
			requestEnvelope, request := fixture.latestDispositionRequest(t)
			fixture.appendInstruction(t, fmt.Sprintf("conflict-state-%d", conflict))
			err := fixture.runner.acceptDispositionRejection(context.Background(),
				dispositionRejectionEnvelope(
					requestEnvelope, request, "version_conflict", "concurrent append",
					fixture.store.Snapshot().Version,
				))
			if conflict < maximumDispositionRetries && err != nil {
				t.Fatalf("retry %d failed early: %v", conflict+1, err)
			}
			if conflict == maximumDispositionRetries && err == nil {
				t.Fatalf("retry %d exceeded the bound without failure", conflict+1)
			}
		}
		if fixture.runner.pendingDisposition == nil || fixture.runner.active == nil ||
			len(fixture.trigger.snapshot()) != 1 {
			t.Fatalf("retry exhaustion released generation: active=%+v disposition=%+v triggers=%d",
				fixture.runner.active, fixture.runner.pendingDisposition, len(fixture.trigger.snapshot()))
		}
	})
}

func TestActivationRetainsSuppressionUntilMatchingModelResult(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	proposal := activationTestProposal("repeat-call")
	fixture.appendModelProposal(t, runID, proposal)
	terminal := activationEffectTerminalEnvelope(runID, "repeat-call")
	if err := fixture.runner.acceptEffectTerminal(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.pendingTerminal == nil || fixture.runner.active == nil ||
		fixture.runner.active.callID != "" {
		t.Fatalf("terminal-first state: active=%+v pending=%+v",
			fixture.runner.active, fixture.runner.pendingTerminal)
	}
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.active == nil || fixture.runner.pendingTerminal != nil ||
		fixture.runner.pendingDisposition == nil {
		t.Fatalf("terminal-first proposal crossed the commit barrier: active=%+v terminal=%+v disposition=%+v",
			fixture.runner.active, fixture.runner.pendingTerminal, fixture.runner.pendingDisposition)
	}
	if outcome := fixture.lastOutcome(t); outcome.Code == "effect_repetition_suppressed" {
		t.Fatalf("terminal outcome published before disposition commit: %+v", outcome)
	}
	fixture.commitDisposition(t)
	if fixture.runner.active != nil || fixture.runner.pendingDisposition != nil {
		t.Fatalf("terminal-first proposal was not released after commit: active=%+v disposition=%+v",
			fixture.runner.active, fixture.runner.pendingDisposition)
	}
	if outcome := fixture.lastOutcome(t); outcome.Code != "effect_repetition_suppressed" {
		t.Fatalf("terminal-first suppression outcome = %+v", outcome)
	}
}

func TestActivationPendingSuppressionContradictingNoProposalFailsClosed(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, "repeat-call")); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, nil,
	)); err == nil {
		t.Fatal("terminal/no-proposal contradiction was silently accepted")
	}
	if fixture.runner.active == nil || fixture.runner.pendingTerminal == nil {
		t.Fatalf("contradiction mutated fail-closed state: active=%+v pending=%+v",
			fixture.runner.active, fixture.runner.pendingTerminal)
	}
}

func TestActivationEffectTerminalIdentityMismatchDoesNotReleaseProposal(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{activationTestProposal("expected-call")},
	)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, "other-call")); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.active == nil || fixture.runner.active.callID != "expected-call" ||
		fixture.runner.pendingTerminal != nil {
		t.Fatalf("mismatched terminal released proposal: active=%+v pending=%+v",
			fixture.runner.active, fixture.runner.pendingTerminal)
	}
	if outcome := fixture.lastOutcome(t); outcome.Kind != policyelements.GenerationRefused ||
		outcome.Code != "effect_terminal_call_mismatch" {
		t.Fatalf("mismatched terminal outcome = %+v", outcome)
	}
}

func TestActivationSuppressionReplaysVisualThatPredatesNoEffect(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	visualEnvelope, visualCommit := fixture.appendVisual(t, "screen-changed", "warning changed", "user-task")
	if err := fixture.runner.acceptCommit(context.Background(), visualEnvelope); err != nil {
		t.Fatal(err)
	}
	proposal := activationTestProposal("repeat-call")
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	fixture.appendModelProposal(t, runID, proposal)
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, "repeat-call")); err != nil {
		t.Fatal(err)
	}
	if triggers := fixture.trigger.snapshot(); len(triggers) != 1 {
		t.Fatalf("suppression replayed retained visual before disposition commit: %+v", triggers)
	}
	dispositionCommit, dispositionItem := fixture.commitDisposition(t)
	triggers := fixture.trigger.snapshot()
	if len(triggers) != 2 {
		t.Fatalf("suppression did not replay retained visual: %+v", triggers)
	}
	replayed := triggers[1].Payload.(cognitionelements.Generate)
	if replayed.ExpectedContextVersion == nil ||
		*replayed.ExpectedContextVersion != dispositionCommit.Version ||
		replayed.ExpectedContextItemID != dispositionCommit.Context.StateItemID ||
		replayed.CommittedContext == nil ||
		!reflect.DeepEqual(*replayed.CommittedContext, dispositionCommit.Context) {
		t.Fatalf("replayed visual trigger = %+v, want disposition commit %+v", replayed, dispositionCommit)
	}
	if !slices.Contains(triggers[1].CausalParents, visualCommit.TrajectoryItemID) ||
		!slices.Contains(triggers[1].CausalParents, dispositionItem.ID) {
		t.Fatalf("replayed trigger parents = %v, want observation %q and disposition %q",
			triggers[1].CausalParents, visualCommit.TrajectoryItemID, dispositionItem.ID)
	}
	if fixture.runner.deferred != nil || fixture.runner.active == nil ||
		fixture.runner.active.id == runID {
		t.Fatalf("post-suppression replay state: deferred=%+v active=%+v",
			fixture.runner.deferred, fixture.runner.active)
	}
}

func TestActivationDeferredObservationAfterDispositionUsesItsOwnNewerPrefix(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	proposal := activationTestProposal("late-visual-call")
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	fixture.appendModelProposal(t, runID, proposal)
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, proposal.Call.CallID)); err != nil {
		t.Fatal(err)
	}
	reply, dispositionCommit, _ := fixture.prepareDispositionCommit(t)

	visualEnvelope, visualCommit := fixture.appendVisual(
		t, "screen-after-disposition", "warning changed after policy decision", "user-task",
	)
	if visualCommit.StoreVersion <= dispositionCommit.Version {
		t.Fatalf("test visual version %d is not newer than disposition %d",
			visualCommit.StoreVersion, dispositionCommit.Version)
	}
	if err := fixture.runner.acceptCommit(context.Background(), visualEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(fixture.trigger.snapshot()) != 1 || fixture.runner.deferred == nil {
		t.Fatalf("newer visual crossed pending acknowledgement: deferred=%+v triggers=%d",
			fixture.runner.deferred, len(fixture.trigger.snapshot()))
	}
	if err := fixture.runner.acceptDispositionCommit(context.Background(), reply); err != nil {
		t.Fatal(err)
	}

	triggers := fixture.trigger.snapshot()
	if len(triggers) != 2 {
		t.Fatalf("newer visual was not replayed after acknowledgement: %+v", triggers)
	}
	replayed := triggers[1].Payload.(cognitionelements.Generate)
	if replayed.ExpectedContextVersion == nil ||
		*replayed.ExpectedContextVersion != visualCommit.StoreVersion ||
		replayed.ExpectedContextItemID != visualCommit.Context.StateItemID ||
		replayed.CommittedContext == nil ||
		!reflect.DeepEqual(*replayed.CommittedContext, visualCommit.Context) {
		t.Fatalf("newer visual replay = %+v, want own commit %+v", replayed, visualCommit)
	}
	if fixture.runner.active == nil ||
		fixture.runner.active.contextVersion != visualCommit.StoreVersion || fixture.runner.deferred != nil {
		t.Fatalf("newer visual replay state: active=%+v deferred=%+v",
			fixture.runner.active, fixture.runner.deferred)
	}
}

func TestActivationCancellationAndNewIntentCannotCrossDispositionBarrier(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	proposal := activationTestProposal("cancel-barrier-call")
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	fixture.appendModelProposal(t, runID, proposal)
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, proposal.Call.CallID)); err != nil {
		t.Fatal(err)
	}
	firstEnvelope, firstRequest := fixture.latestDispositionRequest(t)
	if err := fixture.runner.acceptCancel(context.Background(), element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: "cancel-at-disposition-barrier",
		SessionID: activationTestSession, Sequence: fixture.store.Snapshot().Version,
		Payload: policyelements.GenerationCancel{
			GenerationID: runID, Reason: "participant changed the task",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.active == nil || fixture.runner.pendingDisposition == nil ||
		fixture.runner.intent != nil {
		t.Fatalf("cancellation removed the append barrier: active=%+v disposition=%+v intent=%+v",
			fixture.runner.active, fixture.runner.pendingDisposition, fixture.runner.intent)
	}

	replacementEnvelope, replacementCommit := fixture.appendUser(
		t, "replacement-task", "click a different control",
	)
	if err := fixture.runner.acceptCommit(context.Background(), replacementEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(fixture.trigger.snapshot()) != 1 || fixture.runner.deferred == nil ||
		fixture.runner.intent == nil || fixture.runner.intent.itemID != "replacement-task" {
		t.Fatalf("replacement intent crossed pending acknowledgement: intent=%+v deferred=%+v triggers=%d",
			fixture.runner.intent, fixture.runner.deferred, len(fixture.trigger.snapshot()))
	}
	if err := fixture.runner.acceptDispositionRejection(context.Background(),
		dispositionRejectionEnvelope(
			firstEnvelope, firstRequest, "version_conflict", "replacement intent committed",
			fixture.store.Snapshot().Version,
		)); err != nil {
		t.Fatal(err)
	}
	dispositionCommit, dispositionItem := fixture.commitDisposition(t)
	triggers := fixture.trigger.snapshot()
	if len(triggers) != 2 {
		t.Fatalf("replacement intent did not activate after acknowledgement: %+v", triggers)
	}
	replayed := triggers[1].Payload.(cognitionelements.Generate)
	if replayed.ExpectedContextVersion == nil ||
		*replayed.ExpectedContextVersion != dispositionCommit.Version ||
		replayed.ExpectedContextItemID != dispositionCommit.Context.StateItemID ||
		!slices.Contains(triggers[1].CausalParents, replacementCommit.TrajectoryItemID) ||
		!slices.Contains(triggers[1].CausalParents, dispositionItem.ID) {
		t.Fatalf("replacement replay = %+v parents=%v, want disposition %d and replacement %q",
			replayed, triggers[1].CausalParents, dispositionCommit.Version,
			replacementCommit.TrajectoryItemID)
	}
	if fixture.runner.active == nil || fixture.runner.active.id == runID ||
		fixture.runner.pendingDisposition != nil {
		t.Fatalf("replacement state after acknowledgement: active=%+v disposition=%+v",
			fixture.runner.active, fixture.runner.pendingDisposition)
	}
}

func TestActivationToolPolicySuppressionReplaysDeferredVisualInEitherArrivalOrder(t *testing.T) {
	for _, terminalFirst := range []bool{false, true} {
		name := "model_result_before_terminal"
		if terminalFirst {
			name = "terminal_before_model_result"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newActivationTestFixture(t)
			runID, contextVersion := fixture.startGeneration(
				t, "user-task", "wait for the threshold without taking a placeholder action",
			)
			visualEnvelope, visualCommit := fixture.appendVisual(
				t, "screen-84", "84 C; threshold exceeded", "user-task",
			)
			if err := fixture.runner.acceptCommit(context.Background(), visualEnvelope); err != nil {
				t.Fatal(err)
			}
			proposal := activationTestProposal("placeholder-wait")
			proposal.Call.Name = "computer.wait"
			proposal.Call.Arguments = json.RawMessage(`{"duration_ms":1000}`)
			fixture.appendModelProposal(t, runID, proposal)
			terminal := activationEffectTerminalEnvelopeWithKind(
				runID, proposal.Call.CallID, actionelements.PreEffectToolPolicySuppressed,
			)
			if terminalFirst {
				if err := fixture.runner.acceptEffectTerminal(context.Background(), terminal); err != nil {
					t.Fatal(err)
				}
			}
			if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
				runID, contextVersion, []cognitionelements.ToolProposal{proposal},
			)); err != nil {
				t.Fatal(err)
			}
			if !terminalFirst {
				if err := fixture.runner.acceptEffectTerminal(context.Background(), terminal); err != nil {
					t.Fatal(err)
				}
			}
			if triggers := fixture.trigger.snapshot(); len(triggers) != 1 {
				t.Fatalf("tool-policy suppression replayed before disposition commit: %+v", triggers)
			}
			dispositionCommit, dispositionItem := fixture.commitDisposition(t)

			triggers := fixture.trigger.snapshot()
			if len(triggers) != 2 {
				t.Fatalf("tool-policy suppression did not replay retained visual: %+v", triggers)
			}
			replayed := triggers[1].Payload.(cognitionelements.Generate)
			if replayed.ExpectedContextVersion == nil ||
				*replayed.ExpectedContextVersion != dispositionCommit.Version ||
				replayed.ExpectedContextItemID != dispositionCommit.Context.StateItemID ||
				replayed.CommittedContext == nil ||
				!reflect.DeepEqual(*replayed.CommittedContext, dispositionCommit.Context) {
				t.Fatalf("tool-policy replay = %+v, want disposition commit %+v", replayed, dispositionCommit)
			}
			if !slices.Contains(triggers[1].CausalParents, visualCommit.TrajectoryItemID) ||
				!slices.Contains(triggers[1].CausalParents, dispositionItem.ID) {
				t.Fatalf("tool-policy replay parents = %v, want observation %q and disposition %q",
					triggers[1].CausalParents, visualCommit.TrajectoryItemID, dispositionItem.ID)
			}
			if fixture.runner.deferred != nil || fixture.runner.pendingTerminal != nil ||
				fixture.runner.active == nil || fixture.runner.active.id == runID {
				t.Fatalf("tool-policy replay state: deferred=%+v pending=%+v active=%+v",
					fixture.runner.deferred, fixture.runner.pendingTerminal, fixture.runner.active)
			}
			foundTerminalOutcome := false
			for _, envelope := range fixture.outcome.snapshot() {
				outcome, ok := envelope.Payload.(policyelements.GenerationOutcome)
				if ok && outcome.GenerationID == runID &&
					outcome.Code == "effect_tool_policy_suppressed" {
					foundTerminalOutcome = true
				}
			}
			if !foundTerminalOutcome {
				t.Fatal("activation did not publish the typed tool-policy terminal outcome")
			}
		})
	}
}

func TestActivationCancellationClearsPendingSuppressionAndLateInputsCannotRevive(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	terminal := activationEffectTerminalEnvelope(runID, "repeat-call")
	if err := fixture.runner.acceptEffectTerminal(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runner.acceptCancel(context.Background(), element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: "cancel-suppression",
		SessionID: activationTestSession, Sequence: 100,
		Payload: policyelements.GenerationCancel{GenerationID: runID, Reason: "participant canceled"},
	}); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.pendingTerminal != nil || fixture.runner.active != nil || fixture.runner.intent != nil {
		t.Fatalf("cancellation retained activation state: pending=%+v active=%+v intent=%+v",
			fixture.runner.pendingTerminal, fixture.runner.active, fixture.runner.intent)
	}
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{activationTestProposal("repeat-call")},
	)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.runner.acceptEffectTerminal(context.Background(), terminal); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.pendingTerminal != nil || fixture.runner.active != nil || fixture.runner.intent != nil ||
		len(fixture.trigger.snapshot()) != 1 {
		t.Fatalf("late suppression inputs revived state: pending=%+v active=%+v intent=%+v triggers=%d",
			fixture.runner.pendingTerminal, fixture.runner.active, fixture.runner.intent,
			len(fixture.trigger.snapshot()))
	}
}

func TestActivationNewIntentStartsNormallyAfterSuppression(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	proposal := activationTestProposal("repeat-call")
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	fixture.appendModelProposal(t, runID, proposal)
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, "repeat-call")); err != nil {
		t.Fatal(err)
	}
	fixture.commitDisposition(t)
	replacementEnvelope, replacementCommit := fixture.appendUser(t, "replacement-task", "click a different control")
	if err := fixture.runner.acceptCommit(context.Background(), replacementEnvelope); err != nil {
		t.Fatal(err)
	}
	if len(fixture.trigger.snapshot()) != 2 || fixture.runner.active == nil ||
		fixture.runner.active.contextVersion != replacementCommit.StoreVersion ||
		fixture.runner.intent == nil || fixture.runner.intent.itemID != "replacement-task" {
		t.Fatalf("replacement intent did not activate normally: active=%+v intent=%+v triggers=%d",
			fixture.runner.active, fixture.runner.intent, len(fixture.trigger.snapshot()))
	}
}

func TestActivationReplacementIntentClearsTerminalPendingModelResult(t *testing.T) {
	fixture := newActivationTestFixture(t)
	runID, contextVersion := fixture.startGeneration(t, "user-task", "click the warning once")
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, "repeat-call")); err != nil {
		t.Fatal(err)
	}
	replacementEnvelope, replacementCommit := fixture.appendUser(t, "replacement-task", "click a different control")
	if err := fixture.runner.acceptCommit(context.Background(), replacementEnvelope); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.pendingTerminal != nil || fixture.runner.active == nil ||
		fixture.runner.active.id == runID || fixture.runner.active.contextVersion != replacementCommit.StoreVersion ||
		fixture.runner.intent == nil || fixture.runner.intent.itemID != "replacement-task" {
		t.Fatalf("replacement did not clear old pending terminal: pending=%+v active=%+v intent=%+v",
			fixture.runner.pendingTerminal, fixture.runner.active, fixture.runner.intent)
	}
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, contextVersion, []cognitionelements.ToolProposal{activationTestProposal("repeat-call")},
	)); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.active == nil || fixture.runner.active.contextVersion != replacementCommit.StoreVersion {
		t.Fatalf("late old result disturbed replacement generation: %+v", fixture.runner.active)
	}
}

func TestActivationCancellationAndNewIntentClearDeferredVisual(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		fixture := newActivationTestFixture(t)
		firstRun, firstVersion := fixture.startGeneration(t, "user-task", "watch the display")
		visualEnvelope, _ := fixture.appendVisual(t, "screen-changed", "display changed", "user-task")
		if err := fixture.runner.acceptCommit(context.Background(), visualEnvelope); err != nil {
			t.Fatal(err)
		}
		if fixture.runner.deferred == nil {
			t.Fatal("visual was not deferred before cancellation")
		}
		if err := fixture.runner.acceptCancel(context.Background(), element.Envelope{
			Type: policyelements.GenerationCancelType(), ItemID: "cancel-user-task",
			SessionID: activationTestSession, Sequence: 100,
			Payload: policyelements.GenerationCancel{
				GenerationID: firstRun, Reason: "participant interrupted",
			},
		}); err != nil {
			t.Fatal(err)
		}
		if fixture.runner.deferred != nil || fixture.runner.active != nil || fixture.runner.intent != nil {
			t.Fatalf("state after cancellation: deferred=%+v active=%+v intent=%+v",
				fixture.runner.deferred, fixture.runner.active, fixture.runner.intent)
		}
		// The already terminal run may still return. It must not revive the
		// retained visual or create a second generation.
		if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
			firstRun, firstVersion, nil,
		)); err != nil {
			t.Fatal(err)
		}
		if triggers := fixture.trigger.snapshot(); len(triggers) != 1 {
			t.Fatalf("late canceled result replayed visual: %+v", triggers)
		}
	})

	t.Run("new durable intent", func(t *testing.T) {
		fixture := newActivationTestFixture(t)
		fixture.startGeneration(t, "user-task", "watch the display")
		visualEnvelope, _ := fixture.appendVisual(t, "screen-old-task", "old task state", "user-task")
		if err := fixture.runner.acceptCommit(context.Background(), visualEnvelope); err != nil {
			t.Fatal(err)
		}
		newIntentEnvelope, _ := fixture.appendUser(t, "replacement-task", "do something else")
		if err := fixture.runner.acceptCommit(context.Background(), newIntentEnvelope); err != nil {
			t.Fatal(err)
		}
		if fixture.runner.deferred != nil || fixture.runner.intent == nil ||
			fixture.runner.intent.itemID != "replacement-task" {
			t.Fatalf("state after replacement intent: deferred=%+v intent=%+v",
				fixture.runner.deferred, fixture.runner.intent)
		}
		if outcome := fixture.lastOutcome(t); outcome.Code != "generation_pending" {
			t.Fatalf("replacement intent outcome = %+v", outcome)
		}
	})
}

type activationTestFixture struct {
	runner    *activationRunner
	store     *trajectory.Store
	append    *recordingOutputPort
	trigger   *recordingOutputPort
	authority *recordingOutputPort
	state     *recordingOutputPort
	outcome   *recordingOutputPort
	nextNS    uint64
	nextRev   uint64
}

func newActivationTestFixture(t *testing.T) *activationTestFixture {
	t.Helper()
	store := trajectory.NewStore()
	fixture := &activationTestFixture{
		store: store,
		append: &recordingOutputPort{
			name: "disposition_append", typeName: stateelements.AppendType(),
		},
		trigger: &recordingOutputPort{
			name: "trigger", typeName: cognitionelements.GenerateType(),
		},
		authority: &recordingOutputPort{
			name: "authority", typeName: authority.CandidateType(),
		},
		state: &recordingOutputPort{
			name: "state", typeName: policyelements.GenerationStateType(),
		},
		outcome: &recordingOutputPort{
			name: "outcome", typeName: policyelements.GenerationOutcomeType(),
		},
	}
	fixture.runner = &activationRunner{
		instance: "activation-test",
		config: policyelements.GenerateOnObservationConfig{
			Role: "computer-use",
			Invocation: continuation.Invocation{
				Instruction: "act on the durable user task", MaxOutputTokens: 64,
			},
			TerminalMemory: 32, CancelMemory: 16,
		},
		clock: graphruntime.ClockFunc(func() uint64 {
			fixture.nextNS++
			return fixture.nextNS
		}),
		sequences: graphruntime.NewSequenceAllocator(), store: store,
		ports: activationPorts{
			trigger: fixture.trigger, candidate: fixture.authority,
			state: fixture.state, outcome: fixture.outcome,
			dispositionAppend: fixture.append,
		},
		terminal: make(map[string]struct{}),
		state: policyelements.GenerationState{
			Role: "computer-use", TerminalMemory: 32, CancellationMemory: 16,
		},
	}
	fixture.appendRaw(t, fixture.visualItem("screen-72", "72 C", ""))
	return fixture
}

func (fixture *activationTestFixture) appendModelProposal(
	t *testing.T, runID string, proposal cognitionelements.ToolProposal,
) trajectory.Item {
	t.Helper()
	snapshot := fixture.store.Snapshot()
	if fixture.runner.active == nil || fixture.runner.active.id != runID ||
		fixture.runner.active.contextVersion == 0 ||
		fixture.runner.active.contextVersion > snapshot.Version {
		t.Fatalf("cannot append proposal for inactive run %q: %+v", runID, fixture.runner.active)
	}
	contextTail := snapshot.Items[fixture.runner.active.contextVersion-1]
	call := proposal.Call
	call.Arguments = append(json.RawMessage(nil), proposal.Call.Arguments...)
	item := trajectory.Item{
		ID:   "canonical-proposal-" + proposal.Call.CallID,
		Kind: trajectory.KindToolProposal, MonotonicNS: fixture.nextMonotonicNS(),
		CausalParentIDs: []string{contextTail.ID}, SourceRevision: contextTail.SourceRevision,
		InvocationID: runID, Producer: trajectory.Producer{Phase: trajectory.PhaseFast},
		ToolCall: &call,
	}
	fixture.appendRaw(t, item)
	return item
}

func (fixture *activationTestFixture) commitDisposition(
	t *testing.T,
) (stateelements.Commit, trajectory.Item) {
	t.Helper()
	reply, commit, item := fixture.prepareDispositionCommit(t)
	if err := fixture.runner.acceptDispositionCommit(context.Background(), reply); err != nil {
		t.Fatal(err)
	}
	return commit, item
}

func (fixture *activationTestFixture) latestDispositionRequest(
	t *testing.T,
) (element.Envelope, stateelements.Append) {
	t.Helper()
	requests := fixture.append.snapshot()
	if len(requests) == 0 {
		t.Fatal("activation emitted no proposal-disposition append")
	}
	requestEnvelope := requests[len(requests)-1]
	request, ok := requestEnvelope.Payload.(stateelements.Append)
	if !ok || !request.Compare || len(request.Items) != 1 {
		t.Fatalf("proposal-disposition request = %#v", requestEnvelope.Payload)
	}
	return requestEnvelope, request
}

func (fixture *activationTestFixture) prepareDispositionCommit(
	t *testing.T,
) (element.Envelope, stateelements.Commit, trajectory.Item) {
	t.Helper()
	requestEnvelope, request := fixture.latestDispositionRequest(t)
	if err := fixture.store.AppendBatchAt(request.ExpectedVersion, request.Items); err != nil {
		t.Fatalf("commit proposal disposition: %v", err)
	}
	snapshot, prefix, err := fixture.store.SnapshotWithPrefixIdentity()
	if err != nil {
		t.Fatal(err)
	}
	commit := stateelements.Commit{
		Version: snapshot.Version, AppendedIDs: []string{request.Items[0].ID}, Snapshot: snapshot,
		Context: stateelements.CommittedContext{
			Prefix: prefix, StateItemID: fmt.Sprintf("trajectory-state-%d", snapshot.Version),
		},
	}
	reply := requestEnvelope.Clone()
	reply.Type = stateelements.CommitType()
	reply.ItemID = requestEnvelope.ItemID + ":committed"
	reply.CausalParents = appendUniqueString(reply.CausalParents, requestEnvelope.ItemID)
	reply.Payload = commit
	return reply, commit, request.Items[0]
}

func dispositionRejectionEnvelope(
	requestEnvelope element.Envelope, request stateelements.Append,
	code, message string, currentVersion uint64,
) element.Envelope {
	reply := requestEnvelope.Clone()
	reply.Type = stateelements.RejectionType()
	reply.ItemID = requestEnvelope.ItemID + ":rejected"
	reply.CausalParents = appendUniqueString(reply.CausalParents, requestEnvelope.ItemID)
	reply.Payload = stateelements.Rejection{
		Code: code, Message: message,
		ExpectedVersion: request.ExpectedVersion, CurrentVersion: currentVersion,
	}
	return reply
}

func (fixture *activationTestFixture) startGeneration(
	t *testing.T, itemID, content string,
) (string, uint64) {
	t.Helper()
	envelope, commit := fixture.appendUser(t, itemID, content)
	if err := fixture.runner.acceptCommit(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	triggers := fixture.trigger.snapshot()
	if len(triggers) == 0 {
		t.Fatal("user intent did not activate cognition")
	}
	return triggers[len(triggers)-1].RunID, commit.StoreVersion
}

func (fixture *activationTestFixture) appendUser(
	t *testing.T, itemID, content string,
) (element.Envelope, stateelements.ObservationCommitOutcome) {
	t.Helper()
	fixture.nextRev++
	item := trajectory.Item{
		ID: itemID, Kind: trajectory.KindObservation, MonotonicNS: fixture.nextMonotonicNS(),
		SourceRevision: fixture.nextRev, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content: content,
		Event: &trajectory.EventMetadata{
			EventID: itemID + "-event", Type: "microphone.endpoint", Source: "microphone",
			Channel: SourceMicrophone, OccurredNS: fixture.nextNS,
		},
	}
	return fixture.appendObservation(t, item, "microphone-stream")
}

func (fixture *activationTestFixture) appendVisual(
	t *testing.T, itemID, content, intentID string,
) (element.Envelope, stateelements.ObservationCommitOutcome) {
	t.Helper()
	return fixture.appendObservation(t, fixture.visualItem(itemID, content, intentID), "screen-stream")
}

func (fixture *activationTestFixture) visualItem(itemID, content, intentID string) trajectory.Item {
	fixture.nextRev++
	parents := []string(nil)
	if intentID != "" {
		parents = []string{intentID}
	}
	return trajectory.Item{
		ID: itemID, Kind: trajectory.KindObservation, MonotonicNS: fixture.nextMonotonicNS(),
		CausalParentIDs: parents, SourceRevision: fixture.nextRev,
		Producer: trajectory.Producer{Phase: trajectory.PhaseObserver, Provider: "vision"},
		Content:  content,
		Observation: &trajectory.ObservationMeta{
			Observer: "vision", Source: SourceScreen, Authority: trajectory.AuthorityObserver,
		},
		Event: &trajectory.EventMetadata{
			EventID: itemID + "-event", Type: "vision.endpoint", Source: "vision",
			Channel: SourceScreen, OccurredNS: fixture.nextNS,
		},
	}
}

func (fixture *activationTestFixture) appendObservation(
	t *testing.T, item trajectory.Item, streamID string,
) (element.Envelope, stateelements.ObservationCommitOutcome) {
	t.Helper()
	fixture.appendRaw(t, item)
	snapshot := fixture.store.Snapshot()
	prefix, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	commit := stateelements.ObservationCommitOutcome{
		Kind: stateelements.ObservationCommitted, TriggerItemID: item.Event.EventID,
		TrajectoryItemID: item.ID, StreamID: streamID, ObservationRevision: item.SourceRevision,
		SourceRevision: item.SourceRevision, StoreVersion: snapshot.Version,
		Context: stateelements.CommittedContext{
			Prefix: prefix, StateItemID: fmt.Sprintf("trajectory-state-%d", snapshot.Version),
		},
	}
	return element.Envelope{
		Type: stateelements.ObservationCommitOutcomeType(), ItemID: item.ID + "-commit",
		SessionID: activationTestSession, Sequence: snapshot.Version, Payload: commit,
	}, commit
}

func (fixture *activationTestFixture) appendInstruction(t *testing.T, itemID string) {
	t.Helper()
	fixture.appendRaw(t, trajectory.Item{
		ID: itemID, Kind: trajectory.KindInstruction, MonotonicNS: fixture.nextMonotonicNS(),
		Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, Content: "unrelated runtime state",
	})
}

func (fixture *activationTestFixture) appendRaw(t *testing.T, item trajectory.Item) {
	t.Helper()
	if err := fixture.store.Append(item); err != nil {
		t.Fatalf("append trajectory item %s: %v", item.ID, err)
	}
}

func (fixture *activationTestFixture) nextMonotonicNS() uint64 {
	fixture.nextNS++
	return fixture.nextNS
}

func (fixture *activationTestFixture) lastOutcome(t *testing.T) policyelements.GenerationOutcome {
	t.Helper()
	envelopes := fixture.outcome.snapshot()
	if len(envelopes) == 0 {
		t.Fatal("activation published no outcome")
	}
	outcome, ok := envelopes[len(envelopes)-1].Payload.(policyelements.GenerationOutcome)
	if !ok {
		t.Fatalf("activation outcome payload = %T", envelopes[len(envelopes)-1].Payload)
	}
	return outcome
}

func activationResultEnvelope(
	runID string, contextVersion uint64, proposals []cognitionelements.ToolProposal,
) element.Envelope {
	return element.Envelope{
		Type: interactionelements.SafeModelResultType(), ItemID: runID + "-result",
		SessionID: activationTestSession, RunID: runID,
		Payload: cognitionelements.Result{
			RunID: runID, ContextVersion: contextVersion, ToolProposals: proposals,
		},
	}
}

func activationTestProposal(callID string) cognitionelements.ToolProposal {
	return cognitionelements.ToolProposal{
		Call: trajectory.ToolCall{
			CallID: callID, Name: "computer.click", Arguments: json.RawMessage(`{"x":1,"y":2}`),
		},
		Declared: true, ProviderAuthority: continuation.ToolAuthorityPropose,
	}
}

func activationEffectTerminalEnvelope(runID, callID string) element.Envelope {
	return activationEffectTerminalEnvelopeWithKind(
		runID, callID, actionelements.PreEffectRepetitionSuppressed,
	)
}

func activationEffectTerminalEnvelopeWithKind(
	runID, callID string, kind actionelements.PreEffectTerminalKind,
) element.Envelope {
	return element.Envelope{
		Type: actionelements.PreEffectTerminalType(), ItemID: runID + "-" + callID + "-terminal",
		SessionID: activationTestSession, RunID: runID,
		Payload: actionelements.PreEffectTerminal{
			Kind: kind, CallID: callID,
		},
	}
}
