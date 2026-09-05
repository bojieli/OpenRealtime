package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const trajectoryGraph = `graph trajectory_state {
    state.TrajectoryStore :: store;
    input append = store.append;
    output snapshot = store.snapshot;
    output committed = store.committed;
    output rejected = store.rejected;
}
`

const observationCommitGraph = `graph observation_commit {
    state.ObservationCommit :: commit;
    state.TrajectoryStore :: store;
    commit.append -> store.append;
    store.committed -> commit.committed;
    store.rejected -> commit.rejected;
    input observations = commit.observations;
    output outcome = commit.outcome;
    output snapshot = store.snapshot;
}
`

const observationCommitReplyGraph = `graph observation_commit_reply {
    state.ObservationCommit :: commit;
    input observations = commit.observations;
    input committed = commit.committed;
    input rejected = commit.rejected;
    output append = commit.append;
    output outcome = commit.outcome;
}
`

const sharedObservationCommitGraph = `graph shared_observation_commit {
    state.ObservationCommit :: audio_commit;
    state.ObservationCommit :: message_commit;
    state.TrajectoryStore :: store;
    flow.Mux :: append_mux;
    flow.Tee :: committed_copy;
    flow.Tee :: rejected_copy;
    audio_commit.append -> append_mux.in;
    message_commit.append -> append_mux.in;
    append_mux.out -> store.append;
    store.committed -> committed_copy.in;
    committed_copy.out -> audio_commit.committed;
    committed_copy.out -> message_commit.committed;
    store.rejected -> rejected_copy.in;
    rejected_copy.out -> audio_commit.rejected;
    rejected_copy.out -> message_commit.rejected;
    input audio = audio_commit.observations;
    input message = message_commit.observations;
    output audio_outcome = audio_commit.outcome;
    output message_outcome = message_commit.outcome;
    output snapshot = store.snapshot;
}
`

func TestTrajectoryStorePublishesSeedCommitsAndTypedRejections(t *testing.T) {
	graph := compileTrajectoryGraph(t)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	store := trajectory.NewStore()
	services := graphruntime.NewServiceSet()
	if _, err := services.Set(stateelements.TrajectoryStoreService,
		&stateelements.TrajectoryStoreServiceValue{Store: store}); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry, Services: services,
		Values: map[string]json.RawMessage{"store": json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- mounted.Run(runCtx) }()
	t.Cleanup(func() {
		cancelRun()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("trajectory graph did not stop")
		}
	})

	appendInput, _ := mounted.Ingress("append")
	snapshots, _ := mounted.Egress("snapshot")
	committed, _ := mounted.Egress("committed")
	rejected, _ := mounted.Egress("rejected")

	seedEnvelope, err := snapshots.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	seed, ok := seedEnvelope.Payload.(trajectory.Snapshot)
	if !ok || seed.Version != 0 || len(seed.Items) != 0 {
		t.Fatalf("seed snapshot = %#v", seedEnvelope.Payload)
	}
	assertPureStateResolution(t, mounted, "store", "state.TrajectoryStore")

	requestType := element.Request(
		element.Named("trajectory.Append"), element.Named("flow.RequestID"),
	)
	first := stateelements.Append{Compare: true, ExpectedVersion: 0, Items: []trajectory.Item{{
		ID: "user-1", Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello",
	}}}
	if _, err := appendInput.Broadcast(context.Background(), element.Envelope{
		Type: requestType, ItemID: "append-1", RunID: "turn-1", Payload: first,
	}); err != nil {
		t.Fatal(err)
	}
	stateEnvelope, err := snapshots.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state := stateEnvelope.Payload.(trajectory.Snapshot)
	if state.Version != 1 || len(state.Items) != 1 || state.Items[0].Content != "hello" {
		t.Fatalf("committed state = %+v", state)
	}
	commitEnvelope, err := committed.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	commit, ok := commitEnvelope.Payload.(stateelements.Commit)
	if !ok || commit.Version != 1 || commit.Snapshot.Version != 1 ||
		len(commit.AppendedIDs) != 1 || commit.AppendedIDs[0] != "user-1" ||
		commitEnvelope.RunID != "turn-1" ||
		commit.Context.StateItemID != stateEnvelope.ItemID ||
		!slices.Contains(commitEnvelope.CausalParents, stateEnvelope.ItemID) {
		t.Fatalf("commit = %#v, envelope = %+v", commitEnvelope.Payload, commitEnvelope)
	}
	if err := trajectory.VerifyPrefix(commit.Snapshot, commit.Context.Prefix); err != nil {
		t.Fatalf("commit context does not identify its exact snapshot: %v", err)
	}

	stale := stateelements.Append{Compare: true, ExpectedVersion: 0, Items: []trajectory.Item{{
		ID: "stale", Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "stale",
	}}}
	if _, err := appendInput.Broadcast(context.Background(), element.Envelope{
		Type: requestType, ItemID: "append-stale", RunID: "turn-2", Payload: stale,
	}); err != nil {
		t.Fatal(err)
	}
	rejectionEnvelope, err := rejected.Receive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rejection, ok := rejectionEnvelope.Payload.(stateelements.Rejection)
	if !ok || rejection.Code != "version_conflict" || rejection.CurrentVersion != 1 ||
		rejection.ExpectedVersion != 0 || rejectionEnvelope.RunID != "turn-2" {
		t.Fatalf("rejection = %#v, envelope = %+v", rejectionEnvelope.Payload, rejectionEnvelope)
	}
	if got := store.Snapshot(); got.Version != 1 || len(got.Items) != 1 {
		t.Fatalf("rejected append mutated store: %+v", got)
	}
}

