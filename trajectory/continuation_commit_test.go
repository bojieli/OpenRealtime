package trajectory_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestContinuationCommitDistinguishesSpeechFromNewEvidence(t *testing.T) {
	for _, superseded := range []bool{false, true} {
		t.Run(fmt.Sprintf("superseded=%t", superseded), func(t *testing.T) {
			store := trajectory.NewStore()
			if err := store.AppendBatch([]trajectory.Item{
				{ID: "request", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "Book the appointment."},
				{ID: "voice", Kind: trajectory.KindAssistant, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "One moment.", Visibility: trajectory.VisibilityPrepared},
			}); err != nil {
				t.Fatal(err)
			}
			if superseded {
				if err := store.Append(trajectory.Item{
					ID: "cancel", Kind: trajectory.KindObservation,
					Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "Stop. Do not book anything.",
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := store.Snapshot().Version
			err := store.AppendBatchAfter(1, func(item trajectory.Item) bool {
				return item.Kind == trajectory.KindObservation
			}, []trajectory.Item{{
				ID: "booking", Kind: trajectory.KindToolCall, InvocationID: "booking",
				CausalParentIDs: []string{"request"}, Producer: trajectory.Producer{Phase: trajectory.PhaseSlow},
				ToolCall: &trajectory.ToolCall{CallID: "booking", Name: "book", Arguments: json.RawMessage(`{}`)},
			}})
			snapshot := store.Snapshot()
			if superseded {
				if !errors.Is(err, trajectory.ErrVersionConflict) || snapshot.Version != before {
					t.Fatalf("superseded output changed the trajectory: err=%v snapshot=%+v", err, snapshot)
				}
			} else if err != nil || snapshot.Version != before+1 || snapshot.Items[len(snapshot.Items)-1].ID != "booking" {
				t.Fatalf("unrelated speech prevented the action from committing: err=%v snapshot=%+v", err, snapshot)
			}
		})
	}
}

// An observation and a continuation can reach the store together. Either the
// continuation commits first, or it must refuse the now-stale tool call. Checking
// the suffix and publishing output as separate transactions permits a third,
// invalid ordering: accepted output after the evidence that invalidated it.
func TestContinuationCommitCannotCrossSupersedingObservation(t *testing.T) {
	const attempts = 256
	const writers = 8
	for attempt := range attempts {
		store := trajectory.NewStore()
		if err := store.AppendBatch([]trajectory.Item{
			{ID: "request", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "Book the appointment."},
			{ID: "voice", Kind: trajectory.KindAssistant, Producer: trajectory.Producer{Phase: trajectory.PhaseFast}, Content: "One moment.", Visibility: trajectory.VisibilityPrepared},
		}); err != nil {
			t.Fatal(err)
		}
		checked := make(chan struct{})
		var checking sync.Once
		supersedes := func(item trajectory.Item) bool {
			checking.Do(func() { close(checked) })
			// Let the observation producer compete at the actual commit boundary.
			runtime.Gosched()
			return item.Kind == trajectory.KindObservation
		}
		var workers sync.WaitGroup
		errorsSeen := make(chan error, writers)
		workers.Add(writers)
		for writer := range writers {
			go func() {
				defer workers.Done()
				id := fmt.Sprintf("booking-%d", writer)
				err := store.AppendBatchAfter(1, supersedes, []trajectory.Item{{
					ID: id, Kind: trajectory.KindToolCall, InvocationID: id,
					CausalParentIDs: []string{"request"}, Producer: trajectory.Producer{Phase: trajectory.PhaseSlow},
					ToolCall: &trajectory.ToolCall{CallID: id, Name: "book", Arguments: json.RawMessage(`{}`)},
				}})
				if err != nil && !errors.Is(err, trajectory.ErrVersionConflict) {
					errorsSeen <- err
				}
			}()
		}
		finished := make(chan struct{})
		go func() { workers.Wait(); close(finished) }()
		select {
		case <-checked:
		case <-finished:
			select {
			case <-checked:
			default:
				t.Fatal("continuation commits did not check the newer speech")
			}
		}
		observationErr := store.Append(trajectory.Item{
			ID: "cancel", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "Stop. Do not book anything.",
		})
		<-finished
		if observationErr != nil {
			t.Fatal(observationErr)
		}
		close(errorsSeen)
		for err := range errorsSeen {
			t.Fatal(err)
		}
		canceled := false
		for _, item := range store.Snapshot().Items {
			if item.ID == "cancel" {
				canceled = true
			}
			if canceled && item.Kind == trajectory.KindToolCall {
				t.Fatalf("attempt %d: stale tool call %s committed after the superseding observation", attempt, item.ID)
			}
		}
	}
}
