package realtimecu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

func TestActivationConfigRequiresIndependentTemporalAdmissionContract(t *testing.T) {
	validator := activationFactory{}
	for _, source := range []string{
		`{"role":"computer-use","invocation":{"instruction":"act"},"expected_admission":{"mode":"immediate"}}`,
		`{"role":"computer-use","invocation":{"instruction":"act"},"expected_admission":{"mode":"after_intent","source_set":"observed_before_intent"},"expected_settlement":{"expected_admission":{"mode":"after_intent","source_set":"observed_before_intent"},"candidate_sources":[{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}}`,
		`{"role":"computer-use","invocation":{"instruction":"act"},"expected_admission":{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"camera"}]},"expected_settlement":{"expected_admission":{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"camera"}]},"candidate_sources":[{"observer":"vision","source":"camera"}],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}}`,
	} {
		if err := validator.ValidateConfig(json.RawMessage(source)); err != nil {
			t.Errorf("valid activation config %s: %v", source, err)
		}
	}
	for _, source := range []string{
		`{"role":"computer-use","invocation":{"instruction":"act"}}`,
		`{"role":"computer-use","invocation":{"instruction":"act"},"expected_admission":{"mode":"immediate"},"expected_settlement":{"expected_admission":{"mode":"immediate"},"candidate_sources":[{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}}`,
		`{"role":"computer-use","invocation":{"instruction":"act"},"expected_admission":{"mode":"after_intent","source_set":"observed_before_intent"},"expected_settlement":null}`,
		`{"role":"computer-use","invocation":{"instruction":"act"},"expected_admission":{"mode":"after_intent","source_set":"observed_before_intent"},"expected_settlement":{"expected_admission":{"mode":"after_intent","source_set":"explicit","required":[{"observer":"vision","source":"screen"}]},"candidate_sources":[{"observer":"vision","source":"screen"}],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}}`,
		`{"role":"computer-use","invocation":{"instruction":"act"},"expected_admission":{"mode":"after_intent","source_set":"observed_before_intent"},"expected_settlement":{"expected_admission":{"mode":"after_intent","source_set":"observed_before_intent"},"candidate_sources":[],"detector":{"reference":"settlement-primary","revision":"v1","configuration_digest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}}`,
	} {
		if err := validator.ValidateConfig(json.RawMessage(source)); err == nil {
			t.Errorf("invalid activation config was accepted: %s", source)
		}
	}
}

func TestActivationMountRequiresTrustedTrajectorySession(t *testing.T) {
	services := graphruntime.NewServiceSet()
	for name, value := range map[string]any{
		graphruntime.ClockServiceName:        graphruntime.ClockFunc(func() uint64 { return 1 }),
		graphruntime.SequenceServiceName:     graphruntime.NewSequenceAllocator(),
		stateelements.TrajectoryStoreService: &stateelements.TrajectoryStoreServiceValue{Store: trajectory.NewStore()},
	} {
		if _, err := services.Set(name, value); err != nil {
			t.Fatal(err)
		}
	}
	_, err := (activationFactory{}).Mount(context.Background(), element.MountContext{
		InstanceID: "activation", Services: services,
		Config: json.RawMessage(
			`{"role":"computer-use","invocation":{"instruction":"act"},"expected_admission":{"mode":"immediate"}}`,
		),
	})
	if err == nil || !strings.Contains(err.Error(), "trusted canonical session ID") {
		t.Fatalf("activation mount without trusted trajectory session error = %v", err)
	}
}

func TestActivationSettlementWiringRequiresConfigurationAndBothLanes(t *testing.T) {
	for _, testCase := range []struct {
		name                   string
		configured             bool
		settlementInputs       int
		acknowledgementOutputs int
		wantError              bool
	}{
		{name: "legacy unwired", configured: false},
		{name: "configured handshake", configured: true, settlementInputs: 1, acknowledgementOutputs: 1},
		{name: "configured without input", configured: true, acknowledgementOutputs: 1, wantError: true},
		{name: "configured without acknowledgement", configured: true, settlementInputs: 1, wantError: true},
		{name: "configured without lanes", configured: true, wantError: true},
		{name: "unconfigured input", settlementInputs: 1, wantError: true},
		{name: "unconfigured acknowledgement", acknowledgementOutputs: 1, wantError: true},
		{name: "unconfigured half handshake", settlementInputs: 1, acknowledgementOutputs: 1, wantError: true},
		{name: "multiple inputs", configured: true, settlementInputs: 2, acknowledgementOutputs: 1, wantError: true},
		{name: "multiple acknowledgements", configured: true, settlementInputs: 1, acknowledgementOutputs: 2, wantError: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateActivationSettlementWiring(
				testCase.configured, testCase.settlementInputs, testCase.acknowledgementOutputs,
			)
			if (err != nil) != testCase.wantError {
				t.Fatalf("settlement wiring validation error = %v, wantError %t", err, testCase.wantError)
			}
		})
	}
}