func TestTrajectoryStoreRejectsInvalidConfigBeforeRun(t *testing.T) {
	graph := compileTrajectoryGraph(t)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	_, err = graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry,
		Values: map[string]json.RawMessage{"store": json.RawMessage(`{"unknown":true}`)},
	})
	if err == nil {
		t.Fatal("unknown state-element configuration was accepted")
	}
}

func TestTrajectoryStoreDescriptorIsInProductionCatalog(t *testing.T) {
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, found := catalog.Latest("state.TrajectoryStore")
	if !found {
		t.Fatal("production catalog has no trajectory store")
	}
	if err := descriptor.Validate(); err != nil {
		t.Fatal(err)
	}
	if descriptor.Revision != 2 {
		t.Fatalf("trajectory store descriptor revision = %d, want 2", descriptor.Revision)
	}
	if !descriptor.Reaction.BreaksCycles {
		t.Fatal("seeded trajectory state does not declare its causal break")
	}
	observationDescriptor, found := catalog.Latest("state.ObservationCommit")
	if !found || observationDescriptor.Revision != 2 {
		t.Fatalf("observation commit descriptor = %+v, found=%t; want revision 2",
			observationDescriptor, found)
	}
}

func TestObservationCommitSerializesRevisionsThroughAuthoritativeStoreReplies(t *testing.T) {
	graph := compileStateGraph(t, observationCommitGraph)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry,
		Values: map[string]json.RawMessage{
			"commit": json.RawMessage(`{}`), "store": json.RawMessage(`{}`),
		},
		Now: func() uint64 { return 42 },
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- mounted.Run(runCtx) }()
	defer func() {
		cancelRun()
		select {
		case <-runDone:
		case <-time.After(time.Second):
			t.Error("observation commit graph did not stop")
		}
	}()

	input, _ := mounted.Ingress("observations")
	snapshots, _ := mounted.Egress("snapshot")
	outcomes, _ := mounted.Egress("outcome")
	if seed := receiveState(t, snapshots).Payload.(trajectory.Snapshot); seed.Version != 0 {
		t.Fatalf("seed = %+v", seed)
	}
	assertPureStateResolution(t, mounted, "store", "state.TrajectoryStore")
	assertPureStateResolution(t, mounted, "commit", "state.ObservationCommit")
	typeOf := element.Revisions(
		element.Named("perception.Observation"), element.Named("perception.RevisionID"),
	)
	partial := perception.Observation{
		Text: "hello", Observer: "audio", Source: "microphone",
		Authority: trajectory.AuthorityUser, Revision: 1, StableText: "hello", Provisional: true,
	}
	final := perception.Observation{
		Text: "hello world", Observer: "audio", Source: "microphone",
		Authority: trajectory.AuthorityUser, Revision: 2, Supersedes: 1,
		StableText: "hello world", Final: true,
	}
	if _, err := input.Broadcast(context.Background(), element.Envelope{
		Type: typeOf, ItemID: "asr-missing-session", SourceID: "missing-session",
		Payload: partial,
	}); err != nil {
		t.Fatal(err)
	}
	missingSession := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	if missingSession.Kind != stateelements.ObservationRejected || missingSession.Code != "missing_session" {
		t.Fatalf("missing-session outcome = %+v", missingSession)
	}
	for index, observation := range []perception.Observation{partial, final} {
		if _, err := input.Broadcast(context.Background(), element.Envelope{
			Type: typeOf, ItemID: fmt.Sprintf("asr-%d", index+1), SourceID: "utterance-1",
			SessionID: "session-state", Payload: observation,
		}); err != nil {
			t.Fatal(err)
		}
	}
	firstStateEnvelope := receiveState(t, snapshots)
	firstSnapshot := firstStateEnvelope.Payload.(trajectory.Snapshot)
	firstOutcomeEnvelope := receiveState(t, outcomes)
	firstOutcome := firstOutcomeEnvelope.Payload.(stateelements.ObservationCommitOutcome)
	secondStateEnvelope := receiveState(t, snapshots)
	secondSnapshot := secondStateEnvelope.Payload.(trajectory.Snapshot)
	secondOutcomeEnvelope := receiveState(t, outcomes)
	secondOutcome := secondOutcomeEnvelope.Payload.(stateelements.ObservationCommitOutcome)
	if firstOutcome.Kind != stateelements.ObservationCommitted || firstOutcome.SourceRevision != 1 ||
		secondOutcome.Kind != stateelements.ObservationCommitted || secondOutcome.SourceRevision != 2 {
		t.Fatalf("outcomes = %+v, %+v", firstOutcome, secondOutcome)
	}
	if firstSnapshot.Version != 1 || secondSnapshot.Version != 2 {
		t.Fatalf("snapshot versions = %d, %d", firstSnapshot.Version, secondSnapshot.Version)
	}
	for _, committed := range []struct {
		snapshot trajectory.Snapshot
		state    element.Envelope
		outcome  element.Envelope
		context  stateelements.CommittedContext
	}{
		{firstSnapshot, firstStateEnvelope, firstOutcomeEnvelope, firstOutcome.Context},
		{secondSnapshot, secondStateEnvelope, secondOutcomeEnvelope, secondOutcome.Context},
	} {
		if committed.context.StateItemID != committed.state.ItemID ||
			!slices.Contains(committed.outcome.CausalParents, committed.state.ItemID) {
			t.Fatalf("observation context is not causally bound: context=%+v state=%+v outcome=%+v",
				committed.context, committed.state, committed.outcome)
		}
		if err := trajectory.VerifyPrefix(committed.snapshot, committed.context.Prefix); err != nil {
			t.Fatalf("observation outcome prefix is invalid: %v", err)
		}
	}
	second := secondSnapshot.Items[1]
	if second.Event == nil || second.Event.SupersedesRevision != 1 ||
		!slices.Contains(second.CausalParentIDs, secondSnapshot.Items[0].ID) {
		t.Fatalf("superseding trajectory item = %+v", second)
	}

	unknown := final
	unknown.Revision, unknown.Supersedes = 100, 99
	if _, err := input.Broadcast(context.Background(), element.Envelope{
		Type: typeOf, ItemID: "asr-unknown", SourceID: "utterance-2",
		SessionID: "session-state", Payload: unknown,
	}); err != nil {
		t.Fatal(err)
	}
	rejected := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	if rejected.Kind != stateelements.ObservationRejected || rejected.Code != "unknown_superseded_revision" {
		t.Fatalf("unknown supersession outcome = %+v", rejected)
	}
	assertNoStateEnvelope(t, snapshots)
}

