package scenarioconversation

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestDelayedActivityAcceptsOnlyExactlyClosedAudioStream(t *testing.T) {
	session := &session{
		sessionID: "session-a", sink: &playbackClientSink{}, audioStream: 2,
		closedAudio: make(map[string]struct{}), utterances: make(map[string]struct{}),
	}
	closed := session.audioStreamID(1)
	session.closedAudio[closed] = struct{}{}
	activity := acousticelements.SpeechActivity{
		Kind: acousticelements.SpeechStarted, StreamID: closed,
		Source: SourceMicrophone, SampleRateHz: 24_000,
	}
	envelope := element.Envelope{
		ItemID: "activity-start", SessionID: session.sessionID, Payload: activity,
	}
	if err := session.publishActivity(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	activity.Kind = acousticelements.SpeechStopped
	envelope.ItemID, envelope.Payload = "activity-stop", activity
	if err := session.publishActivity(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if _, found := session.closedAudio[closed]; found {
		t.Fatal("stopped activity did not retire its exact closed-stream evidence")
	}
	activity.Kind, activity.StreamID = acousticelements.SpeechStarted, session.audioStreamID(1)
	envelope.ItemID, envelope.Payload = "activity-replay", activity
	if err := session.publishActivity(context.Background(), envelope); err == nil {
		t.Fatal("retired closed-stream activity replay was accepted")
	}
}

func TestResponseCreateContextWaitsForExactPublishedSnapshot(t *testing.T) {
	session := &session{
		sessionID: "session-a", bundle: &sessionBundle{store: trajectory.NewStore()},
		contentAcks: make(map[string]*pendingContent), snapshotChanged: make(chan struct{}),
	}
	type binding struct {
		context stateelements.CommittedContext
		err     error
	}
	result := make(chan binding, 1)
	go func() {
		committed, err := session.responseCreateContext(context.Background())
		result <- binding{context: committed, err: err}
	}()
	select {
	case got := <-result:
		t.Fatalf("response context crossed unpublished snapshot: %+v", got)
	case <-time.After(25 * time.Millisecond):
	}
	if err := session.acceptTrajectorySnapshot(element.Envelope{
		ItemID: "trajectory-snapshot-0", SessionID: session.sessionID,
		Payload: trajectory.Snapshot{},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if got.err != nil || got.context.Prefix.Version != 0 ||
			got.context.StateItemID != "trajectory-snapshot-0" || got.context.Prefix.Digest == "" {
			t.Fatalf("response context binding = %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("response context did not observe exact published snapshot")
	}
	if err := session.acceptTrajectorySnapshot(element.Envelope{
		ItemID: "forged-snapshot-0", SessionID: session.sessionID,
		Payload: trajectory.Snapshot{},
	}); err == nil {
		t.Fatal("same-version snapshot identity drift was accepted")
	}
	item := trajectory.Item{
		ID: "durable-user-1", Kind: trajectory.KindObservation, MonotonicNS: 1,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "durable",
	}
	if err := session.bundle.store.Append(item); err != nil {
		t.Fatal(err)
	}
	forged := item
	forged.Content = "forged"
	if err := session.acceptTrajectorySnapshot(element.Envelope{
		ItemID: "trajectory-snapshot-1", SessionID: session.sessionID,
		Payload: trajectory.Snapshot{Version: 1, Items: []trajectory.Item{forged}},
	}); err == nil {
		t.Fatal("snapshot bytes outside the durable prefix were accepted")
	}
	if err := session.acceptTrajectorySnapshot(element.Envelope{
		ItemID: "trajectory-snapshot-1", SessionID: session.sessionID,
		Payload: session.bundle.store.Snapshot(),
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInvocationOutcomeAcceptsOnlyTheExactGraphInternalSilenceCreate(t *testing.T) {
	const (
		sessionID    = "session-a"
		generationID = "generation-a"
		causeID      = "post_commit_silence:post_commit_silence:7"
	)
	outcome := policyelements.SessionInvocationOutcome{
		Kind: policyelements.SessionInvocationEmitted, Operation: "create",
		Role: "foreground", GenerationID: generationID,
	}
	valid := element.Envelope{
		ItemID: causeID + ":session_invocation_outcome:9", SessionID: sessionID,
		CausalParents: []string{
			"durable-commit", "trajectory-state", causeID, "semantic_admission:decision:8",
		}, Payload: outcome,
	}
	newSession := func() *session {
		return &session{
			sessionID: sessionID, pendingOps: make(map[string]*pendingOperation),
			active: make(map[string]struct{}), terminalRuns: make(map[string]struct{}),
		}
	}
	accepted := newSession()
	if err := accepted.acceptInvocationOutcome(valid); err != nil {
		t.Fatal(err)
	}
	if _, found := accepted.active[generationID]; !found {
		t.Fatal("exact post-commit silence create did not acquire its generation")
	}

	tests := []struct {
		name string
		edit func(*element.Envelope, *policyelements.SessionInvocationOutcome)
	}{
		{name: "missing direct cause", edit: func(envelope *element.Envelope, _ *policyelements.SessionInvocationOutcome) {
			envelope.CausalParents = []string{"durable-commit", "trajectory-state"}
		}},
		{name: "wrong node", edit: func(envelope *element.Envelope, _ *policyelements.SessionInvocationOutcome) {
			envelope.ItemID = "other:post_commit_silence:7:session_invocation_outcome:9"
			envelope.CausalParents[2] = "other:post_commit_silence:7"
		}},
		{name: "invalid cause sequence", edit: func(envelope *element.Envelope, _ *policyelements.SessionInvocationOutcome) {
			envelope.ItemID = "post_commit_silence:post_commit_silence:latest:session_invocation_outcome:9"
			envelope.CausalParents[2] = "post_commit_silence:post_commit_silence:latest"
		}},
		{name: "refused create", edit: func(_ *element.Envelope, outcome *policyelements.SessionInvocationOutcome) {
			outcome.Kind = policyelements.SessionInvocationRefused
		}},
		{name: "missing semantic decision", edit: func(envelope *element.Envelope, _ *policyelements.SessionInvocationOutcome) {
			envelope.CausalParents = envelope.CausalParents[:3]
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := newSession()
			envelope := valid.Clone()
			candidateOutcome := outcome
			test.edit(&envelope, &candidateOutcome)
			envelope.Payload = candidateOutcome
			if err := candidate.acceptInvocationOutcome(envelope); err == nil {
				t.Fatal("drifted internal create was accepted")
			}
			if len(candidate.active) != 0 {
				t.Fatal("invalid internal create mutated active generations")
			}
		})
	}
}

func TestInvocationOutcomeAcceptsOnlyExactGatewaySemanticCreateChain(t *testing.T) {
	const (
		sessionID    = "session-a"
		requestID    = "sc_create_7"
		generationID = "generation-a"
	)
	outcome := policyelements.SessionInvocationOutcome{
		Kind: policyelements.SessionInvocationEmitted, Operation: "create",
		Role: "foreground", GenerationID: generationID,
	}
	valid := element.Envelope{
		ItemID: requestID + ":session_invocation_outcome:9", SessionID: sessionID,
		CausalParents: []string{"semantic_admission:decision:8", requestID}, Payload: outcome,
	}
	newSession := func() (*session, *pendingOperation) {
		pending := &pendingOperation{
			requestID: requestID, operation: "create", result: make(chan operationAck, 1),
		}
		return &session{
			sessionID: sessionID, pendingOps: map[string]*pendingOperation{requestID: pending},
			active: make(map[string]struct{}), terminalRuns: make(map[string]struct{}),
		}, pending
	}
	accepted, pending := newSession()
	if err := accepted.acceptInvocationOutcome(valid); err != nil {
		t.Fatal(err)
	}
	ack := <-pending.result
	if ack.err != nil || ack.generationID != generationID || !pending.completed {
		t.Fatalf("semantic create acknowledgement = %+v pending=%+v", ack, pending)
	}
	if _, found := accepted.active[generationID]; !found || len(accepted.pendingOps) != 0 {
		t.Fatalf("semantic create active=%v pending=%v", accepted.active, accepted.pendingOps)
	}

	tests := []struct {
		name string
		edit func(*element.Envelope)
	}{
		{name: "missing semantic decision", edit: func(envelope *element.Envelope) {
			envelope.CausalParents = []string{requestID}
		}},
		{name: "different policy node", edit: func(envelope *element.Envelope) {
			envelope.CausalParents[0] = "other_admission:decision:8"
		}},
		{name: "zero decision sequence", edit: func(envelope *element.Envelope) {
			envelope.CausalParents[0] = "semantic_admission:decision:0"
		}},
		{name: "arbitrary extra parent", edit: func(envelope *element.Envelope) {
			envelope.CausalParents = append(envelope.CausalParents, "forged-parent")
		}},
		{name: "outcome derived from another cause", edit: func(envelope *element.Envelope) {
			envelope.ItemID = "sc_create_8:session_invocation_outcome:9"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, pending := newSession()
			envelope := valid.Clone()
			test.edit(&envelope)
			if err := candidate.acceptInvocationOutcome(envelope); err == nil {
				t.Fatal("drifted semantic create acknowledgement was accepted")
			}
			if len(candidate.active) != 0 || pending.completed ||
				candidate.pendingOps[requestID] != pending {
				t.Fatalf("invalid acknowledgement mutated session active=%v pending=%+v",
					candidate.active, pending)
			}
		})
	}
	unknown, _ := newSession()
	unknown.pendingOps = make(map[string]*pendingOperation)
	if err := unknown.acceptInvocationOutcome(valid); err == nil {
		t.Fatal("semantic create for an unknown gateway request was accepted")
	}
	if len(unknown.active) != 0 {
		t.Fatalf("unknown semantic create acquired active generation: %v", unknown.active)
	}
}

func TestSemanticAdmissionOutcomeTerminatesSuppressedGatewayCreate(t *testing.T) {
	const (
		sessionID = "session-a"
		requestID = "sc_create_7"
	)
	newSession := func() (*session, *pendingOperation) {
		pending := &pendingOperation{
			requestID: requestID, operation: "create", result: make(chan operationAck, 1),
		}
		return &session{
			sessionID: sessionID, pendingOps: map[string]*pendingOperation{requestID: pending},
		}, pending
	}
	base := element.Envelope{
		ItemID: "semantic_admission:outcome:9", SessionID: sessionID,
		CausalParents: []string{requestID},
	}

	admitted, pending := newSession()
	envelope := base.Clone()
	envelope.Payload = policyelements.SemanticAdmissionOutcome{
		Kind: policyelements.SemanticAdmissionAdmitted, Operation: "create",
		Act: coreinteraction.ActAnswer, DecisionItemID: "semantic_admission:decision:8",
	}
	if err := admitted.acceptSemanticAdmissionOutcome(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	if pending.completed || admitted.pendingOps[requestID] != pending {
		t.Fatal("admitted semantic outcome completed before its invocation acknowledgement")
	}

	suppressed, pending := newSession()
	envelope = base.Clone()
	envelope.Payload = policyelements.SemanticAdmissionOutcome{
		Kind: policyelements.SemanticAdmissionSuppressed, Operation: "create",
		Act: coreinteraction.ActStaySilent, DecisionItemID: "semantic_admission:decision:8",
		Code: "listen", Message: "policy selected no generation",
	}
	if err := suppressed.acceptSemanticAdmissionOutcome(context.Background(), envelope); err != nil {
		t.Fatal(err)
	}
	ack := <-pending.result
	if ack.err == nil || !strings.Contains(ack.err.Error(), "suppressed/listen") ||
		!pending.completed || len(suppressed.pendingOps) != 0 {
		t.Fatalf("suppressed semantic acknowledgement=%+v pending=%+v map=%v",
			ack, pending, suppressed.pendingOps)
	}

	invalid, pending := newSession()
	envelope = base.Clone()
	envelope.Payload = policyelements.SemanticAdmissionOutcome{
		Kind: policyelements.SemanticAdmissionAdmitted, Operation: "create",
		Act: coreinteraction.ActAnswer, DecisionItemID: "other:decision:8",
	}
	if err := invalid.acceptSemanticAdmissionOutcome(context.Background(), envelope); err == nil {
		t.Fatal("admitted semantic outcome with forged decision identity was accepted")
	}
	if pending.completed || invalid.pendingOps[requestID] != pending {
		t.Fatal("invalid semantic outcome mutated the pending gateway operation")
	}
}

func TestClientToolResultOutcomeRequiresEveryCanonicalReceiptField(t *testing.T) {
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: []byte(`{"ok":true}`)}
	digest := actionelements.ClientToolResultDigest(result)
	baseOutcome := actionelements.Outcome{
		Kind: actionelements.OutcomeSucceeded, Stage: "client_tool_result", Operation: "result",
		CallID: result.CallID, ResultDigest: digest, IngressItemID: "api-result-a",
		AcceptedItemID: "accepted-a", CanonicalEnvelopeItemID: "canonical-envelope-a",
		CanonicalTrajectoryItemID: "canonical-trajectory-a", CanonicalStoreVersion: 7,
	}
	baseEnvelope := element.Envelope{
		Type: actionelements.OutcomeType(), ItemID: "outcome-a", SessionID: "session-a", RunID: "run-a",
		OpportunityID: result.CallID, CancellationScope: "run-a",
		CausalParents: []string{"api-result-a", "accepted-a", "canonical-envelope-a"},
	}

	tests := []struct {
		name string
		edit func(*element.Envelope, *actionelements.Outcome)
	}{
		{name: "digest", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.ResultDigest += "00" }},
		{name: "ingress", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.IngressItemID = "other-ingress" }},
		{name: "accepted", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.AcceptedItemID = "" }},
		{name: "canonical envelope", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.CanonicalEnvelopeItemID = "" }},
		{name: "canonical trajectory", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.CanonicalTrajectoryItemID = "" }},
		{name: "store version", edit: func(_ *element.Envelope, outcome *actionelements.Outcome) { outcome.CanonicalStoreVersion = 0 }},
		{name: "run", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) { envelope.RunID = "other-run" }},
		{name: "scope", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) { envelope.CancellationScope = "other-run" }},
		{name: "opportunity", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) { envelope.OpportunityID = "other-call" }},
		{name: "accepted parent", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) {
			envelope.CausalParents = slices.DeleteFunc(envelope.CausalParents, func(value string) bool { return value == "accepted-a" })
		}},
		{name: "canonical parent", edit: func(envelope *element.Envelope, _ *actionelements.Outcome) {
			envelope.CausalParents = slices.DeleteFunc(envelope.CausalParents, func(value string) bool { return value == "canonical-envelope-a" })
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, pending := toolResultOutcomeSession(digest, false)
			envelope := baseEnvelope.Clone()
			outcome := baseOutcome
			test.edit(&envelope, &outcome)
			envelope.Payload = outcome
			if err := session.acceptClientToolResultOutcome(envelope); err == nil {
				t.Fatal("drifted tool-result receipt was accepted")
			}
			if session.pendingOps[pending.requestID] != pending || pending.completed {
				t.Fatal("invalid receipt consumed the exact pending operation")
			}
		})
	}

	t.Run("exact", func(t *testing.T) {
		session, pending := toolResultOutcomeSession(digest, false)
		envelope := baseEnvelope.Clone()
		envelope.Payload = baseOutcome
		if err := session.acceptClientToolResultOutcome(envelope); err != nil {
			t.Fatal(err)
		}
		if session.pendingOps[pending.requestID] != nil || !pending.completed {
			t.Fatal("exact receipt did not retire its operation")
		}
		if ack := <-pending.result; ack.err != nil {
			t.Fatalf("exact receipt ACK = %v", ack.err)
		}
	})
}

