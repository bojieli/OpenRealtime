package cascade

import (
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/continuation"
)

// A turn that said nothing has to say why.
//
// Silence is indistinguishable from working correctly and having nothing to
// add, and the two want opposite reactions from whoever is watching. The case
// that matters is a fast provider that spent its entire output budget
// deliberating: with thinking left on and a short budget, every token goes
// into a reasoning block that never closes, the runtime correctly declines to
// speak any of it, and what reaches the world is nothing at all - the same
// failure as reading the deliberation aloud, minus the evidence.
//
// The diagnosis needs nothing inferred. The provider says it wrote reasoning
// into the content field, it says the output limit was what stopped it, and
// the turn holds no assistant text. Three facts the runtime already has.

type turnReport struct {
	mu         sync.Mutex
	spoke      bool
	truncated  bool
	deliberate bool
}

// record folds in one continuation's result.
func (report *turnReport) record(result continuation.RunResult) {
	report.mu.Lock()
	defer report.mu.Unlock()
	if strings.TrimSpace(result.AssistantText) != "" {
		report.spoke = true
	}
	if result.Completion.StopReason == "length" {
		report.truncated = true
	}
	if result.Completion.ReasoningInContent {
		report.deliberate = true
	}
}

// outcome is what the turn owes an explanation for.
func (report *turnReport) outcome() binding.TurnOutcome {
	report.mu.Lock()
	defer report.mu.Unlock()
	if report.spoke || !report.truncated {
		// Either it said something, or it stopped for an ordinary reason and
		// had nothing to add. Neither needs explaining.
		return binding.TurnOutcome{}
	}
	outcome := binding.TurnOutcome{Incomplete: true, Reason: binding.TurnIncompleteTokens}
	if report.deliberate {
		outcome.Detail = "the output limit was reached inside a reasoning block, so the whole budget " +
			"was spent deliberating: raise -fast-max-tokens, or select a fast provider that can be " +
			"asked to disable thinking (-fast-provider vllm rather than openai-compatible)"
		return outcome
	}
	outcome.Detail = "the output limit was reached before anything was said: raise -fast-max-tokens"
	return outcome
}
