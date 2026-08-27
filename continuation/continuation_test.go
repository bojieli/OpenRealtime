package continuation_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// A wait is not a result. The hint that carries background state tells the
// voice a result has arrived and to answer from it in its own words, so four
// consecutive waits reached the voice as four instructions to say something -
// which is where a count that arrived before the first animal came from.
func TestAWaitIsNotABackgroundResult(t *testing.T) {
	silent := trajectory.Item{
		Kind:     trajectory.KindAssistant,
		Content:  continuation.WaitToken,
		Producer: trajectory.Producer{SpeechAuthority: string(continuation.SpeechAuthoritySilent)},
	}
	if continuation.CarriesBackgroundResult(silent) {
		t.Fatal("a wait was carried as background state")
	}
	found := silent
	found.Content = "The build finished at 14:02."
	if !continuation.CarriesBackgroundResult(found) {
		t.Fatal("a real result was dropped")
	}
	spoken := found
	spoken.Producer.SpeechAuthority = "voice"
	if continuation.CarriesBackgroundResult(spoken) {
		t.Fatal("something the user heard is not background state")
	}
}
