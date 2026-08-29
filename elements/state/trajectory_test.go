package state_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
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
		commitEnvelope.RunID != "turn-1" {
		t.Fatalf("commit = %#v, envelope = %+v", commitEnvelope.Payload, commitEnvelope)
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
	if !descriptor.Reaction.BreaksCycles {
		t.Fatal("seeded trajectory state does not declare its causal break")
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
	for index, observation := range []perception.Observation{partial, final} {
		if _, err := input.Broadcast(context.Background(), element.Envelope{
			Type: typeOf, ItemID: fmt.Sprintf("asr-%d", index+1), SourceID: "utterance-1",
			Payload: observation,
		}); err != nil {
			t.Fatal(err)
		}
	}
	firstSnapshot := receiveState(t, snapshots).Payload.(trajectory.Snapshot)
	firstOutcome := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	secondSnapshot := receiveState(t, snapshots).Payload.(trajectory.Snapshot)
	secondOutcome := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	if firstOutcome.Kind != stateelements.ObservationCommitted || firstOutcome.SourceRevision != 1 ||
		secondOutcome.Kind != stateelements.ObservationCommitted || secondOutcome.SourceRevision != 2 {
		t.Fatalf("outcomes = %+v, %+v", firstOutcome, secondOutcome)
	}
	if firstSnapshot.Version != 1 || secondSnapshot.Version != 2 {
		t.Fatalf("snapshot versions = %d, %d", firstSnapshot.Version, secondSnapshot.Version)
	}
	second := secondSnapshot.Items[1]
	if second.Event == nil || second.Event.SupersedesRevision != 1 ||
		!slices.Contains(second.CausalParentIDs, secondSnapshot.Items[0].ID) {
		t.Fatalf("superseding trajectory item = %+v", second)
	}

	unknown := final
	unknown.Revision, unknown.Supersedes = 100, 99
	if _, err := input.Broadcast(context.Background(), element.Envelope{
		Type: typeOf, ItemID: "asr-unknown", SourceID: "utterance-2", Payload: unknown,
	}); err != nil {
		t.Fatal(err)
	}
	rejected := receiveState(t, outcomes).Payload.(stateelements.ObservationCommitOutcome)
	if rejected.Kind != stateelements.ObservationRejected || rejected.Code != "unknown_superseded_revision" {
		t.Fatalf("unknown supersession outcome = %+v", rejected)
	}
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

func assertPureStateResolution(
	t *testing.T, mounted *graphruntime.Mounted, node, elementName string,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		resolution := mounted.Live().Nodes[node].Resolution
		if resolution != nil && resolution.RuntimeEvidence == inspect.EvidenceLive &&
			resolution.Runtime.ID == "builtin://openrealtime/elements/"+elementName &&
			resolution.Runtime.Revision == "implementation:1" &&
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
