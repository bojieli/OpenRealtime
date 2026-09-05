package scenarioconversation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func bindPlaybackStateLoopback(t *testing.T, session *session) {
	t.Helper()
	if session.sessionID == "" {
		session.sessionID = "session-playback"
	}
	session.pendingOps = make(map[string]*pendingOperation)
	session.snapshotChanged = make(chan struct{})
	session.ports.playbackStateAppend = mediaTestOutput{typeOf: stateelements.AppendType(), broadcast: func(_ context.Context, request element.Envelope) (element.SendResult, error) {
		publication, reply := applyPlaybackStateRequest(t, session, request)
		if err := session.acceptTrajectorySnapshot(publication); err != nil {
			return element.SendResult{}, err
		}
		if err := session.acceptPlaybackStateReply(trajectoryCommitBoundary, reply); err != nil {
			return element.SendResult{}, err
		}
		return element.SendResult{Delivered: 1}, nil
	}}
}

func applyPlaybackStateRequest(t *testing.T, session *session, request element.Envelope) (element.Envelope, element.Envelope) {
	t.Helper()
	append := request.Payload.(stateelements.Append)
	if append.Compare || append.Prefix == nil || append.ExpectedVersion != append.Prefix.Version {
		t.Fatal("playback append lost its original prefix")
	}
	if err := session.bundle.store.AppendBatchOnPrefix(*append.Prefix, append.Items); err != nil {
		t.Fatal(err)
	}
	snapshot, prefix, err := session.bundle.store.SnapshotWithPrefixIdentity()
	if err != nil {
		t.Fatal(err)
	}
	context := stateelements.CommittedContext{Prefix: prefix, StateItemID: request.ItemID + ":snapshot"}
	publication := request.Clone()
	publication.Type, publication.ItemID, publication.Payload = stateelements.SnapshotType(), context.StateItemID, snapshot
	publication.CausalParents = appendSliceParent(publication.CausalParents, request.ItemID)
	ids := make([]string, len(append.Items))
	for i := range ids {
		ids[i] = append.Items[i].ID
	}
	reply := request.Clone()
	reply.Type, reply.ItemID = stateelements.CommitType(), request.ItemID+":committed"
	reply.CausalParents = appendSliceParent(reply.CausalParents, request.ItemID, context.StateItemID)
	reply.Payload = stateelements.Commit{Version: snapshot.Version, Snapshot: snapshot, Context: context, AppendedIDs: ids}
	return publication, reply
}

func appendSliceParent(original []string, parents ...string) []string {
	return append(append([]string(nil), original...), parents...)
}

func playbackStateFixture(t *testing.T) (*session, chan element.Envelope, trajectory.Snapshot, []trajectory.Item) {
	t.Helper()
	store := trajectory.NewStore()
	if err := store.Append(trajectory.Item{ID: "assistant", Kind: trajectory.KindAssistant, MonotonicNS: 10, InvocationID: "run", Content: "One two three.", Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Visibility: trajectory.VisibilityPrepared}); err != nil {
		t.Fatal(err)
	}
	session := &session{sessionID: "playback-session", bundle: &sessionBundle{store: store}, pendingOps: make(map[string]*pendingOperation), snapshotChanged: make(chan struct{})}
	requests := make(chan element.Envelope, 1)
	session.ports.playbackStateAppend = mediaTestOutput{typeOf: stateelements.AppendType(), broadcast: func(ctx context.Context, request element.Envelope) (element.SendResult, error) {
		select {
		case requests <- request:
			return element.SendResult{Delivered: 1}, nil
		case <-ctx.Done():
			return element.SendResult{}, context.Cause(ctx)
		}
	}}
	items := []trajectory.Item{{ID: "played", Kind: trajectory.KindAssistantState, MonotonicNS: 10, InvocationID: "run", CausalParentIDs: []string{"assistant"}, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime}, AssistantState: &trajectory.AssistantState{AssistantItemID: "assistant", Visibility: trajectory.VisibilityPlayed, PlayedAudioMS: 250, Heard: &spoken.Mark{Spoken: "One", Pending: "two three.", Cut: "two", Measured: true, PlayedMS: 250}}}}
	return session, requests, store.Snapshot(), items
}