func TestObservationCommitFanoutIgnoresOnlyForeignInstanceReceipts(t *testing.T) {
	graph := compileStateGraph(t, sharedObservationCommitGraph)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry,
		Values: map[string]json.RawMessage{
			"audio_commit": json.RawMessage(`{}`), "message_commit": json.RawMessage(`{}`),
			"store": json.RawMessage(`{}`),
		},
		Now: func() uint64 { return 42 },
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- mounted.Run(runCtx) }()
	defer func() {
		cancelRun()
		select {
		case err := <-runDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("shared observation commit graph: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("shared observation commit graph did not stop")
		}
	}()

	audio, _ := mounted.Ingress("audio")
	message, _ := mounted.Ingress("message")
	audioOutcomes, _ := mounted.Egress("audio_outcome")
	messageOutcomes, _ := mounted.Egress("message_outcome")
	snapshots, _ := mounted.Egress("snapshot")
	if seed := receiveState(t, snapshots).Payload.(trajectory.Snapshot); seed.Version != 0 {
		t.Fatalf("shared store seed = %+v", seed)
	}

	typeOf := stateelements.ObservationType()
	sendObservation := func(port element.OutputPort, itemID, stream, text string) {
		t.Helper()
		delivery, sendErr := port.Broadcast(context.Background(), element.Envelope{
			Type: typeOf, ItemID: itemID, SessionID: "session-shared-store", SourceID: stream,
			Payload: perception.Observation{
				Text: text, Observer: "client", Source: stream,
				Authority: trajectory.AuthorityUser, Revision: 1, StableText: text, Final: true,
			},
		})
		if sendErr != nil || delivery.Delivered != 1 || delivery.Dropped != 0 {
			t.Fatalf("send shared observation = %+v, %v", delivery, sendErr)
		}
	}

	sendObservation(audio, "audio-final", "audio-stream", "heard speech")
	first := receiveState(t, snapshots).Payload.(trajectory.Snapshot)
	audioOutcome := receiveState(t, audioOutcomes).Payload.(stateelements.ObservationCommitOutcome)
	if first.Version != 1 || audioOutcome.Kind != stateelements.ObservationCommitted ||
		audioOutcome.StreamID != "audio-stream" {
		t.Fatalf("audio shared-store commit = %+v / %+v", first, audioOutcome)
	}
	assertNoStateEnvelope(t, messageOutcomes)

	sendObservation(message, "message-final", "message-stream", "typed request")
	second := receiveState(t, snapshots).Payload.(trajectory.Snapshot)
	messageOutcome := receiveState(t, messageOutcomes).Payload.(stateelements.ObservationCommitOutcome)
	if second.Version != 2 || len(second.Items) != 2 ||
		messageOutcome.Kind != stateelements.ObservationCommitted ||
		messageOutcome.StreamID != "message-stream" {
		t.Fatalf("message shared-store commit = %+v / %+v", second, messageOutcome)
	}
	assertNoStateEnvelope(t, audioOutcomes)
}

