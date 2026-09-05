package scenarioconversation

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// playbackStateAppend records a performed speech effect, not fresh model or
// action authority. The exact original prefix remains its historical basis.
type playbackStateAppend struct {
	prefix trajectory.PrefixIdentity
	items  []trajectory.Item
}

func (session *session) commitPlaybackState(ctx context.Context, runID string, snapshot trajectory.Snapshot, items []trajectory.Item) error {
	if session.ports.playbackStateAppend == nil {
		return errors.New("playback state has no graph append boundary")
	}
	prefix, err := trajectory.IdentifyPrefix(snapshot, snapshot.Version)
	if err != nil {
		return err
	}
	itemID, sequence := session.nextEnvelopeIdentity("playback-state")
	pending := &pendingOperation{
		operation: "playback_state", generation: runID, result: make(chan operationAck, 1),
		playback: &playbackStateAppend{prefix: prefix, items: clonePlaybackStateItems(items)},
	}
	if err := session.registerOperation(itemID, pending); err != nil {
		return err
	}
	parents := make([]string, 0, len(items))
	for _, item := range items {
		parents = append(parents, item.CausalParentIDs...)
	}
	if err := sendExact(ctx, session.ports.playbackStateAppend, element.Envelope{
		Type: stateelements.AppendType(), ItemID: itemID, SessionID: session.sessionID,
		RunID: runID, SourceID: "gateway", Sequence: sequence, TraceID: itemID,
		CancellationScope: runID, CausalParents: parents,
		Payload: stateelements.Append{ExpectedVersion: prefix.Version, Prefix: &prefix, Items: clonePlaybackStateItems(items)},
	}, "append scenario conversation playback state"); err != nil {
		session.removeOperation(itemID, pending)
		return err
	}
	if _, err := session.awaitOperation(ctx, itemID, pending); err != nil {
		return err
	}
	// The store publishes on a separate lane before returning its receipt. Wait
	// for that lane too before exposing TurnEnd to the next client request.
	_, err = session.responseCreateContext(ctx)
	return err
}

func clonePlaybackStateItems(items []trajectory.Item) []trajectory.Item {
	copy := slices.Clone(items)
	for i := range copy {
		copy[i].CausalParentIDs = slices.Clone(items[i].CausalParentIDs)
		if items[i].AssistantState != nil {
			state := *items[i].AssistantState
			if state.Heard != nil {
				heard := *state.Heard
				state.Heard = &heard
			}
			copy[i].AssistantState = &state
		}
	}
	return copy
}

// All graph transactions share these receipt lanes. Only this adapter's exact
// pending playback request can acknowledge playback; other transactions are
// drained without acquiring that authority.
func (session *session) acceptPlaybackStateReply(boundary string, envelope element.Envelope) error {
	session.operationMu.Lock()
	defer session.operationMu.Unlock()
	candidates := slices.Clone(envelope.CausalParents)
	suffix := ":committed"
	if boundary == trajectoryRejectionBoundary {
		suffix = ":rejected"
	}
	if strings.HasSuffix(envelope.ItemID, suffix) {
		candidates = append(candidates, strings.TrimSuffix(envelope.ItemID, suffix))
	}
	var pending *pendingOperation
	for _, id := range candidates {
		candidate := session.pendingOps[id]
		if candidate == nil || candidate.operation != "playback_state" {
			continue
		}
		if pending != nil && pending != candidate {
			return errors.New("playback state reply names multiple pending requests")
		}
		pending = candidate
	}
	if pending == nil {
		return nil
	}
	if envelope.SessionID != session.sessionID || envelope.RunID != pending.generation ||
		envelope.CancellationScope != pending.generation || envelope.ItemID != pending.requestID+suffix ||
		!slices.Contains(envelope.CausalParents, pending.requestID) {
		return errors.New("playback state reply drifted from its exact session, run, or request")
	}
	var result operationAck
	switch boundary {
	case trajectoryCommitBoundary:
		commit, ok := envelope.Payload.(stateelements.Commit)
		if pointer, yes := envelope.Payload.(*stateelements.Commit); yes && pointer != nil {
			commit, ok = *pointer, true
		}
		if !ok {
			return fmt.Errorf("playback state commit has payload %T", envelope.Payload)
		}
		if err := session.attestPlaybackStateCommit(envelope, commit, pending.playback); err != nil {
			return err
		}
	case trajectoryRejectionBoundary:
		rejection, ok := envelope.Payload.(stateelements.Rejection)
		if pointer, yes := envelope.Payload.(*stateelements.Rejection); yes && pointer != nil {
			rejection, ok = *pointer, true
		}
		if !ok || rejection.ExpectedVersion != pending.playback.prefix.Version || rejection.Code == "" {
			return errors.New("playback state rejection lacks its exact original basis")
		}
		result.err = fmt.Errorf("playback state append rejected: %s: %s", rejection.Code, rejection.Message)
	default:
		return fmt.Errorf("unsupported playback state reply boundary %q", boundary)
	}
	pending.completed = true
	delete(session.pendingOps, pending.requestID)
	if !pending.abandoned {
		pending.result <- result
	}
	return nil
}

func (session *session) attestPlaybackStateCommit(envelope element.Envelope, commit stateelements.Commit, pending *playbackStateAppend) error {
	if pending == nil || len(pending.items) == 0 || commit.Version != commit.Snapshot.Version ||
		commit.Version != uint64(len(commit.Snapshot.Items)) || commit.Version < uint64(len(pending.items)) ||
		commit.Version-uint64(len(pending.items)) < pending.prefix.Version {
		return errors.New("playback state commit has inconsistent version or item population")
	}
	if err := trajectory.VerifyPrefix(commit.Snapshot, pending.prefix); err != nil {
		return fmt.Errorf("playback state original prefix: %w", err)
	}
	if commit.Context.Prefix.Version != commit.Version || !canonicalIdentity(commit.Context.StateItemID) ||
		!slices.Contains(envelope.CausalParents, commit.Context.StateItemID) {
		return errors.New("playback state commit lacks its published context identity")
	}
	if err := trajectory.VerifyPrefix(commit.Snapshot, commit.Context.Prefix); err != nil {
		return err
	}
	if err := trajectory.VerifyPrefix(session.bundle.store.Snapshot(), commit.Context.Prefix); err != nil {
		return fmt.Errorf("playback state commit differs from durable history: %w", err)
	}
	start := len(commit.Snapshot.Items) - len(pending.items)
	expected := clonePlaybackStateItems(pending.items)
	boundary := uint64(0)
	if start > 0 {
		boundary = commit.Snapshot.Items[start-1].MonotonicNS
	}
	ids := make([]string, len(expected))
	for i := range expected {
		expected[i].MonotonicNS = max(expected[i].MonotonicNS, boundary)
		boundary = expected[i].MonotonicNS
		ids[i] = expected[i].ID
	}
	if !slices.Equal(commit.AppendedIDs, ids) || !reflect.DeepEqual(commit.Snapshot.Items[start:], expected) {
		return errors.New("playback state commit changed the exact appended items")
	}
	return nil
}
