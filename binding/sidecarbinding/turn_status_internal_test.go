package sidecarbinding

import (
	"context"
	"testing"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/sidecar"
)

type outcomeSink struct {
	binding.Sink
	outcomes []action.Outcome
}

func (s *outcomeSink) SpeechEnd(_ context.Context, _ action.Utterance, outcome action.Outcome) error {
	s.outcomes = append(s.outcomes, outcome)
	return nil
}

// A model that stops because the user took the floor did not complete its
// turn; clients see that as a cancelled response and can stop playback.
func TestInterruptedTurnIsNotReportedComplete(t *testing.T) {
	for _, check := range []struct {
		status    string
		completed bool
	}{{"", true}, {sidecar.TurnInterrupted, false}} {
		sink := &outcomeSink{}
		r := &runtime{ctx: context.Background(), sink: sink, utterance: &action.Utterance{ID: "utt_1"}}
		message := sidecar.Message{Type: sidecar.TypeTurnDone, TurnStatus: check.status}
		if err := message.Validate(); err != nil {
			t.Fatalf("status %q: %v", check.status, err)
		}
		if err := r.mirrorMessage(message); err != nil {
			t.Fatal(err)
		}
		if len(sink.outcomes) != 1 || sink.outcomes[0].Completed != check.completed {
			t.Fatalf("status %q: outcomes %+v, want completed=%v", check.status, sink.outcomes, check.completed)
		}
	}
	if err := (sidecar.Message{Type: sidecar.TypeTurnDone, TurnStatus: "halfway"}).Validate(); err == nil {
		t.Fatal("an unknown turn status was accepted")
	}
}