func TestPlaybackStateWaitsForCommitAndPublicationAcrossNewerObservation(t *testing.T) {
	for _, commitFirst := range []bool{false, true} {
		t.Run(map[bool]string{true: "commit before publication", false: "publication before commit"}[commitFirst], func(t *testing.T) {
			session, requests, basis, items := playbackStateFixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- session.commitPlaybackState(ctx, "run", basis, items) }()
			request := <-requests
			if err := session.bundle.store.Append(trajectory.Item{ID: "later", Kind: trajectory.KindObservation, MonotonicNS: 1 << 60, Content: "newer", Producer: trajectory.Producer{Phase: trajectory.PhaseUser}}); err != nil {
				t.Fatal(err)
			}
			publication, reply := applyPlaybackStateRequest(t, session, request)
			commit := func() {
				t.Helper()
				if err := session.acceptPlaybackStateReply(trajectoryCommitBoundary, reply); err != nil {
					t.Fatal(err)
				}
			}
			publish := func() {
				t.Helper()
				if err := session.acceptTrajectorySnapshot(publication); err != nil {
					t.Fatal(err)
				}
			}
			if commitFirst {
				commit()
			} else {
				publish()
			}
			select {
			case err := <-done:
				t.Fatalf("playback released before both boundaries: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if commitFirst {
				publish()
			} else {
				commit()
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			state := session.bundle.store.Snapshot().Items[2]
			if state.MonotonicNS != 1<<60 || state.AssistantState.Heard.Spoken != "One" || state.AssistantState.Heard.Pending != "two three." || items[0].MonotonicNS != 10 {
				t.Fatal("playback state or caller timestamp changed")
			}
			if len(session.pendingOps) != 0 {
				t.Fatal("completed playback operation retained")
			}
			if err := session.acceptPlaybackStateReply(trajectoryCommitBoundary, reply); err != nil {
				t.Fatal(err)
			}
			if session.bundle.store.Snapshot().Version != 3 {
				t.Fatal("duplicate receipt appended state")
			}
		})
	}
}

func TestPlaybackStateRefusesAlteredOrUnrelatedCommit(t *testing.T) {
	for _, mode := range []string{"session", "run", "request parent", "reply ID", "appended IDs", "source prefix", "heard words", "timestamp", "published prefix", "published identity", "durable history", "wrong payload", "unrelated"} {
		t.Run(mode, func(t *testing.T) {
			session, requests, basis, items := playbackStateFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- session.commitPlaybackState(ctx, "run", basis, items) }()
			request := <-requests
			_, reply := applyPlaybackStateRequest(t, session, request)
			commit := reply.Payload.(stateelements.Commit)
			switch mode {
			case "session":
				reply.SessionID = "other"
			case "run":
				reply.RunID = "other"
			case "request parent":
				reply.CausalParents = nil
			case "reply ID":
				reply.ItemID = "other:committed"
			case "appended IDs":
				commit.AppendedIDs = []string{"other"}
			case "source prefix":
				commit.Snapshot.Items[0].Content = "forged"
			case "heard words":
				commit.Snapshot.Items[1].AssistantState.Heard.Spoken = "One two three."
			case "timestamp":
				commit.Snapshot.Items[1].MonotonicNS++
			case "published prefix":
				commit.Context.Prefix.Digest = "sha256:" + strings.Repeat("0", 64)
			case "published identity":
				commit.Context.StateItemID = "other-state"
			case "durable history":
				session.bundle.store = trajectory.NewStore()
			case "unrelated":
				reply.ItemID = "other:committed"
				reply.CausalParents = nil
			}
			reply.Payload = commit
			if mode == "wrong payload" {
				reply.Payload = "wrong"
			}
			err := session.acceptPlaybackStateReply(trajectoryCommitBoundary, reply)
			if mode == "unrelated" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("altered commit accepted")
			}
			select {
			case err := <-done:
				t.Fatalf("invalid receipt released playback: %v", err)
			default:
			}
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled wait: %v", err)
			}
		})
	}
}

func TestPlaybackStateRejectionAndCanceledWaitRetireExactOperation(t *testing.T) {
	for _, abandon := range []bool{false, true} {
		t.Run(map[bool]string{true: "canceled", false: "waiting"}[abandon], func(t *testing.T) {
			session, requests, basis, items := playbackStateFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- session.commitPlaybackState(ctx, "run", basis, items) }()
			request := <-requests
			if abandon {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			reply := request.Clone()
			reply.ItemID = request.ItemID + ":rejected"
			reply.CausalParents = appendSliceParent(reply.CausalParents, request.ItemID)
			reply.Payload = stateelements.Rejection{ExpectedVersion: basis.Version, CurrentVersion: basis.Version, Code: "invalid_batch", Message: "retained refusal"}
			if err := session.acceptPlaybackStateReply(trajectoryRejectionBoundary, reply); err != nil {
				t.Fatal(err)
			}
			if !abandon {
				if err := <-done; err == nil || !strings.Contains(err.Error(), "retained refusal") {
					t.Fatal(err)
				}
			}
			if len(session.pendingOps) != 0 {
				t.Fatal("rejected playback operation retained")
			}
			if session.bundle.store.Snapshot().Version != basis.Version {
				t.Fatal("rejection invented history")
			}
		})
	}
}
