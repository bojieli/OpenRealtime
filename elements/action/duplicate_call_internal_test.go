package action

import (
	"encoding/json"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// The same key pressed twice for one menu is not two decisions; the same key
// pressed on the next call the person makes is.
func TestDuplicateCallInUtterance(t *testing.T) {
	call := func(id, digit string) trajectory.Item {
		return trajectory.Item{ID: id, Kind: trajectory.KindToolCall,
			ToolCall: &trajectory.ToolCall{CallID: id, Name: "press_key", Arguments: json.RawMessage(`{"digit":"` + digit + `"}`)}}
	}
	observation := func(id, kind string) trajectory.Item {
		return trajectory.Item{ID: id, Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
			Observation: &trajectory.ObservationMeta{Authority: trajectory.AuthorityUser},
			Event:       &trajectory.EventMetadata{EventID: "e-" + id, Type: kind}}
	}
	press := trajectory.ToolCall{CallID: "new", Name: "press_key", Arguments: json.RawMessage(`{"digit": "2"}`)}
	sameUtterance := []trajectory.Item{
		observation("menu-1", "asr.revision"), call("first", "2"), observation("menu-2", "asr.revision"),
	}
	if earlier, duplicate := duplicateCallInUtterance(sameUtterance, "menu-2", press); !duplicate || earlier.ID != "first" {
		t.Fatalf("a second identical press in the same utterance was not a duplicate: %+v %v", earlier, duplicate)
	}
	if _, duplicate := duplicateCallInUtterance(sameUtterance, "menu-2", trajectory.ToolCall{Name: "press_key", Arguments: json.RawMessage(`{"digit":"3"}`)}); duplicate {
		t.Fatal("a different key is not a duplicate")
	}
	nextTurn := []trajectory.Item{
		observation("menu-1", "asr.revision"), call("first", "2"), observation("menu-final", "asr.endpoint"),
		observation("again", "asr.revision"),
	}
	if _, duplicate := duplicateCallInUtterance(nextTurn, "again", press); duplicate {
		t.Fatal("the same key on the next turn is a new decision")
	}
	settled := []trajectory.Item{observation("request", "input_text.endpoint"), call("first", "2"), observation("frame", "vision.endpoint")}
	if _, duplicate := duplicateCallInUtterance(settled, "frame", press); duplicate {
		t.Fatal("a call proposed on settled evidence is a decision of its own")
	}
}