func TestObservationCommitRetriesExactMonotonicConflictWithNewRequestIdentity(t *testing.T) {
	graph := compileStateGraph(t, observationCommitReplyGraph)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Uint64
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry,
		Values: map[string]json.RawMessage{"commit": json.RawMessage(`{}`)},
		Now:    func() uint64 { return clock.Add(10) },
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- mounted.Run(runCtx) }()
	defer func() {
		cancelRun()
		select {
		case err := <-runDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("observation retry graph: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("observation retry graph did not stop")
		}
	}()

	observations, _ := mounted.Ingress("observations")
	rejected, _ := mounted.Ingress("rejected")
	appends, _ := mounted.Egress("append")
	outcomes, _ := mounted.Egress("outcome")
	sendStateEnvelope(t, observations, element.Envelope{
		Type: stateelements.ObservationType(), ItemID: "retry-observation",
		SessionID: "session-retry", SourceID: "stream-retry",
		Payload: perception.Observation{
			Text: "retry me", Observer: "client", Source: "message",
			Authority: trajectory.AuthorityUser, Revision: 1, StableText: "retry me", Final: true,
		},
	})
	firstEnvelope := receiveState(t, appends)
	first := firstEnvelope.Payload.(stateelements.Append)
	if len(first.Items) != 1 {
		t.Fatalf("first observation append = %+v", first)
	}
	itemIndex := 0
	monotonicRejection := element.Envelope{
		Type: stateelements.RejectionType(), ItemID: firstEnvelope.ItemID + ":rejected",
		SessionID: "session-retry", CausalParents: []string{firstEnvelope.ItemID},
		Payload: stateelements.Rejection{
			Code:           "invalid_batch",
			Message:        "trajectory item 0 (observation retry): trajectory monotonic time moved backwards",
			CurrentVersion: 1,
			ItemIndex:      &itemIndex, ItemID: first.Items[0].ID,
		},
	}
	sendStateEnvelope(t, rejected, monotonicRejection)
	retryEnvelope := receiveState(t, appends)
	retry := retryEnvelope.Payload.(stateelements.Append)
	firstSemantic, retrySemantic := first.Items[0], retry.Items[0]
	firstSemantic.MonotonicNS, retrySemantic.MonotonicNS = 0, 0
	if retryEnvelope.ItemID == firstEnvelope.ItemID ||
		retryEnvelope.ItemID != firstEnvelope.ItemID+"-retry-1" || len(retry.Items) != 1 ||
		!reflect.DeepEqual(retrySemantic, firstSemantic) ||
		retry.Items[0].MonotonicNS < first.Items[0].MonotonicNS ||
		!slices.Contains(retryEnvelope.CausalParents, monotonicRejection.ItemID) {
		t.Fatalf("causal observation retry = %+v / %+v", retry, retryEnvelope)
	}
	assertNoStateEnvelope(t, outcomes)
	terminal := element.Envelope{
		Type: stateelements.RejectionType(), ItemID: retryEnvelope.ItemID + ":rejected",
		SessionID: "session-retry", CausalParents: []string{retryEnvelope.ItemID},
		Payload: stateelements.Rejection{
			Code: "invalid_batch", Message: "terminal test rejection", CurrentVersion: 1,
			ItemIndex: &itemIndex, ItemID: retry.Items[0].ID,
		},
	}
	sendStateEnvelope(t, rejected, terminal)
	outcome := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	if outcome.Kind != stateelements.ObservationRejected || outcome.Code != "invalid_batch" {
		t.Fatalf("terminal retry outcome = %+v", outcome)
	}
	sendStateEnvelope(t, rejected, terminal)
	replay := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	if replay.Code != "unknown_rejection_reply" {
		t.Fatalf("retry rejection replay = %+v", replay)
	}
}