func TestActivationRejectsAdmissionThatDiffersFromPinnedTemporalContract(t *testing.T) {
	fixture := newActivationTestFixture(t)
	fixture.runner.config.ExpectedAdmission = policyelements.TemporalEvidenceAdmissionConfig{
		Mode:      policyelements.TemporalEvidenceAdmissionAfterIntent,
		SourceSet: policyelements.TemporalEvidenceSourceSetObservedBeforeIntent,
	}
	intentEnvelope, intentCommit := fixture.appendUser(t, "contract-intent", "watch the display")
	intentIdentity := intentEnvelope.Payload.(policyelements.AdmittedTemporalEvidence).TriggerObservation
	freshEnvelope, _ := fixture.appendVisual(
		t, "contract-screen", "ready", intentCommit.TrajectoryItemID,
	)
	honest := afterIntentAdmissionEnvelope(freshEnvelope, intentIdentity)

	for _, testCase := range []struct {
		name   string
		mutate func(*policyelements.AdmittedTemporalEvidence)
	}{
		{name: "mode downgrade", mutate: func(value *policyelements.AdmittedTemporalEvidence) {
			value.Mode = policyelements.TemporalEvidenceAdmissionImmediate
			value.SourceSet = ""
			value.DurableIntent = nil
			value.QualifyingObservations = nil
		}},
		{name: "source-set replacement", mutate: func(value *policyelements.AdmittedTemporalEvidence) {
			value.SourceSet = policyelements.TemporalEvidenceSourceSetExplicit
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			forged := honest.Clone()
			admission := forged.Payload.(policyelements.AdmittedTemporalEvidence)
			testCase.mutate(&admission)
			forged.Payload = admission
			if err := fixture.runner.acceptAdmission(context.Background(), forged); err != nil {
				t.Fatal(err)
			}
			if outcome := fixture.lastOutcome(t); outcome.Kind != policyelements.GenerationRefused ||
				outcome.Code != "invalid_temporal_admission" {
				t.Fatalf("forged admission outcome = %+v", outcome)
			}
			if triggers := fixture.trigger.snapshot(); len(triggers) != 0 {
				t.Fatalf("forged admission activated cognition: %+v", triggers)
			}
		})
	}
	if err := fixture.runner.acceptAdmission(context.Background(), honest); err != nil {
		t.Fatal(err)
	}
	if triggers := fixture.trigger.snapshot(); len(triggers) != 1 {
		t.Fatalf("honest pinned admission triggers = %+v", triggers)
	}
}

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
	visualAdmission := visualEnvelope.Payload.(policyelements.AdmittedTemporalEvidence)
	visualEnvelope.Payload = &visualAdmission
	if err := fixture.runner.acceptAdmission(context.Background(), visualEnvelope); err != nil {
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
	visualAdmission.TriggerCommit.StoreVersion = 999
	visualAdmission.TriggerCommit.Context.Prefix.Digest = "mutated-by-caller"
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
	if err := fixture.runner.acceptAdmission(context.Background(), firstEnvelope); err != nil {
		t.Fatal(err)
	}
	latestEnvelope, latestCommit := fixture.appendVisual(t, "screen-84", "84 C", "user-task")
	if err := fixture.runner.acceptAdmission(context.Background(), latestEnvelope); err != nil {
		t.Fatal(err)
	}
	// A delayed older lane must not replace the newer retained world state.
	if err := fixture.runner.acceptAdmission(context.Background(), firstEnvelope); err != nil {
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
	if err := fixture.runner.acceptAdmission(context.Background(), visualEnvelope); err != nil {
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
	if err := fixture.runner.acceptAdmission(context.Background(), preEffectEnvelope); err != nil {
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

func TestActivationSettlesDispositionWithoutSupersededDeferredAdmission(t *testing.T) {
	fixture := newActivationTestFixture(t)
	fixture.runner.config.ExpectedAdmission = policyelements.TemporalEvidenceAdmissionConfig{
		Mode:      policyelements.TemporalEvidenceAdmissionAfterIntent,
		SourceSet: policyelements.TemporalEvidenceSourceSetObservedBeforeIntent,
	}
	intentEnvelope, intentCommit := fixture.appendUser(t, "user-task", "click the warning")
	intentIdentity := intentEnvelope.Payload.(policyelements.AdmittedTemporalEvidence).TriggerObservation
	initialEnvelope, initialCommit := fixture.appendVisual(
		t, "screen-initial", "warning not visible", intentCommit.TrajectoryItemID,
	)
	initialEnvelope = afterIntentAdmissionEnvelope(initialEnvelope, intentIdentity)
	if err := fixture.runner.acceptAdmission(context.Background(), initialEnvelope); err != nil {
		t.Fatal(err)
	}
	runID := fixture.trigger.snapshot()[0].RunID

	visualEnvelope, visualCommit := fixture.appendVisual(
		t, "screen-warning", "warning visible", intentCommit.TrajectoryItemID,
	)
	visualEnvelope = afterIntentAdmissionEnvelope(visualEnvelope, intentIdentity)
	if err := fixture.runner.acceptAdmission(context.Background(), visualEnvelope); err != nil {
		t.Fatal(err)
	}
	proposal := activationTestProposal("superseded-deferred-call")
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, initialCommit.StoreVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	proposalItem := fixture.appendModelProposal(t, runID, proposal)
	replacementEnvelope, replacementCommit := fixture.appendUser(
		t, "replacement-task", "click a different warning",
	)
	// Simulate the replacement commit reaching the canonical store while the
	// old admitted visual is still queued behind the active proposal.
	if err := fixture.runner.acceptEffectTerminal(context.Background(),
		activationEffectTerminalEnvelope(runID, proposal.Call.CallID)); err != nil {
		t.Fatalf("superseded deferred admission killed disposition settlement: %v", err)
	}
	if fixture.runner.deferred != nil {
		t.Fatalf("superseded deferred admission was retained: %+v", fixture.runner.deferred)
	}
	_, request := fixture.latestDispositionRequest(t)
	if slices.Contains(request.Items[0].CausalParentIDs, visualCommit.TrajectoryItemID) ||
		!slices.Equal(request.Items[0].CausalParentIDs, []string{proposalItem.ID}) {
		t.Fatalf("disposition inherited superseded evidence: %v", request.Items[0].CausalParentIDs)
	}
	fixture.commitDisposition(t)
	if len(fixture.trigger.snapshot()) != 1 {
		t.Fatalf("superseded admission replayed after disposition: %+v", fixture.trigger.snapshot())
	}

	replacementIdentity := replacementEnvelope.Payload.(policyelements.AdmittedTemporalEvidence).TriggerObservation
	freshEnvelope, freshCommit := fixture.appendVisual(
		t, "replacement-screen", "different warning visible", replacementCommit.TrajectoryItemID,
	)
	freshEnvelope = afterIntentAdmissionEnvelope(freshEnvelope, replacementIdentity)
	if err := fixture.runner.acceptAdmission(context.Background(), freshEnvelope); err != nil {
		t.Fatal(err)
	}
	triggers := fixture.trigger.snapshot()
	if len(triggers) != 2 || fixture.runner.intent == nil ||
		fixture.runner.intent.itemID != replacementCommit.TrajectoryItemID {
		t.Fatalf("fresh replacement intent did not activate: intent=%+v triggers=%+v",
			fixture.runner.intent, triggers)
	}
	generate := triggers[1].Payload.(cognitionelements.Generate)
	if generate.ExpectedContextVersion == nil || *generate.ExpectedContextVersion != freshCommit.StoreVersion {
		t.Fatalf("replacement generation context = %+v, want %d", generate, freshCommit.StoreVersion)
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
	if err := fixture.runner.acceptAdmission(context.Background(), visualEnvelope); err != nil {
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
	if err := fixture.runner.acceptAdmission(context.Background(), visualEnvelope); err != nil {
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
	if err := fixture.runner.acceptAdmission(context.Background(), replacementEnvelope); err != nil {
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
			if err := fixture.runner.acceptAdmission(context.Background(), visualEnvelope); err != nil {
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
	if err := fixture.runner.acceptAdmission(context.Background(), replacementEnvelope); err != nil {
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
	if err := fixture.runner.acceptAdmission(context.Background(), replacementEnvelope); err != nil {
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
		if err := fixture.runner.acceptAdmission(context.Background(), visualEnvelope); err != nil {
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
		if err := fixture.runner.acceptAdmission(context.Background(), visualEnvelope); err != nil {
			t.Fatal(err)
		}
		newIntentEnvelope, _ := fixture.appendUser(t, "replacement-task", "do something else")
		if err := fixture.runner.acceptAdmission(context.Background(), newIntentEnvelope); err != nil {
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
	runner        *activationRunner
	store         *trajectory.Store
	append        *recordingOutputPort
	trigger       *recordingOutputPort
	authority     *recordingOutputPort
	state         *recordingOutputPort
	outcome       *recordingOutputPort
	settlementAck *recordingOutputPort
	nextNS        uint64
	nextRev       uint64
}

type activationSettlementScenario struct {
	decision policyelements.IntentSettlementDecision
	terminal element.Envelope
	evidence element.Envelope
}

type failOnceActivationOutputPort struct {
	delegate element.OutputPort
	failed   bool
	attempts int
	first    element.Envelope
}

func (port *failOnceActivationOutputPort) Name() string { return port.delegate.Name() }

func (port *failOnceActivationOutputPort) Type() element.Type { return port.delegate.Type() }

func (port *failOnceActivationOutputPort) Lanes() []element.Sender { return port.delegate.Lanes() }

func (port *failOnceActivationOutputPort) Broadcast(
	ctx context.Context, envelope element.Envelope,
) (element.SendResult, error) {
	port.attempts++
	if !port.failed {
		port.failed = true
		port.first = envelope.Clone()
		return element.SendResult{}, errors.New("injected settlement acknowledgement failure")
	}
	return port.delegate.Broadcast(ctx, envelope)
}

type undeliverableActivationOutputPort struct {
	delegate  element.OutputPort
	zero      bool
	attempts  int
	envelopes []element.Envelope
}

func (port *undeliverableActivationOutputPort) Name() string { return port.delegate.Name() }

func (port *undeliverableActivationOutputPort) Type() element.Type { return port.delegate.Type() }

func (port *undeliverableActivationOutputPort) Lanes() []element.Sender {
	return port.delegate.Lanes()
}

func (port *undeliverableActivationOutputPort) Broadcast(
	ctx context.Context, envelope element.Envelope,
) (element.SendResult, error) {
	port.attempts++
	port.envelopes = append(port.envelopes, envelope.Clone())
	if err := context.Cause(ctx); err != nil {
		return element.SendResult{}, err
	}
	if port.zero {
		return element.SendResult{}, nil
	}
	return element.SendResult{}, errors.New("injected persistent acknowledgement failure")
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
		settlementAck: &recordingOutputPort{
			name: "settlement_ack", typeName: policyelements.IntentSettlementAcknowledgementType(),
		},
	}
	fixture.runner = &activationRunner{
		instance: "activation-test", sessionID: activationTestSession,
		config: ActivationConfig{
			GenerateOnObservationConfig: policyelements.GenerateOnObservationConfig{
				Role: "computer-use",
				Invocation: continuation.Invocation{
					Instruction: "act on the durable user task", MaxOutputTokens: 64,
				},
				TerminalMemory: 32, CancelMemory: 16,
			},
			ExpectedAdmission: policyelements.TemporalEvidenceAdmissionConfig{
				Mode: policyelements.TemporalEvidenceAdmissionImmediate,
			},
		},
		clock: graphruntime.ClockFunc(func() uint64 {
			fixture.nextNS++
			return fixture.nextNS
		}),
		sequences: graphruntime.NewSequenceAllocator(), store: store,
		ports: activationPorts{
			trigger: fixture.trigger, candidate: fixture.authority,
			state: fixture.state, outcome: fixture.outcome,
			dispositionAppend:         fixture.append,
			settlementAcknowledgement: fixture.settlementAck,
		},
		terminal:               make(map[string]struct{}),
		canceledEffects:        make(map[string]*canceledActivationEffect),
		canceledIntents:        make(map[string]policyelements.TemporalEvidenceItemIdentity),
		pendingSettlementAcks:  make(map[string]pendingSettlementAcknowledgement),
		acknowledgedSettlement: make(map[string]struct{}),
		state: policyelements.GenerationState{
			Role: "computer-use", TerminalMemory: 32, CancellationMemory: 16,
		},
	}
	fixture.appendRaw(t, fixture.visualItem("screen-72", "72 C", ""))
	return fixture
}

func (fixture *activationTestFixture) prepareSettlementScenario(
	t *testing.T, kind policyelements.IntentSettlementDecisionKind,
) activationSettlementScenario {
	t.Helper()
	contract := policyelements.IntentSettlementConfig{
		ExpectedAdmission: policyelements.TemporalEvidenceAdmissionConfig{
			Mode:      policyelements.TemporalEvidenceAdmissionAfterIntent,
			SourceSet: policyelements.TemporalEvidenceSourceSetObservedBeforeIntent,
		},
		CandidateSources: []policyelements.TemporalEvidenceRequirement{
			{Observer: "vision", Source: SourceScreen},
		},
		Detector: policyelements.IntentDetectorIdentity{
			Reference: "settlement-primary", Revision: "v1",
			ConfigurationDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	fixture.runner.config.ExpectedAdmission = contract.ExpectedAdmission
	fixture.runner.config.ExpectedSettlement = &contract

	intentEnvelope, intentCommit := fixture.appendUser(t, "settlement-intent", "click the warning")
	intent := intentEnvelope.Payload.(policyelements.AdmittedTemporalEvidence).TriggerObservation
	initialEnvelope, initialCommit := fixture.appendVisual(
		t, "settlement-initial-screen", "warning visible", intentCommit.TrajectoryItemID,
	)
	initialEnvelope = afterIntentAdmissionEnvelope(initialEnvelope, intent)
	if err := fixture.runner.acceptAdmission(context.Background(), initialEnvelope); err != nil {
		t.Fatal(err)
	}
	runID := fixture.runner.active.id
	proposal := activationTestProposal("settlement-call")
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		runID, initialCommit.StoreVersion, []cognitionelements.ToolProposal{proposal},
	)); err != nil {
		t.Fatal(err)
	}
	proposalItem := fixture.appendModelProposal(t, runID, proposal)
	call := proposal.Call
	call.Arguments = append(json.RawMessage(nil), proposal.Call.Arguments...)
	callItem := trajectory.Item{
		ID: "settlement-call-item", Kind: trajectory.KindToolCall,
		MonotonicNS: fixture.nextMonotonicNS(), CausalParentIDs: []string{proposalItem.ID},
		SourceRevision: proposalItem.SourceRevision, InvocationID: runID,
		Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, ToolCall: &call,
	}
	fixture.appendRaw(t, callItem)
	resultItem := trajectory.Item{
		ID: "settlement-result-item", Kind: trajectory.KindToolResult,
		MonotonicNS: fixture.nextMonotonicNS(), CausalParentIDs: []string{callItem.ID},
		SourceRevision: proposalItem.SourceRevision, InvocationID: runID,
		Producer: trajectory.Producer{Phase: trajectory.PhaseTool},
		ToolResult: &trajectory.ToolResult{
			CallID: call.CallID, Name: call.Name, Output: json.RawMessage(`{"ok":true}`),
		},
	}
	fixture.appendRaw(t, resultItem)
	consequence := fixture.visualItem(
		"settlement-consequence", "warning dismissed", intentCommit.TrajectoryItemID,
	)
	consequence.CausalParentIDs = []string{intentCommit.TrajectoryItemID, resultItem.ID}
	consequenceEnvelope, _ := fixture.appendObservation(t, consequence, "screen-stream")
	consequenceEnvelope = afterIntentAdmissionEnvelope(consequenceEnvelope, intent)
	evidence := consequenceEnvelope.Payload.(policyelements.AdmittedTemporalEvidence)
	resultVersion := evidence.TriggerCommit.StoreVersion - 1
	issuedNS := fixture.nextMonotonicNS()
	probe := policyelements.IntentSettlementProbe{
		Issuer: "settlement", Sequence: 1, SessionID: activationTestSession,
		Evidence: evidence, DurableIntent: intent,
		TriggerObservation: evidence.TriggerObservation,
		Result: policyelements.IntentSettlementResultIdentity{
			TrajectoryItemID: resultItem.ID, StoreVersion: resultVersion,
			InvocationID: runID, CallID: call.CallID, Tool: call.Name,
		},
		Prefix: evidence.Prefix, Detector: contract.Detector, IssuedNS: issuedNS,
	}
	probe.ProbeID = activationTestSettlementProbeID(t, probe)
	dispositionKind := policyelements.IntentDispositionSucceeded
	if kind == policyelements.IntentSettlementDecisionFailed {
		dispositionKind = policyelements.IntentDispositionFailed
	}
	disposition := policyelements.IntentDisposition{
		Probe: probe, Detector: contract.Detector, Kind: dispositionKind,
		DecisionStartedNS:  fixture.nextMonotonicNS(),
		DecisionFinishedNS: fixture.nextMonotonicNS(),
	}
	decision := policyelements.IntentSettlementDecision{
		Kind: kind, SessionID: activationTestSession, Evidence: evidence, Probe: probe,
		Disposition: &disposition, InvocationID: runID,
		StateRevisionBefore: 4, StateRevisionAfter: 5,
		FinishedNS: fixture.nextMonotonicNS(),
	}
	decision.TerminalID = activationTestSettlementTerminalID(t, decision)
	if err := policyelements.VerifyIntentSettlementDecision(
		fixture.store.Snapshot(), decision, contract,
	); err != nil {
		t.Fatalf("prepare settlement decision: %v", err)
	}
	terminal := element.Envelope{
		Type: policyelements.IntentSettlementDecisionType(), ItemID: decision.TerminalID,
		SessionID: activationTestSession, RunID: runID, CancellationScope: runID,
		CausalParents: []string{"settlement-disposition", probe.ProbeID}, Payload: decision,
	}
	return activationSettlementScenario{
		decision: decision, terminal: terminal, evidence: consequenceEnvelope,
	}
}

func (fixture *activationTestFixture) cancelGeneration(
	t *testing.T, generationID string, sequence uint64,
) {
	t.Helper()
	envelope := element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: "cancel-" + generationID,
		SessionID: activationTestSession, Sequence: sequence,
		Payload: policyelements.GenerationCancel{
			GenerationID: generationID, Reason: "participant canceled",
		},
	}
	if fixture.runner.config.ExpectedSettlement != nil {
		var intent policyelements.TemporalEvidenceItemIdentity
		switch {
		case fixture.runner.active != nil && fixture.runner.active.id == generationID:
			intent = fixture.runner.active.intent
		case fixture.runner.canceledEffects[generationID] != nil:
			intent = fixture.runner.canceledEffects[generationID].generation.intent
		default:
			t.Fatal("settlement-aware cancellation has no exact generation intent")
		}
		envelope.CancellationScope = intent.TrajectoryItemID
		envelope.Payload = policyelements.GenerationCancel{
			GenerationID: generationID, StreamID: activationTestSession,
			Reason: "participant canceled", DurableIntent: &intent,
		}
	}
	if err := fixture.runner.acceptCancel(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
}

func activationTestSettlementProbeID(
	t *testing.T, probe policyelements.IntentSettlementProbe,
) string {
	t.Helper()
	probe.ProbeID = ""
	payload, err := json.Marshal(probe)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return "intent-settlement-probe:sha256:" + hex.EncodeToString(digest[:])
}

func activationTestSettlementTerminalID(
	t *testing.T, decision policyelements.IntentSettlementDecision,
) string {
	t.Helper()
	decision.TerminalID = ""
	payload, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	return "intent-settlement-terminal:sha256:" + hex.EncodeToString(digest[:])
}

func readdressActivationSettlementScenario(
	t *testing.T, scenario *activationSettlementScenario, sessionID string,
) {
	t.Helper()
	probe := cloneActivationSettlementProbe(scenario.decision.Probe)
	probe.SessionID = sessionID
	probe.ProbeID = activationTestSettlementProbeID(t, probe)
	scenario.decision.SessionID = sessionID
	scenario.decision.Probe = probe
	if scenario.decision.Disposition != nil {
		disposition := *scenario.decision.Disposition
		disposition.Probe = probe
		scenario.decision.Disposition = &disposition
	}
	scenario.decision.TerminalID = activationTestSettlementTerminalID(t, scenario.decision)
	scenario.terminal.ItemID = scenario.decision.TerminalID
	scenario.terminal.SessionID = sessionID
	scenario.terminal.CausalParents = []string{"settlement-disposition", probe.ProbeID}
	scenario.terminal.Payload = scenario.decision
}

func cloneActiveGeneration(source *activeGeneration) *activeGeneration {
	if source == nil {
		return nil
	}
	copy := *source
	return &copy
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

func afterIntentAdmissionEnvelope(
	envelope element.Envelope, intent policyelements.TemporalEvidenceItemIdentity,
) element.Envelope {
	admission := envelope.Payload.(policyelements.AdmittedTemporalEvidence)
	admission.Mode = policyelements.TemporalEvidenceAdmissionAfterIntent
	admission.SourceSet = policyelements.TemporalEvidenceSourceSetObservedBeforeIntent
	admission.DurableIntent = &intent
	admission.QualifyingObservations = []policyelements.TemporalEvidenceItemIdentity{
		admission.TriggerObservation,
	}
	envelope.Payload = admission
	return envelope
}

func (fixture *activationTestFixture) startGeneration(
	t *testing.T, itemID, content string,
) (string, uint64) {
	t.Helper()
	envelope, commit := fixture.appendUser(t, itemID, content)
	if err := fixture.runner.acceptAdmission(context.Background(), envelope); err != nil {
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

func TestActivationSettlementTerminalClearsExactEffectAndAcknowledgesWithoutReactivation(t *testing.T) {
	fixture := newActivationTestFixture(t)
	scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionSucceeded)
	oldActive := *fixture.runner.active
	fixture.runner.revokedSequence = 17
	fixture.runner.revokedStoreVersion = 3
	triggerCount := len(fixture.trigger.snapshot())

	decision := cloneActivationSettlementDecision(scenario.decision)
	envelope := scenario.terminal.Clone()
	envelope.Payload = &decision
	if err := fixture.runner.acceptSettlement(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	// Mutation after receipt must not change the echoed acknowledgement.
	decision.TerminalID = "mutated-by-caller"
	decision.Evidence.QualifyingObservations[0].TrajectoryItemID = "mutated-by-caller"

	if fixture.runner.active != nil || fixture.runner.intent != nil || fixture.runner.deferred != nil ||
		fixture.runner.pendingTerminal != nil || fixture.runner.pendingDisposition != nil {
		t.Fatalf("terminal settlement retained work: active=%+v intent=%+v deferred=%+v pending=%+v disposition=%+v",
			fixture.runner.active, fixture.runner.intent, fixture.runner.deferred,
			fixture.runner.pendingTerminal, fixture.runner.pendingDisposition)
	}
	if got := len(fixture.trigger.snapshot()); got != triggerCount {
		t.Fatalf("terminal consequence created a cognition turn: triggers=%d, want %d", got, triggerCount)
	}
	if fixture.runner.revokedSequence != 17 || fixture.runner.revokedStoreVersion != 3 {
		t.Fatalf("terminal settlement changed cancellation floors: sequence=%d store=%d",
			fixture.runner.revokedSequence, fixture.runner.revokedStoreVersion)
	}

	acks := fixture.settlementAck.snapshot()
	if len(acks) != 1 {
		t.Fatalf("settlement acknowledgements = %d, want 1", len(acks))
	}
	ackEnvelope := acks[0]
	ack, ok := ackEnvelope.Payload.(policyelements.IntentSettlementAcknowledgement)
	if !ok {
		t.Fatalf("settlement acknowledgement payload = %T", ackEnvelope.Payload)
	}
	if !reflect.DeepEqual(ack.Decision, scenario.decision) ||
		ack.GenerationID != scenario.decision.InvocationID ||
		ack.AcknowledgedNS < scenario.decision.FinishedNS || ack.AcknowledgedNS == 0 {
		t.Fatalf("settlement acknowledgement = %+v", ack)
	}
	if ackEnvelope.SessionID != activationTestSession || ackEnvelope.RunID != scenario.decision.InvocationID ||
		ackEnvelope.CancellationScope != scenario.decision.InvocationID || ackEnvelope.Sequence == 0 ||
		!slices.Equal(ackEnvelope.CausalParents, []string{
			scenario.decision.TerminalID, scenario.decision.Probe.ProbeID,
			scenario.decision.Probe.Result.TrajectoryItemID,
		}) {
		t.Fatalf("settlement acknowledgement envelope = %+v", ackEnvelope)
	}
	wantID, err := activationSettlementAcknowledgementID(
		fixture.runner.instance, ackEnvelope.Sequence, ack,
	)
	if err != nil {
		t.Fatal(err)
	}
	if ackEnvelope.ItemID != wantID {
		t.Fatalf("settlement acknowledgement ID = %q, want %q", ackEnvelope.ItemID, wantID)
	}
	if outcome := fixture.lastOutcome(t); outcome.Kind != policyelements.GenerationIgnored ||
		outcome.GenerationID != oldActive.id || outcome.Code != "intent_succeeded" ||
		outcome.ContextVersion != oldActive.contextVersion {
		t.Fatalf("terminal settlement outcome = %+v", outcome)
	}
}

func TestActivationSettlementWaitsForIndependentModelResultCopy(t *testing.T) {
	fixture := newActivationTestFixture(t)
	scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionSucceeded)
	contextVersion := fixture.runner.active.contextVersion
	fixture.runner.active.callID = ""
	fixture.runner.active.tool = ""

	if err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.pendingSettlement == nil || fixture.runner.active == nil ||
		len(fixture.settlementAck.snapshot()) != 0 {
		t.Fatalf("early settlement state: pending=%+v active=%+v acknowledgements=%+v",
			fixture.runner.pendingSettlement, fixture.runner.active, fixture.settlementAck.snapshot())
	}
	if outcome := fixture.lastOutcome(t); outcome.Code != "settlement_waiting_for_model_result" {
		t.Fatalf("early settlement outcome = %+v", outcome)
	}

	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		scenario.decision.InvocationID, contextVersion,
		[]cognitionelements.ToolProposal{activationTestProposal("settlement-call")},
	)); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.pendingSettlement != nil || fixture.runner.active != nil ||
		len(fixture.settlementAck.snapshot()) != 1 {
		t.Fatalf("settlement after result copy: pending=%+v active=%+v acknowledgements=%+v",
			fixture.runner.pendingSettlement, fixture.runner.active, fixture.settlementAck.snapshot())
	}
}

func TestActivationSettlementAcknowledgesCanceledExactEffectAcrossOrdering(t *testing.T) {
	t.Run("cancel after model result before terminal", func(t *testing.T) {
		fixture := newActivationTestFixture(t)
		scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionSucceeded)
		fixture.cancelGeneration(t, scenario.decision.InvocationID, fixture.store.Snapshot().Version)
		if fixture.runner.active != nil || fixture.runner.canceledEffects[scenario.decision.InvocationID] == nil {
			t.Fatalf("cancellation did not retain exact effect: active=%+v canceled=%+v",
				fixture.runner.active, fixture.runner.canceledEffects)
		}
		newIntentEnvelope, newIntentCommit := fixture.appendUser(
			t, "replacement-intent", "click the replacement warning",
		)
		newIntent := newIntentEnvelope.Payload.(policyelements.AdmittedTemporalEvidence).TriggerObservation
		newVisual, _ := fixture.appendVisual(
			t, "replacement-screen", "replacement warning visible", newIntentCommit.TrajectoryItemID,
		)
		newVisual = afterIntentAdmissionEnvelope(newVisual, newIntent)
		if err := fixture.runner.acceptAdmission(context.Background(), newVisual); err != nil {
			t.Fatal(err)
		}
		newRun := fixture.runner.active.id
		if err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal); err != nil {
			t.Fatal(err)
		}
		if fixture.runner.canceledEffects[scenario.decision.InvocationID] != nil ||
			len(fixture.settlementAck.snapshot()) != 1 || len(fixture.trigger.snapshot()) != 2 ||
			fixture.runner.active == nil || fixture.runner.active.id != newRun {
			t.Fatalf("late terminal changed replacement work: canceled=%+v ack=%+v triggers=%+v active=%+v",
				fixture.runner.canceledEffects, fixture.settlementAck.snapshot(),
				fixture.trigger.snapshot(), fixture.runner.active)
		}
	})

	t.Run("cancel and terminal before model result", func(t *testing.T) {
		fixture := newActivationTestFixture(t)
		scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionFailed)
		contextVersion := fixture.runner.active.contextVersion
		fixture.runner.active.callID = ""
		fixture.runner.active.tool = ""
		fixture.cancelGeneration(t, scenario.decision.InvocationID, 51)
		if err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal); err != nil {
			t.Fatal(err)
		}
		retained := fixture.runner.canceledEffects[scenario.decision.InvocationID]
		if retained == nil || retained.settlement == nil || len(fixture.settlementAck.snapshot()) != 0 {
			t.Fatalf("canceled early terminal was not retained: %+v", retained)
		}
		if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
			scenario.decision.InvocationID, contextVersion,
			[]cognitionelements.ToolProposal{activationTestProposal("settlement-call")},
		)); err != nil {
			t.Fatal(err)
		}
		if fixture.runner.canceledEffects[scenario.decision.InvocationID] != nil ||
			len(fixture.settlementAck.snapshot()) != 1 || len(fixture.trigger.snapshot()) != 1 {
			t.Fatalf("result copy did not settle canceled terminal: canceled=%+v ack=%+v triggers=%+v",
				fixture.runner.canceledEffects, fixture.settlementAck.snapshot(), fixture.trigger.snapshot())
		}
	})
}

func TestActivationCanceledProposalWithoutSettlementReleasesEffectCapacity(t *testing.T) {
	fixture := newActivationTestFixture(t)
	scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionSucceeded)
	contextVersion := fixture.runner.active.contextVersion
	fixture.runner.active.callID = ""
	fixture.runner.active.tool = ""
	triggerCount := len(fixture.trigger.snapshot())

	fixture.cancelGeneration(t, scenario.decision.InvocationID, fixture.store.Snapshot().Version)
	if fixture.runner.canceledEffects[scenario.decision.InvocationID] == nil {
		t.Fatal("exact cancellation did not retain the model-result race window")
	}
	if err := fixture.runner.acceptResult(context.Background(), activationResultEnvelope(
		scenario.decision.InvocationID, contextVersion,
		[]cognitionelements.ToolProposal{activationTestProposal("late-canceled-proposal")},
	)); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.canceledEffects[scenario.decision.InvocationID] != nil ||
		len(fixture.runner.canceledEffectOrder) != 0 || fixture.runner.active != nil ||
		len(fixture.settlementAck.snapshot()) != 0 || len(fixture.trigger.snapshot()) != triggerCount {
		t.Fatalf("late canceled proposal retained capacity or revived work: effects=%+v order=%+v active=%+v ack=%+v triggers=%+v",
			fixture.runner.canceledEffects, fixture.runner.canceledEffectOrder,
			fixture.runner.active, fixture.settlementAck.snapshot(), fixture.trigger.snapshot())
	}
	if !fixture.runner.intentCanceled(scenario.decision.Probe.DurableIntent) {
		t.Fatal("releasing the heavy canceled effect also lost the durable-intent tombstone")
	}
}

func TestActivationCancellationCapacityCannotEvictUnacknowledgedEffect(t *testing.T) {
	fixture := newActivationTestFixture(t)
	fixture.runner.config.CancelMemory = 1
	scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionSucceeded)
	oldRun := scenario.decision.InvocationID
	fixture.cancelGeneration(t, oldRun, fixture.store.Snapshot().Version)
	oldEffect := fixture.runner.canceledEffects[oldRun]
	if oldEffect == nil {
		t.Fatal("first cancellation did not retain its exact effect")
	}

	newIntentEnvelope, newIntentCommit := fixture.appendUser(
		t, "capacity-replacement-intent", "click the replacement warning",
	)
	newIntent := newIntentEnvelope.Payload.(policyelements.AdmittedTemporalEvidence).TriggerObservation
	newVisual, _ := fixture.appendVisual(
		t, "capacity-replacement-screen", "replacement warning visible", newIntentCommit.TrajectoryItemID,
	)
	newVisual = afterIntentAdmissionEnvelope(newVisual, newIntent)
	if err := fixture.runner.acceptAdmission(context.Background(), newVisual); err != nil {
		t.Fatal(err)
	}
	newActive := cloneActiveGeneration(fixture.runner.active)
	if newActive == nil {
		t.Fatal("replacement generation was not activated")
	}
	newIntentBasis := *fixture.runner.intent
	revokedSequence := fixture.runner.revokedSequence
	revokedStoreVersion := fixture.runner.revokedStoreVersion
	canceledCount := fixture.runner.state.Canceled
	secondCancel := element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: "cancel-" + newActive.id,
		SessionID: activationTestSession, Sequence: fixture.store.Snapshot().Version + 1,
		CancellationScope: newActive.intent.TrajectoryItemID,
		Payload: policyelements.GenerationCancel{
			GenerationID: newActive.id, StreamID: activationTestSession,
			Reason: "second cancellation", DurableIntent: &newActive.intent,
		},
	}
	if err := fixture.runner.acceptCancel(context.Background(), secondCancel); err != nil {
		t.Fatalf("second cancellation refusal publication: %v", err)
	}
	if outcome := fixture.lastOutcome(t); outcome.Kind != policyelements.GenerationRefused ||
		outcome.Code != "intent_cancel_capacity" ||
		!strings.Contains(outcome.Message, "full of unacknowledged effects") {
		t.Fatalf("second cancellation capacity outcome = %+v", outcome)
	}
	if len(fixture.runner.canceledEffects) != 1 || fixture.runner.canceledEffects[oldRun] != oldEffect ||
		!reflect.DeepEqual(fixture.runner.active, newActive) ||
		!reflect.DeepEqual(*fixture.runner.intent, newIntentBasis) ||
		fixture.runner.revokedSequence != revokedSequence ||
		fixture.runner.revokedStoreVersion != revokedStoreVersion ||
		fixture.runner.state.Canceled != canceledCount {
		t.Fatalf("capacity failure displaced live state: canceled=%+v active=%+v intent=%+v sequence=%d store=%d count=%d",
			fixture.runner.canceledEffects, fixture.runner.active, fixture.runner.intent,
			fixture.runner.revokedSequence, fixture.runner.revokedStoreVersion,
			fixture.runner.state.Canceled)
	}

	if err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.canceledEffects[oldRun] != nil ||
		!reflect.DeepEqual(fixture.runner.active, newActive) {
		t.Fatalf("old acknowledgement changed replacement generation: canceled=%+v active=%+v",
			fixture.runner.canceledEffects, fixture.runner.active)
	}
	if err := fixture.runner.acceptCancel(context.Background(), secondCancel); err != nil {
		t.Fatalf("second cancellation after exact acknowledgement: %v", err)
	}
	if fixture.runner.active != nil || fixture.runner.canceledEffects[newActive.id] == nil {
		t.Fatalf("reclaimed capacity did not retain the second effect: canceled=%+v active=%+v",
			fixture.runner.canceledEffects, fixture.runner.active)
	}
}

func TestActivationExactCancellationGenerationMismatchIsFailureAtomic(t *testing.T) {
	fixture := newActivationTestFixture(t)
	scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionSucceeded)
	activeBefore := cloneActiveGeneration(fixture.runner.active)
	intentBefore := *fixture.runner.intent
	deferredBefore := fixture.runner.deferred
	pendingTerminalBefore := fixture.runner.pendingTerminal
	pendingDispositionBefore := fixture.runner.pendingDisposition
	pendingSettlementBefore := clonePendingActivationSettlement(fixture.runner.pendingSettlement)
	canceledEffectsBefore := len(fixture.runner.canceledEffects)
	canceledEffectOrderBefore := slices.Clone(fixture.runner.canceledEffectOrder)
	canceledIntentsBefore := len(fixture.runner.canceledIntents)
	canceledIntentOrderBefore := slices.Clone(fixture.runner.canceledIntentOrder)
	revokedSequenceBefore := fixture.runner.revokedSequence
	revokedStoreVersionBefore := fixture.runner.revokedStoreVersion
	canceledCountBefore := fixture.runner.state.Canceled
	refusedBefore := fixture.runner.state.Refused
	intent := scenario.decision.Probe.DurableIntent

	if err := fixture.runner.acceptCancel(context.Background(), element.Envelope{
		Type: policyelements.GenerationCancelType(), ItemID: "forged-generation-cancel",
		SessionID: activationTestSession, Sequence: 71,
		CancellationScope: intent.TrajectoryItemID,
		Payload: policyelements.GenerationCancel{
			GenerationID: "forged-generation", StreamID: activationTestSession,
			Reason: "forged", DurableIntent: &intent,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if outcome := fixture.lastOutcome(t); outcome.Kind != policyelements.GenerationRefused ||
		outcome.Code != "generation_mismatch" {
		t.Fatalf("mismatched generation outcome = %+v", outcome)
	}
	if !reflect.DeepEqual(fixture.runner.active, activeBefore) ||
		fixture.runner.intent == nil || *fixture.runner.intent != intentBefore ||
		fixture.runner.deferred != deferredBefore ||
		fixture.runner.pendingTerminal != pendingTerminalBefore ||
		fixture.runner.pendingDisposition != pendingDispositionBefore ||
		!reflect.DeepEqual(fixture.runner.pendingSettlement, pendingSettlementBefore) ||
		len(fixture.runner.canceledEffects) != canceledEffectsBefore ||
		!reflect.DeepEqual(fixture.runner.canceledEffectOrder, canceledEffectOrderBefore) ||
		len(fixture.runner.canceledIntents) != canceledIntentsBefore ||
		!reflect.DeepEqual(fixture.runner.canceledIntentOrder, canceledIntentOrderBefore) ||
		fixture.runner.revokedSequence != revokedSequenceBefore ||
		fixture.runner.revokedStoreVersion != revokedStoreVersionBefore ||
		fixture.runner.state.Canceled != canceledCountBefore ||
		fixture.runner.state.Refused != refusedBefore+1 {
		t.Fatalf("mismatched generation mutated cancellation state: active=%+v intent=%+v effects=%+v intents=%+v sequence=%d store=%d state=%+v",
			fixture.runner.active, fixture.runner.intent, fixture.runner.canceledEffects,
			fixture.runner.canceledIntents, fixture.runner.revokedSequence,
			fixture.runner.revokedStoreVersion, fixture.runner.state)
	}
}

func TestActivationSettlementRetriesExactAcknowledgementAfterDeliveryFailure(t *testing.T) {
	fixture := newActivationTestFixture(t)
	scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionSucceeded)
	flaky := &failOnceActivationOutputPort{
		delegate: fixture.settlementAck,
	}
	fixture.runner.ports.settlementAcknowledgement = flaky
	if err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal); err != nil {
		t.Fatal(err)
	}
	delivered := fixture.settlementAck.snapshot()
	if len(fixture.runner.pendingSettlementAcks) != 0 || len(delivered) != 1 ||
		fixture.runner.active != nil || fixture.runner.intent != nil ||
		len(fixture.trigger.snapshot()) != 1 || flaky.attempts != 2 {
		t.Fatalf("acknowledgement retry changed lifecycle: pending=%+v ack=%+v active=%+v triggers=%+v",
			fixture.runner.pendingSettlementAcks, fixture.settlementAck.snapshot(),
			fixture.runner.active, fixture.trigger.snapshot())
	}
	if !reflect.DeepEqual(flaky.first, delivered[0]) {
		t.Fatalf("acknowledgement retry changed the exact envelope: first=%+v delivered=%+v",
			flaky.first, delivered[0])
	}
}

func TestActivationSettlementRetainsClearedEffectReceiptAfterBoundedAcknowledgementFailure(t *testing.T) {
	for _, testCase := range []struct {
		name string
		zero bool
	}{
		{name: "persistent error"},
		{name: "zero delivery", zero: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newActivationTestFixture(t)
			scenario := fixture.prepareSettlementScenario(
				t, policyelements.IntentSettlementDecisionSucceeded,
			)
			blocked := &undeliverableActivationOutputPort{
				delegate: fixture.settlementAck, zero: testCase.zero,
			}
			fixture.runner.ports.settlementAcknowledgement = blocked
			err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf(
				"failed after %d bounded attempts", maximumSettlementAckTries,
			)) {
				t.Fatalf("bounded acknowledgement error = %v", err)
			}
			pending, found := fixture.runner.pendingSettlementAcks[scenario.decision.TerminalID]
			if !found || blocked.attempts != maximumSettlementAckTries ||
				fixture.runner.active != nil || fixture.runner.intent != nil ||
				len(fixture.settlementAck.snapshot()) != 0 || len(fixture.trigger.snapshot()) != 1 {
				t.Fatalf("bounded failure lost receipt or revived work: pending=%+v attempts=%d active=%+v intent=%+v ack=%+v triggers=%+v",
					fixture.runner.pendingSettlementAcks, blocked.attempts, fixture.runner.active,
					fixture.runner.intent, fixture.settlementAck.snapshot(), fixture.trigger.snapshot())
			}
			for index, attempted := range blocked.envelopes {
				if !reflect.DeepEqual(attempted, pending.envelope) {
					t.Fatalf("attempt %d changed retained acknowledgement: attempted=%+v retained=%+v",
						index, attempted, pending.envelope)
				}
			}

			fixture.runner.ports.settlementAcknowledgement = fixture.settlementAck
			if err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal); err != nil {
				t.Fatal(err)
			}
			delivered := fixture.settlementAck.snapshot()
			if len(delivered) != 1 || !reflect.DeepEqual(delivered[0], pending.envelope) ||
				len(fixture.runner.pendingSettlementAcks) != 0 || fixture.runner.active != nil ||
				len(fixture.trigger.snapshot()) != 1 {
				t.Fatalf("retained acknowledgement recovery = ack=%+v pending=%+v active=%+v triggers=%+v",
					delivered, fixture.runner.pendingSettlementAcks,
					fixture.runner.active, fixture.trigger.snapshot())
			}
		})
	}
}

