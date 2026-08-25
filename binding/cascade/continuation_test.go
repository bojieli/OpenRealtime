package cascade_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/binding/cascade"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/interaction"
)

// pinningExtractor pins whatever it is shown, so a test can see what the pass
// was actually given to read.
type pinningExtractor struct {
	mu    sync.Mutex
	read  []string
	force []interaction.StandingInstruction
}

func (extractor *pinningExtractor) Name() string { return "pinning" }

func (extractor *pinningExtractor) Extract(
	_ context.Context, existing []interaction.StandingInstruction, _ []string, utterance string,
) (interaction.Extraction, error) {
	extractor.mu.Lock()
	extractor.read = append(extractor.read, utterance)
	extractor.force = append([]interaction.StandingInstruction(nil), existing...)
	extractor.mu.Unlock()
	return interaction.Extraction{
		Kind: "pin",
		Instruction: interaction.StandingInstruction{
			Text: utterance, Scope: interaction.ScopeConversation,
		},
	}, nil
}

func (extractor *pinningExtractor) seen() []string {
	extractor.mu.Lock()
	defer extractor.mu.Unlock()
	return append([]string(nil), extractor.read...)
}

// A recogniser cuts where somebody breathes, so "tell me the moment the build
// finishes and don't say anything else" arrives as two utterances. Both halves
// read alone are wrong: the front half pins a truncation, and the back half,
// capitalised and punctuated like a sentence of its own, revokes it - measured
// five times out of five against the model that runs this pass. Read whole, it
// pins the whole instruction.
func TestASentenceCutInTwoIsReadWhole(t *testing.T) {
	extractor := &pinningExtractor{}
	policies := interaction.Defaults()
	policies.Extraction = extractor
	var utterance atomic.Int64
	halves := []string{"tell me the moment the build", "finishes and don't say anything else"}
	runtime, _ := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			index := int(utterance.Add(1)) - 1
			if index >= len(halves) {
				index = len(halves) - 1
			}
			// Partials as well as a final, because the pass reads an
			// utterance many times as it grows - and joining each reading onto
			// the last joined text would compound the sentence with itself.
			return &scriptedASR{
				partials: []string{halves[index], halves[index]},
				final:    halves[index],
			}, nil
		},
		Fast: newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Will do."}}),
		Slow: newSlow(), Policies: policies,
	}, binding.Settings{})

	speak(t, runtime, 3)
	// Until it is in the log there is no previous utterance to join onto, and
	// the pass reads what it is given: the wait is on the trajectory rather
	// than on the reading, which happens first.
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if strings.Contains(item.Content, "the moment the build") {
				return true
			}
		}
		return false
	}, "the first half never reached the log")
	// The speaker draws breath and carries on. The recogniser calls that a new
	// utterance; the clock says it is the same sentence.
	speak(t, runtime, 3)
	waitFor(t, func() bool { return len(extractor.seen()) > 1 }, "the continuation was never read")

	joined := false
	for _, read := range extractor.seen() {
		if strings.Count(read, "the build") > 1 || strings.Count(read, "anything else") > 1 {
			t.Fatalf("the sentence was compounded with itself: %q", read)
		}
		if strings.Contains(read, "the build") && strings.Contains(read, "anything else") {
			joined = true
		}
	}
	if !joined {
		t.Fatalf("the halves were never read as one sentence: %q", extractor.seen())
	}
}
