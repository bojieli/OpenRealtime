package cascade

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
)

// The diagnosis needs nothing inferred: the provider says it wrote its
// deliberation into the content field, it says the output limit is what
// stopped it, and the turn holds no assistant text. What the table below
// guards is that silence alone is never reported as a fault - a turn that had
// nothing to add is the runtime working.
func TestOnlyASilenceWithACauseIsReported(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		results    []continuation.RunResult
		incomplete bool
		mentions   string
	}{{
		name: "a turn that spoke explains nothing",
		results: []continuation.RunResult{{
			AssistantText: "Forty dollars.",
			Completion:    continuation.Completion{StopReason: "stop"},
		}},
	}, {
		name: "a turn with nothing to add explains nothing",
		results: []continuation.RunResult{{
			Completion: continuation.Completion{StopReason: "stop"},
		}},
	}, {
		name: "a turn cut off mid-sentence still spoke",
		results: []continuation.RunResult{{
			AssistantText: "The balance is",
			Completion:    continuation.Completion{StopReason: "length"},
		}},
	}, {
		name: "a budget spent entirely on deliberation names the knob",
		results: []continuation.RunResult{{
			Completion: continuation.Completion{StopReason: "length", ReasoningInContent: true},
		}},
		incomplete: true,
		mentions:   "reasoning block",
	}, {
		name: "a budget exhausted without deliberation says so plainly",
		results: []continuation.RunResult{{
			Completion: continuation.Completion{StopReason: "length"},
		}},
		incomplete: true,
		mentions:   "-fast-max-tokens",
	}, {
		name: "one step speaking is enough for the whole turn",
		results: []continuation.RunResult{
			{Completion: continuation.Completion{StopReason: "length", ReasoningInContent: true}},
			{AssistantText: "Forty dollars.", Completion: continuation.Completion{StopReason: "stop"}},
		},
	}, {
		name: "whitespace is not speech",
		results: []continuation.RunResult{{
			AssistantText: "  \n ",
			Completion:    continuation.Completion{StopReason: "length"},
		}},
		incomplete: true,
		mentions:   "-fast-max-tokens",
	}} {
		t.Run(testCase.name, func(t *testing.T) {
			report := &turnReport{}
			for _, result := range testCase.results {
				report.record(result)
			}
			outcome := report.outcome()
			if outcome.Incomplete != testCase.incomplete {
				t.Fatalf("incomplete = %v, want %v (detail %q)",
					outcome.Incomplete, testCase.incomplete, outcome.Detail)
			}
			if !testCase.incomplete {
				if outcome.Detail != "" || outcome.Reason != "" {
					t.Fatalf("a turn that owes no explanation must give none, got %+v", outcome)
				}
				return
			}
			if outcome.Reason != "max_output_tokens" {
				t.Fatalf("reason = %q, want the protocol's own vocabulary", outcome.Reason)
			}
			if !strings.Contains(outcome.Detail, testCase.mentions) {
				t.Fatalf("detail must name what to change (%q), got %q",
					testCase.mentions, outcome.Detail)
			}
		})
	}
}
