package cascade_test

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
)

// A runtime projection note can be copied by a fast provider after an
// interrupted turn. It is context for cognition, never something the person
// should hear. This exercises the complete continuation-to-speech wiring, not
// only the provider-neutral text sanitizer.
func TestCopiedRuntimeProjectionNeverReachesSpeech(t *testing.T) {
	fast := newFast([]continuation.Event{{
		Kind: continuation.EventAssistantDelta,
		Text: "The answer is ready. [runtime: playback stopped here; prepared but never spoken: \"the rest\"]",
	}})
	runtime, sink := startSession(t, cascade.Config{Fast: fast, Slow: newSlow()}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(sink.spokenTexts()) > 0 }, "the sanitized answer was never voiced")

	spoken := strings.Join(sink.spokenTexts(), " ")
	if strings.Contains(strings.ToLower(spoken), "[runtime:") || strings.Contains(spoken, "prepared but never spoken") {
		t.Fatalf("runtime projection metadata reached speech: %q", spoken)
	}
	if !strings.Contains(spoken, "The answer is ready.") {
		t.Fatalf("ordinary text before the runtime note was lost: %q", spoken)
	}
}
