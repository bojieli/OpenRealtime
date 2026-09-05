package cognition_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestTextModelReconstructsCommittedPrefixFromLaterStateAndRefusesReplay(t *testing.T) {
	provider := &scriptedProvider{
		descriptor: testDescriptor,
		seen:       make(chan continuation.Request, 4),
	}
	mounted, done, stop := mountCommittedTextModel(t, provider)
	defer stopTextModel(t, mounted, done, stop)

	snapshot := committedTestSnapshot()
	v1 := committedContext(t, snapshot, 1, "context-v1")
	v2 := committedContext(t, snapshot, 2, "context-v2")
	sendCommittedSnapshot(t, mounted, snapshot, "context-v2", "session-a")

	sendCommittedGenerate(t, mounted, "late-v1", "session-a", v1)
	requestV1 := receiveProviderRequest(t, provider.seen)
	wantV1, err := trajectory.Prefix(snapshot, v1.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(requestV1.Trajectory, wantV1) {
		t.Fatalf("late v1 provider trajectory = %+v, want %+v", requestV1.Trajectory, wantV1)
	}
	resultEnvelopeV1 := receive(t, mustEgress(t, mounted, "result"))
	resultV1 := resultEnvelopeV1.Payload.(cognitionelements.Result)
	outcomeV1 := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if resultV1.ContextVersion != 1 || resultV1.ContextTailID != "observation-v1" || resultV1.ContextPrefix != v1.Prefix ||
		outcomeV1.Kind != cognitionelements.OutcomeSucceeded {
		t.Fatalf("late v1 result/outcome = %+v / %+v", resultV1, outcomeV1)
	}
	if !contains(resultEnvelopeV1.CausalParents, "context-v1") ||
		contains(resultEnvelopeV1.CausalParents, "context-v2") {
		t.Fatalf("late v1 result causal parents = %v", resultEnvelopeV1.CausalParents)
	}

	sendCommittedGenerate(t, mounted, "current-v2", "session-a", v2)
	requestV2 := receiveProviderRequest(t, provider.seen)
	if !reflect.DeepEqual(requestV2.Trajectory, snapshot) {
		t.Fatalf("v2 provider trajectory = %+v, want %+v", requestV2.Trajectory, snapshot)
	}
	resultEnvelopeV2 := receive(t, mustEgress(t, mounted, "result"))
	resultV2 := resultEnvelopeV2.Payload.(cognitionelements.Result)
	outcomeV2 := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if resultV2.ContextVersion != 2 || resultV2.ContextTailID != "observation-v2" || resultV2.ContextPrefix != v2.Prefix ||
		outcomeV2.Kind != cognitionelements.OutcomeSucceeded ||
		!contains(resultEnvelopeV2.CausalParents, "context-v2") {
		t.Fatalf("v2 result/outcome/parents = %+v / %+v / %v",
			resultV2, outcomeV2, resultEnvelopeV2.CausalParents)
	}

	for _, replay := range []struct {
		run     string
		context stateelements.CommittedContext
	}{
		{run: "replay-v1", context: v1},
		{run: "replay-v2", context: v2},
	} {
		sendCommittedGenerate(t, mounted, replay.run, "session-a", replay.context)
		outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
		if outcome.Kind != cognitionelements.OutcomeRefused || outcome.Code != "committed_context_replay" {
			t.Fatalf("%s outcome = %+v", replay.run, outcome)
		}
	}
	assertNoProviderRequest(t, provider.seen)
	assertNoEnvelope(t, mustEgress(t, mounted, "result"))
}

func TestTextModelCommittedContextValidationFailsClosedWithoutPoisoningFloor(t *testing.T) {
	t.Run("wrong digest", func(t *testing.T) {
		provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 2)}
		mounted, done, stop := mountCommittedTextModel(t, provider)
		defer stopTextModel(t, mounted, done, stop)

		snapshot := committedTestSnapshot()
		valid := committedContext(t, snapshot, 1, "context-v1")
		forged := valid
		forged.Prefix.Digest = replaceDigestTail(forged.Prefix.Digest)
		sendCommittedSnapshot(t, mounted, snapshot, "context-v2", "session-a")
		sendCommittedGenerate(t, mounted, "wrong-digest", "session-a", forged)
		assertCommittedRefusal(t, mounted, "context_prefix_mismatch")
		assertNoProviderRequest(t, provider.seen)

		// A refused, unverified high or malformed identity must not advance the
		// scalar admission floor and lock out the authentic committed trigger.
		sendCommittedGenerate(t, mounted, "valid-after-digest", "session-a", valid)
		if request := receiveProviderRequest(t, provider.seen); request.Trajectory.Version != 1 {
			t.Fatalf("valid request after digest refusal = %+v", request)
		}
		_ = receive(t, mustEgress(t, mounted, "result"))
		if outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome); outcome.Kind != cognitionelements.OutcomeSucceeded {
			t.Fatalf("valid outcome after digest refusal = %+v", outcome)
		}
	})

	t.Run("wrong exact State item", func(t *testing.T) {
		provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 1)}
		mounted, done, stop := mountCommittedTextModel(t, provider)
		defer stopTextModel(t, mounted, done, stop)

		snapshot := committedTestSnapshot()
		prefix, err := trajectory.Prefix(snapshot, committedContext(t, snapshot, 1, "unused").Prefix)
		if err != nil {
			t.Fatal(err)
		}
		forged := committedContext(t, snapshot, 1, "context-forged")
		sendCommittedSnapshot(t, mounted, prefix, "context-v1", "session-a")
		sendCommittedGenerate(t, mounted, "wrong-state", "session-a", forged)
		assertCommittedRefusal(t, mounted, "context_identity_mismatch")
		assertNoProviderRequest(t, provider.seen)
	})

	t.Run("wrong State session", func(t *testing.T) {
		provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 1)}
		mounted, done, stop := mountCommittedTextModel(t, provider)
		defer stopTextModel(t, mounted, done, stop)

		snapshot := committedTestSnapshot()
		prefix, err := trajectory.Prefix(snapshot, committedContext(t, snapshot, 1, "unused").Prefix)
		if err != nil {
			t.Fatal(err)
		}
		binding := committedContext(t, snapshot, 1, "context-v1")
		sendCommittedSnapshot(t, mounted, prefix, "context-v1", "session-b")
		sendCommittedGenerate(t, mounted, "wrong-session", "session-a", binding)
		assertCommittedRefusal(t, mounted, "context_session_mismatch")
		assertNoProviderRequest(t, provider.seen)
	})

	t.Run("empty trigger session", func(t *testing.T) {
		provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 1)}
		mounted, done, stop := mountCommittedTextModel(t, provider)
		defer stopTextModel(t, mounted, done, stop)

		binding := committedContext(t, committedTestSnapshot(), 1, "context-v1")
		sendCommittedGenerate(t, mounted, "empty-session", "", binding)
		assertCommittedRefusal(t, mounted, "invalid_context_session")
		assertNoProviderRequest(t, provider.seen)
	})

	t.Run("legacy stale trigger", func(t *testing.T) {
		provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 1)}
		mounted, done, stop := mountCommittedTextModel(t, provider)
		defer stopTextModel(t, mounted, done, stop)

		sendCommittedSnapshot(t, mounted, committedTestSnapshot(), "context-v2", "session-a")
		version := uint64(1)
		send(t, mustIngress(t, mounted, "trigger"), element.Envelope{
			Type: cognitionelements.GenerateType(), ItemID: "trigger-legacy-stale",
			SessionID: "session-a", RunID: "legacy-stale",
			Payload: cognitionelements.Generate{
				ExpectedContextVersion: &version,
				ExpectedContextItemID:  "context-v1",
				Invocation:             continuation.Invocation{Instruction: "answer"},
			},
		})
		assertCommittedRefusal(t, mounted, "context_version_mismatch")
		assertNoProviderRequest(t, provider.seen)
	})
}

