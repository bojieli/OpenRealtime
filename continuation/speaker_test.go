package continuation

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// One microphone carries everybody, and a third party's speech arrives with
// the same authority as the user's own. Rendered bare it tells the model the
// user said it, which is a false premise a correct answer cannot survive:
// asked to interpret for a colleague, the voice read the colleague's Mandarin
// as the user's own words.
func TestAThirdPartyIsNamedAndTheUserIsNot(t *testing.T) {
	spoke := func(source, text string) trajectory.Item {
		return trajectory.Item{
			Kind: trajectory.KindObservation, Content: text,
			Observation: &trajectory.ObservationMeta{Source: source},
		}
	}
	for _, source := range []string{"", "microphone", "voice", "text", "user"} {
		got := ObservationContent(spoke(source, "book me a table"), "")
		if got != "book me a table" {
			t.Fatalf("the user's own speech was labelled from source %q: %q", source, got)
		}
	}
	got := ObservationContent(spoke("someone else in the room", "你好"), "")
	if !strings.HasPrefix(got, "someone else in the room: ") {
		t.Fatalf("a third party was not named: %q", got)
	}
	if !strings.HasSuffix(got, "你好") {
		t.Fatalf("what they said was lost: %q", got)
	}
	// The elapsed prefix still leads, because when it was said is read before
	// who said it.
	timed := ObservationContent(spoke("someone else in the room", "你好"), "[2s later]")
	if !strings.HasPrefix(timed, "[2s later] someone else in the room: ") {
		t.Fatalf("the timing and the speaker did not compose: %q", timed)
	}
}
