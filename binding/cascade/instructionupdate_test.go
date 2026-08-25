package cascade_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/perception"
)

// An instruction arrives after the session exists - every client sends it in
// session.update, including the official ones - and the phase prompts were
// composed once at construction from whatever was known then. The voice never
// saw it, while the interaction model did, because that reads the settings on
// every decision. Measured: told a deadline was the third, the decision layer
// correctly cut in to correct a wrong date and the voice invented one.
func TestASessionInstructionReachesTheVoice(t *testing.T) {
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Right."}})
	slow := newSlow([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Noted."}})
	runtime, _ := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{})

	if err := runtime.Update(t.Context(), binding.Settings{
		Gate:        perception.DefaultGateConfig(),
		Instruction: "The deadline the client gave is the third of the month.",
	}); err != nil {
		t.Fatal(err)
	}
	speak(t, runtime, 3)
	waitFor(t, func() bool { return fast.invocations() > 0 }, "expected the voice to be invoked")

	fast.mu.Lock()
	defer fast.mu.Unlock()
	for _, request := range fast.requests {
		if strings.Contains(request.Invocation.Instruction, "third of the month") {
			return
		}
	}
	t.Fatal("the session instruction never reached the voice")
}
