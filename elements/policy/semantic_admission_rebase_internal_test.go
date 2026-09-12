package policy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A held decision is taken against newer context than its commit named, and
// the grant is rebased onto it. The rebased commit has to say what the tail of
// that context is: the tool candidate authorised from it names the tail, the
// model's result names the tail of the snapshot it ran on, and the provenance
// join refuses a call whose two sides disagree. Measured, a key press at a
// phone menu was refused for exactly that after the decision had waited for
// the voice to finish.
func TestRebasedSemanticCommitNamesTheNewerTail(t *testing.T) {
	item := func(id string) trajectory.Item {
		return trajectory.Item{ID: id, Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: id}
	}
	request := semanticRequest{
		operation: "committed", version: 2,
		commit: stateelements.ObservationCommitOutcome{
			TrajectoryItemID: "menu-partial-1", StoreVersion: 2,
			Context: stateelements.CommittedContext{StateItemID: "state-2"},
		},
	}
	newer := trajectory.Snapshot{Version: 4, Items: []trajectory.Item{item("call"), item("menu-partial-1"), item("agent-said"), item("menu-partial-2")}}
	rebased := rebasedSemanticCommit(request, semanticContextSample{
		envelope: element.Envelope{ItemID: "state-4"}, snapshot: newer,
	})
	if rebased.StoreVersion != 4 || rebased.Context.StateItemID != "state-4" ||
		rebased.TrajectoryItemID != "menu-partial-1" || rebased.TailItemID() != "menu-partial-2" {
		t.Fatalf("rebased commit = %+v, want version 4 with tail menu-partial-2 and the same observation", rebased)
	}
	same := trajectory.Snapshot{Version: 3, Items: []trajectory.Item{item("call"), item("menu-partial-1")}}
	kept := rebasedSemanticCommit(request, semanticContextSample{
		envelope: element.Envelope{ItemID: "state-3"}, snapshot: same,
	})
	if kept.Context.TailItemID != "" || kept.TailItemID() != "menu-partial-1" {
		t.Fatalf("a rebase whose tail is the observation itself must not name another tail: %+v", kept)
	}
	if untouched := rebasedSemanticCommit(request, semanticContextSample{}); untouched.TailItemID() != "menu-partial-1" || untouched.StoreVersion != 2 {
		t.Fatalf("no newer context leaves the commit alone: %+v", untouched)
	}
}

// A key press is not audible. The step that invoked the voice records the
// call the trajectory shows for it, so the next step sees the occurrence as
// answered rather than asking for the key again.
func TestStepHistoryRecordsTheToolCallTheVoiceMade(t *testing.T) {
	runner := &semanticAdmissionRunner{}
	runner.steps = []semanticStep{
		{stream: "menu", event: coreinteraction.TranscriptPartial, heard: "Press one for billing", choice: coreinteraction.Choice{}},
		{stream: "menu", event: coreinteraction.TranscriptPartial, heard: "Press one for billing. Press two for order status", choice: coreinteraction.Choice{Speak: true}, spoke: true},
	}
	call := trajectory.ToolCall{CallID: "c1", Name: "press_key", Arguments: json.RawMessage(`{"digit":"2"}`)}
	before := semanticContextSample{envelope: element.Envelope{ItemID: "state-3"}, snapshot: trajectory.Snapshot{Version: 3, Items: []trajectory.Item{
		{ID: "a"}, {ID: "b"}, {ID: "c"},
	}}}
	after := semanticContextSample{envelope: element.Envelope{ItemID: "state-5"}, snapshot: trajectory.Snapshot{Version: 5, Items: []trajectory.Item{
		{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "proposal", Kind: trajectory.KindToolProposal, ToolCall: &call},
		{ID: "call", Kind: trajectory.KindToolCall, ToolCall: &call},
	}}}
	runner.noteAgentActions(before, after)
	runner.noteAgentActions(after, after)
	lines := runner.stepLines()
	if len(lines) != 2 || !strings.Contains(lines[1], `agent did press_key({"digit":"2"}) and said nothing`) || strings.Contains(lines[0], "did") {
		t.Fatalf("step lines = %q", lines)
	}
	if len(runner.steps[1].said) != 1 {
		t.Fatalf("the call was recorded %d times", len(runner.steps[1].said))
	}
}
