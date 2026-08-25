package cascade_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// describingNarrator says what it was shown.
type describingNarrator struct{ text string }

func (describingNarrator) Name() string { return "describing" }

func (narrator describingNarrator) Narrate(
	_ context.Context, frames []perception.Frame, _ trajectory.Snapshot,
) (string, error) {
	if len(frames) == 0 {
		return "", nil
	}
	return narrator.text, nil
}

// A client attaching a picture is the ordinary Realtime way to show the agent
// a screen, and on that path the picture reached the one provider that can
// see and nothing else. Every text layer - the interaction model deciding
// whether this is a moment worth speaking at, most of all - was handed "The
// user attached an image" and asked to judge from it.
func TestAnAttachedPictureIsDescribedForEverythingThatCannotSee(t *testing.T) {
	fast := newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Right."}})
	runtime, _ := startSession(t, cascade.Config{
		Fast: fast, Slow: newSlow(),
		Narrator: describingNarrator{text: "a terminal, the build at 41 percent"},
	}, binding.Settings{})

	if err := runtime.Text(t.Context(), binding.TextInput{
		ItemID: "item-1", Role: "user",
		Images: []binding.Image{{MIMEType: "image/png", Bytes: []byte("not really a png"), Width: 8, Height: 8}},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if strings.Contains(item.Content, "build at 41 percent") {
				return true
			}
		}
		return false
	}, "the picture never became text")

	// And the handle is still on it, so a provider that can see still sees it.
	for _, item := range runtime.Trajectory().Items {
		if !strings.Contains(item.Content, "build at 41 percent") {
			continue
		}
		if item.Observation == nil || len(item.Observation.Media) == 0 {
			t.Fatal("describing the picture dropped the picture")
		}
		return
	}
}

// The description is a narrator's account of a terminal window, not something
// the person asked for, and the pass that lifts standing policies out of what
// people say must not read it as one. Measured, one screen description in five
// revoked the policy the conversation was running on.
func TestADescribedPictureIsNotReadForStandingPolicies(t *testing.T) {
	extractor := &countingExtractor{}
	policies := interaction.Defaults()
	policies.Extraction = extractor
	runtime, _ := startSession(t, cascade.Config{
		Fast: newFast(), Slow: newSlow(), Policies: policies,
		Narrator: describingNarrator{text: "a terminal, the build at 41 percent"},
	}, binding.Settings{})

	if err := runtime.Text(t.Context(), binding.TextInput{
		ItemID: "item-1", Role: "user",
		Images: []binding.Image{{MIMEType: "image/png", Bytes: []byte("not really a png"), Width: 8, Height: 8}},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if strings.Contains(item.Content, "build at 41 percent") {
				return true
			}
		}
		return false
	}, "the picture never became text")
	if seen := extractor.seen(); len(seen) != 0 {
		t.Fatalf("a screen description was read for standing policies: %q", seen)
	}
}

// countingExtractor records every utterance the standing pass was given.
type countingExtractor struct {
	mu         sync.Mutex
	utterances []string
}

func (extractor *countingExtractor) Name() string { return "counting" }

func (extractor *countingExtractor) Extract(
	_ context.Context, _ []interaction.StandingInstruction, _ []string,
	utterance string, _ bool,
) (interaction.Extraction, error) {
	extractor.mu.Lock()
	defer extractor.mu.Unlock()
	extractor.utterances = append(extractor.utterances, utterance)
	return interaction.Extraction{Kind: "none"}, nil
}

func (extractor *countingExtractor) seen() []string {
	extractor.mu.Lock()
	defer extractor.mu.Unlock()
	return append([]string(nil), extractor.utterances...)
}

// With no vision model configured there is nothing to describe with, and the
// line saying a picture arrived is the honest answer rather than a failure.
func TestAnAttachedPictureStillArrivesWithoutANarrator(t *testing.T) {
	runtime, _ := startSession(t, cascade.Config{
		Fast: newFast(), Slow: newSlow(),
	}, binding.Settings{})
	if err := runtime.Text(t.Context(), binding.TextInput{
		ItemID: "item-1", Role: "user",
		Images: []binding.Image{{MIMEType: "image/png", Bytes: []byte("not really a png"), Width: 8, Height: 8}},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if strings.Contains(item.Content, "attached an image") {
				return true
			}
		}
		return false
	}, "a picture with no narrator produced no observation at all")
}