func TestTextModelCommittedTriggerWaitsForStateAndCanceledRunStaysTerminal(t *testing.T) {
	provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 2)}
	mounted, done, stop := mountCommittedTextModel(t, provider)
	defer stopTextModel(t, mounted, done, stop)

	snapshot := committedTestSnapshot()
	v1 := committedContext(t, snapshot, 1, "context-v1")
	sendCommittedGenerate(t, mounted, "wait-then-cancel", "session-a", v1)
	assertNoProviderRequest(t, provider.seen)
	send(t, mustIngress(t, mounted, "cancel"), element.Envelope{
		Type: cognitionelements.CancelType(), ItemID: "cancel-waiting",
		SessionID: "session-a", RunID: "wait-then-cancel",
		Payload: cognitionelements.Cancel{RunID: "wait-then-cancel", Reason: "withdrawn"},
	})
	canceled := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if canceled.Kind != cognitionelements.OutcomeCanceled || canceled.Code != "canceled_before_start" {
		t.Fatalf("waiting cancellation = %+v", canceled)
	}

	prefix, err := trajectory.Prefix(snapshot, v1.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	sendCommittedSnapshot(t, mounted, prefix, "context-v1", "session-a")
	sendCommittedGenerate(t, mounted, "wait-then-cancel", "session-a", v1)
	assertCommittedRefusal(t, mounted, "committed_run_replay")
	assertNoProviderRequest(t, provider.seen)

	// Cancellation happened before prefix validation, so it terminalizes the
	// addressed RunID but does not poison the committed version for a new run.
	sendCommittedGenerate(t, mounted, "replacement-v1", "session-a", v1)
	request := receiveProviderRequest(t, provider.seen)
	if request.Trajectory.Version != 1 {
		t.Fatalf("replacement trajectory = %+v", request.Trajectory)
	}
	_ = receive(t, mustEgress(t, mounted, "result"))
	if outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome); outcome.Kind != cognitionelements.OutcomeSucceeded {
		t.Fatalf("replacement outcome = %+v", outcome)
	}
}