func TestObservationCommitMountedReplyOwnershipAndReplayAreExact(t *testing.T) {
	graph := compileStateGraph(t, observationCommitReplyGraph)
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: graph, Registry: registry,
		Values: map[string]json.RawMessage{"commit": json.RawMessage(`{}`)},
		Now:    func() uint64 { return 42 },
	})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- mounted.Run(runCtx) }()
	defer func() {
		cancelRun()
		select {
		case err := <-runDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("observation reply graph: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("observation reply graph did not stop")
		}
	}()

	observations, _ := mounted.Ingress("observations")
	committed, _ := mounted.Ingress("committed")
	rejected, _ := mounted.Ingress("rejected")
	appends, _ := mounted.Egress("append")
	outcomes, _ := mounted.Egress("outcome")

	foreign := element.Envelope{
		Type: stateelements.CommitType(), ItemID: "other_commit-append-1:committed",
		SessionID: "session-reply", CausalParents: []string{"other_commit-append-1"},
		Payload: stateelements.Commit{},
	}
	sendStateEnvelope(t, committed, foreign)
	assertNoStateEnvelope(t, outcomes)

	sendStateEnvelope(t, observations, element.Envelope{
		Type: stateelements.ObservationType(), ItemID: "typed-observation-1",
		SessionID: "session-reply", SourceID: "typed-stream",
		Payload: perception.Observation{
			Text: "typed request", Observer: "client", Source: "message",
			Authority: trajectory.AuthorityUser, Revision: 1, StableText: "typed request", Final: true,
		},
	})
	appendEnvelope := receiveState(t, appends)
	appendRequest := appendEnvelope.Payload.(stateelements.Append)
	if len(appendRequest.Items) != 1 || appendEnvelope.ItemID != "commit-append-1" {
		t.Fatalf("observation append = %+v / %+v", appendRequest, appendEnvelope)
	}
	reply, commit := mountedObservationCommitReply(t, appendEnvelope, "session-reply")
	sendStateEnvelope(t, committed, reply)
	accepted := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	if accepted.Kind != stateelements.ObservationCommitted || accepted.StoreVersion != 1 ||
		accepted.Context != commit.Context {
		t.Fatalf("mounted observation commit = %+v", accepted)
	}

	sendStateEnvelope(t, committed, reply)
	replay := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	if replay.Kind != stateelements.ObservationRejected || replay.Code != "unknown_commit_reply" {
		t.Fatalf("same-instance commit replay = %+v", replay)
	}
	sendStateEnvelope(t, rejected, element.Envelope{
		Type: stateelements.RejectionType(), ItemID: "commit-append-999:rejected",
		SessionID: "session-reply", CausalParents: []string{"commit-append-999"},
		Payload: stateelements.Rejection{Code: "forged", Message: "forged"},
	})
	unknown := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	if unknown.Kind != stateelements.ObservationRejected || unknown.Code != "unknown_rejection_reply" {
		t.Fatalf("same-instance unknown rejection = %+v", unknown)
	}

	for _, itemID := range []string{
		"other-commit-append-2:committed", "Commit-append-2:committed",
		"commit-append-2suffix:committed", "commit-append-02:committed",
	} {
		nearCollision := foreign.Clone()
		nearCollision.ItemID = itemID
		nearCollision.CausalParents = []string{strings.TrimSuffix(itemID, ":committed")}
		sendStateEnvelope(t, committed, nearCollision)
		assertNoStateEnvelope(t, outcomes)
	}
}

