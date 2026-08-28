package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
)

// Measured against the live endpoint: asking for a level and a budget together
// is a 400, "you can only set only one of thinking budget and thinking level".
// One field carries the effort, so the rejected request cannot be built.
func TestAnEffortIsALevelOrABudgetAndNeverBoth(t *testing.T) {
	for _, effort := range []continuation.Effort{"low", "high", "512", "0"} {
		config := thinkingFor(effort, false)
		if config.ThinkingLevel != "" && config.ThinkingBudget != nil {
			t.Fatalf("%q rendered both a level and a budget", effort)
		}
		if config.ThinkingLevel == "" && config.ThinkingBudget == nil {
			t.Fatalf("%q rendered neither a level nor a budget", effort)
		}
	}
	if budget := thinkingFor("512", false).ThinkingBudget; budget == nil || *budget != 512 {
		t.Fatalf("a numeric effort did not become a budget: %v", budget)
	}
	if level := thinkingFor("low", false).ThinkingLevel; level != "LOW" {
		t.Fatalf("a named effort did not become a level: %q", level)
	}
	// Zero is a real budget - do not think - and not an absent one.
	zero := thinkingFor("0", false)
	if zero.ThinkingBudget == nil || *zero.ThinkingBudget != 0 || zero.ThinkingLevel != "" {
		t.Fatalf("a zero budget was lost: %+v", zero)
	}
}

// How long the voice may think and how long it may speak are separate
// quantities. Gemini spends one allowance on thinking first, so a ceiling
// meant to keep a spoken turn short is spent before any speech is produced:
// measured at ninety-six with thinking on, the reply came back empty with
// finishReason MAX_TOKENS. An unset ceiling must not be sent at all.
func TestNoOutputCeilingIsSentUnlessOneIsAskedFor(t *testing.T) {
	encoded, err := json.Marshal(geminiGenerationConfig{
		ThinkingConfig: thinkingFor(continuation.EffortLow, false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "maxOutputTokens") {
		t.Fatalf("an unset ceiling was sent anyway: %s", encoded)
	}
	ceiling := 4096
	encoded, err = json.Marshal(geminiGenerationConfig{MaxOutputTokens: ceiling})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"maxOutputTokens":4096`) {
		t.Fatalf("an asked-for ceiling was dropped: %s", encoded)
	}
}
