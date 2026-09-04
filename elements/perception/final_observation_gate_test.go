package perception_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	coreperception "github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const finalObservationGateGraph = `graph final_observation_gate {
    perception.FinalObservationGate :: gate;
	input observations = gate.observations;
	input flush = gate.flush;
	output admitted = gate.admitted;
	output finals = gate.finals;
    output outcome = gate.outcome;
}`

const finalObservationCommitGraph = `graph final_observation_commit {
    perception.FinalObservationGate :: gate;
    state.ObservationCommit :: commit;
    state.TrajectoryStore :: store;
	gate.admitted -> commit.observations;
    commit.append -> store.append;
    store.committed -> commit.committed;
    store.rejected -> commit.rejected;
    input observations = gate.observations;
    input flush = gate.flush;
    output gate_outcome = gate.outcome;
    output commit_outcome = commit.outcome;
    output snapshot = store.snapshot;
}`

func TestFinalObservationGateAdmitsOrderedPartialsAndOnlyFlushAttestedFinal(t *testing.T) {
	mounted, done, cancel := mountFinalObservationGate(t)
	defer stopASR(t, mounted, done, cancel)
	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	admitted, _ := mounted.Egress("admitted")
	finals, _ := mounted.Egress("finals")
	outcomes, _ := mounted.Egress("outcome")

	partial := gateObservationEnvelope("session-a", "stream-a", "observe-a", "partial-a", coreperception.Observation{
		Text: "hello", Observer: "asr", Source: "microphone", Authority: trajectory.AuthorityUser,
		Revision: 1, StableText: "hel", Provisional: true,
	})
	sendGate(t, observations, partial)
	emittedPartialEnvelope := receive(t, admitted)
	emittedPartial := emittedPartialEnvelope.Payload.(coreperception.Observation)
	if emittedPartial.Revision != 1 || emittedPartial.Supersedes != 0 || !emittedPartial.Provisional ||
		emittedPartial.Final || emittedPartialEnvelope.ItemID == partial.ItemID ||
		len(emittedPartialEnvelope.CausalParents) != 1 || emittedPartialEnvelope.CausalParents[0] != partial.ItemID {
		t.Fatalf("admitted provisional = %+v / %+v", emittedPartial, emittedPartialEnvelope)
	}
	partialOutcome := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	if partialOutcome.Kind != perceptionelements.ProvisionalObservationEmitted ||
		partialOutcome.Code != "provisional_admitted" || partialOutcome.Revision != 1 ||
		partialOutcome.EmittedObservationItemID != emittedPartialEnvelope.ItemID {
		t.Fatalf("provisional outcome = %+v", partialOutcome)
	}
	assertNoGateEnvelope(t, finals)

	terminal := gateObservationEnvelope("session-a", "stream-a", "flush-a", "final-a", coreperception.Observation{
		Text: "hello world", Observer: "asr", Source: "microphone", Authority: trajectory.AuthorityUser,
		Revision: 2, Supersedes: 1, StableText: "hello world", Final: true,
	})
	sendGate(t, observations, terminal)
	pendingEnvelope := receive(t, outcomes)
	pending := pendingEnvelope.Payload.(perceptionelements.FinalObservationGateOutcome)
	if pending.Kind != perceptionelements.FinalObservationPending || pending.Code != "awaiting_flush" {
		t.Fatalf("final pending outcome = %+v", pending)
	}
	assertNoGateEnvelope(t, finals)

	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "flush-a", "flush-a:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	emittedEnvelope := receive(t, admitted)
	emitted := emittedEnvelope.Payload.(coreperception.Observation)
	if emitted.Revision != 2 || emitted.Supersedes != 1 || !emitted.Final || emitted.Provisional ||
		emittedEnvelope.ItemID == "final-a" ||
		len(emittedEnvelope.CausalParents) != 2 || emittedEnvelope.CausalParents[0] != "final-a" ||
		emittedEnvelope.CausalParents[1] != "flush-a:outcome" {
		t.Fatalf("emitted final = %+v / %+v", emitted, emittedEnvelope)
	}
	finalOnlyEnvelope := receive(t, finals)
	if finalOnly := finalOnlyEnvelope.Payload.(coreperception.Observation); finalOnly.Revision != 2 || finalOnly.Supersedes != 1 || finalOnlyEnvelope.ItemID != emittedEnvelope.ItemID {
		t.Fatalf("final-only observation = %+v / %+v", finalOnly, finalOnlyEnvelope)
	}
	gateOutcomeEnvelope := receive(t, outcomes)
	gateOutcome := gateOutcomeEnvelope.Payload.(perceptionelements.FinalObservationGateOutcome)
	if gateOutcome.Kind != perceptionelements.FinalObservationEmitted || gateOutcome.Revision != 2 ||
		gateOutcome.SourceObservationItemID != "final-a" ||
		gateOutcome.EmittedObservationItemID != emittedEnvelope.ItemID ||
		len(gateOutcomeEnvelope.CausalParents) != 3 ||
		gateOutcomeEnvelope.CausalParents[0] != "final-a" ||
		gateOutcomeEnvelope.CausalParents[1] != "flush-a:outcome" ||
		gateOutcomeEnvelope.CausalParents[2] != emittedEnvelope.ItemID {
		t.Fatalf("emitted outcome = %+v / %+v", gateOutcome, gateOutcomeEnvelope)
	}

	// Replaying either half cannot activate a second generation.
	sendGate(t, observations, terminal)
	replayEnvelope := receive(t, outcomes)
	replay := replayEnvelope.Payload.(perceptionelements.FinalObservationGateOutcome)
	if replay.Kind != perceptionelements.FinalObservationIgnored || replay.Code != "terminal_replay" {
		t.Fatalf("replay outcome = %+v", replay)
	}
	if replayEnvelope.ItemID == pendingEnvelope.ItemID {
		t.Fatalf("gate outcome reused immutable item ID %q", replayEnvelope.ItemID)
	}
	assertNoGateEnvelope(t, admitted)
	assertNoGateEnvelope(t, finals)
}

func TestFinalObservationGateRefusesCrossSessionJoinAndClearsCanceledFlush(t *testing.T) {
	mounted, done, cancel := mountFinalObservationGate(t)
	defer stopASR(t, mounted, done, cancel)
	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	admitted, _ := mounted.Egress("admitted")
	finals, _ := mounted.Egress("finals")
	outcomes, _ := mounted.Egress("outcome")

	final := gateObservationEnvelope("session-a", "stream-a", "flush-shared", "final-a", coreperception.Observation{
		Text: "cancel me", Observer: "asr", Authority: trajectory.AuthorityUser, Revision: 1, Final: true,
	})
	sendGate(t, observations, final)
	_ = receive(t, outcomes)
	// Same stream/cause from another session is a distinct pending join.
	sendGate(t, flush, gateFlushEnvelope("session-b", "stream-a", "flush-shared", "flush-b:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	otherPending := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	if otherPending.Kind != perceptionelements.FinalObservationPending || otherPending.Code != "awaiting_final" {
		t.Fatalf("cross-session flush = %+v", otherPending)
	}
	assertNoGateEnvelope(t, finals)
	assertNoGateEnvelope(t, admitted)

	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "flush-shared", "flush-a:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeCanceled, Operation: "flush",
			StreamID: "stream-a", Code: "canceled", Message: "barge-in"}))
	canceled := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	if canceled.Kind != perceptionelements.FinalObservationCanceled || canceled.Code != "canceled" {
		t.Fatalf("canceled flush = %+v", canceled)
	}
	assertNoGateEnvelope(t, finals)
	assertNoGateEnvelope(t, admitted)
}

func TestFinalObservationGateJoinsFlushArrivingBeforeFinalExactlyOnce(t *testing.T) {
	mounted, done, cancel := mountFinalObservationGate(t)
	defer stopASR(t, mounted, done, cancel)
	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	admitted, _ := mounted.Egress("admitted")
	finals, _ := mounted.Egress("finals")
	outcomes, _ := mounted.Egress("outcome")

	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "flush-a", "flush-a:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	if pending := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); pending.Kind != perceptionelements.FinalObservationPending || pending.Code != "awaiting_final" {
		t.Fatalf("early flush outcome = %+v", pending)
	}
	sendGate(t, observations, gateObservationEnvelope("session-a", "stream-a", "flush-a", "final-a",
		coreperception.Observation{Text: "hello", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 1, Final: true}))
	if final := receive(t, admitted).Payload.(coreperception.Observation); final.Revision != 1 || !final.Final {
		t.Fatalf("reverse-order admitted final = %+v", final)
	}
	if final := receive(t, finals).Payload.(coreperception.Observation); final.Revision != 1 || !final.Final {
		t.Fatalf("reverse-order final = %+v", final)
	}
	if emitted := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); emitted.Kind != perceptionelements.FinalObservationEmitted {
		t.Fatalf("reverse-order outcome = %+v", emitted)
	}
}

func TestFinalObservationGateCommitsEveryRevisionAndOnlyFlushAttestedFinal(t *testing.T) {
	graph := compileFinalObservationSource(t, finalObservationCommitGraph)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry,
		Values: map[string]json.RawMessage{
			"gate":   json.RawMessage(`{"admit_provisional":true}`),
			"commit": json.RawMessage(`{}`), "store": json.RawMessage(`{}`),
		},
		Now: func() uint64 { return 42 },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	defer stopASR(t, mounted, done, cancel)

	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	gateOutcomes, _ := mounted.Egress("gate_outcome")
	commitOutcomes, _ := mounted.Egress("commit_outcome")
	snapshots, _ := mounted.Egress("snapshot")
	if seed := receive(t, snapshots).Payload.(trajectory.Snapshot); seed.Version != 0 {
		t.Fatalf("final-gate store seed = %+v", seed)
	}

	// Speaker attribution is itself provisional. The first live hypothesis can
	// look like the session user and the corrected revision can be attributed to
	// somebody else without becoming a different ASR revision stream.
	partial := gateObservationEnvelope("session-a", "stream-a", "observe-a", "partial-a",
		coreperception.Observation{
			Text: "A copy by", Observer: "audio", Source: "microphone",
			Authority: trajectory.AuthorityUser, Revision: 1,
			StableText: "A copy by", Provisional: true,
		})
	sendGate(t, observations, partial)
	partialGate := receive(t, gateOutcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	if partialGate.Kind != perceptionelements.ProvisionalObservationEmitted || partialGate.Code != "provisional_admitted" {
		t.Fatalf("partial gate outcome = %+v", partialGate)
	}
	partialSnapshot := receive(t, snapshots).Payload.(trajectory.Snapshot)
	partialCommit := receive(t, commitOutcomes).Payload.(stateelements.ObservationCommitOutcome)
	if partialCommit.Kind != stateelements.ObservationCommitted || partialCommit.ObservationRevision != 1 ||
		partialSnapshot.Version != 1 || len(partialSnapshot.Items) != 1 ||
		partialSnapshot.Items[0].Content != "A copy by" || partialSnapshot.Items[0].Event == nil ||
		partialSnapshot.Items[0].Event.Type != "audio.revision" {
		t.Fatalf("provisional commit = %+v / %+v", partialCommit, partialSnapshot)
	}

	final := gateObservationEnvelope("session-a", "stream-a", "flush-a", "final-a",
		coreperception.Observation{
			Text: "A capybara wandered over", Observer: "audio", Source: "someone else in the room",
			Authority: trajectory.AuthorityUser, Revision: 2, Supersedes: 1,
			StableText: "A capybara wandered over", Final: true,
		})
	sendGate(t, observations, final)
	pending := receive(t, gateOutcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	if pending.Kind != perceptionelements.FinalObservationPending || pending.Code != "awaiting_flush" {
		t.Fatalf("final gate pending = %+v", pending)
	}
	assertNoGateEnvelope(t, commitOutcomes)

	flushEnvelope := gateFlushEnvelope("session-a", "stream-a", "flush-a", "flush-a:outcome",
		perceptionelements.Outcome{
			Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1,
		})
	sendGate(t, flush, flushEnvelope)
	emitted := receive(t, gateOutcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	committedSnapshot := receive(t, snapshots).Payload.(trajectory.Snapshot)
	committed := receive(t, commitOutcomes).Payload.(stateelements.ObservationCommitOutcome)
	if emitted.Kind != perceptionelements.FinalObservationEmitted ||
		committed.Kind != stateelements.ObservationCommitted || committed.ObservationRevision != 2 ||
		committedSnapshot.Version != 2 || len(committedSnapshot.Items) != 2 {
		t.Fatalf("flush-attested commit = gate %+v commit %+v snapshot %+v",
			emitted, committed, committedSnapshot)
	}
	item := committedSnapshot.Items[1]
	if item.Content != "A capybara wandered over" || item.Event == nil ||
		item.Event.CorrelationID != "stream-a" ||
		item.Event.SupersedesRevision != partialCommit.SourceRevision || item.SourceRevision != committed.SourceRevision {
		t.Fatalf("flush-attested trajectory item = %+v", item)
	}

	sendGate(t, observations, final)
	if replay := receive(t, gateOutcomes).Payload.(perceptionelements.FinalObservationGateOutcome); replay.Kind != perceptionelements.FinalObservationIgnored || replay.Code != "terminal_replay" {
		t.Fatalf("final replay outcome = %+v", replay)
	}
	sendGate(t, flush, flushEnvelope)
	if replay := receive(t, gateOutcomes).Payload.(perceptionelements.FinalObservationGateOutcome); replay.Kind != perceptionelements.FinalObservationIgnored || replay.Code != "terminal_replay" {
		t.Fatalf("flush replay outcome = %+v", replay)
	}
	assertNoGateEnvelope(t, commitOutcomes)
	assertNoGateEnvelope(t, snapshots)
}

func TestFinalObservationGatePreservesCancelFailureSemanticsAndRefusedCancelCannotErase(t *testing.T) {
	mounted, done, cancel := mountFinalObservationGate(t)
	defer stopASR(t, mounted, done, cancel)
	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	admitted, _ := mounted.Egress("admitted")
	finals, _ := mounted.Egress("finals")
	outcomes, _ := mounted.Egress("outcome")

	final := gateObservationEnvelope("session-a", "stream-a", "flush-a", "final-a",
		coreperception.Observation{Text: "keep me", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 1, Final: true})
	sendGate(t, observations, final)
	_ = receive(t, outcomes)
	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "cancel-refused", "cancel-refused:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeRefused, Operation: "cancel",
			StreamID: "stream-a", Code: "not_cancelled"}))
	refused := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	if refused.Kind != perceptionelements.FinalObservationRefused || refused.Code != "not_cancelled" {
		t.Fatalf("refused cancel = %+v", refused)
	}
	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "flush-a", "flush-a:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	_ = receive(t, admitted)
	_ = receive(t, finals)
	_ = receive(t, outcomes)

	sendGate(t, observations, gateObservationEnvelope("session-a", "stream-a", "flush-b", "final-b",
		coreperception.Observation{Text: "clear me", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 1, Final: true}))
	_ = receive(t, outcomes)
	failedCancel := gateFlushEnvelope("session-a", "stream-a", "cancel-failed", "cancel-failed:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeFailed, Operation: "cancel",
			StreamID: "stream-a", Code: "provider_close_failed"})
	sendGate(t, flush, failedCancel)
	failed := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	if failed.Kind != perceptionelements.FinalObservationFailed || failed.Code != "provider_close_failed" {
		t.Fatalf("failed cancel = %+v", failed)
	}
	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "flush-b", "flush-b:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	cleared := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	if cleared.Kind != perceptionelements.FinalObservationIgnored || cleared.Code != "terminal_replay" {
		t.Fatalf("flush after failed cancel = %+v", cleared)
	}
	assertNoGateEnvelope(t, finals)
	assertNoGateEnvelope(t, admitted)

	successfulCancel := gateFlushEnvelope("session-a", "stream-a", "cancel-success", "cancel-success:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeCanceled, Operation: "cancel",
			StreamID: "stream-a", Code: "canceled"})
	sendGate(t, flush, successfulCancel)
	if got := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); got.Kind != perceptionelements.FinalObservationCanceled {
		t.Fatalf("successful cancel = %+v", got)
	}
	sendGate(t, flush, successfulCancel)
	if got := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); got.Kind != perceptionelements.FinalObservationIgnored || got.Code != "terminal_replay" {
		t.Fatalf("cancel replay = %+v", got)
	}
}

func TestFinalObservationGateUsesExplicitDirectCauseAndRejectsNoncanonicalAddresses(t *testing.T) {
	mounted, done, cancel := mountFinalObservationGate(t)
	defer stopASR(t, mounted, done, cancel)
	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	admitted, _ := mounted.Egress("admitted")
	finals, _ := mounted.Egress("finals")
	outcomes, _ := mounted.Egress("outcome")

	final := gateObservationEnvelope("session-a", "stream-a", "cause-a", "final-a",
		coreperception.Observation{Text: "direct", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 1, Final: true})
	final.CausalParents = []string{"cause-b", "cause-a"}
	sendGate(t, observations, final)
	_ = receive(t, outcomes)
	crossed := gateFlushEnvelope("session-a", "stream-a", "cause-b", "flush-b:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1})
	crossed.CausalParents = []string{"cause-a", "cause-b"}
	sendGate(t, flush, crossed)
	if got := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); got.Kind != perceptionelements.FinalObservationPending || got.CauseItemID != "cause-b" {
		t.Fatalf("cross-cause flush = %+v", got)
	}
	assertNoGateEnvelope(t, finals)
	assertNoGateEnvelope(t, admitted)
	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "cause-a", "flush-a:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	_ = receive(t, admitted)
	_ = receive(t, finals)
	_ = receive(t, outcomes)

	invalids := []element.Envelope{
		gateObservationEnvelope("session-a", " stream-a ", "cause-c", "padded-stream",
			coreperception.Observation{Text: "bad", Observer: "asr", Authority: trajectory.AuthorityUser, Revision: 1, Final: true}),
		gateObservationEnvelope("session-a", "stream-a", " cause-d ", "padded-cause",
			coreperception.Observation{Text: "bad", Observer: "asr", Authority: trajectory.AuthorityUser, Revision: 1, Final: true}),
		gateObservationEnvelope(string([]byte{0xff}), "stream-a", "cause-e", "invalid-utf8",
			coreperception.Observation{Text: "bad", Observer: "asr", Authority: trajectory.AuthorityUser, Revision: 1, Final: true}),
		gateObservationEnvelope("session\u00a0id", "stream-a", "cause-f", "unicode-space",
			coreperception.Observation{Text: "bad", Observer: "asr", Authority: trajectory.AuthorityUser, Revision: 1, Final: true}),
	}
	for _, invalid := range invalids {
		sendGate(t, observations, invalid)
		outcome := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
		if outcome.Kind != perceptionelements.FinalObservationRefused || outcome.Code != "invalid_address" {
			t.Fatalf("noncanonical address %q = %+v", invalid.ItemID, outcome)
		}
	}
}

func TestFinalObservationGateRefusesSuccessfulFlushForOnlyProvisionalEvidence(t *testing.T) {
	mounted, done, cancel := mountFinalObservationGate(t)
	defer stopASR(t, mounted, done, cancel)
	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	admitted, _ := mounted.Egress("admitted")
	finals, _ := mounted.Egress("finals")
	outcomes, _ := mounted.Egress("outcome")
	sendGate(t, observations, gateObservationEnvelope("session-a", "stream-a", "flush-a", "partial-a",
		coreperception.Observation{Text: "partial", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 1, Provisional: true, StableText: "part"}))
	_ = receive(t, admitted)
	_ = receive(t, outcomes)
	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "flush-a", "flush-a:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	refused := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome)
	if refused.Kind != perceptionelements.FinalObservationRefused || refused.Code != "flush_observation_not_final" {
		t.Fatalf("provisional flush = %+v", refused)
	}
	assertNoGateEnvelope(t, finals)
}

func TestFinalObservationGateReplayProtectionSurvivesRendezvousWindow(t *testing.T) {
	mounted, done, cancel := mountFinalObservationGate(t)
	defer stopASR(t, mounted, done, cancel)
	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	admitted, _ := mounted.Egress("admitted")
	finals, _ := mounted.Egress("finals")
	outcomes, _ := mounted.Egress("outcome")
	var firstFinal, firstFlush element.Envelope
	for index := 0; index < 513; index++ {
		cause := fmt.Sprintf("flush-%d", index)
		finalEnvelope := gateObservationEnvelope("session-a", "stream-a", cause, fmt.Sprintf("final-%d", index),
			coreperception.Observation{Text: "terminal", Observer: "asr", Authority: trajectory.AuthorityUser,
				Revision: 1, Final: true})
		flushEnvelope := gateFlushEnvelope("session-a", "stream-a", cause, cause+":outcome",
			perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
				StreamID: "stream-a", ObservationCount: 1})
		if index == 0 {
			firstFinal, firstFlush = finalEnvelope, flushEnvelope
		}
		sendGate(t, observations, finalEnvelope)
		_ = receive(t, outcomes)
		sendGate(t, flush, flushEnvelope)
		_ = receive(t, admitted)
		_ = receive(t, finals)
		_ = receive(t, outcomes)
	}
	sendGate(t, observations, firstFinal)
	if replay := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); replay.Kind != perceptionelements.FinalObservationIgnored || replay.Code != "terminal_replay" {
		t.Fatalf("old final replay = %+v", replay)
	}
	sendGate(t, flush, firstFlush)
	if replay := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); replay.Kind != perceptionelements.FinalObservationIgnored || replay.Code != "terminal_replay" {
		t.Fatalf("old flush replay = %+v", replay)
	}
	assertNoGateEnvelope(t, finals)
	assertNoGateEnvelope(t, admitted)
}

func TestFinalObservationGateObserveCadenceDoesNotConsumeActivationReplayBudget(t *testing.T) {
	mounted, done, cancel := mountFinalObservationGate(t)
	defer stopASR(t, mounted, done, cancel)
	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	admitted, _ := mounted.Egress("admitted")
	finals, _ := mounted.Egress("finals")
	outcomes, _ := mounted.Egress("outcome")
	for index := 0; index < 4097; index++ {
		cause := fmt.Sprintf("observe-%d", index)
		sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", cause, cause+":outcome",
			perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "observe",
				StreamID: "stream-a", ObservationCount: 1}))
		_ = receive(t, outcomes)
	}
	sendGate(t, observations, gateObservationEnvelope("session-a", "stream-a", "flush-final", "final",
		coreperception.Observation{Text: "still live", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 1, Final: true}))
	_ = receive(t, outcomes)
	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "flush-final", "flush-final:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	_ = receive(t, admitted)
	_ = receive(t, finals)
	if emitted := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); emitted.Kind != perceptionelements.FinalObservationEmitted {
		t.Fatalf("post-cadence final = %+v", emitted)
	}
}

func TestFinalObservationGateEmitsValidatedSnapshotAndPoisonsAmbiguousEvidence(t *testing.T) {
	mounted, done, cancel := mountFinalObservationGate(t)
	defer stopASR(t, mounted, done, cancel)
	observations, _ := mounted.Ingress("observations")
	flush, _ := mounted.Ingress("flush")
	admitted, _ := mounted.Egress("admitted")
	finals, _ := mounted.Egress("finals")
	outcomes, _ := mounted.Egress("outcome")

	mutable := &coreperception.Observation{Text: "accepted", Observer: "asr", Authority: trajectory.AuthorityUser,
		Revision: 1, Final: true}
	envelope := gateObservationEnvelope("session-a", "stream-a", "snapshot", "snapshot-final", *mutable)
	envelope.Payload = mutable
	sendGate(t, observations, envelope)
	_ = receive(t, outcomes)
	mutable.Text, mutable.Revision = "mutated", 2
	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "snapshot", "snapshot:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	emitted := receive(t, admitted).Payload.(coreperception.Observation)
	_ = receive(t, finals)
	if emitted.Text != "accepted" || emitted.Revision != 1 {
		t.Fatalf("emitted observation drifted from accepted snapshot: %+v", emitted)
	}
	_ = receive(t, outcomes)

	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "invalid", "invalid:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	_ = receive(t, outcomes)
	sendGate(t, observations, gateObservationEnvelope("session-a", "stream-a", "invalid", "invalid-observation",
		coreperception.Observation{Text: "bad revision", Observer: "asr", Authority: trajectory.AuthorityUser,
			Final: true}))
	if invalid := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); invalid.Kind != perceptionelements.FinalObservationRefused || invalid.Code != "invalid_observation" {
		t.Fatalf("invalid evidence = %+v", invalid)
	}
	sendGate(t, observations, gateObservationEnvelope("session-a", "stream-a", "invalid", "later-valid",
		coreperception.Observation{Text: "later", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 1, Final: true}))
	if replay := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); replay.Kind != perceptionelements.FinalObservationIgnored || replay.Code != "terminal_replay" {
		t.Fatalf("valid evidence after invalid = %+v", replay)
	}

	sendGate(t, observations, gateObservationEnvelope("session-a", "stream-a", "duplicate", "duplicate-a",
		coreperception.Observation{Text: "first", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 1, Final: true}))
	_ = receive(t, outcomes)
	sendGate(t, observations, gateObservationEnvelope("session-a", "stream-a", "duplicate", "duplicate-b",
		coreperception.Observation{Text: "second", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 2, Supersedes: 1, Final: true}))
	if duplicate := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); duplicate.Kind != perceptionelements.FinalObservationRefused || duplicate.Code != "duplicate_final" {
		t.Fatalf("duplicate final = %+v", duplicate)
	}
	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "duplicate", "duplicate:outcome",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	if replay := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); replay.Kind != perceptionelements.FinalObservationIgnored || replay.Code != "terminal_replay" {
		t.Fatalf("flush after duplicate final = %+v", replay)
	}

	sendGate(t, observations, gateObservationEnvelope("session-a", "stream-a", "collision", "same-evidence",
		coreperception.Observation{Text: "collision", Observer: "asr", Authority: trajectory.AuthorityUser,
			Revision: 1, Final: true}))
	_ = receive(t, outcomes)
	sendGate(t, flush, gateFlushEnvelope("session-a", "stream-a", "collision", "same-evidence",
		perceptionelements.Outcome{Kind: perceptionelements.OutcomeSucceeded, Operation: "flush",
			StreamID: "stream-a", ObservationCount: 1}))
	if collision := receive(t, outcomes).Payload.(perceptionelements.FinalObservationGateOutcome); collision.Kind != perceptionelements.FinalObservationRefused || collision.Code != "evidence_identity_collision" {
		t.Fatalf("evidence identity collision = %+v", collision)
	}
	assertNoGateEnvelope(t, finals)
	assertNoGateEnvelope(t, admitted)
}

func mountFinalObservationGate(
	t *testing.T,
) (*graphruntime.Mounted, <-chan error, context.CancelFunc) {
	t.Helper()
	graph := compileFinalObservationGate(t)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Values: map[string]json.RawMessage{
			"gate": json.RawMessage(`{"admit_provisional":true}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return mounted, done, cancel
}