func TestTextModelCommittedCancelBeforeQueuedTriggerTombstonesExactRun(t *testing.T) {
	provider := &scriptedProvider{
		descriptor: testDescriptor,
		seen:       make(chan continuation.Request, 1),
	}
	mounted, done, stop := mountCommittedTextModel(t, provider)
	defer stopTextModel(t, mounted, done, stop)

	const runID = "cancel-before-trigger"
	send(t, mustIngress(t, mounted, "cancel"), element.Envelope{
		Type: cognitionelements.CancelType(), ItemID: "cancel-before-trigger-control",
		SessionID: "session-a", RunID: runID, CancellationScope: runID,
		Payload: cognitionelements.Cancel{RunID: runID, Reason: "withdrawn before dequeue"},
	})
	canceled := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if canceled.Kind != cognitionelements.OutcomeCanceled ||
		canceled.Code != "canceled_before_start" || canceled.RunID != runID {
		t.Fatalf("idle pre-cancel outcome = %+v", canceled)
	}

	snapshot := committedTestSnapshot()
	binding := committedContext(t, snapshot, 1, "context-v1")
	sendCommittedSnapshot(t, mounted,
		trajectory.Snapshot{Version: 1, Items: snapshot.Items[:1]}, "context-v1", "session-a")
	sendCommittedGenerate(t, mounted, runID, "session-a", binding)
	replay := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if replay.Kind != cognitionelements.OutcomeRefused ||
		replay.Code != "committed_run_replay" || replay.RunID != runID {
		t.Fatalf("pre-canceled queued trigger outcome = %+v", replay)
	}
	assertNoProviderRequest(t, provider.seen)

	send(t, mustIngress(t, mounted, "cancel"), element.Envelope{
		Type: cognitionelements.CancelType(), ItemID: "duplicate-pre-cancel",
		SessionID: "session-a", RunID: runID, CancellationScope: runID,
		Payload: cognitionelements.Cancel{RunID: runID},
	})
	duplicate := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if duplicate.Kind != cognitionelements.OutcomeIgnored || duplicate.Code != "already_canceled" {
		t.Fatalf("duplicate idle pre-cancel outcome = %+v", duplicate)
	}
}

