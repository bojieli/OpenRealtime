package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/spoken"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// Retained native state is the whole turn, unheard words included. A partly
// heard turn therefore falls back to the portable projection, exactly as a
// cancelled one does.
func TestRetainedNativeStateNeverRestoresWordsTheUserNeverHeard(t *testing.T) {
	adapter, err := New(Config{Model: "claude-test", APIKey: "k", Phase: trajectory.PhaseFast})
	if err != nil {
		t.Fatal(err)
	}
	native, err := json.Marshal(retainedState{
		Model:   "claude-test",
		Content: []json.RawMessage{json.RawMessage(`{"type":"text","text":"one two three four five"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := adapter.buildRequest(continuation.Request{
		Descriptor: adapter.Descriptor(), InvocationID: "next",
		Trajectory: trajectory.Snapshot{Items: []trajectory.Item{
			{ID: "user-1", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "count for me"},
			{ID: "fast", Kind: trajectory.KindAssistant, InvocationID: "inv-counting",
				Producer:          trajectory.Producer{Phase: trajectory.PhaseFast, Provider: adapter.Descriptor().Provider, Model: "claude-test"},
				Content:           "one two three four five",
				ProviderStateType: ProviderStateType, ProviderState: native},
			{ID: "played", Kind: trajectory.KindAssistantState, Producer: trajectory.Producer{Phase: trajectory.PhaseRuntime},
				AssistantState: &trajectory.AssistantState{
					AssistantItemID: "fast", Visibility: trajectory.VisibilityPlayed, PlayedAudioMS: 900,
					Heard: &spoken.Mark{Spoken: "one two", Cut: "three", Pending: "three four five", Measured: true},
				}},
			{ID: "user-2", Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hold on"},
		}},
		Invocation: continuation.Invocation{Instruction: "Continue from what was actually heard."},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(body)
	if strings.Contains(string(encoded), "one two three four five") {
		t.Fatalf("the retained native turn put the unheard words back: %s", encoded)
	}
	if !strings.Contains(string(encoded), "one two") || !strings.Contains(string(encoded), "three four five") {
		t.Fatalf("heard and prepared halves must both survive: %s", encoded)
	}
}