func compileFinalObservationGate(t *testing.T) ir.Graph {
	t.Helper()
	return compileFinalObservationSource(t, finalObservationGateGraph)
}

func compileFinalObservationSource(t *testing.T, source string) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse("final-observation-gate.ortg", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled.Graph
}

func gateObservationEnvelope(
	session, stream, cause, item string, observation coreperception.Observation,
) element.Envelope {
	return element.Envelope{
		Type: perceptionelements.ObservationType(), ItemID: item, SessionID: session,
		SourceID: stream, OpportunityID: cause, CancellationScope: stream,
		CausalParents: []string{cause}, Payload: observation,
	}
}

func gateFlushEnvelope(
	session, stream, cause, item string, outcome perceptionelements.Outcome,
) element.Envelope {
	return element.Envelope{
		Type: perceptionelements.OutcomeType(), ItemID: item, SessionID: session,
		SourceID: stream, OpportunityID: cause, CancellationScope: stream,
		CausalParents: []string{cause}, Payload: func() perceptionelements.Outcome {
			outcome.CauseItemID = cause
			return outcome
		}(),
	}
}

func sendGate(t *testing.T, output element.OutputPort, envelope element.Envelope) {
	t.Helper()
	result, err := output.Broadcast(context.Background(), envelope)
	if err != nil || result.Delivered != 1 || result.Dropped != 0 {
		t.Fatalf("send gate envelope = %+v, %v", result, err)
	}
}

func assertNoGateEnvelope(t *testing.T, input element.InputPort) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if envelope, err := input.Receive(ctx); err == nil {
		t.Fatalf("unexpected gate envelope %+v", envelope)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for absent gate envelope: %v", err)
	}
}