func TestTextModelCommittedTriggerBeforeStateRunsAfterVerifiedPrefixArrives(t *testing.T) {
	provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 1)}
	mounted, done, stop := mountCommittedTextModel(t, provider)
	defer stopTextModel(t, mounted, done, stop)

	snapshot := committedTestSnapshot()
	v1 := committedContext(t, snapshot, 1, "context-v1")
	sendCommittedGenerate(t, mounted, "trigger-before-state", "session-a", v1)
	assertNoProviderRequest(t, provider.seen)
	prefix, err := trajectory.Prefix(snapshot, v1.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	sendCommittedSnapshot(t, mounted, prefix, "context-v1", "session-a")
	request := receiveProviderRequest(t, provider.seen)
	if !reflect.DeepEqual(request.Trajectory, prefix) {
		t.Fatalf("trigger-before-State trajectory = %+v, want %+v", request.Trajectory, prefix)
	}
	_ = receive(t, mustEgress(t, mounted, "result"))
	if outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome); outcome.Kind != cognitionelements.OutcomeSucceeded || outcome.ContextVersion != 1 {
		t.Fatalf("trigger-before-State outcome = %+v", outcome)
	}
}

func TestTextModelCommittedConstraintMustAgreeExactly(t *testing.T) {
	tests := []struct {
		name string
		edit func(*cognitionelements.Generate)
		code string
	}{
		{
			name: "missing expected version",
			edit: func(generate *cognitionelements.Generate) { generate.ExpectedContextVersion = nil },
			code: "incomplete_committed_context",
		},
		{
			name: "missing expected item",
			edit: func(generate *cognitionelements.Generate) { generate.ExpectedContextItemID = "" },
			code: "incomplete_committed_context",
		},
		{
			name: "different version",
			edit: func(generate *cognitionelements.Generate) {
				generate.ExpectedContextVersion = uint64Pointer(2)
			},
			code: "committed_context_mismatch",
		},
		{
			name: "different item",
			edit: func(generate *cognitionelements.Generate) {
				generate.ExpectedContextItemID = "context-other"
			},
			code: "committed_context_mismatch",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 1)}
			mounted, done, stop := mountCommittedTextModel(t, provider)
			defer stopTextModel(t, mounted, done, stop)

			binding := committedContext(t, committedTestSnapshot(), 1, "context-v1")
			version := binding.Prefix.Version
			generate := cognitionelements.Generate{
				Invocation:             continuation.Invocation{Instruction: "answer"},
				ExpectedContextVersion: &version, ExpectedContextItemID: binding.StateItemID,
				CommittedContext: &binding,
			}
			testCase.edit(&generate)
			send(t, mustIngress(t, mounted, "trigger"), element.Envelope{
				Type: cognitionelements.GenerateType(), ItemID: "trigger-constraint",
				SessionID: "session-a", RunID: "constraint",
				CausalParents: []string{binding.StateItemID}, Payload: generate,
			})
			assertCommittedRefusal(t, mounted, testCase.code)
			assertNoProviderRequest(t, provider.seen)
		})
	}
}