func TestActivationSettlementContinueUsesAdmissionPathAndDoesNotAcknowledge(t *testing.T) {
	fixture := newActivationTestFixture(t)
	scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionSucceeded)
	oldRun := fixture.runner.active.id
	released := scenario.evidence.Clone()
	released.ItemID = "intent-settlement-admitted:sha256:continue-test"
	released.CausalParents = []string{"continued-disposition", scenario.decision.Probe.ProbeID}
	if err := fixture.runner.acceptAdmission(context.Background(), released); err != nil {
		t.Fatal(err)
	}
	if fixture.runner.active == nil || fixture.runner.active.id == oldRun ||
		fixture.runner.active.contextVersion != scenario.decision.Evidence.TriggerCommit.StoreVersion ||
		fixture.runner.intent == nil ||
		fixture.runner.intent.identity != scenario.decision.Probe.DurableIntent {
		t.Fatalf("continued settlement did not activate the next exact turn: active=%+v intent=%+v",
			fixture.runner.active, fixture.runner.intent)
	}
	if triggers := fixture.trigger.snapshot(); len(triggers) != 2 || triggers[1].RunID != fixture.runner.active.id {
		t.Fatalf("continued settlement triggers = %+v", triggers)
	}
	if acknowledgements := fixture.settlementAck.snapshot(); len(acknowledgements) != 0 {
		t.Fatalf("continuation emitted terminal acknowledgement: %+v", acknowledgements)
	}
}