func TestAbandonedToolResultOperationConsumesValidatedLateCanonicalACK(t *testing.T) {
	result := trajectory.ToolResult{CallID: "call-a", Name: "lookup", Output: []byte(`true`)}
	digest := actionelements.ClientToolResultDigest(result)
	session, pending := toolResultOutcomeSession(digest, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := session.awaitOperation(ctx, pending.requestID, pending); !errors.Is(err, context.Canceled) {
		t.Fatalf("await cancellation = %v", err)
	}
	if !pending.abandoned || session.pendingOps[pending.requestID] != pending {
		t.Fatal("caller timeout did not leave a bounded exact tombstone")
	}
	outcome := actionelements.Outcome{
		Kind: actionelements.OutcomeSucceeded, Stage: "client_tool_result", Operation: "result",
		CallID: "call-a", ResultDigest: digest, IngressItemID: pending.requestID,
		AcceptedItemID: "accepted-a", CanonicalEnvelopeItemID: "canonical-a",
		CanonicalTrajectoryItemID: "trajectory-a", CanonicalStoreVersion: 3,
	}
	envelope := element.Envelope{
		Type: actionelements.OutcomeType(), ItemID: "outcome-a", SessionID: "session-a", RunID: "run-a",
		OpportunityID: "call-a", CancellationScope: "run-a",
		CausalParents: []string{pending.requestID, "accepted-a", "canonical-a"}, Payload: outcome,
	}
	if err := session.acceptClientToolResultOutcome(envelope); err != nil {
		t.Fatal(err)
	}
	if session.pendingOps[pending.requestID] != nil || !pending.completed || len(pending.result) != 0 {
		t.Fatal("late canonical ACK was not consumed without reviving the timed-out caller")
	}
}

func TestClientToolResultCancelOutcomeUsesActiveOrExactTerminalRun(t *testing.T) {
	active := activeClientCall{runID: "run-a", call: trajectory.ToolCall{CallID: "call-a", Name: "lookup"}}
	session := &session{
		sessionID: "session-a", calls: map[string]activeClientCall{},
		terminalCalls: map[string]struct{}{
			clientCallScopeKey("session-a", "run-a", "call-a"): {},
		},
	}
	_ = active
	outcome := actionelements.Outcome{
		Kind: actionelements.OutcomeIgnored, Stage: "client_tool_result", Operation: "cancel",
		CallID: "call-a", Code: "already_terminal",
	}
	envelope := element.Envelope{
		Type: actionelements.OutcomeType(), ItemID: "cancel-outcome", SessionID: "session-a", RunID: "run-a",
		OpportunityID: "call-a", CancellationScope: "run-a", Payload: outcome,
	}
	if err := session.acceptClientToolResultOutcome(envelope); err != nil {
		t.Fatal(err)
	}
	envelope.RunID, envelope.CancellationScope = "run-b", "run-b"
	if err := session.acceptClientToolResultOutcome(envelope); err == nil {
		t.Fatal("cross-run cancellation outcome was accepted")
	}
}

func toolResultOutcomeSession(digest string, abandoned bool) (*session, *pendingOperation) {
	pending := &pendingOperation{
		requestID: "api-result-a", operation: "tool_result", generation: "run-a",
		callID: "call-a", name: "lookup", digest: digest, abandoned: abandoned,
		result: make(chan operationAck, 1),
	}
	return &session{
		sessionID: "session-a", pendingOps: map[string]*pendingOperation{pending.requestID: pending},
		calls: make(map[string]activeClientCall), terminalCalls: make(map[string]struct{}),
	}, pending
}