func TestTextModelCommittedContextRequiresCanonicalIdentityAndCausalBinding(t *testing.T) {
	snapshot := committedTestSnapshot()
	valid := committedContext(t, snapshot, 1, "context-v1")
	tests := []struct {
		name      string
		sessionID string
		binding   stateelements.CommittedContext
		causes    []string
		code      string
	}{
		{
			name: "session surrounding whitespace", sessionID: " session-a",
			binding: valid, causes: []string{valid.StateItemID}, code: "invalid_context_session",
		},
		{
			name: "session control", sessionID: "session-a\n",
			binding: valid, causes: []string{valid.StateItemID}, code: "invalid_context_session",
		},
		{
			name: "session internal whitespace", sessionID: "session a",
			binding: valid, causes: []string{valid.StateItemID}, code: "invalid_context_session",
		},
		{
			name: "session invalid UTF-8", sessionID: string([]byte{'s', 0xff}),
			binding: valid, causes: []string{valid.StateItemID}, code: "invalid_context_session",
		},
		{
			name: "session size bound", sessionID: strings.Repeat("s", 257),
			binding: valid, causes: []string{valid.StateItemID}, code: "invalid_context_session",
		},
		{
			name: "State identity whitespace", sessionID: "session-a",
			binding: func() stateelements.CommittedContext {
				copy := valid
				copy.StateItemID = "context v1"
				return copy
			}(),
			causes: []string{"context v1"}, code: "invalid_context_identity",
		},
		{
			name: "State identity empty", sessionID: "session-a",
			binding: func() stateelements.CommittedContext {
				copy := valid
				copy.StateItemID = ""
				return copy
			}(),
			code: "invalid_context_identity",
		},
		{
			name: "State identity size bound", sessionID: "session-a",
			binding: func() stateelements.CommittedContext {
				copy := valid
				copy.StateItemID = strings.Repeat("c", 257)
				return copy
			}(),
			causes: []string{strings.Repeat("c", 257)}, code: "invalid_context_identity",
		},
		{
			name: "missing State cause", sessionID: "session-a",
			binding: valid, code: "missing_context_cause",
		},
		{
			name: "zero prefix version", sessionID: "session-a",
			binding: func() stateelements.CommittedContext {
				empty := trajectory.Snapshot{Version: 0, Items: []trajectory.Item{}}
				return committedContext(t, empty, 0, "context-v0")
			}(),
			causes: []string{"context-v0"}, code: "invalid_committed_context",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 1)}
			mounted, done, stop := mountCommittedTextModel(t, provider)
			defer stopTextModel(t, mounted, done, stop)

			version := testCase.binding.Prefix.Version
			send(t, mustIngress(t, mounted, "trigger"), element.Envelope{
				Type: cognitionelements.GenerateType(), ItemID: "trigger-invalid-binding",
				SessionID: testCase.sessionID, RunID: "invalid-binding",
				CausalParents: testCase.causes,
				Payload: cognitionelements.Generate{
					Invocation:             continuation.Invocation{Instruction: "answer"},
					ExpectedContextVersion: &version,
					ExpectedContextItemID:  testCase.binding.StateItemID,
					CommittedContext:       &testCase.binding,
				},
			})
			assertCommittedRefusal(t, mounted, testCase.code)
			assertNoProviderRequest(t, provider.seen)
		})
	}
}

func TestTextModelCommittedContextPinsOneMountedSession(t *testing.T) {
	provider := &scriptedProvider{descriptor: testDescriptor, seen: make(chan continuation.Request, 2)}
	mounted, done, stop := mountCommittedTextModel(t, provider)
	defer stopTextModel(t, mounted, done, stop)

	snapshot := committedTestSnapshot()
	v1 := committedContext(t, snapshot, 1, "context-v1")
	prefix, err := trajectory.Prefix(snapshot, v1.Prefix)
	if err != nil {
		t.Fatal(err)
	}
	sendCommittedSnapshot(t, mounted, prefix, "context-v1", "session-a")
	sendCommittedGenerate(t, mounted, "establish-session", "session-a", v1)
	_ = receiveProviderRequest(t, provider.seen)
	_ = receive(t, mustEgress(t, mounted, "result"))
	if outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome); outcome.Kind != cognitionelements.OutcomeSucceeded {
		t.Fatalf("session-establishing outcome = %+v", outcome)
	}

	v2 := committedContext(t, snapshot, 2, "context-v2")
	sendCommittedGenerate(t, mounted, "cross-session", "session-b", v2)
	assertCommittedRefusal(t, mounted, "committed_session_mismatch")
	assertNoProviderRequest(t, provider.seen)
}