func TestActivationSettlementFailsClosedOnUntrustedOrMisorderedControl(t *testing.T) {
	testCases := []struct {
		name     string
		mutate   func(*activationTestFixture, *activationSettlementScenario)
		wantCode string
	}{
		{
			name: "unconfigured settlement contract",
			mutate: func(fixture *activationTestFixture, _ *activationSettlementScenario) {
				fixture.runner.config.ExpectedSettlement = nil
			},
			wantCode: "settlement_not_configured",
		},
		{
			name: "forged terminal identity",
			mutate: func(_ *activationTestFixture, scenario *activationSettlementScenario) {
				scenario.decision.TerminalID = "intent-settlement-terminal:sha256:" + strings.Repeat("0", 64)
				scenario.terminal.ItemID = scenario.decision.TerminalID
				scenario.terminal.Payload = scenario.decision
			},
			wantCode: "invalid_settlement_decision",
		},
		{
			name: "stale generation",
			mutate: func(fixture *activationTestFixture, _ *activationSettlementScenario) {
				fixture.runner.active = nil
			},
			wantCode: "stale_settlement_decision",
		},
		{
			name: "wrong active effect",
			mutate: func(fixture *activationTestFixture, _ *activationSettlementScenario) {
				fixture.runner.active.callID = "another-call"
			},
			wantCode: "settlement_effect_mismatch",
		},
		{
			name: "cross session",
			mutate: func(_ *activationTestFixture, scenario *activationSettlementScenario) {
				readdressActivationSettlementScenario(t, scenario, "foreign-session")
			},
			wantCode: "settlement_session_mismatch",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			fixture := newActivationTestFixture(t)
			scenario := fixture.prepareSettlementScenario(
				t, policyelements.IntentSettlementDecisionSucceeded,
			)
			originalActive := cloneActiveGeneration(fixture.runner.active)
			originalIntent := *fixture.runner.intent
			testCase.mutate(fixture, &scenario)
			expectedActive := cloneActiveGeneration(fixture.runner.active)
			if err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fixture.runner.active, expectedActive) ||
				!reflect.DeepEqual(*fixture.runner.intent, originalIntent) {
				t.Fatalf("refused settlement mutated work: before=%+v original=%+v after=%+v intent=%+v",
					expectedActive, originalActive, fixture.runner.active, fixture.runner.intent)
			}
			if len(fixture.settlementAck.snapshot()) != 0 || len(fixture.trigger.snapshot()) != 1 {
				t.Fatalf("refused settlement emitted control: ack=%+v triggers=%+v",
					fixture.settlementAck.snapshot(), fixture.trigger.snapshot())
			}
			if outcome := fixture.lastOutcome(t); outcome.Kind != policyelements.GenerationRefused ||
				outcome.Code != testCase.wantCode {
				t.Fatalf("refused settlement outcome = %+v, want %q", outcome, testCase.wantCode)
			}
		})
	}
}

