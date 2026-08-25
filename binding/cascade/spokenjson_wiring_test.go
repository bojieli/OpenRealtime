package cascade_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
)

// Measured on a phone menu: the agent said [{"name":"press_key",...}] out loud.
// The predicate that recognises it is worth testing on its own, and so is the
// wiring - removing the call from the publish path leaves the predicate
// passing and the user still listening to JSON.
func TestAToolCallWrittenAsProseNeverReachesSpeech(t *testing.T) {
	fast := newFast(
		[]continuation.Event{{
			Kind: continuation.EventAssistantDelta,
			Text: `[{"name":"press_key","arguments":{"key":"2"}}]`,
		}},
		[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Pressing two."}},
	)
	slow := newSlow([]continuation.Event{{
		Kind: continuation.EventAssistantDelta, Text: "The menu offered order status on two.",
	}})
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: slow}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(sink.spokenTexts()) >= 1 }, "expected the turn to produce speech")

	for _, text := range sink.spokenTexts() {
		if strings.Contains(text, `"arguments"`) || strings.Contains(text, `"name"`) {
			t.Fatalf("a tool call written as prose was spoken: %q", text)
		}
	}
}