func mountCommittedTextModel(
	t *testing.T, provider *scriptedProvider,
) (*graphruntime.Mounted, <-chan error, context.CancelFunc) {
	t.Helper()
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("primary", testDescriptor, func() (continuation.Provider, error) {
		return provider, nil
	}); err != nil {
		t.Fatal(err)
	}
	mounted, done, stop := mountTextModel(t, providers, json.RawMessage(`{"provider":"primary"}`))
	_ = receive(t, mustEgress(t, mounted, "resolved"))
	return mounted, done, stop
}

func committedTestSnapshot() trajectory.Snapshot {
	return trajectory.Snapshot{Version: 2, Items: []trajectory.Item{
		{
			ID: "observation-v1", Kind: trajectory.KindObservation, MonotonicNS: 1,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "first",
		},
		{
			ID: "observation-v2", Kind: trajectory.KindObservation, MonotonicNS: 2,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "second",
		},
	}}
}

func committedContext(
	t *testing.T, snapshot trajectory.Snapshot, version uint64, stateItemID string,
) stateelements.CommittedContext {
	t.Helper()
	identity, err := trajectory.IdentifyPrefix(snapshot, version)
	if err != nil {
		t.Fatal(err)
	}
	return stateelements.CommittedContext{Prefix: identity, StateItemID: stateItemID}
}

func sendCommittedSnapshot(
	t *testing.T, mounted *graphruntime.Mounted, snapshot trajectory.Snapshot,
	itemID, sessionID string,
) {
	t.Helper()
	send(t, mustIngress(t, mounted, "context"), element.Envelope{
		Type: cognitionelements.ContextType(), ItemID: itemID, SessionID: sessionID, Payload: snapshot,
	})
}

func sendCommittedGenerate(
	t *testing.T, mounted *graphruntime.Mounted, runID, sessionID string,
	binding stateelements.CommittedContext,
) {
	t.Helper()
	version := binding.Prefix.Version
	send(t, mustIngress(t, mounted, "trigger"), element.Envelope{
		Type: cognitionelements.GenerateType(), ItemID: "trigger-" + runID,
		SessionID: sessionID, RunID: runID, CausalParents: []string{binding.StateItemID},
		Payload: cognitionelements.Generate{
			Invocation:             continuation.Invocation{Instruction: "answer"},
			ExpectedContextVersion: &version, ExpectedContextItemID: binding.StateItemID,
			CommittedContext: &binding,
		},
	})
}

func assertCommittedRefusal(t *testing.T, mounted *graphruntime.Mounted, code string) {
	t.Helper()
	outcome := receive(t, mustEgress(t, mounted, "outcome")).Payload.(cognitionelements.Outcome)
	if outcome.Kind != cognitionelements.OutcomeRefused || outcome.Code != code {
		t.Fatalf("committed-context outcome = %+v, want refused/%s", outcome, code)
	}
}

func receiveProviderRequest(t *testing.T, requests <-chan continuation.Request) continuation.Request {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(time.Second):
		t.Fatal("provider was not invoked")
		return continuation.Request{}
	}
}

func assertNoProviderRequest(t *testing.T, requests <-chan continuation.Request) {
	t.Helper()
	select {
	case request := <-requests:
		t.Fatalf("provider unexpectedly received request: %+v", request)
	case <-time.After(20 * time.Millisecond):
	}
}

func replaceDigestTail(digest string) string {
	replacement := "0"
	if strings.HasSuffix(digest, replacement) {
		replacement = "1"
	}
	return digest[:len(digest)-1] + replacement
}