func TestObservationCommitMountedMatchingReplyTamperFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*element.Envelope, *stateelements.Commit)
		want   string
	}{
		{
			name: "cross session",
			mutate: func(envelope *element.Envelope, _ *stateelements.Commit) {
				envelope.SessionID = "other-session"
			},
			want: "does not match pending observation session",
		},
		{
			name: "tampered appended identity",
			mutate: func(_ *element.Envelope, commit *stateelements.Commit) {
				commit.AppendedIDs[0] = "forged-observation"
			},
			want: "do not exactly attest pending observation",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph := compileStateGraph(t, observationCommitReplyGraph)
			registry, err := elements.RuntimeRegistry()
			if err != nil {
				t.Fatal(err)
			}
			mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
				Graph: graph, Registry: registry,
				Values: map[string]json.RawMessage{"commit": json.RawMessage(`{}`)},
				Now:    func() uint64 { return 42 },
			})
			if err != nil {
				t.Fatal(err)
			}
			runCtx, cancelRun := context.WithCancel(context.Background())
			defer cancelRun()
			runDone := make(chan error, 1)
			go func() { runDone <- mounted.Run(runCtx) }()

			observations, _ := mounted.Ingress("observations")
			committed, _ := mounted.Ingress("committed")
			appends, _ := mounted.Egress("append")
			sendStateEnvelope(t, observations, element.Envelope{
				Type: stateelements.ObservationType(), ItemID: "typed-observation-1",
				SessionID: "session-reply", SourceID: "typed-stream",
				Payload: perception.Observation{
					Text: "typed request", Observer: "client", Source: "message",
					Authority: trajectory.AuthorityUser, Revision: 1,
					StableText: "typed request", Final: true,
				},
			})
			appendEnvelope := receiveState(t, appends)
			reply, commit := mountedObservationCommitReply(t, appendEnvelope, "session-reply")
			test.mutate(&reply, &commit)
			reply.Payload = commit
			sendStateEnvelope(t, committed, reply)
			select {
			case runErr := <-runDone:
				if runErr == nil || !strings.Contains(runErr.Error(), test.want) {
					t.Fatalf("tampered matching reply error = %v, want %q", runErr, test.want)
				}
			case <-time.After(time.Second):
				t.Fatal("tampered matching reply did not fail the mounted graph")
			}
		})
	}
}

