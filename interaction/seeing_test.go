package interaction_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

// A frame is evidence in its own right for a model that can look at one.
// Requiring a description first is what put a cloud round trip on the critical
// path of every visual turn - 1.45 seconds measured, the largest single cost
// in this system - and what the sentence left out was gone.
func TestAFrameIsEvidenceWithoutBeingDescribedFirst(t *testing.T) {
	blind := interaction.Situation{Silence: "2s"}
	if blind.Decidable() {
		t.Fatal("nothing at all must decide nothing")
	}
	described := interaction.Situation{Seen: "the build finished", Silence: "2s"}
	if !described.Decidable() {
		t.Fatal("a description is evidence, which is how this worked before")
	}
	seeing := interaction.Situation{
		Seeing:  []interaction.Image{{MIMEType: "image/png", Bytes: []byte("a frame")}},
		Silence: "2s",
	}
	if !seeing.Decidable() {
		t.Fatal("a frame the model can look at is evidence without a narrator in between")
	}
	// And the rendering says nothing about it: the picture goes to the model
	// as a picture, so a line claiming to describe it would be a second,
	// worse account of the same thing.
	if got := seeing.Render(); len(got) == 0 {
		t.Fatal("a situation with a frame still renders its text")
	}
}