func TestActivationSettlementDuplicateDoesNotReacknowledgeOrReactivate(t *testing.T) {
	fixture := newActivationTestFixture(t)
	scenario := fixture.prepareSettlementScenario(t, policyelements.IntentSettlementDecisionFailed)
	if err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal); err != nil {
		t.Fatal(err)
	}
	firstAck := fixture.settlementAck.snapshot()
	if len(firstAck) != 1 {
		t.Fatalf("first acknowledgement = %+v", firstAck)
	}
	if err := fixture.runner.acceptSettlement(context.Background(), scenario.terminal); err != nil {
		t.Fatal(err)
	}
	if acknowledgements := fixture.settlementAck.snapshot(); len(acknowledgements) != 1 ||
		!reflect.DeepEqual(acknowledgements[0], firstAck[0]) {
		t.Fatalf("duplicate terminal was reacknowledged: %+v", acknowledgements)
	}
	if len(fixture.trigger.snapshot()) != 1 || fixture.runner.active != nil || fixture.runner.intent != nil {
		t.Fatalf("duplicate terminal changed lifecycle: triggers=%+v active=%+v intent=%+v",
			fixture.trigger.snapshot(), fixture.runner.active, fixture.runner.intent)
	}
	if outcome := fixture.lastOutcome(t); outcome.Kind != policyelements.GenerationIgnored ||
		outcome.Code != "duplicate_settlement_decision" {
		t.Fatalf("duplicate settlement outcome = %+v", outcome)
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
	observer, source := item.Event.Source, item.Event.Channel
	if item.Observation != nil {
		observer, source = item.Observation.Observer, item.Observation.Source
	}
	identity := policyelements.TemporalEvidenceItemIdentity{
		TrajectoryItemID: item.ID, TriggerItemID: item.Event.EventID,
		StoreVersion: snapshot.Version, SourceRevision: item.SourceRevision,
		OccurredNS: item.Event.OccurredNS, Authority: trajectory.AuthorityOf(item),
		Observer: observer, Source: source,
	}
	admission := policyelements.AdmittedTemporalEvidence{
		Mode:          policyelements.TemporalEvidenceAdmissionImmediate,
		TriggerCommit: commit, TriggerObservation: identity, Prefix: prefix,
	}
	return element.Envelope{
		Type: policyelements.AdmittedTemporalEvidenceType(), ItemID: item.ID + "-admitted",
		SessionID: activationTestSession, Sequence: snapshot.Version, Payload: admission,
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