func mountedObservationCommitReply(
	t testing.TB, appendEnvelope element.Envelope, sessionID string,
) (element.Envelope, stateelements.Commit) {
	t.Helper()
	appendRequest := appendEnvelope.Payload.(stateelements.Append)
	snapshot := trajectory.Snapshot{Version: 1, Items: slices.Clone(appendRequest.Items)}
	prefix, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		t.Fatal(err)
	}
	commit := stateelements.Commit{
		Version: 1, AppendedIDs: []string{appendRequest.Items[0].ID}, Snapshot: snapshot,
		Context: stateelements.CommittedContext{
			Prefix: prefix, StateItemID: "trajectory-state-1",
		},
	}
	return element.Envelope{
		Type: stateelements.CommitType(), ItemID: appendEnvelope.ItemID + ":committed",
		SessionID:     sessionID,
		CausalParents: []string{appendEnvelope.ItemID, commit.Context.StateItemID},
		Payload:       commit,
	}, commit
}

func compileTrajectoryGraph(t *testing.T) ir.Graph {
	return compileStateGraph(t, trajectoryGraph)
}

func compileStateGraph(t *testing.T, source string) ir.Graph {
	t.Helper()
	parsed, err := syntax.Parse("state.ortg", []byte(source))
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

func receiveState(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}

func sendStateEnvelope(t *testing.T, output element.OutputPort, envelope element.Envelope) {
	t.Helper()
	delivery, err := output.Broadcast(context.Background(), envelope)
	if err != nil || delivery.Delivered != 1 || delivery.Dropped != 0 {
		t.Fatalf("send state envelope = %+v, %v", delivery, err)
	}
}

func assertNoStateEnvelope(t *testing.T, input element.InputPort) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if envelope, err := input.Receive(ctx); err == nil {
		t.Fatalf("unexpected state envelope %+v", envelope)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait for absent state envelope: %v", err)
	}
}

func assertPureStateResolution(
	t *testing.T, mounted *graphruntime.Mounted, node, elementName string,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		resolution := mounted.Live().Nodes[node].Resolution
		wantRevision := "implementation:3"
		if elementName == "state.ObservationCommit" {
			wantRevision = "implementation:4"
		}
		if resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive &&
			resolution.Runtime.ID == "builtin://openrealtime/elements/"+elementName &&
			resolution.Runtime.Revision == wantRevision &&
			resolution.CapabilitiesEvidence == inspect.EvidenceLive &&
			len(resolution.Capabilities) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pure state node %s live resolution = %+v", node, resolution)
		}
		time.Sleep(time.Millisecond)
	}
}
