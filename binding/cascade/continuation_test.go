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
			return &scriptedASR{final: halves[index]}, nil
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

	for _, read := range extractor.seen() {
		if strings.Contains(read, "the build") && strings.Contains(read, "anything else") {
			return
		}
	}
	t.Fatalf("the halves were never read as one sentence: %q", extractor.seen())
}

// The same fact the extraction pass needs, for the decision that comes first.
// The runtime knows two things the reader cannot: the speaker started again
// within a breath, and something the agent said sits after the last thing they
// finished. Without the second, an ordinary pause mid-request would read as a
// sentence already dealt with and the reply would never come.
func TestTheDecisionIsToldWhenATailHasAlreadyBeenAnswered(t *testing.T) {
	decider := &recordingDecider{}
	model, err := interaction.NewInteractionModel(decider)
	if err != nil {
		t.Fatal(err)
	}
	policies := interaction.Defaults()
	policies.Interaction = model
	floor, err := interaction.NewActFloor(model, interaction.ActFloorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	policies.Floor = floor
	var utterance atomic.Int64
	runtime, _ := startSession(t, cascade.Config{
		Perception: func() (v1.PerceptionProvider, error) {
			if int(utterance.Add(1)) == 1 {
				return &scriptedASR{final: "tell me the moment the build"}, nil
			}
			// Partials, because the decision this is about is taken while
			// somebody is still speaking and there is nothing to take it on
			// until the recogniser has said something.
			return &scriptedASR{
				partials: []string{"finishes", "finishes and don't say"},
				final:    "finishes and don't say anything else",
			}, nil
		},
		Fast: newFast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Will do."}}),
		Slow: newSlow(), Policies: policies,
	}, binding.Settings{})

	speak(t, runtime, 3)
	waitFor(t, func() bool {
		for _, item := range runtime.Trajectory().Items {
			if strings.Contains(item.Content, "Will do") {
				return true
			}
		}
		return false
	}, "the first half was never answered")
	speak(t, runtime, 3)
	waitFor(t, func() bool {
		for _, evidence := range decider.seen() {
			if strings.Contains(evidence, "already replied to") {
				return true
			}
		}
		return false
	}, "the decision was never told the tail had already been answered")
}

// recordingDecider keeps every rendered situation it was asked about, and
// always chooses to stay silent.
type recordingDecider struct {
	mu       sync.Mutex
	evidence []string
}

func (decider *recordingDecider) Name() string { return "recording" }

func (decider *recordingDecider) Decide(
	_ context.Context, request interaction.Decision,
) (interaction.Outcome, error) {
	decider.mu.Lock()
	decider.evidence = append(decider.evidence, request.Evidence)
	decider.mu.Unlock()
	for index, option := range request.Options {
		if option == string(interaction.ActStaySilent) {
			return interaction.Outcome{Index: index}, nil
		}
	}
	return interaction.Outcome{Index: 0}, nil
}

func (decider *recordingDecider) seen() []string {
	decider.mu.Lock()
	defer decider.mu.Unlock()
	return append([]string(nil), decider.evidence...)
}
