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

// An empty piece of retained reasoning is not a turn. The preamble announces
// working state and then shows nothing, which reads as the agent having taken
// a turn and said nothing - five of them appeared between four lines of a
// story, and the voice counted an animal nobody had mentioned.
func TestEmptyInternalStateIsNotATurn(t *testing.T) {
	if continuation.CarriesInternalState("") {
		t.Fatal("nothing was carried as internal state")
	}
	if continuation.CarriesInternalState("   \n ") {
		t.Fatal("whitespace was carried as internal state")
	}
	if !continuation.CarriesInternalState("checked the build log; it is still running") {
		t.Fatal("real working state was dropped")
	}
}

// The interrupted-turn projection is deliberately visible to the model, but
// its runtime annotation is never a sentence for the person. A copied note
// must therefore stop at the reserved marker, including when the model changes
// its casing or leaves the note unfinished.
func TestRuntimeProjectionAnnotationCannotBecomeSpeech(t *testing.T) {
	for _, test := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "complete note",
			in:   "Let's continue. [runtime: playback stopped here. Prepared but never spoken: \"the rest\"]",
			want: "Let's continue.",
		},
		{
			name: "model casing and whitespace",
			in:   "I can help. [RUNTIME:\ncopied control text",
			want: "I can help.",
		},
		{
			name: "unicode before marker",
			in:   "你好，I can help. [RuNtImE: copied control text",
			want: "你好，I can help.",
		},
		{
			name: "marker at start",
			in:   "[runtime: the whole answer was metadata]",
			want: "",
		},
		{
			name: "ordinary text",
			in:   "The runtime is ready.",
			want: "The runtime is ready.",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, found := continuation.StripRuntimeAnnotations(test.in)
			if found != (test.name != "ordinary text") || got != test.want {
				t.Fatalf("StripRuntimeAnnotations(%q) = (%q, %t), want (%q, %t)",
					test.in, got, found, test.want, test.name != "ordinary text")
			}
		})
	}
}
