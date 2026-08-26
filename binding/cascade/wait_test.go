package cascade_test

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/cognition"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
)

// An empty answer works and reads exactly like a broken one. That ambiguity
// cost more time than any other single thing here: an interjection returning
// "" looked identical to a turn that was never asked for, and absence got read
// as evidence for hours.
//
// The token separates them. A decision to be silent is the system working; a
// turn that produced nothing is worth looking at.
func TestTheVoiceCanSayNothingOnPurpose(t *testing.T) {
	fast := newFast([]continuation.Event{
		{Kind: continuation.EventAssistantDelta, Text: cognition.WaitToken},
	})
	runtime, sink := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			return &scriptedASR{final: "it was a warm afternoon by the river"}, nil
		},
		Fast: fast, Slow: newSlow(), Speech: toneSpeech{chunks: 4},
	}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return fast.invocations() > 0 }, "the voice was never asked")
	time.Sleep(250 * time.Millisecond)

	for _, spoken := range sink.spokenTexts() {
		if strings.Contains(spoken, cognition.WaitToken) {
			t.Fatalf("the token reached the world as speech: %q", spoken)
		}
	}
	sink.mu.Lock()
	frames := sink.frames
	sink.mu.Unlock()
	if frames != 0 {
		t.Fatalf("a deliberate silence produced %d frames of audio", frames)
	}
	// The trajectory records the decision, which is honest - but the
	// conversation window must not render it as a line the agent said, or the
	// next turn reads back that the agent says "<wait>" out loud.
	for _, line := range interaction.RecentLines(runtime.Trajectory().Items, 8) {
		if strings.Contains(line, cognition.WaitToken) {
			t.Fatalf("a deliberate silence was rendered as conversation: %q", line)
		}
	}
}
